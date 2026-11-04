import { spawn, type ChildProcess } from 'node:child_process';
import { createHash, randomUUID } from 'node:crypto';
import { closeSync, existsSync, openSync, statSync } from 'node:fs';
import { chmod, mkdir, readFile, rm, truncate, writeFile } from 'node:fs/promises';
import { createServer } from 'node:net';
import { hostname as systemHostname } from 'node:os';
import { join } from 'node:path';

import type { TailscaleNodeStatus } from '@yuanpu-agent/protocol';

import { tailnetMatcher, type TailnetRoute } from './tailnet-routing.js';

/**
 * Runs the bundled tailscaled in userspace-networking mode: no system extension, no
 * TUN device and no admin rights. The node only exists while the Runtime runs. Its
 * state (machine key, login) lives under the Yuanpu home so a reboot reconnects without
 * spending the auth key again.
 */
export interface TailscaleNodeOptions {
  binaryRoot: string;
  stateRoot: string;
  hostname?: string;
  platform?: NodeJS.Platform;
  log?: (message: string) => void;
}

interface StatusDocument {
  BackendState?: string;
  AuthURL?: string;
  MagicDNSSuffix?: string;
  CurrentTailnet?: { Name?: string; MagicDNSSuffix?: string } | null;
  Self?: { DNSName?: string; HostName?: string; TailscaleIPs?: string[] | null; Online?: boolean } | null;
  Peer?: Record<string, { DNSName?: string; HostName?: string; PrimaryRoutes?: string[] | null }> | null;
  Health?: string[] | null;
}

const SOCKET_PATH_LIMIT = 103;
const RESTART_WINDOW_MS = 60_000;
const RESTART_BUDGET = 3;
const STATUS_INTERVAL_MS = 30_000;
const LOG_LIMIT_BYTES = 5 * 1024 * 1024;

export function tailscaleHostname(name = systemHostname()): string {
  const label = name.toLowerCase().replace(/\.local$/, '').replace(/[^a-z0-9-]+/g, '-')
    .replace(/^-+|-+$/g, '').slice(0, 40);
  return `mindlink-${label || 'mac'}`;
}

export function keyFingerprint(authKey: string): string {
  return createHash('sha256').update(authKey).digest('hex');
}

/** Pulls the routing facts out of `tailscale status --json`. */
export function statusView(document: StatusDocument): {
  status: TailscaleNodeStatus; magicDnsSuffix?: string; peerNames: string[]; subnetRoutes: string[];
} {
  const backend = document.BackendState ?? 'NoState';
  const state: TailscaleNodeStatus['state'] = backend === 'Running' ? 'connected'
    : backend === 'NeedsLogin' || backend === 'NeedsMachineAuth' ? 'needs-login'
      : backend === 'Stopped' ? 'stopped' : 'starting';
  const suffix = document.CurrentTailnet?.MagicDNSSuffix || document.MagicDNSSuffix || undefined;
  const peers = Object.values(document.Peer ?? {});
  const peerNames = peers.flatMap((peer) => {
    const dns = peer.DNSName?.replace(/\.$/, '');
    return [dns, dns?.split('.')[0], peer.HostName].filter((name): name is string => Boolean(name));
  });
  const subnetRoutes = peers.flatMap((peer) => peer.PrimaryRoutes ?? []);
  const ipv4 = document.Self?.TailscaleIPs?.find((address) => address.includes('.'));
  const dnsName = document.Self?.DNSName?.replace(/\.$/, '');
  const tailnet = document.CurrentTailnet?.Name;
  const message = backend === 'NeedsMachineAuth' ? '设备等待管理员在 Tailscale 后台批准。'
    : backend === 'NeedsLogin' ? 'Auth Key 无效或已过期，请更换后重新保存。'
      : document.Health?.length ? document.Health[0] : undefined;
  return {
    status: {
      state,
      ...(ipv4 ? { ipv4 } : {}),
      ...(dnsName ? { dnsName } : {}),
      ...(tailnet ? { tailnet } : {}),
      ...(message ? { message } : {}),
    },
    ...(suffix ? { magicDnsSuffix: suffix } : {}),
    peerNames,
    subnetRoutes,
  };
}

function freeLoopbackPort(): Promise<number> {
  return new Promise((resolvePort, reject) => {
    const server = createServer();
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const address = server.address();
      server.close(() => (address && typeof address === 'object' ? resolvePort(address.port) : reject(new Error('No free port.'))));
    });
  });
}

function run(executable: string, args: string[], timeoutMs: number): Promise<{ code: number | null; stdout: string; stderr: string }> {
  return new Promise((resolveRun, reject) => {
    const child = spawn(executable, args, { stdio: ['ignore', 'pipe', 'pipe'] });
    let stdout = '';
    let stderr = '';
    child.stdout.on('data', (chunk) => { stdout = (stdout + chunk).slice(-1_000_000); });
    child.stderr.on('data', (chunk) => { stderr = (stderr + chunk).slice(-16_000); });
    const timer = setTimeout(() => child.kill('SIGKILL'), timeoutMs);
    child.once('error', (error) => { clearTimeout(timer); reject(error); });
    child.once('close', (code) => { clearTimeout(timer); resolveRun({ code, stdout, stderr }); });
  });
}

function redact(text: string): string {
  return text.replace(/tskey-[A-Za-z0-9-]+/g, 'tskey-…').trim().split('\n').slice(-3).join(' ').slice(0, 400);
}

export class TailscaleNode implements TailnetRoute {
  readonly tailscaled: string;
  readonly tailscale: string;
  readonly socket: string;
  private readonly hostname: string;
  private readonly log: (message: string) => void;
  private child: ChildProcess | undefined;
  private proxyPort: number | undefined;
  private matcher: ((hostname: string) => boolean) | undefined;
  private current: TailscaleNodeStatus = { state: 'stopped' };
  private statusTimer: NodeJS.Timeout | undefined;
  private restarts: number[] = [];
  private stopping = false;
  private closed = false;
  private queue: Promise<unknown> = Promise.resolve();

  /** Returns a node only when the desktop shipped both binaries for this platform. */
  static fromEnvironment(stateRoot: string, environment: NodeJS.ProcessEnv = process.env,
    platform: NodeJS.Platform = process.platform): TailscaleNode | undefined {
    const binaryRoot = environment.YUANPU_TAILSCALE_ROOT;
    if (!binaryRoot || platform !== 'darwin' || environment.YUANPU_TAILSCALE_DISABLED === '1') return undefined;
    if (!existsSync(join(binaryRoot, 'tailscaled')) || !existsSync(join(binaryRoot, 'tailscale'))) return undefined;
    return new TailscaleNode({ binaryRoot, stateRoot, platform });
  }

  constructor(private readonly options: TailscaleNodeOptions) {
    this.tailscaled = join(options.binaryRoot, 'tailscaled');
    this.tailscale = join(options.binaryRoot, 'tailscale');
    this.socket = join(options.stateRoot, 'tailscaled.sock');
    this.hostname = options.hostname ?? tailscaleHostname();
    this.log = options.log ?? ((message) => console.warn(`[tailscale] ${message}`));
  }

  status(): TailscaleNodeStatus {
    return { ...this.current };
  }

  proxyPortFor(hostname: string): number | undefined {
    if (this.current.state !== 'connected' || !this.proxyPort || !this.matcher) return undefined;
    return this.matcher(hostname) ? this.proxyPort : undefined;
  }

  /** Serialized: brings the node in line with the saved auth key (or logs it out). */
  apply(authKey: string | undefined): Promise<TailscaleNodeStatus> {
    const next = this.queue.then(() => this.applyNow(authKey));
    this.queue = next.catch(() => undefined);
    return next;
  }

  private async applyNow(authKey: string | undefined): Promise<TailscaleNodeStatus> {
    const marker = join(this.options.stateRoot, 'key.sha256');
    if (!authKey) {
      if (this.child || existsSync(join(this.options.stateRoot, 'state'))) {
        await this.ensureDaemon().catch(() => undefined);
        if (this.child) await this.cli(['logout'], 30_000).catch(() => undefined);
      }
      await this.stopDaemon();
      await rm(marker, { force: true });
      await rm(join(this.options.stateRoot, 'state'), { recursive: true, force: true });
      this.current = { state: 'stopped' };
      return this.status();
    }
    try {
      await this.ensureDaemon();
      const fingerprint = keyFingerprint(authKey);
      const previous = await readFile(marker, 'utf8').catch(() => '');
      let view = await this.refresh();
      const changed = previous.trim() !== fingerprint;
      if (changed && view.state !== 'needs-login') {
        await this.cli(['logout'], 30_000).catch(() => undefined);
        view = await this.refresh();
      }
      if (changed || view.state === 'needs-login' || view.state === 'stopped') {
        await this.up(authKey);
        await writeFile(marker, `${fingerprint}\n`, { mode: 0o600 });
      }
      await this.refresh();
    } catch (error) {
      this.current = { state: 'error', message: redact(error instanceof Error ? error.message : String(error)) };
    }
    return this.status();
  }

  private async up(authKey: string): Promise<void> {
    const keyFile = join(this.options.stateRoot, `.auth-${randomUUID()}`);
    await writeFile(keyFile, authKey, { flag: 'wx', mode: 0o600 });
    try {
      const result = await this.cli(['up', `--auth-key=file:${keyFile}`, `--hostname=${this.hostname}`,
        '--accept-routes', '--reset', '--timeout=60s'], 75_000);
      if (result.code !== 0) throw new Error(`tailscale up failed: ${redact(result.stderr || result.stdout)}`);
    } finally {
      await rm(keyFile, { force: true });
    }
  }

  private cli(args: string[], timeoutMs: number) {
    return run(this.tailscale, [`--socket=${this.socket}`, ...args], timeoutMs);
  }

  private async refresh(): Promise<TailscaleNodeStatus> {
    if (!this.child) return this.status();
    const result = await this.cli(['status', '--json'], 15_000);
    let document: StatusDocument;
    try {
      document = JSON.parse(result.stdout) as StatusDocument;
    } catch {
      throw new Error(`tailscale status failed: ${redact(result.stderr || result.stdout)}`);
    }
    const view = statusView(document);
    this.matcher = tailnetMatcher(view);
    this.current = view.status;
    return this.status();
  }

  private async ensureDaemon(): Promise<void> {
    if (this.child) return;
    if (this.closed) throw new Error('Tailscale node is shut down.');
    const root = this.options.stateRoot;
    if (this.socket.length > SOCKET_PATH_LIMIT) throw new Error(`Tailscale socket path is too long: ${this.socket}`);
    await mkdir(join(root, 'state'), { recursive: true, mode: 0o700 });
    await chmod(root, 0o700);
    await this.stopStale();
    await rm(this.socket, { force: true });
    const logPath = join(root, 'tailscaled.log');
    try { if (statSync(logPath).size > LOG_LIMIT_BYTES) await truncate(logPath, 0); } catch { /* no log yet */ }
    const port = await freeLoopbackPort();
    const log = openSync(logPath, 'a', 0o600);
    this.stopping = false;
    this.current = { state: 'starting' };
    const child = spawn(this.tailscaled, [
      '--tun=userspace-networking',
      `--statedir=${join(root, 'state')}`,
      `--socket=${this.socket}`,
      '--port=0',
      `--outbound-http-proxy-listen=127.0.0.1:${port}`,
      '--no-logs-no-support',
    ], { stdio: ['ignore', log, log] });
    closeSync(log);
    await new Promise<void>((resolveSpawn, reject) => {
      child.once('spawn', resolveSpawn);
      child.once('error', reject);
    });
    this.child = child;
    this.proxyPort = port;
    await writeFile(join(root, 'tailscaled.pid'), `${child.pid}\n`, { mode: 0o600 });
    child.once('exit', (code, signal) => this.onExit(child, code, signal));
    const deadline = Date.now() + 20_000;
    for (;;) {
      if (this.child !== child) throw new Error('tailscaled exited during startup; see tailscaled.log.');
      if (existsSync(this.socket)) {
        const probe = await this.cli(['status', '--json'], 5_000).catch(() => undefined);
        if (probe?.stdout.trim().startsWith('{')) break;
      }
      if (Date.now() > deadline) {
        await this.stopDaemon();
        throw new Error('tailscaled did not become ready within 20s.');
      }
      await new Promise((resolveWait) => setTimeout(resolveWait, 250));
    }
    this.statusTimer = setInterval(() => { void this.refresh().catch(() => undefined); }, STATUS_INTERVAL_MS);
    this.statusTimer.unref();
  }

  private onExit(child: ChildProcess, code: number | null, signal: NodeJS.Signals | null): void {
    if (this.child !== child) return;
    this.child = undefined;
    this.proxyPort = undefined;
    this.matcher = undefined;
    clearInterval(this.statusTimer);
    if (this.stopping) return;
    const now = Date.now();
    this.restarts = this.restarts.filter((time) => now - time < RESTART_WINDOW_MS);
    if (this.restarts.length >= RESTART_BUDGET) {
      this.current = { state: 'error', message: 'tailscaled 反复退出，已停止重试；请查看 tailscaled.log。' };
      return;
    }
    this.restarts.push(now);
    this.log(`tailscaled exited (${signal ?? code}); restarting.`);
    // Login state is persisted, so the daemon reconnects without the auth key.
    const restart = this.queue.then(async () => {
      if (this.stopping || this.child) return;
      await this.ensureDaemon();
      await this.refresh();
    });
    this.queue = restart.catch((error: unknown) => {
      this.current = { state: 'error', message: redact(error instanceof Error ? error.message : String(error)) };
    });
  }

  /** A tailscaled left behind by a crashed Runtime would still own the socket. */
  private async stopStale(): Promise<void> {
    const pidFile = join(this.options.stateRoot, 'tailscaled.pid');
    const pid = Number((await readFile(pidFile, 'utf8').catch(() => '')).trim());
    await rm(pidFile, { force: true });
    if (!Number.isSafeInteger(pid) || pid <= 1) return;
    const ps = await run('/bin/ps', ['-p', String(pid), '-o', 'command='], 5_000).catch(() => undefined);
    if (!ps?.stdout.includes(this.tailscaled)) return;
    try { process.kill(pid, 'SIGTERM'); } catch { return; }
    for (let attempt = 0; attempt < 20; attempt++) {
      await new Promise((resolveWait) => setTimeout(resolveWait, 100));
      try { process.kill(pid, 0); } catch { return; }
    }
    try { process.kill(pid, 'SIGKILL'); } catch { /* already gone */ }
  }

  private async stopDaemon(): Promise<void> {
    const child = this.child;
    this.stopping = true;
    clearInterval(this.statusTimer);
    if (!child) return;
    const exited = new Promise<void>((resolveExit) => child.once('exit', () => resolveExit()));
    child.kill('SIGTERM');
    // Runtime cleanup has a 2s budget; a stale daemon is also reaped on next start.
    const timer = setTimeout(() => child.kill('SIGKILL'), 1_500);
    await exited;
    clearTimeout(timer);
    await rm(join(this.options.stateRoot, 'tailscaled.pid'), { force: true });
  }

  /** Runtime shutdown: stop the daemon but keep the login for the next launch. */
  async stop(): Promise<void> {
    this.closed = true;
    // Do not wait for a pending `tailscale up`; killing the daemon makes it fail fast.
    await this.stopDaemon();
    this.current = { state: 'stopped' };
  }
}

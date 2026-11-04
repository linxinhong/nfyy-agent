import { execFile, spawn, type ChildProcess } from 'node:child_process';
import { createHash, randomBytes, X509Certificate } from 'node:crypto';
import { closeSync, existsSync, openSync } from 'node:fs';
import { chmod, copyFile, mkdir, readFile, rm, stat, writeFile } from 'node:fs/promises';
import { createConnection } from 'node:net';
import { homedir } from 'node:os';
import { join } from 'node:path';
import { promisify } from 'node:util';

const execFileAsync = promisify(execFile);

/** Ports fixed by the bridge and by the side-loaded Office add-in manifest. */
export const WORDBRIDGE_MCP_PORT = 9877;
export const WORDBRIDGE_ADDIN_PORT = 9879;
export const WORDBRIDGE_MCP_URL = `http://127.0.0.1:${WORDBRIDGE_MCP_PORT}/mcp`;
const ADDIN_MANIFEST_NAME = 'mindlink-wordbridge.xml';
const CERT_RENEW_AFTER_MS = 800 * 24 * 60 * 60 * 1000;

const CERT_REQUEST_CONFIG = `[req]
distinguished_name = dn
x509_extensions = v3
prompt = no
[dn]
CN = localhost
[v3]
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature,keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = DNS:localhost,IP:127.0.0.1
subjectKeyIdentifier = hash
`;

// ws_port 0 disables the unauthenticated legacy debug WebSocket.
const BRIDGE_CONFIG = `bridge:
  name: "WordBridge"
  version: "1.0.0"
  log_level: "INFO"
  log_file: "wordbridge.log"
server:
  host: "127.0.0.1"
  ws_port: 0
  http_port: ${WORDBRIDGE_MCP_PORT}
regions:
  database: "regions.db"
  ttl_seconds: 300
  conflict_similarity_threshold: 0.85
`;

export interface WordBridgeHostOptions {
  /** Bundled payload produced by scripts/prepare-wordbridge.mjs. */
  payloadRoot: string;
  /** Writable per-user state: config, certificate, region database and logs. */
  stateRoot: string;
  homeDirectory?: string;
  wordApplicationPath?: string;
  log?: (message: string) => void;
  restartLimit?: number;
  shutdownGraceMs?: number;
}

/**
 * Runs the WordBridge MCP server for the lifetime of the desktop App.
 *
 * The bridge drives Microsoft Word for Mac through a side-loaded Office add-in
 * served over HTTPS from a per-machine certificate. Its MCP endpoint requires
 * a per-launch bearer token that only the Runtime receives.
 */
export class WordBridgeHost {
  readonly token = randomBytes(32).toString('hex');
  private child?: ChildProcess;
  private stopping = false;
  private restarts: number[] = [];
  private readonly home: string;
  private readonly log: (message: string) => void;

  private constructor(private readonly options: WordBridgeHostOptions) {
    this.home = options.homeDirectory ?? homedir();
    this.log = options.log ?? ((message) => console.log(`[wordbridge] ${message}`));
  }

  /** Returns a host only when the payload, the platform and Microsoft Word are all present. */
  static detect(options: WordBridgeHostOptions, platform: NodeJS.Platform = process.platform): WordBridgeHost | undefined {
    if (platform !== 'darwin' || process.env.YUANPU_WORDBRIDGE_DISABLED === '1') return undefined;
    if (!existsSync(join(options.payloadRoot, 'payload.json'))) return undefined;
    if (!existsSync(options.wordApplicationPath ?? '/Applications/Microsoft Word.app')) return undefined;
    return new WordBridgeHost(options);
  }

  get python(): string {
    return join(this.options.payloadRoot, 'python', 'bin', 'python3');
  }

  get certificateDirectory(): string {
    return join(this.options.stateRoot, 'certs');
  }

  /** Variables that let the Runtime register the bridge as a built-in MCP server. */
  runtimeEnvironment(): NodeJS.ProcessEnv {
    return { YUANPU_WORDBRIDGE_MCP_URL: WORDBRIDGE_MCP_URL, YUANPU_WORDBRIDGE_MCP_TOKEN: this.token };
  }

  async start(): Promise<void> {
    this.stopping = false;
    await mkdir(this.options.stateRoot, { recursive: true, mode: 0o700 });
    await this.writeConfig();
    await this.stopStaleBridge();
    const certificate = await this.ensureCertificate();
    await this.ensureCertificateTrusted(certificate);
    await this.sideloadAddin();
    for (const port of [WORDBRIDGE_MCP_PORT, WORDBRIDGE_ADDIN_PORT]) {
      if (await portInUse(port)) {
        this.log(`port ${port} is already in use; WordBridge stays off for this session.`);
        return;
      }
    }
    this.spawnBridge();
  }

  async stop(): Promise<void> {
    this.stopping = true;
    const child = this.child;
    this.child = undefined;
    if (child && child.exitCode === null && child.signalCode === null) {
      child.kill('SIGTERM');
      if (!await waitForExit(child, this.options.shutdownGraceMs ?? 3_000)) {
        child.kill('SIGKILL');
        await waitForExit(child, 1_000);
      }
    }
    await rm(this.pidFile, { force: true });
  }

  private get pidFile(): string {
    return join(this.options.stateRoot, 'bridge.pid');
  }

  private async writeConfig(): Promise<void> {
    await writeFile(join(this.options.stateRoot, 'config.yaml'), BRIDGE_CONFIG, { mode: 0o600 });
  }

  /** A previous App crash can leave an orphaned bridge holding the fixed ports. */
  private async stopStaleBridge(): Promise<void> {
    const pid = Number.parseInt(await readFile(this.pidFile, 'utf8').catch(() => ''), 10);
    if (!Number.isSafeInteger(pid) || pid <= 1) return;
    const command = await execFileAsync('/bin/ps', ['-p', String(pid), '-o', 'command=']).then(
      ({ stdout }) => stdout.trim(),
      () => '',
    );
    // Only signal a process that is verifiably our bundled interpreter.
    if (command.startsWith(this.python) && command.includes('run_bridge.py')) {
      this.log(`stopping orphaned bridge ${pid}`);
      try { process.kill(pid, 'SIGTERM'); } catch { /* already gone */ }
      await delay(500);
    }
    await rm(this.pidFile, { force: true });
  }

  /** Generates a per-machine localhost leaf certificate (CA:FALSE, serverAuth only). */
  private async ensureCertificate(): Promise<{ path: string; fingerprint: string }> {
    const directory = this.certificateDirectory;
    const certificate = join(directory, 'localhost.pem');
    const key = join(directory, 'localhost-key.pem');
    const current = await stat(certificate).catch(() => undefined);
    const fresh = current && existsSync(key) && Date.now() - current.mtimeMs < CERT_RENEW_AFTER_MS;
    if (!fresh) {
      await mkdir(directory, { recursive: true, mode: 0o700 });
      const config = join(directory, 'request.cnf');
      await writeFile(config, CERT_REQUEST_CONFIG, { mode: 0o600 });
      await execFileAsync('/usr/bin/openssl', [
        'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-sha256', '-days', '825',
        '-config', config, '-keyout', key, '-out', certificate,
      ]);
      await chmod(key, 0o600);
      await rm(config, { force: true });
      this.log('generated a per-machine localhost certificate');
    }
    const pem = await readFile(certificate);
    return { path: certificate, fingerprint: new X509Certificate(pem).fingerprint256 };
  }

  /**
   * Word's add-in web view only loads the task pane over trusted HTTPS. Trust is
   * added to the user's login keychain for SSL only, once per certificate; macOS
   * asks the user to confirm. A refusal is remembered so it is not asked again.
   */
  private async ensureCertificateTrusted(certificate: { path: string; fingerprint: string }): Promise<void> {
    const marker = join(this.certificateDirectory, 'trust-attempted');
    const attempted = await readFile(marker, 'utf8').catch(() => '');
    if (attempted.trim() === certificate.fingerprint) return;
    try {
      await execFileAsync('/usr/bin/security', [
        'add-trusted-cert', '-r', 'trustRoot', '-p', 'ssl',
        '-k', join(this.home, 'Library', 'Keychains', 'login.keychain-db'),
        certificate.path,
      ], { timeout: 180_000 });
      this.log('trusted the localhost certificate for SSL');
    } catch (error) {
      this.log(`certificate trust was not added: ${error instanceof Error ? error.message : String(error)}`);
    }
    await writeFile(marker, `${certificate.fingerprint}\n`, { mode: 0o600 });
  }

  /** Side-loads the add-in manifest so Word shows the MindLink task pane button. */
  private async sideloadAddin(): Promise<void> {
    const source = join(this.options.payloadRoot, 'app', 'addin', 'manifest.xml');
    const directory = join(this.home, 'Library', 'Containers', 'com.microsoft.Word', 'Data', 'Documents', 'wef');
    const destination = join(directory, ADDIN_MANIFEST_NAME);
    const [wanted, existing] = await Promise.all([readFile(source), readFile(destination).catch(() => undefined)]);
    if (existing && digest(existing) === digest(wanted)) return;
    await mkdir(directory, { recursive: true });
    await copyFile(source, destination);
    this.log(`side-loaded the Word add-in manifest to ${destination}`);
  }

  private spawnBridge(): void {
    const stateRoot = this.options.stateRoot;
    const stderr = openSync(join(stateRoot, 'bridge.stderr.log'), 'w', 0o600);
    let child: ChildProcess;
    try {
      // -I: ignore user site-packages and PYTHON* variables; -B: the signed bundle is read-only.
      child = spawn(this.python, ['-I', '-B', join(this.options.payloadRoot, 'app', 'run_bridge.py'),
        '--no-hotkey', '--config', join(stateRoot, 'config.yaml')], {
        cwd: stateRoot,
        stdio: ['ignore', 'ignore', stderr],
        env: {
          ...process.env,
          WORDBRIDGE_CONFIG: join(stateRoot, 'config.yaml'),
          WORDBRIDGE_CERT_DIR: this.certificateDirectory,
          WORDBRIDGE_AUTH_TOKEN: this.token,
        },
      });
    } finally {
      closeSync(stderr);
    }
    this.child = child;
    child.once('spawn', () => {
      if (child.pid) void writeFile(this.pidFile, `${child.pid}\n`, { mode: 0o600 }).catch(() => undefined);
      this.log(`bridge started (pid ${child.pid})`);
    });
    child.once('error', (error) => this.log(`bridge failed to start: ${error.message}`));
    child.once('exit', (code, signal) => {
      if (this.child !== child) return;
      this.child = undefined;
      if (this.stopping) return;
      this.log(`bridge exited (code ${code ?? 'none'}, signal ${signal ?? 'none'})`);
      this.scheduleRestart();
    });
  }

  private scheduleRestart(): void {
    const now = Date.now();
    this.restarts = this.restarts.filter((time) => now - time < 60_000);
    if (this.restarts.length >= (this.options.restartLimit ?? 3)) {
      this.log('restart budget exhausted; restart the App to retry WordBridge.');
      return;
    }
    this.restarts.push(now);
    setTimeout(() => {
      if (!this.stopping && !this.child) this.spawnBridge();
    }, 1_000 * this.restarts.length).unref();
  }
}

function digest(value: Buffer): string {
  return createHash('sha256').update(value).digest('hex');
}

function delay(ms: number): Promise<void> {
  return new Promise((resolveDelay) => setTimeout(resolveDelay, ms));
}

function waitForExit(child: ChildProcess, timeoutMs: number): Promise<boolean> {
  if (child.exitCode !== null || child.signalCode !== null) return Promise.resolve(true);
  return new Promise((resolveExit) => {
    const timer = setTimeout(() => { child.off('exit', onExit); resolveExit(false); }, timeoutMs);
    const onExit = () => { clearTimeout(timer); resolveExit(true); };
    child.once('exit', onExit);
  });
}

export function portInUse(port: number, host = '127.0.0.1'): Promise<boolean> {
  return new Promise((resolvePort) => {
    const socket = createConnection({ port, host });
    socket.setTimeout(500);
    socket.once('connect', () => { socket.destroy(); resolvePort(true); });
    socket.once('timeout', () => { socket.destroy(); resolvePort(false); });
    socket.once('error', () => resolvePort(false));
  });
}

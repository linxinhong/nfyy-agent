// Bounded smoke test for the bundled tailscaled (no auth key, no network login).
// Usage: node apps/desktop/scripts/smoke-tailscale.mjs <dir-with-tailscaled-and-tailscale>
// Starts the daemon exactly as the Runtime does, checks the local API answers with a
// NeedsLogin state, the outbound proxy listens on loopback, and SIGTERM stops it.
import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { createConnection, createServer } from 'node:net';
import { join, resolve } from 'node:path';

const root = resolve(process.argv[2] ?? '');
// macOS limits Unix socket paths to 104 bytes, so keep the state path short.
const state = mkdtempSync('/tmp/ts-smoke-');
const socket = join(state, 'tailscaled.sock');
const port = await new Promise((resolvePort) => {
  const server = createServer().listen(0, '127.0.0.1', () => {
    const { port: free } = server.address();
    server.close(() => resolvePort(free));
  });
});
const child = spawn(join(root, 'tailscaled'), ['--tun=userspace-networking', `--statedir=${join(state, 'state')}`,
  `--socket=${socket}`, '--port=0', `--outbound-http-proxy-listen=127.0.0.1:${port}`, '--no-logs-no-support'],
{ stdio: ['ignore', 'ignore', 'pipe'], env: { ...process.env, TS_LOGS_DIR: state } });
let stderr = '';
child.stderr.on('data', (chunk) => { stderr = (stderr + chunk).slice(-8_000); });
const exited = new Promise((resolveExit) => child.once('exit', (code, signal) => resolveExit({ code, signal })));
const listening = (target) => new Promise((resolvePort) => {
  const probe = createConnection({ port: target, host: '127.0.0.1' });
  probe.once('connect', () => { probe.destroy(); resolvePort(true); });
  probe.once('error', () => resolvePort(false));
});

try {
  const deadline = Date.now() + 30_000;
  let status;
  for (;;) {
    if (child.exitCode !== null) throw new Error(`tailscaled exited early (${child.exitCode})`);
    if (existsSync(socket)) {
      const result = spawnSync(join(root, 'tailscale'), [`--socket=${socket}`, 'status', '--json'], { encoding: 'utf8', timeout: 5_000 });
      if (result.stdout.trim().startsWith('{')) { status = JSON.parse(result.stdout); break; }
    }
    if (Date.now() > deadline) throw new Error('tailscaled did not answer within 30s');
    await new Promise((resolveWait) => setTimeout(resolveWait, 250));
  }
  assert.equal(status.BackendState, 'NeedsLogin', `unexpected backend state ${status.BackendState}`);
  assert.equal(await listening(port), true, 'the outbound HTTP proxy should listen on loopback');
  assert.match(readFileSync(join(root, 'VERSION'), 'utf8'), /^v\d+\.\d+\.\d+/);
  assert.doesNotMatch(stderr, /\.local\/share\/tailscale|Library\/Caches\/Tailscale/, 'tailscaled must keep its log policy inside the state dir');
  console.log('[tailscale] bundle smoke ok');
} catch (error) {
  console.error(stderr);
  throw error;
} finally {
  child.kill('SIGTERM');
  const timer = setTimeout(() => child.kill('SIGKILL'), 5_000);
  const result = await exited;
  clearTimeout(timer);
  rmSync(state, { recursive: true, force: true });
  if (result.signal === 'SIGKILL') throw new Error('tailscaled ignored SIGTERM');
}

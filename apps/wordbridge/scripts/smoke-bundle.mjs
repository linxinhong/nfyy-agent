// Bounded smoke test for a packaged WordBridge payload (macOS).
// Usage: node apps/wordbridge/scripts/smoke-bundle.mjs <payload-root>
// Starts the bundled interpreter exactly as the desktop host does, then checks
// that MCP requires the bearer token, the legacy debug socket stays closed, and
// the process exits on SIGTERM. Microsoft Word is not required.
import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { createConnection } from 'node:net';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

const payload = resolve(process.argv[2] ?? '');
JSON.parse(readFileSync(join(payload, 'payload.json'), 'utf8'));
const state = mkdtempSync(join(tmpdir(), 'wordbridge-smoke-'));
const token = randomBytes(32).toString('hex');
const base = 'http://127.0.0.1:9877';

const cnf = join(state, 'req.cnf');
writeFileSync(cnf, '[req]\ndistinguished_name = dn\nx509_extensions = v3\nprompt = no\n[dn]\nCN = localhost\n'
  + '[v3]\nbasicConstraints = critical,CA:FALSE\nsubjectAltName = DNS:localhost,IP:127.0.0.1\n');
const cert = spawnSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1', '-config', cnf,
  '-keyout', join(state, 'localhost-key.pem'), '-out', join(state, 'localhost.pem')], { stdio: 'ignore' });
assert.equal(cert.status, 0, 'openssl failed to create the smoke certificate');
writeFileSync(join(state, 'config.yaml'), 'server:\n  host: "127.0.0.1"\n  ws_port: 0\n  http_port: 9877\n'
  + 'bridge:\n  log_file: "wordbridge.log"\nregions:\n  database: "regions.db"\n');

const child = spawn(join(payload, 'python', 'bin', 'python3'),
  ['-I', '-B', join(payload, 'app', 'run_bridge.py'), '--no-hotkey', '--config', join(state, 'config.yaml')], {
    cwd: state,
    stdio: ['ignore', 'ignore', 'pipe'],
    env: { ...process.env, WORDBRIDGE_CERT_DIR: state, WORDBRIDGE_AUTH_TOKEN: token, WORDBRIDGE_CONFIG: join(state, 'config.yaml') },
  });
let stderr = '';
child.stderr.on('data', (chunk) => { stderr = (stderr + chunk).slice(-8_000); });
const exited = new Promise((resolveExit) => child.once('exit', (code, signal) => resolveExit({ code, signal })));

function listening(port) {
  return new Promise((resolvePort) => {
    const socket = createConnection({ port, host: '127.0.0.1' });
    socket.once('connect', () => { socket.destroy(); resolvePort(true); });
    socket.once('error', () => resolvePort(false));
  });
}

const initialize = (headers) => fetch(`${base}/mcp`, {
  method: 'POST',
  headers: { 'content-type': 'application/json', accept: 'application/json, text/event-stream', ...headers },
  body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'initialize', params: {
    protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'smoke', version: '0' } } }),
});

try {
  const deadline = Date.now() + 60_000;
  for (;;) {
    if (child.exitCode !== null) throw new Error(`bridge exited early (${child.exitCode})`);
    const health = await fetch(`${base}/health`).catch(() => undefined);
    if (health) { await health.arrayBuffer(); break; }
    if (Date.now() > deadline) throw new Error('bridge did not start within 60s');
    await new Promise((resolveWait) => setTimeout(resolveWait, 500));
  }
  assert.equal((await initialize({})).status, 401, 'MCP must reject requests without the token');
  assert.equal((await initialize({ authorization: 'Bearer wrong' })).status, 401, 'MCP must reject a wrong token');
  const authorized = await initialize({ authorization: `Bearer ${token}` });
  const body = await authorized.text();
  assert.equal(authorized.status, 200, `authorized initialize failed: ${body}`);
  assert.match(body, /WordBridge/);
  assert.equal(await listening(9876), false, 'the legacy debug WebSocket must stay disabled');
  assert.equal(await listening(9879), true, 'the add-in HTTPS server should be listening');
  console.log('[wordbridge] bundle smoke ok');
} catch (error) {
  console.error(stderr);
  throw error;
} finally {
  child.kill('SIGTERM');
  const timer = setTimeout(() => child.kill('SIGKILL'), 5_000);
  const result = await exited;
  clearTimeout(timer);
  rmSync(state, { recursive: true, force: true });
  if (result.signal === 'SIGKILL') throw new Error('bridge ignored SIGTERM');
}

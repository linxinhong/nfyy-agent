import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createServerClient, ServerApiError } from '../../../packages/yuanpu-server-client/dist/index.mjs';

const root = fileURLToPath(new URL('../../../', import.meta.url));
const project = `yuanpu-server-smoke-${randomBytes(6).toString('hex')}`;
const temporary = await mkdtemp(join(tmpdir(), project));
const envFile = join(temporary, '.env');
await writeFile(envFile, '', { mode: 0o600 });
const password = randomBytes(24).toString('hex');
const env = { ...process.env, YUANPU_DB_PASSWORD: password, YUANPU_DB_PORT: '0', YUANPU_SERVER_PORT: '0' };
const compose = ['compose', '--env-file', envFile, '-p', project, '-f', 'server/backend/deploy/compose.yaml'];

function run(command, args, options = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { cwd: root, env: options.env ?? env, stdio: ['ignore', 'pipe', 'pipe'] });
    let output = '';
    let errors = '';
    const safe = (value) => value.replaceAll(password, '[redacted]');
    child.stdout.on('data', (chunk) => { output += chunk; if (!options.quiet) process.stdout.write(safe(chunk.toString())); });
    child.stderr.on('data', (chunk) => { errors += chunk; if (!options.quiet) process.stderr.write(safe(chunk.toString())); });
    const timer = setTimeout(() => child.kill('SIGKILL'), options.timeoutMs ?? 300_000);
    child.on('error', (error) => { clearTimeout(timer); reject(error); });
    child.on('exit', (code) => {
      clearTimeout(timer);
      if (code === 0) resolve(output.trim());
      else reject(new Error(`${command} ${args[0]} failed (${code}): ${safe(errors.slice(-2000))}`));
    });
  });
}

try {
  await run('docker', [...compose, 'up', '--build', '-d', '--wait', '--wait-timeout', '90']);
  const serverAddress = await run('docker', [...compose, 'port', 'server', '8788'], { quiet: true });
  const databaseAddress = await run('docker', [...compose, 'port', 'db', '5432'], { quiet: true });
  const client = createServerClient({ baseUrl: `http://${serverAddress}` });
  assert.equal((await client.health()).apiVersion, 1);
  assert.equal((await client.ready()).schemaVersion, 8);
  await run('go', ['-C', 'server/backend', 'test', './internal/database', './internal/httpapi', './internal/identity', '-v', '-count=1'], {
    env: { ...env, YUANPU_TEST_DATABASE_URL: `postgres://yuanpu:${password}@${databaseAddress}/yuanpu?sslmode=disable` },
  });
  await run('docker', [...compose, 'run', '--rm', 'migrate']);
  assert.equal((await client.ready()).status, 'ready');
  await run('docker', [...compose, 'stop', 'db']);
  assert.equal((await client.health()).status, 'ok');
  await assert.rejects(client.ready(), (error) => error instanceof ServerApiError && error.code === 'DATABASE_UNAVAILABLE');
  await run('docker', [...compose, 'start', 'db']);
  await waitReady(client);
  await run('docker', [...compose, 'restart', 'server']);
  // Docker may reassign a randomly published host port when restarting a container.
  const restartedAddress = await run('docker', [...compose, 'port', 'server', '8788'], { quiet: true });
  await waitReady(createServerClient({ baseUrl: `http://${restartedAddress}` }));
  console.log('PASS: Compose build/start, pgvector migration, real SDK calls, migration integrity/idempotency, database outage/recovery, restart.');
} catch (error) {
  await run('docker', [...compose, 'logs', '--no-color', '--tail', '40'], { timeoutMs: 60_000 }).catch(() => {});
  throw error;
} finally {
  try {
    // Only this randomly named test project and its own volume are removed.
    await run('docker', [...compose, 'down', '--volumes', '--remove-orphans', '--rmi', 'local'], { timeoutMs: 60_000 });
  } finally {
    await rm(temporary, { recursive: true, force: true });
  }
}

async function waitReady(client) {
  const deadline = Date.now() + 15_000;
  while (true) {
    try { assert.equal((await client.ready()).status, 'ready'); break; }
    catch (error) {
      if (Date.now() >= deadline) throw error;
      await new Promise((resolve) => setTimeout(resolve, 200));
    }
  }
}

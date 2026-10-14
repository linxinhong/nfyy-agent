import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

import { resolveDevCapabilityEnvironment } from './dev-capabilities.mjs';

const root = fileURLToPath(new URL('../../..', import.meta.url));
const children = [];

function run(command, args, options = {}) {
  const child = spawn(command, args, { cwd: root, stdio: 'inherit', ...options });
  children.push(child);
  return child;
}

const pnpm = process.platform === 'win32' ? 'pnpm.cmd' : 'pnpm';
const capabilityEnvironment = await resolveDevCapabilityEnvironment(root, process.env, async () => {
  console.log('Building signed local capability artifacts for development...');
  await new Promise((resolve, reject) => {
    const child = run(pnpm, ['build:python-artifact']);
    child.once('error', reject);
    child.once('exit', (code, signal) => {
      if (code === 0) resolve();
      else reject(new Error(`Development capability build failed: ${signal ?? code}`));
    });
  });
});
const serverUrl = process.env.YUANPU_SERVER_URL || 'http://127.0.0.1:8788';
const catalogUrl = process.env.YUANPU_CATALOG_URL || serverUrl;
async function serverIsRunning() {
  try {
    const response = await fetch(new URL('v1/health', `${serverUrl.replace(/\/$/, '')}/`), { signal: AbortSignal.timeout(1_000) });
    const body = await response.json();
    return response.ok && body.service === 'yuanpu-server' && body.apiVersion === 1;
  } catch { return false; }
}
const server = !process.env.YUANPU_SERVER_URL && !(await serverIsRunning())
  ? run(pnpm, ['dev:server'], {
  env: { ...process.env, ...capabilityEnvironment },
  }) : undefined;
const vite = run(pnpm, ['--filter', '@yuanpu-agent/app', 'dev', '--host', '127.0.0.1']);

let rendererReady = false;
let serverReady = false;
for (let attempt = 0; attempt < 100; attempt += 1) {
  try {
    const rendererResponse = await fetch('http://127.0.0.1:5173');
    rendererReady = rendererResponse.ok;
  } catch {}
  serverReady = await serverIsRunning();
  if (rendererReady && serverReady) break;
  await new Promise((resolve) => setTimeout(resolve, 100));
}

if (!rendererReady || !serverReady) {
  vite.kill();
  server?.kill();
  throw new Error('Vite or Yuanpu Server did not become ready');
}
try {
  const [catalogResponse, manifestResponse] = await Promise.all([
    fetch(`${catalogUrl}/v1/catalog/search`, { signal: AbortSignal.timeout(3_000) }),
    fetch(`${catalogUrl}/v1/capability-packages/builtin.python.echo/manifest`, { signal: AbortSignal.timeout(3_000) }),
  ]);
  if (!catalogResponse.ok || !manifestResponse.ok) throw new Error('catalog or capability manifest unavailable');
} catch (error) {
  vite.kill();
  server?.kill();
  throw new Error(`Go catalog at ${catalogUrl} is not publishing the local capability artifact; restart it with YUANPU_ARTIFACT_ROOT.`, { cause: error });
}

const electron = run(
  pnpm,
  ['--filter', '@yuanpu-agent/desktop', 'start'],
  {
    env: {
      ...process.env,
      ...capabilityEnvironment,
      YUANPU_RENDERER_URL: 'http://127.0.0.1:5173',
      YUANPU_CATALOG_URL: catalogUrl,
      YUANPU_SERVER_URL: serverUrl,
    },
  },
);

electron.once('exit', (code) => {
  vite.kill();
  server?.kill();
  process.exitCode = code ?? 0;
});

process.once('SIGINT', () => children.forEach((child) => child.kill()));
process.once('SIGTERM', () => children.forEach((child) => child.kill()));

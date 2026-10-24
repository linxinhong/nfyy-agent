import { spawn } from 'node:child_process';
import { createServer } from 'node:net';
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
async function availableRendererPort() {
  for (let port = 5173; port <= 5183; port += 1) {
    const free = await new Promise((resolve) => {
      const probe = createServer();
      probe.once('error', () => resolve(false));
      probe.listen(port, '127.0.0.1', () => probe.close(() => resolve(true)));
    });
    if (free) return port;
  }
  throw new Error('No free renderer port between 5173 and 5183');
}
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
const centerUrl = process.env.YUANPU_CENTER_URL || 'http://127.0.0.1:8790';
async function serverIsRunning() {
  try {
    const response = await fetch(new URL('v1/health', `${serverUrl.replace(/\/$/, '')}/`), { signal: AbortSignal.timeout(1_000) });
    const body = await response.json();
    return response.ok && body.service === 'yuanpu-server' && body.apiVersion === 1;
  } catch { return false; }
}
const server = !process.env.YUANPU_SERVER_URL && !(await serverIsRunning())
  ? run(pnpm, ['dev:server:legacy'], {
  env: { ...process.env, ...capabilityEnvironment },
  }) : undefined;
async function centerIsRunning() {
  try {
    const response = await fetch(new URL('/healthz', centerUrl), { signal: AbortSignal.timeout(1_000) });
    const body = await response.json();
    return response.ok && body.status === 'ok' && typeof body.version === 'string';
  } catch { return false; }
}
const center = !process.env.YUANPU_CENTER_URL && process.env.EAP_DATABASE_URL && !(await centerIsRunning())
  ? run(pnpm, ['dev:server'], {
    env: {
      ...process.env,
      ...(capabilityEnvironment.YUANPU_ARTIFACT_ROOT
        ? { EAP_ARTIFACT_ROOT: capabilityEnvironment.YUANPU_ARTIFACT_ROOT }
        : {}),
    },
  }) : undefined;
const rendererPort = await availableRendererPort();
const rendererUrl = `http://127.0.0.1:${rendererPort}`;
const vite = run(pnpm, ['--filter', '@yuanpu-agent/app', 'dev', '--host', '127.0.0.1', '--port', String(rendererPort), '--strictPort']);

let rendererReady = false;
let serverReady = false;
let centerReady = false;
for (let attempt = 0; attempt < 100; attempt += 1) {
  try {
    const rendererResponse = await fetch(rendererUrl);
    rendererReady = rendererResponse.ok;
  } catch {}
  serverReady = await serverIsRunning();
  centerReady = !center || await centerIsRunning();
  if (rendererReady && serverReady && centerReady) break;
  await new Promise((resolve) => setTimeout(resolve, 100));
}

if (!rendererReady || !serverReady || !centerReady) {
  vite.kill();
  server?.kill();
  center?.kill();
  throw new Error('Renderer, catalog service, or knowledge service center did not become ready');
}
if (!(await centerIsRunning())) console.warn(`知识服务中心未在 ${centerUrl} 启动；配置 EAP_DATABASE_URL 与 EAP_REDIS_ADDR 后可随桌面开发进程启动。`);
try {
  const [catalogResponse, manifestResponse] = await Promise.all([
    fetch(`${catalogUrl}/v1/catalog/search`, { signal: AbortSignal.timeout(3_000) }),
    fetch(`${catalogUrl}/v1/capability-packages/builtin.python.echo/manifest`, { signal: AbortSignal.timeout(3_000) }),
  ]);
  if (!catalogResponse.ok || !manifestResponse.ok) throw new Error('catalog or capability manifest unavailable');
} catch (error) {
  vite.kill();
  server?.kill();
  center?.kill();
  throw new Error(`Go catalog at ${catalogUrl} is not publishing the local capability artifact; restart it with YUANPU_ARTIFACT_ROOT.`, { cause: error });
}

const electron = run(
  pnpm,
  ['--filter', '@yuanpu-agent/desktop', 'start'],
  {
    env: {
      ...process.env,
      ...capabilityEnvironment,
      YUANPU_RENDERER_URL: rendererUrl,
      YUANPU_CATALOG_URL: catalogUrl,
      YUANPU_SERVER_URL: serverUrl,
      YUANPU_CENTER_URL: centerUrl,
    },
  },
);

electron.once('exit', (code) => {
  vite.kill();
  server?.kill();
  center?.kill();
  process.exitCode = code ?? 0;
});

process.once('SIGINT', () => children.forEach((child) => child.kill()));
process.once('SIGTERM', () => children.forEach((child) => child.kill()));

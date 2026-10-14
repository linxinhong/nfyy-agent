import { execFile } from 'node:child_process';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { mkdir } from 'node:fs/promises';
import { createInterface } from 'node:readline';
import { join, resolve } from 'node:path';
import { promisify } from 'node:util';

const runFile = promisify(execFile);
const backend = resolve(import.meta.dirname, '../../../server/backend');
const binary = join(backend, 'dist', process.platform === 'win32' ? 'yuanpu-server.exe' : 'yuanpu-server');
let buildPromise;

async function buildGoServer() {
  buildPromise ??= (async () => {
    await mkdir(join(backend, 'dist'), { recursive: true });
    await runFile('go', ['-C', backend, 'build', '-o', binary, './cmd/yuanpu-server']);
  })();
  await buildPromise;
}

export async function startGoCatalog(artifactRoot) {
  await buildGoServer();
  const child = spawn(binary, ['serve'], {
    env: {
      ...process.env,
      YUANPU_SERVER_LISTEN: '127.0.0.1:0',
      YUANPU_DATABASE_URL: '',
      YUANPU_ARTIFACT_ROOT: artifactRoot,
    },
    stdio: ['ignore', 'ignore', 'pipe'],
  });
  const exited = once(child, 'exit');
  const lines = createInterface({ input: child.stderr });
  let timeout;
  try {
    const address = await Promise.race([
      new Promise((resolveAddress, reject) => {
        child.once('error', reject);
        lines.on('line', (line) => {
          try {
            const event = JSON.parse(line);
            if (event.msg === 'listening') resolveAddress(event.address);
          } catch { /* Ignore non-JSON diagnostics. */ }
        });
      }),
      exited.then(([code]) => { throw new Error(`Go catalog exited before listening (${code})`); }),
      new Promise((_, reject) => { timeout = setTimeout(() => reject(new Error('Go catalog startup timed out')), 15_000); }),
    ]);
    return {
      origin: `http://${address}`,
      async close() {
        if (child.exitCode === null && child.signalCode === null) child.kill('SIGTERM');
        const force = setTimeout(() => child.kill('SIGKILL'), 12_000);
        try { await exited; } finally { clearTimeout(force); lines.close(); }
      },
    };
  } catch (error) {
    child.kill('SIGTERM');
    await exited.catch(() => {});
    lines.close();
    throw error;
  } finally {
    clearTimeout(timeout);
  }
}

export async function forwardGoCatalog(origin, request, response) {
  try {
    const upstream = await fetch(new URL(request.url ?? '/', origin), {
      method: request.method,
      signal: AbortSignal.timeout(30_000),
    });
    const body = Buffer.from(await upstream.arrayBuffer());
    response.statusCode = upstream.status;
    for (const name of ['content-type', 'cache-control', 'x-request-id']) {
      const value = upstream.headers.get(name);
      if (value) response.setHeader(name, value);
    }
    response.end(body);
  } catch {
    response.writeHead(502).end();
  }
}

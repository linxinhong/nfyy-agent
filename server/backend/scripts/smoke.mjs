import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';
import { createServerClient, ServerApiError } from '../../../packages/yuanpu-server-client/dist/index.mjs';

const executable = process.platform === 'win32' ? 'yuanpu-server.exe' : 'yuanpu-server';
const useRunner = process.argv.includes('--dev-runner');
const child = spawn(useRunner ? process.execPath : fileURLToPath(new URL(`../dist/${executable}`, import.meta.url)),
  useRunner ? [fileURLToPath(new URL('./go.mjs', import.meta.url)), 'dev'] : ['serve'], {
  env: { ...process.env, YUANPU_SERVER_LISTEN: '127.0.0.1:0', YUANPU_DATABASE_URL: '', YUANPU_ARTIFACT_ROOT: '' },
  stdio: ['ignore', 'ignore', 'pipe'],
});
const exited = new Promise((resolve, reject) => {
  child.on('error', reject);
  child.on('exit', (code, signal) => resolve({ code, signal }));
});
// Observe early failure even while waiting for the listening event.
exited.catch(() => {});
let timer;
try {
  const address = await Promise.race([
    new Promise((resolve) => {
      const lines = createInterface({ input: child.stderr });
      lines.on('line', (line) => {
        try {
          const event = JSON.parse(line);
          if (event.msg === 'listening') resolve(event.address);
        } catch { /* Non-JSON startup diagnostics are not a readiness signal. */ }
      });
    }),
    exited.then(() => { throw new Error('Server exited before listening'); }),
    new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('Server did not start within 10 seconds')), 10_000); }),
  ]);
  clearTimeout(timer);
  const baseUrl = `http://${address}`;
  const client = createServerClient({ baseUrl });
  assert.equal((await client.health()).service, 'yuanpu-server');
  const wiki = await client.wikiPreview();
  assert.equal(wiki.preview, true);
  assert.equal(wiki.entries.length, 3);
  assert.equal(wiki.entries[0].id, 'knowledge-center');
  const catalog = await client.searchCatalog('Python');
  assert.equal(catalog.schemaVersion, 1);
  assert.equal(catalog.items.length, 1);
  assert.equal(catalog.items[0].id, 'builtin.python.echo');
  assert.equal((await client.getCatalogItem('builtin.python.echo')).id, 'builtin.python.echo');
  const unpublished = await fetch(`${baseUrl}/v1/capability-packages/builtin.python.echo/manifest`);
  assert.equal(unpublished.status, 404);
  await assert.rejects(client.ready(), (error) => error instanceof ServerApiError && error.status === 503 && error.code === 'DATABASE_NOT_CONFIGURED' && Boolean(error.requestId));
  const spec = await fetch(`${baseUrl}/openapi.yaml`);
  assert.match(await spec.text(), /operationId: getHealth/);
  const unknown = await fetch(`${baseUrl}/v1/accounts`);
  assert.equal(unknown.status, 404);
  assert.equal((await unknown.json()).error.code, 'NOT_FOUND');
  console.log(`PASS: ${useRunner ? 'dev runner' : 'Go binary'} + TS SDK, wiki preview, catalog, liveness, unconfigured database, OpenAPI and error contract.`);
} finally {
  clearTimeout(timer);
  if (child.exitCode === null && child.signalCode === null) child.kill('SIGTERM');
  const force = setTimeout(() => child.kill('SIGKILL'), 12_000);
  const result = await exited.finally(() => clearTimeout(force));
  assert.equal(result.code, 0, `Server did not shut down cleanly (${result.signal})`);
}

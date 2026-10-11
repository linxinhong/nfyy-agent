import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { once } from 'node:events';
import test from 'node:test';
import { createServerClient, ServerApiError } from '../dist/index.mjs';

async function fixture(context, handler) {
  const server = createServer(handler);
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  context.after(() => new Promise((resolve) => {
    server.closeAllConnections();
    server.close(resolve);
  }));
  return `http://127.0.0.1:${server.address().port}`;
}

test('client uses the generated health and readiness contract behind a URL prefix', async (context) => {
  const paths = [];
  const base = await fixture(context, (request, response) => {
    paths.push(request.url);
    assert.equal(request.headers.accept, 'application/json');
    response.setHeader('content-type', 'application/json');
    response.end(JSON.stringify(request.url.endsWith('/health')
      ? { status: 'ok', service: 'yuanpu-server', version: '0.1.0', apiVersion: 1 }
      : { status: 'ready', schemaVersion: 1 }));
  });
  const client = createServerClient({ baseUrl: `${base}/center/` });
  assert.equal((await client.health()).apiVersion, 1);
  assert.equal((await client.ready()).schemaVersion, 1);
  assert.deepEqual(paths, ['/center/v1/health', '/center/v1/ready']);
});

test('readiness exposes structured errors and request correlation', async (context) => {
  const baseUrl = await fixture(context, (_request, response) => {
    response.writeHead(503, { 'content-type': 'application/json', 'x-request-id': 'request-123' });
    response.end(JSON.stringify({ error: { code: 'MIGRATIONS_REQUIRED', message: 'Run migrations', requestId: 'request-123' } }));
  });
  await assert.rejects(createServerClient({ baseUrl }).ready(), (error) => {
    assert.ok(error instanceof ServerApiError);
    assert.equal(error.status, 503);
    assert.equal(error.code, 'MIGRATIONS_REQUIRED');
    assert.equal(error.requestId, 'request-123');
    return true;
  });
});

test('legacy catalog liveness is not mistaken for a compatible center', async (context) => {
  const baseUrl = await fixture(context, (_request, response) => {
    response.setHeader('content-type', 'application/json');
    response.end(JSON.stringify({ status: 'ok', schemaVersion: 1, items: 4 }));
  });
  await assert.rejects(createServerClient({ baseUrl }).health(), { code: 'INVALID_RESPONSE' });
});

test('timeout and caller cancellation bound network waits', async (context) => {
  const baseUrl = await fixture(context, () => {});
  await assert.rejects(createServerClient({ baseUrl, timeoutMs: 50 }).health(), { name: 'TimeoutError' });
  const controller = new AbortController();
  const pending = createServerClient({ baseUrl }).health({ signal: controller.signal });
  controller.abort();
  await assert.rejects(pending, { name: 'AbortError' });
});

test('client rejects redirects instead of silently contacting another endpoint', async (context) => {
  let followed = false;
  const baseUrl = await fixture(context, (request, response) => {
    if (request.url === '/other') followed = true;
    response.writeHead(302, { location: '/other' });
    response.end();
  });
  await assert.rejects(createServerClient({ baseUrl }).health());
  assert.equal(followed, false);
});

test('configuration rejects credentials and invalid timeouts', () => {
  for (const baseUrl of ['file:///tmp/server', 'http://user:secret@localhost', 'http://localhost/?secret=x', 'http://localhost/#fragment']) {
    assert.throws(() => createServerClient({ baseUrl }), TypeError);
  }
  for (const timeoutMs of [0, -1, NaN, 1.1, Infinity, 300001]) {
    assert.throws(() => createServerClient({ baseUrl: 'http://localhost', timeoutMs }), RangeError);
  }
});

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

test('wiki preview reads validated sample entries from the server', async (context) => {
  const baseUrl = await fixture(context, (request, response) => {
    assert.equal(request.url, '/v1/preview/wiki');
    response.setHeader('content-type', 'application/json');
    response.end(JSON.stringify({ preview: true, entries: [{
      id: 'knowledge-center', title: '知识中心', summary: '示例', body: '正文',
      level: '示例', experience: 12, updatedAt: '2026-10-11T00:00:00Z', links: [],
    }] }));
  });
  const result = await createServerClient({ baseUrl }).wikiPreview();
  assert.equal(result.entries[0].title, '知识中心');
});

test('wiki preview rejects an incompatible payload', async (context) => {
  const baseUrl = await fixture(context, (_request, response) => {
    response.setHeader('content-type', 'application/json');
    response.end(JSON.stringify({ preview: true, entries: [{ id: 'broken' }] }));
  });
  await assert.rejects(createServerClient({ baseUrl }).wikiPreview(), { code: 'INVALID_RESPONSE' });
});

test('industry and BookStack methods stay behind the authenticated server client', async (context) => {
  const calls = [];
  const baseUrl = await fixture(context, async (request, response) => {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    calls.push({ method: request.method, path: request.url, authorization: request.headers.authorization, body: Buffer.concat(chunks).toString() });
    response.setHeader('content-type', 'application/json');
    if (request.url === '/v1/industry') response.end(JSON.stringify({ id: 'legal', name: '律所知识', knowledgeTypes: [], areas: [], requiredSections: [], confidences: [] }));
    else if (request.url === '/v1/industry/templates/case_experience') response.end(JSON.stringify({ markdown: '# 办案经验' }));
    else if (request.url === '/v1/industry/validate') response.end(JSON.stringify({ valid: false, violations: [{ field: 'source', message: 'required' }] }));
    else response.end(JSON.stringify({ status: 'ready' }));
  });
  const client = createServerClient({ baseUrl, token: 'member-session' });
  assert.equal((await client.getIndustry()).id, 'legal');
  assert.equal(await client.getIndustryTemplate('case_experience'), '# 办案经验');
  assert.equal((await client.validateIndustrySubmission({
    title: '', type: 'case_experience', author: '', submittedAt: '', area: '', source: '', confidence: '',
    redacted: false, fields: {}, sections: {},
  })).violations[0].field, 'source');
  assert.equal((await client.bookStackStatus()).status, 'ready');
  assert.deepEqual(calls.map(({ method, path }) => `${method} ${path}`), [
    'GET /v1/industry', 'GET /v1/industry/templates/case_experience', 'POST /v1/industry/validate',
    'GET /v1/integrations/bookstack/status',
  ]);
  assert.ok(calls.every(({ authorization }) => authorization === 'Bearer member-session'));
  assert.equal(JSON.parse(calls[2].body).type, 'case_experience');
});

test('catalog client searches and reads Go directory entries', async (context) => {
  const baseUrl = await fixture(context, (request, response) => {
    response.setHeader('content-type', 'application/json');
    const item = {
      id: 'builtin.python.echo', name: 'yuanpu-echo-mcp', displayName: 'Python 示例能力',
      version: '0.1.0', description: 'Example', publisher: 'yuanpu',
      source: 'artifact:/v1/capability-packages/builtin.python.echo/manifest',
      components: ['connector'], permissions: ['background'],
    };
    if (request.url === '/v1/catalog/search?q=Python') response.end(JSON.stringify({ schemaVersion: 1, items: [item] }));
    else if (request.url === '/v1/catalog/items/builtin.python.echo') response.end(JSON.stringify(item));
    else response.writeHead(404).end();
  });
  const client = createServerClient({ baseUrl });
  assert.equal((await client.searchCatalog('Python')).items[0].id, 'builtin.python.echo');
  assert.deepEqual((await client.getCatalogItem('builtin.python.echo')).components, ['connector']);
});

test('account client sends bearer sessions only to the configured server', async (context) => {
  const requests = [];
  const baseUrl = await fixture(context, async (request, response) => {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    requests.push({ path: request.url, method: request.method, authorization: request.headers.authorization, body: Buffer.concat(chunks).toString() });
    response.setHeader('content-type', 'application/json');
    if (request.url === '/v1/auth/api-key') response.end(JSON.stringify({ user: { id: 'a', email: 'a@example.com', name: 'A', role: 'admin', teamId: null, active: true }, token: 'session-token' }));
    else if (request.url === '/v1/admin/teams' && request.method === 'POST') response.end(JSON.stringify({ id: 'team', name: 'Team' }));
    else response.end(JSON.stringify([]));
  });
  const anonymous = createServerClient({ baseUrl });
  const session = await anonymous.loginWithAPIKey('ypk_test');
  const admin = createServerClient({ baseUrl, token: session.token });
  assert.equal((await admin.createTeam('Team')).id, 'team');
  assert.deepEqual(await admin.listTeams(), []);
  assert.equal(requests[0].authorization, undefined);
  assert.equal(requests[1].authorization, 'Bearer session-token');
  assert.equal(requests[2].authorization, 'Bearer session-token');
  assert.equal(JSON.parse(requests[0].body).apiKey, 'ypk_test');
});

test('API key client exchanges a secret and lets an administrator issue, list and revoke keys', async (context) => {
  const calls = [];
  const baseUrl = await fixture(context, async (request, response) => {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    calls.push({ path: request.url, method: request.method, authorization: request.headers.authorization,
      body: Buffer.concat(chunks).toString() });
    response.setHeader('content-type', 'application/json');
    const key = { id: 'key-1', userId: 'member-1', issuedBy: 'admin-1', name: 'Desktop', keyPrefix: 'ypk_sample',
      createdAt: '2026-10-12T00:00:00Z', expiresAt: '2027-01-10T00:00:00Z', lastUsedAt: null, revokedAt: null };
    if (request.url === '/v1/auth/api-key') response.end(JSON.stringify({ user: {
      id: 'member-1', email: 'member@example.com', name: 'Member', role: 'member', teamId: null, active: true,
    }, token: 'member-session' }));
    else if (request.method === 'POST') {
      response.writeHead(201);
      response.end(JSON.stringify({ apiKey: key, secret: 'ypk_secret' }));
    } else if (request.method === 'GET') response.end(JSON.stringify([key]));
    else { response.writeHead(204); response.end(); }
  });
  const anonymous = createServerClient({ baseUrl });
  assert.equal((await anonymous.loginWithAPIKey('ypk_secret')).token, 'member-session');
  const admin = createServerClient({ baseUrl, token: 'admin-session' });
  assert.equal((await admin.issueAPIKey({ userId: 'member-1', name: 'Desktop', expiresInDays: 90 })).apiKey.id, 'key-1');
  assert.equal((await admin.listAPIKeys())[0].keyPrefix, 'ypk_sample');
  await admin.revokeAPIKey('key-1');
  assert.deepEqual(calls.map(({ method, path }) => `${method} ${path}`), [
    'POST /v1/auth/api-key', 'POST /v1/admin/api-keys', 'GET /v1/admin/api-keys', 'DELETE /v1/admin/api-keys/key-1',
  ]);
  assert.equal(calls[0].authorization, undefined);
  assert.equal(JSON.parse(calls[0].body).apiKey, 'ypk_secret');
  assert.equal(calls[1].authorization, 'Bearer admin-session');
});

test('role, workflow, log and binary data methods use the configured prefix and bearer token', async (context) => {
  const calls = [];
  const base = await fixture(context, async (request, response) => {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    calls.push({ path: request.url, method: request.method, authorization: request.headers.authorization,
      filename: request.headers['x-file-name'], body: Buffer.concat(chunks) });
    if (request.url.endsWith('/data-uploads/file-1') && request.method === 'GET') {
      response.writeHead(200, { 'content-type': 'text/plain' }); response.end('test data'); return;
    }
    response.setHeader('content-type', 'application/json');
    if (request.url.endsWith('/admin/roles') && request.method === 'GET') response.end(JSON.stringify([{ role: 'member', action: 'read', scope: 'team' }]));
    else if (request.url.endsWith('/admin/roles/member/read')) response.end(JSON.stringify({ role: 'member', action: 'read', scope: 'all' }));
    else if (request.url.endsWith('/resources/mine')) response.end('[]');
    else if (request.url.endsWith('/logs')) { response.writeHead(201); response.end('{"id":"log-1"}'); }
    else { response.writeHead(201); response.end(JSON.stringify({ id: 'file-1', filename: 'note.txt', contentType: 'text/plain', byteSize: 9, sha256: 'a', createdAt: '2026-10-15T00:00:00Z' })); }
  });
  const api = createServerClient({ baseUrl: `${base}/center/`, token: 'session' });
  assert.equal((await api.listRolePermissions())[0].scope, 'team');
  assert.equal((await api.updateRolePermission('member', 'read', 'all')).scope, 'all');
  assert.deepEqual(await api.listMyResources(), []);
  assert.equal((await api.postOperationLog({ idempotencyKey: 'test-run-1', kind: 'agent_run', outcome: 'succeeded', durationMs: 10, occurredAt: '2026-10-15T00:00:00Z' })).id, 'log-1');
  assert.equal((await api.uploadData('note.txt', 'text/plain', new TextEncoder().encode('test data'), 'resource-1')).id, 'file-1');
  assert.equal(await (await api.downloadData('file-1')).text(), 'test data');
  assert.deepEqual(calls.map((call) => `${call.method} ${call.path}`), [
    'GET /center/v1/admin/roles', 'PUT /center/v1/admin/roles/member/read', 'GET /center/v1/resources/mine',
    'POST /center/v1/logs', 'POST /center/v1/data-uploads?resourceId=resource-1', 'GET /center/v1/data-uploads/file-1',
  ]);
  assert.ok(calls.every((call) => call.authorization === 'Bearer session'));
  assert.equal(calls[4].filename, 'note.txt');
  assert.equal(calls[4].body.toString(), 'test data');
});

test('resource client passes bearer auth and preserves structured denials', async (context) => {
  const requests = [];
  const baseUrl = await fixture(context, (request, response) => {
    requests.push({ method: request.method, path: request.url, authorization: request.headers.authorization });
    response.setHeader('content-type', 'application/json');
    if (request.url?.startsWith('/v1/resources?')) {
      response.end(JSON.stringify([]));
      return;
    }
    response.setHeader('x-request-id', 'resource-request');
    response.writeHead(404);
    response.end(JSON.stringify({ error: { code: 'NOT_FOUND', message: 'Resource not found', requestId: 'resource-request' } }));
  });
  const client = createServerClient({ baseUrl, token: 'member-token' });
  assert.deepEqual(await client.listResources({ kind: 'wiki', q: '案号 123' }), []);
  assert.equal(requests[0].authorization, 'Bearer member-token');
  assert.equal(requests[0].method, 'GET');
  assert.match(requests[0].path, /kind=wiki/);
  assert.match(requests[0].path, /q=%E6%A1%88%E5%8F%B7/);
  await assert.rejects(client.getResource('missing'), { code: 'NOT_FOUND', status: 404, requestId: 'resource-request' });
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

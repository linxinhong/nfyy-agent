import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { once } from 'node:events';
import test from 'node:test';

import { getServerCenterStatus, getServerWikiPreview, ServerCenterSession } from '../dist/server-center.cjs';

test('desktop host connects to the center SDK and keeps wiki preview separate from local knowledge', async (context) => {
  const requests = [];
  const server = createServer((request, response) => {
    requests.push(request.url);
    response.setHeader('content-type', 'application/json');
    if (request.url === '/v1/health') {
      response.end(JSON.stringify({ status: 'ok', service: 'yuanpu-server', version: '0.1.0', apiVersion: 1 }));
    } else if (request.url === '/v1/ready') {
      response.end(JSON.stringify({ status: 'ready', schemaVersion: 1 }));
    } else {
      response.end(JSON.stringify({ preview: true, entries: [{
        id: 'sample', title: '示例', summary: '服务端示例', body: '正文',
        level: '示例', experience: 1, updatedAt: '2026-10-11T00:00:00Z', links: [],
      }] }));
    }
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  context.after(() => new Promise((resolve) => server.close(resolve)));
  const baseUrl = `http://127.0.0.1:${server.address().port}`;
  const status = await getServerCenterStatus(baseUrl);
  const wiki = await getServerWikiPreview(baseUrl);
  assert.equal(status.state, 'ready');
  assert.equal(status.schemaVersion, 1);
  assert.equal(wiki.entries[0].title, '示例');
  assert.deepEqual(requests, ['/v1/health', '/v1/ready', '/v1/preview/wiki']);
});

test('desktop host reports a stopped server without exposing a renderer-controlled URL', async () => {
  const status = await getServerCenterStatus('http://127.0.0.1:1');
  assert.equal(status.state, 'offline');
  assert.equal(status.issueCode, 'NETWORK_UNAVAILABLE');
});

test('center session keeps the token in the host and rechecks access when opening a resource', async (context) => {
  const requests = [];
  const key = `ypk_${'a'.repeat(43)}`;
  let accessible = true;
  const resource = {
    id: 'news-1', kind: 'news', ownerId: 'member-1', visibility: 'organization', teamId: null,
    revision: 1, status: 'approved', title: '组织资讯', summary: '最新动态', body: '正文', details: {},
  };
  const server = createServer(async (request, response) => {
    requests.push({ url: request.url, authorization: request.headers.authorization });
    response.setHeader('content-type', 'application/json');
    if (request.url === '/v1/auth/api-key') {
      let body = '';
      for await (const chunk of request) body += chunk;
      assert.equal(JSON.parse(body).apiKey, key);
      response.end(JSON.stringify({ token: 'private-token', user: {
        id: 'member-1', email: 'member@example.test', name: '成员', role: 'member', teamId: null, active: true,
      } }));
    } else if (request.url === '/v1/auth/logout') {
      response.end('{}');
    } else if (request.url === '/v1/catalog/search?q=') {
      response.end(JSON.stringify({ schemaVersion: 1, items: [{
        id: 'cap-1', name: 'cap', displayName: '能力', description: '说明', version: '1.0',
        publisher: '中心', source: 'artifact:cap', components: ['skill'], permissions: ['instructions'],
      }] }));
    } else if (request.url === '/v1/resources' || request.url === '/v1/resources?q=') {
      if (request.headers.authorization !== 'Bearer private-token') response.writeHead(401);
      response.end(request.headers.authorization === 'Bearer private-token' ? JSON.stringify([resource]) : JSON.stringify({ error: { code: 'UNAUTHORIZED', message: 'Unauthorized' } }));
    } else if (request.url === '/v1/resources/news-1') {
      if (!accessible) response.writeHead(403);
      response.end(accessible ? JSON.stringify(resource) : JSON.stringify({ error: { code: 'FORBIDDEN', message: 'Forbidden' } }));
    } else {
      response.writeHead(404).end();
    }
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  context.after(() => new Promise((resolve) => server.close(resolve)));
  const session = new ServerCenterSession(`http://127.0.0.1:${server.address().port}`);

  assert.deepEqual(await session.listResources(''), { ok: false, code: 'UNAUTHORIZED' });
  assert.equal((await session.searchCatalog(''))[0].displayName, '能力');
  const login = await session.useAPIKey(key);
  assert.equal(login.ok, true);
  assert.equal('token' in login, false);
  assert.equal((await session.listResources('')).value[0].title, '组织资讯');
  assert.equal((await session.getResource('news-1')).ok, true);
  accessible = false;
  assert.deepEqual(await session.getResource('news-1'), { ok: false, code: 'FORBIDDEN' });
  await session.logout();
  assert.deepEqual(await session.listResources(''), { ok: false, code: 'UNAUTHORIZED' });
  assert.deepEqual(requests.map((request) => request.url), [
    '/v1/catalog/search?q=', '/v1/auth/api-key', '/v1/resources?q=',
    '/v1/resources/news-1', '/v1/resources/news-1', '/v1/auth/logout',
  ]);
  assert.equal(requests[0].authorization, undefined);
  assert.equal(requests[2].authorization, 'Bearer private-token');
});

test('configured API key restores a session and renews an expired bearer token once', async (context) => {
  const key = `ypk_${'c'.repeat(43)}`;
  let loginCount = 0;
  let resourceCount = 0;
  const server = createServer(async (request, response) => {
    response.setHeader('content-type', 'application/json');
    if (request.url === '/v1/auth/api-key') {
      let body = '';
      for await (const chunk of request) body += chunk;
      if (JSON.parse(body).apiKey !== key) { response.writeHead(401).end('{}'); return; }
      loginCount++;
      response.end(JSON.stringify({ token: `token-${loginCount}`, user: {
        id: 'member-1', email: 'member@example.test', name: '成员', role: 'member', teamId: null, active: true,
      } }));
    } else if (request.url === '/v1/resources?q=') {
      response.end('[]');
    } else if (request.url === '/v1/resources/news-1') {
      resourceCount++;
      if (request.headers.authorization === 'Bearer token-1') {
        response.writeHead(401).end(JSON.stringify({ error: { code: 'UNAUTHORIZED', message: 'Expired' } }));
      } else {
        response.end(JSON.stringify({
          id: 'news-1', kind: 'news', ownerId: 'member-1', visibility: 'organization', teamId: null,
          revision: 1, status: 'approved', title: '资讯', summary: '', body: '正文', details: {},
        }));
      }
    } else if (request.url === '/v1/auth/logout') response.writeHead(204).end();
    else response.writeHead(404).end();
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  context.after(() => new Promise((resolve) => server.close(resolve)));
  const session = new ServerCenterSession(`http://127.0.0.1:${server.address().port}`, async () => key);
  assert.equal((await session.restore()).ok, true);
  assert.equal(await session.portalToken(), 'token-1');
  assert.equal((await session.listResources('')).ok, true);
  assert.equal((await session.getResource('news-1')).value.title, '资讯');
  assert.equal(loginCount, 2);
  assert.equal(resourceCount, 2);
  await session.logout();
  assert.deepEqual(await session.restore(), { ok: false, code: 'UNAUTHORIZED' });
});

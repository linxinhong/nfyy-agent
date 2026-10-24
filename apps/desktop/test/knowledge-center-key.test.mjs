import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { once } from 'node:events';
import test from 'node:test';

import { checkKnowledgeCenterKey } from '../dist/knowledge-center-key.cjs';

test('knowledge center validates an EAP employee key at /enterprise/v1/me', async (context) => {
  const key = `agk_live_${'a'.repeat(32)}`;
  const server = createServer((request, response) => {
    assert.equal(request.url, '/enterprise/v1/me');
    if (request.headers.authorization !== `Bearer ${key}`) { response.writeHead(401).end(); return; }
    response.setHeader('content-type', 'application/json');
    response.end(JSON.stringify({ user: { id: 'u1', email: 'member@example.test', display_name: '成员' },
      workspaces: [{ id: 'w1', roles: ['admin'] }] }));
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  context.after(() => new Promise((resolve) => server.close(resolve)));
  const endpoint = `http://127.0.0.1:${server.address().port}`;

  assert.deepEqual(await checkKnowledgeCenterKey(endpoint, key), { ok: true,
    value: { id: 'u1', email: 'member@example.test', name: '成员', role: 'admin' } });
  assert.deepEqual(await checkKnowledgeCenterKey(endpoint, `ypk_${'a'.repeat(43)}`),
    { ok: false, code: 'UNAUTHORIZED' });
  assert.deepEqual(await checkKnowledgeCenterKey(endpoint, `agk_test_${'b'.repeat(32)}`),
    { ok: false, code: 'UNAUTHORIZED' });
});

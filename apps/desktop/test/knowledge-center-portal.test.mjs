import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { once } from 'node:events';
import test from 'node:test';

import { forwardKnowledgeCenterRequest, getKnowledgeCenterProfile } from '../dist/knowledge-center-portal.cjs';

test('desktop key restores a profile and forwards only center API calls', async (context) => {
  const key = `agk_live_${'a'.repeat(32)}`;
  const seen = [];
  const server = createServer((request, response) => {
    seen.push({ url: request.url, authorization: request.headers.authorization, workspace: request.headers['x-agent-workspace'] });
    response.setHeader('content-type', 'application/json');
    if (request.headers.authorization !== `Bearer ${key}`) { response.writeHead(401).end('{}'); return; }
    if (request.url === '/enterprise/v1/me') {
      response.end(JSON.stringify({ user: { id: 'u1', email: 'member@example.test', display_name: '成员' },
        workspaces: [{ id: 'w1', name: '团队一' }], default_workspace: 'w1' }));
      return;
    }
    response.end(JSON.stringify({ resources: [{ id: 'news-1', title: '团队资讯' }] }));
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  context.after(() => new Promise((resolve) => server.close(resolve)));
  const endpoint = `http://127.0.0.1:${server.address().port}`;

  const profile = await getKnowledgeCenterProfile(endpoint, key);
  assert.equal(profile?.user.email, 'member@example.test');
  const result = await forwardKnowledgeCenterRequest(endpoint, key,
    { path: '/enterprise/v1/resources?q=team', workspace: 'w1' });
  assert.equal(result.status, 200);
  assert.deepEqual(result.body, { resources: [{ id: 'news-1', title: '团队资讯' }] });
  assert.equal(seen[1].authorization, `Bearer ${key}`);
  assert.equal(seen[1].workspace, 'w1');
  assert.equal(JSON.stringify(result).includes(key), false);

  await assert.rejects(forwardKnowledgeCenterRequest(endpoint, key, { path: 'https://example.test/' }), /Invalid center request path/);
  await assert.rejects(forwardKnowledgeCenterRequest(endpoint, key, { path: '/enterprise/v1/../../outside' }), /Invalid center request path/);
});

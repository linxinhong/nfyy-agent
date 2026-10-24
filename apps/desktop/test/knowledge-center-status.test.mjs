import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { once } from 'node:events';
import test from 'node:test';

import { getKnowledgeCenterStatus } from '../dist/knowledge-center-status.cjs';

test('knowledge service center checks EAP health and dependency readiness', async (context) => {
  let ready = false;
  const server = createServer((request, response) => {
    response.setHeader('content-type', 'application/json');
    if (request.url === '/healthz') response.end(JSON.stringify({ status: 'ok', version: '1.2.3' }));
    else if (request.url === '/readyz') {
      response.writeHead(ready ? 200 : 503);
      response.end(JSON.stringify({ status: ready ? 'ready' : 'not_ready' }));
    } else response.writeHead(404).end();
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  context.after(() => new Promise((resolve) => server.close(resolve)));
  const endpoint = `http://127.0.0.1:${server.address().port}`;

  assert.deepEqual(await getKnowledgeCenterStatus(endpoint).then(({ state, version, issueCode }) => ({ state, version, issueCode })),
    { state: 'running', version: '1.2.3', issueCode: 'DEPENDENCY_UNAVAILABLE' });
  ready = true;
  assert.equal((await getKnowledgeCenterStatus(endpoint)).state, 'ready');
  assert.equal((await getKnowledgeCenterStatus('http://127.0.0.1:1')).state, 'offline');
});

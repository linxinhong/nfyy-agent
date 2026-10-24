import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { createYuanpuChatSession, parseCenterConnection } from '../dist/index.mjs';

const centerKey = `agk_test_${'A'.repeat(32)}`;

test('center connection rejects remote plain HTTP and secrets in the endpoint', () => {
  assert.throws(() => parseCenterConnection({ endpoint: 'http://example.com', apiKey: centerKey }), /中心端点/);
  assert.throws(() => parseCenterConnection({ endpoint: 'https://user:pass@example.com', apiKey: centerKey }), /中心端点/);
  assert.throws(() => parseCenterConnection({ endpoint: 'https://example.com', apiKey: 'invalid' }), /API key/);
});

test('a protected center skill routes a real Pi chat through the center and never stores its body', async (context) => {
  const requests = [];
  let active = true;
  let routed = true;
  let authorized = true;
  const server = createServer(async (request, response) => {
    const path = new URL(request.url, 'http://127.0.0.1').pathname;
    if (path.startsWith('/enterprise/') || path.startsWith('/v1/')) {
      assert.equal(request.headers.authorization, `Bearer ${centerKey}`);
      if (!authorized) {
        response.writeHead(401, { 'content-type': 'application/json' });
        response.end(JSON.stringify({ code: 'unauthenticated' }));
        return;
      }
    }
    if (path === '/enterprise/v1/me') {
      response.setHeader('content-type', 'application/json');
      response.end(JSON.stringify({ default_workspace: 'secops', workspaces: [{ id: 'secops' }] }));
      return;
    }
    if (path === '/enterprise/v1/me/skills' || path === '/v1/models' || path === '/v1/chat/completions') {
      assert.equal(request.headers['x-agent-workspace'], 'secops');
    }
    if (path === '/enterprise/v1/me/skills') {
      response.setHeader('content-type', 'application/json');
      response.end(JSON.stringify({ skills: [{ name: 'review-check', protected: true, active }] }));
      return;
    }
    if (path === '/v1/models') {
      response.setHeader('content-type', 'application/json');
      response.end(JSON.stringify({ data: routed ? [{ id: 'fixture' }] : [] }));
      return;
    }
    let body = '';
    for await (const chunk of request) body += chunk;
    requests.push({ path, headers: request.headers, body: JSON.parse(body) });
    response.writeHead(200, { 'content-type': 'text/event-stream' });
    const frame = (delta, finish_reason = null) => `data: ${JSON.stringify({ id: 'fixture',
      object: 'chat.completion.chunk', model: 'fixture', choices: [{ index: 0, delta, finish_reason }] })}\n\n`;
    response.end(`${frame({ role: 'assistant', content: path === '/v1/chat/completions' ? 'center reply' : 'direct reply' })}${frame({}, 'stop')}data: [DONE]\n\n`);
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  context.after(() => new Promise((resolve) => { server.closeAllConnections(); server.close(resolve); }));
  const endpoint = `http://127.0.0.1:${server.address().port}`;
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-center-skill-'));
  context.after(() => rm(root, { recursive: true, force: true }));
  await writeFile(join(root, 'models.json'), JSON.stringify({ providers: { fixture: {
    api: 'openai-completions', baseUrl: `${endpoint}/direct`,
    models: [{ id: 'fixture', name: 'fixture', reasoning: false, input: ['text'], contextWindow: 8192, maxTokens: 256 }],
  } } }));
  const sessions = join(root, 'sessions');
  const chat = await createYuanpuChatSession({
    agentDir: root, modelConfigDir: root, cwd: root, provider: 'fixture', model: 'fixture', apiKey: 'direct-key',
    centerConnection: parseCenterConnection({ endpoint, apiKey: centerKey }),
    piSession: { id: 'a4b66afc-15ac-447c-b394-c43570df5b24', directory: sessions },
    capabilityClient: { async search() { return { matches: [] }; }, async execute() { throw new Error('unused'); } },
  });
  context.after(() => chat.dispose());

  assert.equal((await chat.prompt('Does the center Skill apply?')).message, 'center reply');
  assert.equal(requests[0].path, '/v1/chat/completions');
  assert.equal(requests[0].headers['x-mindlink-skills'], 'review-check');
  assert.equal(requests[0].headers.authorization, `Bearer ${centerKey}`);
  assert.ok(Number.isSafeInteger(requests[0].body.max_tokens) && requests[0].body.max_tokens > 0);
  assert.equal(requests[0].body.max_completion_tokens, undefined);
  authorized = false;
  await assert.rejects(chat.prompt('A revoked key must not bypass the center'), /知识服务中心请求失败（HTTP 401）/);
  assert.equal(requests.length, 1);
  authorized = true;
  active = false;
  assert.equal((await chat.prompt('Use the ordinary model')).message, 'direct reply');
  assert.equal(requests[1].path, '/direct/chat/completions');
  assert.equal(requests[1].headers['x-mindlink-skills'], undefined);

  active = true;
  routed = false;
  await assert.rejects(chat.prompt('No route should fail closed'), /中心尚未配置模型 fixture 的路由/);
  assert.equal(requests.length, 2);
  const saved = await readFile(join(sessions, (await readdir(sessions)).find((name) => name.endsWith('.jsonl'))), 'utf8');
  assert.doesNotMatch(saved, /agk_test_/);
  assert.equal(saved.includes('review-check'), false, saved.match(/.{0,70}review-check.{0,70}/)?.[0]);
  for (const name of await readdir(root)) {
    if (!name.endsWith('.json')) continue;
    assert.equal((await readFile(join(root, name), 'utf8')).includes(centerKey), false, `${name} persisted the center key`);
  }
});

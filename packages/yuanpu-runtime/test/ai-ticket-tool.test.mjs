import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { createAiTicketTool, createYuanpuChatSession } from '../dist/index.mjs';

test('main agent can create, inspect, switch, inject into and stop independent tickets', async () => {
  const calls = [];
  const ticket = { id: 'ticket-1', task: 'Research', status: 'completed',
    messages: Array.from({ length: 6 }, (_, index) => ({ id: `m${index}`, role: 'assistant', text: 'x'.repeat(2_500) })),
    activity: Array.from({ length: 13 }, (_, index) => ({ id: `a${index}`, text: 'y'.repeat(1_500) })),
  };
  const parentId = 'work:parent';
  const client = {
    create: async (...args) => { calls.push(['create', ...args]); return ticket; },
    list: async (...args) => { calls.push(['list', ...args]); return [ticket]; },
    get: async (...args) => { calls.push(['get', ...args]); return ticket; },
    message: async (...args) => { calls.push(['message', ...args]); return ticket; },
    cancel: async (...args) => { calls.push(['cancel', ...args]); },
    navigate: async (...args) => { calls.push(['navigate', ...args]); },
  };
  const tool = createAiTicketTool(client, parentId, () => 'parent transcript');
  const invoke = async (params) => JSON.parse((await tool.execute('call-1', params, new AbortController().signal)).content[0].text);
  assert.equal((await invoke({ action: 'create', task: 'Research' })).id, ticket.id);
  assert.deepEqual(calls.shift(), ['create', parentId, 'Research', 'parent transcript', {
    title: undefined, deliverables: undefined, acceptanceCriteria: undefined,
  }]);
  await invoke({ action: 'list' });
  assert.deepEqual(calls.shift(), ['list', parentId]);
  await invoke({ action: 'overview' });
  assert.deepEqual(calls.splice(0, 2), [['navigate', parentId], ['list', parentId]]);
  await invoke({ action: 'open', ticketId: ticket.id });
  assert.deepEqual(calls.splice(0, 2), [['navigate', parentId, ticket.id], ['get', parentId, ticket.id]]);
  const status = await invoke({ action: 'status', ticketId: ticket.id });
  assert.equal(status.messages, undefined);
  assert.equal(status.activity, undefined);
  assert.deepEqual(calls.shift(), ['get', parentId, ticket.id]);
  const progress = await invoke({ action: 'progress', ticketId: ticket.id });
  assert.equal(progress.messages.length, 4);
  assert.equal(progress.messages[0].text.length, 2_000);
  assert.equal(progress.activity.length, 12);
  assert.equal(progress.activity[0].text.length, 1_000);
  assert.deepEqual(calls.shift(), ['get', parentId, ticket.id]);
  await invoke({ action: 'message', ticketId: ticket.id, message: 'Continue' });
  assert.deepEqual(calls.splice(0, 2), [['message', parentId, ticket.id, 'Continue'], ['navigate', parentId, ticket.id]]);
  await invoke({ action: 'inject', ticketId: ticket.id, content: 'New evidence' });
  assert.match(calls.shift()[3], /New evidence/);
  assert.deepEqual(calls.shift(), ['navigate', parentId, ticket.id]);
  await invoke({ action: 'cancel', ticketId: ticket.id });
  assert.deepEqual(calls.shift(), ['cancel', parentId, ticket.id]);
});

test('Work model receives ai_ticket rather than the legacy subagent tool', async (t) => {
  const requests = [];
  const server = createServer(async (request, response) => {
    let body = '';
    for await (const chunk of request) body += chunk;
    requests.push(JSON.parse(body));
    response.writeHead(400, { 'content-type': 'application/json' });
    response.end(JSON.stringify({ error: { message: 'fixture failure' } }));
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise((resolve) => { server.closeAllConnections(); server.close(resolve); }));
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-ai-ticket-routing-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  await writeFile(join(root, 'models.json'), JSON.stringify({ providers: { fixture: {
    baseUrl: `http://127.0.0.1:${server.address().port}/v1`, api: 'openai-completions',
    models: [{ id: 'fixture', name: 'fixture', reasoning: false, input: ['text'], contextWindow: 8192, maxTokens: 1024 }],
  } } }));
  const aiTickets = {
    create: async () => ({}), list: async () => [], get: async () => ({}),
    message: async () => ({}), cancel: async () => {}, navigate: async () => {},
  };
  const chat = await createYuanpuChatSession({
    agentDir: root, modelConfigDir: root, cwd: root, provider: 'fixture', model: 'fixture',
    apiKey: 'fixture-only', includeGlobalMemory: false, aiTickets,
    capabilityClient: { async search() { return { matches: [] }; }, async execute() { throw new Error('unused'); } },
    capabilityContext: { sessionId: randomUUID(), conversationId: `work:${randomUUID()}`,
      workspaceId: root, userId: 'fixture' },
  });
  try { await assert.rejects(chat.prompt('新建一个绘制 UI 的工单'), /模型请求失败/); }
  finally { await chat.dispose(); }
  assert.equal(requests.length, 1);
  const tools = requests[0].tools.map((tool) => tool.function.name);
  assert.ok(tools.includes('ai_ticket'));
  assert.equal(tools.includes('subagent'), false);
});

import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { createYuanpuChatSession, withYuanpuModelCompatibility } from '../dist/index.mjs';

test('LongCat defaults are endpoint scoped and preserve explicit compatibility overrides', () => {
  const base = { api: 'openai-completions', baseUrl: 'https://api.longcat.chat/openai/v1' };
  assert.deepEqual(withYuanpuModelCompatibility(base).compat, {
    maxTokensField: 'max_tokens', thinkingFormat: 'deepseek', supportsReasoningEffort: false,
  });
  const custom = { ...base, compat: { maxTokensField: 'max_completion_tokens', supportsReasoningEffort: true } };
  assert.equal(withYuanpuModelCompatibility(custom).compat.maxTokensField, 'max_completion_tokens');
  assert.equal(withYuanpuModelCompatibility(custom).compat.supportsReasoningEffort, true);
  assert.equal(base.compat, undefined);
  for (const other of [{ ...base, api: 'anthropic-messages' },
    { ...base, baseUrl: 'https://api.longcat.chat.example.test/v1' }, { ...base, baseUrl: 'invalid' }]) {
    assert.equal(withYuanpuModelCompatibility(other), other);
  }
});

test('real Pi serialization uses LongCat thinking controls and records safe phase timings', async (context) => {
  const bodies = [];
  const server = createServer(async (request, response) => {
    let body = '';
    for await (const chunk of request) body += chunk;
    bodies.push(JSON.parse(body));
    response.writeHead(200, { 'content-type': 'text/event-stream', 'x-private-fixture': 'SECRET-HEADER' });
    const chunk = (delta, finish_reason = null) => `data: ${JSON.stringify({ id: 'fixture',
      object: 'chat.completion.chunk', model: 'fixture', choices: [{ index: 0, delta, finish_reason }] })}\n\n`;
    response.write(chunk({ role: 'assistant', content: 'fixture reply' }));
    response.end(`${chunk({}, 'stop')}data: [DONE]\n\n`);
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  context.after(() => new Promise((resolve) => { server.closeAllConnections(); server.close(resolve); }));
  const originalFetch = globalThis.fetch;
  globalThis.fetch = (input, init) => originalFetch(`http://127.0.0.1:${server.address().port}${new URL(String(input)).pathname}`, init);
  context.after(() => { globalThis.fetch = originalFetch; });
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-model-request-'));
  context.after(() => rm(root, { recursive: true, force: true }));
  await writeFile(join(root, 'models.json'), JSON.stringify({ providers: { fixture: {
    api: 'openai-completions', baseUrl: 'https://api.longcat.chat/openai/v1',
    models: [{ id: 'fixture', name: 'fixture', reasoning: true, input: ['text'], contextWindow: 128000, maxTokens: 1024 }],
  } } }));
  const directory = join(root, 'sessions');
  const chat = await createYuanpuChatSession({
    agentDir: root, modelConfigDir: root, cwd: root, provider: 'fixture', model: 'fixture', apiKey: 'SECRET-KEY',
    piSession: { id: 'c0f07863-e105-43cf-bd9a-c674bd68a570', directory },
    capabilityClient: { async search() { return { matches: [] }; }, async execute() { throw new Error('unused'); } },
  });
  context.after(() => chat.dispose());
  for (const thinkingLevel of ['medium', 'off']) {
    const reply = await chat.prompt('PRIVATE-PROMPT', { runId: `run-${thinkingLevel}`,
      modelSelection: { provider: 'fixture', model: 'fixture', thinkingLevel } });
    assert.equal(reply.message, 'fixture reply');
  }
  assert.equal(bodies.length, 2);
  for (const body of bodies) {
    assert.equal(body.max_tokens, 1024);
    assert.equal(body.max_completion_tokens, undefined);
    assert.equal(body.reasoning_effort, undefined);
  }
  assert.deepEqual(bodies.map((body) => body.thinking), [{ type: 'enabled' }, { type: 'disabled' }]);
  const entries = (await readFile(join(directory, (await readdir(directory)).find((name) => name.endsWith('.jsonl'))), 'utf8'))
    .trim().split('\n').map(JSON.parse).filter((entry) => entry.customType === 'yuanpu_model_timing').map((entry) => entry.data);
  for (const runId of ['run-medium', 'run-off']) {
    const timings = entries.filter((entry) => entry.runId === runId);
    assert.deepEqual(timings.map((entry) => entry.phase), ['request', 'headers', 'first_event', 'first_text', 'end']);
    assert.equal(new Set(timings.map((entry) => entry.requestId)).size, 1);
    assert.equal(timings[1].status, 200);
    assert.equal(timings.at(-1).stopReason, 'stop');
    assert.ok(timings.every((entry, index) => entry.elapsedMs >= (timings[index - 1]?.elapsedMs ?? 0)));
  }
  assert.doesNotMatch(JSON.stringify(entries), /PRIVATE-PROMPT|SECRET-KEY|SECRET-HEADER|fixture reply/);
});

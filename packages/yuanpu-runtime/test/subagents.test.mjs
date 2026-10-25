import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { YuanpuSubagentManager } from '../dist/index.mjs';

async function setup(t, runChild) {
  const directory = await mkdtemp(join(tmpdir(), 'yuanpu-subagents-'));
  const manager = new YuanpuSubagentManager({ directory, tools: () => ['read', 'write'], prepareChild: () => runChild });
  t.after(async () => { await manager.dispose(); await rm(directory, { recursive: true, force: true }); });
  return manager;
}
test('parallel runs respect global concurrency, tool ceiling, and persist results', async (t) => {
  let active = 0, peak = 0;
  const manager = await setup(t, async (input) => {
    assert.deepEqual(input.tools, ['read']);
    peak = Math.max(peak, ++active);
    await new Promise((resolve) => setTimeout(resolve, 10));
    active--;
    return { text: input.task };
  });
  const tasks = Array.from({ length: 5 }, (_, i) => ({ agent: 'scout', task: String(i) }));
  const runs = await Promise.all([manager.start({ tasks }), manager.start({ tasks })]);
  assert.equal(peak, 3);
  assert.ok(runs.every((run) => run.status === 'completed'));
  const saved = JSON.parse(await readFile(join(runs[0].directory, 'result.json'), 'utf8'));
  assert.deepEqual(saved.children.map((c) => c.text), ['0', '1', '2', '3', '4']);
});
test('running child exposes its Pi session before the child finishes', async (t) => {
  let finish;
  const manager = await setup(t, (input) => new Promise((resolve) => {
    input.onSessionStarted('child-session', '/workspace');
    input.onProgress('Inspecting files');
    finish = () => resolve({ text: 'done', sessionId: 'child-session', cwd: '/workspace' });
  }));
  const run = await manager.start({ agent: 'scout', task: 'inspect', async: true });
  let child;
  for (let attempt = 0; attempt < 50; attempt++) {
    child = manager.status(run.id).children[0];
    if (child.sessionId) break;
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  assert.equal(child?.sessionId, 'child-session');
  assert.equal(child?.cwd, '/workspace');
  assert.equal(child?.progress, 'Inspecting files');
  assert.equal(child?.status, 'running');
  finish();
  for (let attempt = 0; attempt < 50 && manager.status(run.id).status === 'running'; attempt++) {
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  assert.equal(manager.status(run.id).status, 'completed');
});
test('chain forwards output and stops at approval boundary', async (t) => {
  let count = 0;
  const manager = await setup(t, async ({ task }) => {
    count++;
    if (count === 1) return { text: 'evidence' };
    assert.equal(task, 'review evidence');
    return { text: '', pendingApprovalRequestId: 'approval-1' };
  });
  const run = await manager.start({ chain: [
    { agent: 'scout', task: 'inspect' }, { agent: 'reviewer', task: 'review {previous}' }, { agent: 'worker', task: 'edit' },
  ] });
  assert.equal(run.status, 'needs_approval');
  assert.equal(count, 2);
  assert.equal(run.children[2].status, 'cancelled');
});
test('background cancellation and parent disposal stop children; foreign IDs rejected', async (t) => {
  const manager = await setup(t, ({ signal }) => new Promise((resolve, reject) => {
    signal.addEventListener('abort', () => reject(signal.reason), { once: true });
    if (signal.aborted) reject(signal.reason);
  }));
  const run = await manager.start({ agent: 'worker', task: 'wait', async: true });
  assert.throws(() => manager.cancel('../foreign'));
  manager.cancel(run.id);
  await manager.abortAll();
  assert.equal(manager.status(run.id).status, 'cancelled');
  await manager.dispose();
  await assert.rejects(manager.start({ agent: 'worker', task: 'no' }), /closed/);
});
test('timeout, validation and child failures are visible', async (t) => {
  const manager = await setup(t, ({ signal }) => new Promise((resolve, reject) => {
    const timer = setTimeout(resolve, 200);
    signal.addEventListener('abort', () => { clearTimeout(timer); reject(signal.reason); }, { once: true });
  }));
  await assert.rejects(manager.start({ agent: 'unknown', task: 'x' }), /Unknown agent/);
  await assert.rejects(manager.start({ tasks: [] }), /1–8/);
  const run = await manager.start({ agent: 'worker', task: 'wait', timeoutMs: 20 });
  assert.equal(run.status, 'timed_out');
  const failing = await setup(t, async () => { throw new Error('fixture failed'); });
  const failed = await failing.start({ agent: 'worker', task: 'fail' });
  assert.equal(failed.status, 'failed');
  assert.match(failed.children[0].error, /fixture failed/);
});

for (const approval of [false, true]) test(`real SDK delegation preserves model, tools and approval boundary (approval=${approval})`, async (t) => {
  const { createServer } = await import('node:http');
  const { writeFile } = await import('node:fs/promises');
  const { createYuanpuChatSession } = await import('../dist/index.mjs');
  const directory = await mkdtemp(join(tmpdir(), 'yuanpu-subagent-sdk-'));
  const requests = [];
  const server = createServer(async (req, res) => {
    let body = '';
    for await (const chunk of req) body += chunk;
    const input = JSON.parse(body);
    requests.push(input);
    const child = input.messages.some((m) => m.role === 'system' && (m.content.includes('You are scout.') || m.content.includes('You are researcher.')));
    const returned = input.messages.some((m) => m.role === 'tool');
    res.writeHead(200, { 'content-type': 'text/event-stream' });
    const send = (delta, finish_reason = null) => res.write(`data: ${JSON.stringify({ id: 'fixture', object: 'chat.completion.chunk', created: 1, model: 'fixture', choices: [{ index: 0, delta, finish_reason }] })}\n\n`);
    if (child && approval && !returned) {
      send({ role: 'assistant', tool_calls: [{ index: 0, id: 'external-1', type: 'function', function: { name: 'execute_capability', arguments: JSON.stringify({ name: 'fixture:protected' }) } }] });
      send({}, 'tool_calls');
    } else if (!child && !returned) {
      send({ role: 'assistant', tool_calls: [{ index: 0, id: 'delegate-1', type: 'function', function: { name: 'subagent', arguments: JSON.stringify({ action: 'run', agent: approval ? 'researcher' : 'scout', task: 'Return child evidence.' }) } }] });
      send({}, 'tool_calls');
    } else {
      send({ role: 'assistant', content: child ? 'Child evidence.' : 'Delegation complete.' });
      send({}, 'stop');
    }
    res.end('data: [DONE]\n\n');
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(async () => {
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
    await rm(directory, { recursive: true, force: true });
  });
  await writeFile(join(directory, 'models.json'), JSON.stringify({ providers: { fixture: {
    baseUrl: `http://127.0.0.1:${server.address().port}/v1`, api: 'openai-completions',
    models: [{ id: 'fixture', name: 'fixture', reasoning: false, input: ['text'], contextWindow: 128000, maxTokens: 4096 }],
  } } }));
  const chat = await createYuanpuChatSession({ agentDir: directory, modelConfigDir: directory, cwd: directory,
    provider: 'fixture', model: 'fixture', apiKey: 'test-only',
    capabilityClient: { async search() { return { matches: [] }; }, async execute(_input, context) {
      assert.equal(context.runId, 'parent-run');
      throw Object.assign(new Error('Approval required'), { failure: { error: 'needs_approval', message: 'Approval required', approvalRequestId: 'approval-test' } });
    } },
  });
  try {
    const result = await chat.prompt('Delegate this test to an agent.', { runId: 'parent-run' });
    assert.equal(result.message, approval ? '等待授权后执行当前操作。' : 'Delegation complete.');
    assert.equal(requests.length, approval ? 2 : 3);
    assert.ok(requests[0].tools.some((tool) => tool.function.name === 'subagent'));
    assert.equal(requests[1].tools.some((tool) => tool.function.name === 'subagent'), false);
    assert.match(JSON.stringify(requests[1].messages), /主会话用户.*Delegate this test to an agent/);
    if (!approval) assert.deepEqual(requests[1].tools.map((tool) => tool.function.name).sort(), ['find', 'grep', 'ls', 'read']);
    if (approval) {
      assert.equal(result.pendingApprovalRequestId, 'approval-test');
    } else {
      const returned = requests[2].messages.find((m) => m.role === 'tool');
      assert.match(returned.content, /Child evidence/);
      assert.match(returned.content, /completed/);
    }
  } finally { await chat.abort(); chat.dispose(); }
});

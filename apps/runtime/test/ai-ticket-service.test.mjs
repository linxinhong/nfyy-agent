import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { SessionManager } from '@earendil-works/pi-coding-agent';
const { AiTicketService } = await import('../src/ai-ticket-service.ts');

async function settle(service, parentId, id) {
  for (let attempt = 0; attempt < 100; attempt++) {
    const ticket = await service.get(parentId, id);
    if (ticket.status !== 'queued' && ticket.status !== 'running') return ticket;
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  throw new Error('Ticket did not settle.');
}

test('AI ticket starts a separate persistent session, receives context once, and accepts later turns', async () => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-ticket-test-'));
  const parentId = `work:${randomUUID()}`;
  const calls = [];
  const service = new AiTicketService({
    root: join(root, 'tickets'), sessionsPath: join(root, 'sessions'), userId: 'local-user',
    chat: () => ({ agentDir: root, modelConfigDir: root, cwd: root, provider: 'fixture', model: 'fixture' }),
    capabilityClient: () => ({}),
    resolveParent: (id) => id === parentId ? { cwd: root, piSessionId: randomUUID() } : undefined,
    createSession: async (options) => ({
      prompt: async (message) => { calls.push({ sessionId: options.piSession.id, message }); return {}; },
      dispose: async () => new Promise((resolve) => setTimeout(resolve, 30)),
    }),
  });
  try {
    const ticket = await service.create(parentId, '制作计算器', '主会话需求和约束', {
      title: '计算器 UI', deliverables: ['calculator.html'], acceptanceCriteria: ['支持键盘输入'],
    });
    assert.notEqual(ticket.id, parentId.slice(5));
    assert.equal(ticket.title, '计算器 UI');
    assert.deepEqual(ticket.deliverables, ['calculator.html']);
    assert.deepEqual(ticket.acceptanceCriteria, ['支持键盘输入']);
    assert.equal((await settle(service, parentId, ticket.id)).status, 'completed');
    assert.match(calls[0].message, /主会话需求和约束/);
    await service.message(parentId, ticket.id, '增加键盘输入');
    assert.equal((await settle(service, parentId, ticket.id)).status, 'completed');
    assert.equal(calls[1].sessionId, calls[0].sessionId);
    assert.equal(calls[1].message, '增加键盘输入');
    await service.navigate(parentId);
    assert.equal(service.navigation(parentId).ticketId, undefined);
    await service.navigate(parentId, ticket.id);
    assert.equal(service.navigation(parentId).ticketId, ticket.id);
    await assert.rejects(() => service.get(`work:${randomUUID()}`, ticket.id), /Unknown parent/);
    const reopened = new AiTicketService({
      root: join(root, 'tickets'), sessionsPath: join(root, 'sessions'), userId: 'local-user',
      chat: () => ({ agentDir: root, modelConfigDir: root, cwd: root, provider: 'fixture', model: 'fixture' }),
      capabilityClient: () => ({}),
      resolveParent: (id) => id === parentId ? { cwd: root, piSessionId: randomUUID() } : undefined,
      createSession: async () => { throw new Error('not used'); },
    });
    assert.equal((await reopened.list(parentId))[0].id, ticket.id);
    const persisted = await reopened.get(parentId, ticket.id);
    assert.equal(persisted.status, 'completed');
    assert.deepEqual(persisted.acceptanceCriteria, ['支持键盘输入']);
  } finally { await service.dispose(); await rm(root, { recursive: true, force: true }); }
});

test('stopping an active ticket turn preserves the ticket for later messages', async () => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-ticket-cancel-'));
  const parentId = `work:${randomUUID()}`;
  const service = new AiTicketService({
    root: join(root, 'tickets'), sessionsPath: join(root, 'sessions'), userId: 'local-user',
    chat: () => ({ agentDir: root, modelConfigDir: root, cwd: root, provider: 'fixture', model: 'fixture' }),
    capabilityClient: () => ({}),
    resolveParent: (id) => id === parentId ? { cwd: root, piSessionId: randomUUID() } : undefined,
    createSession: async () => ({
      prompt: async (_message, options) => new Promise((_resolve, reject) => {
        if (options.signal.aborted) { reject(options.signal.reason); return; }
        options.signal.addEventListener('abort', () => reject(options.signal.reason), { once: true });
      }),
      dispose: async () => {},
    }),
  });
  try {
    const ticket = await service.create(parentId, '待取消的任务');
    for (let i = 0; i < 100 && (await service.get(parentId, ticket.id)).status !== 'running'; i++) {
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
    await service.cancel(parentId, ticket.id);
    assert.equal((await service.get(parentId, ticket.id)).status, 'cancelled');
  } finally { await service.dispose(); await rm(root, { recursive: true, force: true }); }
});

test('ticket composer choices reach its own run and unrestricted access ends with that run', async () => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-ticket-options-'));
  const parentId = `work:${randomUUID()}`;
  const prompts = [];
  let finishSecond;
  const service = new AiTicketService({
    root: join(root, 'tickets'), sessionsPath: join(root, 'sessions'), userId: 'local-user',
    chat: () => ({ agentDir: root, modelConfigDir: root, cwd: root, provider: 'fixture', model: 'fixture' }),
    capabilityClient: () => ({}),
    resolveParent: (id) => id === parentId ? { cwd: root, piSessionId: randomUUID() } : undefined,
    createSession: async (options) => ({
      prompt: async (message, runOptions) => {
        prompts.push({ sessionId: options.piSession.id, message, runOptions });
        if (prompts.length === 2) await new Promise((resolve) => { finishSecond = resolve; });
        return {};
      },
      getContextUsage: () => ({ tokens: 1_024, contextWindow: 8_192, percent: 12.5 }),
      dispose: async () => {},
    }),
  });
  try {
    const ticket = await service.create(parentId, '制作计算器');
    assert.equal((await settle(service, parentId, ticket.id)).status, 'completed');
    assert.equal(service.isUnrestrictedCapabilityRun(prompts[0].runOptions.runId, prompts[0].sessionId, root), false);

    await service.message(parentId, ticket.id, '按照新模型继续', {
      modelSelection: { provider: 'fixture', model: 'advanced', thinkingLevel: 'high' },
      approvalMode: 'unrestricted',
    });
    for (let i = 0; i < 100 && prompts.length < 2; i++) {
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
    assert.equal(prompts.length, 2);
    const { sessionId, runOptions } = prompts[1];
    assert.deepEqual(runOptions.modelSelection, { provider: 'fixture', model: 'advanced', thinkingLevel: 'high' });
    assert.equal(service.isUnrestrictedCapabilityRun(runOptions.runId, sessionId, root), true);
    assert.equal(service.isUnrestrictedCapabilityRun(randomUUID(), sessionId, root), false);
    assert.equal(service.isUnrestrictedCapabilityRun(runOptions.runId, randomUUID(), root), false);
    assert.equal(service.isUnrestrictedCapabilityRun(runOptions.runId, sessionId, join(root, 'other')), false);
    finishSecond();
    const settled = await settle(service, parentId, ticket.id);
    assert.equal(settled.status, 'completed');
    assert.equal(settled.model, 'fixture/advanced');
    assert.deepEqual(settled.modelSelection, { provider: 'fixture', model: 'advanced', thinkingLevel: 'high' });
    assert.deepEqual(settled.context, { tokens: 1_024, contextWindow: 8_192, percent: 12.5 });
    assert.equal(service.isUnrestrictedCapabilityRun(runOptions.runId, sessionId, root), false);
  } finally {
    finishSecond?.();
    await service.dispose();
    await rm(root, { recursive: true, force: true });
  }
});

test('ticket conversation groups tool activity like the main Agent and reports completion once after retry', async () => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-ticket-feedback-'));
  const parentId = `work:${randomUUID()}`;
  const sessionsPath = join(root, 'sessions');
  const ticketsPath = join(root, 'tickets');
  const completions = [];
  let rejectFirst = true;
  const common = {
    root: ticketsPath, sessionsPath, userId: 'local-user',
    chat: () => ({ agentDir: root, modelConfigDir: root, cwd: root, provider: 'fixture', model: 'fixture' }),
    capabilityClient: () => ({}),
    resolveParent: (id) => id === parentId ? { cwd: root, piSessionId: randomUUID() } : undefined,
    onCompleted: async (completion) => {
      completions.push(completion);
      if (rejectFirst) { rejectFirst = false; throw new Error('temporary queue failure'); }
    },
  };
  const service = new AiTicketService({ ...common,
    createSession: async (options) => {
      const session = SessionManager.create(root, sessionsPath, { id: options.piSession.id });
      const usage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0,
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
      const assistant = (content, stopReason) => session.appendMessage({ role: 'assistant', content,
        api: 'fixture', provider: 'fixture', model: 'fixture', stopReason, timestamp: Date.now(), usage });
      return {
        prompt: async (message, runOptions) => {
          session.appendMessage({ role: 'user', content: message, timestamp: Date.now() });
          assistant([{ type: 'text', text: '先检查文件。' },
            { type: 'toolCall', id: 'tool-1', name: 'bash', arguments: { command: 'pwd' } }], 'toolUse');
          runOptions.onProgress({ type: 'text', delta: '先检查文件。' });
          runOptions.onProgress({ type: 'tool', id: 'tool-1', name: 'bash', status: 'running',
            step: { id: 'tool-1', name: 'bash', status: 'running', startedAt: new Date().toISOString() } });
          session.appendMessage({ role: 'toolResult', toolCallId: 'tool-1', toolName: 'bash',
            content: [{ type: 'text', text: root }], isError: false, timestamp: Date.now() });
          assistant([{ type: 'text', text: '计算器已完成。' }], 'stop');
          return { message: '计算器已完成。', tools: [{ name: 'bash', status: 'completed' }] };
        },
        dispose: async () => {},
      };
    },
  });
  try {
    const created = await service.create(parentId, '制作计算器');
    const settled = await settle(service, parentId, created.id);
    assert.equal(settled.status, 'completed');
    assert.equal(settled.messages.length, 2);
    assert.equal(settled.messages[1].role, 'assistant');
    assert.match(settled.messages[1].text, /先检查文件。.*计算器已完成。/s);
    assert.equal(settled.messages[1].run.steps[0].name, 'bash');
    assert.equal(settled.messages[1].run.steps[0].status, 'completed');
    assert.equal(completions.length, 1);
    await service.dispose();
    const restarted = new AiTicketService({ ...common });
    await restarted.reconcilePendingFeedback();
    await restarted.reconcilePendingFeedback();
    assert.equal(completions.length, 2);
    assert.equal(completions[0].turnId, completions[1].turnId);
    assert.equal(completions[1].parentConversationId, parentId);
    assert.equal(completions[1].status, 'completed');
    assert.equal(completions[1].result, '计算器已完成。');
    assert.equal((await restarted.get(parentId, created.id)).pendingFeedback, undefined);
  } finally { await service.dispose(); await rm(root, { recursive: true, force: true }); }
});

test('failed and interrupted tickets report to the parent without waiting for a user reminder', async () => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-ticket-terminal-feedback-'));
  const parentId = `work:${randomUUID()}`;
  const ticketsPath = join(root, 'tickets');
  const completions = [];
  const service = new AiTicketService({
    root: ticketsPath, sessionsPath: join(root, 'sessions'), userId: 'local-user',
    chat: () => ({ agentDir: root, modelConfigDir: root, cwd: root, provider: 'fixture', model: 'fixture' }),
    capabilityClient: () => ({}),
    resolveParent: (id) => id === parentId ? { cwd: root, piSessionId: randomUUID() } : undefined,
    createSession: async () => ({ prompt: async () => { throw new Error('fetch failed'); }, dispose: async () => {} }),
    onCompleted: async (completion) => { completions.push(completion); },
  });
  try {
    const failed = await service.create(parentId, '编写文档');
    assert.equal((await settle(service, parentId, failed.id)).status, 'failed');
    assert.equal(completions[0].status, 'failed');
    assert.match(completions[0].result, /fetch failed/);

    const interruptedId = randomUUID();
    const now = new Date().toISOString();
    await mkdir(ticketsPath, { recursive: true });
    await writeFile(join(ticketsPath, `${interruptedId}.json`), JSON.stringify({
      id: interruptedId, parentConversationId: parentId, cwd: root, sessionId: randomUUID(),
      task: '制作计算器', status: 'running', createdAt: now, updatedAt: now,
    }));
    const restarted = new AiTicketService({
      root: ticketsPath, sessionsPath: join(root, 'sessions'), userId: 'local-user',
      chat: () => ({ agentDir: root, modelConfigDir: root, cwd: root, provider: 'fixture', model: 'fixture' }),
      capabilityClient: () => ({}),
      resolveParent: (id) => id === parentId ? { cwd: root, piSessionId: randomUUID() } : undefined,
      onCompleted: async (completion) => { completions.push(completion); },
    });
    await restarted.reconcilePendingFeedback();
    await restarted.reconcilePendingFeedback();
    assert.equal((await restarted.get(parentId, interruptedId)).status, 'interrupted');
    assert.equal(completions.length, 2);
    assert.equal(completions[1].status, 'interrupted');
  } finally { await service.dispose(); await rm(root, { recursive: true, force: true }); }
});

test('active ticket exposes one live run with collapsible tool steps instead of raw tool messages', async () => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-ticket-live-'));
  const parentId = `work:${randomUUID()}`;
  let finish;
  const service = new AiTicketService({
    root: join(root, 'tickets'), sessionsPath: join(root, 'sessions'), userId: 'local-user',
    chat: () => ({ agentDir: root, modelConfigDir: root, cwd: root, provider: 'fixture', model: 'fixture' }),
    capabilityClient: () => ({}),
    resolveParent: (id) => id === parentId ? { cwd: root, piSessionId: randomUUID() } : undefined,
    createSession: async (options) => ({
      prompt: async (message, runOptions) => {
        const session = SessionManager.create(root, join(root, 'sessions'), { id: options.piSession.id });
        session.appendMessage({ role: 'user', content: message, timestamp: Date.now() });
        runOptions.onProgress({ type: 'text', delta: '正在检查。' });
        runOptions.onProgress({ type: 'tool', id: 'tool-1', name: 'bash', status: 'running',
          step: { id: 'tool-1', name: 'bash', status: 'running', startedAt: new Date().toISOString(),
            arguments: '{"command":"pwd"}' } });
        return await new Promise((resolve) => { finish = () => resolve({ message: '完成。', tools: [] }); });
      },
      dispose: async () => {},
    }),
  });
  try {
    const ticket = await service.create(parentId, '检查文件');
    for (let attempt = 0; !finish && attempt < 100; attempt++) {
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
    assert.ok(finish);
    const active = await service.get(parentId, ticket.id);
    assert.equal(active.status, 'running');
    assert.equal(active.messages.length, 1);
    assert.equal(active.messages[0].role, 'user');
    assert.equal(active.progress, '正在检查。');
    assert.equal(active.liveRun.status, 'running');
    assert.equal(active.liveRun.steps[0].name, 'bash');
    finish();
    assert.equal((await settle(service, parentId, ticket.id)).status, 'completed');
  } finally { finish?.(); await service.dispose(); await rm(root, { recursive: true, force: true }); }
});

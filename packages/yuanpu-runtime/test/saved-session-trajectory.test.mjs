import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { SessionManager } from '@earendil-works/pi-coding-agent';
import { readYuanpuSavedContextUsage, readYuanpuSessionTrajectory } from '../dist/index.mjs';

test('saved trajectory refreshes after the Pi session grows', async (context) => {
  const cwd = await mkdtemp(join(tmpdir(), 'yuanpu-trajectory-'));
  context.after(() => rm(cwd, { recursive: true, force: true }));
  const directory = join(cwd, 'sessions');
  const sessionId = randomUUID();
  const session = SessionManager.create(cwd, directory, { id: sessionId });
  const answer = (text) => session.appendMessage({ role: 'assistant', content: [{ type: 'text', text }],
    api: 'fixture', provider: 'fixture', model: 'fixture', stopReason: 'stop', timestamp: Date.now(),
    usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } } });
  session.appendMessage({ role: 'user', content: 'first', timestamp: Date.now() });
  answer('one');
  const read = () => readYuanpuSessionTrajectory(cwd, sessionId, directory, 'work:test', cwd);
  assert.equal(read().rounds, 1);
  assert.equal(read().rounds, 1);
  session.appendMessage({ role: 'user', content: 'second', timestamp: Date.now() });
  answer('two');
  assert.equal(read().rounds, 2);
});

test('saved Work session restores its context ring from Pi usage and the configured model window', async (context) => {
  const cwd = await mkdtemp(join(tmpdir(), 'yuanpu-context-'));
  context.after(() => rm(cwd, { recursive: true, force: true }));
  await writeFile(join(cwd, 'models.json'), JSON.stringify({ providers: { fixture: {
    baseUrl: 'https://example.test/v1', api: 'openai-completions',
    models: [{ id: 'fixture', name: 'fixture', reasoning: false, input: ['text'],
      contextWindow: 1000, maxTokens: 100 }],
  } } }));
  const directory = join(cwd, 'sessions');
  const sessionId = randomUUID();
  const session = SessionManager.create(cwd, directory, { id: sessionId });
  session.appendMessage({ role: 'user', content: 'first', timestamp: Date.now() });
  session.appendMessage({ role: 'assistant', content: [{ type: 'text', text: 'done' }],
    api: 'openai-completions', provider: 'fixture', model: 'fixture', stopReason: 'stop', timestamp: Date.now(),
    usage: { input: 220, output: 30, cacheRead: 0, cacheWrite: 0, totalTokens: 250,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } } });
  const usage = await readYuanpuSavedContextUsage(cwd, sessionId, directory, cwd, cwd,
    { provider: 'fixture', model: 'fixture' });
  assert.equal(usage?.tokens, 250);
  assert.equal(usage?.contextWindow, 1000);
  assert.equal(usage?.percent, 25);
});

test('completed async AI tickets replace their queued tool snapshot after restart', async (context) => {
  const cwd = await mkdtemp(join(tmpdir(), 'yuanpu-async-ticket-'));
  context.after(() => rm(cwd, { recursive: true, force: true }));
  const sessionId = randomUUID();
  const runId = randomUUID();
  const directory = join(cwd, 'sessions');
  const session = SessionManager.create(cwd, directory, { id: sessionId });
  const queued = { id: runId, mode: 'single', status: 'queued', createdAt: new Date().toISOString(),
    children: [{ agent: 'scout', task: 'Inspect the contract', status: 'queued', text: '' }] };
  session.appendMessage({ role: 'user', content: 'Inspect the contract', timestamp: Date.now() });
  session.appendMessage({ role: 'assistant', content: [{ type: 'toolCall', id: 'delegate-1',
    name: 'subagent', arguments: { action: 'run', agent: 'scout', task: 'Inspect the contract', async: true } }],
  api: 'fixture', provider: 'fixture', model: 'fixture', stopReason: 'toolUse', timestamp: Date.now(),
  usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0,
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } } });
  session.appendMessage({ role: 'toolResult', toolCallId: 'delegate-1', toolName: 'subagent',
    content: [{ type: 'text', text: JSON.stringify(queued) }], isError: false, timestamp: Date.now() });
  const runDirectory = join(cwd, 'subagent-runs', runId);
  await mkdir(runDirectory, { recursive: true });
  await writeFile(join(runDirectory, 'result.json'), JSON.stringify({ ...queued, status: 'completed',
    children: [{ ...queued.children[0], status: 'completed', text: 'Evidence found.' }] }));
  const ticket = readYuanpuSessionTrajectory(cwd, sessionId, directory, 'work:test', cwd).children[0];
  assert.equal(ticket.status, 'completed');
  assert.equal(ticket.task, 'Inspect the contract');
  assert.equal(ticket.result, 'Evidence found.');
});

import assert from 'node:assert/strict';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { DatabaseSync } from 'node:sqlite';

import { YuanpuMetadataDatabase } from '@yuanpu-agent/runtime-kit';
import { AssistantHostService } from '../src/assistant-host.ts';

const now = '2026-09-26T00:00:00.000Z';
const tick = () => new Promise((resolve) => setTimeout(resolve, 5));

function setup() {
  const raw = new DatabaseSync(':memory:');
  raw.exec('PRAGMA foreign_keys = ON');
  const metadata = new YuanpuMetadataDatabase(raw);
  metadata.channels.bindConnection({ provider: 'wecom', connectionId: 'bot-1',
    providerAccountDigest: 'a'.repeat(64), credentialBindingDigest: 'b'.repeat(64), now });
  metadata.channels.pair('wecom', 'bot-1', 'sender-1', now);
  metadata.channels.observePrivateSender({ provider: 'wecom', connectionId: 'bot-1',
    senderDigest: 'sender-1', recipientId: 'member-1', now });
  const records = new Map();
  let available = true;
  let prompts = 0;
  const worker = {
    task: async (id) => { if (!available) throw new Error('offline'); return records.get(id); },
    taskRecords: async (ids) => ids.map((id) => records.get(id)).filter(Boolean),
    prompt: async (id, text, _deadline, sessionId) => {
      if (!available) throw new Error('offline');
      prompts++;
      const record = { id, status: 'completed', sessionId, message: `answer:${text}`,
        updatedAt: new Date().toISOString() };
      records.set(id, record);
      return record;
    },
    cancel() {},
  };
  const service = new AssistantHostService(metadata.assistantHost, worker, '/work');
  return { metadata, service, worker, records, setAvailable(value) { available = value; },
    get prompts() { return prompts; } };
}

function message(overrides = {}) {
  return { provider: 'wecom', connectionId: 'bot-1', providerBotId: 'bot',
    providerMessageId: 'msg-1', providerRequestId: 'req-1', senderId: 'member-1',
    conversationType: 'single', conversationId: 'member-1', messageType: 'text', text: 'hello',
    ...overrides };
}

test('two desktop clients attach one durable request; offline worker recovers without shared Work run', async () => {
  const env = setup();
  try {
    env.setAvailable(false);
    const receipt = env.service.submitDesktop('hello');
    assert.match(receipt.runId, /^asst_/);
    await tick();
    assert.equal((await env.service.getDesktopRun(receipt.runId)).status, 'running');
    await tick();
    env.setAvailable(true);
    env.service.recover();
    await tick();
    const firstClient = await env.service.getDesktopRun(receipt.runId);
    const secondClient = await env.service.getDesktopRun(receipt.runId);
    assert.equal(firstClient.status, 'succeeded');
    assert.equal(secondClient.output.message, 'answer:hello');
    assert.equal(env.prompts, 1);
    env.service.close();
    const reattached = new AssistantHostService(env.metadata.assistantHost, env.worker, '/work');
    assert.equal((await reattached.getDesktopRun(receipt.runId)).status, 'succeeded');
    assert.equal(reattached.transcript().length, 2);
    reattached.close();
  } finally { env.service.close(); env.metadata.close(); }
});

test('Assistant history restores persisted tool evidence using only authorized transcript task IDs', async () => {
  const env = setup();
  try {
    const receipt = env.service.submitDesktop('inspect work');
    await tick();
    await env.service.getDesktopRun(receipt.runId);
    const record = env.records.get(receipt.runId);
    record.progress = { text: 'answer:inspect work', steps: [{ id: 'candidate-call',
      name: 'assistant_work_candidates', status: 'completed', startedAt: now, result: '[]' }] };
    env.records.set('unrelated', { id: 'unrelated', progress: { text: 'PRIVATE', steps: [] } });
    const requested = [];
    env.worker.taskRecords = async (ids) => { requested.push(...ids); return ids.map((id) => env.records.get(id)); };
    const history = await env.service.transcriptWithProgress();
    assert.deepEqual(requested, [receipt.runId]);
    assert.equal(history[1].run.steps[0].id, 'candidate-call');
    assert.equal(history[1].run.status, 'succeeded');
    assert.doesNotMatch(JSON.stringify(history), /PRIVATE/);
    assert.equal((await env.service.getDesktopRun(receipt.runId)).progress.steps[0].result, '[]');
  } finally { env.service.close(); env.metadata.close(); }
});

test('proactive WeCom send requires opt-in and a paired private target; accepted is not read', async (t) => {
  const root = await mkdtemp(join(tmpdir(), 'yp-proactive-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const config = join(root, 'config.json');
  await writeFile(config, '{"schemaVersion":1}\n');
  const env = setup();
  const service = new AssistantHostService(env.metadata.assistantHost, env.worker, '/work', config);
  t.after(() => { service.close(); env.service.close(); env.metadata.close(); });
  let sends = 0;
  const transport = { isReady: () => true, sendProactive: async (recipient, content) => {
    sends++;
    assert.equal(recipient, 'member-1');
    assert.equal(content, 'Check the report.');
    return { status: 'accepted' };
  } };
  service.recoverWecom('bot-1', transport);
  const id = `suggestion-${'b'.repeat(24)}`;
  assert.deepEqual(await service.deliverSuggestion(id, 'Check the report.'), { status: 'deferred' });
  await writeFile(config, '{"schemaVersion":1,"proactiveWecomEnabled":true}\n');
  assert.deepEqual(await service.deliverSuggestion(id, 'Check the report.'), { status: 'deferred' },
    'an unbound contact cannot receive proactive suggestions');
  env.metadata.assistantHost.linkWecomContact(env.metadata.channels.listPrivateContacts('wecom')[0].contactId);
  assert.deepEqual(await service.deliverSuggestion(id, 'Check the report.'), {
    status: 'accepted', ref: `assistant-proactive:${id}` });
  assert.equal(sends, 1);
  assert.deepEqual(await service.deliverSuggestion(id, 'Check the report.'), {
    status: 'accepted', ref: `assistant-proactive:${id}` });
  assert.equal(sends, 1);
});

test('desktop retry key survives lost response and refuses conflicting text', async () => {
  const env = setup();
  try {
    const first = env.service.submitDesktop('same text', 'client-message-1');
    const repeated = env.service.submitDesktop('same text', 'client-message-1');
    assert.equal(repeated.runId, first.runId);
    assert.equal(repeated.duplicate, true);
    assert.throws(() => env.service.submitDesktop('different text', 'client-message-1'), /Conflicting/);
    await tick();
    assert.equal(env.prompts, 1);
  } finally { env.service.close(); env.metadata.close(); }
});

test('offline accepted desktop request can be cancelled durably before Worker recovery', async () => {
  const env = setup();
  try {
    env.setAvailable(false);
    const accepted = env.service.submitDesktop('do not run', 'cancel-offline');
    assert.equal(env.service.cancelDesktop(accepted.runId).result, 'cancellation_requested');
    await tick();
    env.setAvailable(true);
    env.service.recover();
    await tick();
    assert.equal((await env.service.getDesktopRun(accepted.runId)).status, 'cancelled');
    assert.equal(env.prompts, 0);
  } finally { env.service.close(); env.metadata.close(); }
});

test('in-flight desktop cancellation reaches Worker and terminal state remains queryable', async () => {
  const env = setup();
  try {
    let resolvePrompt;
    env.worker.prompt = async (id, _text, _deadline, sessionId) => new Promise((resolve) => {
      resolvePrompt = (status) => {
        const record = { id, sessionId, status, updatedAt: new Date().toISOString() };
        env.records.set(id, record);
        resolve(record);
      };
    });
    env.worker.cancel = () => resolvePrompt?.('cancelled');
    const receipt = env.service.submitDesktop('stop this', 'cancel-active');
    await tick();
    assert.equal(env.service.cancelDesktop(receipt.runId).result, 'cancellation_requested');
    await tick();
    assert.equal((await env.service.getDesktopRun(receipt.runId)).status, 'cancelled');
    assert.equal(env.metadata.assistantHost.get(receipt.runId).cancelRequested, true);
  } finally { env.service.close(); env.metadata.close(); }
});

test('paired WeCom reply stays on original req_id; duplicate and unknown delivery never resend', async () => {
  const env = setup();
  try {
    const contact = env.metadata.channels.listPrivateContacts('wecom')[0];
    const binding = await env.service.linkContact(contact.contactId);
    assert.notEqual(binding.sessionId, env.metadata.assistantHost.desktop().sessionId);
    let ready = false;
    const sent = [];
    const transport = { isReady: () => ready,
      reply: async (route, id, content) => { sent.push({ route, id, content });
        return { status: 'accepted' }; } };
    assert.equal((await env.service.handleWecom(message({ senderId: 'stranger', conversationId: 'stranger' }), transport)).accepted, false);
    assert.equal((await env.service.handleWecom(message({ conversationType: 'group', conversationId: 'room' }), transport)).accepted, false);
    const first = await env.service.handleWecom(message(), transport);
    assert.equal(first.accepted, true);
    await tick();
    assert.equal(sent.length, 0);
    ready = true;
    env.service.recoverWecom('bot-1', transport);
    await tick();
    assert.equal(sent.length, 1);
    assert.equal(sent[0].route.providerRequestId, 'req-1');
    assert.equal(sent[0].content, 'answer:hello');
    const duplicate = await env.service.handleWecom(message({ providerRequestId: 'req-redelivery' }), transport);
    assert.equal(duplicate.duplicate, true);
    await tick();
    assert.equal(env.prompts, 1);
    assert.equal(sent.length, 1);
    const next = await env.service.handleWecom(message({ providerMessageId: 'msg-2', providerRequestId: 'req-2' }),
      { isReady: () => true, reply: async () => { throw new Error('ack timeout'); } });
    await tick();
    assert.equal(env.metadata.assistantHost.delivery(next.runId).status, 'unknown');
    env.service.recoverWecom('bot-1', transport);
    await tick();
    assert.equal(sent.length, 1);
  } finally { env.service.close(); env.metadata.close(); }
});

test('revocation during an accepted slow turn retains result but blocks WeCom delivery', async () => {
  const env = setup();
  try {
    const contact = env.metadata.channels.listPrivateContacts('wecom')[0];
    const firstBinding = await env.service.linkContact(contact.contactId);
    let release;
    const pending = new Promise((resolve) => { release = resolve; });
    env.worker.prompt = async (id, text, _deadline, sessionId) => {
      await pending;
      const record = { id, status: 'completed', sessionId, message: `answer:${text}`,
        updatedAt: new Date().toISOString() };
      env.records.set(id, record);
      return record;
    };
    let sends = 0;
    const transport = { isReady: () => true, reply: async () => { sends++; return { status: 'accepted' }; } };
    const receipt = await env.service.handleWecom(message(), transport);
    await tick();
    await env.service.unlinkContact();
    const secondBinding = await env.service.linkContact(contact.contactId);
    assert.equal(secondBinding.generation, firstBinding.generation + 1);
    release();
    await tick();
    assert.equal(env.metadata.assistantHost.get(receipt.runId).status, 'completed');
    assert.equal(env.metadata.assistantHost.delivery(receipt.runId).status, 'failed');
    assert.equal(sends, 0);
    assert.equal(env.metadata.assistantHost.wecomLink()?.contactId, contact.contactId);
  } finally { env.service.close(); env.metadata.close(); }
});

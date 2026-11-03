import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { appendFile, mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { register } from 'tsx/esm/api';
import { SessionManager } from '@earendil-works/pi-coding-agent';

register();
const { openYuanpuMetadataDatabase } = await import('../src/persistence/index.ts');
const { searchWorkConversations } = await import('../src/persistence/work-search.ts');
const { readSavedWorkMessageWindow } = await import('../src/pi/work-message-search.ts');
const { summarizeTranscript } = await import('../src/pi/transcript-summary.ts');

const usage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0,
  cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };

test('searches scoped Work metadata and only visible saved text with stable Pi entry IDs', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-work-search-'));
  const sessions = join(root, 'sessions');
  await mkdir(sessions);
  const db = openYuanpuMetadataDatabase(join(root, 'metadata.sqlite'));
  context.after(() => db.close());
  context.after(() => rm(root, { recursive: true, force: true }));
  const scope = join(root, 'workspace');
  const otherScope = join(root, 'other');
  const parentId = `folder:${randomUUID()}`;
  const childId = `folder:${randomUUID()}`;
  db.workConversations.createFolder(scope, parentId, null, 'Clients', 'folder', `f-${parentId.slice(7)}`);
  db.workConversations.createFolder(scope, childId, parentId, 'Acme', 'folder',
    `f-${parentId.slice(7)}/f-${childId.slice(7)}`);
  const tag = db.workConversations.createTag(scope, 'urgent');
  const conversation = db.workConversations.create(scope, join(root, 'chat'), childId);
  db.workConversations.updateConversation(scope, conversation.id, { title: 'Q4 launch', tagIds: [tag.id] });
  const foreign = db.workConversations.create(otherScope, join(root, 'foreign'));
  db.workConversations.updateConversation(otherScope, foreign.id, { title: 'foreign needle' });
  const session = SessionManager.create(conversation.workingDirectory, sessions,
    { id: db.workConversations.sessionId(scope, conversation.id) });
  const userId = session.appendMessage({ role: 'user', content: 'find needle in the message', timestamp: Date.now() });
  session.appendMessage({ role: 'assistant', content: [
    { type: 'thinking', thinking: 'private-needle-thinking' },
    { type: 'toolCall', id: 'call-1', name: 'read', arguments: { path: 'private-needle-argument' } },
  ], api: 'fixture', provider: 'fixture', model: 'fixture', stopReason: 'toolUse', usage, timestamp: Date.now() });
  session.appendMessage({ role: 'toolResult', toolCallId: 'call-1', toolName: 'read',
    content: [{ type: 'text', text: 'private-needle-result' }], isError: false, timestamp: Date.now() });
  const assistantId = session.appendMessage({ role: 'assistant', content: [{ type: 'text', text: 'needle response' }],
    api: 'fixture', provider: 'fixture', model: 'fixture', stopReason: 'stop', usage, timestamp: Date.now() });

  const search = (query, extra = {}) => searchWorkConversations(db.workConversations,
    { workspaceId: scope, sessionsDirectory: sessions, query, ...extra });
  const messages = search('needle');
  assert.deepEqual(messages.items.map((item) => item.messageEntryId), [userId, assistantId]);
  assert.deepEqual(messages.items.map((item) => item.role), ['user', 'assistant']);
  assert.deepEqual(messages.items[0].folderPath.map((item) => item.name), ['Clients', 'Acme']);
  assert.equal(messages.items[0].conversationId, conversation.id);
  assert.doesNotMatch(JSON.stringify(messages), /private-needle|foreign needle/);
  assert.deepEqual(search('private-needle-thinking').items, []);
  assert.deepEqual(search('private-needle-argument').items, []);
  assert.deepEqual(search('private-needle-result').items, []);
  assert.deepEqual(messages.contentFailures, []);
  assert.equal(search('Clients').items[0].matchedField, 'folder');
  assert.equal(search('launch').items[0].matchedField, 'title');
  assert.equal(search('urgent').items[0].matchedField, 'tag');
  const firstPage = search('needle', { limit: 1 });
  assert.equal(firstPage.items.length, 1);
  assert.ok(firstPage.nextCursor);
  assert.equal(search('needle', { limit: 1, cursor: firstPage.nextCursor }).items[0].messageEntryId, assistantId);
  assert.throws(() => search('launch', { cursor: firstPage.nextCursor }), /Invalid search cursor/);
  db.workConversations.updateConversation(scope, conversation.id, { title: 'Renamed Q4 launch' });
  assert.throws(() => search('needle', { limit: 1, cursor: firstPage.nextCursor }), /Search results changed/);
  assert.throws(() => search(' '), /Search query/);
  assert.throws(() => search('x'.repeat(201)), /Search query/);
  assert.throws(() => search('needle', { limit: 51 }), /Search page size/);

  db.workConversations.updateConversation(scope, conversation.id, { archived: true });
  assert.equal(search('needle').items.length, 0);
  assert.equal(search('needle', { archive: 'archived' }).items.length, 2);
  db.workConversations.updateFolder(scope, parentId, { name: 'Renamed clients' });
  assert.equal(search('needle', { archive: 'all' }).items[0].folderPath[0].name, 'Renamed clients');

  const file = session.getSessionFile();
  await appendFile(file, '{bad json}\n');
  const damaged = search('launch', { archive: 'all' });
  assert.equal(damaged.items[0].matchedField, 'title');
  assert.deepEqual(damaged.contentFailures, [{ conversationId: conversation.id, reason: 'corrupt' }]);
  await writeFile(file, 'not json\n');
  assert.deepEqual(search('launch', { archive: 'all' }).contentFailures,
    [{ conversationId: conversation.id, reason: 'corrupt' }]);
  await rm(file);
  assert.deepEqual(search('launch', { archive: 'all' }).contentFailures,
    [{ conversationId: conversation.id, reason: 'missing' }]);
  assert.deepEqual(search('launch', { archive: 'all', sessionsDirectory: join(root, 'missing-sessions') }).contentFailures,
    [{ conversationId: conversation.id, reason: 'missing' }]);
});

test('an early search hit remains addressable beyond the default 100-message transcript', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-work-search-window-'));
  const sessions = join(root, 'sessions');
  await mkdir(sessions);
  const db = openYuanpuMetadataDatabase(join(root, 'metadata.sqlite'));
  context.after(() => db.close());
  context.after(() => rm(root, { recursive: true, force: true }));
  const scope = join(root, 'workspace');
  const conversation = db.workConversations.create(scope, join(root, 'chat'));
  const sessionId = db.workConversations.sessionId(scope, conversation.id);
  const session = SessionManager.create(conversation.workingDirectory, sessions, { id: sessionId });
  const earlyId = session.appendMessage({ role: 'user', content: 'early needle', timestamp: Date.now() });
  for (let index = 0; index < 110; index++) {
    session.appendMessage({ role: 'user', content: `later ${index}`, timestamp: Date.now() });
  }
  session.appendMessage({ role: 'assistant', content: [{ type: 'text', text: 'saved' }],
    api: 'fixture', provider: 'fixture', model: 'fixture', stopReason: 'stop', usage, timestamp: Date.now() });
  assert.equal(summarizeTranscript(session.getBranch()).some((item) => item.id === earlyId), false);
  const found = searchWorkConversations(db.workConversations,
    { workspaceId: scope, sessionsDirectory: sessions, query: 'early needle' });
  assert.deepEqual(found.contentFailures, []);
  assert.equal(found.items[0].messageEntryId, earlyId);
  const window = readSavedWorkMessageWindow(conversation.workingDirectory, sessionId, sessions, earlyId, 3);
  assert.equal(window.status, 'ok');
  assert.equal(window.messages[window.targetIndex].entryId, earlyId);
  assert.equal(window.hasAfter, true);
  assert.equal(readSavedWorkMessageWindow(conversation.workingDirectory, sessionId, sessions, 'unknown').status, 'not_found');
});

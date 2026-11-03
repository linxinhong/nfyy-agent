import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { copyFile, mkdir, mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import test from 'node:test';
import { SessionManager } from '@earendil-works/pi-coding-agent';

import { openYuanpuMetadataDatabase, readYuanpuChatTranscript } from '../dist/index.mjs';

test('Work transcript pages load newest first and older pages without repeating the cursor', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-work-transcript-pages-'));
  context.after(() => rm(root, { recursive: true, force: true }));
  const workspace = join(root, 'workspace');
  const sessions = join(root, 'sessions');
  await mkdir(workspace);
  const session = SessionManager.create(workspace, sessions, { id: 'paged-session' });
  for (let index = 0; index < 7; index++) {
    session.appendMessage({ role: 'user', content: `message ${index}`, timestamp: Date.now() + index });
  }
  session.appendMessage({ role: 'assistant', content: [{ type: 'text', text: 'done' }],
    api: 'fixture', provider: 'fixture', model: 'fixture', stopReason: 'stop',
    usage: { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } }, timestamp: Date.now() });
  const recent = readYuanpuChatTranscript(workspace, 'paged-session', sessions, 3);
  assert.deepEqual(recent.map((item) => item.text), ['message 5', 'message 6', 'done']);
  const older = readYuanpuChatTranscript(workspace, 'paged-session', sessions, 3, false, recent[0].id);
  assert.deepEqual(older.map((item) => item.text), ['message 2', 'message 3', 'message 4']);
  assert.deepEqual(readYuanpuChatTranscript(workspace, 'paged-session', sessions, 3, false, older[0].id)
    .map((item) => item.text), ['message 0', 'message 1']);
  assert.deepEqual(readYuanpuChatTranscript(workspace, 'paged-session', sessions, 3, false, 'missing'), []);
});

test('unrestricted capability grant stays bound to one running desktop Work session', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-work-permission-'));
  const database = openYuanpuMetadataDatabase(join(root, 'automation.sqlite'));
  context.after(() => database.close());
  context.after(() => rm(root, { recursive: true, force: true }));
  const runs = database.agentRuns;
  const now = new Date().toISOString();
  const result = runs.submit({
    request: {
      contractVersion: 1, entryPoint: 'desktop',
      identity: { kind: 'local_user', subjectId: 'local-user', authorityId: 'local-desktop', authenticatedBy: 'electron' },
      workspaceId: '/workspace', conversation: { namespace: 'desktop', conversationId: 'work:one' },
      input: { type: 'text', text: 'work' }, approvalMode: 'unrestricted',
      idempotencyKey: 'permission-test', delivery: { kind: 'desktop' },
    }, requestFingerprint: 'fingerprint', inputDigest: 'digest', runId: 'run-one',
    bindingId: 'binding-one', piSessionId: 'session-one', now, maximumQueuedRuns: 10,
  });
  assert.equal(result.kind, 'created');
  assert.equal(runs.isUnrestrictedCapabilityRun('run-one', 'session-one', '/workspace'), false);
  runs.claimQueued('run-one', now);
  assert.equal(runs.isUnrestrictedCapabilityRun('run-one', 'session-one', '/workspace'), true);
  assert.equal(runs.isUnrestrictedCapabilityRun('run-one', 'session-two', '/workspace'), false);
  assert.equal(runs.isUnrestrictedCapabilityRun('run-one', 'session-one', '/other'), false);
  assert.equal(runs.isUnrestrictedCapabilityRun('other-run', 'session-one', '/workspace'), false);
});

test('new Work sessions remain isolated and selected across restart while default is read only', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-work-conversations-'));
  context.after(() => rm(root, { recursive: true, force: true }));
  const path = join(root, 'automation.sqlite');
  const workspace = join(root, 'workspace');
  const sessions = join(root, 'sessions');
  await mkdir(workspace);
  const legacySession = SessionManager.create(workspace, sessions, { id: 'legacy-pi-session' });
  legacySession.appendMessage({ role: 'user', content: 'old Work question', timestamp: Date.now() });
  legacySession.appendMessage({ role: 'assistant', content: [{ type: 'text', text: 'old Work answer' }],
    api: 'anthropic-messages', provider: 'anthropic', model: 'fixture', stopReason: 'stop',
    usage: { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } }, timestamp: Date.now() });
  openYuanpuMetadataDatabase(path).close();
  const legacy = new DatabaseSync(path);
  legacy.prepare(`INSERT INTO yp_conversation_bindings
    (binding_id, entry_point, authority_id, subject_id, namespace, conversation_id,
      thread_id, pi_session_id, workspace_id, created_at, updated_at)
    VALUES ('legacy-binding', 'desktop', 'local-desktop', 'local-user', 'desktop', 'default',
      '', 'legacy-pi-session', ?, '2026-09-25T00:00:00Z', '2026-09-25T00:00:00Z')`).run(workspace);
  legacy.close();
  const database = openYuanpuMetadataDatabase(path);
  const first = database.workConversations.current(workspace);
  const second = database.workConversations.create(workspace);
  assert.notEqual(first.id, second.id);
  assert.notEqual(database.workConversations.sessionId(workspace, first.id),
    database.workConversations.sessionId(workspace, second.id));
  assert.equal(first.workingDirectory, workspace);
  assert.equal(database.workConversations.current(workspace).id, second.id);
  const isolated = database.workConversations.create(workspace, join(root, 'isolated'));
  assert.equal(isolated.workingDirectory, join(root, 'isolated'));
  assert.equal(database.workConversations.hasWorkingDirectory(workspace, isolated.workingDirectory), true);
  const isolatedBindingDb = new DatabaseSync(path);
  const isolatedBinding = isolatedBindingDb.prepare('SELECT workspace_id FROM yp_conversation_bindings WHERE conversation_id = ?')
    .get(isolated.id);
  assert.equal(isolatedBinding.workspace_id, isolated.workingDirectory);
  isolatedBindingDb.close();
  assert.equal(database.workConversations.sessionId(workspace, 'default'), 'legacy-pi-session');
  assert.equal(database.workConversations.list(workspace).find((item) => item.id === 'default').archived, true);
  assert.throws(() => database.workConversations.select(workspace, 'default'), /Unknown Work conversation/);
  assert.throws(() => database.workConversations.select('/another/workspace', first.id), /Unknown Work conversation/);
  database.workConversations.select(workspace, first.id);
  database.close();

  const reopened = openYuanpuMetadataDatabase(path);
  assert.equal(reopened.workConversations.current(workspace).id, first.id);
  assert.deepEqual(reopened.workConversations.list(workspace).map((item) => item.id).sort(),
    [first.id, second.id, isolated.id, 'default'].sort());
  assert.deepEqual(readYuanpuChatTranscript(workspace, 'legacy-pi-session', sessions).map((item) => item.text),
    ['old Work question', 'old Work answer']);
  reopened.close();
});

test('nested folders, metadata, tags and archive persist without changing stable paths', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-work-tree-'));
  context.after(() => rm(root, { recursive: true, force: true }));
  const path = join(root, 'automation.sqlite');
  const scope = join(root, 'workspace');
  let database = openYuanpuMetadataDatabase(path);
  const store = database.workConversations;
  const firstId = 'folder:11111111-1111-4111-8111-111111111111';
  const secondId = 'folder:22222222-2222-4222-8222-222222222222';
  const firstPath = 'f-11111111-1111-4111-8111-111111111111';
  const secondPath = `${firstPath}/f-22222222-2222-4222-8222-222222222222`;
  const first = store.createFolder(scope, firstId, null, '研发', 'folder', firstPath);
  const second = store.createFolder(scope, secondId, firstId, '模型', 'code', secondPath);
  assert.equal(second.parentId, first.id);
  assert.throws(() => store.createFolder('/other', 'folder:33333333-3333-4333-8333-333333333333',
    firstId, 'wrong scope', 'folder', 'f-33333333-3333-4333-8333-333333333333'), /Unknown Work parent/);
  assert.throws(() => store.updateFolder(scope, secondId, { iconId: 'shell-command' }), /Invalid icon/);
  const renamed = store.updateFolder(scope, firstId, { name: '新研发' });
  assert.equal(renamed.relativeDirectory, firstPath);
  const one = store.create(scope, join(scope, secondPath, 'c-one'), secondId);
  const two = store.create(scope, join(scope, secondPath, 'c-two'), secondId);
  assert.equal(store.titleIfEmpty(scope, two.id, '浏览器侧栏测试'), true);
  assert.equal(store.titleIfEmpty(scope, two.id, '不应覆盖'), false);
  assert.equal(store.listExisting(scope).find((item) => item.id === two.id).title, '浏览器侧栏测试');
  assert.equal(store.updateConversation(scope, two.id, { iconId: 'wrench' }).iconId, 'wrench');
  assert.equal(store.updateConversation(scope, two.id, { iconId: 'trophy' }).iconId, 'trophy');
  assert.notEqual(store.sessionId(scope, one.id), store.sessionId(scope, two.id));
  const requestId = '44444444-4444-4444-8444-444444444444';
  const requested = store.create(scope, join(scope, secondPath, 'c-three'), secondId,
    'work:33333333-3333-4333-8333-333333333333', requestId);
  assert.equal(store.conversationForRequest(scope, requestId).id, requested.id);
  assert.throws(() => store.create(scope, join(scope, secondPath, 'c-four'), secondId,
    'work:55555555-5555-4555-8555-555555555555', requestId));
  const intentId = 'work:66666666-6666-4666-8666-666666666666';
  store.beginCreateIntent(scope, intentId, 'c-66666666-6666-4666-8666-666666666666', 'conversation');
  assert.deepEqual(store.pendingCreateIntents(scope), [{
    id: intentId, relativeDirectory: 'c-66666666-6666-4666-8666-666666666666', committed: false,
  }]);
  store.finishCreateIntent(intentId);
  assert.deepEqual(store.pendingCreateIntents(scope), []);
  store.reorder(scope, 'conversation', secondId, [two.id, one.id, requested.id]);
  const tag = store.createTag(scope, '排查', 'blue');
  store.updateConversation(scope, one.id, { title: '接口联调', iconId: 'star', tagIds: [tag.id] });
  assert.throws(() => store.updateConversation(scope, one.id, { tagIds: ['tag:foreign'] }), /Unknown Work tag/);
  assert.deepEqual(store.listExisting(scope).find((item) => item.id === one.id).tagIds, [tag.id]);
  const archived = store.updateConversation(scope, one.id, { archived: true });
  assert.equal(archived.archived, true);
  assert.throws(() => store.select(scope, one.id), /Unknown Work conversation/);
  assert.equal(store.updateConversation(scope, one.id, { archived: false }).archived, false);
  assert.throws(() => store.updateConversation(scope, 'default', { archived: false }), /Unknown Work conversation/);
  database.close();
  database = openYuanpuMetadataDatabase(path);
  assert.equal(database.workConversations.listFolders(scope).find((item) => item.id === firstId).name, '新研发');
  assert.equal(database.workConversations.listExisting(scope).find((item) => item.id === one.id).title, '接口联调');
  assert.equal(database.workConversations.listExisting(scope).find((item) => item.id === one.id).workingDirectory,
    join(scope, secondPath, 'c-one'));
  assert.deepEqual(database.workConversations.listExisting(scope).filter((item) => item.folderId === secondId)
    .map((item) => item.id), [two.id, one.id, requested.id]);
  assert.equal(database.workConversations.listTags(scope)[0].id, tag.id);
  database.close();
});

test('Work folders stop at five levels even when created through the store', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-work-depth-'));
  const database = openYuanpuMetadataDatabase(join(root, 'automation.sqlite'));
  context.after(() => database.close());
  context.after(() => rm(root, { recursive: true, force: true }));
  const scope = join(root, 'workspace');
  let parentId = null;
  let path = '';
  for (let level = 1; level <= 5; level++) {
    const uuid = `${String(level).repeat(8)}-${String(level).repeat(4)}-4${String(level).repeat(3)}-8${String(level).repeat(3)}-${String(level).repeat(12)}`;
    const id = `folder:${uuid}`;
    path = [path, `f-${uuid}`].filter(Boolean).join('/');
    database.workConversations.createFolder(scope, id, parentId, `第 ${level} 级`, 'folder', path);
    parentId = id;
  }
  const sixthId = 'folder:66666666-6666-4666-8666-666666666666';
  assert.throws(() => database.workConversations.createFolder(scope, sixthId, parentId, '第 6 级', 'folder',
    `${path}/f-66666666-6666-4666-8666-666666666666`), /五级/);
});

test('only saved complete turns produce stable source events on repeated scans', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-work-source-'));
  context.after(() => rm(root, { recursive: true, force: true }));
  const path = join(root, 'automation.sqlite');
  const database = openYuanpuMetadataDatabase(path);
  const conversation = database.workConversations.current('/workspace');
  const fixture = new DatabaseSync(path);
  const binding = fixture.prepare('SELECT binding_id FROM yp_conversation_bindings WHERE conversation_id = ?')
    .get(conversation.id).binding_id;
  const addRun = (id, input, status, answer) => {
    fixture.prepare(`INSERT INTO yp_agent_runs
      (run_id, entry_point, authority_id, subject_id, idempotency_key, request_fingerprint,
       input_digest, request_metadata_json, binding_id, status, created_at, updated_at)
      VALUES (?, 'desktop', 'local-desktop', 'local-user', ?, ?, ?, '{}', ?, ?, ?, ?)`)
      .run(id, id, 'a'.repeat(64), createHash('sha256').update(input).digest('hex'), binding,
        status, '2026-09-26T01:00:00Z', '2026-09-26T01:01:00Z');
    fixture.prepare(`INSERT INTO yp_agent_run_outputs(run_id, output_json, created_at) VALUES (?, ?, ?)`)
      .run(id, JSON.stringify({ message: answer, tools: [] }), '2026-09-26T01:01:00Z');
  };
  addRun('run-one', 'first', 'running', 'answer');
  addRun('run-two', 'unfinished', 'failed', 'not saved');
  const messages = [
    { id: 'user-1', role: 'user', text: 'first', at: '2026-09-26T01:00:00Z' },
    { id: 'assistant-1', role: 'assistant', text: 'answer', at: '2026-09-26T01:01:00Z' },
    { id: 'user-2', role: 'user', text: 'unfinished', at: '2026-09-26T01:02:00Z' },
    { id: 'assistant-2', role: 'assistant', text: 'not saved', at: '2026-09-26T01:03:00Z' },
  ];
  assert.equal(database.workConversations.recordSavedTurns(conversation.id, messages), 0);
  fixture.prepare("UPDATE yp_agent_runs SET status = 'succeeded' WHERE run_id = 'run-one'").run();
  assert.equal(database.workConversations.recordSavedTurns(conversation.id, messages), 1);
  assert.equal(database.workConversations.recordSavedTurns(conversation.id, messages), 0);
  const sources = database.workConversations.sources(conversation.id);
  assert.equal(sources.length, 1);
  assert.equal(sources[0].turnId, 'assistant-1');
  assert.equal(sources[0].runId, 'run-one');
  assert.equal(sources[0].userText, 'first');
  assert.equal(sources[0].assistantText, 'answer');
  const sourceChanges = database.workConversations.sourceChanges(conversation.id);
  assert.deepEqual(JSON.parse(JSON.stringify(sourceChanges)), sourceChanges);
  assert.deepEqual(sourceChanges[0].audience, { kind: 'personal', id: 'local-user' });
  assert.equal(sourceChanges[0].sourceId, sources[0].sourceId);
  const page = database.workConversations.sourcePage(0, 1);
  assert.equal(page.length, 1);
  assert.deepEqual(page[0].change, sourceChanges[0]);
  assert.deepEqual(database.workConversations.sourcePage(page[0].eventId, 1), []);
  fixture.exec('VACUUM');
  assert.deepEqual(database.workConversations.sourcePage(0, 1), page,
    'durable source cursor must survive SQLite row reorganization');
  assert.equal(database.workConversations.resolveContentRef(sourceChanges[0].contentRef).runId, 'run-one');
  fixture.close();
  database.close();

  const movedRoot = join(root, 'moved-home');
  await mkdir(movedRoot);
  const movedPath = join(movedRoot, 'automation.sqlite');
  await copyFile(path, movedPath);
  const reopened = openYuanpuMetadataDatabase(movedPath);
  assert.equal(reopened.workConversations.recordSavedTurns(conversation.id, messages), 0);
  assert.deepEqual(reopened.workConversations.sources(conversation.id), sources);
  assert.deepEqual(reopened.workConversations.sourceChanges(conversation.id), sourceChanges);
  assert.deepEqual(reopened.workConversations.sourcePage(0, 1), page);
  reopened.database.prepare('DELETE FROM yp_work_turn_sources WHERE content_ref = ?')
    .run(sourceChanges[0].contentRef);
  reopened.database.prepare(`INSERT INTO yp_work_turn_sources(conversation_id,turn_id,run_id,
    content_ref,source_version,committed_at,user_text,assistant_text)
    VALUES (?,?,?,?,?,?,?,?)`).run(conversation.id, 'assistant-new', 'run-new', 'work-content:new',
      'new-hash', '2026-09-27T00:00:00Z', 'new user', 'new answer');
  const afterDelete = reopened.workConversations.sourcePage(page[0].eventId, 1);
  assert.equal(afterDelete.length, 1);
  assert.ok(afterDelete[0].eventId > page[0].eventId,
    'deleting the maximum event must not recycle an acknowledged cursor');
  reopened.close();
});

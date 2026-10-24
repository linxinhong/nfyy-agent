import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { DatabaseSync } from 'node:sqlite';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { AssistantAutomationEngine, AssistantAutomationStore, AssistantSourceStore,
  AssistantWorkReviewStore } from '@yuanpu-agent/assistant';

const require = createRequire(import.meta.url);
const { assistantAutomationHandler } = require('../dist/index.cjs');
const audience = { kind: 'personal', id: 'local-user' };

test('saved Work source invokes bounded review and commits the record once', async (t) => {
  const root = await mkdtemp(join(tmpdir(), 'yp-review-wake-'));
  const home = join(root, 'assistant');
  const sources = await AssistantSourceStore.open(home);
  t.after(async () => { sources.close(); await rm(root, { recursive: true, force: true }); });
  const change = { sourceId: 'work-turn:work:one:turn-1', sourceVersion: 'version-1',
    kind: 'created', audience, occurredAt: new Date().toISOString(),
    contentRef: 'opaque:turn-1', workId: 'work:one' };
  sources.enqueuePage('work', '0', { nextCursor: '1', events: [{ eventId: '1', change }] });
  await sources.processNext({ currentSource: async () => ({ status: 'available',
    sourceVersion: change.sourceVersion }), readSource: async () => ({ status: 'available',
    sourceVersion: change.sourceVersion, text: 'User: Write a report.\nAssistant: Done.' }) });
  const store = new AssistantAutomationStore(sources.database);
  const reviews = new AssistantWorkReviewStore(sources.database, sources, home);
  assert.equal(store.reconcileProcessedSources(), 1);
  let modelCalls = 0;
  const handler = assistantAutomationHandler({ sources: { database: sources.database } }, store,
    undefined, { store: reviews, async review(snapshot, _signal, beforeModel) {
      assert.equal(snapshot.workId, 'work:one');
      assert.equal(beforeModel(), true);
      modelCalls++;
      return { costUsd: 0.01, message: JSON.stringify({ goal: 'Write a report',
        constraints: [], judgment: 'supported', findings: [{ claim: 'Report completed',
          judgment: 'supported', evidenceRefs: [change.sourceId] }], unresolved: [], followUp: [],
        memoryCandidates: [], ledgerCandidates: [] }) };
    } });
  const engine = new AssistantAutomationEngine(store, handler);
  assert.equal((await engine.tick()).status, 'completed');
  assert.equal(modelCalls, 1);
  const row = sources.database.prepare('SELECT review_id FROM work_reviews').get();
  assert.equal(reviews.get(row.review_id).judgment, 'unverified');
  assert.match(await readFile(join(home, 'reviews', 'one', `${row.review_id}.md`), 'utf8'),
    /Judgment: unverified/);
  assert.equal(store.reconcileProcessedSources(), 0);
  await engine.tick();
  assert.equal(modelCalls, 1);
});

test('a lost review model result becomes a visible unverified review without rebilling', async (t) => {
  const root = await mkdtemp(join(tmpdir(), 'yp-review-lost-'));
  const home = join(root, 'assistant');
  const sources = await AssistantSourceStore.open(home);
  t.after(async () => { sources.close(); await rm(root, { recursive: true, force: true }); });
  const change = { sourceId: 'work-turn:work:lost:turn-1', sourceVersion: 'v1',
    kind: 'created', audience, occurredAt: new Date().toISOString(),
    contentRef: 'opaque:lost', workId: 'work:lost' };
  sources.enqueuePage('work', '0', { nextCursor: '1', events: [{ eventId: '1', change }] });
  await sources.processNext({ currentSource: async () => ({ status: 'available',
    sourceVersion: 'v1' }), readSource: async () => ({ status: 'available',
    sourceVersion: 'v1', text: 'User: Make a report.\nAssistant: Done.' }) });
  const store = new AssistantAutomationStore(sources.database);
  const reviews = new AssistantWorkReviewStore(sources.database, sources, home);
  store.reconcileProcessedSources();
  const job = store.byKey('source:review-work:work-turn:work:lost:turn-1:v1');
  let calls = 0;
  const handler = assistantAutomationHandler({ sources: { database: sources.database } }, store,
    undefined, { store: reviews, async review(_snapshot, _signal, beforeModel) {
      assert.equal(beforeModel(), true);
      calls++;
      throw new Error('model response lost');
    } });
  const engine = new AssistantAutomationEngine(store, handler);
  assert.equal((await engine.tick()).status, 'waiting');
  sources.database.prepare('UPDATE automation_jobs SET retry_at=? WHERE job_id=?')
    .run(new Date(0).toISOString(), job.jobId);
  assert.equal((await engine.tick()).status, 'completed');
  assert.equal(calls, 1);
  const row = sources.database.prepare('SELECT review_id FROM work_reviews WHERE job_id=?')
    .get(job.jobId);
  assert.equal(reviews.get(row.review_id).judgment, 'unverified');
  assert.match(await readFile(join(home, 'reviews', 'lost', `${row.review_id}.md`), 'utf8'),
    /model result was interrupted or lost/);
});

function fixture() {
  const database = new DatabaseSync(':memory:');
  const store = new AssistantAutomationStore(database);
  const record = { taskId: 'delegated_one', assistantSessionId: 'assistant_one',
    status: 'completed', updatedAt: new Date().toISOString(),
    completionCriteria: ['Cite evidence'], result: { status: 'completed',
      resultRef: 'opaque:result', evidenceRefs: ['source:one'] } };
  const job = store.enqueueDelegation(record.taskId, 'v1', audience, record.updatedAt, record.status);
  const memory = { sources: { database } };
  return { database, store, record, job, memory };
}

test('daily reflection stays silent without fresh evidence and considers a candidate once', async (t) => {
  const database = new DatabaseSync(':memory:');
  t.after(() => database.close());
  const store = new AssistantAutomationStore(database);
  store.scheduleActivePeriods();
  let candidates = [];
  let calls = 0;
  const considered = [];
  const recorded = [];
  const suggestions = { store: {
    async candidates() { return candidates; },
    record(items, proposal) { considered.push(...items); recorded.push(...proposal); candidates = []; },
    recordReflectionAttempt() {}, reflectionAttempt() { return undefined; },
  }, async reflect(items, _signal, beforeModel) {
    assert.equal(beforeModel(), true);
    calls++;
    return { costUsd: 0.01, message: JSON.stringify({ suggestions: [{ candidateId: items[0].candidateId,
      reason: 'The report is unverified.', nextStep: 'Check the report totals.' }] }) };
  } };
  const engine = new AssistantAutomationEngine(store,
    assistantAutomationHandler({ sources: { database } }, store, undefined, undefined, undefined, suggestions));
  assert.equal((await engine.tick()).status, 'completed');
  assert.equal(calls, 0);
  candidates = [{ candidateId: 'follow-up-one', fingerprint: 'v1', context: 'report',
    text: 'The report is unverified.', evidence: [{ sourceId: 'work-one', sourceVersion: 'v1',
      observedAt: new Date().toISOString() }] }];
  assert.equal((await engine.tick()).status, 'completed');
  assert.equal(calls, 1);
  assert.equal(considered.length, 1);
  assert.equal(recorded[0].nextStep, 'Check the report totals.');
  await engine.tick();
  assert.equal(calls, 1);
});

test('a remembered follow-up is considered on resume after the daily check ran', async (t) => {
  const database = new DatabaseSync(':memory:');
  t.after(() => database.close());
  const store = new AssistantAutomationStore(database);
  const original = { candidateId: 'follow-up-one', fingerprint: 'v1',
    context: 'Report', text: 'The report needs verification.', evidence: [] };
  let candidates = [original];
  let calls = 0;
  const considered = [];
  const host = { store: {
    async candidates() { return candidates; },
    record(items) { considered.push(...items); candidates = []; },
    recordReflectionAttempt() {}, reflectionAttempt() { return undefined; },
  }, async reflect(items, _signal, beforeModel) {
    assert.equal(beforeModel(), true);
    calls++;
    return { costUsd: 0.01, message: JSON.stringify({ suggestions: [{
      candidateId: items[0].candidateId, reason: 'Verification is still open.',
      nextStep: 'Check the report totals.' }] }) };
  } };
  const engine = new AssistantAutomationEngine(store,
    assistantAutomationHandler({ sources: { database } }, store,
      undefined, undefined, undefined, host));
  const daily = store.scheduleActivePeriods()[0];
  // Today's periodic check has already finished before this memory candidate appears.
  database.prepare("UPDATE automation_jobs SET status='completed' WHERE kind IN ('daily-check','weekly-check')")
    .run();
  assert.equal(store.get(daily.jobId).status, 'completed');
  const first = store.enqueueFollowUpCheck([{ candidateId: original.candidateId,
    fingerprint: original.fingerprint }]);
  assert.equal((await engine.tick()).status, 'completed');
  assert.equal(calls, 1);
  assert.deepEqual(considered, [original]);
  assert.equal(store.enqueueFollowUpCheck([{ candidateId: original.candidateId,
    fingerprint: original.fingerprint }]).jobId, first.jobId);
  assert.equal(await engine.tick(), undefined);

  candidates = [{ ...original, fingerprint: 'v2', text: 'The report changed.' }];
  const stale = store.enqueueFollowUpCheck([{ candidateId: original.candidateId,
    fingerprint: 'v1' }]);
  assert.equal(stale.jobId, first.jobId);
  const current = store.enqueueFollowUpCheck([{ candidateId: original.candidateId,
    fingerprint: 'v2' }]);
  assert.equal((await engine.tick()).jobId, current.jobId);
  assert.equal(calls, 2);
});

test('a queued follow-up with changed evidence skips its old snapshot', async (t) => {
  const database = new DatabaseSync(':memory:');
  t.after(() => database.close());
  const store = new AssistantAutomationStore(database);
  const old = { candidateId: 'follow-up-one', fingerprint: 'v1', context: '', text: '', evidence: [] };
  const newer = { ...old, fingerprint: 'v2' };
  const job = store.enqueueFollowUpCheck([old]);
  const host = { store: { async candidates() { return [newer]; },
    record() { throw new Error('stale evidence must not be considered'); },
    recordReflectionAttempt() {}, reflectionAttempt() { return undefined; } },
  async reflect() { throw new Error('stale evidence must not call the model'); } };
  const engine = new AssistantAutomationEngine(store,
    assistantAutomationHandler({ sources: { database } }, store,
      undefined, undefined, undefined, host));
  assert.equal((await engine.tick()).status, 'completed');
  assert.equal(store.hasCheckpoint(job.effectId), true);
});

test('a changed candidate after a paid reflection is not silently marked considered', async (t) => {
  const database = new DatabaseSync(':memory:');
  t.after(() => database.close());
  const store = new AssistantAutomationStore(database);
  const daily = store.scheduleActivePeriods()[0];
  const original = { candidateId: 'follow-up-one', fingerprint: 'v1', context: 'one',
    text: 'Original evidence', evidence: [] };
  const changed = { ...original, fingerprint: 'v2', text: 'New evidence' };
  let candidates = [original];
  let calls = 0;
  const recorded = [];
  const host = { store: { async candidates() { return candidates; },
    record(items) { recorded.push(...items); },
    recordReflectionAttempt() {}, reflectionAttempt() { return [original]; } },
  async reflect(_items, _signal, beforeModel) {
    assert.equal(beforeModel(), true);
    calls++;
    candidates = [changed];
    return { costUsd: 0.01, message: '{"suggestions":[]}' };
  } };
  const engine = new AssistantAutomationEngine(store,
    assistantAutomationHandler({ sources: { database } }, store, undefined, undefined, undefined, host));
  assert.equal((await engine.tick()).status, 'waiting');
  database.prepare('UPDATE automation_jobs SET retry_at=? WHERE job_id=?')
    .run(new Date(0).toISOString(), daily.jobId);
  assert.equal((await engine.tick()).status, 'completed');
  assert.equal(calls, 1, 'the same period is never billed twice');
  assert.deepEqual(recorded, [], 'new evidence remains eligible for a later period');
});

test('verification job accounts for cost before linking evidence and commits once', async (t) => {
  const { database, store, record, job, memory } = fixture();
  t.after(() => database.close());
  let notifications = 0;
  let links = 0;
  const host = { async current() { return record; },
    async notify(_record, _signal, beforeModel) { assert.equal(beforeModel(), true); notifications++; return { costUsd: 0.01,
      message: JSON.stringify({ checks: [{ criterion: 'Cite evidence', evidenceRefs: ['source:one'] }] }) }; },
    async linkEvidence() { links++; } };
  const engine = new AssistantAutomationEngine(store, assistantAutomationHandler(memory, store, host));
  assert.equal((await engine.tick()).status, 'completed');
  assert.equal(store.hasCheckpoint(job.effectId), true);
  assert.equal(store.hasEffectAttempt(job.effectId), true);
  assert.equal(notifications, 1);
  assert.equal(links, 1);
  await engine.tick();
  assert.equal(notifications, 1);
});

test('over-budget and interrupted notifications never link evidence or replay', async (t) => {
  const first = fixture();
  t.after(() => first.database.close());
  let overBudgetLinks = 0;
  const costly = { async current() { return first.record; },
    async notify(_record, _signal, beforeModel) { assert.equal(beforeModel(), true);
      return { costUsd: 1, message: '{"checks":[]}' }; },
    async linkEvidence() { overBudgetLinks++; } };
  const expensive = new AssistantAutomationEngine(first.store,
    assistantAutomationHandler(first.memory, first.store, costly));
  assert.equal((await expensive.tick()).status, 'failed');
  assert.equal(overBudgetLinks, 0);
  assert.equal(first.store.hasCheckpoint(first.job.effectId), false);

  const second = fixture();
  t.after(() => second.database.close());
  let notifications = 0;
  const interrupted = { async current() { return second.record; },
    async notify(_record, _signal, beforeModel) { assert.equal(beforeModel(), true);
      notifications++; throw new Error('Worker died after model request'); },
    async linkEvidence() { throw new Error('must not link'); } };
  const before = new AssistantAutomationEngine(second.store,
    assistantAutomationHandler(second.memory, second.store, interrupted));
  assert.equal((await before.tick()).status, 'waiting');
  const after = new AssistantAutomationEngine(second.store,
    assistantAutomationHandler(second.memory, second.store, interrupted));
  second.store.database.prepare("UPDATE automation_jobs SET retry_at=? WHERE job_id=?")
    .run(new Date(0).toISOString(), second.job.jobId);
  assert.equal((await after.tick()).status, 'waiting');
  assert.equal(notifications, 1);
  assert.equal(second.store.hasEffectAttempt(second.job.effectId), true);
});

test('an interrupted model attempt remains unknown after reopening the automation database', async (t) => {
  const root = await mkdtemp(join(tmpdir(), 'yp-delegation-wake-restart-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const path = join(root, 'state.sqlite');
  const record = { taskId: 'restart_task', assistantSessionId: 'assistant_one',
    status: 'completed', updatedAt: new Date().toISOString() };
  let calls = 0;
  const host = { async current() { return record; },
    async notify(_record, _signal, beforeModel) { assert.equal(beforeModel(), true);
      calls++; throw new Error('model response lost'); },
    async linkEvidence() { throw new Error('must not link'); } };
  const firstDb = new DatabaseSync(path);
  const firstStore = new AssistantAutomationStore(firstDb);
  const job = firstStore.enqueueDelegation(record.taskId, 'v1', audience,
    record.updatedAt, record.status);
  const first = new AssistantAutomationEngine(firstStore,
    assistantAutomationHandler({ sources: { database: firstDb } }, firstStore, host));
  assert.equal((await first.tick()).status, 'waiting');
  firstDb.close();
  const reopenedDb = new DatabaseSync(path);
  t.after(() => reopenedDb.close());
  const reopenedStore = new AssistantAutomationStore(reopenedDb);
  reopenedDb.prepare("UPDATE automation_jobs SET retry_at=? WHERE job_id=?")
    .run(new Date(0).toISOString(), job.jobId);
  const restarted = new AssistantAutomationEngine(reopenedStore,
    assistantAutomationHandler({ sources: { database: reopenedDb } }, reopenedStore, host));
  assert.equal((await restarted.tick()).status, 'waiting');
  assert.equal(reopenedStore.hasEffectAttempt(job.effectId), true);
  assert.equal(calls, 1);
});

test('foreground preemption before model dispatch can resume the same notification', async (t) => {
  const { database, store, record, job, memory } = fixture();
  t.after(() => database.close());
  let entered;
  const firstNotify = new Promise((resolveEntered) => { entered = resolveEntered; });
  let calls = 0;
  let links = 0;
  const host = { async current() { return record; },
    async notify(_record, signal, beforeModel) {
      calls++;
      if (calls === 1) {
        entered();
        return new Promise((_resolve, reject) => signal.addEventListener('abort',
          () => reject(new Error('preempted before model dispatch')), { once: true }));
      }
      assert.equal(beforeModel(), true);
      return { costUsd: 0, message: JSON.stringify({ checks: [{ criterion: 'Cite evidence',
        evidenceRefs: ['source:one'] }] }) };
    },
    async linkEvidence() { links++; } };
  const engine = new AssistantAutomationEngine(store, assistantAutomationHandler(memory, store, host));
  const interrupted = engine.tick();
  await firstNotify;
  engine.setForeground(true);
  await interrupted;
  assert.equal(store.get(job.jobId).status, 'waiting');
  assert.equal(store.hasEffectAttempt(job.effectId), false);
  engine.setForeground(false);
  assert.equal((await engine.tick()).status, 'completed');
  assert.equal(calls, 2);
  assert.equal(links, 1);
});

test('a persisted billed proposal resumes local evidence commit without a second model turn', async (t) => {
  const root = await mkdtemp(join(tmpdir(), 'yp-delegation-proposal-restart-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const path = join(root, 'state.sqlite');
  const archive = join(root, 'archive.json');
  const record = { taskId: 'proposal_task', assistantSessionId: 'assistant_one',
    status: 'completed', updatedAt: new Date().toISOString() };
  let notifications = 0;
  let links = 0;
  const host = { async current() { return record; },
    async notify(_record, _signal, beforeModel) {
      assert.equal(beforeModel(), true);
      notifications++;
      return { costUsd: 0.01, message: JSON.stringify({ checks: [{ criterion: 'Cite evidence',
        evidenceRefs: ['source:one'] }] }) };
    },
    async linkEvidence() {
      links++;
      await writeFile(archive, JSON.stringify({ taskId: record.taskId,
        verification: { 'Cite evidence': ['source:one'] } }));
      if (links === 1) throw new Error('Worker died after the idempotent archive write');
    } };
  const firstDb = new DatabaseSync(path);
  const firstStore = new AssistantAutomationStore(firstDb);
  const job = firstStore.enqueueDelegation(record.taskId, 'v1', audience,
    record.updatedAt, record.status);
  const first = new AssistantAutomationEngine(firstStore,
    assistantAutomationHandler({ sources: { database: firstDb } }, firstStore, host));
  assert.equal((await first.tick()).status, 'waiting');
  assert.ok(firstStore.preparedProposal(job));
  firstDb.close();
  const secondDb = new DatabaseSync(path);
  t.after(() => secondDb.close());
  const secondStore = new AssistantAutomationStore(secondDb);
  secondDb.prepare("UPDATE automation_jobs SET retry_at=? WHERE job_id=?")
    .run(new Date(0).toISOString(), job.jobId);
  const second = new AssistantAutomationEngine(secondStore,
    assistantAutomationHandler({ sources: { database: secondDb } }, secondStore, host));
  assert.equal((await second.tick()).status, 'completed');
  assert.equal(notifications, 1);
  assert.equal(links, 2);
  assert.equal(secondStore.hasCheckpoint(job.effectId), true);
  assert.deepEqual(JSON.parse(await readFile(archive, 'utf8')).verification,
    { 'Cite evidence': ['source:one'] });
});

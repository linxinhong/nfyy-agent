import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { AssistantAutomationEngine, AssistantAutomationStore, AssistantSourceStore } from '../dist/index.mjs';

const audience = { kind: 'personal', id: 'local-user' };

async function fixture(t, now = new Date(2026, 8, 27, 12)) {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-automation-'));
  const home = join(root, 'assistant');
  let sources = await AssistantSourceStore.open(home);
  t.after(async () => { sources?.close(); await rm(root, { recursive: true, force: true }); });
  const clock = { now: () => now, set: (date) => { now = date; } };
  return { home, clock, get sources() { return sources; },
    reopen: async () => { sources.close(); sources = await AssistantSourceStore.open(home); } };
}

function source(eventId, sourceVersion, feedId = 'work', kind = 'created') {
  return { feedId, eventId, status: 'processed', change: {
    sourceId: feedId === 'work' ? 'work-turn:one' : 'assistant-turn:one', sourceVersion,
    kind, audience, occurredAt: '2026-09-27T00:00:00.000Z',
    ...(kind === 'deleted' ? {} : { contentRef: `opaque:${sourceVersion}` }),
  } };
}

test('source jobs dedupe versions, supersede stale work, and never ingest review output', async (t) => {
  const { sources, clock } = await fixture(t);
  const store = new AssistantAutomationStore(sources.database, clock.now);
  const first = store.enqueueSource(source('1', 'v1'));
  assert.equal(store.enqueueSource(source('1', 'v1')).jobId, first.jobId);
  assert.equal(first.kind, 'review-work');
  const second = store.enqueueSource(source('2', 'v2'));
  assert.notEqual(second.jobId, first.jobId);
  assert.equal(store.get(first.jobId).status, 'cancelled');
  assert.equal(store.next().jobId, second.jobId);
  store.start(second.jobId);
  store.enqueueSource(source('7', 'v7'));
  assert.equal(store.get(second.jobId).status, 'cancelled',
    'a running stale proposal cannot apply after a newer source version');
  assert.equal(store.enqueueSource(source('3', 'v3', 'assistant')).kind, 'understand-user');
  const deletedSource = { ...source('4', 'v4', 'work-deletions', 'deleted'),
    change: { ...source('4', 'v4', 'work-deletions', 'deleted').change, sourceId: 'work-turn:one' } };
  assert.equal(store.enqueueSource(deletedSource).kind,
    'maintain-memory');
  assert.equal(store.get(store.byKey('source:review-work:work-turn:one:v7').jobId).status, 'cancelled');
  assert.equal(store.enqueueSource({ ...source('5', 'v5'), feedId: 'reviews' }), undefined);
  assert.equal(store.enqueueSource({ ...source('6', 'v6'), status: 'obsolete' }), undefined);
  assert.throws(() => store.enqueue({ kind: 'review-work', dedupeKey: first.dedupeKey,
    sourceId: 'another', sourceVersion: 'v1', audience }), /Conflicting/);
});

test('waiting in the queue does not consume the execution-time budget', async (t) => {
  const { sources, clock } = await fixture(t);
  const store = new AssistantAutomationStore(sources.database, clock.now);
  const job = store.enqueue({ kind: 'review-work', dedupeKey: 'queued-budget', audience,
    durationMs: 2_000, dueAt: new Date(clock.now().getTime() + 60_000) });
  clock.set(new Date(clock.now().getTime() + 70_000));
  const started = store.start(job.jobId);
  assert.equal(Date.parse(started.deadlineAt) - clock.now().getTime(), 2_000);
});

test('processed source left by a Worker crash is enrolled exactly once after restart', async (t) => {
  const context = await fixture(t);
  context.sources.enqueuePage('work', '0', { nextCursor: '1',
    events: [{ eventId: '1', change: source('1', 'v1').change }] });
  context.sources.database.prepare("UPDATE source_events SET status='processed'").run();
  await context.reopen();
  let store = new AssistantAutomationStore(context.sources.database, context.clock.now);
  assert.equal(store.reconcileProcessedSources(), 1);
  const job = store.next();
  assert.equal(job.kind, 'review-work');
  await context.reopen();
  store = new AssistantAutomationStore(context.sources.database, context.clock.now);
  assert.equal(store.reconcileProcessedSources(), 0);
  assert.equal(store.next().jobId, job.jobId);
});

test('old empty understanding results are reanalyzed once while their source is current', async (t) => {
  const { sources, clock } = await fixture(t);
  const event = source('1', 'v1', 'assistant');
  sources.enqueuePage('assistant', '0', { nextCursor: '1',
    events: [{ eventId: '1', change: event.change }] });
  sources.setCurrent(sources.event('assistant', '1'), 'available',
    'User: 默认使用英文文件名。\nAssistant: 好的。');
  const store = new AssistantAutomationStore(sources.database, clock.now);
  assert.equal(store.reconcileProcessedSources(), 1);
  const original = store.next();
  store.start(original.jobId);
  assert.equal(store.commit(original.jobId, () =>
    store.recordCheckpoint(original, { observationCount: 0 })), true);
  assert.equal(store.requeueEmptyUnderstanding(), 1);
  assert.equal(store.requeueEmptyUnderstanding(), 0);
  const retry = store.next();
  assert.equal(retry.kind, 'understand-user');
  assert.equal(retry.sourceId, original.sourceId);
  assert.notEqual(retry.jobId, original.jobId);
  sources.forgetSource(original.sourceId);
  assert.equal(store.requeueEmptyUnderstanding(), 0);
});

test('processed delegated result recovers a new Work review after restart', async (t) => {
  const context = await fixture(t);
  const work = { ...source('1', 'work-v1').change, workId: 'work:one' };
  context.sources.enqueuePage('work', '0', { nextCursor: '1',
    events: [{ eventId: '1', change: work }] });
  context.sources.setCurrent(context.sources.event('work', '1'), 'available',
    'User: Check this result.\nAssistant: Working.');
  const delegated = { sourceId: 'delegation:task-one', sourceVersion: 'result-v1',
    kind: 'created', audience, occurredAt: '2026-09-27T00:00:01.000Z',
    contentRef: 'delegation-result:task-one:result-v1', workId: 'work:one' };
  context.sources.enqueuePage('delegation', '0', { nextCursor: '1',
    events: [{ eventId: '1', change: delegated }] });
  context.sources.setCurrent(context.sources.event('delegation', '1'), 'available',
    'Delegated task returned one reference; Work outcome is unverified.');
  await context.reopen();
  const store = new AssistantAutomationStore(context.sources.database, context.clock.now);
  assert.equal(store.reconcileProcessedSources(), 2);
  const review = store.byKey('delegation-review:delegation:task-one:result-v1');
  assert.equal(review.kind, 'review-work');
  assert.equal(review.sourceId, work.sourceId);
  assert.equal(store.reconcileProcessedSources(), 0);
});

test('out-of-order delegation status cannot supersede a newer or terminal job', async (t) => {
  const context = await fixture(t);
  let store = new AssistantAutomationStore(context.sources.database, context.clock.now);
  const running = store.enqueueDelegation('task-1', 'running-hash', audience,
    '2026-09-27T00:00:00.000Z', 'running');
  const completed = store.enqueueDelegation('task-1', 'completed-hash', audience,
    '2026-09-27T00:01:00.000Z', 'completed');
  assert.equal(store.get(running.jobId).status, 'cancelled');
  assert.equal(store.enqueueDelegation('task-1', 'late-running', audience,
    '2026-09-27T00:00:00.000Z', 'running').jobId, completed.jobId);
  assert.equal(store.enqueueDelegation('task-1', 'same-millisecond-running', audience,
    '2026-09-27T00:01:00.000Z', 'running').jobId, completed.jobId);
  await context.reopen();
  store = new AssistantAutomationStore(context.sources.database, context.clock.now);
  assert.equal(store.enqueueDelegation('task-1', 'late-running', audience,
    '2026-09-27T00:00:00.000Z', 'running').jobId, completed.jobId);
  assert.equal(context.sources.database.prepare(`SELECT COUNT(*) AS n FROM automation_jobs
    WHERE kind='verify-delegation'`).get().n, 2);
});

test('work evidence and its work deletion share one canonical source owner', async (t) => {
  const { sources, clock } = await fixture(t);
  const store = new AssistantAutomationStore(sources.database, clock.now);
  const sourceId = 'work-evidence:run-1';
  const created = { ...source('1', 'evidence-v1', 'work-evidence'),
    change: { ...source('1', 'evidence-v1', 'work-evidence').change, sourceId } };
  sources.enqueuePage('work-evidence', '0', { nextCursor: '1',
    events: [{ eventId: '1', change: created.change }] });
  let current = { status: 'available', sourceVersion: 'evidence-v1' };
  const host = { currentSource: async () => current,
    readSource: async () => ({ status: 'available', sourceVersion: 'evidence-v1',
      text: '工具结果与产物的授权摘要。' }) };
  assert.equal((await sources.processNext(host)).status, 'processed');
  assert.equal(store.reconcileProcessedSources(), 1);
  const review = store.byKey(`source:review-work:${sourceId}:evidence-v1`);
  assert.equal(review.kind, 'review-work');
  const deleted = { ...source('2', 'deleted-v1', 'work-deletions', 'deleted'),
    change: { ...source('2', 'deleted-v1', 'work-deletions', 'deleted').change, sourceId } };
  sources.enqueuePage('work-deletions', '0', { nextCursor: '2',
    events: [{ eventId: '2', change: deleted.change }] });
  current = { status: 'deleted', sourceVersion: 'deleted-v1' };
  assert.equal((await sources.processNext(host)).status, 'processed');
  assert.equal(sources.source(sourceId).availability, 'deleted');
  assert.equal(store.reconcileProcessedSources(), 1);
  assert.equal(store.get(review.jobId).status, 'cancelled');
  assert.equal(store.byKey(`source:maintain-memory:${sourceId}:deleted-v1`).kind, 'maintain-memory');
});

test('daily and weekly checks use current local period and collapse missed downtime', async (t) => {
  const context = await fixture(t, new Date(2026, 8, 27, 12));
  let store = new AssistantAutomationStore(context.sources.database, context.clock.now);
  const first = store.scheduleActivePeriods();
  assert.deepEqual(first.map((job) => job.kind), ['daily-check', 'weekly-check']);
  assert.deepEqual(store.scheduleActivePeriods().map((job) => job.jobId), first.map((job) => job.jobId));
  await context.reopen();
  context.clock.set(new Date(2026, 9, 15, 12));
  store = new AssistantAutomationStore(context.sources.database, context.clock.now);
  const resumed = store.scheduleActivePeriods();
  assert.notEqual(resumed[0].jobId, first[0].jobId);
  assert.notEqual(resumed[1].jobId, first[1].jobId);
  assert.deepEqual(first.map((job) => store.get(job.jobId).status), ['cancelled', 'cancelled']);
  assert.equal(store.next().jobId, resumed[0].jobId,
    'a missed old daily check must not execute before the current period');
  assert.equal(context.sources.database.prepare('SELECT COUNT(*) AS n FROM automation_checks').get().n,
    4, 'downtime does not enqueue every missed local day or week');
});

test('memory follow-up checks survive restart and dedupe the same evidence', async (t) => {
  const context = await fixture(t);
  let store = new AssistantAutomationStore(context.sources.database, context.clock.now);
  const candidate = [{ candidateId: 'follow-up-one', fingerprint: 'v1' }];
  assert.equal(store.enqueueFollowUpCheck([]), undefined);
  const first = store.enqueueFollowUpCheck(candidate);
  assert.equal(first.kind, 'follow-up-check');
  assert.equal(store.enqueueFollowUpCheck(candidate).jobId, first.jobId);
  await context.reopen();
  store = new AssistantAutomationStore(context.sources.database, context.clock.now);
  assert.equal(store.enqueueFollowUpCheck(candidate).jobId, first.jobId);
  assert.equal(store.next().jobId, first.jobId);
  assert.notEqual(store.enqueueFollowUpCheck([{ ...candidate[0], fingerprint: 'v2' }]).jobId,
    first.jobId, 'changed evidence can be considered separately');
});

test('foreground and cancellation discard late model proposals before any write', async (t) => {
  const { sources, clock } = await fixture(t);
  const store = new AssistantAutomationStore(sources.database, clock.now);
  const job = store.enqueue({ kind: 'review-work', dedupeKey: 'foreground-one', audience });
  let release;
  const prepared = new Promise((resolve) => { release = resolve; });
  const effects = [];
  const engine = new AssistantAutomationEngine(store, {
    lookup: async () => 'absent',
    prepare: async () => prepared,
    apply: async (item, _proposal, commit) => { commit(() => effects.push(item.effectId)); },
  }, clock.now);
  engine.setForeground(true);
  assert.equal(await engine.tick(), undefined);
  assert.equal(store.get(job.jobId).status, 'queued');
  engine.setForeground(false);
  const running = engine.tick();
  await Promise.resolve();
  engine.setForeground(true);
  release({ costUsd: 0.01, value: 'late model answer' });
  await running;
  assert.deepEqual(effects, []);
  assert.equal(store.get(job.jobId).status, 'waiting');
  engine.setForeground(false);
  const retry = engine.tick();
  await Promise.resolve();
  engine.cancel(job.jobId);
  await retry;
  assert.equal(store.get(job.jobId).status, 'cancelled');
  assert.deepEqual(effects, []);
});

test('budget, restart and unknown result never replay an external effect blindly', async (t) => {
  const context = await fixture(t);
  let store = new AssistantAutomationStore(context.sources.database, context.clock.now);
  const costly = store.enqueue({ kind: 'review-work', dedupeKey: 'costly', audience, maxCostUsd: 0.01 });
  let applied = 0;
  let engine = new AssistantAutomationEngine(store, { lookup: async () => 'absent',
    prepare: async () => ({ costUsd: 0.02 }), apply: async (_item, _proposal, commit) => {
      commit(() => { applied++; });
    } }, context.clock.now);
  await engine.tick();
  assert.equal(store.get(costly.jobId).status, 'failed');
  assert.equal(applied, 0);
  const interrupted = store.enqueue({ kind: 'verify-delegation', dedupeKey: 'remote-1', audience,
    delegationId: 'task-1' });
  store.start(interrupted.jobId);
  await context.reopen();
  store = new AssistantAutomationStore(context.sources.database, context.clock.now);
  assert.equal(store.get(interrupted.jobId).status, 'waiting');
  let lookup = 'unknown';
  let prepared = 0;
  engine = new AssistantAutomationEngine(store, { lookup: async () => lookup,
    prepare: async () => { prepared++; return { costUsd: 0 }; },
    apply: async (_item, _proposal, commit) => { commit(() => { applied++; }); } }, context.clock.now);
  await engine.tick();
  assert.equal(store.get(interrupted.jobId).status, 'waiting');
  assert.equal(prepared, 0);
  context.clock.set(new Date(context.clock.now().getTime() + 60_001));
  lookup = 'applied';
  await engine.tick();
  assert.equal(store.get(interrupted.jobId).status, 'completed');
  assert.equal(applied, 0);
});

test('unimplemented skill jobs stay durable while a periodic checkpoint recovers without replay', async (t) => {
  const context = await fixture(t);
  let store = new AssistantAutomationStore(context.sources.database, context.clock.now);
  const pending = store.enqueue({ kind: 'review-work', dedupeKey: 'waiting-skill', audience });
  let applied = 0;
  let engine = new AssistantAutomationEngine(store, {
    lookup: async (job) => job.kind === 'review-work' ? 'deferred'
      : store.hasCheckpoint(job.effectId) ? 'applied' : 'absent',
    prepare: async () => ({ costUsd: 0, value: { sourceCount: 0 } }),
    apply: async (job, proposal, commit) => { commit(() => {
      store.recordCheckpoint(job, proposal.value); applied++;
    }); },
  }, context.clock.now);
  await engine.tick();
  assert.equal(store.get(pending.jobId).status, 'waiting');
  const [daily] = store.scheduleActivePeriods();
  await engine.tick();
  assert.equal(store.get(daily.jobId).status, 'completed');
  assert.equal(applied, 1);
  context.sources.database.prepare("UPDATE automation_jobs SET status='running' WHERE job_id=?")
    .run(daily.jobId);
  await context.reopen();
  store = new AssistantAutomationStore(context.sources.database, context.clock.now);
  engine = new AssistantAutomationEngine(store, {
    lookup: async (job) => store.hasCheckpoint(job.effectId) ? 'applied' : 'unknown',
    prepare: async () => { throw new Error('Checkpoint must prevent rerun'); },
    apply: async (_job, _proposal, commit) => { commit(() => { applied++; }); },
  }, context.clock.now);
  await engine.tick();
  assert.equal(store.get(daily.jobId).status, 'completed');
  assert.equal(applied, 1);
});

for (const interruption of ['cancel', 'stop', 'foreground']) {
  test(`late async apply cannot commit after ${interruption}`, async (t) => {
    const { sources, clock } = await fixture(t);
    const store = new AssistantAutomationStore(sources.database, clock.now);
    const job = store.enqueue({ kind: 'review-work', dedupeKey: `late-${interruption}`, audience });
    let entered;
    const applying = new Promise((resolve) => { entered = resolve; });
    let release;
    const hold = new Promise((resolve) => { release = resolve; });
    let effects = 0;
    const engine = new AssistantAutomationEngine(store, { lookup: async () => 'absent',
      prepare: async () => ({ costUsd: 0 }),
      apply: async (_item, _proposal, commit) => {
        entered();
        await hold;
        assert.equal(commit(() => { effects++; }), false);
      } }, clock.now);
    const running = engine.tick();
    await applying;
    if (interruption === 'cancel') engine.cancel(job.jobId);
    else if (interruption === 'stop') engine.stop();
    else engine.setForeground(true);
    release();
    await running;
    assert.equal(effects, 0);
    assert.equal(store.get(job.jobId).status, interruption === 'cancel' ? 'cancelled' : 'waiting');
  });
}

import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import test from 'node:test';

import { RuntimeManager } from '../dist/runtime-manager.cjs';

const fixture = resolve(import.meta.dirname, 'fixtures/runtime-process.mjs');

async function waitFor(check, message, timeoutMs = 10_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const value = await check();
    if (value) return value;
    await new Promise((resolveWait) => setTimeout(resolveWait, 20));
  }
  throw new Error(message);
}

async function events(path) {
  try {
    return (await readFile(path, 'utf8')).trim().split('\n').filter(Boolean).map(JSON.parse);
  } catch {
    return [];
  }
}

async function createManager(context, mode, overrides = {}) {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-runtime-manager-'));
  context.after(() => rm(root, { recursive: true, force: true }));
  const eventFile = join(root, 'events.jsonl');
  const counterFile = join(root, 'counter.txt');
  const errors = [];
  const manager = new RuntimeManager(root, root, root, false, '0.1.0', {
    command: { executable: process.execPath, args: [fixture, mode, eventFile, counterFile] },
    startupTimeoutMs: 8_000,
    shutdownGraceMs: 100,
    restartBaseDelayMs: 20,
    onError: (error) => errors.push(error.message),
    ...overrides,
  });
  context.after(() => manager.stop());
  return { manager, eventFile, errors };
}

test('deduplicates concurrent starts and performs a bounded graceful stop', async (context) => {
  const { manager, eventFile } = await createManager(context, 'healthy');
  const results = await Promise.all([manager.start(), manager.start(), manager.info()]);
  assert.equal(results[0].port, results[1].port);
  assert.equal(results[0].version, results[2].version);
  assert.equal((await events(eventFile)).filter((event) => event.event === 'start').length, 1);

  await manager.stop();
  assert.equal((await events(eventFile)).filter((event) => event.event === 'term').length, 1);
});

test('center credentials cross only the authenticated desktop-to-Runtime bridge', async (context) => {
  let apiKey = `agk_test_${'A'.repeat(32)}`;
  const { manager, eventFile } = await createManager(context, 'healthy', {
    centerConnection: async () => ({ endpoint: 'http://127.0.0.1:8790', apiKey }),
  });
  await manager.start();
  apiKey = undefined;
  await manager.refreshCenterConnection();
  const logged = await events(eventFile);
  assert.equal(logged.find((event) => event.event === 'start')?.hasCenterKey, true);
  assert.equal(logged.find((event) => event.event === 'center-connection')?.hasKey, false);
  assert.equal(JSON.stringify(logged).includes('agk_test_'), false);
});

test('loads a validated notification run target through the authenticated Runtime bridge', async (context) => {
  const { manager } = await createManager(context, 'healthy');
  const run = await manager.getAgentRun('run-fixture');
  assert.equal(run.runId, 'run-fixture');
  assert.equal(run.status, 'succeeded');
  assert.equal(run.context.conversation.conversationId, 'default');
  assert.throws(() => manager.getAgentRun(''), /runId must be a non-empty string/);
});

test('management bridge uses authenticated schedule routes and handles empty revoke responses', async (context) => {
  const { manager } = await createManager(context, 'management');
  assert.equal((await manager.listSchedules())[0].scheduleId, 'schedule-fixture');
  assert.equal((await manager.previewSchedule({})).nextTriggerAt, '2026-09-23T01:00:00.000Z');
  assert.equal((await manager.listWecomConnections()).connections[0].status, 'connected');
  assert.equal((await manager.testWecomConnection('test')).status, 'connected');
  assert.equal((await manager.saveWecomConnection({ connectionId: 'test', enabled: false })).status, 'disabled');
  assert.equal((await manager.submitDesktopMessage('fixture message')).runId, 'run-fixture');
  assert.equal((await manager.searchWorkConversations({ query: 'needle', archive: 'all', limit: 5 })).items[0].messageEntryId, 'entry-1');
  assert.equal((await manager.getWorkMessageWindow('work:fixture', 'entry-1', 3)).messages[0].id, 'entry-1');
  assert.equal((await manager.listSchedulePrivateContacts())[0].contactId, 'contact-fixture');
  assert.equal((await manager.getScheduleHistory('schedule-fixture', 3))[0].runId, 'run-fixture');
  assert.equal((await manager.setScheduleEnabled('schedule-fixture', false)).enabled, false);
  assert.equal((await manager.cancelAgentRun('run-fixture')).result, 'already_terminal');
  await manager.revokeSchedulePrivateTarget('route-fixture');
  assert.throws(() => manager.getScheduleHistory('schedule-fixture', 201), /limit must be an integer/);
  assert.throws(() => manager.cancelAgentRun(''), /runId must be a non-empty string/);
});

test('stop cancels a start that has not spawned its Runtime yet', async (context) => {
  const { manager, eventFile } = await createManager(context, 'healthy');
  const starting = manager.start().then(
    () => undefined,
    (error) => error,
  );
  await manager.stop();
  const error = await starting;
  assert.match(error.message, /cancelled because the App is stopping/);
  assert.equal((await events(eventFile)).filter((event) => event.event === 'start').length, 0);
});

test('restarts a crashed Runtime once without overlapping instances', async (context) => {
  const { manager, eventFile, errors } = await createManager(context, 'crash-once');
  const first = await manager.start();
  const starts = await waitFor(async () => {
    const values = (await events(eventFile)).filter((event) => event.event === 'start');
    return values.length >= 2 ? values : undefined;
  }, 'Runtime was not restarted after its crash');
  assert.equal(starts.length, 2);
  assert.notEqual(starts[0].pid, starts[1].pid);
  const sequence = await events(eventFile);
  assert.equal(sequence.findIndex((event) => event.event === 'crash'), 1);
  assert.equal(sequence.findIndex((event) => event.event === 'start' && event.startNumber === 2), 2);
  const recovered = await waitFor(async () => {
    try { return await manager.info(); } catch { return undefined; }
  }, 'restarted Runtime did not become healthy');
  assert.notEqual(recovered.port, first.port);
  assert.equal(errors.some((message) => message.includes('code 23')), true);
});

test('stops restarting after the bounded crash budget is exhausted', async (context) => {
  const { manager, eventFile, errors } = await createManager(context, 'always-crash', {
    restartLimit: 2,
    restartWindowMs: 10_000,
    restartBaseDelayMs: 10,
  });
  await manager.start();
  await waitFor(
    async () => errors.some((message) => message.includes('restart budget is exhausted')),
    'Runtime restart budget was not enforced',
    5_000,
  );
  await new Promise((resolveWait) => setTimeout(resolveWait, 150));
  assert.equal((await events(eventFile)).filter((event) => event.event === 'start').length, 3);
});

test('force terminates a Runtime that ignores the graceful shutdown deadline', async (context) => {
  const { manager, eventFile } = await createManager(context, 'ignore-term');
  await manager.start();
  const [{ pid }] = (await events(eventFile)).filter((event) => event.event === 'start');
  await manager.stop();
  await waitFor(() => {
    try { process.kill(pid, 0); return false; } catch { return true; }
  }, 'Runtime survived the bounded force-termination path');
});

test('rejects an incompatible Runtime protocol with an explicit error and no restart loop', async (context) => {
  const { manager, eventFile } = await createManager(context, 'ready-protocol-mismatch');
  await assert.rejects(
    manager.start(),
    /desktop expects 9, Runtime reported 999/,
  );
  await new Promise((resolveWait) => setTimeout(resolveWait, 100));
  assert.equal((await events(eventFile)).filter((event) => event.event === 'start').length, 1);
});

import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { chmod, copyFile, mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { DatabaseSync } from 'node:sqlite';

import { RuntimeManager } from '../dist/runtime-manager.cjs';

const packagedResources = resolve(import.meta.dirname, '../release/mac-arm64/南医小助.app/Contents/Resources');
const bundledSea = join(packagedResources, 'runtime', 'YuanpuAgentRuntime-darwin-arm64');
const root = await mkdtemp(join(tmpdir(), 'yuanpu-task-021-packaged-update-'));
const userData = join(root, 'user-data');
const home = join(root, 'home');
const workspace = join(root, 'workspace');
const runtimeRoot = join(userData, 'runtime');
const staging = join(runtimeRoot, '.staging');
const databasePath = join(home, 'workflows', 'automation.sqlite');
const previousHome = process.env.YUANPU_HOME;
const managers = [];

function manager(errors) {
  const value = new RuntimeManager(
    join(packagedResources, 'app'), packagedResources, userData, true, '0.1.0',
    { activationStabilityMs: 150, onError: (error) => errors.push(error.message) },
  );
  managers.push(value);
  return value;
}

async function staged(executable, version) {
  await mkdir(staging, { recursive: true });
  const target = join(staging, 'runtime');
  await copyFile(executable, target);
  await chmod(target, 0o755);
  await writeFile(join(staging, 'staged.json'), JSON.stringify({
    version, filename: 'runtime', sha256: createHash('sha256').update(await readFile(target)).digest('hex'),
  }));
}

try {
  process.env.YUANPU_HOME = home;
  await mkdir(join(home, 'app'), { recursive: true });
  await mkdir(workspace, { recursive: true });
  await mkdir(userData, { recursive: true });
  await writeFile(join(home, 'app', 'config.json'), JSON.stringify({
    schemaVersion: 1, provider: 'task-021-fixture', model: 'fixture-model',
    apiKeyEnv: 'TASK_021_UNUSED_KEY', workingDirectory: workspace,
    baseUrl: 'http://127.0.0.1:9/v1', api: 'openai-completions',
  }));
  assert.equal(execFileSync(bundledSea, ['--version'], { encoding: 'utf8' }).trim(), '0.1.0');
  const errors = [];
  const first = manager(errors);
  assert.equal((await first.start()).version, '0.1.0');
  const schedule = await first.createSchedule({
    contractVersion: 1, name: 'Packaged update fixture', prompt: 'synthetic', workspaceId: workspace,
    timing: { kind: 'once', at: '2099-01-01T00:00:00.000Z' }, timeZone: 'UTC', delivery: { kind: 'desktop' },
  });
  assert.equal((await first.listSchedules()).length, 1);
  await first.stop();

  await staged(bundledSea, '0.1.0');
  const second = manager(errors);
  assert.equal((await second.start()).version, '0.1.0');
  const active = JSON.parse(await readFile(join(runtimeRoot, 'current.json'), 'utf8'));
  assert.equal(active.version, '0.1.0');
  assert.equal(active.executable, join(runtimeRoot, 'versions', '0.1.0', 'YuanpuAgentRuntime'));
  assert.equal((await second.listSchedules())[0].scheduleId, schedule.scheduleId);
  await new Promise((done) => setTimeout(done, 350));
  await assert.rejects(readFile(join(runtimeRoot, 'activation-pending.json')), { code: 'ENOENT' });
  await second.stop();

  const badExecutable = join(root, 'bad-runtime');
  await writeFile(badExecutable, '#!/bin/sh\nif [ "$1" = "--version" ]; then echo 9.9.9; else exit 9; fi\n');
  await chmod(badExecutable, 0o755);
  await staged(badExecutable, '9.9.9');
  const third = manager(errors);
  assert.equal((await third.start()).version, '0.1.0');
  assert.equal((await third.listSchedules())[0].scheduleId, schedule.scheduleId);
  const restored = JSON.parse(await readFile(join(runtimeRoot, 'current.json'), 'utf8'));
  assert.deepEqual(restored, active);
  assert.equal(errors.some((message) => message.includes('previous version was restored')), true);
  const database = new DatabaseSync(databasePath, { readOnly: true });
  assert.equal(database.prepare('SELECT COUNT(*) AS count FROM yp_schedules').get().count, 1);
  database.close();
  await third.stop();
  console.log(JSON.stringify({
    status: 'passed', packagedSeaStarts: 3, stagedActivationConfirmed: true,
    failedActivationRolledBack: true, schedulePersisted: true,
  }));
} finally {
  for (const value of managers) await value.stop().catch(() => undefined);
  if (previousHome === undefined) delete process.env.YUANPU_HOME;
  else process.env.YUANPU_HOME = previousHome;
  await rm(root, { recursive: true, force: true });
}

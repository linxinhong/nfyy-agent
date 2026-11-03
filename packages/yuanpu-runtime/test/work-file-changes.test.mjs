import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import test from 'node:test';

import {
  beginWorkFileCapture,
  completeWorkFileCapture,
  mergeWorkFileChanges,
  openYuanpuMetadataDatabase,
  resolveWorkspaceRelativePath,
  WORK_FILE_CHANGE_MAX_BYTES,
} from '../dist/index.mjs';

function reader(files) {
  return async (absolutePath) => {
    if (!(absolutePath in files)) return null;
    const value = files[absolutePath];
    if (value instanceof Error) throw value;
    return Buffer.from(value, 'utf8');
  };
}

test('workspace path resolution confines targets to the root directory', () => {
  const root = '/tmp/yuanpu-review-root';
  assert.equal(resolveWorkspaceRelativePath(root, 'src/a.ts'), 'src/a.ts');
  assert.equal(resolveWorkspaceRelativePath(root, '/tmp/yuanpu-review-root/src/a.ts'), 'src/a.ts');
  assert.equal(resolveWorkspaceRelativePath(root, '../outside.ts'), undefined);
  assert.equal(resolveWorkspaceRelativePath(root, '/etc/passwd'), undefined);
  assert.equal(resolveWorkspaceRelativePath(root, ''), undefined);
});

test('capture records new and modified files and skips unchanged or failed reads', async () => {
  const root = '/tmp/yuanpu-review-root';
  const files = { [resolve(root, 'a.ts')]: 'const b = 1;\n' };
  const read = reader(files);
  const capture = beginWorkFileCapture({ rootDir: root, requestedPath: 'new.ts', toolCallId: 't1', toolName: 'write', reader: read });
  assert.ok(capture);
  assert.equal(capture.relativePath, 'new.ts');
  const before = await capture.before;
  assert.equal(before, null, 'missing file captures a null before snapshot');
  files[resolve(root, 'new.ts')] = 'export const done = true;\n';
  const change = await completeWorkFileCapture(capture, read);
  assert.ok(change);
  assert.equal(change.before, null);
  assert.equal(change.after.content, 'export const done = true;\n');

  const sameCapture = beginWorkFileCapture({ rootDir: root, requestedPath: 'a.ts', toolCallId: 't2', toolName: 'edit', reader: read });
  assert.equal(await (await completeWorkFileCapture(sameCapture, read)), undefined, 'unchanged file is skipped');

  const failing = beginWorkFileCapture({ rootDir: root, requestedPath: 'a.ts', toolCallId: 't3', toolName: 'edit', reader: reader({ [resolve(root, 'a.ts')]: new Error('boom') }) });
  assert.equal(await completeWorkFileCapture(failing, read), undefined, 'failed before read skips capture');
});

test('capture truncates oversized snapshots and keeps the full-size digest basis', async () => {
  const root = '/tmp/yuanpu-review-root';
  const big = 'x'.repeat(WORK_FILE_CHANGE_MAX_BYTES + 10);
  const read = reader({ [resolve(root, 'big.txt')]: big });
  const capture = beginWorkFileCapture({ rootDir: root, requestedPath: 'big.txt', toolCallId: 't4', toolName: 'edit', reader: read });
  assert.equal((await capture.before).truncated, true);
  assert.equal((await capture.before).content.length, WORK_FILE_CHANGE_MAX_BYTES);
});

test('merge keeps the first before and the last after per file in first-seen order', () => {
  const merged = mergeWorkFileChanges([
    { changeId: 1, conversationId: 'c1', piSessionId: 's1', runId: 'r1', toolCallId: 't1', toolName: 'edit',
      relativePath: 'a.ts', before: { content: 'a1', sha256: 'x', size: 2, truncated: false },
      after: { content: 'a2', sha256: 'y', size: 2, truncated: false }, createdAt: '2026-01-01T00:00:00Z' },
    { changeId: 2, conversationId: 'c1', piSessionId: 's1', runId: 'r1', toolCallId: 't2', toolName: 'edit',
      relativePath: 'b.ts', before: null, after: { content: 'b1', sha256: 'z', size: 2, truncated: false }, createdAt: '2026-01-01T00:00:01Z' },
    { changeId: 3, conversationId: 'c1', piSessionId: 's1', runId: 'r2', toolCallId: 't3', toolName: 'write',
      relativePath: 'a.ts', before: { content: 'a9', sha256: 'w', size: 2, truncated: false },
      after: { content: 'a3', sha256: 'v', size: 2, truncated: false }, createdAt: '2026-01-01T00:00:02Z' },
  ]);
  assert.deepEqual(merged.map((item) => item.path), ['a.ts', 'b.ts']);
  assert.equal(merged[0].before.content, 'a1');
  assert.equal(merged[0].after.content, 'a3');
  assert.equal(merged[0].runId, 'r2');
  assert.equal(merged[0].updatedAt, '2026-01-01T00:00:02Z');
  assert.equal(merged[1].before, null);
});

test('store records idempotently, strips the work namespace and filters by run', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-file-changes-'));
  const database = openYuanpuMetadataDatabase(join(root, 'automation.sqlite'));
  context.after(() => database.close());
  context.after(() => rm(root, { recursive: true, force: true }));
  assert.ok(database.schemaVersion >= 16);
  const changes = [
    { toolCallId: 't1', toolName: 'edit', relativePath: 'a.ts',
      before: { content: 'a1', sha256: 'x', size: 2, truncated: false },
      after: { content: 'a2', sha256: 'y', size: 2, truncated: false } },
    { toolCallId: 't2', toolName: 'write', relativePath: 'new.ts', before: null,
      after: { content: 'n1', sha256: 'n', size: 2, truncated: false } },
  ];
  database.workFileChanges.record('work:conv-1', 'pi-1', 'run-1', changes);
  database.workFileChanges.record('work:conv-1', 'pi-1', 'run-1', changes);

  const all = database.workFileChanges.list('work:conv-1');
  assert.equal(all.length, 2, 'duplicate record is ignored');
  assert.equal(all[0].conversationId, 'conv-1');
  const scoped = database.workFileChanges.list('conv-1', 'run-1');
  assert.equal(scoped.length, 2);
  assert.equal(database.workFileChanges.list('conv-1', 'run-other').length, 0);
  const merged = mergeWorkFileChanges(all);
  assert.equal(merged.length, 2);
  assert.equal(merged[1].before, null);
});

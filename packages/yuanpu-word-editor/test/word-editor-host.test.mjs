import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { chmod, copyFile, mkdtemp, readFile, rm, symlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { WordEditorHost } from '../dist/index.js';

async function fixture(t, extra = {}) {
  const root = await mkdtemp(join(tmpdir(), 'word-host-test-'));
  const enginePath = join(root, 'fake-engine.mjs');
  await copyFile(new URL('./fake-engine.mjs', import.meta.url), enginePath);
  await chmod(enginePath, 0o755);
  await writeFile(join(root, 'source.docx'), 'ORIGINAL');
  const engineSha256 = createHash('sha256').update(await readFile(enginePath)).digest('hex');
  const host = new WordEditorHost({ workspaceRoot: root, enginePath, engineSha256, ...extra });
  const open = host.open.bind(host);
  host.open = (path, options) => open(path, { load: 'model', ...options });
  t.after(async () => { await host.dispose({ discardChanges: true }); await rm(root, { recursive: true, force: true }); });
  return { host, root };
}

function code(expected) { return (error) => error.code === expected; }

test('concurrent opens share a session; queued replacement is saved before close', async (t) => {
  const { host, root } = await fixture(t);
  const [first, second] = await Promise.all([host.open('source.docx'), host.open('source.docx')]);
  assert.equal(first.id, second.id);
  assert.equal(new URL(first.url).hostname, '127.0.0.1');
  const edit = host.replaceText(first.id, 'ORIGINAL', 'CHANGED');
  const save = host.save(first.id);
  const close = host.close(first.id);
  assert.equal(await edit, 1);
  await Promise.all([save, close]);
  assert.equal(await readFile(join(root, 'source.docx'), 'utf8'), 'CHANGED');
  await assert.rejects(host.status(first.id), code('unknown_document'));
  const reopened = await host.open('source.docx');
  assert.equal((await host.findText(reopened.id, 'CHANGED')).length, 1);
});

test('dirty close and disposal preserve changes; Save As switches later save targets', async (t) => {
  const { host, root } = await fixture(t);
  const document = await host.open('source.docx');
  await host.replaceText(document.id, 'ORIGINAL', 'COPY');
  await assert.rejects(host.close(document.id), code('unsaved_changes'));
  await assert.rejects(host.dispose(), code('unsaved_changes'));
  const renamed = await host.saveAs(document.id, 'copy.docx');
  assert.equal(renamed.id, document.id);
  assert.equal(renamed.path, 'copy.docx');
  await assert.rejects(host.open('source.docx'), code('document_already_open'));
  await host.replaceText(document.id, 'COPY', 'LATEST');
  await host.save(document.id);
  assert.equal(await readFile(join(root, 'copy.docx'), 'utf8'), 'LATEST');
  assert.equal(await readFile(join(root, 'source.docx'), 'utf8'), 'ORIGINAL');
  await assert.rejects(host.saveAs(document.id, 'source.docx'), code('destination_exists'));
  await host.close(document.id);
  const original = await host.open('source.docx');
  assert.equal((await host.findText(original.id, 'ORIGINAL')).length, 1);
});

test('rejects traversal, external symlinks, changed save paths and oversized documents', async (t) => {
  const { host, root } = await fixture(t, { maxFileBytes: 16 });
  const outside = await mkdtemp(join(tmpdir(), 'word-outside-'));
  t.after(() => rm(outside, { recursive: true, force: true }));
  await writeFile(join(outside, 'other.docx'), 'SECRET');
  await symlink(outside, join(root, 'escape'));
  await symlink(join(outside, 'other.docx'), join(root, 'linked.docx'));
  for (const path of ['../other.docx', '/tmp/other.docx', 'linked.docx']) {
    await assert.rejects(host.open(path), code('path_outside_workspace'));
  }
  await writeFile(join(root, 'large.docx'), 'a'.repeat(17));
  await assert.rejects(host.open('large.docx'), code('file_too_large'));
  await assert.rejects(host.open('source.txt'), code('unsupported_format'));
  const document = await host.open('source.docx');
  await assert.rejects(host.saveAs(document.id, 'escape/new.docx'), code('path_outside_workspace'));
  await rm(join(root, 'source.docx'));
  await symlink(join(outside, 'other.docx'), join(root, 'source.docx'));
  await assert.rejects(host.save(document.id), code('path_outside_workspace'));
  assert.equal(await readFile(join(outside, 'other.docx'), 'utf8'), 'SECRET');
});

test('read-only session rejects mutations and invalid identities', async (t) => {
  const { host } = await fixture(t);
  const document = await host.open('source.docx', { mode: 'readonly' });
  assert.equal(new URL(document.url).searchParams.get('toolbar'), 'hide');
  for (const operation of [host.save(document.id), host.saveAs(document.id, 'copy.docx'), host.replaceText(document.id, 'ORIGINAL', 'CHANGED')]) {
    await assert.rejects(operation, code('readonly_document'));
  }
  await assert.rejects(host.open('source.docx', { mode: 'edit' }), code('document_already_open'));
  await assert.rejects(host.findText('foreign-id', 'ORIGINAL'), code('unknown_document'));
});

test('pinned engine mismatch does not launch; dead engine fails promptly', async (t) => {
  const mismatched = await fixture(t, { engineSha256: '0'.repeat(64) });
  await assert.rejects(mismatched.host.open('source.docx'), code('engine_integrity'));
  const dead = await fixture(t);
  await writeFile(join(dead.root, 'dead-engine'), '#!/usr/bin/env node\nprocess.exit(3);\n');
  await chmod(join(dead.root, 'dead-engine'), 0o755);
  const engineSha256 = createHash('sha256').update(await readFile(join(dead.root, 'dead-engine'))).digest('hex');
  const host = new WordEditorHost({ enginePath: join(dead.root, 'dead-engine'), engineSha256, workspaceRoot: dead.root, startupTimeoutMs: 1000 });
  t.after(() => host.dispose({ discardChanges: true }));
  await assert.rejects(host.open('source.docx', { load: 'model' }), code('engine_start_failed'));
});

test('request timeout is bounded and disposal terminates the owned process', async (t) => {
  const { host, root } = await fixture(t, { requestTimeoutMs: 150 });
  const document = await host.open('source.docx');
  const canonical = new URL(document.url).searchParams.get('localFilePath');
  const origin = document.origin;
  await writeFile(join(root, 'hang.docx'), 'ORIGINAL');
  await assert.rejects(host.open('hang.docx'), code('request_cancelled'));
  assert.ok(canonical.endsWith('/source.docx'));
  await host.dispose();
  await assert.rejects(fetch(`${origin}/localapi/editor/status`));
  await assert.rejects(host.open('source.docx'), code('disposed'));
});

test('frame imports own their native IDs; the host binds operations by the authorized document path', async (t) => {
  const { host, root } = await fixture(t);
  const document = await host.open('source.docx', { load: 'frame' });
  await assert.rejects(host.status(document.id), code('document_not_ready'));
  const path = new URL(document.url).searchParams.get('localFilePath');
  const response = await fetch(`${document.origin}/localapi/open`, { method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ file_id: 'frontend-generated-id', file_path: path }) });
  assert.equal(response.status, 200);
  assert.equal((await host.status(document.id)).dirty, false);
  await host.replaceText(document.id, 'ORIGINAL', 'FRAME_EDIT');
  await host.save(document.id);
  assert.equal(await readFile(join(root, 'source.docx'), 'utf8'), 'FRAME_EDIT');
  await host.close(document.id);
});

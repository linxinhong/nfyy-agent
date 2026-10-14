import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { copyFile, mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { WordEditorHost } from '../dist/index.js';
import { engineConfig } from './engine-config.mjs';

const { enginePath, engineSha256 } = await engineConfig();
const root = await mkdtemp(join(tmpdir(), 'yuanpu-word-native-smoke-'));
await copyFile(new URL('../test/fixtures/sample.docx', import.meta.url), join(root, 'sample.docx'));
const digest = async (path) => createHash('sha256').update(await readFile(path)).digest('hex');
const originalDigest = await digest(join(root, 'sample.docx'));
const host = new WordEditorHost({ enginePath, engineSha256, workspaceRoot: root });
try {
  const document = await host.open('sample.docx', { load: 'model' });
  assert.equal((await host.findText(document.id, 'ORIGINAL_MARKER')).length, 1);
  assert.equal(await host.replaceText(document.id, 'ORIGINAL_MARKER', 'SAVED_MARKER'), 1);
  assert.equal((await host.status(document.id)).dirty, true);
  await assert.rejects(host.close(document.id), (error) => error.code === 'unsaved_changes');
  await assert.rejects(host.dispose(), (error) => error.code === 'unsaved_changes');
  const copied = await host.saveAs(document.id, 'saved-as.docx');
  assert.equal(copied.path, 'saved-as.docx');
  assert.equal((await host.status(document.id)).dirty, false);
  assert.equal(await digest(join(root, 'sample.docx')), originalDigest);
  await host.replaceText(document.id, 'SAVED_MARKER', 'SECOND_MARKER');
  await host.save(document.id);
  await host.close(document.id);
  const reopened = await host.open('saved-as.docx', { load: 'model' });
  assert.equal((await host.findText(reopened.id, 'SECOND_MARKER')).length, 1);
  assert.equal((await host.findText(reopened.id, 'ORIGINAL_MARKER')).length, 0);
  await host.close(reopened.id);
  const original = await host.open('sample.docx', { mode: 'readonly', load: 'model' });
  assert.equal((await host.findText(original.id, 'ORIGINAL_MARKER')).length, 1);
  await assert.rejects(host.replaceText(original.id, 'ORIGINAL_MARKER', 'INVALID'), (error) => error.code === 'readonly_document');
  await host.close(original.id);
  await host.dispose();
  await assert.rejects(fetch(`${document.origin}/localapi/editor/status`));
  console.log(JSON.stringify({ ok: true, engineSha256, root: process.env.YUANPU_WORD_KEEP_FIXTURE ? root : undefined,
    checks: ['native-import', 'edit', 'dirty-guard', 'save-as', 'save', 'reopen-persistence', 'original-preserved', 'readonly', 'process-cleanup'] }, null, 2));
} finally {
  await host.dispose({ discardChanges: true });
  if (!process.env.YUANPU_WORD_KEEP_FIXTURE) await rm(root, { recursive: true, force: true });
}

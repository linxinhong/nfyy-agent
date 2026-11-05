import assert from 'node:assert/strict';
import { test } from 'node:test';
import { mkdtemp, mkdir, writeFile, symlink, rm, realpath, readFile, readdir } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { register } from 'tsx/esm/api';

register();

const {
  MAX_TEXT_PREVIEW_BYTES,
  MAX_TREE_ENTRIES,
  WorkspaceFileAccessError,
  classifyWorkspaceFile,
  isProbablyBinary,
  listWorkspaceFiles,
  listWorkspaceTree,
  readWorkspaceFile,
  resolveWorkspacePath,
  resolveWorkspaceFilePath,
  importWorkspaceFile,
  safeImportName,
} = await import('../src/workspace-files.ts');

async function createWorkspace() {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-workspace-files-'));
  await mkdir(join(root, 'src'));
  await mkdir(join(root, 'docs'));
  await writeFile(join(root, 'README.md'), '# 标题\n\n工作区说明。\n');
  await writeFile(join(root, 'src', 'index.ts'), 'export const answer = 42;\n');
  await writeFile(join(root, 'docs', 'report.pdf'), Buffer.from('%PDF-1.4 fake pdf bytes'));
  await writeFile(join(root, 'logo.png'), Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]));
  await writeFile(join(root, 'empty.txt'), '');
  return root;
}

async function assertAccessError(promise, statusCode) {
  await assert.rejects(promise, (error) => {
    assert.ok(error instanceof WorkspaceFileAccessError);
    assert.equal(error.statusCode, statusCode);
    return true;
  });
}

test('listWorkspaceFiles lists directories first and normalizes the root path', async () => {
  const root = await createWorkspace();
  try {
    const listing = await listWorkspaceFiles(root, '');
    assert.equal(listing.path, '');
    assert.deepEqual(listing.entries.map((entry) => entry.name), ['docs', 'src', 'empty.txt', 'logo.png', 'README.md']);
    assert.equal(listing.entries[0]?.kind, 'directory');
    const nested = await listWorkspaceFiles(root, './src/');
    assert.equal(nested.path, 'src');
    assert.deepEqual(nested.entries.map((entry) => entry.name), ['index.ts']);
    assert.equal(typeof nested.entries[0]?.size, 'number');
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('resolveWorkspacePath rejects .., absolute escape and missing paths', async () => {
  const root = await createWorkspace();
  try {
    await assertAccessError(resolveWorkspacePath(root, '../outside.txt'), 400);
    await assertAccessError(resolveWorkspacePath(root, 'src/../../secret'), 400);
    await assertAccessError(resolveWorkspacePath(root, join(root, 'README.md')), 400);
    await assertAccessError(resolveWorkspacePath(root, 'src\0nul'), 400);
    const resolved = await resolveWorkspacePath(root, './src/index.ts');
    assert.equal(resolved, await realpath(join(root, 'src', 'index.ts')));
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('resolveWorkspacePath rejects symlink escapes but allows in-workspace links', async () => {
  const root = await createWorkspace();
  const outside = await mkdtemp(join(tmpdir(), 'yuanpu-outside-'));
  try {
    await writeFile(join(outside, 'secret.txt'), 'outside');
    await symlink(outside, join(root, 'escape-dir'));
    await symlink(join(outside, 'secret.txt'), join(root, 'escape-file.txt'));
    await symlink('src', join(root, 'inside-link'));
    await assertAccessError(resolveWorkspacePath(root, 'escape-dir/secret.txt'), 400);
    await assertAccessError(resolveWorkspacePath(root, 'escape-file.txt'), 400);
    const inside = await resolveWorkspacePath(root, 'inside-link/index.ts');
    assert.equal(inside, await realpath(join(root, 'src', 'index.ts')));
  } finally {
    await rm(root, { recursive: true, force: true });
    await rm(outside, { recursive: true, force: true });
  }
});

test('resolveWorkspaceFilePath accepts regular files and rejects directories or escapes', async () => {
  const root = await createWorkspace();
  const outside = await mkdtemp(join(tmpdir(), 'yuanpu-outside-'));
  try {
    await writeFile(join(outside, 'secret.txt'), 'outside');
    await symlink(join(outside, 'secret.txt'), join(root, 'escape-file.txt'));
    assert.equal(await resolveWorkspaceFilePath(root, 'README.md'), await realpath(join(root, 'README.md')));
    await assertAccessError(resolveWorkspaceFilePath(root, 'src'), 400);
    await assertAccessError(resolveWorkspaceFilePath(root, 'escape-file.txt'), 400);
    await assertAccessError(resolveWorkspaceFilePath(root, '../secret.txt'), 400);
  } finally {
    await rm(root, { recursive: true, force: true });
    await rm(outside, { recursive: true, force: true });
  }
});

test('readWorkspaceFile reads text, markdown and empty files', async () => {
  const root = await createWorkspace();
  try {
    const markdown = await readWorkspaceFile(root, 'README.md');
    assert.equal(markdown.kind, 'text');
    if (markdown.kind !== 'text') return;
    assert.equal(markdown.truncated, false);
    assert.match(markdown.content, /# 标题/);
    assert.equal(markdown.size, Buffer.byteLength('# 标题\n\n工作区说明。\n'));
    const empty = await readWorkspaceFile(root, 'empty.txt');
    assert.equal(empty.kind, 'text');
    if (empty.kind !== 'text') return;
    assert.equal(empty.content, '');
    await assertAccessError(readWorkspaceFile(root, 'missing.txt'), 404);
    await assertAccessError(readWorkspaceFile(root, ''), 400);
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('readWorkspaceFile returns image and pdf base64 payloads', async () => {
  const root = await createWorkspace();
  try {
    const tinyPng = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/nS8AAAAASUVORK5CYII=', 'base64');
    await writeFile(join(root, 'arch.png'), tinyPng);
    const image = await readWorkspaceFile(root, 'logo.png');
    assert.equal(image.kind, 'image');
    if (image.kind !== 'image') return;
    assert.equal(image.mediaType, 'image/png');
    const bytes = Buffer.from(image.base64, 'base64');
    assert.deepEqual([...bytes.subarray(0, 4)], [0x89, 0x50, 0x4e, 0x47]);
    assert.equal(isProbablyBinary(tinyPng), true);
    const realImage = await readWorkspaceFile(root, 'arch.png');
    assert.equal(realImage.kind, 'image');
    if (realImage.kind === 'image') assert.deepEqual(Buffer.from(realImage.base64, 'base64'), tinyPng);
    await writeFile(join(root, 'fake.png'), 'this is not a PNG');
    assert.equal((await readWorkspaceFile(root, 'fake.png')).kind, 'unsupported');
    const pdf = await readWorkspaceFile(root, 'docs/report.pdf');
    assert.equal(pdf.kind, 'pdf');
    if (pdf.kind !== 'pdf') return;
    assert.match(Buffer.from(pdf.base64, 'base64').toString('utf-8'), /^%PDF-1.4/);
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('readWorkspaceFile truncates oversized text and refuses binary content', async () => {
  const root = await createWorkspace();
  try {
    const big = 'a'.repeat(MAX_TEXT_PREVIEW_BYTES + 10);
    await writeFile(join(root, 'big.txt'), big);
    const truncated = await readWorkspaceFile(root, 'big.txt');
    assert.equal(truncated.kind, 'text');
    if (truncated.kind !== 'text') return;
    assert.equal(truncated.truncated, true);
    assert.ok(truncated.content.length <= MAX_TEXT_PREVIEW_BYTES);

    const binary = Buffer.concat([Buffer.from([0x00, 0x01, 0x02]), Buffer.alloc(64, 0x03)]);
    await writeFile(join(root, 'blob.bin'), binary);
    const unsupported = await readWorkspaceFile(root, 'blob.bin');
    assert.equal(unsupported.kind, 'unsupported');
    if (unsupported.kind !== 'unsupported') return;
    assert.match(unsupported.reason, /二进制/);
    assert.equal(unsupported.size, binary.length);
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('classifyWorkspaceFile and isProbablyBinary follow extension and content rules', () => {
  assert.equal(classifyWorkspaceFile('docs/report.PDF'), 'pdf');
  assert.equal(classifyWorkspaceFile('a/logo.jpeg'), 'image');
  assert.equal(classifyWorkspaceFile('a/b/notes.markdown'), 'text');
  assert.equal(isProbablyBinary(Buffer.from([0x00, 0x01])), true);
  assert.equal(isProbablyBinary(Buffer.from('正常文本内容')), false);
  assert.equal(isProbablyBinary(Buffer.alloc(0)), false);
});

test('listWorkspaceTree flattens descendants and reports truncation', async () => {
  const root = await createWorkspace();
  try {
    await mkdir(join(root, 'src', 'deep'));
    await writeFile(join(root, 'src', 'deep', 'leaf.txt'), 'leaf');
    const tree = await listWorkspaceTree(root, '');
    assert.equal(tree.truncated, false);
    assert.deepEqual(
      tree.entries.filter((entry) => entry.kind === 'file').map((entry) => entry.path).sort(),
      ['README.md', 'docs/report.pdf', 'empty.txt', 'logo.png', 'src/deep/leaf.txt', 'src/index.ts'],
    );
    assert.ok(tree.entries.some((entry) => entry.path === 'src/deep' && entry.kind === 'directory'));
    const nested = await listWorkspaceTree(root, 'src');
    assert.deepEqual(
      nested.entries.map((entry) => entry.path).sort(),
      ['src/deep', 'src/deep/leaf.txt', 'src/index.ts'],
    );
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('listWorkspaceTree skips symlink escapes and cuts symlink cycles', async () => {
  const root = await createWorkspace();
  const outside = await mkdtemp(join(tmpdir(), 'yuanpu-outside-'));
  try {
    await writeFile(join(outside, 'secret.txt'), 'outside');
    await symlink(outside, join(root, 'escape-dir'));
    await symlink(join(root, 'src'), join(root, 'src', 'self-loop'));
    const tree = await listWorkspaceTree(root, '');
    const paths = tree.entries.map((entry) => entry.path);
    assert.ok(!paths.includes('escape-dir/secret.txt'));
    assert.ok(!paths.some((path) => path.includes('self-loop') && path.split('self-loop').length > 2));
    assert.ok(paths.includes('src/index.ts'));
  } finally {
    await rm(root, { recursive: true, force: true });
    await rm(outside, { recursive: true, force: true });
  }
});

test('listWorkspaceTree stops growing at the entry cap', async () => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-tree-cap-'));
  try {
    for (let index = 0; index < MAX_TREE_ENTRIES + 10; index += 1) {
      await writeFile(join(root, `file-${String(index).padStart(5, '0')}.txt`), 'x');
    }
    const tree = await listWorkspaceTree(root, '');
    assert.equal(tree.entries.length, MAX_TREE_ENTRIES);
    assert.equal(tree.truncated, true);
  } finally { await rm(root, { recursive: true, force: true }); }
});

async function* chunks(...parts) { for (const part of parts) yield Buffer.from(part); }

test('dropped files are saved under 附件 without overwriting and keep their bytes', async (t) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-import-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const first = await importWorkspaceFile(root, '合同 v2.pdf', chunks('%PDF-', '1.7'));
  assert.deepEqual(first, { path: '附件/合同 v2.pdf', size: 8 });
  assert.equal(await readFile(join(root, '附件', '合同 v2.pdf'), 'utf8'), '%PDF-1.7');
  const second = await importWorkspaceFile(root, '合同 v2.pdf', chunks('other'));
  assert.equal(second.path, '附件/合同 v2 (2).pdf');
  assert.equal(await readFile(join(root, '附件', '合同 v2.pdf'), 'utf8'), '%PDF-1.7');
  assert.deepEqual((await readdir(join(root, '附件'))).filter((name) => name.endsWith('.part')), []);
});

test('import names cannot escape the attachment folder', async (t) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-import-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  assert.equal(safeImportName('../../etc/passwd'), 'passwd');
  assert.equal(safeImportName('..\\..\\boot.ini'), 'boot.ini');
  assert.equal(safeImportName('.env'), 'env');
  assert.equal(safeImportName('a:b*c?.docx'), 'a_b_c_.docx');
  assert.equal(safeImportName(''), 'attachment');
  assert.equal(safeImportName('..'), 'attachment');
  const saved = await importWorkspaceFile(root, '../../outside.txt', chunks('x'));
  assert.equal(saved.path, '附件/outside.txt');
});

test('oversized imports are rejected and leave nothing behind', async (t) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-import-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  await assert.rejects(importWorkspaceFile(root, 'big.bin', chunks('12345', '67890'), 8),
    (error) => error instanceof WorkspaceFileAccessError && error.statusCode === 413);
  assert.deepEqual(await readdir(join(root, '附件')), []);
});

test('a symlinked attachment folder pointing outside the workspace is refused', { skip: process.platform === 'win32' }, async (t) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-import-'));
  const outside = await mkdtemp(join(tmpdir(), 'yuanpu-outside-'));
  t.after(() => Promise.all([rm(root, { recursive: true, force: true }), rm(outside, { recursive: true, force: true })]));
  await symlink(outside, join(root, '附件'));
  await assert.rejects(importWorkspaceFile(root, 'x.txt', chunks('x')),
    (error) => error instanceof WorkspaceFileAccessError && error.statusCode === 400);
  assert.deepEqual(await readdir(outside), []);
});

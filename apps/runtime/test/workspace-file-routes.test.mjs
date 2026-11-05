import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { generateKeyPairSync, randomBytes } from 'node:crypto';
import { mkdir, mkdtemp, readFile, realpath, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

test('work file routes serve the workspace with containment enforcement', async (context) => {
  const home = await mkdtemp(join(tmpdir(), 'yuanpu-file-routes-home-'));
  const workspace = await mkdtemp(join(tmpdir(), 'yuanpu-file-routes-workspace-'));
  await mkdir(join(home, 'app'), { recursive: true });
  await writeFile(join(home, 'app', 'config.json'), `${JSON.stringify({
    schemaVersion: 1,
    provider: 'openai',
    model: 'gpt-5.6-luna',
    workingDirectory: workspace,
  }, null, 2)}\n`);

  const token = randomBytes(32).toString('hex');
  const approvalPublicKey = generateKeyPairSync('ed25519').publicKey
    .export({ type: 'spki', format: 'der' }).toString('base64');
  const child = spawn(process.execPath, ['dist/index.cjs', '--serve', '--port', '0'], {
    stdio: ['pipe', 'pipe', 'pipe'],
    env: { ...process.env, YUANPU_HOME: home, YUANPU_PYTHON_MCP_EXECUTABLE: '', YUANPU_PYTHON_MCP_ROOT: '' },
  });
  child.stdin.end(`${JSON.stringify({ token, approvalPublicKey, parentPid: process.pid })}\n`);
  context.after(async () => {
    if (child.exitCode === null && child.signalCode === null) {
      await new Promise((resolve) => {
        child.once('exit', resolve);
        child.kill();
      });
    }
    await rm(home, { recursive: true, force: true });
    await rm(workspace, { recursive: true, force: true });
  });
  const ready = await new Promise((resolve, reject) => {
    let stdout = '';
    const timeout = setTimeout(() => reject(new Error('Runtime did not become ready')), 15_000);
    child.once('error', reject);
    child.once('exit', (code) => reject(new Error(`Runtime exited before ready: ${code}`)));
    child.stdout.setEncoding('utf8');
    child.stdout.on('data', (chunk) => {
      stdout += chunk;
      const newline = stdout.indexOf('\n');
      if (newline < 0) return;
      clearTimeout(timeout);
      resolve(JSON.parse(stdout.slice(0, newline)));
    });
  });
  const root = `http://${ready.host}:${ready.port}`;
  const send = async (path) => fetch(`${root}${path}`, { headers: { authorization: `Bearer ${token}` } });
  const created = await fetch(`${root}/v1/work/conversations`, {
    method: 'POST',
    headers: { authorization: `Bearer ${token}` },
  });
  assert.equal(created.status, 201);
  const conversation = await created.json();
  assert.match(conversation.id, /^work:/);
  // Each conversation owns a fresh working directory; the fixtures go there.
  const conversationWorkspace = conversation.workingDirectory;
  assert.ok(conversationWorkspace && conversationWorkspace !== workspace);
  await mkdir(join(conversationWorkspace, 'sub'));
  await writeFile(join(conversationWorkspace, 'notes.md'), '# 工作区笔记\n内容正文。\n');
  await writeFile(join(conversationWorkspace, 'sub', 'child.txt'), 'child');

  const listing = await send(`/v1/work/files?conversationId=${encodeURIComponent(conversation.id)}`);
  assert.equal(listing.status, 200);
  const listingBody = await listing.json();
  assert.equal(listingBody.path, '');
  assert.deepEqual(listingBody.entries.map((entry) => entry.name), ['sub', 'notes.md']);
  assert.equal(listingBody.entries[0].kind, 'directory');
  assert.equal(listingBody.entries[1].kind, 'file');

  const nested = await send(`/v1/work/files?conversationId=${encodeURIComponent(conversation.id)}&path=${encodeURIComponent('sub')}`);
  assert.equal(nested.status, 200);
  const nestedBody = await nested.json();
  assert.equal(nestedBody.path, 'sub');
  assert.deepEqual(nestedBody.entries.map((entry) => entry.path), ['sub/child.txt']);

  const text = await send(`/v1/work/files/content?conversationId=${encodeURIComponent(conversation.id)}&path=${encodeURIComponent('notes.md')}`);
  assert.equal(text.status, 200);
  const textBody = await text.json();
  assert.equal(textBody.kind, 'text');
  assert.equal(textBody.truncated, false);
  assert.match(textBody.content, /# 工作区笔记/);

  const localPath = await send(`/v1/work/files/path?conversationId=${encodeURIComponent(conversation.id)}&path=${encodeURIComponent('notes.md')}`);
  assert.equal(localPath.status, 200);
  assert.equal((await localPath.json()).path, await realpath(join(conversationWorkspace, 'notes.md')));
  const localDirectory = await send(`/v1/work/files/path?conversationId=${encodeURIComponent(conversation.id)}&path=${encodeURIComponent('sub')}`);
  assert.equal(localDirectory.status, 400);
  const localEscape = await send(`/v1/work/files/path?conversationId=${encodeURIComponent(conversation.id)}&path=${encodeURIComponent('../../secrets')}`);
  assert.equal(localEscape.status, 400);

  const directory = await send(`/v1/work/files/content?conversationId=${encodeURIComponent(conversation.id)}&path=${encodeURIComponent('sub')}`);
  assert.equal(directory.status, 400);

  const escape = await send(`/v1/work/files/content?conversationId=${encodeURIComponent(conversation.id)}&path=${encodeURIComponent('../../secrets')}`);
  assert.equal(escape.status, 400);

  const missing = await send(`/v1/work/files?conversationId=${encodeURIComponent(conversation.id)}&path=${encodeURIComponent('nope.txt')}`);
  assert.equal(missing.status, 404);

  const imported = await fetch(`${root}/v1/work/files/import?conversationId=${encodeURIComponent(conversation.id)}&name=${encodeURIComponent('报告.pdf')}`, {
    method: 'POST', headers: { authorization: `Bearer ${token}`, 'content-type': 'application/octet-stream' }, body: Buffer.from('%PDF-1.7 bytes'),
  });
  assert.equal(imported.status, 200);
  assert.deepEqual(await imported.json(), { path: '附件/报告.pdf', size: 14 });
  assert.equal(await readFile(join(conversationWorkspace, '附件', '报告.pdf'), 'utf8'), '%PDF-1.7 bytes');
  const importUnknown = await fetch(`${root}/v1/work/files/import?conversationId=work:does-not-exist&name=x.txt`, {
    method: 'POST', headers: { authorization: `Bearer ${token}` }, body: 'x',
  });
  assert.equal(importUnknown.status, 404);
  const importUnauthenticated = await fetch(`${root}/v1/work/files/import?conversationId=${encodeURIComponent(conversation.id)}&name=x.txt`, {
    method: 'POST', body: 'x',
  });
  assert.equal(importUnauthenticated.status, 401);

  const unknownConversation = await send('/v1/work/files?conversationId=work:does-not-exist');
  assert.equal(unknownConversation.status, 404);

  const unauthenticated = await fetch(`${root}/v1/work/files?conversationId=${encodeURIComponent(conversation.id)}`);
  assert.equal(unauthenticated.status, 401);
});

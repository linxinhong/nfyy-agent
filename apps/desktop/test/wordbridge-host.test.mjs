import assert from 'node:assert/strict';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { portInUse, WORDBRIDGE_MCP_URL, WordBridgeHost } from '../dist/wordbridge-host.cjs';

async function fixture(t) {
  const root = await mkdtemp(join(tmpdir(), 'wordbridge-host-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  const payloadRoot = join(root, 'payload');
  const word = join(root, 'Microsoft Word.app');
  await mkdir(payloadRoot, { recursive: true });
  await mkdir(word);
  await writeFile(join(payloadRoot, 'payload.json'), '{}');
  return { payloadRoot, stateRoot: join(root, 'state'), homeDirectory: root, wordApplicationPath: word };
}

test('WordBridge is enabled only on macOS with a payload and Microsoft Word installed', async (t) => {
  const options = await fixture(t);
  assert.ok(WordBridgeHost.detect(options, 'darwin'));
  assert.equal(WordBridgeHost.detect(options, 'linux'), undefined);
  assert.equal(WordBridgeHost.detect(options, 'win32'), undefined);
  assert.equal(WordBridgeHost.detect({ ...options, wordApplicationPath: join(options.homeDirectory, 'missing') }, 'darwin'), undefined);
  assert.equal(WordBridgeHost.detect({ ...options, payloadRoot: join(options.homeDirectory, 'empty') }, 'darwin'), undefined);
});

test('WordBridge can be switched off by environment', async (t) => {
  const options = await fixture(t);
  process.env.YUANPU_WORDBRIDGE_DISABLED = '1';
  t.after(() => { delete process.env.YUANPU_WORDBRIDGE_DISABLED; });
  assert.equal(WordBridgeHost.detect(options, 'darwin'), undefined);
});

test('the Runtime receives a loopback endpoint and a fresh per-launch token', async (t) => {
  const options = await fixture(t);
  const first = WordBridgeHost.detect(options, 'darwin');
  const second = WordBridgeHost.detect(options, 'darwin');
  assert.equal(WORDBRIDGE_MCP_URL, 'http://127.0.0.1:9877/mcp');
  assert.deepEqual(Object.keys(first.runtimeEnvironment()).sort(), ['YUANPU_WORDBRIDGE_MCP_TOKEN', 'YUANPU_WORDBRIDGE_MCP_URL']);
  assert.match(first.token, /^[0-9a-f]{64}$/);
  assert.notEqual(first.token, second.token);
  assert.equal(first.python, join(options.payloadRoot, 'python', 'bin', 'python3'));
});

test('port probing distinguishes listening and free loopback ports', async () => {
  const server = createServer();
  await new Promise((resolveListen) => server.listen(0, '127.0.0.1', resolveListen));
  const { port } = server.address();
  assert.equal(await portInUse(port), true);
  server.close();
  await new Promise((resolveClose) => server.once('close', resolveClose));
  assert.equal(await portInUse(port), false);
});

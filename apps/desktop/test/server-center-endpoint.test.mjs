import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm, symlink } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { isCenterPortalUrl, normalizeServerCenterEndpoint, ServerCenterEndpointStore } from '../dist/server-center-endpoint.cjs';

test('center endpoint persists an explicit service address across launches', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-center-endpoint-'));
  context.after(() => rm(root, { recursive: true, force: true }));
  const path = join(root, 'endpoint.json');
  const store = new ServerCenterEndpointStore(path, 'http://127.0.0.1:8788');
  assert.equal(await store.load(), 'http://127.0.0.1:8788');
  assert.equal(await store.save('http://127.0.0.1:8799/'), 'http://127.0.0.1:8799');
  assert.equal(await new ServerCenterEndpointStore(path, 'http://127.0.0.1:8788').load(), 'http://127.0.0.1:8799');
  assert.deepEqual(JSON.parse(await readFile(path, 'utf8')), { version: 1, url: 'http://127.0.0.1:8799' });
});

test('the retired local website address follows the new center default', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-center-endpoint-'));
  context.after(() => rm(root, { recursive: true, force: true }));
  const path = join(root, 'endpoint.json');
  const old = new ServerCenterEndpointStore(path, 'http://127.0.0.1:8789');
  await old.save('http://127.0.0.1:8799');
  await old.save('http://127.0.0.1:8789');
  const current = new ServerCenterEndpointStore(path, 'http://127.0.0.1:8790');
  assert.equal(await current.load(), 'http://127.0.0.1:8790');
});

test('center endpoint rejects credentials and unsafe configuration files', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-center-endpoint-'));
  context.after(() => rm(root, { recursive: true, force: true }));
  for (const value of ['file:///tmp/server', 'http://user:secret@127.0.0.1:8788', 'http://127.0.0.1:8788?key=secret', 'http://127.0.0.1:8788/#token', 'http://center.example.test']) {
    assert.throws(() => normalizeServerCenterEndpoint(value));
  }
  const path = join(root, 'linked.json');
  await symlink(join(root, 'elsewhere.json'), path);
  const store = new ServerCenterEndpointStore(path, 'http://127.0.0.1:8788');
  await assert.rejects(store.save('https://center.example.test'), /不是普通文件/);
});

test('center guest navigation stays on its configured website origin', () => {
  const endpoint = 'http://127.0.0.1:8789';
  assert.equal(isCenterPortalUrl('http://127.0.0.1:8789/#wiki', endpoint), true);
  assert.equal(isCenterPortalUrl('http://127.0.0.1:8788/', endpoint), false);
  assert.equal(isCenterPortalUrl('http://127.0.0.1:8789.evil.test/', endpoint), false);
  assert.equal(isCenterPortalUrl('file:///tmp/private', endpoint), false);
});

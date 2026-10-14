import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm, symlink } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { ServerCenterCredentialStore } from '../dist/server-center-credential.cjs';

const protector = {
  isEncryptionAvailable: () => true,
  getSelectedStorageBackend: () => 'keychain',
  encryptString: (value) => Buffer.from([...value].map((character) => character.charCodeAt(0) ^ 71)),
  decryptString: (value) => String.fromCharCode(...[...value].map((byte) => byte ^ 71)),
};

test('desktop center stores only encrypted API key and reports presence without revealing it', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-center-secret-'));
  context.after(() => rm(root, { recursive: true, force: true }));
  const path = join(root, 'center-key.json');
  const store = new ServerCenterCredentialStore(path, protector);
  const key = `ypk_${'a'.repeat(43)}`;
  assert.deepEqual(await store.status(), { configured: false, storageAvailable: true });
  await store.saveKey(key);
  assert.deepEqual(await store.status(), { configured: true, storageAvailable: true });
  assert.equal((await readFile(path, 'utf8')).includes(key), false);
  assert.equal(await store.loadKey(), key);
  await store.removeKey();
  assert.equal(await store.loadKey(), undefined);
});

test('desktop center rejects symlink credential files and an unprotected backend', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-center-secret-'));
  context.after(() => rm(root, { recursive: true, force: true }));
  const key = `ypk_${'b'.repeat(43)}`;
  const insecure = new ServerCenterCredentialStore(join(root, 'insecure.json'), { ...protector, getSelectedStorageBackend: () => 'basic_text' });
  await assert.rejects(insecure.saveKey(key), /系统安全存储不可用/);
  const path = join(root, 'linked.json');
  await symlink(join(root, 'elsewhere.json'), path);
  const store = new ServerCenterCredentialStore(path, protector);
  await assert.rejects(store.saveKey(key), /不是普通文件/);
});

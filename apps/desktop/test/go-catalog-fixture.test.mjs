import assert from 'node:assert/strict';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { startGoCatalog } from './go-catalog-fixture.mjs';

test('desktop catalog fixture uses the Go backend and publishes the configured manifest', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-go-catalog-'));
  context.after(() => rm(root, { recursive: true, force: true }));
  await writeFile(join(root, 'manifest.json'), JSON.stringify({ id: 'builtin.python.echo', version: '9.8.7', artifacts: [] }));
  const server = await startGoCatalog(root);
  context.after(() => server.close());
  const search = await fetch(`${server.origin}/v1/catalog/search?q=Python`).then((response) => response.json());
  assert.equal(search.items[0].version, '9.8.7');
  const manifest = await fetch(`${server.origin}/v1/capability-packages/builtin.python.echo/manifest`).then((response) => response.json());
  assert.equal(manifest.id, 'builtin.python.echo');
});

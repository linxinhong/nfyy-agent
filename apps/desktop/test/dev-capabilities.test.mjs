import assert from 'node:assert/strict';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { resolveDevCapabilityEnvironment } from '../scripts/dev-capabilities.mjs';

async function fixture(context) {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-dev-capabilities-'));
  context.after(() => rm(root, { recursive: true, force: true }));
  return root;
}

async function prepareArtifacts(root) {
  await mkdir(join(root, 'bundle'), { recursive: true });
  await Promise.all([
    writeFile(join(root, 'manifest.json'), '{}'),
    writeFile(join(root, 'bundle', 'manifest.json'), '{}'),
    writeFile(join(root, 'bundle', 'trust-root.json'), '{}'),
  ]);
}

test('development wires the local catalog artifacts and matching host trust root', async (context) => {
  const root = await fixture(context);
  const artifactRoot = join(root, 'apps/python-capabilities/dist-artifact');
  await prepareArtifacts(artifactRoot);
  const env = await resolveDevCapabilityEnvironment(root, {}, () => assert.fail('unexpected rebuild'));
  assert.equal(env.YUANPU_ARTIFACT_ROOT, artifactRoot);
  assert.equal(env.YUANPU_CAPABILITY_TRUST_ROOT_FILE, join(artifactRoot, 'bundle/trust-root.json'));
});

test('first development launch builds missing signed artifacts before starting services', async (context) => {
  const root = await fixture(context);
  let builds = 0;
  await resolveDevCapabilityEnvironment(root, {}, async () => {
    builds += 1;
    await prepareArtifacts(join(root, 'apps/python-capabilities/dist-artifact'));
  });
  assert.equal(builds, 1);
});

test('explicit artifact and trust root overrides are preserved and never rebuilt', async (context) => {
  const root = await fixture(context);
  await prepareArtifacts(join(root, 'custom'));
  await writeFile(join(root, 'host-trust.json'), '{}');
  await writeFile(join(root, 'manifest.json'), '{}');
  const env = await resolveDevCapabilityEnvironment(root, {
    YUANPU_ARTIFACT_ROOT: 'custom',
    YUANPU_CAPABILITY_TRUST_ROOT_FILE: 'host-trust.json',
  }, () => assert.fail('unexpected rebuild'));
  assert.equal(env.YUANPU_ARTIFACT_ROOT, join(root, 'custom'));
  assert.equal(env.YUANPU_CAPABILITY_TRUST_ROOT_FILE, join(root, 'host-trust.json'));
  await assert.rejects(resolveDevCapabilityEnvironment(root, {
    YUANPU_ARTIFACT_ROOT: 'missing',
  }, () => assert.fail('custom configuration must not be overwritten')), /configured.*missing/i);
});

test('a failed artifact build prevents development startup', async (context) => {
  const root = await fixture(context);
  await assert.rejects(resolveDevCapabilityEnvironment(root, {}, async () => {
    throw new Error('artifact build failed');
  }), /artifact build failed/);
});

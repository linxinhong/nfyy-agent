import assert from 'node:assert/strict';
import { createHash, generateKeyPairSync, sign } from 'node:crypto';
import { spawn } from 'node:child_process';
import { mkdtemp, mkdir, readFile, rm, symlink, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import * as tar from 'tar';

import {
  CapabilityArtifactManager,
  artifactInstallLockPort,
  capabilityManifestDigest,
  capabilityManifestSigningPayload,
  detectMcpOwnershipConflicts,
  validateCapabilityConfig,
} from '../dist/index.mjs';

const { privateKey, publicKey } = generateKeyPairSync('ed25519');
const publicKeyPem = publicKey.export({ format: 'pem', type: 'spki' }).toString();
const healthy = async () => {};

test('validates capability configuration against the signed active schema', () => {
  const schema = {
    type: 'object',
    properties: { responsePrefix: { type: 'string', maxLength: 4 } },
    additionalProperties: false,
  };
  assert.deepEqual(validateCapabilityConfig(schema, { responsePrefix: 'ok' }), {
    valid: true,
    errors: [],
  });
  assert.equal(validateCapabilityConfig(schema, { responsePrefix: 'too-long' }).valid, false);
  assert.equal(validateCapabilityConfig(schema, { unexpected: true }).valid, false);
});

test('manifest digest binds the permissions, connections, and configuration snapshot', () => {
  const base = {
    manifestVersion: 1,
    kind: 'python-mcp',
    id: 'builtin.python.echo',
    version: '1.0.0',
    capabilityContractVersion: 1,
    runtimeCompatibility: { minimum: '0.1.0' },
    artifacts: [],
    configSchema: { type: 'object' },
    permissions: ['background'],
    connections: ['yuanpu_echo_mcp'],
    issuedAt: '2026-09-21T00:00:00.000Z',
    signature: { algorithm: 'ed25519', keyId: 'test-root', value: 'ignored-by-digest' },
  };
  const digest = capabilityManifestDigest(base);
  assert.notEqual(capabilityManifestDigest({ ...base, permissions: ['network'] }), digest);
  assert.notEqual(capabilityManifestDigest({ ...base, connections: ['other'] }), digest);
  assert.notEqual(capabilityManifestDigest({ ...base, configSchema: { type: 'string' } }), digest);
});

async function fixtureRoot(t) {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-artifacts-'));
  t.after(() => rm(root, { recursive: true, force: true }));
  return root;
}

async function archiveFixture(root, { prefix, size = 12, symlinkEntry = false } = {}) {
  const input = join(root, `input-${Math.random()}`);
  await mkdir(input, { recursive: true });
  await writeFile(join(input, 'server'), 'x'.repeat(size));
  if (symlinkEntry) await symlink('server', join(input, 'linked-server'));
  const archive = join(root, `fixture-${Math.random()}.tar.gz`);
  await tar.c({ cwd: input, file: archive, gzip: true, prefix }, ['server', ...(symlinkEntry ? ['linked-server'] : [])]);
  const body = await readFile(archive);
  return { body, sha256: createHash('sha256').update(body).digest('hex') };
}

function signedManifest({ url, body, sha256, version = '1.0.0', issuedAt = '2026-09-21T00:00:00.000Z', runtimeCompatibility } = {}) {
  const manifest = {
    manifestVersion: 1,
    kind: 'python-mcp',
    id: 'builtin.python.echo',
    version,
    capabilityContractVersion: 1,
    runtimeCompatibility: runtimeCompatibility ?? { minimum: '0.1.0', maximumExclusive: '1.0.0' },
    artifacts: [{
      platform: process.platform,
      arch: process.arch,
      format: 'tar.gz',
      url,
      size: body.length,
      sha256,
      entrypoint: 'server',
    }],
    configSchema: { type: 'object', properties: { responsePrefix: { type: 'string' } } },
    permissions: ['background'],
    connections: ['yuanpu_echo_mcp'],
    issuedAt,
    signature: { algorithm: 'ed25519', keyId: 'test-root', value: '' },
  };
  manifest.signature.value = sign(null, capabilityManifestSigningPayload(manifest), privateKey).toString('base64');
  return manifest;
}

async function artifactServer(t, bodies) {
  let requests = 0;
  const server = createServer((request, response) => {
    requests += 1;
    const body = bodies.get(request.url);
    if (!body) {
      response.writeHead(404).end();
      return;
    }
    response.setHeader('content-length', body.length);
    response.end(body);
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => { if (server.listening) server.close(); });
  const address = server.address();
  assert.ok(address && typeof address !== 'string');
  return {
    url: (path) => `http://127.0.0.1:${address.port}${path}`,
    requests: () => requests,
    close: () => new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve())),
  };
}

function manager(root, overrides = {}) {
  return new CapabilityArtifactManager(join(root, 'packages'), {
    runtimeVersion: '0.1.0',
    trustRoots: [{ keyId: 'test-root', publicKeyPem }],
    ...overrides,
  });
}

test('installs signed artifacts atomically, stays offline after install, and preserves Pi settings/config', async (t) => {
  const root = await fixtureRoot(t);
  const fixture = await archiveFixture(root);
  const source = await artifactServer(t, new Map([['/artifacts/archive', fixture.body]]));
  const settingsPath = join(root, 'agent', 'settings.json');
  const configPath = join(root, 'packages', 'config', 'builtin.python.echo', 'user.json');
  await mkdir(join(root, 'agent'), { recursive: true });
  await mkdir(join(root, 'packages', 'config', 'builtin.python.echo'), { recursive: true });
  await writeFile(settingsPath, JSON.stringify({ packages: ['./existing-plugin'] }));
  await writeFile(configPath, JSON.stringify({ token: '${ECHO_TOKEN}' }));

  const instance = manager(root);
  const manifest = signedManifest({ url: 'artifacts/archive', ...fixture });
  const [first, concurrent] = await Promise.all([
    instance.install(manifest, { manifestUrl: source.url('/manifest'), healthCheck: healthy }),
    instance.install(manifest, { manifestUrl: source.url('/manifest'), healthCheck: healthy }),
  ]);
  assert.equal(first.entrypoint, concurrent.entrypoint);
  assert.equal(source.requests(), 1);
  assert.equal(await readFile(first.entrypoint, 'utf8'), 'x'.repeat(12));
  assert.deepEqual(JSON.parse(await readFile(settingsPath, 'utf8')), { packages: ['./existing-plugin'] });
  assert.deepEqual(JSON.parse(await readFile(configPath, 'utf8')), { token: '${ECHO_TOKEN}' });

  await source.close();
  const active = await instance.active('builtin.python.echo');
  assert.equal(active.version, '1.0.0');
  assert.deepEqual(active.permissions, ['background']);
  assert.deepEqual(active.connections, ['yuanpu_echo_mcp']);
  assert.equal(active.configSchema.properties.responsePrefix.type, 'string');
});

test('rejects bad signatures, incompatible runtimes, stale replay, and implicit downgrade', async (t) => {
  const root = await fixtureRoot(t);
  const fixture = await archiveFixture(root);
  const source = await artifactServer(t, new Map([['/artifact', fixture.body]]));
  const instance = manager(root);
  const bad = signedManifest({ url: source.url('/artifact'), ...fixture });
  bad.signature.value = Buffer.from('not-a-signature').toString('base64');
  await assert.rejects(instance.install(bad, { healthCheck: healthy }), /signature is invalid/);
  assert.equal(source.requests(), 0);

  const incompatible = signedManifest({
    url: source.url('/artifact'),
    ...fixture,
    runtimeCompatibility: { minimum: '2.0.0' },
  });
  await assert.rejects(instance.install(incompatible, { healthCheck: healthy }), /incompatible/);
  assert.equal(source.requests(), 0);

  const current = signedManifest({
    url: source.url('/artifact'), ...fixture, version: '2.0.0', issuedAt: '2026-09-21T02:00:00.000Z',
  });
  await instance.install(current, { healthCheck: healthy });
  const replay = signedManifest({
    url: source.url('/artifact'), ...fixture, version: '3.0.0', issuedAt: '2026-09-21T01:00:00.000Z',
  });
  await assert.rejects(instance.install(replay, { healthCheck: healthy }), /stale.*replay/i);
  const downgrade = signedManifest({
    url: source.url('/artifact'), ...fixture, version: '1.0.0', issuedAt: '2026-09-21T03:00:00.000Z',
  });
  await assert.rejects(instance.install(downgrade, { healthCheck: healthy }), /downgrade requires explicit/);
  assert.equal((await instance.active('builtin.python.echo')).version, '2.0.0');
});

test('rejects traversal prefixes, links, and extraction bombs without activating a package', async (t) => {
  const root = await fixtureRoot(t);
  const traversal = await archiveFixture(root, { prefix: '../escape' });
  const linked = await archiveFixture(root, { symlinkEntry: true });
  const oversized = await archiveFixture(root, { size: 64 });
  const source = await artifactServer(t, new Map([
    ['/traversal', traversal.body],
    ['/linked', linked.body],
    ['/oversized', oversized.body],
  ]));
  const instance = manager(root, { maxUnpackedBytes: 32 });
  await assert.rejects(
    instance.install(signedManifest({ url: source.url('/traversal'), ...traversal }), { healthCheck: healthy }),
    /Unsafe archive entry/,
  );
  await assert.rejects(
    instance.install(
      signedManifest({ url: source.url('/linked'), ...linked, version: '1.0.1' }),
      { healthCheck: healthy },
    ),
    /Unsafe archive entry/,
  );
  await assert.rejects(
    instance.install(
      signedManifest({ url: source.url('/oversized'), ...oversized, version: '1.0.2' }),
      { healthCheck: healthy },
    ),
    /extraction exceeded/,
  );
  assert.equal(await instance.active('builtin.python.echo'), undefined);
});

test('retains immutable versions for Windows-safe rollback and recovers from failed health checks', async (t) => {
  const root = await fixtureRoot(t);
  const fixture = await archiveFixture(root);
  const source = await artifactServer(t, new Map([['/artifact', fixture.body]]));
  const instance = manager(root);
  const first = signedManifest({ url: source.url('/artifact'), ...fixture, version: '1.0.0' });
  await instance.install(first, { healthCheck: healthy });
  const second = signedManifest({
    url: source.url('/artifact'), ...fixture, version: '2.0.0', issuedAt: '2026-09-21T01:00:00.000Z',
  });
  const destination = join(root, 'packages', 'artifacts', 'builtin.python.echo', '2.0.0', `${process.platform}-${process.arch}`);
  await assert.rejects(
    instance.install(second, { healthCheck: async (entrypoint, cwd) => {
      assert.equal(cwd, destination);
      assert.equal(entrypoint, join(destination, 'server'));
      assert.equal((await instance.active('builtin.python.echo')).version, '1.0.0');
      await writeFile(entrypoint, 'failed-health-orphan');
      throw new Error('crash during switch');
    } }),
    /crash during switch/,
  );
  assert.equal((await instance.active('builtin.python.echo')).version, '1.0.0');
  await instance.install(second, { healthCheck: async (entrypoint, cwd) => {
    assert.equal(cwd, destination);
    assert.equal(await readFile(entrypoint, 'utf8'), 'x'.repeat(12));
    assert.equal((await instance.active('builtin.python.echo')).version, '1.0.0');
  } });
  assert.equal((await instance.rollback('builtin.python.echo', '1.0.0')).version, '1.0.0');
  assert.equal(await readFile(join(root, 'packages', 'artifacts', 'builtin.python.echo', '2.0.0', `${process.platform}-${process.arch}`, 'server'), 'utf8'), 'x'.repeat(12));
});

test('reports duplicate ownership without changing built-in MCP configuration', async (t) => {
  const root = await fixtureRoot(t);
  await mkdir(join(root, 'agent'), { recursive: true });
  await mkdir(join(root, 'workspace', '.pi'), { recursive: true });
  const config = { mcpServers: { docs: {}, legacy: {}, disabled: { enabled: false } } };
  await writeFile(join(root, 'agent', 'mcp.json'), JSON.stringify(config));
  await writeFile(join(root, 'workspace', '.pi', 'mcp.json'), JSON.stringify({ mcpServers: {
    docs: {}, legacy: {}, disabled: { enabled: false },
  } }));
  const conflicts = await detectMcpOwnershipConflicts({
    yuanpuConnections: ['docs', 'disabled'],
    agentRoot: join(root, 'agent'),
    workspaceRoot: join(root, 'workspace'),
  });
  assert.deepEqual(conflicts, [{
    name: 'docs',
    owners: ['yuanpu', 'pi-mcp-user', 'pi-mcp-workspace'],
  }]);
  assert.deepEqual(JSON.parse(await readFile(join(root, 'agent', 'mcp.json'), 'utf8')), config);
});

test('concurrent first readers initialize state once without truncation', async (t) => {
  const root = await fixtureRoot(t);
  const instances = Array.from({ length: 20 }, () => manager(root));
  const states = await Promise.all(instances.map((instance) => instance.list()));
  assert.deepEqual(states, Array.from({ length: 20 }, () => []));
  const state = JSON.parse(await readFile(join(root, 'packages', 'artifact-state.json'), 'utf8'));
  assert.deepEqual(state, { schemaVersion: 1, packages: {} });
});

test('recovers the install lock after its operating-system owner is interrupted', async (t) => {
  const root = await fixtureRoot(t);
  const fixture = await archiveFixture(root);
  const source = await artifactServer(t, new Map([['/artifact', fixture.body]]));
  const packagesRoot = join(root, 'packages');
  await mkdir(packagesRoot, { recursive: true });
  const holder = spawn(process.execPath, ['-e', [
    "const { createServer } = require('node:net')",
    `const server = createServer().listen(${artifactInstallLockPort(packagesRoot)}, '127.0.0.1', () => console.log('ready'))`,
    'setInterval(() => {}, 1000)',
  ].join(';')], { stdio: ['ignore', 'pipe', 'inherit'] });
  await new Promise((resolve, reject) => {
    holder.once('error', reject);
    holder.stdout.once('data', resolve);
  });
  t.after(() => { if (!holder.killed) holder.kill('SIGKILL'); });
  const install = manager(root).install(
    signedManifest({ url: source.url('/artifact'), ...fixture }),
    { healthCheck: healthy },
  );
  await new Promise((resolve) => setTimeout(resolve, 100));
  assert.equal(source.requests(), 0);
  holder.kill('SIGKILL');
  await new Promise((resolve) => holder.once('exit', resolve));
  const installed = await install;
  assert.equal(installed.version, '1.0.0');
});

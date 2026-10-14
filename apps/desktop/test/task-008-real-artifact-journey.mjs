import assert from 'node:assert/strict';
import { cp, mkdir, mkdtemp, readFile, writeFile } from 'node:fs/promises';
import { execFileSync } from 'node:child_process';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

import { RuntimeManager } from '../dist/runtime-manager.cjs';
import { forwardGoCatalog, startGoCatalog } from './go-catalog-fixture.mjs';
import { ManagedMcpCapabilitySource } from '../../../packages/yuanpu-runtime/dist/index.mjs';

const fixtureRoot = resolve(process.argv[2] || '.');
const fixture = JSON.parse(await readFile(join(fixtureRoot, 'metadata/fixture.json'), 'utf8'));
assert.equal(fixture.target, `${process.platform}-${process.arch}`);
const repo = resolve(import.meta.dirname, '../../..');
const root = process.argv[3]
  ? resolve(process.argv[3]) : await mkdtemp(join(tmpdir(), 'yuanpu-task008-journey-'));
if (process.argv[3]) await mkdir(root);
const home = join(root, 'home');
const resources = join(root, 'resources');
const workspace = join(root, 'workspace');
const name = 'builtin.python.echo';
const originalEnv = { ...process.env };
const errors = [];
const options = { artifactRoot: join(fixtureRoot, '0.1.0') };
let catalog;
let fault;
const server = createServer(async (request, response) => {
  if (fault === 'signature' && request.url?.endsWith('/manifest')) {
    const manifest = JSON.parse(await readFile(join(options.artifactRoot, 'manifest.json'), 'utf8'));
    manifest.signature.value = Buffer.alloc(64).toString('base64');
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(JSON.stringify(manifest));
  } else if (fault === 'archive' && request.url?.includes('/artifacts/')) {
    response.writeHead(200, { 'content-type': 'application/gzip' });
    response.end('interrupted synthetic download');
  } else {
    await forwardGoCatalog(catalog.origin, request, response);
  }
});
let manager;
let serverOpen = false;
const cases = [];

async function active(expected) {
  const state = JSON.parse(await readFile(join(home, 'packages/artifact-state.json'), 'utf8'));
  const entry = state.packages[name];
  assert.equal(entry.activeVersion, expected);
  const listed = (await manager.listPlugins()).find((item) => item.name === name);
  assert.equal(listed.version, expected);
  return entry.versions[expected];
}

async function openInstalled(expectedVersion) {
  const installed = await active(expectedVersion);
  return new ManagedMcpCapabilitySource({
    sourceInstanceId: name, packageVersion: expectedVersion,
    command: installed.entrypoint, args: [], cwd: installed.installPath,
    privateHome: join(root, 'mcp-private'), initializationTimeoutMs: 20_000,
    env: {
      PATH: process.platform === 'win32' ? join(process.env.SYSTEMROOT, 'System32') : join(root, 'empty-path'),
      ...(process.env.SYSTEMROOT ? { SYSTEMROOT: process.env.SYSTEMROOT } : {}),
      YUANPU_CAPABILITY_CONFIG_FILE: join(home, 'packages/config', name, 'user.json'),
    },
  });
}

async function callInstalled(expectedVersion) {
  const source = await openInstalled(expectedVersion);
  try {
    const tools = await source.list({});
    assert.ok(tools.some((tool) => tool.name === 'yuanpu_echo_text'));
    const result = await source.execute({
      capabilityId: 'yuanpu_echo_text', originalName: 'yuanpu_echo_text',
      arguments: { text: 'installed' },
    }, {});
    assert.deepEqual(result.structuredContent, { text: 'TASK008: installed', length: 18 });
  } finally {
    await source.close();
  }
}

try {
  catalog = await startGoCatalog(options.artifactRoot);
  await mkdir(join(home, 'app'), { recursive: true });
  await mkdir(workspace, { recursive: true });
  await mkdir(join(root, 'empty-path'), { recursive: true });
  await cp(join(fixtureRoot, '0.1.0/bundle'), join(resources, 'capabilities', name), { recursive: true });
  await writeFile(join(home, 'app/config.json'), JSON.stringify({
    schemaVersion: 1, provider: 'task008-unused', model: 'unused',
    apiKeyEnv: 'TASK008_UNUSED_KEY', workingDirectory: workspace,
    baseUrl: 'http://127.0.0.1:9/v1', api: 'openai-completions',
  }));
  await new Promise((done) => server.listen(0, '127.0.0.1', done));
  serverOpen = true;
  process.env.YUANPU_HOME = home;
  process.env.YUANPU_CATALOG_URL = `http://127.0.0.1:${server.address().port}`;
  process.env.PATH = process.platform === 'win32'
    ? join(process.env.SYSTEMROOT, 'System32') : join(root, 'empty-path');
  const executable = join(repo, 'apps/runtime/dist-native/bin',
    `YuanpuAgentRuntime-${fixture.target}${process.platform === 'win32' ? '.exe' : ''}`);
  manager = new RuntimeManager(join(resources, 'app'), resources, join(root, 'user-data'), true, '0.1.0', {
    command: { executable, args: [] }, startupTimeoutMs: 30_000,
    restartLimit: 0, onError: (error) => errors.push(error.message),
  });
  await manager.start();
  let item = (await manager.searchPlugins('Python')).find((candidate) => candidate.id === name);
  assert.ok(item?.artifactManifestDigest);
  assert.equal((await manager.installPlugin(item.source, item.artifactManifestDigest)).version, '0.1.0');
  await active('0.1.0');
  await manager.savePluginConfig({ name, scope: 'user', value: { responsePrefix: 'TASK008: ' } });
  await callInstalled('0.1.0');
  cases.push('signed-install-configure-call-0.1.0');

  options.artifactRoot = join(fixtureRoot, '0.2.0');
  await catalog.close();
  catalog = await startGoCatalog(options.artifactRoot);
  fault = 'signature';
  await assert.rejects(manager.installPlugin(item.source, item.artifactManifestDigest), /signature|签名/i);
  await active('0.1.0');
  cases.push('bad-signature-preserves-0.1.0');
  fault = undefined;
  item = (await manager.searchPlugins('Python')).find((candidate) => candidate.id === name);
  assert.equal(item.version, '0.2.0');
  fault = 'archive';
  await assert.rejects(manager.installPlugin(item.source, item.artifactManifestDigest), /size|hash|checksum|大小|哈希/i);
  await callInstalled('0.1.0');
  cases.push('bad-download-preserves-working-0.1.0');
  fault = undefined;
  const runningOld = await openInstalled('0.1.0');
  try {
    // Keep the actual old executable loaded while installing the new immutable
    // version. On Windows this exercises a real executable image/file lock.
    await runningOld.list({});
    assert.equal((await manager.installPlugin(item.source, item.artifactManifestDigest)).version, '0.2.0');
    const oldResult = await runningOld.execute({
      capabilityId: 'yuanpu_echo_text', originalName: 'yuanpu_echo_text',
      arguments: { text: 'still-running' },
    }, {});
    assert.equal(oldResult.structuredContent.text, 'TASK008: still-running');
    cases.push('update-with-old-frozen-executable-running');
  } finally {
    await runningOld.close();
  }
  await callInstalled('0.2.0');
  assert.equal((await manager.getPluginConfig(name, 'user')).value.responsePrefix, 'TASK008: ');
  cases.push('real-update-to-0.2.0-retains-config');
  assert.equal((await manager.rollbackPlugin(name, '0.1.0')).version, '0.1.0');
  await callInstalled('0.1.0');
  cases.push('explicit-rollback-to-0.1.0');

  await manager.stop();
  await new Promise((done) => server.close(done));
  serverOpen = false;
  await manager.start();
  await callInstalled('0.1.0');
  assert.equal((await manager.getPluginConfig(name, 'user')).value.responsePrefix, 'TASK008: ');
  await manager.stop();
  cases.push('restart-and-call-with-catalog-stopped');
  assert.deepEqual(errors, []);
  const result = {
    status: 'passed', target: fixture.target, fixtureSourceCommit: fixture.sourceCommit,
    testedCommit: execFileSync('git', ['rev-parse', 'HEAD'], {
      cwd: repo, env: originalEnv, encoding: 'utf8',
    }).trim(),
    realSea: true, realFrozenPython: true, noPythonOnChildPath: true, cases,
    limits: ['Host API verification; no desktop UI or model approval', 'Catalog stopped; host network not isolated', 'Hosted build machine may have Python outside child PATH'],
  };
  await writeFile(join(root, 'result.json'), JSON.stringify(result, null, 2) + '\n');
  console.log(JSON.stringify({ ...result, evidenceRoot: root }));
} finally {
  await manager?.stop();
  if (serverOpen) await new Promise((done) => server.close(done));
  await catalog?.close();
  for (const key of ['YUANPU_HOME', 'YUANPU_CATALOG_URL', 'PATH']) {
    if (originalEnv[key] === undefined) delete process.env[key];
    else process.env[key] = originalEnv[key];
  }
}

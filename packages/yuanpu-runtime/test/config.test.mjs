import assert from 'node:assert/strict';
import { access, mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { ensureYuanpuHome } from '../dist/index.mjs';

test('Yuanpu home separates application, Agent, packages, and workflow data', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-home-test-'));
  context.after(() => rm(root, { recursive: true, force: true }));

  const home = await ensureYuanpuHome(root);
  const config = JSON.parse(await readFile(home.configPath, 'utf8'));
  await Promise.all([
    access(home.skillsPath),
    access(join(home.memoryPath, 'MEMORY.md')),
    access(home.packagesPath),
    access(home.sessionsPath),
    access(home.workflowsPath),
  ]);

  assert.equal(config.schemaVersion, 1);
  assert.equal(config.apiKeyEnv, undefined);
  assert.equal(home.root, root);
  assert.equal(home.configPath, join(root, 'app', 'config.json'));
  assert.equal(home.agentPath, join(root, 'agent'));
});

test('legacy model files move to app and generated model file is retired', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-model-migration-'));
  context.after(() => rm(root, { recursive: true, force: true }));
  await mkdir(join(root, 'app'), { recursive: true });
  await mkdir(join(root, 'agent'), { recursive: true });
  await writeFile(join(root, 'app', 'config.json'), JSON.stringify({
    schemaVersion: 1, provider: 'custom', model: 'current', apiKeyEnv: 'CUSTOM_KEY',
    baseUrl: 'https://custom.example/v1', api: 'openai-completions', workingDirectory: root,
  }));
  await writeFile(join(root, 'agent', 'auth.json'), JSON.stringify({ custom: { type: 'api_key', key: 'fixture-key' } }));
  await writeFile(join(root, 'agent', 'yuanpu-models.json'), JSON.stringify({ providers: { custom: {
    baseUrl: 'https://custom.example/v1', api: 'openai-completions',
    models: [{ id: 'current' }],
  } } }));
  const home = await ensureYuanpuHome(root);
  const config = JSON.parse(await readFile(home.configPath, 'utf8'));
  const models = JSON.parse(await readFile(join(home.appPath, 'models.json'), 'utf8'));
  const auth = JSON.parse(await readFile(join(home.appPath, 'auth.json'), 'utf8'));
  assert.deepEqual({ provider: config.provider, model: config.model }, { provider: 'custom', model: 'current' });
  assert.equal(config.baseUrl, undefined);
  assert.equal(config.apiKeyEnv, undefined);
  assert.equal(models.providers.custom.baseUrl, 'https://custom.example/v1');
  assert.equal(auth.custom.key, 'fixture-key');
  await assert.rejects(access(join(home.agentPath, 'auth.json')), { code: 'ENOENT' });
  await assert.rejects(access(join(home.agentPath, 'yuanpu-models.json')), { code: 'ENOENT' });
});

test('a conflicting custom endpoint leaves both configurations intact', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-model-conflict-'));
  context.after(() => rm(root, { recursive: true, force: true }));
  await mkdir(join(root, 'app'), { recursive: true });
  const configPath = join(root, 'app', 'config.json');
  const modelsPath = join(root, 'app', 'models.json');
  await writeFile(configPath, JSON.stringify({
    schemaVersion: 1, provider: 'custom', model: 'current', workingDirectory: root,
    baseUrl: 'https://old.example/v1', api: 'openai-completions',
  }));
  await writeFile(modelsPath, JSON.stringify({ providers: { custom: {
    baseUrl: 'https://new.example/v1', api: 'openai-completions', models: [{ id: 'current' }],
  } } }));
  await assert.rejects(ensureYuanpuHome(root), /differs between legacy config/);
  assert.equal(JSON.parse(await readFile(configPath, 'utf8')).baseUrl, 'https://old.example/v1');
  assert.equal(JSON.parse(await readFile(modelsPath, 'utf8')).providers.custom.baseUrl, 'https://new.example/v1');
});

test('credential migration refuses to overwrite a different saved key', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-auth-conflict-'));
  context.after(() => rm(root, { recursive: true, force: true }));
  await mkdir(join(root, 'app'), { recursive: true });
  await mkdir(join(root, 'agent'), { recursive: true });
  await writeFile(join(root, 'app', 'auth.json'), JSON.stringify({ custom: { type: 'api_key', key: 'new-key' } }));
  await writeFile(join(root, 'agent', 'auth.json'), JSON.stringify({ custom: { type: 'api_key', key: 'old-key' } }));
  await assert.rejects(ensureYuanpuHome(root), /Credential custom differs/);
  assert.equal(JSON.parse(await readFile(join(root, 'app', 'auth.json'), 'utf8')).custom.key, 'new-key');
  assert.equal(JSON.parse(await readFile(join(root, 'agent', 'auth.json'), 'utf8')).custom.key, 'old-key');
});

test('Yuanpu home migrates the legacy flat layout without losing package paths', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-home-migration-'));
  context.after(() => rm(root, { recursive: true, force: true }));
  const oldInstallPath = join(root, 'plugins', 'installed', 'example', 'node_modules', 'example');
  await mkdir(oldInstallPath, { recursive: true });
  await mkdir(join(root, 'skills', 'example'), { recursive: true });
  await writeFile(join(root, 'config.json'), JSON.stringify({
    schemaVersion: 1,
    provider: 'openai',
    model: 'gpt-5.6-luna',
    apiKeyEnv: 'OPENAI_API_KEY',
    workingDirectory: root,
  }));
  await writeFile(join(root, 'settings.json'), JSON.stringify({ packages: [oldInstallPath] }));
  await writeFile(join(root, 'models-store.json'), '{}');
  await writeFile(join(root, 'plugins', 'state.json'), JSON.stringify({
    schemaVersion: 1,
    plugins: { example: { name: 'example', installPath: oldInstallPath } },
  }));

  const home = await ensureYuanpuHome(root);
  const state = JSON.parse(await readFile(join(home.packagesPath, 'state.json'), 'utf8'));
  const settings = JSON.parse(await readFile(join(home.agentPath, 'settings.json'), 'utf8'));
  const newInstallPath = join(root, 'packages', 'installed', 'example', 'node_modules', 'example');
  assert.equal(state.plugins.example.installPath, newInstallPath);
  assert.deepEqual(settings.packages, [newInstallPath]);
  await access(join(home.skillsPath, 'example'));
  await access(join(home.agentPath, 'models-store.json'));
});

test('notification preference is read from app config and rejects non-boolean values', async (context) => {
  const root = await mkdtemp(join(tmpdir(), 'yuanpu-notification-config-'));
  context.after(() => rm(root, { recursive: true, force: true }));
  await mkdir(join(root, 'app'), { recursive: true });
  const configPath = join(root, 'app', 'config.json');
  const base = {
    schemaVersion: 1,
    provider: 'openai',
    model: 'gpt-5.6-luna',
    apiKeyEnv: 'OPENAI_API_KEY',
    workingDirectory: root,
  };
  await writeFile(configPath, JSON.stringify({ ...base, notifications: { enabled: false } }));
  assert.equal((await ensureYuanpuHome(root)).config.notifications.enabled, false);

  await writeFile(configPath, JSON.stringify({ ...base, notifications: { enabled: 'no' } }));
  await assert.rejects(ensureYuanpuHome(root), /Invalid 南医小助 config/);
  await writeFile(configPath, JSON.stringify({ ...base, notifications: null }));
  await assert.rejects(ensureYuanpuHome(root), /Invalid 南医小助 config/);
});

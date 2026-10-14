import assert from 'node:assert/strict';
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { PluginManager } from '../dist/index.mjs';

async function temporaryHome() {
  return mkdtemp(join(tmpdir(), 'yuanpu-packages-'));
}

test('searches the configured npm registry for pi packages', async (t) => {
  const server = createServer((request, response) => {
    const url = new URL(request.url, 'http://127.0.0.1');
    assert.equal(url.pathname, '/-/v1/search');
    assert.match(url.searchParams.get('text'), /keywords:pi-package memory/);
    response.setHeader('content-type', 'application/json');
    response.end(JSON.stringify({
      objects: [{
        package: {
          name: 'pi-memory',
          version: '1.2.3',
          description: 'Memory extension',
          publisher: { username: 'tester' },
        },
      }],
    }));
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => server.close());
  const address = server.address();
  assert.ok(address && typeof address !== 'string');
  const root = await temporaryHome();
  t.after(() => rm(root, { recursive: true, force: true }));

  const manager = new PluginManager(join(root, 'plugins'), root, `http://127.0.0.1:${address.port}`);
  const results = await manager.search('memory');
  assert.deepEqual(results, [{
    name: 'pi-memory',
    version: '1.2.3',
    description: 'Memory extension',
    publisher: 'tester',
    source: 'npm:pi-memory@1.2.3',
  }]);
});

test('searches a Yuanpu skill catalog with product metadata', async (t) => {
  const server = createServer((request, response) => {
    const url = new URL(request.url, 'http://127.0.0.1');
    assert.equal(url.pathname, '/v1/catalog/search');
    assert.equal(url.searchParams.get('q'), 'workflow');
    response.setHeader('content-type', 'application/json');
    response.end(JSON.stringify({
      schemaVersion: 1,
      items: [{
        id: 'works.workflow',
        name: '@works/workflow',
        displayName: '工作流编排',
        version: '1.2.3',
        description: 'Workflow skill',
        source: 'npm:@works/workflow@1.2.3',
        components: ['agent', 'workflow'],
        permissions: ['background'],
      }],
    }));
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => server.close());
  const address = server.address();
  assert.ok(address && typeof address !== 'string');
  const root = await temporaryHome();
  t.after(() => rm(root, { recursive: true, force: true }));

  const manager = new PluginManager(
    join(root, 'packages'),
    join(root, 'agent'),
    undefined,
    root,
    `http://127.0.0.1:${address.port}`,
  );
  const [result] = await manager.search('workflow');
  assert.equal(result.displayName, '工作流编排');
  assert.deepEqual(result.components, ['agent', 'workflow']);
});

test('enables, disables and uninstalls only managed plugin paths', async (t) => {
  const root = await temporaryHome();
  t.after(() => rm(root, { recursive: true, force: true }));
  const pluginsRoot = join(root, 'plugins');
  const container = join(pluginsRoot, 'installed', 'scope-plugin-1.0.0-deadbeef');
  const installPath = join(container, 'node_modules', '@scope', 'plugin');
  await mkdir(installPath, { recursive: true });
  await writeFile(join(installPath, 'package.json'), '{}\n');
  await writeFile(join(root, 'settings.json'), JSON.stringify({ packages: ['./keep-me'] }));
  await mkdir(pluginsRoot, { recursive: true });
  await writeFile(join(pluginsRoot, 'state.json'), JSON.stringify({
    schemaVersion: 1,
    plugins: {
      '@scope/plugin': {
        name: '@scope/plugin',
        version: '1.0.0',
        description: 'Test plugin',
        source: 'npm:@scope/plugin@1.0.0',
        installPath,
        enabled: true,
        installedAt: '2026-09-21T00:00:00.000Z',
      },
    },
  }));

  const manager = new PluginManager(pluginsRoot, root);
  assert.equal((await manager.list()).length, 1);
  await manager.setEnabled('@scope/plugin', false);
  let settings = JSON.parse(await readFile(join(root, 'settings.json'), 'utf8'));
  assert.deepEqual(settings.packages, ['./keep-me']);

  await manager.setEnabled('@scope/plugin', true);
  settings = JSON.parse(await readFile(join(root, 'settings.json'), 'utf8'));
  assert.deepEqual(settings.packages, ['./keep-me', installPath]);

  await manager.markLoadError('@scope/plugin', 'broken extension');
  assert.equal((await manager.list())[0].loadError, 'broken extension');
  settings = JSON.parse(await readFile(join(root, 'settings.json'), 'utf8'));
  assert.deepEqual(settings.packages, ['./keep-me']);

  await manager.setEnabled('@scope/plugin', true);
  assert.equal((await manager.list())[0].loadError, undefined);

  await manager.uninstall('@scope/plugin');
  await assert.rejects(readFile(container), /ENOENT|EISDIR/);
  assert.deepEqual(await manager.list(), []);
});

test('rejects floating npm versions and unpinned git sources', async () => {
  const root = await temporaryHome();
  const manager = new PluginManager(join(root, 'plugins'), root);
  await assert.rejects(manager.install('npm:pi-example@latest'), /精确版本/);
  await assert.rejects(manager.install('https://github.com/example/plugin'), /commit SHA/);
  await rm(root, { recursive: true, force: true });
});

test('retires pi-mcp-adapter and configures built-in MCP without writing plaintext secrets', async (t) => {
  const root = await temporaryHome();
  t.after(() => rm(root, { recursive: true, force: true }));
  const pluginsRoot = join(root, 'plugins');
  const workspace = join(root, 'workspace');
  const installPath = join(pluginsRoot, 'installed', 'mcp-adapter', 'node_modules', 'pi-mcp-adapter');
  await mkdir(installPath, { recursive: true });
  await mkdir(workspace, { recursive: true });
  await writeFile(join(installPath, 'package.json'), JSON.stringify({ name: 'pi-mcp-adapter', version: '2.34.0' }));
  await writeFile(join(pluginsRoot, 'state.json'), JSON.stringify({
    schemaVersion: 1,
    plugins: {
      'pi-mcp-adapter': {
        name: 'pi-mcp-adapter',
        version: '2.34.0',
        description: 'MCP adapter',
        source: 'npm:pi-mcp-adapter@2.34.0',
        installPath,
        enabled: true,
        installedAt: '2026-09-21T00:00:00.000Z',
      },
    },
  }));

  const manager = new PluginManager(pluginsRoot, root, undefined, workspace);
  assert.deepEqual(await manager.list(), []);
  await assert.rejects(readFile(join(installPath, 'package.json')), { code: 'ENOENT' });
  await assert.rejects(manager.install('npm:pi-mcp-adapter@2.34.0'), /内置/);

  const initial = await manager.getConfig('pi-mcp', 'user');
  assert.equal(initial.path, join(root, 'mcp.json'));
  assert.deepEqual(initial.value, { mcpServers: {} });

  const plaintext = await manager.validateConfig({
    name: 'pi-mcp',
    scope: 'user',
    value: { mcpServers: { private: { url: 'https://example.test', token: 'plain-secret' } } },
  });
  assert.equal(plaintext.valid, false);
  assert.match(plaintext.errors.join('\n'), /环境变量引用/);

  const value = {
    mcpServers: {
      docs: { url: 'https://example.test/mcp', headers: { authorization: '${DOCS_TOKEN}' } },
    },
  };
  await manager.saveConfig({ name: 'pi-mcp', scope: 'user', value });
  assert.deepEqual(JSON.parse(await readFile(join(root, 'mcp.json'), 'utf8')), value);

  await assert.rejects(
    manager.saveConfig({ name: 'pi-mcp', scope: 'user', value: { invalid: true } }),
    /mcpServers/,
  );
  assert.deepEqual(JSON.parse(await readFile(join(root, 'mcp.json'), 'utf8')), value);

  await manager.saveConfig({ name: 'pi-mcp', scope: 'workspace', value: { mcpServers: {} } });
  assert.deepEqual(
    JSON.parse(await readFile(join(workspace, '.pi', 'mcp.json'), 'utf8')),
    { mcpServers: {} },
  );
});

test('retiring a disabled adapter preserves disabled connections and their original config', async (t) => {
  const root = await temporaryHome();
  t.after(() => rm(root, { recursive: true, force: true }));
  const plugins = join(root, 'packages');
  const installPath = join(plugins, 'installed', 'old-adapter', 'node_modules', 'pi-mcp-adapter');
  await mkdir(installPath, { recursive: true });
  const original = JSON.stringify({ mcpServers: { private: { command: 'do-not-run', env: { KEY: '${TEST_KEY}' } } } });
  await writeFile(join(root, 'mcp.json'), original);
  await writeFile(join(root, 'settings.json'), JSON.stringify({ packages: [installPath,
    'npm:pi-mcp-adapter@2.34.0', 'npm:other-package@1.0.0'] }));
  await writeFile(join(plugins, 'state.json'), JSON.stringify({ schemaVersion: 1, plugins: {
    'pi-mcp-adapter': { name: 'pi-mcp-adapter', version: '2.34.0', installPath, enabled: false },
  } }));
  const manager = new PluginManager(plugins, root);
  await manager.initialize();
  const config = await manager.getConfig('pi-mcp', 'user');
  assert.equal(config.value.mcpServers.private.enabled, false);
  assert.equal(await readFile(join(root, 'mcp.json.pre-pi-1.bak'), 'utf8'), original);
  assert.deepEqual(JSON.parse(await readFile(join(root, 'settings.json'), 'utf8')).packages, ['npm:other-package@1.0.0']);
  await manager.saveConfig({ name: 'pi-mcp', scope: 'user', value: { mcpServers: {} } });
  await manager.initialize();
  assert.deepEqual((await manager.getConfig('pi-mcp', 'user')).value, { mcpServers: {} });
});

test('loads a Yuanpu config schema without storing config in the install directory', async (t) => {
  const root = await temporaryHome();
  t.after(() => rm(root, { recursive: true, force: true }));
  const pluginsRoot = join(root, 'plugins');
  const installPath = join(pluginsRoot, 'installed', 'schema-plugin', 'node_modules', 'schema-plugin');
  await mkdir(installPath, { recursive: true });
  await writeFile(join(installPath, 'config.schema.json'), JSON.stringify({
    type: 'object',
    properties: { endpoint: { type: 'string' } },
    required: ['endpoint'],
    additionalProperties: false,
  }));
  await writeFile(join(installPath, 'package.json'), JSON.stringify({
    name: 'schema-plugin',
    version: '1.0.0',
    yuanpu: {
      config: {
        schema: './config.schema.json',
        required: true,
        scope: ['user'],
      },
    },
  }));
  await writeFile(join(pluginsRoot, 'state.json'), JSON.stringify({
    schemaVersion: 1,
    plugins: {
      'schema-plugin': {
        name: 'schema-plugin',
        version: '1.0.0',
        description: 'Schema plugin',
        source: 'npm:schema-plugin@1.0.0',
        installPath,
        enabled: true,
        installedAt: '2026-09-21T00:00:00.000Z',
      },
    },
  }));

  const manager = new PluginManager(pluginsRoot, root);
  assert.equal((await manager.list())[0].configStatus, 'invalid');
  const saved = await manager.saveConfig({
    name: 'schema-plugin',
    scope: 'user',
    value: { endpoint: 'https://example.test' },
  });
  assert.equal(saved.kind, 'schema');
  assert.equal(saved.path.startsWith(join(pluginsRoot, 'config')), true);
  assert.equal(saved.path.startsWith(installPath), false);
});

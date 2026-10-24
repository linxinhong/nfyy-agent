import Arborist from '@npmcli/arborist';
import Ajv, { type ErrorObject, type ValidateFunction } from 'ajv';
// npm-package-arg does not currently publish declarations; the narrow shape used here is validated below.
// @ts-expect-error missing upstream declarations
import npa from 'npm-package-arg';
import { createHash, randomUUID } from 'node:crypto';
import { constants } from 'node:fs';
import {
  access,
  mkdir,
  readFile,
  rename,
  rm,
  writeFile,
} from 'node:fs/promises';
import { dirname, isAbsolute, join, relative, resolve, sep } from 'node:path';

export * from './artifacts.js';

const STATE_SCHEMA_VERSION = 1;
const DEFAULT_REGISTRY = 'https://registry.npmjs.org';
const LIFECYCLE_SCRIPTS = ['preinstall', 'install', 'postinstall'] as const;

export interface PluginSearchResult {
  id?: string;
  name: string;
  displayName?: string;
  version: string;
  description: string;
  publisher?: string;
  updatedAt?: string;
  npmUrl?: string;
  source: string;
  components?: Array<'skill' | 'agent' | 'workflow' | 'extension' | 'prompt' | 'theme' | 'connector'>;
  permissions?: Array<'instructions' | 'scripts' | 'filesystem' | 'network' | 'credentials' | 'notifications' | 'background'>;
}

export interface InstalledPlugin {
  name: string;
  version: string;
  description: string;
  source: string;
  installPath: string;
  enabled: boolean;
  installedAt: string;
  loadError?: string;
  configurable?: boolean;
  configStatus?: 'unsupported' | 'optional' | 'required' | 'valid' | 'invalid';
}

interface PluginState {
  schemaVersion: 1;
  plugins: Record<string, InstalledPlugin>;
}

const builtinMcpPlugin: InstalledPlugin = {
  name: 'pi-mcp', version: '1.4.0', description: 'Pi 内置 MCP 服务连接',
  source: 'builtin:pi-mcp', installPath: '', enabled: true, installedAt: '',
};

interface PackageJson {
  name?: string;
  version?: string;
  description?: string;
  scripts?: Record<string, string>;
  dependencies?: Record<string, string>;
  pi?: {
    extensions?: string[];
    skills?: string[];
    prompts?: string[];
    themes?: string[];
  };
  yuanpu?: {
    config?: {
      schema?: string;
      schemaVersion?: number;
      title?: string;
      description?: string;
      required?: boolean;
      scope?: Array<'user' | 'workspace'>;
      reload?: 'session';
    };
  };
}

export type PluginConfigScope = 'user' | 'workspace';

export interface PluginConfigDocument {
  pluginName: string;
  kind: 'mcp' | 'schema';
  title: string;
  description: string;
  scope: PluginConfigScope;
  path: string;
  value: Record<string, unknown>;
  schema?: Record<string, unknown>;
  supportsWorkspace: boolean;
  secretPolicy: 'environment-only';
}

export interface PluginConfigInput {
  name: string;
  scope: PluginConfigScope;
  value: Record<string, unknown>;
}

export interface PluginConfigValidation {
  valid: boolean;
  errors: string[];
}

interface ConfigAdapter {
  kind: PluginConfigDocument['kind'];
  title: string;
  description: string;
  required: boolean;
  supportsWorkspace: boolean;
  path: (scope: PluginConfigScope) => string;
  schema?: Record<string, unknown>;
  validate: ValidateFunction<Record<string, unknown>>;
  defaultValue: Record<string, unknown>;
}

function initialState(): PluginState {
  return { schemaVersion: STATE_SCHEMA_VERSION, plugins: {} };
}

async function pathExists(path: string): Promise<boolean> {
  try {
    await access(path, constants.F_OK);
    return true;
  } catch {
    return false;
  }
}

async function readJson<T>(path: string): Promise<T> {
  return JSON.parse(await readFile(path, 'utf8')) as T;
}

async function writeJsonAtomic(path: string, value: unknown): Promise<void> {
  const temporaryPath = `${path}.${randomUUID()}.tmp`;
  await mkdir(dirname(path), { recursive: true });
  await writeFile(temporaryPath, `${JSON.stringify(value, null, 2)}\n`, { mode: 0o600 });
  await rename(temporaryPath, path);
}

function isInside(parent: string, child: string): boolean {
  const path = relative(resolve(parent), resolve(child));
  return !isAbsolute(path) && path !== '..' && !path.startsWith(`..${sep}`) && !path.includes(`${sep}..${sep}`);
}

function pluginDirectoryName(name: string, version: string, source: string): string {
  const readable = name.replace(/^@/, '').replace(/[^a-zA-Z0-9._-]+/g, '-').slice(0, 64);
  const digest = createHash('sha256').update(`${name}\0${version}\0${source}`).digest('hex').slice(0, 12);
  return `${readable}-${version.replace(/[^a-zA-Z0-9._-]+/g, '-')}-${digest}`;
}

function configDirectoryName(name: string): string {
  const readable = name.replace(/^@/, '').replace(/[^a-zA-Z0-9._-]+/g, '-').slice(0, 64);
  return `${readable}-${createHash('sha256').update(name).digest('hex').slice(0, 10)}`;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return Boolean(value) && typeof value === 'object' && !Array.isArray(value);
}

function secretReferenceErrors(value: unknown, path = ''): string[] {
  if (Array.isArray(value)) {
    return value.flatMap((entry, index) => secretReferenceErrors(entry, `${path}/${index}`));
  }
  if (!isRecord(value)) return [];
  return Object.entries(value).flatMap(([key, entry]) => {
    const entryPath = `${path}/${key}`;
    const nested = secretReferenceErrors(entry, entryPath);
    if (
      typeof entry === 'string'
      && /^(?:api[-_]?key|token|secret|password|authorization)$/i.test(key)
      && entry.length > 0
      && !/^\$\{[A-Z_][A-Z0-9_]*\}$/.test(entry)
      && !/^\$env:[A-Z_][A-Z0-9_]*$/.test(entry)
    ) {
      return [`${entryPath} 必须使用环境变量引用，例如 \${API_TOKEN}。`, ...nested];
    }
    return nested;
  });
}

function formatValidationErrors(errors: ErrorObject[] | null | undefined): string[] {
  return (errors ?? []).map((error) => `${error.instancePath || '/'} ${error.message ?? '配置无效'}`);
}

function assertInstallSource(source: string): string {
  const normalized = source.startsWith('npm:') ? source.slice(4) : source;
  if (/^(?:git\+)?https:\/\/github\.com\//.test(normalized)) {
    const fragment = normalized.split('#')[1];
    if (!fragment || !/^[0-9a-f]{40}$/i.test(fragment)) {
      throw new Error('Git 插件必须固定到完整的 40 位 commit SHA。');
    }
    return normalized;
  }

  const parsed = npa(normalized);
  if (parsed.type !== 'version' || !parsed.name) {
    throw new Error('npm 插件必须使用精确版本，例如 npm:package-name@1.2.3。');
  }
  return `${parsed.name}@${parsed.rawSpec}`;
}

function hasPiResources(packageJson: PackageJson, packageRoot: string): Promise<boolean> {
  const manifest = packageJson.pi;
  if (manifest && ['extensions', 'skills', 'prompts', 'themes'].some((field) => {
    const entries = manifest[field as keyof NonNullable<PackageJson['pi']>];
    return Array.isArray(entries) && entries.length > 0;
  })) return Promise.resolve(true);

  return Promise.all(['extensions', 'skills', 'prompts', 'themes']
    .map((directory) => pathExists(join(packageRoot, directory))))
    .then((results) => results.some(Boolean));
}

async function findRootPlugin(transactionRoot: string): Promise<{ path: string; package: PackageJson }> {
  const containerPackage = await readJson<PackageJson>(join(transactionRoot, 'package.json'));
  const direct = Object.keys(containerPackage.dependencies ?? {});
  if (direct.length !== 1) {
    throw new Error(`插件安装结果应包含一个直接依赖，实际为 ${direct.length} 个。`);
  }
  const path = join(transactionRoot, 'node_modules', direct[0]!);
  return { path, package: await readJson<PackageJson>(join(path, 'package.json')) };
}

export class PluginManager {
  private queue: Promise<void> = Promise.resolve();
  private readonly statePath: string;
  private readonly installedRoot: string;
  private readonly stagingRoot: string;
  private readonly configRoot: string;
  private readonly ajv = new Ajv({ allErrors: true, strict: false });

  constructor(
    private readonly pluginsRoot: string,
    private readonly agentRoot: string,
    private readonly registryUrl = process.env.YUANPU_PLUGIN_REGISTRY_URL || DEFAULT_REGISTRY,
    private readonly workingDirectory = process.cwd(),
    private readonly catalogUrl = process.env.YUANPU_CATALOG_URL,
  ) {
    this.statePath = join(pluginsRoot, 'state.json');
    this.installedRoot = join(pluginsRoot, 'installed');
    this.stagingRoot = join(pluginsRoot, '.staging');
    this.configRoot = join(pluginsRoot, 'config');
  }

  private async exclusive<T>(operation: () => Promise<T>): Promise<T> {
    const previous = this.queue;
    let release!: () => void;
    this.queue = new Promise<void>((resolveQueue) => { release = resolveQueue; });
    await previous;
    try {
      return await operation();
    } finally {
      release();
    }
  }

  private async ensure(): Promise<void> {
    await Promise.all([
      mkdir(this.installedRoot, { recursive: true }),
      mkdir(this.stagingRoot, { recursive: true }),
      mkdir(this.configRoot, { recursive: true }),
    ]);
    if (!await pathExists(this.statePath)) await writeJsonAtomic(this.statePath, initialState());
  }

  private async readState(): Promise<PluginState> {
    await this.ensure();
    const state = await readJson<PluginState>(this.statePath);
    if (state.schemaVersion !== STATE_SCHEMA_VERSION || !state.plugins || typeof state.plugins !== 'object') {
      throw new Error(`无效的插件状态文件：${this.statePath}`);
    }
    const legacy = state.plugins['pi-mcp-adapter'];
    if (legacy) {
      if (!legacy.enabled) {
        for (const path of [join(this.agentRoot, 'mcp.json'), join(resolve(this.workingDirectory), '.pi', 'mcp.json')]) {
          if (!await pathExists(path)) continue;
          const config = await readJson<Record<string, unknown>>(path);
          if (isRecord(config.mcpServers)) {
            await writeFile(`${path}.pre-pi-1.bak`, await readFile(path), { flag: 'wx', mode: 0o600 })
              .catch((error: NodeJS.ErrnoException) => { if (error.code !== 'EEXIST') throw error; });
            const mcpServers = Object.fromEntries(Object.entries(config.mcpServers)
              .map(([name, value]) => [name, isRecord(value) ? { ...value, enabled: false } : value]));
            await writeJsonAtomic(path, { ...config, mcpServers });
          }
        }
      }
      delete state.plugins['pi-mcp-adapter'];
      await writeJsonAtomic(this.statePath, state);
      await this.syncPiSettings(state);
      // Configuration lives in agent/mcp.json, outside this managed installation.
      if (isInside(this.installedRoot, legacy.installPath)) {
        await rm(this.containerRoot(legacy.installPath), { recursive: true, force: true });
      }
    }
    return state;
  }

  async initialize(): Promise<void> {
    await this.exclusive(async () => this.syncPiSettings(await this.readState()));
  }

  private async syncPiSettings(state: PluginState): Promise<void> {
    const settingsPath = join(this.agentRoot, 'settings.json');
    let settings: Record<string, unknown> = {};
    try {
      settings = await readJson<Record<string, unknown>>(settingsPath);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error;
    }
    const packages = Array.isArray(settings.packages) ? settings.packages : [];
    const unmanaged = packages.filter((entry) => {
      const source = typeof entry === 'string'
        ? entry
        : entry && typeof entry === 'object' && typeof (entry as { source?: unknown }).source === 'string'
          ? (entry as { source: string }).source
          : undefined;
      if (source && /^npm:pi-mcp-adapter(?:@|$)/.test(source)) return false;
      return !source || !isInside(this.installedRoot, source);
    });
    const enabled = Object.values(state.plugins)
      .filter((plugin) => plugin.enabled)
      .map((plugin) => plugin.installPath)
      .sort();
    await writeJsonAtomic(settingsPath, { ...settings, packages: [...unmanaged, ...enabled] });
  }

  private containerRoot(installPath: string): string {
    const [directory] = relative(this.installedRoot, installPath).split(sep);
    const container = join(this.installedRoot, directory ?? '');
    if (!directory || !isInside(this.installedRoot, container)) {
      throw new Error('拒绝不安全的插件目录。');
    }
    return container;
  }

  private async configAdapter(plugin: InstalledPlugin): Promise<ConfigAdapter | undefined> {
    if (plugin.name === 'pi-mcp') {
      const schema = {
        type: 'object',
        properties: {
          mcpServers: { type: 'object', additionalProperties: { type: 'object' }, default: {} },
        },
        required: ['mcpServers'],
        additionalProperties: true,
      } as const;
      return {
        kind: 'mcp',
        title: 'MCP 服务',
        description: '由 Pi 内置 MCP 连接服务，通过工具搜索按需发现工具。修改后在新会话中生效。',
        required: false,
        supportsWorkspace: true,
        path: (scope) => scope === 'user'
          ? join(this.agentRoot, 'mcp.json')
          : join(resolve(this.workingDirectory), '.pi', 'mcp.json'),
        schema,
        validate: this.ajv.compile<Record<string, unknown>>(schema),
        defaultValue: { mcpServers: {} },
      };
    }

    const packageJson = await readJson<PackageJson>(join(plugin.installPath, 'package.json'));
    const declaration = packageJson.yuanpu?.config;
    if (!declaration?.schema) return undefined;
    const schemaPath = resolve(plugin.installPath, declaration.schema);
    if (!isInside(plugin.installPath, schemaPath)) {
      throw new Error(`${plugin.name} 的配置 Schema 路径超出插件目录。`);
    }
    const schema = await readJson<Record<string, unknown>>(schemaPath);
    const directory = join(this.configRoot, configDirectoryName(plugin.name));
    const workspaceKey = createHash('sha256').update(resolve(this.workingDirectory)).digest('hex');
    return {
      kind: 'schema',
      title: declaration.title ?? plugin.name,
      description: declaration.description ?? '配置该插件的运行参数。',
      required: declaration.required ?? false,
      supportsWorkspace: declaration.scope?.includes('workspace') ?? false,
      path: (scope) => scope === 'workspace'
        ? join(directory, 'workspaces', `${workspaceKey}.json`)
        : join(directory, 'user.json'),
      schema,
      validate: this.ajv.compile<Record<string, unknown>>(schema),
      defaultValue: isRecord(schema.default) ? schema.default : {},
    };
  }

  private async readConfig(adapter: ConfigAdapter, scope: PluginConfigScope): Promise<Record<string, unknown>> {
    try {
      const value = await readJson<unknown>(adapter.path(scope));
      if (!isRecord(value)) throw new Error('插件配置必须是 JSON 对象。');
      return value;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === 'ENOENT') return structuredClone(adapter.defaultValue);
      throw error;
    }
  }

  private validation(adapter: ConfigAdapter, value: Record<string, unknown>): PluginConfigValidation {
    const valid = adapter.validate(value);
    const errors = [
      ...formatValidationErrors(adapter.validate.errors),
      ...secretReferenceErrors(value),
    ];
    return { valid: Boolean(valid) && errors.length === 0, errors };
  }

  private async configDocument(
    plugin: InstalledPlugin,
    adapter: ConfigAdapter,
    scope: PluginConfigScope,
  ): Promise<PluginConfigDocument> {
    if (scope === 'workspace' && !adapter.supportsWorkspace) {
      throw new Error(`${plugin.name} 不支持工作区级配置。`);
    }
    return {
      pluginName: plugin.name,
      kind: adapter.kind,
      title: adapter.title,
      description: adapter.description,
      scope,
      path: adapter.path(scope),
      value: await this.readConfig(adapter, scope),
      ...(adapter.schema ? { schema: adapter.schema } : {}),
      supportsWorkspace: adapter.supportsWorkspace,
      secretPolicy: 'environment-only',
    };
  }

  async search(query = '', limit = 20): Promise<PluginSearchResult[]> {
    if (this.catalogUrl) {
      const url = new URL('/v1/catalog/search', this.catalogUrl);
      url.searchParams.set('q', query.trim());
      const response = await fetch(url, {
        headers: { accept: 'application/json', 'user-agent': 'YuanpuAgent/0.1' },
        signal: AbortSignal.timeout(15_000),
      });
      if (!response.ok) throw new Error(`技能市场搜索失败：HTTP ${response.status}`);
      const body = await response.json() as { items?: PluginSearchResult[] };
      return (body.items ?? []).filter((item) => item.name !== 'pi-mcp-adapter')
        .slice(0, Math.min(Math.max(limit, 1), 20)).map((item) => ({
        ...item,
        source: item.source.startsWith('artifact:')
          ? `artifact:${new URL(item.source.slice('artifact:'.length), response.url).toString()}`
          : item.source,
      }));
    }
    const terms = ['keywords:pi-package', query.trim()].filter(Boolean).join(' ');
    const url = new URL('/-/v1/search', this.registryUrl);
    url.searchParams.set('text', terms);
    url.searchParams.set('size', String(Math.min(Math.max(limit, 1), 20)));
    const response = await fetch(url, {
      headers: { accept: 'application/json', 'user-agent': 'YuanpuAgent/0.1' },
      signal: AbortSignal.timeout(15_000),
    });
    if (!response.ok) throw new Error(`插件搜索失败：HTTP ${response.status}`);
    const body = await response.json() as {
      objects?: Array<{
        package?: {
          name?: string;
          version?: string;
          description?: string;
          publisher?: { username?: string };
          date?: string;
          links?: { npm?: string };
        };
      }>;
    };
    return (body.objects ?? []).flatMap(({ package: pkg }) => {
      if (!pkg?.name || !pkg.version || pkg.name === 'pi-mcp-adapter') return [];
      return [{
        name: pkg.name,
        version: pkg.version,
        description: pkg.description ?? '暂无描述',
        ...(pkg.publisher?.username ? { publisher: pkg.publisher.username } : {}),
        ...(pkg.date ? { updatedAt: pkg.date } : {}),
        ...(pkg.links?.npm ? { npmUrl: pkg.links.npm } : {}),
        source: `npm:${pkg.name}@${pkg.version}`,
      }];
    });
  }

  async list(): Promise<InstalledPlugin[]> {
    return this.exclusive(async () => {
      const state = await this.readState();
      let changed = false;
      for (const [name, plugin] of Object.entries(state.plugins)) {
        if (!isInside(this.installedRoot, plugin.installPath) || !await pathExists(plugin.installPath)) {
          delete state.plugins[name];
          changed = true;
        }
      }
      if (changed) {
        await writeJsonAtomic(this.statePath, state);
        await this.syncPiSettings(state);
      }
      const plugins = await Promise.all(Object.values(state.plugins).map(async (plugin) => {
        const adapter = await this.configAdapter(plugin);
        if (!adapter) return { ...plugin, configurable: false, configStatus: 'unsupported' as const };
        const value = await this.readConfig(adapter, 'user');
        const validation = this.validation(adapter, value);
        const isDefault = JSON.stringify(value) === JSON.stringify(adapter.defaultValue);
        return {
          ...plugin,
          configurable: true,
          configStatus: validation.valid
            ? adapter.required && isDefault ? 'required' as const : 'valid' as const
            : 'invalid' as const,
        };
      }));
      return plugins.sort((a, b) => a.name.localeCompare(b.name));
    });
  }

  async getConfig(name: string, scope: PluginConfigScope): Promise<PluginConfigDocument> {
    return this.exclusive(async () => {
      const plugin = name === 'pi-mcp' ? builtinMcpPlugin : (await this.readState()).plugins[name];
      if (!plugin) throw new Error(`未安装插件：${name}`);
      const adapter = await this.configAdapter(plugin);
      if (!adapter) throw new Error(`${name} 未声明可由 YuanpuAgent 管理的配置。`);
      return this.configDocument(plugin, adapter, scope);
    });
  }

  async validateConfig(input: PluginConfigInput): Promise<PluginConfigValidation> {
    return this.exclusive(async () => {
      const plugin = input.name === 'pi-mcp' ? builtinMcpPlugin : (await this.readState()).plugins[input.name];
      if (!plugin) throw new Error(`未安装插件：${input.name}`);
      const adapter = await this.configAdapter(plugin);
      if (!adapter) throw new Error(`${input.name} 未声明可由 YuanpuAgent 管理的配置。`);
      if (input.scope === 'workspace' && !adapter.supportsWorkspace) {
        return { valid: false, errors: [`${input.name} 不支持工作区级配置。`] };
      }
      return this.validation(adapter, input.value);
    });
  }

  async saveConfig(input: PluginConfigInput): Promise<PluginConfigDocument> {
    return this.exclusive(async () => {
      const plugin = input.name === 'pi-mcp' ? builtinMcpPlugin : (await this.readState()).plugins[input.name];
      if (!plugin) throw new Error(`未安装插件：${input.name}`);
      const adapter = await this.configAdapter(plugin);
      if (!adapter) throw new Error(`${input.name} 未声明可由 YuanpuAgent 管理的配置。`);
      if (input.scope === 'workspace' && !adapter.supportsWorkspace) {
        throw new Error(`${input.name} 不支持工作区级配置。`);
      }
      const validation = this.validation(adapter, input.value);
      if (!validation.valid) throw new Error(`插件配置无效：${validation.errors.join('；')}`);
      await writeJsonAtomic(adapter.path(input.scope), input.value);
      return this.configDocument(plugin, adapter, input.scope);
    });
  }

  async resetConfig(name: string, scope: PluginConfigScope): Promise<PluginConfigDocument> {
    return this.exclusive(async () => {
      const plugin = name === 'pi-mcp' ? builtinMcpPlugin : (await this.readState()).plugins[name];
      if (!plugin) throw new Error(`未安装插件：${name}`);
      const adapter = await this.configAdapter(plugin);
      if (!adapter) throw new Error(`${name} 未声明可由 YuanpuAgent 管理的配置。`);
      if (scope === 'workspace' && !adapter.supportsWorkspace) {
        throw new Error(`${name} 不支持工作区级配置。`);
      }
      await rm(adapter.path(scope), { force: true });
      return this.configDocument(plugin, adapter, scope);
    });
  }

  async install(source: string): Promise<InstalledPlugin> {
    return this.exclusive(async () => {
      if (/^npm:pi-mcp-adapter(?:@|$)/.test(source.trim())) {
        throw new Error('MCP 已由 Pi 内置支持，请直接配置 MCP 服务。');
      }
      const installSource = assertInstallSource(source.trim());
      await this.ensure();
      const transactionRoot = join(this.stagingRoot, randomUUID());
      await mkdir(transactionRoot, { recursive: true });
      await writeJsonAtomic(join(transactionRoot, 'package.json'), {
        name: 'yuanpu-plugin-container',
        version: '0.0.0',
        private: true,
      });

      try {
        const arborist = new Arborist({
          path: transactionRoot,
          registry: this.registryUrl,
          ignoreScripts: true,
          audit: false,
          fund: false,
          legacyPeerDeps: true,
        });
        await arborist.reify({ add: [installSource], save: true });
        const pluginNode = await findRootPlugin(transactionRoot);
        const packageJson = pluginNode.package;
        if (!packageJson.name || !packageJson.version) throw new Error('插件缺少 name 或 version。');
        if (packageJson.name === 'pi-mcp-adapter') throw new Error('MCP 已由 Pi 内置支持，请直接配置 MCP 服务。');
        if (!await hasPiResources(packageJson, pluginNode.path)) {
          throw new Error(`${packageJson.name} 不是可识别的 Pi 插件包。`);
        }
        const lifecycle = LIFECYCLE_SCRIPTS.find((name) => packageJson.scripts?.[name]);
        if (lifecycle) throw new Error(`插件声明了 ${lifecycle} 生命周期脚本，首版暂不允许安装。`);
        const state = await this.readState();
        const previous = state.plugins[packageJson.name];
        const directory = `${pluginDirectoryName(packageJson.name, packageJson.version, installSource)}-${randomUUID().slice(0, 8)}`;
        const finalRoot = join(this.installedRoot, directory);
        if (!isInside(this.installedRoot, finalRoot)) throw new Error('拒绝不安全的插件安装路径。');
        await rename(transactionRoot, finalRoot);
        const relativePackagePath = relative(transactionRoot, pluginNode.path);
        const installPath = join(finalRoot, relativePackagePath);
        const plugin: InstalledPlugin = {
          name: packageJson.name,
          version: packageJson.version,
          description: packageJson.description ?? '暂无描述',
          source,
          installPath,
          enabled: true,
          installedAt: new Date().toISOString(),
        };
        state.plugins[plugin.name] = plugin;
        await writeJsonAtomic(this.statePath, state);
        await this.syncPiSettings(state);
        if (previous && previous.installPath !== installPath && isInside(this.installedRoot, previous.installPath)) {
          await rm(this.containerRoot(previous.installPath), { recursive: true, force: true });
        }
        return plugin;
      } catch (error) {
        await rm(transactionRoot, { recursive: true, force: true });
        throw error;
      }
    });
  }

  async setEnabled(name: string, enabled: boolean): Promise<InstalledPlugin> {
    return this.exclusive(async () => {
      const state = await this.readState();
      const plugin = state.plugins[name];
      if (!plugin) throw new Error(`未安装插件：${name}`);
      plugin.enabled = enabled;
      delete plugin.loadError;
      await writeJsonAtomic(this.statePath, state);
      await this.syncPiSettings(state);
      return plugin;
    });
  }

  async markLoadError(name: string, loadError: string): Promise<InstalledPlugin> {
    return this.exclusive(async () => {
      const state = await this.readState();
      const plugin = state.plugins[name];
      if (!plugin) throw new Error(`未安装插件：${name}`);
      plugin.enabled = false;
      plugin.loadError = loadError;
      await writeJsonAtomic(this.statePath, state);
      await this.syncPiSettings(state);
      return plugin;
    });
  }

  async uninstall(name: string): Promise<void> {
    return this.exclusive(async () => {
      const state = await this.readState();
      const plugin = state.plugins[name];
      if (!plugin) return;
      delete state.plugins[name];
      await writeJsonAtomic(this.statePath, state);
      await this.syncPiSettings(state);
      await rm(this.containerRoot(plugin.installPath), { recursive: true, force: true });
    });
  }
}

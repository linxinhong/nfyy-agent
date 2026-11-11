import { access, chmod, mkdir, readFile, rename, rm, writeFile } from 'node:fs/promises';
import { randomUUID } from 'node:crypto';
import { homedir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { isDeepStrictEqual } from 'node:util';

export function greeting(name = 'world'): string {
  return `Hello, ${name}!`;
}

export { deleteModelSettings, getModelCatalog, getModelSettings, saveModelSettings } from './model-settings.js';
export { getHotkeySettings, saveHotkeySetting } from './hotkeys.js';

export interface YuanpuConfig {
  schemaVersion: 1;
  provider: string;
  model: string;
  workingDirectory: string;
  catalogUrl?: string;
  /** Legacy fields, migrated to app/models.json on startup. */
  apiKeyEnv?: string;
  baseUrl?: string;
  api?: 'openai-completions' | 'openai-responses' | 'anthropic-messages' | 'google-generative-ai';
  notifications?: { enabled: boolean };
  hotkeys?: Record<string, string | null>;
}

export interface YuanpuHome {
  root: string;
  workspacePath: string;
  appPath: string;
  agentPath: string;
  packagesPath: string;
  workflowsPath: string;
  configPath: string;
  skillsPath: string;
  memoryPath: string;
  sessionsPath: string;
  config: YuanpuConfig;
}

const DEFAULT_CONFIG: Omit<YuanpuConfig, 'workingDirectory'> = {
  schemaVersion: 1,
  provider: 'openai',
  model: 'gpt-5.6-luna',
};

function validateConfig(value: unknown, configPath: string): YuanpuConfig {
  if (!value || typeof value !== 'object') throw new Error(`Invalid MindLink config: ${configPath}`);
  const config = value as Partial<YuanpuConfig>;
  if (
    config.schemaVersion !== 1
    || typeof config.provider !== 'string'
    || typeof config.model !== 'string'
    || (config.apiKeyEnv !== undefined && typeof config.apiKeyEnv !== 'string')
    || typeof config.workingDirectory !== 'string'
    || (config.catalogUrl !== undefined && typeof config.catalogUrl !== 'string')
    || (config.baseUrl !== undefined && typeof config.baseUrl !== 'string')
    || (config.api !== undefined && ![
      'openai-completions',
      'openai-responses',
      'anthropic-messages',
      'google-generative-ai',
    ].includes(config.api))
    || (config.notifications !== undefined && (
      !config.notifications
      || typeof config.notifications !== 'object'
      || typeof config.notifications.enabled !== 'boolean'
    ))
    || (config.hotkeys !== undefined && (
      !config.hotkeys || typeof config.hotkeys !== 'object' || Array.isArray(config.hotkeys)
      || Object.values(config.hotkeys).some((binding) => binding !== null && typeof binding !== 'string')
    ))
  ) {
    throw new Error(`Invalid MindLink config: ${configPath}`);
  }
  return { ...config } as YuanpuConfig;
}

async function pathExists(path: string): Promise<boolean> {
  try {
    await access(path);
    return true;
  } catch {
    return false;
  }
}

async function migratePath(from: string, to: string): Promise<void> {
  if (!await pathExists(from) || await pathExists(to)) return;
  await mkdir(dirname(to), { recursive: true });
  await rename(from, to);
}

async function writePrivateJson(path: string, value: unknown): Promise<void> {
  const temporary = `${path}.${randomUUID()}.tmp`;
  try {
    await writeFile(temporary, `${JSON.stringify(value, null, 2)}\n`, { mode: 0o600, flag: 'wx' });
    await rename(temporary, path);
  } finally {
    await rm(temporary, { force: true });
  }
}

function jsonObject(value: unknown, path: string): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(`Invalid MindLink JSON: ${path}`);
  }
  return value as Record<string, unknown>;
}

async function migrateModelData(from: string, to: string, kind: 'auth' | 'models', generated = false): Promise<void> {
  if (!await pathExists(from)) return;
  if (!await pathExists(to) && !generated) {
    await migratePath(from, to);
    return;
  }
  const incoming = jsonObject(JSON.parse(await readFile(from, 'utf8')), from);
  const current = await pathExists(to)
    ? jsonObject(JSON.parse(await readFile(to, 'utf8')), to) : {};
  if (kind === 'models') {
    const oldProviders = jsonObject(incoming.providers, from);
    const providers = current.providers === undefined ? {} : jsonObject(current.providers, to);
    for (const [id, definition] of Object.entries(oldProviders)) {
      if (id in providers && !isDeepStrictEqual(providers[id], definition) && !generated) {
        throw new Error(`Model provider ${id} differs between ${from} and ${to}; resolve before migration.`);
      }
      if (!(id in providers)) providers[id] = definition;
    }
    current.providers = providers;
    for (const [key, value] of Object.entries(incoming)) {
      if (key === 'providers') continue;
      if (key in current && !isDeepStrictEqual(current[key], value) && !generated) {
        throw new Error(`Model setting ${key} differs between ${from} and ${to}; resolve before migration.`);
      }
      if (!(key in current)) current[key] = value;
    }
  } else {
    for (const [id, credential] of Object.entries(incoming)) {
      if (id in current && !isDeepStrictEqual(current[id], credential)) {
        throw new Error(`Credential ${id} differs between ${from} and ${to}; resolve before migration.`);
      }
      if (!(id in current)) current[id] = credential;
    }
  }
  await writePrivateJson(to, current);
  await rm(from);
}

function replaceLegacyPath(value: unknown, legacyRoot: string, packagesPath: string): unknown {
  if (typeof value === 'string') {
    return value.startsWith(legacyRoot) ? `${packagesPath}${value.slice(legacyRoot.length)}` : value;
  }
  if (Array.isArray(value)) return value.map((entry) => replaceLegacyPath(entry, legacyRoot, packagesPath));
  if (!value || typeof value !== 'object') return value;
  return Object.fromEntries(Object.entries(value).map(([key, entry]) => (
    [key, replaceLegacyPath(entry, legacyRoot, packagesPath)]
  )));
}

async function rewriteMigratedPackagePaths(agentPath: string, packagesPath: string, legacyPackagesPath: string) {
  for (const path of [join(packagesPath, 'state.json'), join(agentPath, 'settings.json')]) {
    if (!await pathExists(path)) continue;
    const value = JSON.parse(await readFile(path, 'utf8')) as unknown;
    const migrated = replaceLegacyPath(value, legacyPackagesPath, packagesPath);
    await writeFile(path, `${JSON.stringify(migrated, null, 2)}\n`, { mode: 0o600 });
  }
}

async function migrateLegacyModelConfig(configPath: string, appPath: string, config: YuanpuConfig): Promise<YuanpuConfig> {
  const legacy = config as YuanpuConfig & { modelSelections?: unknown };
  if (!legacy.baseUrl && legacy.apiKeyEnv === undefined && legacy.api === undefined
    && legacy.modelSelections === undefined) return config;

  if (legacy.baseUrl) {
    const modelsPath = join(appPath, 'models.json');
    let models: Record<string, unknown> = { providers: {} };
    try {
      models = jsonObject(JSON.parse(await readFile(modelsPath, 'utf8')), modelsPath);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error;
    }
    if (!models || typeof models !== 'object' || Array.isArray(models)
      || !models.providers || typeof models.providers !== 'object' || Array.isArray(models.providers)) {
      throw new Error(`Invalid MindLink models: ${modelsPath}`);
    }
    const providers = models.providers as Record<string, unknown>;
    const existing = providers[config.provider];
    if (existing !== undefined && (!existing || typeof existing !== 'object' || Array.isArray(existing))) {
      throw new Error(`Invalid MindLink models: ${modelsPath}`);
    }
    const definition = existing as Record<string, unknown> | undefined;
    if (definition && (
      (typeof definition.baseUrl === 'string' && definition.baseUrl !== config.baseUrl)
      || (typeof definition.api === 'string' && definition.api !== (config.api ?? 'openai-completions'))
      || (config.apiKeyEnv && typeof definition.apiKey === 'string' && definition.apiKey !== `$${config.apiKeyEnv}`)
    )) {
      throw new Error(`Model provider ${config.provider} differs between legacy config and ${modelsPath}; resolve before migration.`);
    }
    const entries = Array.isArray(definition?.models) ? [...definition.models] : [];
    if (!entries.some((entry) => entry && typeof entry === 'object' && (entry as { id?: unknown }).id === config.model)) {
      entries.push({ id: config.model, name: config.model, reasoning: false,
        input: ['text'], contextWindow: 128_000, maxTokens: 32_000 });
    }
    providers[config.provider] = {
      ...definition,
      baseUrl: config.baseUrl,
      api: config.api ?? 'openai-completions',
      ...(config.apiKeyEnv && !definition?.apiKey ? { apiKey: `$${config.apiKeyEnv}` } : {}),
      models: entries,
    };
    await writePrivateJson(modelsPath, models);
  }

  const next = { ...legacy };
  delete next.baseUrl;
  delete next.api;
  delete next.apiKeyEnv;
  delete next.modelSelections;
  await writePrivateJson(configPath, next);
  return next;
}

export async function ensureYuanpuHome(root = join(homedir(), '.yuanpu')): Promise<YuanpuHome> {
  const resolvedRoot = resolve(root);
  const appPath = join(resolvedRoot, 'app');
  const agentPath = join(resolvedRoot, 'agent');
  const packagesPath = join(resolvedRoot, 'packages');
  const workflowsPath = join(resolvedRoot, 'workflows');
  const workspacePath = join(resolvedRoot, 'workspace');
  const configPath = join(appPath, 'config.json');
  const skillsPath = join(agentPath, 'skills');
  const memoryPath = join(agentPath, 'memory');
  const sessionsPath = join(agentPath, 'sessions');
  const legacyPackagesPath = join(resolvedRoot, 'plugins');

  await mkdir(resolvedRoot, { recursive: true });
  await Promise.all([
    migratePath(join(resolvedRoot, 'config.json'), configPath),
    migratePath(join(resolvedRoot, 'settings.json'), join(agentPath, 'settings.json')),
    migratePath(join(resolvedRoot, 'models-store.json'), join(agentPath, 'models-store.json')),
    migratePath(join(resolvedRoot, 'mcp.json'), join(agentPath, 'mcp.json')),
    migratePath(join(resolvedRoot, 'mcp-cache.json'), join(agentPath, 'mcp-cache.json')),
    migratePath(join(resolvedRoot, 'skills'), skillsPath),
    migratePath(join(resolvedRoot, 'memory'), memoryPath),
    migratePath(join(resolvedRoot, 'sessions'), sessionsPath),
    migratePath(legacyPackagesPath, packagesPath),
  ]);
  await migrateModelData(join(resolvedRoot, 'auth.json'), join(appPath, 'auth.json'), 'auth');
  await migrateModelData(join(agentPath, 'auth.json'), join(appPath, 'auth.json'), 'auth');
  await migrateModelData(join(resolvedRoot, 'models.json'), join(appPath, 'models.json'), 'models');
  await migrateModelData(join(agentPath, 'models.json'), join(appPath, 'models.json'), 'models');
  await Promise.all([
    mkdir(appPath, { recursive: true }),
    mkdir(agentPath, { recursive: true }),
    mkdir(skillsPath, { recursive: true }),
    mkdir(memoryPath, { recursive: true }),
    mkdir(packagesPath, { recursive: true }),
    mkdir(sessionsPath, { recursive: true }),
    mkdir(workflowsPath, { recursive: true }),
    mkdir(workspacePath, { recursive: true, mode: 0o700 }),
  ]);
  if (process.platform !== 'win32') {
    await chmod(workflowsPath, 0o700);
    await chmod(workspacePath, 0o700);
  }
  await rewriteMigratedPackagePaths(agentPath, packagesPath, legacyPackagesPath);

  let config: YuanpuConfig;
  try {
    config = validateConfig(JSON.parse(await readFile(configPath, 'utf8')), configPath);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error;
    config = { ...DEFAULT_CONFIG, workingDirectory: workspacePath };
    await writeFile(configPath, `${JSON.stringify(config, null, 2)}\n`, { flag: 'wx', mode: 0o600 });
  }
  config = await migrateLegacyModelConfig(configPath, appPath, config);
  await migrateModelData(join(resolvedRoot, 'yuanpu-models.json'), join(appPath, 'models.json'), 'models', true);
  await migrateModelData(join(agentPath, 'yuanpu-models.json'), join(appPath, 'models.json'), 'models', true);

  const memoryFile = join(memoryPath, 'MEMORY.md');
  try {
    await writeFile(
      memoryFile,
      '# MindLink memory\n\nAdd durable preferences and working context here.\n',
      { flag: 'wx' },
    );
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== 'EEXIST') throw error;
  }

  return {
    root: resolvedRoot,
    workspacePath,
    appPath,
    agentPath,
    packagesPath,
    workflowsPath,
    configPath,
    skillsPath,
    memoryPath,
    sessionsPath,
    config,
  };
}

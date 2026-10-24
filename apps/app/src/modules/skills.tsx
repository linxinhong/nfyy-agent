import {
  useEffect,
  useMemo,
  useState,
  type FormEvent,
} from 'react';
import type {
  InstalledPlugin,
  LocalSkill,
  PluginConfigDocument,
  PluginConfigScope,
  PluginSearchResult,
} from '@yuanpu-agent/protocol';


type SkillTab = 'marketplace' | 'installed' | 'local' | 'updates';

const previewPlugins: PluginSearchResult[] = [
  {
    name: '@quintinshaw/pi-dynamic-workflows',
    displayName: '工作流编排',
    version: '3.12.0',
    description: '可组合的工作流、任务拆解与自动化执行能力。',
    publisher: 'quintinshaw',
    source: 'npm:@quintinshaw/pi-dynamic-workflows@3.12.0',
    components: ['agent', 'workflow', 'extension'],
    permissions: ['scripts', 'filesystem', 'background'],
  },
  {
    name: 'pi-hermes-memory',
    displayName: '长期记忆',
    version: '0.9.9',
    description: '为 Pi 提供跨会话的长期记忆、存储和检索能力。',
    publisher: 'community',
    source: 'npm:pi-hermes-memory@0.9.9',
    components: ['skill', 'extension'],
    permissions: ['filesystem', 'background'],
  },

];

const builtinMcp: InstalledPlugin = {
  name: 'pi-mcp', version: '1.4.0', description: 'Pi 内置 MCP 服务连接',
  source: 'builtin:pi-mcp', installPath: '', enabled: true, installedAt: '', configurable: true,
};
const previewInstalled: InstalledPlugin[] = [];

function previewConfig(scope: PluginConfigScope): PluginConfigDocument {
  return {
    pluginName: 'pi-mcp',
    kind: 'mcp',
    title: 'MCP 服务',
    description: '使用 Pi 内置 MCP 连接服务，通过工具搜索按需发现工具。',
    scope,
    path: scope === 'user' ? '~/.yuanpu/agent/mcp.json' : '<workspace>/.pi/mcp.json',
    supportsWorkspace: true,
    secretPolicy: 'environment-only',
    value: {
      mcpServers: {
        'chrome-devtools': {
          command: 'npx',
          args: ['-y', 'chrome-devtools-mcp@1.6.0'],
        },
      },
    },
  };
}

function isDirectPluginSource(value: string): boolean {
  return value.startsWith('npm:')
    || value.startsWith('https://github.com/')
    || value.startsWith('git+https://github.com/');
}

function isArtifactSource(value: string): boolean {
  return value.startsWith('artifact:');
}

function formatError(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function asRecord(value: unknown): Record<string, unknown> {
  return value && typeof value === 'object' && !Array.isArray(value)
    ? value as Record<string, unknown>
    : {};
}

function PluginConfigPage({
  plugin,
  active,
  onBack,
  onChanged,
}: {
  plugin: InstalledPlugin;
  active: boolean;
  onBack: () => void;
  onChanged: () => Promise<void>;
}) {
  const desktop = window.yuanpu;
  const [scope, setScope] = useState<PluginConfigScope>('user');
  const [document, setDocument] = useState<PluginConfigDocument>();
  const [draft, setDraft] = useState<Record<string, unknown>>({});
  const [rawDraft, setRawDraft] = useState('{}');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [saved, setSaved] = useState(false);

  async function load(nextScope: PluginConfigScope) {
    setBusy(true);
    setError(undefined);
    setSaved(false);
    try {
      const next = desktop
        ? await desktop.getPluginConfig(plugin.name, nextScope)
        : previewConfig(nextScope);
      setDocument(next);
      setDraft(next.value);
      setRawDraft(JSON.stringify(next.value, null, 2));
    } catch (loadError) {
      setError(formatError(loadError));
    } finally {
      setBusy(false);
    }
  }

  useEffect(() => {
    void load(scope);
  }, [scope, plugin.name, desktop]);

  const mcpServers = asRecord(draft.mcpServers);

  function updateServer(name: string, next: Record<string, unknown>) {
    setDraft((current) => ({
      ...current,
      mcpServers: { ...asRecord(current.mcpServers), [name]: next },
    }));
    setSaved(false);
  }

  function renameServer(name: string, nextName: string) {
    if (!nextName || nextName === name || nextName in mcpServers) return;
    setDraft((current) => {
      const servers = { ...asRecord(current.mcpServers) };
      const value = servers[name];
      delete servers[name];
      servers[nextName] = value;
      return { ...current, mcpServers: servers };
    });
    setSaved(false);
  }

  function removeServer(name: string) {
    setDraft((current) => {
      const servers = { ...asRecord(current.mcpServers) };
      delete servers[name];
      return { ...current, mcpServers: servers };
    });
    setSaved(false);
  }

  function addServer() {
    let name = 'new-server';
    let index = 2;
    while (name in mcpServers) name = `new-server-${index++}`;
    updateServer(name, { command: 'npx', args: ['-y'] });
  }

  async function save() {
    if (!document) return;
    setBusy(true);
    setError(undefined);
    setSaved(false);
    try {
      let value = draft;
      if (document.kind === 'schema') {
        const parsed = JSON.parse(rawDraft) as unknown;
        if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
          throw new Error('技能配置必须是 JSON 对象。');
        }
        value = parsed as Record<string, unknown>;
      }
      if (desktop) {
        const input = { name: plugin.name, scope, value };
        const validation = await desktop.validatePluginConfig(input);
        if (!validation.valid) throw new Error(validation.errors.join('；'));
        const next = await desktop.savePluginConfig(input);
        setDocument(next);
        setDraft(next.value);
        setRawDraft(JSON.stringify(next.value, null, 2));
        await onChanged();
      }
      setSaved(true);
    } catch (saveError) {
      setError(formatError(saveError));
    } finally {
      setBusy(false);
    }
  }

  async function reset() {
    if (!document || !window.confirm(`重置 ${scope === 'user' ? '用户' : '当前工作区'}配置？`)) return;
    setBusy(true);
    setError(undefined);
    try {
      const next = desktop
        ? await desktop.resetPluginConfig(plugin.name, scope)
        : previewConfig(scope);
      setDocument(next);
      setDraft(next.value);
      setRawDraft(JSON.stringify(next.value, null, 2));
      setSaved(true);
      await onChanged();
    } catch (resetError) {
      setError(formatError(resetError));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className={`plugin-panel plugin-config-panel ${active ? '' : 'view-hidden'}`} aria-hidden={!active}>
      <header className="config-topbar">
        <button type="button" className="back-button" onClick={onBack}>← 返回技能列表</button>
      </header>
      <div className="config-layout">
        <aside className="config-plugin-summary">
          <div className="installed-title">
            <h1>{plugin.name === 'pi-mcp' ? 'MCP 服务' : plugin.name}</h1>
            <span className={plugin.enabled ? 'enabled' : 'disabled'}>{plugin.enabled ? '已启用' : '已停用'}</span>
          </div>
          <p>{plugin.description}</p>
          <dl>
            <div><dt>版本</dt><dd>v{plugin.version}</dd></div>
            <div><dt>来源</dt><dd>{plugin.source.startsWith('builtin:') ? '内置' : plugin.source.startsWith('npm:') ? 'npm' : 'Git'}</dd></div>
            <div><dt>配置状态</dt><dd>{plugin.configStatus === 'invalid' ? '需要修复' : '有效'}</dd></div>
          </dl>
          <p className="config-summary-note">配置独立于安装目录保存，升级技能时不会丢失。</p>
        </aside>

        <div className="config-editor">
          <header className="config-editor-header">
            <div>
              <h2>{document?.title ?? '技能配置'}</h2>
              <p>{document?.description ?? '正在读取配置…'}</p>
              {document && <code title={document.path}>{document.path}</code>}
            </div>
            <div className="scope-switch" role="group" aria-label="配置作用域">
              <button type="button" className={scope === 'user' ? 'active' : ''} onClick={() => setScope('user')}>用户</button>
              <button
                type="button"
                className={scope === 'workspace' ? 'active' : ''}
                disabled={!document?.supportsWorkspace}
                onClick={() => setScope('workspace')}
              >当前工作区</button>
            </div>
          </header>

          <div className="secret-notice">
            <span aria-hidden="true">◆</span>
            API Key、Token 等敏感值必须写成环境变量引用，例如 <code>{'${MCP_API_KEY}'}</code>。
          </div>

          {error && <div className="plugin-error" role="alert">{error}</div>}
          {saved && <div className="config-success" role="status">配置已保存，将在下一次对话中生效。</div>}

          <div className="config-editor-body">
            {document?.kind === 'mcp' && Object.entries(mcpServers).map(([name, value]) => {
              const server = asRecord(value);
              const http = typeof server.url === 'string';
              const args = Array.isArray(server.args) ? server.args.filter((item) => typeof item === 'string') as string[] : [];
              return (
                <article className="mcp-server-card" key={name}>
                  <div className="mcp-server-title">
                    <input aria-label="MCP 服务名称" defaultValue={name} onBlur={(event) => renameServer(name, event.target.value.trim())} />
                    <span>{http ? 'http' : 'stdio'}</span>
                    <label><input type="checkbox" checked={server.enabled !== false && server.disabled !== true} onChange={(event) => {
                      const next: Record<string, unknown> = { ...server, enabled: event.target.checked };
                      delete next.disabled;
                      updateServer(name, next);
                    }} /> 启用</label>
                    <button type="button" className="icon-danger" aria-label={`删除 ${name}`} onClick={() => removeServer(name)}>×</button>
                  </div>
                  <div className="transport-switch">
                    <button type="button" className={!http ? 'active' : ''} onClick={() => {
                      const next: Record<string, unknown> = { ...server, type: 'stdio', command: typeof server.command === 'string' ? server.command : 'npx' };
                      delete next.url;
                      delete next.headers;
                      delete next.oauth;
                      updateServer(name, next);
                    }}>本地命令</button>
                    <button type="button" className={http ? 'active' : ''} onClick={() => {
                      const next: Record<string, unknown> = { ...server, type: 'http', url: typeof server.url === 'string' ? server.url : 'https://' };
                      delete next.command;
                      delete next.args;
                      delete next.env;
                      updateServer(name, next);
                    }}>HTTP</button>
                  </div>
                  {http ? (
                    <label className="config-field"><span>服务地址</span><input value={String(server.url ?? '')} onChange={(event) => updateServer(name, { ...server, url: event.target.value })} /></label>
                  ) : (
                    <>
                      <label className="config-field"><span>命令</span><input value={String(server.command ?? '')} onChange={(event) => updateServer(name, { ...server, command: event.target.value })} /></label>
                      <label className="config-field"><span>参数</span><textarea value={args.join('\n')} onChange={(event) => updateServer(name, { ...server, args: event.target.value.split('\n').filter(Boolean) })} placeholder="每行一个参数" /></label>
                    </>
                  )}
                </article>
              );
            })}

            {document?.kind === 'mcp' && (
              <button type="button" className="add-server-button" onClick={addServer}>＋ 添加 MCP 服务</button>
            )}

            {document?.kind === 'schema' && (
              <label className="raw-config-field">
                <span>JSON 配置</span>
                <textarea value={rawDraft} onChange={(event) => { setRawDraft(event.target.value); setSaved(false); }} spellCheck={false} />
              </label>
            )}

            {!document && !error && <div className="plugin-empty">正在读取技能配置…</div>}
          </div>

          <footer className="config-actions">
            <span>保存后将在下一次对话中生效</span>
            <div>
              <button type="button" onClick={() => void reset()} disabled={busy || !document}>重置</button>
              <button type="button" className="primary" onClick={() => void save()} disabled={busy || !document}>{busy ? '处理中…' : '保存并应用'}</button>
            </div>
          </footer>
        </div>
      </div>
    </section>
  );
}

const componentLabels: Record<NonNullable<PluginSearchResult['components']>[number], string> = {
  skill: '使用指南',
  agent: '专家角色',
  workflow: '工作流',
  extension: '运行扩展',
  prompt: '提示模板',
  theme: '界面主题',
  connector: '服务连接',
};

const permissionLabels: Record<NonNullable<PluginSearchResult['permissions']>[number], string> = {
  instructions: '包含指令',
  scripts: '可执行脚本',
  filesystem: '访问本地文件',
  network: '访问网络',
  credentials: '需要密钥',
  notifications: '显示系统弹窗',
  background: '后台运行',
};

export function SkillPage({ active }: { active: boolean }) {
  const desktop = window.yuanpu;
  const [tab, setTab] = useState<SkillTab>('marketplace');
  const [query, setQuery] = useState('');
  const [results, setResults] = useState<PluginSearchResult[]>(desktop ? [] : previewPlugins);
  const [installed, setInstalled] = useState<InstalledPlugin[]>(desktop ? [] : previewInstalled);
  const [localSkills, setLocalSkills] = useState<LocalSkill[]>([]);
  const [localDiagnostics, setLocalDiagnostics] = useState<Array<{ path: string; message: string }>>([]);
  const [searching, setSearching] = useState(false);
  const [pending, setPending] = useState<string>();
  const [error, setError] = useState<string>();
  const [editingPlugin, setEditingPlugin] = useState<InstalledPlugin>();
  const [trustCandidate, setTrustCandidate] = useState<PluginSearchResult>();
  const [installFailures, setInstallFailures] = useState<Record<string, string>>({});

  const installedByName = useMemo(
    () => new Map(installed.map((plugin) => [plugin.name, plugin])),
    [installed],
  );

  const installedFor = (candidate: PluginSearchResult) => (
    installedByName.get(candidate.id ?? candidate.name) ?? installedByName.get(candidate.name)
  );

  async function refreshInstalled() {
    if (!desktop) return;
    setInstalled(await desktop.listPlugins());
  }

  async function refreshLocalSkills() {
    if (!desktop) return;
    const result = await desktop.listLocalSkills();
    setLocalSkills(result.skills);
    setLocalDiagnostics(result.diagnostics);
  }

  async function search(term = query) {
    setError(undefined);
    if (!desktop) {
      const normalized = term.trim().toLowerCase();
      setResults(previewPlugins.filter((plugin) => (
        !normalized
        || plugin.name.toLowerCase().includes(normalized)
        || plugin.displayName?.toLowerCase().includes(normalized)
        || plugin.description.toLowerCase().includes(normalized)
      )));
      return;
    }
    setSearching(true);
    try {
      setResults(await desktop.searchPlugins(term.trim()));
    } catch (searchError) {
      setError(formatError(searchError));
    } finally {
      setSearching(false);
    }
  }

  useEffect(() => {
    if (!desktop) return;
    void Promise.all([search(''), refreshInstalled(), refreshLocalSkills()]).catch((loadError) => {
      setError(formatError(loadError));
    });
  }, [desktop, active]);

  async function install(candidate: PluginSearchResult) {
    const source = candidate.source;
    if (!desktop) {
      setError('浏览器预览模式不会执行技能安装。请通过 Electron 启动 YuanpuAgent。');
      return;
    }
    setTrustCandidate(undefined);
    setPending(source);
    setError(undefined);
    try {
      if (isArtifactSource(source) && !candidate.artifactManifestDigest) {
        throw new Error('能力清单快照缺失，请重新搜索并确认权限。');
      }
      if (isArtifactSource(source)) {
        const conflicts = await desktop.listMcpOwnershipConflicts(source, candidate.artifactManifestDigest!);
        if (conflicts.length) {
          throw new Error(`内置 MCP 已配置同名连接：${conflicts.map((item) => item.name).join('、')}。请先在 MCP 服务中停用这些连接，再安装能力包。`);
        }
      }
      await desktop.installPlugin(source, candidate.artifactManifestDigest);
      await refreshInstalled();
      setInstallFailures((current) => {
        const next = { ...current };
        delete next[source];
        return next;
      });
      setTab('installed');
    } catch (installError) {
      const message = formatError(installError);
      setError(message);
      setInstallFailures((current) => ({ ...current, [source]: message }));
      if (message.includes('能力清单已变化')) {
        const refreshed = await desktop.searchPlugins(query.trim()).catch(() => undefined);
        if (refreshed) setResults(refreshed);
      }
      const current = await desktop.listPlugins().catch(() => []);
      setInstalled(current);
    } finally {
      setPending(undefined);
    }
  }

  async function submitSearch(event: FormEvent) {
    event.preventDefault();
    const term = query.trim();
    if (isDirectPluginSource(term)) {
      setTrustCandidate({
        name: term,
        version: '固定来源',
        description: '手动输入的固定版本插件来源。',
        source: term,
      });
      return;
    }
    await search(term);
  }

  async function setEnabled(plugin: InstalledPlugin, enabled: boolean) {
    if (!desktop) return;
    setPending(plugin.name);
    setError(undefined);
    try {
      await desktop.setPluginEnabled(plugin.name, enabled);
      await refreshInstalled();
    } catch (stateError) {
      setError(formatError(stateError));
    } finally {
      setPending(undefined);
    }
  }

  async function uninstall(plugin: InstalledPlugin) {
    if (!desktop || !window.confirm(`卸载 ${plugin.name}？\n\n能力包会被删除，但不会删除 ~/.yuanpu/agent/skills 中的本地技能。`)) return;
    setPending(plugin.name);
    setError(undefined);
    try {
      await desktop.uninstallPlugin(plugin.name);
      await refreshInstalled();
    } catch (uninstallError) {
      setError(formatError(uninstallError));
    } finally {
      setPending(undefined);
    }
  }

  async function rollback(plugin: InstalledPlugin, version: string) {
    if (!desktop || pending) return;
    setPending(plugin.name);
    setError(undefined);
    try {
      await desktop.rollbackPlugin(plugin.name, version);
      await refreshInstalled();
    } catch (rollbackError) {
      setError(`回滚失败，仍在使用 v${plugin.activeVersion ?? plugin.version}：${formatError(rollbackError)}`);
    } finally {
      setPending(undefined);
    }
  }

  if (editingPlugin) {
    return (
      <PluginConfigPage
        plugin={editingPlugin}
        active={active}
        onBack={() => setEditingPlugin(undefined)}
        onChanged={async () => {
          await refreshInstalled();
          if (desktop) {
            const current = await desktop.listPlugins();
            const updated = current.find((item) => item.name === editingPlugin.name);
            if (updated) setEditingPlugin(updated);
          }
        }}
      />
    );
  }

  return (
    <section className={`plugin-panel ${active ? '' : 'view-hidden'}`} aria-hidden={!active}>
      <header className="plugin-header">
        <div>
          <h1>技能</h1>
          <p>为 YuanpuAgent 添加工作能力</p>
        </div>
        <button type="button" onClick={() => setEditingPlugin(builtinMcp)}>MCP 服务</button>
        <span>已安装 {installed.length}</span>
      </header>

      <div className="plugin-content">
        <form className="plugin-search" onSubmit={(event) => void submitSearch(event)}>
          <span className="search-icon" aria-hidden="true" />
          <input
            aria-label="搜索技能"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder="搜索技能、服务或工作场景"
          />
          <button type="submit" disabled={searching || Boolean(pending)}>
            {searching ? '搜索中…' : isDirectPluginSource(query.trim()) ? '安装来源' : '搜索'}
          </button>
        </form>

        <div className="plugin-tabs" role="tablist" aria-label="技能视图">
          <button type="button" role="tab" aria-selected={tab === 'marketplace'} onClick={() => setTab('marketplace')}>
            技能市场
          </button>
          <button type="button" role="tab" aria-selected={tab === 'installed'} onClick={() => setTab('installed')}>
            已安装
          </button>
          <button type="button" role="tab" aria-selected={tab === 'local'} onClick={() => setTab('local')}>
            本地技能
          </button>
          <button type="button" role="tab" aria-selected={tab === 'updates'} onClick={() => setTab('updates')}>
            更新
          </button>
        </div>

        <div className="security-notice">
          <span aria-hidden="true">!</span>
          技能可能包含操作指令、运行扩展、工作流或外部服务连接。安装前请查看权限并确认来源可信。
        </div>

        {error && <div className="plugin-error" role="alert">{error}</div>}

        <div className="plugin-list">
          {tab === 'marketplace' && results.map((plugin) => {
            const current = installedFor(plugin);
            const isPending = pending === plugin.source;
            return (
              <article className="plugin-card" key={`${plugin.name}@${plugin.version}`}>
                <div className="plugin-card-main">
                  <h2>{plugin.displayName ?? plugin.name}</h2>
                  <p>{plugin.description}</p>
                  <div className="plugin-meta">
                    <span>v{plugin.version}</span>
                    {plugin.publisher && <span>by {plugin.publisher}</span>}
                    <span title={plugin.name}>{plugin.name}</span>
                  </div>
                  <div className="skill-badges">
                    {plugin.components?.map((component) => (
                      <span className="component-badge" key={component}>{componentLabels[component]}</span>
                    ))}
                    {plugin.permissions?.map((permission) => (
                      <span className="permission-badge" key={permission}>{permissionLabels[permission]}</span>
                    ))}
                  </div>
                </div>
                <button
                  type="button"
                  className="install-button"
                  disabled={Boolean(pending) || current?.version === plugin.version}
                  onClick={() => setTrustCandidate(plugin)}
                >
                  {isPending ? '安装中…' : current?.version === plugin.version ? '已安装' : current ? '更新' : '安装'}
                </button>
                {installFailures[plugin.source] && (
                  <div className="install-recovery" role="alert">
                    <span>更新失败，仍在使用 v{current?.activeVersion ?? current?.version ?? '—'}</span>
                    <button type="button" disabled={Boolean(pending)} onClick={() => setTrustCandidate(plugin)}>重试</button>
                  </div>
                )}
              </article>
            );
          })}

          {tab === 'installed' && installed.map((plugin) => (
            <article className="plugin-card installed" key={plugin.name}>
              <div className="plugin-card-main">
                <div className="installed-title">
                  <h2>{plugin.name}</h2>
                  <span className={plugin.enabled ? 'enabled' : 'disabled'}>
                    {plugin.enabled ? '已启用' : '已停用'}
                  </span>
                </div>
                <p>{plugin.description}</p>
                {plugin.loadError && <div className="plugin-load-error">加载失败：{plugin.loadError}</div>}
                <div className="plugin-meta">
                  <span>v{plugin.version}</span>
                  <span title={plugin.source}>{plugin.source.startsWith('npm:') ? 'npm' : plugin.kind === 'python-mcp' ? '能力制品' : 'Git'}</span>
                </div>
              </div>
              <div className="plugin-actions">
                {plugin.configurable && (
                  <button type="button" disabled={Boolean(pending)} onClick={() => setEditingPlugin(plugin)}>
                    配置
                  </button>
                )}
                {plugin.kind !== 'python-mcp' && (
                  <button type="button" disabled={Boolean(pending)} onClick={() => void setEnabled(plugin, !plugin.enabled)}>
                    {pending === plugin.name ? '处理中…' : plugin.enabled ? '停用' : '启用'}
                  </button>
                )}
                {plugin.kind === 'python-mcp' && plugin.availableVersions
                  ?.filter((version) => version !== plugin.activeVersion)
                  .map((version) => (
                    <button type="button" disabled={Boolean(pending)} key={version} onClick={() => void rollback(plugin, version)}>
                      回滚到 v{version}
                    </button>
                  ))}
                {plugin.kind !== 'python-mcp' && (
                  <button type="button" className="danger" disabled={Boolean(pending)} onClick={() => void uninstall(plugin)}>
                    卸载
                  </button>
                )}
              </div>
            </article>
          ))}

          {tab === 'local' && localSkills.map((skill) => (
            <article className="plugin-card installed" key={skill.filePath}>
              <div className="plugin-card-main">
                <div className="installed-title">
                  <h2>{skill.name}</h2>
                  <span className="enabled">已加载</span>
                </div>
                <p>{skill.description}</p>
                <div className="plugin-meta">
                  <span title={skill.filePath}>本地 SKILL.md</span>
                  {skill.disableModelInvocation && <span>仅手动调用</span>}
                </div>
              </div>
            </article>
          ))}

          {tab === 'local' && localDiagnostics.map((diagnostic) => (
            <div className="plugin-error" role="alert" key={`${diagnostic.path}:${diagnostic.message}`}>
              {diagnostic.path ? `${diagnostic.path}：` : ''}{diagnostic.message}
            </div>
          ))}

          {tab === 'local' && localSkills.length === 0 && localDiagnostics.length === 0 && (
            <div className="plugin-empty skill-directory-empty">
              <strong>还没有本地技能</strong>
              <code>~/.yuanpu/agent/skills</code>
              <span>将包含 SKILL.md 的技能目录放到这里，刷新页面或下次启动后自动加载。</span>
            </div>
          )}

          {tab === 'updates' && results.filter((plugin) => {
            const current = installedFor(plugin);
            return current && current.version !== plugin.version;
          }).map((plugin) => {
            const current = installedFor(plugin)!;
            return (
              <article className="plugin-card" key={`update:${plugin.id ?? plugin.name}@${plugin.version}`}>
                <div className="plugin-card-main">
                  <h2>{plugin.displayName ?? plugin.name}</h2>
                  <p>{plugin.description}</p>
                  <div className="plugin-meta">
                    <span>当前 v{current.activeVersion ?? current.version}</span>
                    <span>可更新至 v{plugin.version}</span>
                  </div>
                </div>
                <button type="button" className="install-button" disabled={Boolean(pending)} onClick={() => setTrustCandidate(plugin)}>
                  {pending === plugin.source ? '更新中…' : '更新'}
                </button>
                {installFailures[plugin.source] && (
                  <div className="install-recovery" role="alert">
                    <span>更新失败，仍在使用 v{current.activeVersion ?? current.version}</span>
                    <button type="button" disabled={Boolean(pending)} onClick={() => setTrustCandidate(plugin)}>重试</button>
                  </div>
                )}
              </article>
            );
          })}

          {tab === 'updates' && results.every((plugin) => {
            const current = installedFor(plugin);
            return !current || current.version === plugin.version;
          }) && (
            <div className="plugin-empty">当前没有待更新的技能。能力包更新会保留独立配置。</div>
          )}

          {tab === 'marketplace' && !searching && results.length === 0 && (
            <div className="plugin-empty">没有找到匹配的技能。也可以输入固定版本的 npm 或 Git 来源。</div>
          )}
          {tab === 'installed' && installed.length === 0 && (
            <div className="plugin-empty">还没有安装技能。前往“技能市场”搜索工作能力。</div>
          )}
        </div>
      </div>
      {trustCandidate && (
        <div className="modal-backdrop" role="presentation" onMouseDown={() => setTrustCandidate(undefined)}>
          <section className="trust-dialog" role="dialog" aria-modal="true" aria-labelledby="trust-title" onMouseDown={(event) => event.stopPropagation()}>
            <span className="dialog-kicker">安装确认</span>
            <h2 id="trust-title">信任并安装 {trustCandidate.displayName ?? trustCandidate.name}？</h2>
            <p>{trustCandidate.description}</p>
            <dl>
              <div><dt>版本</dt><dd>{trustCandidate.version}</dd></div>
              <div><dt>来源</dt><dd>{trustCandidate.source}</dd></div>
              <div><dt>发布者</dt><dd>{trustCandidate.publisher ?? '未知发布者'}</dd></div>
              {trustCandidate.artifactManifestDigest && (
                <div><dt>清单摘要</dt><dd>{trustCandidate.artifactManifestDigest.slice(0, 16)}…</dd></div>
              )}
            </dl>
            <div className="skill-badges">
              {trustCandidate.permissions?.map((permission) => (
                <span className="permission-badge" key={permission}>{permissionLabels[permission]}</span>
              ))}
            </div>
            {isArtifactSource(trustCandidate.source) && (
              <p className="trust-boundary">此能力在独立进程中运行，不会作为 Pi extension 加载；安装仍会验证签名、平台、哈希和兼容版本。</p>
            )}
            <footer>
              <button type="button" onClick={() => setTrustCandidate(undefined)}>取消</button>
              <button type="button" className="primary" onClick={() => void install(trustCandidate)}>信任并安装</button>
            </footer>
          </section>
        </div>
      )}
    </section>
  );
}

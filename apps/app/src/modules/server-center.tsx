import { useCallback, useEffect, useRef, useState } from 'react';
import { useNavigate } from 'react-router';
import type { ServerCenterCatalogItem, ServerCenterResource, ServerCenterStatus, ServerCenterUser, ServerWikiPreview } from '@yuanpu-agent/protocol';

import { AppIcon } from '../shared/app-icon.js';
import { MessageContent } from '../shared/message-content.js';

type CenterTab = 'home' | 'news' | 'wiki' | 'catalog';
const tabs: Array<{ id: CenterTab; label: string }> = [
  { id: 'home', label: '首页' }, { id: 'news', label: '资讯' },
  { id: 'wiki', label: '知识 Wiki' }, { id: 'catalog', label: 'AI 能力' },
];

function statusText(status: ServerCenterStatus | null): string {
  if (!status) return '连接中';
  if (status.state === 'ready') return '已连接';
  if (status.state === 'running') {
    if (status.issueCode === 'DATABASE_NOT_CONFIGURED') return '数据库未配置';
    if (status.issueCode === 'MIGRATIONS_REQUIRED') return '数据库待迁移';
    return '数据库不可用';
  }
  return status.state === 'incompatible' ? '服务不兼容' : '未连接';
}

function resourceLabel(kind: ServerCenterResource['kind']): string {
  return { news: '资讯', wiki: '组织词条', capability: '组织能力' }[kind];
}

function resourceSummary(resource: ServerCenterResource): string {
  return resource.summary.trim() || resource.body.trim().slice(0, 100);
}

export function ServerCenterPage({ active }: { active: boolean }) {
  const navigate = useNavigate();
  const [status, setStatus] = useState<ServerCenterStatus | null>(null);
  const [preview, setPreview] = useState<ServerWikiPreview | null>(null);
  const [catalog, setCatalog] = useState<ServerCenterCatalogItem[]>([]);
  const [resources, setResources] = useState<ServerCenterResource[]>([]);
  const [user, setUser] = useState<ServerCenterUser | null>(null);
  const [tab, setTab] = useState<CenterTab>('home');
  const [selectedWikiId, setSelectedWikiId] = useState('');
  const [selectedCatalogId, setSelectedCatalogId] = useState('');
  const [selectedResource, setSelectedResource] = useState<ServerCenterResource | null>(null);
  const [searchInput, setSearchInput] = useState('');
  const [query, setQuery] = useState('');
  const [authError, setAuthError] = useState('');
  const [error, setError] = useState('');
  const [catalogError, setCatalogError] = useState('');
  const [loading, setLoading] = useState(false);
  const [resourcesLoading, setResourcesLoading] = useState(false);
  const refreshSequence = useRef(0);
  const accountSequence = useRef(0);

  const refresh = useCallback(async (term: string) => {
    const bridge = window.yuanpu;
    if (!bridge) { setError('请在桌面应用中查看元朴中心。'); return; }
    const sequence = ++refreshSequence.current;
    setLoading(true);
    setError('');
    setCatalogError('');
    try {
      const nextStatus = await bridge.getServerCenterStatus();
      if (sequence !== refreshSequence.current) return;
      setStatus(nextStatus);
      if (nextStatus.state === 'offline' || nextStatus.state === 'incompatible') {
        setPreview(null);
        setCatalog([]);
        return;
      }
      const [wikiResult, catalogResult] = await Promise.allSettled([
        bridge.getServerWikiPreview(), bridge.searchServerCatalog(term),
      ]);
      if (sequence !== refreshSequence.current) return;
      setPreview(wikiResult.status === 'fulfilled' ? wikiResult.value : null);
      if (catalogResult.status === 'fulfilled') {
        setCatalog(catalogResult.value);
        setSelectedCatalogId((current) => catalogResult.value.some((item) => item.id === current) ? current : catalogResult.value[0]?.id || '');
      } else {
        setCatalog([]);
        setCatalogError('能力目录暂时无法读取。');
      }
    } catch {
      if (sequence === refreshSequence.current) setError('元朴中心读取失败，请刷新重试。');
    } finally {
      if (sequence === refreshSequence.current) setLoading(false);
    }
  }, []);

  const loadResources = useCallback(async (term: string) => {
    const bridge = window.yuanpu;
    if (!bridge) return;
    const sequence = ++accountSequence.current;
    setResourcesLoading(true);
    try {
      const result = await bridge.listServerResources(term);
      if (sequence !== accountSequence.current) return;
      if (result.ok) {
        setResources(result.value);
        setError('');
      } else if (result.code === 'UNAUTHORIZED') {
        setUser(null);
        setResources([]);
        setSelectedResource(null);
        setAuthError('API key 登录已失效，请到“设置 → 元朴中心”更新。');
      } else {
        setResources([]);
        setError('组织内容暂时无法读取，请稍后刷新。');
      }
    } catch {
      if (sequence === accountSequence.current) setError('组织内容暂时无法读取，请稍后刷新。');
    } finally {
      if (sequence === accountSequence.current) setResourcesLoading(false);
    }
  }, []);

  useEffect(() => { if (active) void refresh(query); }, [active, refresh]);
  useEffect(() => {
    if (!active || !window.yuanpu) return;
    let cancelled = false;
    void Promise.all([window.yuanpu.restoreServerCenterSession(), window.yuanpu.getServerCenterCredentialStatus()])
      .then(([result, credential]) => {
        if (cancelled) return;
        if (result.ok) { setUser(result.value); setAuthError(''); }
        else if (credential.configured) setAuthError('已配置的 API key 无法登录，请到“设置 → 元朴中心”更新。');
      })
      .catch(() => { if (!cancelled) setAuthError('自动登录暂时不可用。'); });
    return () => { cancelled = true; };
  }, [active]);
  useEffect(() => { if (active && user) void loadResources(query); }, [active, user, query, loadResources]);

  async function signOut() {
    accountSequence.current++;
    setUser(null);
    setResources([]);
    setSelectedResource(null);
    await window.yuanpu?.logoutServerCenter();
  }

  async function openResource(id: string) {
    if (!window.yuanpu) return;
    setError('');
    const sequence = accountSequence.current;
    try {
      const result = await window.yuanpu.getServerResource(id);
      if (sequence !== accountSequence.current) return;
      if (result.ok) setSelectedResource(result.value);
      else if (result.code === 'UNAUTHORIZED') {
        setUser(null);
        setResources([]);
        setSelectedResource(null);
        setAuthError('API key 登录已失效，请到“设置 → 元朴中心”更新。');
      } else setError('内容已更新、撤回，或当前账号无权阅读。请刷新重试。');
    } catch {
      if (sequence === accountSequence.current) setError('内容暂时无法打开，请稍后重试。');
    }
  }

  function submitSearch(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const term = searchInput.trim();
    setQuery(term);
    setSelectedResource(null);
    void refresh(term);
  }

  const entries = preview?.entries ?? [];
  const matches = (value: string) => !query || value.toLocaleLowerCase().includes(query.toLocaleLowerCase());
  const visibleEntries = entries.filter((entry) => matches(`${entry.title} ${entry.summary}`));
  const selectedWiki = visibleEntries.find((entry) => entry.id === selectedWikiId) ?? visibleEntries[0];
  const selectedCatalog = catalog.find((item) => item.id === selectedCatalogId) ?? catalog[0];
  const news = resources.filter((item) => item.kind === 'news');
  const organizationWiki = resources.filter((item) => item.kind === 'wiki');
  const capabilities = resources.filter((item) => item.kind === 'capability');

  const resourceList = (items: ServerCenterResource[], empty: string) => items.length
    ? <div className="server-center-resource-list">{items.map((item) => <button key={item.id} type="button" onClick={() => void openResource(item.id)}>
      <small>{resourceLabel(item.kind)} · {item.visibility === 'team' ? '团队' : '组织'}</small>
      <strong>{item.title}</strong><span>{resourceSummary(item)}</span><AppIcon name="chevron" />
    </button>)}</div>
    : <p className="server-center-muted">{resourcesLoading ? '正在读取组织内容…' : empty}</p>;

  return <section className={`simple-page server-center-page ${active ? '' : 'view-hidden'}`} aria-hidden={!active}>
    <header className="server-center-heading"><h1>元朴中心</h1>
      <div className="server-center-actions">
        <span className={`server-center-connection ${status?.state ?? 'checking'}`} role="status" title={status?.url || undefined}>
          <span className="server-center-status-dot" aria-hidden="true" />{statusText(status)}
        </span>
        {user ? <><span className="server-center-account">{user.name}</span><button type="button" onClick={() => void signOut()}>退出并清除 Key</button></>
          : <button type="button" onClick={() => navigate('/settings?section=server-center')}>配置 API key</button>}
        <button type="button" onClick={() => { void refresh(query); if (user) void loadResources(query); }} disabled={loading}><AppIcon name="refresh" />{loading ? '检查中…' : '刷新'}</button>
      </div>
    </header>
    <div className="server-center-content">
      <div className="server-center-toolbar"><nav aria-label="元朴中心内容">{tabs.map((item) => <button type="button" key={item.id}
        className={tab === item.id ? 'selected' : ''} aria-current={tab === item.id ? 'page' : undefined}
        onClick={() => { setTab(item.id); setSelectedResource(null); }}>{item.label}</button>)}</nav>
        <form onSubmit={submitSearch} role="search"><input aria-label="搜索中心内容" placeholder="搜索资讯、知识或能力" value={searchInput}
          onChange={(event) => setSearchInput(event.target.value)} /><button type="submit">搜索</button></form></div>
      {query && <p className="server-center-query">正在查看“{query}”的可见内容 <button type="button" onClick={() => { setSearchInput(''); setQuery(''); void refresh(''); }}>清除</button></p>}
      {error && <p className="server-center-error" role="alert">{error}</p>}
      {authError && <p className="server-center-error" role="alert">{authError}</p>}
      {selectedResource ? <article className="server-center-detail"><button type="button" onClick={() => setSelectedResource(null)}>← 返回{tabs.find((item) => item.id === tab)?.label}</button>
        <small>{resourceLabel(selectedResource.kind)} · {selectedResource.visibility === 'team' ? '团队可见' : '组织可见'}</small>
        <h2>{selectedResource.title}</h2><p>{selectedResource.summary}</p><div className="server-center-detail-body"><MessageContent text={selectedResource.body} /></div>
      </article> : <>
        {(tab === 'home' || tab === 'news') && <section className="server-center-section"><div className="server-center-section-heading"><h2>最新资讯</h2><span>组织动态</span></div>
          {user ? resourceList(news.slice(0, tab === 'home' ? 4 : undefined), query ? '没有匹配的已发布资讯。' : '暂无对你可见的已发布资讯。')
            : <p className="server-center-muted">配置 API key 后查看有权访问的组织资讯。</p>}
        </section>}
        {tab === 'home' && <section className="server-center-section"><div className="server-center-section-heading"><h2>精选知识</h2><button type="button" onClick={() => setTab('wiki')}>查看 Wiki <AppIcon name="chevron" /></button></div>
          {user && organizationWiki.length ? resourceList(organizationWiki.slice(0, 3), '') : <div className="server-center-feature-grid">{visibleEntries.slice(0, 3).map((entry) =>
            <button type="button" key={entry.id} onClick={() => { setSelectedWikiId(entry.id); setTab('wiki'); }}><small>公开示例</small><strong>{entry.title}</strong><span>{entry.summary}</span></button>)}</div>}
        </section>}
        {tab === 'home' && <section className="server-center-section"><div className="server-center-section-heading"><h2>能力速览</h2><button type="button" onClick={() => setTab('catalog')}>查看目录 <AppIcon name="chevron" /></button></div>
          {catalogError && <p className="server-center-error">{catalogError}</p>}
          <div className="server-center-feature-grid">{catalog.slice(0, 3).map((item) => <button type="button" key={item.id} onClick={() => { setSelectedCatalogId(item.id); setTab('catalog'); }}><small>AI 能力 · v{item.version}</small><strong>{item.displayName}</strong><span>{item.description}</span></button>)}</div>
        </section>}
        {tab === 'wiki' && <section className="server-center-section"><div className="server-center-section-heading"><h2>知识 Wiki</h2><span>组织词条与公开示例</span></div>
          {user && organizationWiki.length > 0 && <div className="server-center-org-wiki"><h3>组织词条</h3>{resourceList(organizationWiki, '')}</div>}
          {visibleEntries.length > 0 ? <div className="server-wiki-layout"><aside className="server-wiki-list" aria-label="公开示例词条"><div className="server-wiki-list-heading"><span>公开示例</span><small>{visibleEntries.length} 篇</small></div>
            {visibleEntries.map((entry) => <button type="button" key={entry.id} className={selectedWiki?.id === entry.id ? 'selected' : ''} onClick={() => setSelectedWikiId(entry.id)}><strong>{entry.title}</strong><span>{entry.summary}</span></button>)}</aside>
            {selectedWiki && <article className="server-wiki-article"><div className="server-wiki-kicker">服务端 Wiki / 公开示例</div><h2>{selectedWiki.title}</h2><p className="server-wiki-summary">{selectedWiki.summary}</p>
              <div className="server-wiki-meta"><span>{selectedWiki.level}</span><span>经验值 {selectedWiki.experience}</span><time dateTime={selectedWiki.updatedAt}>更新于 {new Date(selectedWiki.updatedAt).toLocaleDateString('zh-CN')}</time></div>
              <div className="server-wiki-body"><MessageContent text={selectedWiki.body} /></div>
              {selectedWiki.links.length > 0 && <div className="server-wiki-links"><strong>关联词条</strong><div>{selectedWiki.links.map((id) => { const linked = entries.find((entry) => entry.id === id); return linked && <button type="button" key={id} onClick={() => setSelectedWikiId(id)}>{linked.title}<AppIcon name="chevron" /></button>; })}</div></div>}
            </article>}</div> : <p className="server-center-muted">{loading ? '正在读取公开知识…' : query ? '没有匹配的知识内容。' : '知识示例暂时不可用。'}</p>}
        </section>}
        {tab === 'catalog' && <section className="server-center-section"><div className="server-center-section-heading"><h2>AI 能力目录</h2><span>来自 Go 中心服务</span></div>
          {catalogError && <p className="server-center-error" role="alert">{catalogError}</p>}
          <div className="server-center-catalog-layout"><div className="server-center-catalog-list">{catalog.map((item) => <button type="button" key={item.id} className={selectedCatalog?.id === item.id ? 'selected' : ''} onClick={() => setSelectedCatalogId(item.id)}><strong>{item.displayName}</strong><span>{item.description}</span><small>v{item.version}</small></button>)}
            {catalog.length === 0 && <p className="server-center-muted">{loading ? '正在读取能力目录…' : '没有找到匹配的能力。'}</p>}</div>
            {selectedCatalog && <article className="server-center-catalog-detail"><small>能力目录 / {selectedCatalog.publisher}</small><h3>{selectedCatalog.displayName}</h3><p>{selectedCatalog.description}</p>
              <dl><div><dt>版本</dt><dd>v{selectedCatalog.version}</dd></div><div><dt>组件</dt><dd>{selectedCatalog.components.join(' · ') || '未标注'}</dd></div><div><dt>权限</dt><dd>{selectedCatalog.permissions.join(' · ') || '未标注'}</dd></div><div><dt>来源</dt><dd>{selectedCatalog.source.startsWith('artifact:') ? '元朴中心发布' : selectedCatalog.source}</dd></div></dl>
            </article>}</div>
          {user && capabilities.length > 0 && <div className="server-center-org-wiki"><h3>组织发布能力</h3>{resourceList(capabilities, '')}</div>}
        </section>}
      </>}
      <p className="server-center-boundary">本机“知识库”保留在本地；元朴中心展示服务端公开内容和当前账号有权阅读的组织内容。</p>
    </div>
  </section>;
}

import { useCallback, useEffect, useMemo, useState } from 'react';
import { createServerClient, ServerApiError } from '@yuanpu-agent/server-client';
import type {
  CatalogItem,
  ServerResource,
  ServerUser,
  WikiPreviewResponse,
} from '@yuanpu-agent/server-client';
import { CatalogDirectory } from './catalog-directory.js';
import { ApiKeyAdmin } from './api-key-admin.js';

const publicClient = createServerClient({ baseUrl: window.location.origin });
type WikiEntry = WikiPreviewResponse['entries'][number];
type ServiceState = 'checking' | 'ready' | 'degraded' | 'offline';
type IconName = 'news' | 'book' | 'spark' | 'grid' | 'search' | 'arrow' | 'shield';

const kindLabels: Record<ServerResource['kind'], string> = {
  news: '资讯',
  wiki: '知识 Wiki',
  capability: 'AI 能力',
};

function Icon({ name, size = 20 }: { name: IconName; size?: number }) {
  const common = {
    fill: 'none',
    stroke: 'currentColor',
    strokeWidth: 1.8,
    strokeLinecap: 'round' as const,
    strokeLinejoin: 'round' as const,
  };
  return (
    <svg aria-hidden="true" width={size} height={size} viewBox="0 0 24 24" {...common}>
      {name === 'news' && (
        <>
          <rect x="4" y="3" width="16" height="18" rx="2" />
          <path d="M8 8h8M8 12h8M8 16h5" />
        </>
      )}
      {name === 'book' && (
        <>
          <path d="M12 5c-2.5-1.7-5.2-1.8-8-1v15c2.8-.8 5.5-.7 8 1 2.5-1.7 5.2-1.8 8-1V4c-2.8-.8-5.5-.7-8 1Z" />
          <path d="M12 5v15" />
        </>
      )}
      {name === 'spark' && (
        <>
          <path d="m12 2 2.1 7.9L22 12l-7.9 2.1L12 22l-2.1-7.9L2 12l7.9-2.1L12 2Z" />
        </>
      )}
      {name === 'grid' && (
        <>
          <rect x="3" y="3" width="7" height="7" rx="1" />
          <rect x="14" y="3" width="7" height="7" rx="1" />
          <rect x="3" y="14" width="7" height="7" rx="1" />
          <rect x="14" y="14" width="7" height="7" rx="1" />
        </>
      )}
      {name === 'search' && (
        <>
          <circle cx="10.8" cy="10.8" r="6.8" />
          <path d="m16 16 5 5" />
        </>
      )}
      {name === 'arrow' && (
        <>
          <path d="M5 12h14m-6-6 6 6-6 6" />
        </>
      )}
      {name === 'shield' && (
        <>
          <path d="m12 2 8 4v6c0 5-3.4 8.3-8 10-4.6-1.7-8-5-8-10V6l8-4Z" />
          <path d="m9 12 2 2 4-4" />
        </>
      )}
    </svg>
  );
}

function serviceMessage(state: ServiceState, issue: string) {
  if (state === 'checking') return '正在检查中心服务';
  if (state === 'ready') return '中心服务已就绪';
  if (state === 'offline') return '中心服务暂不可用';
  if (issue === 'DATABASE_NOT_CONFIGURED') return '服务在线，数据库未配置';
  if (issue === 'MIGRATIONS_REQUIRED') return '服务在线，数据库待迁移';
  return '服务在线，数据库暂不可用';
}

function matches(text: string, query: string) {
  return !query || text.toLocaleLowerCase().includes(query.toLocaleLowerCase());
}

function resourceSummary(resource: ServerResource) {
  return resource.summary.trim() || resource.body.trim().slice(0, 95);
}

export function CenterApp() {
  const [service, setService] = useState<ServiceState>('checking');
  const [serviceIssue, setServiceIssue] = useState('');
  const [version, setVersion] = useState('');
  const [wikiEntries, setWikiEntries] = useState<WikiEntry[]>([]);
  const [catalogItems, setCatalogItems] = useState<CatalogItem[]>([]);
  const [catalogError, setCatalogError] = useState('');
  const [resources, setResources] = useState<ServerResource[]>([]);
  const [resourcesError, setResourcesError] = useState('');
  const [resourcesLoading, setResourcesLoading] = useState(false);
  const [user, setUser] = useState<ServerUser | null>(null);
  const [token, setToken] = useState('');
  const [loginOpen, setLoginOpen] = useState(false);
  const [loginEmail, setLoginEmail] = useState('');
  const [loginPassword, setLoginPassword] = useState('');
  const [loginError, setLoginError] = useState('');
  const [loginLoading, setLoginLoading] = useState(false);
  const [searchInput, setSearchInput] = useState('');
  const [query, setQuery] = useState('');
  const [activeHash, setActiveHash] = useState(window.location.hash || '#home');
  const [selectedResource, setSelectedResource] = useState<ServerResource | null>(null);
  const [selectedWikiId, setSelectedWikiId] = useState('');
  const [refreshing, setRefreshing] = useState(false);

  const refreshPublic = useCallback(async (signal?: AbortSignal) => {
    setRefreshing(true);
    try {
      const health = await publicClient.health({ signal });
      const [readiness, preview] = await Promise.allSettled([
        publicClient.ready({ signal }),
        publicClient.wikiPreview({ signal }),
      ]);
      if (signal?.aborted) return;
      setVersion(health.version);
      setService(readiness.status === 'fulfilled' ? 'ready' : 'degraded');
      setServiceIssue(
        readiness.status === 'rejected'
          ? readiness.reason instanceof ServerApiError
            ? readiness.reason.code
            : 'DATABASE_UNAVAILABLE'
          : '',
      );
      if (preview.status === 'fulfilled') {
        setWikiEntries(preview.value.entries);
        setSelectedWikiId((current) => current || preview.value.entries[0]?.id || '');
      } else {
        setWikiEntries([]);
      }
    } catch {
      if (!signal?.aborted) {
        setService('offline');
        setVersion('');
        setWikiEntries([]);
      }
    } finally {
      if (!signal?.aborted) setRefreshing(false);
    }
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    void refreshPublic(controller.signal);
    return () => controller.abort();
  }, [refreshPublic]);

  useEffect(() => {
    const controller = new AbortController();
    setCatalogError('');
    void publicClient
      .searchCatalog(query, { signal: controller.signal })
      .then((result) => {
        if (!controller.signal.aborted) setCatalogItems(result.items);
      })
      .catch(() => {
        if (!controller.signal.aborted) {
          setCatalogItems([]);
          setCatalogError('能力目录暂时无法读取。');
        }
      });
    return () => controller.abort();
  }, [query]);

  useEffect(() => {
    if (!token) {
      setResources([]);
      setResourcesError('');
      setResourcesLoading(false);
      return;
    }
    const controller = new AbortController();
    const client = createServerClient({ baseUrl: window.location.origin, token });
    setResourcesLoading(true);
    setResourcesError('');
    void Promise.all([
      client.me({ signal: controller.signal }),
      client.listResources({ q: query }, { signal: controller.signal }),
    ])
      .then(([current, items]) => {
        if (!controller.signal.aborted) {
          setUser(current);
          setResources(items);
        }
      })
      .catch((error: unknown) => {
        if (controller.signal.aborted) return;
        if (error instanceof ServerApiError && error.status === 401) {
          setToken('');
          setUser(null);
          setLoginError('登录状态已失效，请重新登录。');
          setLoginOpen(true);
        } else {
          setResourcesError('组织内容暂时无法读取，请稍后重试。');
        }
      })
      .finally(() => {
        if (!controller.signal.aborted) setResourcesLoading(false);
      });
    return () => controller.abort();
  }, [token, query]);

  useEffect(() => {
    if (loginOpen) document.getElementById('login-email')?.focus();
  }, [loginOpen]);

  useEffect(() => {
    const updateHash = () => setActiveHash(window.location.hash || '#home');
    window.addEventListener('hashchange', updateHash);
    return () => window.removeEventListener('hashchange', updateHash);
  }, []);

  const news = resources.filter((item) => item.kind === 'news');
  const organizationWiki = resources.filter((item) => item.kind === 'wiki');
  const organizationCapabilities = resources.filter((item) => item.kind === 'capability');
  const featuredWiki = useMemo(
    () =>
      wikiEntries.filter((entry) => matches(`${entry.title} ${entry.summary}`, query)).slice(0, 3),
    [wikiEntries, query],
  );
  const selectedWiki = wikiEntries.find((entry) => entry.id === selectedWikiId) ?? wikiEntries[0];

  async function signIn(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setLoginError('');
    setLoginLoading(true);
    try {
      const result = await publicClient.login(loginEmail.trim(), loginPassword);
      setToken(result.token);
      setUser(result.user);
      setLoginPassword('');
      setLoginOpen(false);
    } catch (error) {
      setLoginError(
        error instanceof ServerApiError && error.status === 401
          ? '邮箱或密码不正确。'
          : '暂时无法登录，请检查中心服务后重试。',
      );
    } finally {
      setLoginLoading(false);
    }
  }

  function openLogin() {
    setLoginError('');
    setLoginOpen(true);
  }

  async function signOut() {
    const currentToken = token;
    setToken('');
    setUser(null);
    setResources([]);
    setSelectedResource(null);
    if (currentToken) {
      try {
        await createServerClient({ baseUrl: window.location.origin, token: currentToken }).logout();
      } catch {
        /* A failed revocation cannot keep this page signed in. */
      }
    }
  }

  async function openResource(id: string) {
    if (!token) return;
    try {
      const current = await createServerClient({
        baseUrl: window.location.origin,
        token,
      }).getResource(id);
      setSelectedResource(current);
    } catch (error) {
      if (error instanceof ServerApiError && error.status === 401) {
        setToken('');
        setUser(null);
        setLoginError('登录状态已失效，请重新登录。');
        setLoginOpen(true);
      } else {
        setResourcesError('资源已更新、撤回或当前账号无权阅读，请刷新页面。');
      }
    }
  }

  function search(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setQuery(searchInput.trim());
    document.getElementById('content')?.scrollIntoView({ behavior: 'smooth' });
  }

  return (
    <div className="portal-site">
      <header className="portal-header">
        <div className="portal-header-inner">
          <a className="portal-brand" href="#home" aria-label="元朴中心首页">
            <span className="portal-brand-mark">元</span>
            <strong>元朴中心</strong>
          </a>
          <nav className="portal-nav" aria-label="主导航">
            <a href="#home" className={activeHash === '#home' ? 'active' : ''}>
              首页
            </a>
            <a href="#news" className={activeHash === '#news' ? 'active' : ''}>
              资讯
            </a>
            <a
              href="#wiki"
              className={activeHash === '#wiki' || activeHash === '#wiki-reader' ? 'active' : ''}
            >
              知识 Wiki
            </a>
            <a href="#catalog" className={activeHash === '#catalog' ? 'active' : ''}>
              AI 能力
            </a>
            {user?.role === 'admin' && <a href="#admin" className={activeHash === '#admin' ? 'active' : ''}>后台管理</a>}
          </nav>
          <form className="portal-search" role="search" onSubmit={search}>
            <Icon name="search" size={18} />
            <input
              aria-label="搜索中心内容"
              value={searchInput}
              onChange={(event) => setSearchInput(event.target.value)}
              placeholder="搜索资讯、知识或能力"
            />
            <button className="sr-only" type="submit">
              搜索
            </button>
          </form>
          {user ? (
            <div className="portal-account">
              <span title={user.email}>{user.name}</span>
              <button type="button" onClick={() => void signOut()}>
                退出
              </button>
            </div>
          ) : (
            <button type="button" className="portal-login-button" onClick={openLogin}>
              成员登录
            </button>
          )}
        </div>
      </header>

      <main id="home">
        <section className="portal-hero">
          <div className="portal-container portal-hero-inner">
            <div>
              <p className="portal-eyebrow">YUANPU CENTER · 团队共享平台</p>
              <h1>
                团队知识与 AI 能力，
                <br />
                <em>一处可达。</em>
              </h1>
              <p>汇聚团队经验，连接实践与 AI 能力，为每一次工作提供可信的起点。</p>
              <a className="portal-primary-action" href="#catalog">
                探索能力目录 <Icon name="arrow" size={17} />
              </a>
            </div>
            <div className="portal-hero-points" aria-label="平台价值">
              <div>
                <Icon name="book" size={28} />
                <strong>沉淀团队知识</strong>
                <span>让经验可复用</span>
              </div>
              <div>
                <Icon name="spark" size={28} />
                <strong>发现 AI 能力</strong>
                <span>让工具更易找</span>
              </div>
              <div>
                <Icon name="shield" size={28} />
                <strong>有序共享</strong>
                <span>按权限访问</span>
              </div>
            </div>
          </div>
        </section>

        <div className="portal-container portal-content" id="content">
          {query && (
            <div className="portal-search-context" role="status">
              搜索“{query}”的可见内容{' '}
              <button
                type="button"
                onClick={() => {
                  setSearchInput('');
                  setQuery('');
                }}
              >
                清除
              </button>
            </div>
          )}
          <div className="portal-columns">
            <div className="portal-main-column">
              <section className="portal-panel" id="news">
                <div className="portal-panel-heading">
                  <h2>
                    <Icon name="news" />
                    最新资讯
                  </h2>
                  <span>组织动态</span>
                </div>
                {!user ? (
                  <div className="portal-empty">
                    <Icon name="news" size={36} />
                    <strong>登录后查看组织资讯</strong>
                    <p>资讯按成员和团队权限展示。</p>
                    <button type="button" onClick={openLogin}>
                      成员登录 <Icon name="arrow" size={16} />
                    </button>
                  </div>
                ) : resourcesLoading ? (
                  <div className="portal-empty">正在读取资讯…</div>
                ) : resourcesError ? (
                  <div className="portal-empty" role="alert">
                    {resourcesError}
                  </div>
                ) : news.length ? (
                  <div className="portal-news-list">
                    {news.slice(0, 4).map((item) => (
                      <button
                        type="button"
                        key={item.id}
                        onClick={() => void openResource(item.id)}
                      >
                        <span className="portal-item-label">
                          {item.visibility === 'team' ? '团队资讯' : '组织资讯'}
                        </span>
                        <strong>{item.title}</strong>
                        <small>{resourceSummary(item)}</small>
                        <Icon name="arrow" size={17} />
                      </button>
                    ))}
                  </div>
                ) : (
                  <div className="portal-empty">
                    <Icon name="news" size={36} />
                    <strong>{query ? '没有匹配的已发布资讯' : '暂无对你可见的已发布资讯'}</strong>
                    <p>经审核发布的组织动态会在这里出现。</p>
                  </div>
                )}
              </section>

              <section className="portal-panel portal-wiki-preview" id="wiki">
                <div className="portal-panel-heading">
                  <h2>
                    <Icon name="book" />
                    精选知识
                  </h2>
                  <a href="#wiki-reader">
                    进入知识 Wiki <Icon name="arrow" size={16} />
                  </a>
                </div>
                {organizationWiki.length ? (
                  <div className="portal-knowledge-grid">
                    {organizationWiki.slice(0, 3).map((item) => (
                      <button
                        type="button"
                        className="portal-knowledge-card"
                        key={item.id}
                        onClick={() => void openResource(item.id)}
                      >
                        <span className="portal-card-icon">
                          <Icon name="book" size={23} />
                        </span>
                        <span className="portal-card-tag">组织词条</span>
                        <strong>{item.title}</strong>
                        <small>{resourceSummary(item)}</small>
                        <span className="portal-card-arrow">
                          <Icon name="arrow" size={16} />
                        </span>
                      </button>
                    ))}
                  </div>
                ) : featuredWiki.length ? (
                  <div className="portal-knowledge-grid">
                    {featuredWiki.map((entry) => (
                      <button
                        type="button"
                        className="portal-knowledge-card"
                        key={entry.id}
                        onClick={() => {
                          setSelectedWikiId(entry.id);
                          document
                            .getElementById('wiki-reader')
                            ?.scrollIntoView({ behavior: 'smooth' });
                        }}
                      >
                        <span className="portal-card-icon">
                          <Icon name="book" size={23} />
                        </span>
                        <span className="portal-card-tag">公开示例</span>
                        <strong>{entry.title}</strong>
                        <small>{entry.summary}</small>
                        <span className="portal-card-arrow">
                          <Icon name="arrow" size={16} />
                        </span>
                      </button>
                    ))}
                  </div>
                ) : (
                  <div className="portal-empty compact">
                    {query ? '没有匹配的知识内容。' : '知识示例暂时不可用。'}
                  </div>
                )}
              </section>

              {user && (
                <section className="portal-panel portal-resources" id="resources">
                  <div className="portal-panel-heading">
                    <h2>
                      <Icon name="grid" />
                      组织资源
                    </h2>
                    <span>当前账号有权阅读的已发布内容</span>
                  </div>
                  {resourcesLoading ? (
                    <div className="portal-empty compact">正在读取资源…</div>
                  ) : resourcesError ? (
                    <div className="portal-empty compact" role="alert">
                      {resourcesError}
                    </div>
                  ) : resources.length ? (
                    <div className="portal-resource-list">
                      {resources.map((item) => (
                        <button
                          type="button"
                          key={item.id}
                          onClick={() => void openResource(item.id)}
                        >
                          <span>{kindLabels[item.kind]}</span>
                          <strong>{item.title}</strong>
                          <small>{resourceSummary(item)}</small>
                          <Icon name="arrow" size={16} />
                        </button>
                      ))}
                    </div>
                  ) : (
                    <div className="portal-empty compact">当前没有可见的已发布资源。</div>
                  )}
                </section>
              )}
            </div>

            <aside className="portal-side-column">
              <section className="portal-panel portal-overview">
                <div className="portal-panel-heading">
                  <h2>
                    <Icon name="grid" />
                    平台概览
                  </h2>
                </div>
                <div className="portal-status">
                  <span className={`portal-status-dot ${service}`} />
                  <div>
                    <strong>{serviceMessage(service, serviceIssue)}</strong>
                    <small>{version ? `Server ${version}` : 'Go 中心服务'}</small>
                  </div>
                  <button
                    type="button"
                    onClick={() => void refreshPublic()}
                    disabled={refreshing}
                    aria-label="刷新中心服务状态"
                  >
                    ↻
                  </button>
                </div>
                <a className="portal-overview-row" href="#catalog">
                  <span>
                    <Icon name="spark" size={19} />
                    能力目录
                  </span>
                  <strong>{catalogItems.length} 项能力</strong>
                  <Icon name="arrow" size={17} />
                </a>
                <a className="portal-overview-row" href="#wiki">
                  <span>
                    <Icon name="book" size={19} />
                    公开知识
                  </span>
                  <strong>{wikiEntries.length} 篇示例</strong>
                  <Icon name="arrow" size={17} />
                </a>
              </section>
              <section className="portal-panel portal-quick">
                <div className="portal-panel-heading">
                  <h2>
                    <Icon name="spark" />
                    能力速览
                  </h2>
                  <a href="#catalog">
                    查看全部 <Icon name="arrow" size={16} />
                  </a>
                </div>
                {catalogError ? (
                  <p className="portal-side-error" role="alert">
                    {catalogError}
                  </p>
                ) : catalogItems.length ? (
                  catalogItems.slice(0, 3).map((item) => (
                    <a key={item.id} className="portal-capability-row" href="#catalog">
                      <span className="portal-capability-icon">
                        <Icon name="spark" size={19} />
                      </span>
                      <span>
                        <strong>{item.displayName}</strong>
                        <small>{item.description}</small>
                      </span>
                      <Icon name="arrow" size={16} />
                    </a>
                  ))
                ) : (
                  <p className="portal-side-error">
                    {query ? '没有匹配的能力。' : '能力目录正在加载…'}
                  </p>
                )}
              </section>
              {organizationCapabilities.length > 0 && (
                <section className="portal-panel portal-quick">
                  <div className="portal-panel-heading">
                    <h2>
                      <Icon name="shield" />
                      组织发布能力
                    </h2>
                  </div>
                  {organizationCapabilities.slice(0, 3).map((item) => (
                    <button
                      type="button"
                      className="portal-capability-row"
                      key={item.id}
                      onClick={() => void openResource(item.id)}
                    >
                      <span className="portal-capability-icon">
                        <Icon name="spark" size={19} />
                      </span>
                      <span>
                        <strong>{item.title}</strong>
                        <small>{resourceSummary(item)}</small>
                      </span>
                      <Icon name="arrow" size={16} />
                    </button>
                  ))}
                </section>
              )}
            </aside>
          </div>

          <section className="portal-reader" id="wiki-reader">
            <div className="portal-section-heading">
              <div>
                <p className="portal-eyebrow">KNOWLEDGE WIKI</p>
                <h2>公开知识示例</h2>
                <p>了解中心如何整理、审核和分发团队知识。个人知识库仍保留在本机。</p>
              </div>
            </div>
            {selectedWiki ? (
              <div className="portal-reader-layout">
                <div className="portal-reader-list">
                  {wikiEntries.map((entry) => (
                    <button
                      type="button"
                      key={entry.id}
                      className={entry.id === selectedWiki.id ? 'selected' : ''}
                      onClick={() => setSelectedWikiId(entry.id)}
                    >
                      <strong>{entry.title}</strong>
                      <small>{entry.summary}</small>
                    </button>
                  ))}
                </div>
                <article className="portal-reader-article">
                  <span className="portal-card-tag">公开示例</span>
                  <h3>{selectedWiki.title}</h3>
                  <p>{selectedWiki.summary}</p>
                  <div className="portal-reader-meta">
                    <span>{selectedWiki.level}</span>
                    <span>经验值 {selectedWiki.experience}</span>
                    <time dateTime={selectedWiki.updatedAt}>
                      {new Date(selectedWiki.updatedAt).toLocaleDateString('zh-CN')}
                    </time>
                  </div>
                  <div className="portal-reader-body">
                    {selectedWiki.body
                      .split('\n')
                      .map((line, index) =>
                        line.startsWith('## ') ? (
                          <h4 key={index}>{line.slice(3)}</h4>
                        ) : line ? (
                          <p key={index}>{line}</p>
                        ) : null,
                      )}
                  </div>
                </article>
              </div>
            ) : (
              <div className="portal-empty compact">公开知识示例暂时无法读取。</div>
            )}
          </section>
          <CatalogDirectory />
          {user?.role === 'admin' && <ApiKeyAdmin token={token} active={activeHash === '#admin'} />}
        </div>
      </main>

      <footer className="portal-footer">
        <div className="portal-container">
          <strong>元朴中心</strong>
          <span>知识、资讯与 AI 能力，在组织内有序流动。</span>
          <span>服务版本 {version || '—'}</span>
        </div>
      </footer>

      {loginOpen && (
        <div
          className="portal-modal-backdrop"
          onMouseDown={(event) => {
            if (event.target === event.currentTarget) setLoginOpen(false);
          }}
        >
          <div
            className="portal-modal"
            role="dialog"
            aria-modal="true"
            aria-labelledby="login-title"
            onKeyDown={(event) => {
              if (event.key === 'Escape') setLoginOpen(false);
            }}
          >
            <button
              type="button"
              className="portal-modal-close"
              aria-label="关闭登录"
              onClick={() => setLoginOpen(false)}
            >
              ×
            </button>
            <span className="portal-brand-mark">元</span>
            <h2 id="login-title">登录元朴中心</h2>
            <p>使用管理员分配的组织账号，查看按权限共享的内容。</p>
            <form onSubmit={(event) => void signIn(event)}>
              <label htmlFor="login-email">邮箱</label>
              <input
                id="login-email"
                type="email"
                autoComplete="username"
                required
                value={loginEmail}
                onChange={(event) => setLoginEmail(event.target.value)}
              />
              <label htmlFor="login-password">密码</label>
              <input
                id="login-password"
                type="password"
                autoComplete="current-password"
                required
                value={loginPassword}
                onChange={(event) => setLoginPassword(event.target.value)}
              />
              {loginError && (
                <span className="portal-form-error" role="alert">
                  {loginError}
                </span>
              )}
              <button type="submit" className="portal-submit" disabled={loginLoading}>
                {loginLoading ? '正在登录…' : '登录'}
              </button>
            </form>
            <small>登录状态仅保留在当前页面，不会写入浏览器持久存储。</small>
          </div>
        </div>
      )}
      {selectedResource && (
        <div
          className="portal-modal-backdrop"
          onMouseDown={(event) => {
            if (event.target === event.currentTarget) setSelectedResource(null);
          }}
        >
          <article
            className="portal-modal portal-resource-modal"
            role="dialog"
            aria-modal="true"
            aria-labelledby="resource-title"
            onKeyDown={(event) => {
              if (event.key === 'Escape') setSelectedResource(null);
            }}
          >
            <button
              type="button"
              className="portal-modal-close"
              aria-label="关闭资源详情"
              onClick={() => setSelectedResource(null)}
            >
              ×
            </button>
            <span className="portal-card-tag">
              {kindLabels[selectedResource.kind]} ·{' '}
              {selectedResource.visibility === 'team' ? '团队可见' : '组织可见'}
            </span>
            <h2 id="resource-title">{selectedResource.title}</h2>
            <p className="portal-resource-summary">{selectedResource.summary}</p>
            <div className="portal-reader-meta">
              <span>版本 {selectedResource.revision}</span>
              <span>已发布</span>
            </div>
            <div className="portal-resource-body">
              {selectedResource.body
                .split('\n')
                .map((line, index) =>
                  line.startsWith('## ') ? (
                    <h3 key={index}>{line.slice(3)}</h3>
                  ) : line ? (
                    <p key={index}>{line}</p>
                  ) : null,
                )}
            </div>
            {selectedResource.kind === 'capability' && (
              <p className="portal-capability-note">能力执行及安装在桌面客户端完成。</p>
            )}
          </article>
        </div>
      )}
    </div>
  );
}

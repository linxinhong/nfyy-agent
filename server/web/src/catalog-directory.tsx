import { useCallback, useEffect, useState } from 'react';
import { createServerClient } from '@yuanpu-agent/server-client';
import type { CatalogItem } from '@yuanpu-agent/server-client';

const client = createServerClient({ baseUrl: window.location.origin });

export function CatalogDirectory() {
  const [query, setQuery] = useState('');
  const [items, setItems] = useState<CatalogItem[]>([]);
  const [selectedId, setSelectedId] = useState('');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');

  const search = useCallback(async (term: string, signal?: AbortSignal) => {
    setLoading(true);
    setError('');
    try {
      const result = await client.searchCatalog(term, { signal });
      if (signal?.aborted) return;
      setItems(result.items);
      setSelectedId((current) =>
        result.items.some((item) => item.id === current) ? current : (result.items[0]?.id ?? ''),
      );
    } catch {
      if (signal?.aborted) return;
      setItems([]);
      setError('能力目录读取失败，请检查中心服务后重试。');
    } finally {
      if (!signal?.aborted) setLoading(false);
    }
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    void search('', controller.signal);
    return () => controller.abort();
  }, [search]);

  const selected = items.find((item) => item.id === selectedId) ?? items[0];

  return (
    <section className="section catalog-section" id="catalog">
      <div className="section-heading">
        <div>
          <p className="eyebrow">CAPABILITY DIRECTORY</p>
          <h2>技能与 AI 能力</h2>
          <p>发现团队可用的工具与能力包。</p>
        </div>
        <span className="section-number">02 / 能力</span>
      </div>
      <form
        className="catalog-search"
        onSubmit={(event) => {
          event.preventDefault();
          void search(query);
        }}
      >
        <label htmlFor="catalog-query">搜索能力目录</label>
        <div>
          <input
            id="catalog-query"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder="按名称、用途或组件搜索"
          />
          <button type="submit" disabled={loading}>
            搜索
          </button>
        </div>
      </form>
      {error && (
        <p className="catalog-error" role="alert">
          {error}
        </p>
      )}
      <div className="catalog-layout">
        <div className="catalog-list" aria-label="能力目录结果">
          {loading && <p className="catalog-empty">正在读取目录…</p>}
          {!loading && items.length === 0 && <p className="catalog-empty">没有找到匹配的能力。</p>}
          {items.map((item) => (
            <button
              type="button"
              key={item.id}
              className={selected?.id === item.id ? 'selected' : ''}
              onClick={() => setSelectedId(item.id)}
            >
              <span className="catalog-item-mark">{item.displayName.slice(0, 1)}</span>
              <span>
                <strong>{item.displayName}</strong>
                <small>{item.description}</small>
              </span>
              <span className="catalog-item-version">v{item.version}</span>
            </button>
          ))}
        </div>
        {selected && (
          <article className="catalog-detail">
            <p className="article-kicker">能力目录 / {selected.publisher}</p>
            <h3>{selected.displayName}</h3>
            <p>{selected.description}</p>
            <dl>
              <div>
                <dt>版本</dt>
                <dd>v{selected.version}</dd>
              </div>
              <div>
                <dt>组件</dt>
                <dd>{selected.components.join(' · ')}</dd>
              </div>
              <div>
                <dt>权限</dt>
                <dd>{selected.permissions.join(' · ')}</dd>
              </div>
              <div>
                <dt>来源</dt>
                <dd>
                  {selected.source.startsWith('artifact:') ? '元朴中心发布' : selected.source}
                </dd>
              </div>
            </dl>
            <small>安装与权限确认由桌面客户端处理。</small>
          </article>
        )}
      </div>
    </section>
  );
}

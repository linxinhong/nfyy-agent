import { useEffect, useState } from 'react';

import { AppIcon } from '../shared/app-icon.js';
import { MessageContent } from '../shared/message-content.js';
import { readSavedContent, removeSavedContent, savedContentEvent, type SavedContentKind } from '../shared/saved-content.js';

export function KnowledgePage({ active, onOpen }: { active: boolean; onOpen: (view: 'work' | 'server') => void }) {
  const [kind, setKind] = useState<SavedContentKind>('knowledge');
  const [items, setItems] = useState(readSavedContent);
  useEffect(() => {
    const refresh = () => setItems(readSavedContent());
    window.addEventListener(savedContentEvent, refresh);
    if (active) refresh();
    return () => window.removeEventListener(savedContentEvent, refresh);
  }, [active]);
  const visible = items.filter((item) => item.kind === kind);

  return <section className={`simple-page saved-content-page ${active ? '' : 'view-hidden'}`} aria-hidden={!active}>
    <header><span className="eyebrow">YUANPU / KNOWLEDGE</span><h1>知识库</h1><p>整理从助手回复中保存的内容。</p></header>
    <div className="saved-content-tabs" role="group" aria-label="保存内容类型">
      <button type="button" aria-pressed={kind === 'knowledge'} onClick={() => setKind('knowledge')}>知识库资料 <span>{items.filter((item) => item.kind === 'knowledge').length}</span></button>
      <button type="button" aria-pressed={kind === 'memory'} onClick={() => setKind('memory')}>记忆 <span>{items.filter((item) => item.kind === 'memory').length}</span></button>
    </div>
    <p className="saved-content-disclaimer">本机已保存的{kind === 'knowledge' ? '资料' : '记忆'}可在此查看。索引和助理检索尚未接入，保存后不会自动用于回答。</p>
    {visible.length ? <div className="saved-content-list">{visible.map((item) => <article key={item.id} className="saved-content-card">
      <div className="saved-content-card-heading"><span>{item.surface === 'work' ? '工作' : '助理'} · {new Date(item.savedAt).toLocaleString('zh-CN')}</span><button type="button" onClick={() => { if (window.confirm('删除这条本地保存的内容？')) removeSavedContent(item.id); }}>删除</button></div>
      <MessageContent text={item.text} />
    </article>)}</div> : <div className="knowledge-empty">
      <div className="knowledge-empty-copy"><AppIcon name={kind === 'knowledge' ? 'knowledge' : 'bookmark'} />
        <h2>{kind === 'knowledge' ? '还没有知识库资料' : '还没有保存的记忆'}</h2>
        <p>在助手回复下方使用“{kind === 'knowledge' ? '保存到知识库' : '保存到记忆'}”即可添加。</p>
      </div>
      <div className="knowledge-empty-actions">
        <button type="button" onClick={() => onOpen('work')}>开始工作</button>
        {kind === 'knowledge' && <button type="button" onClick={() => onOpen('server')}>浏览团队知识</button>}
      </div>
    </div>}
  </section>;
}

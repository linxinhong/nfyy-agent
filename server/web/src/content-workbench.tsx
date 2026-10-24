import { useCallback, useEffect, useState } from 'react';
import { createServerClient } from '@yuanpu-agent/server-client';
import type { ResourceInput, ServerResource, ServerUser } from '@yuanpu-agent/server-client';

const labels = { news: '资讯', wiki: '知识 Wiki', capability: 'AI 能力' };
const states = { draft: '草稿', submitted: '待审核', approved: '已发布', rejected: '已退回' };

export function ContentWorkbench({ token, user, onPublished }: { token: string; user: ServerUser; onPublished: () => void }) {
  const [mine, setMine] = useState<ServerResource[]>([]);
  const [queue, setQueue] = useState<ServerResource[]>([]);
  const [form, setForm] = useState<ResourceInput>({ kind: 'wiki', visibility: 'organization', teamId: null, title: '', summary: '', body: '', details: {} });
  const [editingId, setEditingId] = useState('');
  const [working, setWorking] = useState(false);
  const [message, setMessage] = useState('');
  const [error, setError] = useState('');
  const api = createServerClient({ baseUrl: window.location.origin, token });
  const refresh = useCallback(async (signal?: AbortSignal) => {
    const client = createServerClient({ baseUrl: window.location.origin, token });
    const [own, review] = await Promise.all([client.listMyResources({ signal }), client.reviewQueue({ signal })]);
    if (!signal?.aborted) { setMine(own); setQueue(review); }
  }, [token]);
  useEffect(() => {
    const controller = new AbortController();
    void refresh(controller.signal).catch(() => { if (!controller.signal.aborted) setError('工作台暂时无法读取。'); });
    return () => controller.abort();
  }, [refresh]);

  async function run(action: () => Promise<unknown>, success: string) {
    setWorking(true); setError(''); setMessage('');
    try { await action(); await refresh(); setMessage(success); onPublished(); }
    catch { setError('操作失败，请检查当前权限及资源状态后重试。'); }
    finally { setWorking(false); }
  }

  async function create(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    await run(async () => {
      const input = { ...form, title: form.title.trim(), summary: form.summary.trim(), body: form.body.trim(),
        teamId: form.visibility === 'team' ? user.teamId : null };
      if (editingId) await api.editResource(editingId, input);
      else await api.createResource(input);
      setEditingId('');
      setForm((value) => ({ ...value, title: '', summary: '', body: '' }));
    }, '草稿已保存。请检查内容后由作者确认提交。');
  }

  return <section className="portal-admin" id="workbench" aria-label="内容工作台">
    <div className="portal-section-heading"><div><p className="portal-eyebrow">CONTENT WORKFLOW</p><h2>内容工作台</h2>
      <p>作者保存草稿并确认提交；另一位有审核权限的成员决定发布或退回。</p></div></div>
    {message && <p className="portal-workbench-message" role="status">{message}</p>}
    {error && <p className="portal-form-error" role="alert">{error}</p>}
    <div className="portal-admin-grid"><div className="portal-admin-panel"><h3>{editingId ? '编辑草稿' : '撰写内容'}</h3>
      <form onSubmit={(event) => void create(event)}>
        <label>类型<select value={form.kind} disabled={Boolean(editingId)} onChange={(event) => setForm({ ...form, kind: event.target.value as ResourceInput['kind'] })}>
          {Object.entries(labels).map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select></label>
        <label>可见范围<select value={form.visibility} disabled={Boolean(editingId)} onChange={(event) => setForm({ ...form, visibility: event.target.value as ResourceInput['visibility'] })}>
          <option value="organization">整个组织</option><option value="team" disabled={!user.teamId}>所属团队</option></select></label>
        <label>标题<input value={form.title} maxLength={160} required onChange={(event) => setForm({ ...form, title: event.target.value })} /></label>
        <label>摘要<input value={form.summary} maxLength={300} onChange={(event) => setForm({ ...form, summary: event.target.value })} /></label>
        <label>正文<textarea value={form.body} required rows={8} maxLength={form.kind === 'wiki' ? 800 : 200000} onChange={(event) => setForm({ ...form, body: event.target.value })} /></label>
        <button type="submit" disabled={working}>{editingId ? '保存新版本' : '保存草稿'}</button>
        {editingId && <button type="button" onClick={() => { setEditingId(''); setForm({ kind: 'wiki', visibility: 'organization', teamId: null, title: '', summary: '', body: '', details: {} }); }}>取消编辑</button>}
      </form></div>
      <div className="portal-admin-panel"><div className="portal-admin-list-heading"><h3>我的内容</h3><button type="button" onClick={() => void refresh().catch(() => setError('刷新失败。'))}>刷新</button></div>
        {mine.length === 0 && <p className="portal-admin-empty">尚无内容。</p>}
        <div className="portal-workbench-list">{mine.map((item) => <article key={item.id} className="portal-workbench-item">
          <div><strong>{item.title}</strong><small>{labels[item.kind]} · {states[item.status]} · 第 {item.revision} 版</small><p>{item.summary}</p></div>
          {(item.status === 'draft' || item.status === 'rejected') && <div className="portal-workbench-actions">
            <button type="button" disabled={working} onClick={() => { setEditingId(item.id); setForm({ kind: item.kind, visibility: item.visibility, teamId: item.teamId, title: item.title, summary: item.summary, body: item.body, details: item.details }); document.getElementById('workbench')?.scrollIntoView({ behavior: 'smooth' }); }}>编辑</button>
            <button type="button" disabled={working} onClick={() => {
              if (window.confirm(`确认“${item.title}”内容准确，并提交给另一位成员审核？`)) void run(() => api.submitResource(item.id), '已由作者确认，等待独立审核。');
            }}>确认并提交</button></div>}
        </article>)}</div></div></div>
    <div className="portal-admin-panel portal-workbench-review"><h3>待我审核</h3>
      {queue.length === 0 && <p className="portal-admin-empty">当前没有可审核的投稿。</p>}
      <div className="portal-workbench-list">{queue.map((item) => <article key={item.id} className="portal-workbench-item">
        <div><strong>{item.title}</strong><small>{labels[item.kind]} · {item.visibility === 'team' ? '团队' : '组织'} · 第 {item.revision} 版</small>
          <p>{item.summary}</p><details><summary>查看正文</summary><div className="portal-workbench-body">{item.body}</div></details></div>
        <div className="portal-workbench-actions"><button type="button" disabled={working} onClick={() => void run(() => api.approveResource(item.id), '内容已发布。')}>批准发布</button>
          <button type="button" disabled={working} onClick={() => void run(() => api.rejectResource(item.id), '内容已退回作者。')}>退回</button></div>
      </article>)}</div></div>
  </section>;
}

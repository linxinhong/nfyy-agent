import { useCallback, useEffect, useState } from 'react';
import { createServerClient } from '@yuanpu-agent/server-client';
import type { IssuedAPIKey, ServerAPIKey, ServerUser } from '@yuanpu-agent/server-client';

function date(value: string) {
  return new Date(value).toLocaleDateString('zh-CN');
}

export function ApiKeyAdmin({ token, active }: { token: string; active: boolean }) {
  const [users, setUsers] = useState<ServerUser[]>([]);
  const [keys, setKeys] = useState<ServerAPIKey[]>([]);
  const [userId, setUserId] = useState('');
  const [name, setName] = useState('桌面客户端');
  const [days, setDays] = useState(90);
  const [issued, setIssued] = useState<IssuedAPIKey | null>(null);
  const [loading, setLoading] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState('');
  const client = createServerClient({ baseUrl: window.location.origin, token });

  const refresh = useCallback(async (signal?: AbortSignal) => {
    setLoading(true);
    setError('');
    try {
      const api = createServerClient({ baseUrl: window.location.origin, token });
      const [members, records] = await Promise.all([api.listUsers({ signal }), api.listAPIKeys({ signal })]);
      if (signal?.aborted) return;
      setUsers(members);
      setKeys(records);
      setUserId((current) => members.some((member) => member.id === current && member.active)
        ? current : members.find((member) => member.active)?.id ?? '');
    } catch {
      if (!signal?.aborted) setError('管理数据读取失败，请检查登录状态后重试。');
    } finally {
      if (!signal?.aborted) setLoading(false);
    }
  }, [token]);

  useEffect(() => {
    const controller = new AbortController();
    void refresh(controller.signal);
    return () => controller.abort();
  }, [refresh]);
  useEffect(() => { if (!active) setIssued(null); }, [active]);

  async function issue(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setSubmitting(true);
    setError('');
    setIssued(null);
    try {
      const result = await client.issueAPIKey({ userId, name: name.trim(), expiresInDays: days });
      setIssued(result);
      await refresh();
    } catch {
      setError('发放失败，请检查成员、名称和有效期后重试。');
    } finally {
      setSubmitting(false);
    }
  }

  async function revoke(key: ServerAPIKey) {
    if (!window.confirm(`撤销“${key.name}”？使用这个 Key 建立的会话会立即失效。`)) return;
    setError('');
    try {
      await client.revokeAPIKey(key.id);
      await refresh();
    } catch {
      setError('撤销失败，请稍后重试。');
    }
  }

  return <section className="portal-admin" id="admin" aria-label="元朴中心后台管理">
    <div className="portal-section-heading"><div><p className="portal-eyebrow">ADMINISTRATION</p><h2>后台管理</h2>
      <p>为成员发放桌面客户端 API key，并随时撤销访问。</p></div></div>
    <div className="portal-admin-grid"><div className="portal-admin-panel"><h3>发放 API key</h3>
      <form onSubmit={(event) => void issue(event)}>
        <label>所属成员<select value={userId} onChange={(event) => setUserId(event.target.value)} required>
          {users.filter((member) => member.active).map((member) => <option key={member.id} value={member.id}>{member.name} · {member.email}</option>)}
        </select></label>
        <label>用途名称<input value={name} onChange={(event) => setName(event.target.value)} maxLength={120} required /></label>
        <label>有效期<select value={days} onChange={(event) => setDays(Number(event.target.value))}>
          <option value={30}>30 天</option><option value={90}>90 天</option><option value={180}>180 天</option><option value={365}>365 天</option>
        </select></label>
        <button type="submit" disabled={!userId || submitting}>{submitting ? '发放中…' : '发放 Key'}</button>
      </form>
      {issued && <div className="portal-admin-issued" role="status"><strong>Key 已发放，仅显示这一次</strong>
        <p>请将它粘贴到 yuanpu-agent 的“设置 → 元朴中心”。关闭此页后无法重新查看。</p>
        <code>{issued.secret}</code>
        <button type="button" onClick={() => void navigator.clipboard.writeText(issued.secret)}>复制 Key</button>
      </div>}
    </div>
    <div className="portal-admin-panel"><div className="portal-admin-list-heading"><h3>已发放的 Key</h3><button type="button" onClick={() => void refresh()} disabled={loading}>刷新</button></div>
      {error && <p className="portal-form-error" role="alert">{error}</p>}
      {loading && <p className="portal-admin-empty">正在读取…</p>}
      {!loading && keys.length === 0 && <p className="portal-admin-empty">尚未发放 API key。</p>}
      <div className="portal-admin-key-list">{keys.map((key) => {
        const member = users.find((item) => item.id === key.userId);
        const revoked = Boolean(key.revokedAt);
        const expired = new Date(key.expiresAt).getTime() <= Date.now();
        return <div className="portal-admin-key" key={key.id}><div><strong>{key.name}</strong><small>{member?.name ?? '已删除成员'} · {key.keyPrefix}…</small>
          <small>发放 {date(key.createdAt)} · 到期 {date(key.expiresAt)}{key.lastUsedAt ? ` · 最近使用 ${date(key.lastUsedAt)}` : ''}</small></div>
          {revoked || expired ? <span>{revoked ? '已撤销' : '已过期'}</span> : <button type="button" onClick={() => void revoke(key)}>撤销</button>}
        </div>;
      })}</div>
    </div></div>
  </section>;
}

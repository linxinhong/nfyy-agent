import { useCallback, useEffect, useState } from 'react';
import { createServerClient } from '@yuanpu-agent/server-client';
import type { IssuedAPIKey, RolePermission, ServerAPIKey, ServerTeam, ServerUser } from '@yuanpu-agent/server-client';

function date(value: string) {
  return new Date(value).toLocaleDateString('zh-CN');
}

export function ApiKeyAdmin({ token, active }: { token: string; active: boolean }) {
  const [users, setUsers] = useState<ServerUser[]>([]);
  const [teams, setTeams] = useState<ServerTeam[]>([]);
  const [keys, setKeys] = useState<ServerAPIKey[]>([]);
  const [permissions, setPermissions] = useState<RolePermission[]>([]);
  const [newEmail, setNewEmail] = useState('');
  const [newName, setNewName] = useState('');
  const [newPassword, setNewPassword] = useState('');
  const [newRole, setNewRole] = useState<ServerUser['role']>('member');
  const [newTeamId, setNewTeamId] = useState('');
  const [teamName, setTeamName] = useState('');
  const [userId, setUserId] = useState('');
  const [memberPassword, setMemberPassword] = useState('');
  const [passwordMessage, setPasswordMessage] = useState('');
  const [name, setName] = useState('元朴中心');
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
      const [members, records, grants, teamItems] = await Promise.all([api.listUsers({ signal }), api.listAPIKeys({ signal }), api.listRolePermissions({ signal }), api.listTeams({ signal })]);
      if (signal?.aborted) return;
      setUsers(members);
      setKeys(records);
      setPermissions(grants);
      setTeams(teamItems);
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

  async function createMember(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault(); setSubmitting(true); setError('');
    try {
      const result = await client.createUser({ email: newEmail.trim(), name: newName.trim(), role: newRole, teamId: newTeamId || null });
      if (newPassword) await client.setUserPassword(result.user.id, newPassword);
      setNewEmail(''); setNewName(''); setNewPassword(''); setNewTeamId(''); await refresh(); setUserId(result.user.id);
    } catch { setError('创建成员失败，请检查邮箱、角色及团队设置。'); }
    finally { setSubmitting(false); }
  }

  async function setPassword(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault(); setSubmitting(true); setError(''); setPasswordMessage('');
    try {
      await client.setUserPassword(userId, memberPassword);
      setMemberPassword(''); setPasswordMessage('密码已更新，旧的密码会话已失效。');
    } catch { setError('设置密码失败，请检查成员状态和密码长度。'); }
    finally { setSubmitting(false); }
  }

  async function changePermission(item: RolePermission, scope: RolePermission['scope']) {
    setError('');
    try { await client.updateRolePermission(item.role, item.action, scope); await refresh(); }
    catch { setError('权限更新失败，请稍后重试。'); }
  }

  async function createTeam(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault(); setError('');
    try { await client.createTeam(teamName.trim()); setTeamName(''); await refresh(); }
    catch { setError('创建团队失败，请检查名称是否重复。'); }
  }

  async function updateMember(member: ServerUser, next: { role?: ServerUser['role']; teamId?: string | null; active?: boolean }) {
    setError('');
    try { await client.updateUser(member.id, { role: next.role ?? member.role, teamId: next.teamId === undefined ? member.teamId : next.teamId, active: next.active ?? member.active }); await refresh(); }
    catch { setError('成员更新失败；最后一名有效管理员不能被停用或降级。'); }
  }

  return <section className="portal-admin" id="admin" aria-label="元朴中心后台管理">
    <div className="portal-section-heading"><div><p className="portal-eyebrow">ADMINISTRATION</p><h2>后台管理</h2>
      <p>管理成员账号、角色权限和桌面 API key。</p></div></div>
    <div className="portal-admin-grid"><div className="portal-admin-panel"><h3>新增成员</h3>
      <form onSubmit={(event) => void createMember(event)}>
        <label>姓名<input value={newName} onChange={(event) => setNewName(event.target.value)} required /></label>
        <label>邮箱<input type="email" value={newEmail} onChange={(event) => setNewEmail(event.target.value)} required /></label>
        <label>初始密码（可选）<input type="password" autoComplete="new-password" minLength={8} value={newPassword} onChange={(event) => setNewPassword(event.target.value)} /></label>
        <label>角色<select value={newRole} onChange={(event) => setNewRole(event.target.value as ServerUser['role'])}>
          <option value="member">成员</option><option value="team_lead">团队负责人</option><option value="reviewer">审核人</option><option value="admin">管理员</option>
        </select></label>
        <label>所属团队<select value={newTeamId} onChange={(event) => setNewTeamId(event.target.value)}>
          <option value="">未分配</option>{teams.map((team) => <option key={team.id} value={team.id}>{team.name}</option>)}
        </select></label>
        <button type="submit" disabled={submitting}>创建成员</button>
      </form><p className="portal-admin-empty">有密码的成员可登录网站；桌面端首次登录会自动生成 Key。</p>
    </div><div className="portal-admin-panel"><h3>发放 API key</h3>
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
    <div className="portal-admin-panel"><h3>设置成员密码</h3>
      <form onSubmit={(event) => void setPassword(event)}>
        <label>成员<select value={userId} onChange={(event) => setUserId(event.target.value)} required>
          {users.filter((member) => member.active).map((member) => <option key={member.id} value={member.id}>{member.name} · {member.email}</option>)}
        </select></label>
        <label>新密码<input type="password" autoComplete="new-password" minLength={8} value={memberPassword} onChange={(event) => setMemberPassword(event.target.value)} required /></label>
        <button type="submit" disabled={!userId || submitting}>保存密码</button>
      </form>{passwordMessage && <p role="status">{passwordMessage}</p>}
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
    </div><div className="portal-admin-panel"><h3>团队与成员</h3>
      <form onSubmit={(event) => void createTeam(event)}><label>新增团队<input value={teamName} onChange={(event) => setTeamName(event.target.value)} required /></label><button type="submit">创建团队</button></form>
      <div className="portal-member-list">{users.map((member) => <div key={member.id} className="portal-member-row"><strong>{member.name}<small>{member.email}</small></strong>
        <select aria-label={`${member.name}的角色`} value={member.role} onChange={(event) => void updateMember(member, { role: event.target.value as ServerUser['role'] })}>
          <option value="member">成员</option><option value="team_lead">团队负责人</option><option value="reviewer">审核人</option><option value="admin">管理员</option>
        </select>
        <select aria-label={`${member.name}的团队`} value={member.teamId ?? ''} onChange={(event) => void updateMember(member, { teamId: event.target.value || null })}>
          <option value="">未分配</option>{teams.map((team) => <option key={team.id} value={team.id}>{team.name}</option>)}
        </select>
        <label><input type="checkbox" checked={member.active} onChange={(event) => void updateMember(member, { active: event.target.checked })} />启用</label>
      </div>)}</div>
    </div><div className="portal-admin-panel"><h3>角色权限</h3><p className="portal-admin-empty">修改会立即应用于现有会话。范围：无、本人、所属团队、全部。</p>
      <div className="portal-permissions">{permissions.map((item) => <label key={`${item.role}-${item.action}`}>
        <span>{item.role} · {item.action}</span><select value={item.scope} onChange={(event) => void changePermission(item, event.target.value as RolePermission['scope'])}>
          <option value="none">无</option><option value="own">本人</option><option value="team">团队</option><option value="all">全部</option>
        </select></label>)}</div>
    </div></div>
  </section>;
}

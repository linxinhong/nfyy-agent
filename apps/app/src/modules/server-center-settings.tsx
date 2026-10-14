import { useEffect, useState } from 'react';
import type { ServerCenterCredentialStatus } from '@yuanpu-agent/protocol';

export function ServerCenterSettingsPanel({ active }: { active: boolean }) {
  const [status, setStatus] = useState<ServerCenterCredentialStatus | null>(null);
  const [key, setKey] = useState('');
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const [error, setError] = useState('');

  useEffect(() => {
    if (!active || !window.yuanpu) return;
    void window.yuanpu.getServerCenterCredentialStatus().then(setStatus)
      .catch(() => setError('无法读取元朴中心凭据配置。'));
  }, [active]);

  async function save(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!window.yuanpu) return;
    setBusy(true);
    setError('');
    setMessage('');
    try {
      const result = await window.yuanpu.saveServerCenterAPIKey(key.trim());
      if (!result.ok) {
        setError(result.code === 'UNAUTHORIZED' ? 'API key 无效、已过期或已撤销。' : '连接中心服务失败，请稍后重试。');
        return;
      }
      setKey('');
      setStatus(await window.yuanpu.getServerCenterCredentialStatus());
      setMessage(`已保存并以 ${result.value.name} 登录。`);
    } catch {
      setError('无法安全保存 API key；请检查系统安全存储是否可用。');
    } finally {
      setBusy(false);
    }
  }

  async function remove() {
    if (!window.yuanpu) return;
    setBusy(true);
    setError('');
    setMessage('');
    try {
      await window.yuanpu.removeServerCenterAPIKey();
      setStatus(await window.yuanpu.getServerCenterCredentialStatus());
      setKey('');
      setMessage('已清除本机 API key。服务端发放记录仍可由管理员撤销。');
    } catch {
      setError('清除本机 API key 失败。');
    } finally {
      setBusy(false);
    }
  }

  return <section className="settings-general server-center-settings" aria-label="元朴中心 API key 配置">
    <h2>元朴中心</h2>
    <p>输入管理员在元朴中心后台发放的 API key。保存后会自动登录，后续会话过期时自动续接。</p>
    <p>当前状态：{status?.configured ? 'API key 已配置' : '未配置 API key'}</p>
    {status && !status.storageAvailable && <p role="alert">系统安全存储不可用，当前设备无法保存 API key。</p>}
    <form onSubmit={(event) => void save(event)}><label htmlFor="server-center-api-key">API key</label>
      <div className="server-center-settings-row"><input id="server-center-api-key" type="password" value={key}
        onChange={(event) => setKey(event.target.value)} placeholder={status?.configured ? '输入新 Key 可替换' : '粘贴 ypk_ 开头的 Key'}
        autoComplete="off" spellCheck={false} /><button type="submit" disabled={!key.trim() || busy || status?.storageAvailable === false}>保存并连接</button></div>
    </form>
    {status?.configured && <button className="server-center-settings-remove" type="button" onClick={() => void remove()} disabled={busy}>清除本机 Key</button>}
    {message && <p role="status">{message}</p>}
    {error && <p role="alert" className="server-center-error">{error}</p>}
    <p>Key 由系统安全存储加密保管；界面不会重新显示已保存的密钥。撤销和发放记录在中心网站“后台管理”中操作。</p>
  </section>;
}

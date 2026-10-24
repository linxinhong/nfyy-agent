import { useEffect, useState } from 'react';
import type { ServerCenterCredentialStatus, ServerCenterStatus } from '@yuanpu-agent/protocol';

export function ServerCenterSettingsPanel({ active }: { active: boolean }) {
  const [endpoint, setEndpoint] = useState('');
  const [savedEndpoint, setSavedEndpoint] = useState('');
  const [connection, setConnection] = useState<ServerCenterStatus | null>(null);
  const [credential, setCredential] = useState<ServerCenterCredentialStatus | null>(null);
  const [key, setKey] = useState('');
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const [error, setError] = useState('');

  useEffect(() => {
    if (!active || !window.yuanpu) return;
    void Promise.all([window.yuanpu.getServerCenterEndpoint(), window.yuanpu.getServerCenterStatus(),
      window.yuanpu.getServerCenterCredentialStatus()])
      .then(([url, current, savedCredential]) => { setEndpoint(url); setSavedEndpoint(url); setConnection(current); setCredential(savedCredential); })
      .catch(() => setError('无法读取知识服务中心配置。'));
  }, [active]);

  async function saveEndpoint(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!window.yuanpu) return;
    setBusy(true);
    setError('');
    setMessage('');
    try {
      const result = await window.yuanpu.saveServerCenterEndpoint(endpoint.trim());
      setEndpoint(result.url);
      setSavedEndpoint(result.url);
      setConnection(await window.yuanpu.getServerCenterStatus().catch(() => null));
      setCredential(await window.yuanpu.getServerCenterCredentialStatus());
      setKey('');
      setMessage(result.keyCleared ? '端点已更新，旧中心凭据已清除。' : '中心端点已保存。');
    } catch {
      setError('中心端点保存失败。请检查地址格式与本机配置文件。');
    } finally {
      setBusy(false);
    }
  }

  async function saveKey(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!window.yuanpu) return;
    setBusy(true);
    setError('');
    setMessage('');
    try {
      const result = await window.yuanpu.saveServerCenterAPIKey(key.trim());
      if (!result.ok) {
        setError(result.code === 'UNAUTHORIZED' ? 'API Key 无效或已撤销。' : '无法验证 API Key，请检查中心连接。');
        return;
      }
      setCredential(await window.yuanpu.getServerCenterCredentialStatus());
      setKey('');
      setMessage(`已保存 ${result.value.name} 的 API Key。`);
    } catch { setError('无法安全保存 API Key。'); }
    finally { setBusy(false); }
  }

  async function removeKey() {
    if (!window.yuanpu) return;
    setBusy(true);
    setError('');
    try {
      await window.yuanpu.removeServerCenterAPIKey();
      setCredential(await window.yuanpu.getServerCenterCredentialStatus());
      setMessage('本机 API Key 已清除。');
    } catch { setError('无法清除本机 API Key。'); }
    finally { setBusy(false); }
  }

  return <section className="settings-general server-center-settings" aria-label="知识服务中心配置">
    <h2>知识服务中心</h2>
    <form onSubmit={(event) => void saveEndpoint(event)}><label htmlFor="server-center-endpoint">中心端点</label>
      <div className="server-center-settings-row"><input id="server-center-endpoint" type="url" value={endpoint}
        onChange={(event) => setEndpoint(event.target.value)} placeholder="http://127.0.0.1:8790" spellCheck={false}
        autoComplete="url" /><button type="submit" disabled={!endpoint.trim() || busy || endpoint.trim() === savedEndpoint}>保存端点</button></div>
    </form>
    <p className="server-center-settings-endpoint-state">{connection?.state === 'ready' ? '已就绪'
      : connection?.state === 'running' ? '服务运行中，依赖尚未就绪'
        : connection?.state === 'incompatible' ? '服务不兼容' : '未连接'}</p>
    <form onSubmit={(event) => void saveKey(event)}><label htmlFor="knowledge-center-api-key">API Key</label>
      <div className="server-center-settings-row"><input id="knowledge-center-api-key" type="password" value={key}
        onChange={(event) => setKey(event.target.value)} placeholder="agk_live_…" autoComplete="off" spellCheck={false} />
        <button type="submit" disabled={!key.trim() || busy || endpoint.trim() !== savedEndpoint || credential?.storageAvailable === false}>保存 Key</button></div>
    </form>
    {credential?.configured && <button className="server-center-settings-remove" type="button" disabled={busy}
      onClick={() => void removeKey()}>清除本机 Key</button>}
    {credential && !credential.storageAvailable && <p role="alert">系统安全存储不可用，无法保存 API Key。</p>}
    {message && <p role="status">{message}</p>}
    {error && <p role="alert" className="server-center-error">{error}</p>}
    <p>网站使用成员账号登录；API Key 可在网站“我的 API Key”创建，本机加密保存。</p>
  </section>;
}

import { useState, type FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import type { SaveTailscaleConnectionInput, TailscaleConnectionSettings, TailscaleNodeStatus } from '@yuanpu-agent/protocol';

function message(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

const stateLabels: Record<TailscaleNodeStatus['state'], string> = {
  unavailable: '此版本未内置 Tailscale（仅 macOS 安装包提供）',
  stopped: '未连接',
  starting: '正在加入 Tailnet…',
  connected: '已连接',
  'needs-login': '需要有效的 Auth Key',
  error: '连接失败',
};

function NodeStatus({ node }: { node: TailscaleNodeStatus }) {
  return <div className={`tailscale-node tailscale-node-${node.state}`} role="status" aria-live="polite">
    <strong>{stateLabels[node.state]}</strong>
    {node.state === 'connected' && <span>
      {[node.dnsName, node.ipv4, node.tailnet && `Tailnet：${node.tailnet}`].filter(Boolean).join(' · ')}
    </span>}
    {node.message && <span>{node.message}</span>}
  </div>;
}

export function TailscaleSettingsPanel({ active, configRoot }: { active: boolean; configRoot: string }) {
  const desktop = window.yuanpu;
  const client = useQueryClient();
  const query = useQuery({
    queryKey: ['connections', 'tailscale'],
    queryFn: () => desktop!.getTailscaleConnection(),
    enabled: active && Boolean(desktop),
    // Joining runs in the background after saving; poll until it settles.
    refetchInterval: (current) => (current.state.data?.node.state === 'starting' ? 2_000 : false),
  });
  const [authKey, setAuthKey] = useState('');
  const [removeAuthKey, setRemoveAuthKey] = useState(false);
  const [notice, setNotice] = useState('');
  const save = useMutation({
    mutationFn: (input: SaveTailscaleConnectionInput) => desktop!.saveTailscaleConnection(input),
    onSuccess: (settings: TailscaleConnectionSettings) => {
      client.setQueryData(['connections', 'tailscale'], settings);
      setAuthKey('');
      setRemoveAuthKey(false);
      setNotice('Tailscale 配置已保存。');
    },
    onError: () => setNotice(''),
  });

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setNotice('');
    save.mutate({
      ...(authKey ? { authKey } : {}),
      ...(removeAuthKey ? { removeAuthKey: true } : {}),
    });
  }

  return <section className="tailscale-settings" aria-label="Tailscale 连接配置">
    <header><h2>Tailscale</h2><p>输入一次 Auth Key，南医小助就会以独立设备身份加入 Tailnet，无需安装 Tailscale。只有 Tailnet 内的地址（100.x、*.ts.net、子网路由）经由该连接，其他网络访问不受影响。</p></header>
    {query.data && <NodeStatus node={query.data.node} />}
    {!desktop && <p role="status">请在桌面应用中配置 Tailscale。</p>}
    {query.isLoading && <p role="status">正在读取配置…</p>}
    {query.error && <p role="alert">读取失败：{message(query.error)}</p>}
    <form onSubmit={submit}>
      <label>设备 Auth Key <span>{query.data?.hasAuthKey ? '已配置 · 留空保持原值' : '尚未配置'}</span>
        <input type="password" value={authKey} onChange={(event) => { setAuthKey(event.target.value); setRemoveAuthKey(false); }}
          placeholder="tskey-auth-…" autoComplete="new-password" spellCheck={false} />
      </label>
      {query.data?.hasAuthKey && <label className="tailscale-remove"><input type="checkbox" checked={removeAuthKey}
        onChange={(event) => { setRemoveAuthKey(event.target.checked); if (event.target.checked) setAuthKey(''); }} />移除设备 Auth Key</label>}
      <p className="tailscale-file-note">保存在 <code>{configRoot}/app/connections/tailscale.json</code>。密钥只写入本机配置文件，界面不回显。建议在 Tailscale 后台创建可复用、带标签、预先批准的 Auth Key；加入后设备凭据保存在本机，重启无需再次使用密钥。</p>
      {save.error && <p role="alert">保存失败：{message(save.error)}</p>}
      {notice && <p role="status">{notice}</p>}
      <button type="submit" disabled={!desktop || save.isPending || query.isLoading || (!authKey && !removeAuthKey)}>{save.isPending ? '保存中…' : '保存配置'}</button>
    </form>
  </section>;
}

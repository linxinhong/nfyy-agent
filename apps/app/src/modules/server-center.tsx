import { useCallback, useEffect, useState } from 'react';

const WebView = 'webview' as unknown as React.FC<Record<string, unknown>>;

/** The center website owns its UI. The desktop only hosts a sandboxed guest. */
export function ServerCenterPage({ active }: { active: boolean }) {
  const [url, setUrl] = useState('');
  const [generation, setGeneration] = useState(0);
  const [error, setError] = useState('');

  const refresh = useCallback(async (reload = false) => {
    if (!window.yuanpu) { setError('请在桌面应用中查看知识服务中心。'); return; }
    setError('');
    try {
      setUrl(await window.yuanpu.getServerCenterEndpoint());
      if (reload) setGeneration((value) => value + 1);
    } catch { setError('中心服务状态暂时无法读取。'); }
  }, []);

  useEffect(() => { if (active) void refresh(true); }, [active, refresh]);

  return <section className={`server-center-web-page ${active ? '' : 'view-hidden'}`} aria-hidden={!active}>
    {error ? <p className="server-center-web-error" role="alert">{error}</p> : url && <WebView key={`${url}:${generation}`}
      className="server-center-webview" src={url} partition="yuanpu-center" />}
  </section>;
}

import type { WordEditorDocument } from './types.js';
export type { WordEditorDocument, WordEditorMode, WordEditorStatus } from './types.js';

export interface WordEditorFrameOptions {
  locale?: 'zh-CN' | 'en-US';
  theme?: 'light' | 'dark';
  /** The iframe shell loaded; poll host.status() to establish document readiness. */
  onLoad?: () => void;
  onDirtyChange?: (dirty: boolean) => void;
  onSaveRequest?: () => void;
}

/** Browser-only entry point. Node and filesystem APIs are never included here. */
export function mountWordEditor(container: HTMLElement, document: WordEditorDocument, options: WordEditorFrameOptions = {}) {
  if (document.load !== 'frame') throw new Error('Open with load: frame before mounting the editor UI.');
  const url = new URL(document.url);
  if (url.protocol !== 'http:' || url.hostname !== '127.0.0.1' || url.origin !== document.origin
    || url.pathname !== '/static/doc/pc.html' || url.username || url.password) {
    throw new Error('An owned loopback Word editor URL is required.');
  }
  const page = container.ownerDocument;
  const view = page.defaultView;
  if (!view) throw new Error('A browser document is required.');
  url.searchParams.set('lng', options.locale ?? 'zh-CN');
  if (options.theme) {
    for (const key of ['theme', 'colorScheme', 'workbuddyTheme']) url.searchParams.set(key, options.theme);
  }
  const iframe = page.createElement('iframe');
  iframe.src = url.toString();
  iframe.title = `${document.title} — Word editor`;
  iframe.referrerPolicy = 'no-referrer';
  iframe.setAttribute('sandbox', 'allow-scripts allow-same-origin');
  iframe.setAttribute('allow', 'clipboard-read; clipboard-write');
  Object.assign(iframe.style, { display: 'block', width: '100%', height: '100%', border: '0' });
  const onMessage = (event: MessageEvent) => {
    if (event.origin !== document.origin || event.source !== iframe.contentWindow
      || !event.data || typeof event.data !== 'object') return;
    if (event.data.type === 'workbuddy:document-status-changed' && typeof event.data.hasUnSavedChange === 'boolean') {
      options.onDirtyChange?.(event.data.hasUnSavedChange);
    }
    if (event.data.type === 'workbuddy:tencent-docs-save-shortcut' && document.mode === 'edit') options.onSaveRequest?.();
  };
  view.addEventListener('message', onMessage);
  const onLoad = () => options.onLoad?.();
  iframe.addEventListener('load', onLoad, { once: true });
  container.appendChild(iframe);
  return { iframe, destroy() {
    view.removeEventListener('message', onMessage);
    iframe.removeEventListener('load', onLoad);
    iframe.remove();
  } };
}

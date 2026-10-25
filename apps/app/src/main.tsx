import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { HashRouter } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { App } from './shell/app-shell.js';
import { brandIconUrl, emo } from './emo.js';
import { applyThemePreference, readThemePreference } from './shared/theme-preference.js';
import { applyFontPreference, readFontPreference } from './shared/font-preference.js';
import { installAutoFadingScrollbars } from './shared/auto-fading-scrollbars.js';
import '../themes/tokens.css';
import './styles.css';
import './muse-theme.css';
import '../themes/mindlink.css';
import '../themes/dark.css';
import './shell/shell-layout.css';
import './scrollbars.css';

applyThemePreference(readThemePreference());
applyFontPreference(readFontPreference());
installAutoFadingScrollbars();
document.title = emo.brand.name;
const favicon = document.createElement('link');
favicon.rel = 'icon';
const faviconUrl = brandIconUrl();
if (faviconUrl) {
  favicon.href = faviconUrl;
  document.head.append(favicon);
}
document.documentElement.dataset.yuanpuDesktop = window.yuanpu ? 'true' : 'false';
const queryClient = new QueryClient({ defaultOptions: { queries: { retry: 1, refetchOnWindowFocus: false }, mutations: { retry: false } } });
createRoot(document.getElementById('root')!).render(
  <StrictMode><HashRouter><QueryClientProvider client={queryClient}><App /></QueryClientProvider></HashRouter></StrictMode>,
);

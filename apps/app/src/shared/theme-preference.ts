export type RendererTheme = 'yuanpu-light' | 'yuanpu-dark' | 'mindlink';

const storageKey = 'yuanpu:theme:v1';

/** The warm theme is the default; an explicit light or dark choice is kept. */
export const defaultTheme: RendererTheme = 'mindlink';

export function readThemePreference(): RendererTheme {
  try {
    const stored = window.localStorage.getItem(storageKey);
    return stored === 'yuanpu-light' || stored === 'yuanpu-dark' || stored === 'mindlink' ? stored : defaultTheme;
  } catch {
    return defaultTheme;
  }
}

export function applyThemePreference(theme: RendererTheme): void {
  document.documentElement.dataset.yuanpuTheme = theme;
  try {
    window.localStorage.setItem(storageKey, theme);
  } catch {
    // The theme still applies for this session when storage is unavailable.
  }
}

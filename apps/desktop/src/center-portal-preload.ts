import { contextBridge, ipcRenderer } from 'electron';

// Available only in the center's isolated webview. The host validates the
// guest origin again before accepting a generated key.
contextBridge.exposeInMainWorld('yuanpuPortal', {
  restore: () => ipcRenderer.invoke('server:center:portal-restore'),
  request: (input: { path: string; method?: string; body?: string; workspace?: string; binary?: boolean }) =>
    ipcRenderer.invoke('server:center:portal-request', input),
  saveAPIKey: (key: string) => ipcRenderer.invoke('server:center:portal-save-api-key', key),
  clearAPIKey: () => ipcRenderer.invoke('server:center:portal-clear-api-key'),
});

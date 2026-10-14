import { randomBytes } from 'node:crypto';
import { existsSync, readFileSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';

import { app, BrowserWindow, dialog, ipcMain, Notification, safeStorage, shell, type IpcMainInvokeEvent } from 'electron';
import { autoUpdater } from 'electron-updater';
import type { NotificationNavigationTarget, RuntimeRecoveryNotice } from '@yuanpu-agent/protocol';

import { BrowserGuestManager } from './browser-guest-manager.js';
import { startBrowserControlService } from './browser-control-service.js';
import { RuntimeManager } from './runtime-manager.js';
import { ElectronNotificationHost, type NativeNotification } from './notification-host.js';
import { isTrustedRendererUrl, packagedRendererUrl } from './renderer-security.js';
import { defaultServerUrl, getServerCenterStatus, getServerWikiPreview, ServerCenterSession } from './server-center.js';
import { ServerCenterCredentialStore } from './server-center-credential.js';

let runtime: RuntimeManager;
let mainWindow: BrowserWindow | undefined;
let trustedRendererEntry = '';
let quitInProgress = false;
let quitAllowed = false;
let notificationHost: ElectronNotificationHost | undefined;
let pendingNotificationTarget: NotificationNavigationTarget | undefined;
let notificationsEnabled = true;
let runtimeRecoveryNotice: RuntimeRecoveryNotice | undefined;

function readEmoBrand(): { name: string; icon: string } {
  const appRoot = app.isPackaged ? join(process.resourcesPath, 'app') : resolve(__dirname, '../../app');
  const iconRoot = app.isPackaged ? appRoot : join(appRoot, 'src/assets');
  try {
    const config = JSON.parse(readFileSync(join(appRoot, 'emo.json'), 'utf8')) as {
      brand?: { name?: unknown; icon?: unknown };
    };
    const name = typeof config.brand?.name === 'string' && config.brand.name.trim()
      ? config.brand.name : 'Yuanpu Agent';
    const icon = typeof config.brand?.icon === 'string' ? resolve(iconRoot, config.brand.icon) : '';
    return { name, icon: icon.startsWith(`${iconRoot}${sep}`) && existsSync(icon) ? icon : '' };
  } catch {
    return { name: 'Yuanpu Agent', icon: '' };
  }
}

const emoBrand = readEmoBrand();

function flushNotificationNavigation(): void {
  if (!mainWindow || mainWindow.webContents.isLoadingMainFrame() || !pendingNotificationTarget) return;
  mainWindow.webContents.send('notifications:navigate', pendingNotificationTarget);
  pendingNotificationTarget = undefined;
}

function focusMainWindow(): void {
  if (!mainWindow) createWindow();
  if (!mainWindow) return;
  if (mainWindow.isMinimized()) mainWindow.restore();
  mainWindow.show();
  mainWindow.focus();
}

function assertTrustedRenderer(event: IpcMainInvokeEvent): void {
  if (
    !mainWindow
    || event.sender !== mainWindow.webContents
    || event.senderFrame !== mainWindow.webContents.mainFrame
    || !isTrustedRendererUrl(event.senderFrame.url, trustedRendererEntry)
  ) {
    throw new Error('Rejected IPC from an untrusted renderer frame');
  }
}

function trustedHandler(listener: (...args: any[]) => unknown) {
  return (event: IpcMainInvokeEvent, ...args: any[]) => {
    assertTrustedRenderer(event);
    return listener(...args);
  };
}

function trustedHandlerWithEvent(listener: (event: IpcMainInvokeEvent, ...args: any[]) => unknown) {
  return (event: IpcMainInvokeEvent, ...args: any[]) => {
    assertTrustedRenderer(event);
    return listener(event, ...args);
  };
}

function createWindow(): void {
  const window = new BrowserWindow({
    width: 1120,
    height: 760,
    minWidth: 800,
    minHeight: 560,
    backgroundColor: '#ffffff',
    titleBarStyle: process.platform === 'darwin' ? 'hiddenInset' : 'default',
    title: emoBrand.name,
    ...(emoBrand.icon ? { icon: emoBrand.icon } : {}),
    webPreferences: {
      preload: join(__dirname, 'preload.cjs'),
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
      webviewTag: true,
    },
  });
  mainWindow = window;
  window.webContents.on('did-finish-load', flushNotificationNavigation);
  window.once('closed', () => {
    if (mainWindow === window) mainWindow = undefined;
  });
  window.webContents.setWindowOpenHandler(() => ({ action: 'deny' }));
  // Embedded-browser guests are hardened here instead of banned outright:
  // force the guest sandbox, strip privileged attributes and only allow
  // http(s) sources (zcode's will-attach-webview hardening).
  window.webContents.on('will-attach-webview', (event, webPreferences, params) => {
    webPreferences.nodeIntegration = false;
    webPreferences.contextIsolation = true;
    webPreferences.sandbox = true;
    delete webPreferences.preload;
    delete (params as Record<string, unknown>).nodeintegration;
    delete (params as Record<string, unknown>).disablewebsecurity;
    if (!/^https?:\/\//i.test(params.src ?? '') && params.src !== 'about:blank') {
      event.preventDefault();
    }
  });
  window.webContents.on('will-navigate', (event, targetUrl) => {
    if (!isTrustedRendererUrl(targetUrl, trustedRendererEntry)) event.preventDefault();
  });

  if (!app.isPackaged && process.env.YUANPU_RENDERER_URL) {
    trustedRendererEntry = process.env.YUANPU_RENDERER_URL;
    void window.loadURL(trustedRendererEntry);
  } else {
    const rendererFile = join(process.resourcesPath, 'app', 'index.html');
    trustedRendererEntry = packagedRendererUrl(rendererFile);
    void window.loadFile(rendererFile);
  }
}

const hasSingleInstanceLock = app.requestSingleInstanceLock();
if (!hasSingleInstanceLock) app.quit();

app.on('second-instance', () => {
  if (!mainWindow) return;
  if (mainWindow.isMinimized()) mainWindow.restore();
  mainWindow.show();
  mainWindow.focus();
});

if (hasSingleInstanceLock) void app.whenReady().then(async () => {
  app.setName(emoBrand.name);
  if (process.platform === 'darwin' && emoBrand.icon) app.dock?.setIcon(emoBrand.icon);
  const browserGuestManager = new BrowserGuestManager({
    onGuestCrashed: (windowId, guestKey) => {
      BrowserWindow.fromId(windowId)?.webContents.send('browser:guest-crashed', guestKey);
    },
    onSessionRequest: (windowId, conversationId) => {
      BrowserWindow.fromId(windowId)?.webContents.send('browser:session-request', conversationId);
    },
    getMainWindowId: () => mainWindow?.id,
  });
  const browserControlToken = randomBytes(32).toString('hex');
  const browserControlService = await startBrowserControlService({
    manager: browserGuestManager,
    token: browserControlToken,
  });
  runtime = new RuntimeManager(
    app.getAppPath(),
    process.resourcesPath,
    app.getPath('userData'),
    app.isPackaged,
    app.getVersion(),
    {
      onUpdateRecovery: (kind) => { runtimeRecoveryNotice = { kind }; },
      browserControl: { port: browserControlService.port, token: browserControlToken },
    },
  );

  ipcMain.handle('runtime:info', trustedHandler(() => runtime.info()));
  const serverUrl = process.env.YUANPU_SERVER_URL || defaultServerUrl;
  const serverCenterCredentials = new ServerCenterCredentialStore(join(app.getPath('userData'), 'server-center-credential.json'), safeStorage);
  const serverCenterSession = new ServerCenterSession(serverUrl, () => serverCenterCredentials.loadKey());
  ipcMain.handle('server:center:status', trustedHandler(() => getServerCenterStatus(serverUrl)));
  ipcMain.handle('server:center:wiki-preview', trustedHandler(() => getServerWikiPreview(serverUrl)));
  ipcMain.handle('server:center:catalog', trustedHandler((query: string) => serverCenterSession.searchCatalog(query)));
  ipcMain.handle('server:center:restore', trustedHandler(() => serverCenterSession.restore()));
  ipcMain.handle('server:center:logout', trustedHandler(async () => {
    await serverCenterSession.logout();
    await serverCenterCredentials.removeKey();
  }));
  ipcMain.handle('server:center:credential-status', trustedHandler(() => serverCenterCredentials.status()));
  ipcMain.handle('server:center:save-api-key', trustedHandler(async (apiKey: string) => {
    const result = await serverCenterSession.useAPIKey(apiKey);
    if (result.ok) {
      try { await serverCenterCredentials.saveKey(apiKey); }
      catch (error) { await serverCenterSession.logout(); throw error; }
    }
    return result;
  }));
  ipcMain.handle('server:center:remove-api-key', trustedHandler(async () => {
    await serverCenterSession.logout();
    await serverCenterCredentials.removeKey();
  }));
  ipcMain.handle('server:center:resources', trustedHandler((query: string) => serverCenterSession.listResources(query)));
  ipcMain.handle('server:center:resource', trustedHandler((id: string) => serverCenterSession.getResource(id)));
  ipcMain.handle('settings:hotkeys:get', trustedHandler(() => runtime.getHotkeySettings()));
  ipcMain.handle('settings:hotkeys:save', trustedHandler((input) => runtime.saveHotkeySetting(input)));
  ipcMain.handle('settings:models:get', trustedHandler(() => runtime.getModelSettings()));
  ipcMain.handle('settings:models:catalog', trustedHandler((provider) => runtime.getModelCatalog(provider)));
  ipcMain.handle('settings:models:save', trustedHandler((input) => runtime.saveModelSettings(input)));
  ipcMain.handle('settings:models:delete', trustedHandler((provider, model) => runtime.deleteModelSettings(provider, model)));
  ipcMain.handle('runtime:recovery-notice', trustedHandler(() => runtimeRecoveryNotice));
  ipcMain.handle('runtime:greeting', trustedHandler((name: string) => runtime.greeting(name)));
  ipcMain.handle('runtime:chat', trustedHandler((message: string) => runtime.chat(message)));
  ipcMain.handle('runtime:chat:submit', trustedHandler((message: string, surface?: 'work' | 'assistant', conversationId?: string,
    clientMessageId?: string, modelSelection?: import('@yuanpu-agent/protocol').AgentRunRequest['modelSelection'],
    approvalMode?: import('@yuanpu-agent/protocol').AgentRunRequest['approvalMode']) =>
    runtime.submitDesktopMessage(message, surface, conversationId, clientMessageId, modelSelection, approvalMode)));
  ipcMain.handle('desktop:canvas:submit', trustedHandler((message: string,
    widgets: import('@yuanpu-agent/protocol').DesktopCanvasWidgetSummary[]) => runtime.submitDesktopCanvasMessage(message, widgets)));
  ipcMain.handle('desktop:transcript', trustedHandler((surface: 'work' | 'assistant' | 'assistantArchive', conversationId?: string,
    beforeId?: string, limit?: number) => runtime.getDesktopTranscript(surface, conversationId, beforeId, limit)));
  ipcMain.handle('work:conversations:list', trustedHandler(() => runtime.listWorkConversations()));
  ipcMain.handle('work:conversations:create', trustedHandler((folderId?: string, requestId?: string) => runtime.createWorkConversation(folderId, requestId)));
  ipcMain.handle('work:conversations:select', trustedHandler((conversationId: string, previewArchived?: boolean) => runtime.selectWorkConversation(conversationId, previewArchived)));
  ipcMain.handle('work:conversations:update', trustedHandler((conversationId: string, patch: { title?: string; iconId?: string; archived?: boolean; tagIds?: string[] }) => runtime.updateWorkConversation(conversationId, patch)));
  ipcMain.handle('work:move', trustedHandler((request: import('@yuanpu-agent/protocol').WorkMoveRequest) => runtime.moveWorkNode(request)));
  ipcMain.handle('work:search', trustedHandler((input: import('@yuanpu-agent/protocol').WorkSearchQuery) => runtime.searchWorkConversations(input)));
  ipcMain.handle('work:message-window', trustedHandler((conversationId: string, entryId: string, radius?: number) =>
    runtime.getWorkMessageWindow(conversationId, entryId, radius)));
  ipcMain.handle('work:trajectory', trustedHandler((conversationId: string) => runtime.getWorkTrajectory(conversationId)));
  ipcMain.handle('work:folders:list', trustedHandler(() => runtime.listWorkFolders()));
  ipcMain.handle('work:folders:create', trustedHandler((parentId: string | null, name: string, iconId?: string, requestId?: string) => runtime.createWorkFolder(parentId, name, iconId, requestId)));
  ipcMain.handle('work:folders:update', trustedHandler((folderId: string, patch: { name?: string; iconId?: string }) => runtime.updateWorkFolder(folderId, patch)));
  ipcMain.handle('work:tags:list', trustedHandler(() => runtime.listWorkTags()));
  ipcMain.handle('work:tags:create', trustedHandler((name: string, color?: string, requestId?: string) => runtime.createWorkTag(name, color, requestId)));
  ipcMain.handle('work:tags:update', trustedHandler((tagId: string, patch: { name?: string; color?: string }) => runtime.updateWorkTag(tagId, patch)));
  ipcMain.handle('work:order', trustedHandler((kind: 'folder' | 'conversation', parentId: string | null, ids: string[]) => runtime.reorderWorkSiblings(kind, parentId, ids)));
  ipcMain.handle('work:files:list', trustedHandler((conversationId: string, dirPath?: string, options?: { recursive?: boolean }) => runtime.listWorkFiles(conversationId, dirPath, options)));
  ipcMain.handle('work:files:read', trustedHandler((conversationId: string, filePath: string) => runtime.readWorkFile(conversationId, filePath)));
  ipcMain.handle('work:files:open', trustedHandler(async (conversationId: string, filePath: string) => {
    const resolved = await runtime.resolveWorkFilePath(conversationId, filePath);
    const error = await shell.openPath(resolved.path);
    if (error) throw new Error(error);
  }));
  ipcMain.handle('work:file-changes:list', trustedHandler((query: { conversationId: string; runId?: string }) => runtime.listWorkFileChanges(query)));
  ipcMain.handle('browser:attach-guest', trustedHandlerWithEvent((event, payload: { key: string; webContentsId: number; conversationId: string }) => {
    const windowId = BrowserWindow.fromWebContents(event.sender)?.id;
    if (windowId === undefined) throw new Error('Browser guest attach requires a known window.');
    return browserGuestManager.attachGuest({ ...payload, windowId, hostWebContentsId: event.sender.id });
  }));
  ipcMain.handle('browser:detach-guest', trustedHandler((key: string) => browserGuestManager.detachGuest(key)));
  ipcMain.handle('browser:open-external', trustedHandler((url: string) => {
    if (!/^https?:\/\//i.test(url)) throw new Error('Only http(s) URLs can be opened externally.');
    return shell.openExternal(url);
  }));
  ipcMain.handle('assistant:link:get', trustedHandler(() => runtime.getAssistantLink()));
  ipcMain.handle('assistant:link:bind', trustedHandler((contactId: string) => runtime.bindAssistantContact(contactId)));
  ipcMain.handle('assistant:link:unbind', trustedHandler(() => runtime.unbindAssistantContact()));
  ipcMain.handle('assistant:suggestions:list', trustedHandler(() => runtime.listAssistantSuggestions()));
  ipcMain.handle('assistant:suggestions:feedback', trustedHandler((id: string,
    action: 'ignored' | 'snoozed' | 'accepted', snoozedUntil?: string) =>
    runtime.feedbackAssistantSuggestion(id, action, snoozedUntil)));
  ipcMain.handle('assistant:suggestions:pause', trustedHandler((until?: string) =>
    runtime.setAssistantSuggestionsPaused(until)));
  ipcMain.handle('assistant:suggestions:read', trustedHandler((id: string) =>
    runtime.markAssistantSuggestionRead(id)));
  ipcMain.handle('assistant:workspace:get', trustedHandler((_event, memoryLimit?: number) =>
    runtime.getAssistantWorkspace(memoryLimit)));
  ipcMain.handle('assistant:source:revoke', trustedHandler((sourceId: string, expectedVersion: string) =>
    runtime.revokeAssistantSource(sourceId, expectedVersion)));
  ipcMain.handle('assistant:memory:correct', trustedHandler((id: string, expectedVersion: number,
    text: string, revisionId: string) => runtime.correctAssistantMemory(id, expectedVersion, text, revisionId)));
  ipcMain.handle('assistant:memory:forget', trustedHandler((id: string) => runtime.forgetAssistantMemory(id)));
  ipcMain.handle('assistant:memory:import-saved', trustedHandler((savedId: string,
    surface: 'work' | 'assistant', text: string, savedAt: string) =>
    runtime.importAssistantSavedMemory(savedId, surface, text, savedAt)));
  ipcMain.handle('assistant:organizing:pause', trustedHandler((until?: string) =>
    runtime.setAssistantOrganizingPaused(until)));
  ipcMain.handle('assistant:delegation:follow-up', trustedHandler((taskId: string, text: string) =>
    runtime.followUpAssistantDelegation(taskId, text)));
  ipcMain.handle('assistant:delegation:cancel', trustedHandler((taskId: string) =>
    runtime.cancelAssistantDelegation(taskId)));
  ipcMain.handle('assistant:mirrors:list', trustedHandler((runId: string) => runtime.listAssistantMirrors(runId)));
  ipcMain.handle('assistant:mirrors:retry', trustedHandler((mirrorId: string) => runtime.retryAssistantMirror(mirrorId)));
  ipcMain.handle('agent:runs:get', trustedHandler((runId: string) => runtime.getAgentRun(runId)));
  ipcMain.handle('im:private-runs:summary', trustedHandler((runId: string) => runtime.getPrivateImRunSummary(runId)));
  ipcMain.handle('agent:runs:cancel', trustedHandler((runId: string) => runtime.cancelAgentRun(runId)));
  ipcMain.handle('schedules:list', trustedHandler(() => runtime.listSchedules()));
  ipcMain.handle('schedules:create', trustedHandler((input) => runtime.createSchedule(input)));
  ipcMain.handle('schedules:preview', trustedHandler((input) => runtime.previewSchedule(input)));
  ipcMain.handle('schedules:update', trustedHandler((scheduleId, input) => runtime.updateSchedule(scheduleId, input)));
  ipcMain.handle('schedules:enabled', trustedHandler((scheduleId, enabled) => (
    runtime.setScheduleEnabled(scheduleId, enabled)
  )));
  ipcMain.handle('schedules:history', trustedHandler((scheduleId, limit) => (
    runtime.getScheduleHistory(scheduleId, limit)
  )));
  ipcMain.handle('schedules:contacts:list', trustedHandler(() => runtime.listSchedulePrivateContacts()));
  ipcMain.handle('schedules:contacts:bind', trustedHandler((contactId) => runtime.bindSchedulePrivateContact(contactId)));
  ipcMain.handle('schedules:targets:revoke', trustedHandler((routeId) => (
    runtime.revokeSchedulePrivateTarget(routeId)
  )));
  ipcMain.handle('connections:wecom:list', trustedHandler(() => runtime.listWecomConnections()));
  ipcMain.handle('connections:wecom:test', trustedHandler((connectionId) => runtime.testWecomConnection(connectionId)));
  ipcMain.handle('connections:wecom:save', trustedHandler((input) => runtime.saveWecomConnection(input)));
  ipcMain.handle('connections:tailscale:get', trustedHandler(() => runtime.getTailscaleConnection()));
  ipcMain.handle('connections:tailscale:save', trustedHandler((input) => runtime.saveTailscaleConnection(input)));
  ipcMain.handle('runtime:update', trustedHandler(() => runtime.checkForUpdate()));
  ipcMain.handle('plugins:search', trustedHandler((query: string) => runtime.searchPlugins(query)));
  ipcMain.handle('plugins:list', trustedHandler(() => runtime.listPlugins()));
  ipcMain.handle('skills:local', trustedHandler(() => runtime.listLocalSkills()));
  ipcMain.handle('plugins:install', trustedHandler((source: string, artifactManifestDigest?: string) => (
    runtime.installPlugin(source, artifactManifestDigest)
  )));
  ipcMain.handle('plugins:state', trustedHandler((name: string, enabled: boolean) => (
    runtime.setPluginEnabled(name, enabled)
  )));
  ipcMain.handle('plugins:uninstall', trustedHandler((name: string) => runtime.uninstallPlugin(name)));
  ipcMain.handle('plugins:config:get', trustedHandler((name, scope) => runtime.getPluginConfig(name, scope)));
  ipcMain.handle('plugins:config:validate', trustedHandler((input) => runtime.validatePluginConfig(input)));
  ipcMain.handle('plugins:config:save', trustedHandler((input) => runtime.savePluginConfig(input)));
  ipcMain.handle('plugins:config:reset', trustedHandler((name, scope) => runtime.resetPluginConfig(name, scope)));
  ipcMain.handle('plugins:rollback', trustedHandler((name: string, version: string) => (
    runtime.rollbackPlugin(name, version)
  )));
  ipcMain.handle('plugins:mcp-conflicts', trustedHandler((source: string, artifactManifestDigest: string) => (
    runtime.listMcpOwnershipConflicts(source, artifactManifestDigest)
  )));
  ipcMain.handle('capabilities:approvals:list', trustedHandler(() => runtime.listCapabilityApprovals()));
  ipcMain.handle('capabilities:approvals:decide', trustedHandler((requestId, decision) => (
    runtime.decideCapabilityApproval(requestId, decision)
  )));
  ipcMain.handle('desktop:update', trustedHandler(async () => {
    if (!app.isPackaged) throw new Error('Desktop updates are only available in packaged builds');
    await autoUpdater.checkForUpdates();
  }));

  app.on('web-contents-created', (event, contents) => {
    if (contents.getType() !== 'webview') return;
    const allowBrowserUrl = (url: string) => /^https?:\/\//i.test(url) || url === 'about:blank';
    contents.on('will-navigate', (navigationEvent, url) => {
      if (!allowBrowserUrl(url)) navigationEvent.preventDefault();
    });
    contents.on('will-redirect', (redirectEvent, url) => {
      if (!allowBrowserUrl(url)) redirectEvent.preventDefault();
    });
    contents.setWindowOpenHandler(({ url }) => {
      // Guest popups are denied; http(s) targets hand off to the system browser.
      if (/^https?:\/\//i.test(url)) void shell.openExternal(url).catch(() => undefined);
      return { action: 'deny' };
    });
  });

  autoUpdater.autoDownload = true;
  autoUpdater.autoInstallOnAppQuit = true;
  autoUpdater.on('error', (error) => console.error('Desktop update failed:', error.message));
  const runtimeInfo = await runtime.start();
  notificationsEnabled = runtimeInfo.notificationsEnabled ?? true;
  notificationHost = new ElectronNotificationHost({
    platform: {
      isSupported: () => Notification.isSupported(),
      // Electron has no cross-platform built-in permission query. A native `failed`
      // event is still reported precisely; injected adapters cover known denial states.
      permissionState: () => 'unknown',
      create: (options) => new Notification(options) as unknown as NativeNotification,
    },
    enabled: () => notificationsEnabled && process.env.YUANPU_NOTIFICATIONS_ENABLED !== '0',
    validateTarget: (target) => runtime.validateNotificationTarget(target),
    focus: focusMainWindow,
    navigate: (target) => {
      pendingNotificationTarget = target;
      focusMainWindow();
      flushNotificationNavigation();
    },
    onError: (error) => console.error('Notification activation failed:', error.message),
  });
  runtime.connectHostEvents((event) => notificationHost!.handle(event));
  createWindow();

  app.on('activate', () => {
    if (BrowserWindow.getAllWindows().length === 0) createWindow();
  });
}).catch((error) => {
  const message = error instanceof Error ? error.message : String(error);
  console.error('Yuanpu Runtime startup failed:', message);
  dialog.showErrorBox('Yuanpu Runtime 无法启动', message);
  app.quit();
});

app.on('before-quit', (event) => {
  if (quitAllowed) return;
  event.preventDefault();
  if (quitInProgress) return;
  quitInProgress = true;
  notificationHost?.stop();
  void Promise.resolve(runtime?.stop()).finally(() => {
    quitAllowed = true;
    app.quit();
  });
});
app.on('window-all-closed', () => {
  if (process.platform !== 'darwin') app.quit();
});

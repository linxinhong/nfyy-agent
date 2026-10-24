import type { DesktopBridge } from '@yuanpu-agent/protocol';
import { contextBridge, ipcRenderer, type IpcRendererEvent } from 'electron';

const bridge: DesktopBridge = {
  runtimeInfo: () => ipcRenderer.invoke('runtime:info'),
  getServerCenterStatus: () => ipcRenderer.invoke('server:center:status'),
  getServerWikiPreview: () => ipcRenderer.invoke('server:center:wiki-preview'),
  searchServerCatalog: (query) => ipcRenderer.invoke('server:center:catalog', query),
  restoreServerCenterSession: () => ipcRenderer.invoke('server:center:restore'),
  logoutServerCenter: () => ipcRenderer.invoke('server:center:logout'),
  getServerCenterCredentialStatus: () => ipcRenderer.invoke('server:center:credential-status'),
  getServerCenterEndpoint: () => ipcRenderer.invoke('server:center:endpoint'),
  saveServerCenterEndpoint: (url) => ipcRenderer.invoke('server:center:save-endpoint', url),
  saveServerCenterAPIKey: (apiKey) => ipcRenderer.invoke('server:center:save-api-key', apiKey),
  removeServerCenterAPIKey: () => ipcRenderer.invoke('server:center:remove-api-key'),
  listServerResources: (query) => ipcRenderer.invoke('server:center:resources', query),
  getServerResource: (id) => ipcRenderer.invoke('server:center:resource', id),
  getHotkeySettings: () => ipcRenderer.invoke('settings:hotkeys:get'),
  saveHotkeySetting: (input) => ipcRenderer.invoke('settings:hotkeys:save', input),
  getModelSettings: () => ipcRenderer.invoke('settings:models:get'),
  getModelCatalog: (provider) => ipcRenderer.invoke('settings:models:catalog', provider),
  saveModelSettings: (input) => ipcRenderer.invoke('settings:models:save', input),
  deleteModelSettings: (provider, model) => ipcRenderer.invoke('settings:models:delete', provider, model),
  runtimeRecoveryNotice: () => ipcRenderer.invoke('runtime:recovery-notice'),
  greeting: (name) => ipcRenderer.invoke('runtime:greeting', name),
  chat: (message) => ipcRenderer.invoke('runtime:chat', message),
  submitDesktopMessage: (message, surface, conversationId, clientMessageId, modelSelection, approvalMode) =>
    ipcRenderer.invoke('runtime:chat:submit', message, surface, conversationId, clientMessageId, modelSelection, approvalMode),
  submitDesktopCanvasMessage: (message, widgets) => ipcRenderer.invoke('desktop:canvas:submit', message, widgets),
  getDesktopTranscript: (surface, conversationId, beforeId, limit) =>
    ipcRenderer.invoke('desktop:transcript', surface, conversationId, beforeId, limit),
  listWorkConversations: () => ipcRenderer.invoke('work:conversations:list'),
  createWorkConversation: (folderId, requestId) => ipcRenderer.invoke('work:conversations:create', folderId, requestId),
  selectWorkConversation: (conversationId, previewArchived) => ipcRenderer.invoke('work:conversations:select', conversationId, previewArchived),
  updateWorkConversation: (conversationId, patch) => ipcRenderer.invoke('work:conversations:update', conversationId, patch),
  moveWorkNode: (request) => ipcRenderer.invoke('work:move', request),
  searchWorkConversations: (input) => ipcRenderer.invoke('work:search', input),
  getWorkMessageWindow: (conversationId, entryId, radius) => ipcRenderer.invoke('work:message-window', conversationId, entryId, radius),
  getWorkTrajectory: (conversationId) => ipcRenderer.invoke('work:trajectory', conversationId),
  listWorkFolders: () => ipcRenderer.invoke('work:folders:list'),
  createWorkFolder: (parentId, name, iconId, requestId) => ipcRenderer.invoke('work:folders:create', parentId, name, iconId, requestId),
  updateWorkFolder: (folderId, patch) => ipcRenderer.invoke('work:folders:update', folderId, patch),
  listWorkTags: () => ipcRenderer.invoke('work:tags:list'),
  createWorkTag: (name, color, requestId) => ipcRenderer.invoke('work:tags:create', name, color, requestId),
  updateWorkTag: (tagId, patch) => ipcRenderer.invoke('work:tags:update', tagId, patch),
  reorderWorkSiblings: (kind, parentId, ids) => ipcRenderer.invoke('work:order', kind, parentId, ids),
  listWorkFiles: (conversationId, dirPath, options) => ipcRenderer.invoke('work:files:list', conversationId, dirPath, options),
  readWorkFile: (conversationId, filePath) => ipcRenderer.invoke('work:files:read', conversationId, filePath),
  openWorkFile: (conversationId, filePath) => ipcRenderer.invoke('work:files:open', conversationId, filePath),
  listWorkFileChanges: (query) => ipcRenderer.invoke('work:file-changes:list', query),
  getAssistantLink: () => ipcRenderer.invoke('assistant:link:get'),
  bindAssistantContact: (contactId) => ipcRenderer.invoke('assistant:link:bind', contactId),
  unbindAssistantContact: () => ipcRenderer.invoke('assistant:link:unbind'),
  listAssistantMirrors: (runId) => ipcRenderer.invoke('assistant:mirrors:list', runId),
  retryAssistantMirror: (mirrorId) => ipcRenderer.invoke('assistant:mirrors:retry', mirrorId),
  listAssistantSuggestions: () => ipcRenderer.invoke('assistant:suggestions:list'),
  feedbackAssistantSuggestion: (id, action, snoozedUntil) =>
    ipcRenderer.invoke('assistant:suggestions:feedback', id, action, snoozedUntil),
  setAssistantSuggestionsPaused: (until) => ipcRenderer.invoke('assistant:suggestions:pause', until),
  markAssistantSuggestionRead: (id) => ipcRenderer.invoke('assistant:suggestions:read', id),
  getAssistantWorkspace: (memoryLimit) => ipcRenderer.invoke('assistant:workspace:get', memoryLimit),
  revokeAssistantSource: (sourceId, expectedVersion) =>
    ipcRenderer.invoke('assistant:source:revoke', sourceId, expectedVersion),
  correctAssistantMemory: (id, expectedVersion, text, revisionId) =>
    ipcRenderer.invoke('assistant:memory:correct', id, expectedVersion, text, revisionId),
  forgetAssistantMemory: (id) => ipcRenderer.invoke('assistant:memory:forget', id),
  importAssistantSavedMemory: (savedId, surface, text, savedAt) =>
    ipcRenderer.invoke('assistant:memory:import-saved', savedId, surface, text, savedAt),
  setAssistantOrganizingPaused: (until) => ipcRenderer.invoke('assistant:organizing:pause', until),
  followUpAssistantDelegation: (taskId, text) =>
    ipcRenderer.invoke('assistant:delegation:follow-up', taskId, text),
  cancelAssistantDelegation: (taskId) => ipcRenderer.invoke('assistant:delegation:cancel', taskId),
  getAgentRun: (runId) => ipcRenderer.invoke('agent:runs:get', runId),
  getPrivateImRunSummary: (runId) => ipcRenderer.invoke('im:private-runs:summary', runId),
  cancelAgentRun: (runId) => ipcRenderer.invoke('agent:runs:cancel', runId),
  listSchedules: () => ipcRenderer.invoke('schedules:list'),
  createSchedule: (input) => ipcRenderer.invoke('schedules:create', input),
  previewSchedule: (input) => ipcRenderer.invoke('schedules:preview', input),
  updateSchedule: (scheduleId, input) => ipcRenderer.invoke('schedules:update', scheduleId, input),
  setScheduleEnabled: (scheduleId, enabled) => ipcRenderer.invoke('schedules:enabled', scheduleId, enabled),
  getScheduleHistory: (scheduleId, limit) => ipcRenderer.invoke('schedules:history', scheduleId, limit),
  listSchedulePrivateContacts: () => ipcRenderer.invoke('schedules:contacts:list'),
  bindSchedulePrivateContact: (contactId) => ipcRenderer.invoke('schedules:contacts:bind', contactId),
  revokeSchedulePrivateTarget: (routeId) => ipcRenderer.invoke('schedules:targets:revoke', routeId),
  listWecomConnections: () => ipcRenderer.invoke('connections:wecom:list'),
  testWecomConnection: (connectionId) => ipcRenderer.invoke('connections:wecom:test', connectionId),
  saveWecomConnection: (input) => ipcRenderer.invoke('connections:wecom:save', input),
  getTailscaleConnection: () => ipcRenderer.invoke('connections:tailscale:get'),
  saveTailscaleConnection: (input) => ipcRenderer.invoke('connections:tailscale:save', input),
  checkRuntimeUpdate: () => ipcRenderer.invoke('runtime:update'),
  checkDesktopUpdate: () => ipcRenderer.invoke('desktop:update'),
  searchPlugins: (query) => ipcRenderer.invoke('plugins:search', query),
  listPlugins: () => ipcRenderer.invoke('plugins:list'),
  listLocalSkills: () => ipcRenderer.invoke('skills:local'),
  installPlugin: (source, artifactManifestDigest) => (
    ipcRenderer.invoke('plugins:install', source, artifactManifestDigest)
  ),
  setPluginEnabled: (name, enabled) => ipcRenderer.invoke('plugins:state', name, enabled),
  uninstallPlugin: (name) => ipcRenderer.invoke('plugins:uninstall', name),
  getPluginConfig: (name, scope) => ipcRenderer.invoke('plugins:config:get', name, scope),
  validatePluginConfig: (input) => ipcRenderer.invoke('plugins:config:validate', input),
  savePluginConfig: (input) => ipcRenderer.invoke('plugins:config:save', input),
  resetPluginConfig: (name, scope) => ipcRenderer.invoke('plugins:config:reset', name, scope),
  rollbackPlugin: (name, version) => ipcRenderer.invoke('plugins:rollback', name, version),
  listMcpOwnershipConflicts: (source, artifactManifestDigest) => (
    ipcRenderer.invoke('plugins:mcp-conflicts', source, artifactManifestDigest)
  ),
  listCapabilityApprovals: () => ipcRenderer.invoke('capabilities:approvals:list'),
  decideCapabilityApproval: (requestId, decision) => (
    ipcRenderer.invoke('capabilities:approvals:decide', requestId, decision)
  ),
  browserAttachGuest: async (payload) => {
    const result = await ipcRenderer.invoke('browser:attach-guest', payload) as { ok: boolean; error?: string };
    if (!result?.ok) throw new Error(result?.error ?? 'Browser guest registration failed.');
  },
  browserDetachGuest: (key) => ipcRenderer.invoke('browser:detach-guest', key),
  openInSystemBrowser: (url) => ipcRenderer.invoke('browser:open-external', url),
  onBrowserGuestCrashed: (listener) => {
    const handler = (_event: IpcRendererEvent, guestKey: Parameters<typeof listener>[0]) => listener(guestKey);
    ipcRenderer.on('browser:guest-crashed', handler);
    return () => ipcRenderer.removeListener('browser:guest-crashed', handler);
  },
  onBrowserSessionRequest: (listener) => {
    const handler = (_event: IpcRendererEvent, conversationId: Parameters<typeof listener>[0]) => listener(conversationId);
    ipcRenderer.on('browser:session-request', handler);
    return () => ipcRenderer.removeListener('browser:session-request', handler);
  },
  onNotificationNavigation: (listener) => {
    const handler = (_event: IpcRendererEvent, target: Parameters<typeof listener>[0]) => listener(target);
    ipcRenderer.on('notifications:navigate', handler);
    return () => ipcRenderer.removeListener('notifications:navigate', handler);
  },
};

contextBridge.exposeInMainWorld('yuanpu', bridge);

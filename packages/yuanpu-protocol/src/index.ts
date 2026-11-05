export const PROTOCOL_VERSION = 9;

import type { NotificationNavigationTarget } from './host-events.js';
import type { RunToolStep } from './run-progress.js';
import type { AssistantMemoryView, AssistantSuggestion, AssistantSuggestionInbox,
  AssistantWorkspaceSnapshot, AssistantDelegationRecord, AssistantSourceRevocationReceipt } from './assistant.js';
import type { AgentRunCancellationReceipt, AgentRunReceipt, AgentRunRecord, AgentRunRequest, AgentRunStatus,
  DesktopCanvasWidgetSummary } from './agent.js';
import type {
  ScheduleHistoryRecord,
  ScheduleInput,
  SchedulePrivateContact,
  ScheduleRecord,
} from './scheduler.js';

export * from './agent.js';
export * from './run-progress.js';
export * from './assistant.js';
export * from './host-events.js';
export * from './hotkeys.js';
export * from './scheduler.js';

export const RUNTIME_ROUTES = {
  health: '/v1/health',
  greeting: '/v1/greeting',
  chat: '/v1/chat',
  chatSubmit: '/v1/chat/submit',
  desktopCanvasSubmit: '/v1/desktop/canvas/submit',
  agentRuns: '/v1/agent/runs',
  privateImRuns: '/v1/im/private-runs',
  hostEvents: '/v1/host/events',
  hostEventReceipts: '/v1/host/events/receipts',
  notificationTargetValidation: '/v1/notifications/targets/validate',
  schedules: '/v1/schedules',
  channelScheduleTargets: '/v1/channels/schedule-targets',
  wecomConnections: '/v1/connections/wecom',
  tailscaleConnection: '/v1/connections/tailscale',
  localSkills: '/v1/skills/local',
  centerConnection: '/v1/center/connection',
  plugins: '/v1/plugins',
  pluginSearch: '/v1/plugins/search',
  pluginInstall: '/v1/plugins/install',
  pluginState: '/v1/plugins/state',
  pluginUninstall: '/v1/plugins/uninstall',
  pluginConfig: '/v1/plugins/config',
  pluginConfigValidate: '/v1/plugins/config/validate',
  pluginConfigSave: '/v1/plugins/config/save',
  pluginConfigReset: '/v1/plugins/config/reset',
  pluginRollback: '/v1/plugins/rollback',
  pluginMcpConflicts: '/v1/plugins/mcp-conflicts',
  capabilityApprovals: '/v1/capabilities/approvals',
  capabilityApprovalDecision: '/v1/capabilities/approvals/decision',
  assistantLink: '/v1/assistant/link',
  assistantMirrors: '/v1/assistant/mirrors',
  assistantSuggestions: '/v1/assistant/suggestions',
  assistantWorkspace: '/v1/assistant/workspace',
  desktopTranscript: '/v1/desktop/transcript',
  workConversations: '/v1/work/conversations',
  workFolders: '/v1/work/folders',
  workMove: '/v1/work/move',
  workSearch: '/v1/work/search',
  workMessageWindow: '/v1/work/messages/window',
  workTrajectory: '/v1/work/trajectory',
  workSubagentCancel: '/v1/work/subagents/cancel',
  workAiTickets: '/v1/work/ai-tickets',
  workAiTicketMessages: '/v1/work/ai-tickets/messages',
  workAiTicketCancel: '/v1/work/ai-tickets/cancel',
  workTags: '/v1/work/tags',
  workOrder: '/v1/work/order',
  workFiles: '/v1/work/files',
  workFileContent: '/v1/work/files/content',
  workFilePath: '/v1/work/files/path',
  workFileImport: '/v1/work/files/import',
  workFileChanges: '/v1/work/file-changes',
  modelSettings: '/v1/settings/models',
  modelSettingsDelete: '/v1/settings/models/delete',
  modelCatalog: '/v1/settings/models/catalog',
  hotkeySettings: '/v1/settings/hotkeys',
} as const;

export type ModelApi = 'openai-completions' | 'openai-responses' | 'anthropic-messages' | 'google-generative-ai';

export interface TailscaleNodeStatus {
  /** `unavailable` means this build does not ship tailscaled (non-macOS or disabled). */
  state: 'unavailable' | 'stopped' | 'starting' | 'connected' | 'needs-login' | 'error';
  ipv4?: string;
  dnsName?: string;
  tailnet?: string;
  message?: string;
}

export interface TailscaleConnectionSettings {
  hasAuthKey: boolean;
  node: TailscaleNodeStatus;
}

export interface SaveTailscaleConnectionInput {
  authKey?: string;
  removeAuthKey?: boolean;
}

export interface ModelSettings {
  provider: string;
  model: string;
  baseUrl: string;
  api: ModelApi;
  apiKeyEnv: string;
  credential: 'none' | 'api_key' | 'oauth';
  providerSuggestions: string[];
  modelSuggestions: string[];
  customModels: CustomModelSettings[];
  customProviders: CustomProviderSettings[];
  providerCredentials: Record<string, 'none' | 'api_key' | 'oauth'>;
  providerNames: Record<string, string>;
}

export interface CustomProviderSettings {
  id: string;
  name: string;
  baseUrl: string;
  api: ModelApi;
  apiKeyEnv: string;
  credential: 'none' | 'api_key' | 'oauth';
}

export interface CustomModelSettings {
  provider: string;
  model: string;
  name: string;
  baseUrl: string;
  api: ModelApi;
  apiKeyEnv: string;
  contextWindow: number;
  maxTokens: number;
  reasoning: boolean;
  inputTypes: Array<'text' | 'image'>;
  thinkingLevelMap?: Record<string, string | number | boolean | null>;
  credential: 'none' | 'api_key' | 'oauth';
  active: boolean;
  source: 'catalog' | 'custom';
}

export interface ModelCatalog {
  providers: { id: string; name: string; modelCount: number }[];
  models: { id: string; name: string; api: string; baseUrl: string }[];
}

export interface SaveModelSettingsInput {
  provider: string;
  model: string;
  baseUrl: string;
  api: ModelApi;
  apiKeyEnv: string;
  providerName?: string;
  contextWindow?: number;
  maxTokens?: number;
  reasoning?: boolean;
  inputTypes?: Array<'text' | 'image'>;
  thinkingLevelMap?: Record<string, string | number | boolean | null>;
  apiKey?: string;
  removeApiKey?: boolean;
  activate?: boolean;
}

export type DesktopConversationSurface = 'work' | 'assistant';
export type DesktopTranscriptSurface = DesktopConversationSurface | 'assistantAll' | 'assistantArchive';

export interface WorkConversation {
  id: string;
  createdAt: string;
  updatedAt: string;
  workingDirectory: string;
  /** Historical absolute paths may still appear verbatim in old messages. */
  previousWorkingDirectories?: string[];
  current: boolean;
  archived: boolean;
  archivedAt: string | null;
  title: string;
  iconId: string;
  folderId: string | null;
  sortOrder: number;
  tagIds: string[];
}

export interface WorkMoveRequest {
  requestId: string;
  kind: 'folder' | 'conversation';
  id: string;
  targetFolderId: string | null;
}
export interface WorkMoveResult {
  requestId: string;
  conversationIds: string[];
  previousDirectories: Array<{ conversationId: string; path: string }>;
  warning: string;
}

export interface WorkSearchQuery {
  query: string;
  archive?: 'active' | 'archived' | 'all';
  limit?: number;
  cursor?: string;
}

export interface WorkSearchItem {
  kind: 'conversation' | 'message';
  conversationId: string;
  title: string;
  archived: boolean;
  folderPath: Array<{ id: string; name: string }>;
  matchedField: 'title' | 'folder' | 'tag' | 'message';
  snippet: string;
  messageEntryId?: string;
  messagePosition?: number;
  role?: 'user' | 'assistant';
  at?: string;
}

export interface WorkSearchResult {
  items: WorkSearchItem[];
  nextCursor?: string;
  contentFailures: Array<{ conversationId: string; reason: 'missing' | 'unreadable' | 'corrupt' | 'too_large' | 'budget_exceeded' }>;
}

export type WorkMessageWindowResult =
  | { status: 'ok'; messages: DesktopTranscriptMessage[]; targetIndex: number; hasBefore: boolean; hasAfter: boolean }
  | { status: 'missing' | 'unreadable' | 'corrupt' | 'too_large' | 'not_found'; messages: [] };

export interface WorkFolder {
  id: string;
  parentId: string | null;
  name: string;
  iconId: string;
  relativeDirectory: string;
  sortOrder: number;
  createdAt: string;
  updatedAt: string;
  system?: boolean;
}

export interface WorkTag {
  id: string;
  name: string;
  color: string;
  createdAt: string;
  updatedAt: string;
}

export interface WorkFileEntry {
  name: string;
  /** Workspace-relative path with POSIX separators. Never an absolute path. */
  path: string;
  kind: 'file' | 'directory';
  size?: number;
  updatedAt?: string;
}

export interface WorkDirectoryListing {
  /** Normalized workspace-relative directory ('' is the workspace root). */
  path: string;
  entries: WorkFileEntry[];
  /** Present on recursive listings: entry growth hit the server-side cap. */
  truncated?: boolean;
}

/** Workspace-relative path of an imported attachment. */
export interface WorkFileImportResult {
  path: string;
  size: number;
}

/** Read-only preview of one workspace file. Never carries the absolute workspace path. */
export type WorkFilePreview =
  | { kind: 'text'; path: string; content: string; truncated: boolean; size: number; updatedAt: string }
  | { kind: 'image'; path: string; mediaType: string; base64: string; size: number; updatedAt: string }
  | { kind: 'pdf'; path: string; base64: string; size: number; updatedAt: string }
  | { kind: 'unsupported'; path: string; reason: string; size?: number };

/** Captured content snapshot of one file version. Never carries an absolute path. */
export interface WorkFileChangeSnapshot {
  content: string;
  truncated: boolean;
}

/** One workspace file merged across its edits within the requested scope. */
export interface WorkFileChangeEntry {
  /** Workspace-relative path with POSIX separators. Never an absolute path. */
  path: string;
  runId: string;
  toolName: 'edit' | 'write';
  /** null means the file did not exist before the change (new file). */
  before: WorkFileChangeSnapshot | null;
  after: WorkFileChangeSnapshot;
  updatedAt: string;
}

export interface WorkFileChangesSummary {
  conversationId: string;
  /** Echoed when the query was scoped to one run; null for conversation-wide. */
  runId: string | null;
  files: WorkFileChangeEntry[];
}

/** One control command addressed to the embedded browser session of a Work conversation. */
export interface BrowserControlCommand {
  method: 'navigate' | 'back' | 'forward' | 'reload' | 'screenshot' | 'snapshot' | 'click' | 'type' | 'scroll' | 'evaluate';
  conversationId: string;
  url?: string;
  fullPage?: boolean;
  x?: number;
  y?: number;
  deltaX?: number;
  deltaY?: number;
  text?: string;
  expression?: string;
}

/** Result of one embedded-browser control command. */
export interface BrowserControlResult {
  ok: boolean;
  method: BrowserControlCommand['method'];
  url?: string;
  title?: string;
  canGoBack?: boolean;
  canGoForward?: boolean;
  base64?: string;
  text?: string;
  error?: string;
}

export interface BrowserGuestAttachPayload {
  /** Stable key of the webview guest (per browser tab). */
  key: string;
  webContentsId: number;
  conversationId: string;
}

/** Public execution summary with optional bounded, redacted tool evidence. Never includes reasoning. */
export interface DesktopReplyRunInfo {
  runId?: string;
  source?: 'runtime' | 'transcript';
  status: AgentRunRecord['status'];
  createdAt: string;
  updatedAt: string;
  events: Array<{ id: number; title: string; detail?: string; at: string }>;
  tools: Array<{ name: string; status: 'completed' | 'failed' }>;
  steps?: RunToolStep[];
}

export interface DesktopTranscriptMessage {
  id: string;
  role: 'user' | 'assistant';
  text: string;
  at: string;
  channel?: 'desktop' | 'wecom';
  run?: DesktopReplyRunInfo;
}

export interface SessionTrajectoryRow {
  id: string;
  round: number;
  kind: 'user' | 'assistant' | 'tool' | 'model' | 'context' | 'subagent';
  title: string;
  text: string;
  at: string;
  status?: 'running' | 'completed' | 'failed';
  toolCallId?: string;
}

export interface SessionTrajectory {
  conversationId: string;
  rows: SessionTrajectoryRow[];
  rounds: number;
  calls: number;
  children: Array<{ id: string; runId?: string; agent: string; task?: string; status: string; at: string; progress?: string;
    result?: string; error?: string;
    rows: SessionTrajectoryRow[]; rounds: number; calls: number }>;
  context?: { tokens: number | null; contextWindow: number; percent: number | null;
    breakdown?: { systemPrompt: number; tools: number; messages: number; other: number } };
  live?: { runId: string; text: string; finishedAt?: string; steps?: RunToolStep[]; tools: Array<{ id: string; name: string; summary: string;
    category: string; status: 'running' | 'completed' | 'failed'; at: string }> };
}

export interface AiTicket {
  id: string;
  parentConversationId: string;
  task: string;
  title?: string;
  deliverables?: string[];
  acceptanceCriteria?: string[];
  model?: string;
  modelSelection?: AgentRunRequest['modelSelection'];
  context?: SessionTrajectory['context'];
  status: 'queued' | 'running' | 'completed' | 'failed' | 'cancelled' | 'interrupted' | 'needs_approval';
  createdAt: string;
  updatedAt: string;
  progress?: string;
  liveRun?: DesktopReplyRunInfo;
  error?: string;
  messages?: DesktopTranscriptMessage[];
  activity?: SessionTrajectoryRow[];
}

export interface AiTicketList {
  tickets: AiTicket[];
  navigation: { ticketId?: string; revision: number };
}

export interface AssistantLinkStatus {
  linked: boolean;
  contactId?: string;
  connectionId?: string;
}

export interface AssistantMirrorStatus {
  mirrorId: string;
  runId: string;
  part: 'user' | 'assistant';
  status: 'pending' | 'delivering' | 'accepted' | 'failed' | 'unknown';
  failureCode?: string;
}

export interface RuntimeInfo {
  version: string;
  protocolVersion: number;
  piVersion: string;
  mcpTools: readonly string[];
  configRoot: string;
  workingDirectory?: string;
  notificationsEnabled: boolean;
}

export interface ServerCenterStatus {
  url: string;
  state: 'offline' | 'incompatible' | 'running' | 'ready';
  checkedAt: string;
  version?: string;
  apiVersion?: number;
  schemaVersion?: number;
  issueCode?: string;
  requestId?: string;
}

export interface ServerWikiPreview {
  preview: true;
  entries: Array<{
    id: string;
    title: string;
    summary: string;
    body: string;
    level: string;
    experience: number;
    updatedAt: string;
    links: string[];
  }>;
}

/** Public capability metadata and account-scoped content returned by Yuanpu Server. */
export interface ServerCenterCatalogItem {
  id: string;
  displayName: string;
  version: string;
  description: string;
  publisher: string;
  source: string;
  components: string[];
  permissions: string[];
}

export interface ServerCenterUser {
  id: string;
  email: string;
  name: string;
  role: 'admin' | 'reviewer' | 'team_lead' | 'member';
}

export interface ServerCenterResource {
  id: string;
  kind: 'news' | 'wiki' | 'capability';
  visibility: 'team' | 'organization';
  title: string;
  summary: string;
  body: string;
  revision: number;
}

export type ServerCenterResult<T> =
  | { ok: true; value: T }
  | { ok: false; code: 'UNAUTHORIZED' | 'FORBIDDEN' | 'UNAVAILABLE' };

export interface ServerCenterCredentialStatus {
  configured: boolean;
  storageAvailable: boolean;
}

export interface RuntimeGreeting {
  message: string;
}

export interface PrivateImRunSummary {
  runId: string;
  runStatus: AgentRunStatus;
  replyDeliveryStatus: 'not_created' | 'pending' | 'delivering' | 'accepted' | 'failed' | 'unknown';
}

/** Deliberately excludes bot ID, credential reference, secret and sender digests. */
export interface WecomConnectionSummary {
  connectionId: string;
  enabled: boolean;
  pairedSenderCount: number;
  groupEnabled: boolean;
  status: 'disabled' | 'connected' | 'connecting' | 'unavailable' | 'invalid_configuration';
  diagnostic?: 'configuration_invalid' | 'credential_unavailable' | 'connection_unavailable' | 'authentication_failed';
}

export interface WecomConnectionList {
  status: 'ok' | 'invalid_configuration';
  connections: WecomConnectionSummary[];
}

export interface WecomConnectionConfigInput {
  connectionId: string;
  enabled: boolean;
  /** Required only when creating a connection. Never returned by a management response. */
  botId?: string;
}

export interface ChatToolEvent {
  name: string;
  status: 'started' | 'completed' | 'failed';
}

export interface ChatResponse {
  message: string;
  tools: ChatToolEvent[];
}

export interface RuntimeUpdateState {
  status: 'idle' | 'checking' | 'current' | 'ready' | 'error';
  currentVersion?: string;
  availableVersion?: string;
  message?: string;
}

export interface RuntimeRecoveryNotice {
  kind: 'incompatible_protocol' | 'activation_failed';
}

export interface CapabilityApprovalSummary {
  requestId: string;
  runId?: string;
  sessionId: string;
  workspaceId: string;
  sourceInstanceId: string;
  capabilityId: string;
  packageVersion?: string;
  argumentsDigest: string;
  status: 'pending';
  createdAt: string;
  expiresAt: string;
  assistantDelegation?: {
    taskId: string;
    skillName: string;
    goal: string;
    completionCriteria: string[];
    contextRefs: string[];
    sourceVersions?: Record<string, string>;
    authorizedCapabilities: string[];
    readOnly: boolean;
  };
}

export interface CapabilityApprovalDecisionInput {
  requestId: string;
  decision: 'approved' | 'denied';
  issuedAt: number;
  nonce: string;
  signature: string;
}

export interface CapabilityApprovalDecisionResult {
  requestId: string;
  status: 'completed' | 'denied';
  message?: string;
}

export interface McpOwnershipConflict {
  name: string;
  owners: Array<'yuanpu' | 'pi-mcp-user' | 'pi-mcp-workspace'>;
}

export function capabilityApprovalSigningPayload(
  input: Omit<CapabilityApprovalDecisionInput, 'signature'>,
): Uint8Array {
  return new TextEncoder().encode(JSON.stringify([
    'yuanpu-capability-approval-v1',
    input.requestId,
    input.decision,
    input.issuedAt,
    input.nonce,
  ]));
}

export const CAPABILITY_PACKAGE_MANIFEST_VERSION = 1;

export type CapabilityPackageKind = 'pi-extension' | 'python-mcp';

export interface CapabilityArtifactTarget {
  platform: 'darwin' | 'linux' | 'win32';
  arch: 'x64' | 'arm64';
  systemBaseline?: string;
  format: 'zip' | 'tar.gz';
  url: string;
  size: number;
  sha256: string;
  entrypoint: string;
}

export interface CapabilityPackageManifest {
  manifestVersion: typeof CAPABILITY_PACKAGE_MANIFEST_VERSION;
  kind: CapabilityPackageKind;
  id: string;
  version: string;
  capabilityContractVersion: number;
  runtimeCompatibility: { minimum: string; maximumExclusive?: string };
  artifacts: CapabilityArtifactTarget[];
  configSchema?: Record<string, unknown>;
  permissions: Array<'filesystem' | 'network' | 'credentials' | 'notifications' | 'background'>;
  connections?: string[];
  issuedAt: string;
  signature: { algorithm: 'ed25519'; keyId: string; value: string };
}

export interface PluginSearchResult {
  id?: string;
  name: string;
  displayName?: string;
  version: string;
  description: string;
  publisher?: string;
  updatedAt?: string;
  npmUrl?: string;
  source: string;
  components?: Array<'skill' | 'agent' | 'workflow' | 'extension' | 'prompt' | 'theme' | 'connector'>;
  permissions?: Array<'instructions' | 'scripts' | 'filesystem' | 'network' | 'credentials' | 'notifications' | 'background'>;
  artifactManifestDigest?: string;
}

export type SkillCatalogItem = PluginSearchResult;

export interface LocalSkill {
  name: string;
  description: string;
  filePath: string;
  disableModelInvocation: boolean;
}

export interface LocalSkillList {
  skills: LocalSkill[];
  diagnostics: Array<{ path: string; message: string }>;
}

export interface InstalledPlugin {
  name: string;
  version: string;
  description: string;
  source: string;
  installPath: string;
  enabled: boolean;
  installedAt: string;
  loadError?: string;
  configurable?: boolean;
  configStatus?: 'unsupported' | 'optional' | 'required' | 'valid' | 'invalid';
  kind?: 'pi-extension' | 'python-mcp';
  activeVersion?: string;
  availableVersions?: string[];
}

export type PluginConfigScope = 'user' | 'workspace';

export interface PluginConfigDocument {
  pluginName: string;
  kind: 'mcp' | 'schema';
  title: string;
  description: string;
  scope: PluginConfigScope;
  path: string;
  value: Record<string, unknown>;
  schema?: Record<string, unknown>;
  supportsWorkspace: boolean;
  secretPolicy: 'environment-only';
}

export interface PluginConfigInput {
  name: string;
  scope: PluginConfigScope;
  value: Record<string, unknown>;
}

export interface PluginConfigValidation {
  valid: boolean;
  errors: string[];
}

export interface DesktopBridge {
  runtimeInfo(): Promise<RuntimeInfo>;
  getServerCenterStatus(): Promise<ServerCenterStatus>;
  getServerWikiPreview(): Promise<ServerWikiPreview>;
  searchServerCatalog(query: string): Promise<ServerCenterCatalogItem[]>;
  restoreServerCenterSession(): Promise<ServerCenterResult<ServerCenterUser>>;
  logoutServerCenter(): Promise<void>;
  getServerCenterCredentialStatus(): Promise<ServerCenterCredentialStatus>;
  getServerCenterEndpoint(): Promise<string>;
  saveServerCenterEndpoint(url: string): Promise<{ url: string; keyCleared: boolean }>;
  saveServerCenterAPIKey(apiKey: string): Promise<ServerCenterResult<ServerCenterUser>>;
  removeServerCenterAPIKey(): Promise<void>;
  listServerResources(query: string): Promise<ServerCenterResult<ServerCenterResource[]>>;
  getServerResource(id: string): Promise<ServerCenterResult<ServerCenterResource>>;
  getHotkeySettings(): Promise<import('./hotkeys.js').HotkeySettings>;
  saveHotkeySetting(input: import('./hotkeys.js').SaveHotkeyInput): Promise<import('./hotkeys.js').HotkeySettings>;
  getModelSettings(): Promise<ModelSettings>;
  getModelCatalog(provider?: string): Promise<ModelCatalog>;
  saveModelSettings(input: SaveModelSettingsInput): Promise<ModelSettings>;
  deleteModelSettings(provider: string, model: string): Promise<ModelSettings>;
  runtimeRecoveryNotice(): Promise<RuntimeRecoveryNotice | undefined>;
  greeting(name: string): Promise<RuntimeGreeting>;
  chat(message: string): Promise<ChatResponse>;
  submitDesktopMessage(message: string, surface?: DesktopConversationSurface, conversationId?: string,
    clientMessageId?: string, modelSelection?: AgentRunRequest['modelSelection'],
    approvalMode?: AgentRunRequest['approvalMode']): Promise<AgentRunReceipt>;
  /** Sends a message to the desktop canvas agent, the only conversation with canvas widget tools. */
  submitDesktopCanvasMessage(message: string, widgets: DesktopCanvasWidgetSummary[]): Promise<AgentRunReceipt>;
  getDesktopTranscript(surface: DesktopTranscriptSurface, conversationId?: string,
    beforeId?: string, limit?: number): Promise<DesktopTranscriptMessage[]>;
  listWorkConversations(): Promise<WorkConversation[]>;
  createWorkConversation(folderId?: string, requestId?: string): Promise<WorkConversation>;
  selectWorkConversation(conversationId: string, previewArchived?: boolean): Promise<WorkConversation>;
  updateWorkConversation(conversationId: string, patch: { title?: string; iconId?: string; archived?: boolean; tagIds?: string[] }): Promise<WorkConversation>;
  moveWorkNode(request: WorkMoveRequest): Promise<WorkMoveResult>;
  searchWorkConversations(input: WorkSearchQuery): Promise<WorkSearchResult>;
  getWorkMessageWindow(conversationId: string, entryId: string, radius?: number): Promise<WorkMessageWindowResult>;
  getWorkTrajectory(conversationId: string): Promise<SessionTrajectory>;
  cancelWorkSubagent(conversationId: string, runId: string): Promise<void>;
  listAiTickets(conversationId: string): Promise<AiTicketList>;
  getAiTicket(conversationId: string, ticketId: string): Promise<AiTicket>;
  createAiTicket(conversationId: string, task: string, details?: {
    title?: string; deliverables?: string[]; acceptanceCriteria?: string[];
  }): Promise<AiTicket>;
  sendAiTicketMessage(conversationId: string, ticketId: string, message: string,
    options?: { modelSelection?: AgentRunRequest['modelSelection']; approvalMode?: AgentRunRequest['approvalMode'] }): Promise<AiTicket>;
  cancelAiTicket(conversationId: string, ticketId: string): Promise<void>;
  listWorkFolders(): Promise<WorkFolder[]>;
  createWorkFolder(parentId: string | null, name: string, iconId?: string, requestId?: string): Promise<WorkFolder>;
  updateWorkFolder(folderId: string, patch: { name?: string; iconId?: string }): Promise<WorkFolder>;
  listWorkTags(): Promise<WorkTag[]>;
  createWorkTag(name: string, color?: string, requestId?: string): Promise<WorkTag>;
  updateWorkTag(tagId: string, patch: { name?: string; color?: string }): Promise<WorkTag>;
  reorderWorkSiblings(kind: 'folder' | 'conversation', parentId: string | null, ids: string[]): Promise<void>;
  listWorkFiles(conversationId: string, dirPath?: string, options?: { recursive?: boolean }): Promise<WorkDirectoryListing>;
  browserAttachGuest(payload: BrowserGuestAttachPayload): Promise<void>;
  browserDetachGuest(key: string): Promise<void>;
  openInSystemBrowser(url: string): Promise<void>;
  onBrowserGuestCrashed(listener: (guestKey: string) => void): () => void;
  onBrowserSessionRequest(listener: (conversationId: string) => void): () => void;
  readWorkFile(conversationId: string, filePath: string): Promise<WorkFilePreview>;
  /** Saves a dropped or picked file into the conversation workspace under 附件/. */
  importWorkFile(conversationId: string, name: string, data: ArrayBuffer): Promise<WorkFileImportResult>;
  openWorkFile(conversationId: string, filePath: string): Promise<void>;
  listWorkFileChanges(query: { conversationId: string; runId?: string }): Promise<WorkFileChangesSummary>;
  getAssistantLink(): Promise<AssistantLinkStatus>;
  bindAssistantContact(contactId: string): Promise<AssistantLinkStatus>;
  unbindAssistantContact(): Promise<AssistantLinkStatus>;
  listAssistantMirrors(runId: string): Promise<AssistantMirrorStatus[]>;
  retryAssistantMirror(mirrorId: string): Promise<AssistantMirrorStatus>;
  listAssistantSuggestions(): Promise<AssistantSuggestionInbox>;
  feedbackAssistantSuggestion(id: string, action: 'ignored' | 'snoozed' | 'accepted',
    snoozedUntil?: string): Promise<AssistantSuggestion>;
  setAssistantSuggestionsPaused(until?: string): Promise<{ pausedUntil?: string }>;
  markAssistantSuggestionRead(id: string): Promise<AssistantSuggestion>;
  getAssistantWorkspace(memoryLimit?: number): Promise<AssistantWorkspaceSnapshot>;
  revokeAssistantSource(sourceId: string, expectedVersion: string):
    Promise<AssistantSourceRevocationReceipt>;
  correctAssistantMemory(id: string, expectedVersion: number, text: string,
    revisionId: string): Promise<AssistantMemoryView>;
  forgetAssistantMemory(id: string): Promise<{ forgottenIds: string[] }>;
  importAssistantSavedMemory(savedId: string, surface: 'work' | 'assistant', text: string,
    savedAt: string): Promise<AssistantMemoryView>;
  setAssistantOrganizingPaused(until?: string): Promise<{ organizingPausedUntil?: string }>;
  followUpAssistantDelegation(taskId: string, text: string): Promise<AssistantDelegationRecord>;
  cancelAssistantDelegation(taskId: string): Promise<AssistantDelegationRecord>;
  getAgentRun(runId: string): Promise<AgentRunRecord>;
  getPrivateImRunSummary(runId: string): Promise<PrivateImRunSummary>;
  cancelAgentRun(runId: string): Promise<AgentRunCancellationReceipt>;
  listSchedules(): Promise<ScheduleRecord[]>;
  createSchedule(input: ScheduleInput): Promise<ScheduleRecord>;
  previewSchedule(input: ScheduleInput): Promise<{ nextTriggerAt?: string }>;
  updateSchedule(scheduleId: string, input: ScheduleInput): Promise<ScheduleRecord>;
  setScheduleEnabled(scheduleId: string, enabled: boolean): Promise<ScheduleRecord>;
  getScheduleHistory(scheduleId: string, limit?: number): Promise<ScheduleHistoryRecord[]>;
  listSchedulePrivateContacts(): Promise<SchedulePrivateContact[]>;
  bindSchedulePrivateContact(contactId: string): Promise<{ routeId: string }>;
  revokeSchedulePrivateTarget(routeId: string): Promise<void>;
  listWecomConnections(): Promise<WecomConnectionList>;
  testWecomConnection(connectionId: string): Promise<WecomConnectionSummary>;
  saveWecomConnection(input: WecomConnectionConfigInput): Promise<WecomConnectionSummary>;
  getTailscaleConnection(): Promise<TailscaleConnectionSettings>;
  saveTailscaleConnection(input: SaveTailscaleConnectionInput): Promise<TailscaleConnectionSettings>;
  checkRuntimeUpdate(): Promise<RuntimeUpdateState>;
  checkDesktopUpdate(): Promise<void>;
  searchPlugins(query: string): Promise<PluginSearchResult[]>;
  listLocalSkills(): Promise<LocalSkillList>;
  listPlugins(): Promise<InstalledPlugin[]>;
  installPlugin(source: string, artifactManifestDigest?: string): Promise<InstalledPlugin>;
  setPluginEnabled(name: string, enabled: boolean): Promise<InstalledPlugin>;
  uninstallPlugin(name: string): Promise<void>;
  getPluginConfig(name: string, scope: PluginConfigScope): Promise<PluginConfigDocument>;
  validatePluginConfig(input: PluginConfigInput): Promise<PluginConfigValidation>;
  savePluginConfig(input: PluginConfigInput): Promise<PluginConfigDocument>;
  resetPluginConfig(name: string, scope: PluginConfigScope): Promise<PluginConfigDocument>;
  rollbackPlugin(name: string, version: string): Promise<InstalledPlugin>;
  listMcpOwnershipConflicts(source: string, artifactManifestDigest: string): Promise<McpOwnershipConflict[]>;
  listCapabilityApprovals(): Promise<CapabilityApprovalSummary[]>;
  decideCapabilityApproval(
    requestId: string,
    decision: CapabilityApprovalDecisionInput['decision'],
  ): Promise<CapabilityApprovalDecisionResult>;
  onNotificationNavigation(listener: (target: NotificationNavigationTarget) => void): () => void;
}

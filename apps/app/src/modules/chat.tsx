import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type KeyboardEvent,
} from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import type {
  CapabilityApprovalSummary,
  NotificationNavigationTarget,
  AgentRunRecord,
  AgentRunRequest,
  PrivateImRunSummary,
  WorkConversation,
  WorkSearchItem,
  WorkMessageWindowResult,
  SessionTrajectory,
  DesktopTranscriptMessage,
} from '@yuanpu-agent/protocol';
import { effectiveHotkeyBinding } from '@yuanpu-agent/protocol';
import mindlinkSeal from '../../themes/assets/mindlink-seal.png';
import { emo } from '../emo.js';

import { AppIcon } from '../shared/app-icon.js';
import { acceptComposerPlaceholder } from '../shared/composer-placeholder.js';
import { ComposerRuntimeControls, threeStepThinking } from '../shared/composer-runtime-controls.js';
import { bindingFromKeyEvent } from '../shared/hotkeys.js';
import { AssistantReply } from '../shared/assistant-reply.js';
import type { ImagePreview } from '../shared/message-image.js';
import { DelegationApprovalDetails, isVisibleAssistantDelegationApproval } from '../shared/delegation-approval-details.js';
import { elapsedLabel, ReplyRunDetails } from '../shared/reply-run-details.js';
import { AvatarMark } from '../shared/avatar-mark.js';
import { resizePanel } from '../shared/panel-resize.js';
import { PageToolbar } from '../shared/page-toolbar.js';
import { AssistantHome } from './assistant-home.js';
import { WorkTree } from './work-tree.js';
import { folderPath } from './work-tree-model.js';
import { cacheReplyRun, findReplyRun, type ReplyRunInfo } from '../shared/reply-run-cache.js';
import { normalizeWorkspacePath } from '../shared/work-file-links.js';
import { FileWorkspace, ImagePreviewPanel, type ImagePreviewRequest } from '../viewer/files/file-tabs.js';
import { ReviewPanel } from '../viewer/review/review-panel.js';
import { SessionTrajectoryViewer } from '../viewer/session-trajectory.js';
import { AiTicketsPanel } from '../viewer/ai-tickets-panel.js';
import type { ViewerBrowserHost } from '../viewer/host/browser-host.js';
import type { ViewerFileHost } from '../viewer/host/file-host.js';

type ToolState = { name: string; status: 'started' | 'completed' | 'failed' };
type LiveTool = NonNullable<SessionTrajectory['live']>['tools'][number];
type ChatMessage = {
  id: number;
  entryId?: string;
  role: 'user' | 'assistant' | 'error' | 'notice';
  text: string;
  at?: string;
  channel?: DesktopTranscriptMessage['channel'];
  tools?: ToolState[];
  process?: LiveTool[];
  run?: ReplyRunInfo;
};
type ActivityEvent = {
  id: number;
  runId: string;
  title: string;
  detail?: string;
  at: string;
  tone: 'active' | 'done' | 'warning' | 'error';
};

function runStatusLabel(status: AgentRunRecord['status']): string {
  return {
    queued: '等待执行',
    running: '正在执行',
    waiting_approval: '等待授权',
    succeeded: '已完成',
    failed: '执行失败',
    cancelled: '已取消',
    interrupted: '执行中断',
    result_unknown: '结果未知',
  }[status];
}

function toolLabel(name: string): string {
  if (name === 'ai_ticket') return '任务工单';
  if (/subagent/i.test(name)) return '子 Agent';
  if (/search|fetch|web/i.test(name)) return 'Web 搜索';
  if (/read|write|edit|patch/i.test(name)) return '文件';
  if (/bash|shell|terminal/i.test(name)) return 'Bash';
  if (/code|python/i.test(name)) return '代码执行';
  if (/capability|mcp/i.test(name)) return 'MCP';
  return '工具调用';
}

function capabilityName(id: string): string {
  if (!id.startsWith('ypcap:')) return id.replaceAll('_', ' ');
  const encoded = id.split(':')[2];
  if (!encoded) return '外部能力';
  try {
    const padded = encoded.replaceAll('-', '+').replaceAll('_', '/').padEnd(Math.ceil(encoded.length / 4) * 4, '=');
    const name = new TextDecoder().decode(Uint8Array.from(atob(padded), (char) => char.charCodeAt(0)));
    if (!/^[\w.-]{1,80}$/.test(name)) return '外部能力';
    return ({ browser_evaluate: '在网页中执行操作', browser_snapshot: '查看网页状态',
      browser_navigate: '打开网页', web_search: '搜索网页', fetch_content: '读取网页内容' } as Record<string, string>)[name]
      ?? name.replaceAll('_', ' ');
  } catch {
    return '外部能力';
  }
}

function capabilitySource(source: string): string {
  if (source === 'builtin.host.browser') return '浏览器';
  if (source.startsWith('builtin.web')) return '网页';
  if (source.startsWith('builtin.')) return '内置能力';
  return source;
}

function stoppedNotice(run: AgentRunRecord): string {
  const duration = elapsedLabel(run);
  return duration ? `你在 ${duration} 后停止了` : '你停止了当前任务';
}

function messageTime(at?: string): string | undefined {
  if (!at || !Number.isFinite(new Date(at).getTime())) return undefined;
  const parts = new Intl.DateTimeFormat('zh-CN', {
    month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit', hourCycle: 'h23',
  }).formatToParts(new Date(at));
  const value = (type: Intl.DateTimeFormatPartTypes) => parts.find((part) => part.type === type)?.value ?? '';
  return `${value('month')}月${value('day')}日 ${value('hour')}:${value('minute')}`;
}

function privateImDeliveryLabel(status: PrivateImRunSummary['replyDeliveryStatus']): string {
  return {
    not_created: '无回复投递记录',
    pending: '等待投递',
    delivering: '正在投递',
    accepted: '企业微信已接收（未确认对方可见）',
    failed: '投递失败',
    unknown: '投递结果未知（不会自动重发）',
  }[status];
}

const initialMessages: ChatMessage[] = [{
  id: 1,
  role: 'assistant',
  text: emo.welcome.work.description,
}];
const assistantGreeting = emo.welcome.assistant.description;

function formatError(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function rightPanelSpace(panelWidth: number, listOpen: boolean, workTree: boolean): number {
  const treeIsOverlay = workTree && window.innerWidth <= 900;
  const leftPanelWidth = listOpen && !treeIsOverlay && window.innerWidth > 780
    ? workTree ? window.innerWidth <= 1100 ? 260 : 300 : window.innerWidth <= 1100 ? 240 : 260 : 0;
  return Math.max(1, panelWidth - leftPanelWidth);
}

function maxRightPanelWidth(panelWidth: number, listOpen: boolean, workTree: boolean): number {
  return Math.floor(rightPanelSpace(panelWidth, listOpen, workTree) * 0.7);
}

export function ChatPanel({
  active,
  surface,
  navigationTarget,
  onReturnToSchedules,
  scheduleOrigin,
  assistantWorkConversationId,
  clearAssistantWorkConversation,
  onOpenWorkConversation,
}: {
  active: boolean;
  surface: 'work' | 'assistant';
  navigationTarget?: NotificationNavigationTarget;
  onReturnToSchedules: () => void;
  scheduleOrigin: boolean;
  assistantWorkConversationId?: string;
  clearAssistantWorkConversation?: () => void;
  onOpenWorkConversation?: (conversationId: string) => void;
}) {
  const [messages, setMessages] = useState(() => surface === 'assistant'
    ? [{ ...initialMessages[0]!, text: assistantGreeting }]
    : initialMessages);
  const [input, setInput] = useState(() => {
    try { return window.localStorage.getItem(`yuanpu:draft:${surface}`) ?? ''; }
    catch { return ''; }
  });
  const [attachments, setAttachments] = useState<Array<{ name: string; contents: string }>>([]);
  const pendingAssistantMessage = useRef<{ text: string; id: string; createdAt: number } | undefined>(undefined);
  const [attachmentError, setAttachmentError] = useState('');
  const [redactionPreviewEnabled, setRedactionPreviewEnabled] = useState(false);
  const [unrestrictedConversationId, setUnrestrictedConversationId] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [approvals, setApprovals] = useState<CapabilityApprovalSummary[]>([]);
  const [approvalBusy, setApprovalBusy] = useState<string>();
  const [readyApprovals, setReadyApprovals] = useState<Set<string>>(new Set());
  const resolvedApprovals = useRef(new Set<string>());
  const [locationRetry, setLocationRetry] = useState(0);
  const [locatedRun, setLocatedRun] = useState<AgentRunRecord | 'loading' | 'error'>();
  const [privateImSummary, setPrivateImSummary] = useState<PrivateImRunSummary | 'loading'>();
  const [activeRunId, setActiveRunId] = useState<string>();
  const [activeRunStatus, setActiveRunStatus] = useState<AgentRunRecord['status']>();
  const [cancelBusy, setCancelBusy] = useState(false);
  const cancelInFlight = useRef(false);
  const requestedStops = useRef(new Set<string>());
  const stoppedRuns = useRef(new Set<string>());
  const nextNoticeId = useRef(-1);
  const [activityOpen, setActivityOpen] = useState(surface === 'assistant');
  const sidebarSessions = useRef(new Map<string, { open: boolean; tab: 'trajectory' | 'subagents' | 'files' | 'review'; reviewRunId?: string; width: number }>());
  const shownSidebarFor = useRef<string | undefined>(undefined);
  const [mountedWorkspaces, setMountedWorkspaces] = useState<string[]>([]);
  const [listOpen, setListOpen] = useState(false);
  const [listMaximized, setListMaximized] = useState(false);
  const [leftPanelResizing, setLeftPanelResizing] = useState(false);
  const [leftPanelWidth, setLeftPanelWidth] = useState(() => {
    try {
      const stored = Number(window.localStorage.getItem('yuanpu:left-panel-width'));
      return stored >= 240 && stored <= 720 ? stored : 300;
    } catch { return 300; }
  });
  const leftPanelDragCleanup = useRef<(() => void) | null>(null);
  const [rightPanelWidth, setRightPanelWidth] = useState(() => {
    if (surface === 'assistant') return (window.innerWidth - 50) / 2;
    return 320;
  });
  const [rightPanelResizing, setRightPanelResizing] = useState(false);
  const [rightPanelMaximized, setRightPanelMaximized] = useState(false);
  const [workspaceTabHost, setWorkspaceTabHost] = useState<HTMLDivElement | null>(null);
  const [activityTab, setActivityTab] = useState<'trajectory' | 'subagents' | 'files' | 'review'>('files');
  const [trajectory, setTrajectory] = useState<SessionTrajectory>();
  const [contextOpen, setContextOpen] = useState(false);
  const [modelOpen, setModelOpen] = useState(false);
  const [modelSelection, setModelSelection] = useState<AgentRunRequest['modelSelection']>();  const [filePreviewPath, setFilePreviewPath] = useState<string>();
  const [imagePreviewRequest, setImagePreviewRequest] = useState<ImagePreviewRequest>();
  const activityEventsRef = useRef<ActivityEvent[]>([]);
  const [lastRun, setLastRun] = useState<AgentRunRecord>();
  const [reviewRefreshKey, setReviewRefreshKey] = useState(0);
  const [reviewRunId, setReviewRunId] = useState<string>();
  const [submittedTask, setSubmittedTask] = useState<{ runId: string; text: string }>();
  const [runRecovery, setRunRecovery] = useState<{ runId: string; text: string }>();
  const [bridgeError, setBridgeError] = useState(false);
  const [archiveOpen, setArchiveOpen] = useState(false);
  const [workConversationId, setWorkConversationId] = useState<string>();
  function restoreSidebarFor(nextId: string) {
    if (shownSidebarFor.current === nextId) return;
    if (shownSidebarFor.current) sidebarSessions.current.set(shownSidebarFor.current, { open: activityOpen, tab: activityTab, reviewRunId, width: Math.max(rightPanelWidth, 260) });
    shownSidebarFor.current = nextId;
    const next = sidebarSessions.current.get(nextId);
    setActivityOpen(next?.open ?? false);
    setActivityTab(next?.tab ?? 'files');
    setReviewRunId(next?.reviewRunId);
    setRightPanelWidth(Math.max(next?.width ?? 320, 260));
    setRightPanelMaximized(false);
    setTopRenaming(false);
  }
  useLayoutEffect(() => {
    if (surface === 'work' && workConversationId) restoreSidebarFor(workConversationId);
  }, [surface, workConversationId]);
  useEffect(() => {
    if (surface !== 'work' || !workConversationId || !activityOpen) return;
    setMountedWorkspaces((current) => current.includes(workConversationId) ? current : [...current, workConversationId]);
  }, [surface, workConversationId, activityOpen]);
  useEffect(() => {
    try { window.localStorage.setItem('yuanpu:left-panel-width', String(leftPanelWidth)); } catch { /* best effort */ }
  }, [leftPanelWidth]);
  useEffect(() => () => leftPanelDragCleanup.current?.(), []);
  const [olderMessages, setOlderMessages] = useState<DesktopTranscriptMessage[]>([]);
  const [olderHasMore, setOlderHasMore] = useState(true);
  const [olderLoading, setOlderLoading] = useState(false);
  const olderLoadingRef = useRef(false);
  const olderLoadGeneration = useRef(0);
  const latestWindowRef = useRef<{ conversationId: string; messages: DesktopTranscriptMessage[] } | undefined>(undefined);
  const followConversationBottom = useRef(true);
  const prependScrollAnchor = useRef<{ height: number; top: number } | undefined>(undefined);
  const sourceNavigationRef = useRef<string | undefined>(undefined);
  const [topRenaming, setTopRenaming] = useState(false);
  const [topRenameDraft, setTopRenameDraft] = useState('');
  const [searchWindow, setSearchWindow] = useState<{ conversationId: string; entryId: string; result: Extract<WorkMessageWindowResult, { status: 'ok' }> }>();
  const [searchLocationError, setSearchLocationError] = useState('');
  const pendingWorkCreateRequest = useRef<{ folderId?: string; requestId: string } | undefined>(undefined);
  const [workListError, setWorkListError] = useState('');
  const approvalContext = `${surface}:${workConversationId ?? ''}:${navigationTarget?.runId ?? ''}`;
  const approvalContextRef = useRef(approvalContext);
  const readyApprovalContextRef = useRef('');
  approvalContextRef.current = approvalContext;
  const [copiedUserMessageId, setCopiedUserMessageId] = useState<number>();
  const sending = useRef(false);
  const settledRuns = useRef(new Map<string, AgentRunRecord>());
  const latestProcess = useRef(new Map<string, LiveTool[]>());
  const observedTicketCalls = useRef<{ conversationId: string; ids: Set<string> } | undefined>(undefined);
  const activityDialog = useRef<HTMLDialogElement>(null);
  const chatPanel = useRef<HTMLElement>(null);
  function openAiTickets() {
    setActivityOpen(true);
    setActivityTab('subagents');
    if (rightPanelWidth === 320 && window.innerWidth > 900) {
      const panelWidth = chatPanel.current?.getBoundingClientRect().width ?? window.innerWidth - 50;
      const available = rightPanelSpace(panelWidth, listOpen, true);
      setRightPanelWidth(Math.min(maxRightPanelWidth(panelWidth, listOpen, true),
        Math.max(320, Math.round(available * 0.46))));
    }
  }
  const rightPanelDragCleanup = useRef<(() => void) | null>(null);
  const rightPanelGesture = useRef({ startedAtLimit: false, stopped: false });
  const assistantPanelRatio = useRef(0.5);
  const activityToggle = useRef<HTMLButtonElement>(null);
  const restoreActivityFocus = useRef(false);
  const nextActivityId = useRef(1);
  const nextId = useRef(2);
  const conversation = useRef<HTMLDivElement>(null);
  const conversationInner = useRef<HTMLDivElement>(null);
  const attachmentInput = useRef<HTMLInputElement>(null);
  const composerInput = useRef<HTMLTextAreaElement>(null);
  const [watermarkCount, setWatermarkCount] = useState(1);
  const desktop = window.yuanpu;
  const queryClient = useQueryClient();
  const modelSettingsQuery = useQuery({ queryKey: ['settings', 'model'],
    queryFn: () => desktop!.getModelSettings(), enabled: active && surface === 'work' && Boolean(desktop) });
  useEffect(() => {
    if (surface !== 'work' || !workConversationId) return;
    try {
      const saved = JSON.parse(window.localStorage.getItem(`yuanpu:model:${workConversationId}`) ?? 'null') as AgentRunRequest['modelSelection'] | null;
      setModelSelection(saved?.provider && saved.model
        ? { ...saved, thinkingLevel: threeStepThinking(saved.thinkingLevel) } : undefined);
    } catch { setModelSelection(undefined); }
  }, [surface, workConversationId]);
  useEffect(() => {
    if (surface !== 'work' || !workConversationId) return;
    try {
      setUnrestrictedConversationId(window.localStorage.getItem(`yuanpu:full-access:${workConversationId}`) === 'true'
        ? workConversationId : undefined);
    } catch { setUnrestrictedConversationId(undefined); }
  }, [surface, workConversationId]);
  useEffect(() => {
    if (!desktop || surface !== 'work' || !active || !workConversationId) {
      setTrajectory(undefined);
      return;
    }
    let stopped = false;
    let timer: number | undefined;
    const refresh = async () => {
      try {
        const next = await desktop.getWorkTrajectory(workConversationId);
        if (!stopped) {
          if (next.conversationId === workConversationId) {
            const liveCalls = next.live?.tools
              .filter((tool) => tool.name === 'ai_ticket')
              .map((tool) => tool.id) ?? [];
            const ids = new Set([...next.rows.filter((row) => row.kind === 'subagent' && row.toolCallId)
              .map((row) => row.toolCallId!), ...liveCalls]);
            const previous = observedTicketCalls.current;
            const current = previous?.conversationId === workConversationId ? previous : undefined;
            if (current ? [...ids].some((id) => !current.ids.has(id)) : liveCalls.length > 0) openAiTickets();
            observedTicketCalls.current = { conversationId: workConversationId, ids };
          }
          if (next.live) latestProcess.current.set(next.live.runId, next.live.tools);
          setTrajectory(next);
        }
      } catch { /* Keep the last readable snapshot during a transient disconnect. */ }
      if (!stopped) timer = window.setTimeout(() => void refresh(), busy ? 450
        : ((activityOpen && (activityTab === 'trajectory' || activityTab === 'subagents')) || contextOpen ? 2_000 : 15_000));
    };
    void refresh();
    return () => { stopped = true; if (timer !== undefined) window.clearTimeout(timer); };
  }, [desktop, surface, active, workConversationId, busy, activityOpen, activityTab, contextOpen]);
  const workConversationsQuery = useQuery({
    queryKey: ['work', 'conversations'],
    queryFn: () => desktop!.listWorkConversations(),
    enabled: active && surface === 'work' && Boolean(desktop),
  });
  const workFoldersQuery = useQuery({
    queryKey: ['work', 'folders'], queryFn: () => desktop!.listWorkFolders(),
    enabled: active && surface === 'work' && Boolean(desktop),
  });
  const workTagsQuery = useQuery({
    queryKey: ['work', 'tags'], queryFn: () => desktop!.listWorkTags(),
    enabled: active && surface === 'work' && Boolean(desktop),
  });
  const selectedWork = workConversationsQuery.data?.find((item) => item.id === workConversationId);
  const workArchived = surface === 'work' && Boolean(selectedWork?.archived || workConversationId === 'default');
  const workTreeLocked = busy || Boolean(runRecovery) || Boolean(approvalBusy)
    || activeRunStatus === 'queued' || activeRunStatus === 'running' || activeRunStatus === 'waiting_approval';
  const workPath = selectedWork ? folderPath(workFoldersQuery.data ?? [], selectedWork.folderId) : [];
  useEffect(() => {
    if (surface !== 'work' || !workConversationsQuery.data || workConversationId) return;
    setWorkConversationId(workConversationsQuery.data.find((item) => item.current)?.id);
  }, [surface, workConversationsQuery.data, workConversationId]);
  const hotkeyQuery = useQuery({
    queryKey: ['settings', 'hotkeys'],
    queryFn: () => desktop!.getHotkeySettings(),
    enabled: active && Boolean(desktop),
  });
  const transcriptQuery = useQuery({
    queryKey: ['assistant', 'transcript', surface, workConversationId],
    queryFn: () => desktop!.getDesktopTranscript(surface === 'assistant' ? 'assistantAll' : surface,
      surface === 'work' ? workConversationId : undefined,
      undefined, surface === 'work' ? 31 : undefined),
    enabled: active && Boolean(desktop) && (surface !== 'work' || Boolean(workConversationId)),
    refetchInterval: active ? 3000 : false,
  });
  const assistantLinkQuery = useQuery({
    queryKey: ['assistant', 'link'],
    queryFn: () => desktop!.getAssistantLink(),
    enabled: active && surface === 'assistant' && Boolean(desktop),
    refetchInterval: active && surface === 'assistant' ? 5000 : false,
  });
  const archiveQuery = useQuery({
    queryKey: ['assistant', 'archive'],
    queryFn: () => desktop!.getDesktopTranscript('assistantArchive'),
    enabled: active && surface === 'assistant' && archiveOpen && Boolean(desktop),
  });
  const mirrorQuery = useQuery({
    queryKey: ['assistant', 'mirrors', activeRunId],
    queryFn: () => desktop!.listAssistantMirrors(activeRunId!),
    enabled: active && surface === 'assistant' && Boolean(desktop) && Boolean(activeRunId),
    refetchInterval: active && surface === 'assistant' && activeRunId ? 3000 : false,
  });
  const appliedTranscript = useRef('');
  const transcriptData = useMemo(() => surface === 'work'
    ? [...olderMessages, ...(transcriptQuery.data ?? []).slice(-30)]
    : transcriptQuery.data, [surface, olderMessages, transcriptQuery.data]);
  const canLoadOlder = surface === 'work' && Boolean(workConversationId) && !searchWindow && !archiveOpen
    && !busy && !olderLoading && olderHasMore && (transcriptQuery.data?.length ?? 0) >= 31;

  async function loadOlderMessages() {
    if (!desktop || !canLoadOlder || olderLoadingRef.current || prependScrollAnchor.current || !workConversationId) return;
    const firstId = transcriptData?.[0]?.id;
    if (!firstId) return;
    const generation = olderLoadGeneration.current;
    const conversationId = workConversationId;
    olderLoadingRef.current = true;
    setOlderLoading(true);
    try {
      const page = await desktop.getDesktopTranscript('work', conversationId, firstId, 31);
      if (generation !== olderLoadGeneration.current) return;
      if (page.length) {
        const pane = conversation.current;
        if (pane) prependScrollAnchor.current = { height: pane.scrollHeight, top: pane.scrollTop };
        setOlderMessages((current) => [...page.slice(-30), ...current]);
      }
      setOlderHasMore(page.length >= 31);
    } finally {
      if (generation === olderLoadGeneration.current) {
        olderLoadingRef.current = false;
        setOlderLoading(false);
      }
    }
  }

  useEffect(() => {
    if (surface !== 'work' || !workConversationId || !transcriptQuery.data) return;
    const latest = transcriptQuery.data.slice(-30);
    const previous = latestWindowRef.current;
    latestWindowRef.current = { conversationId: workConversationId, messages: latest };
    if (!previous || previous.conversationId !== workConversationId) return;
    const currentIds = new Set(latest.map((message) => message.id));
    const displaced = previous.messages.filter((message) => !currentIds.has(message.id));
    if (displaced.length) setOlderMessages((current) => {
      if (!current.length) return current;
      const known = new Set(current.map((message) => message.id));
      return [...current, ...displaced.filter((message) => !known.has(message.id))];
    });
  }, [surface, workConversationId, transcriptQuery.data]);

  useEffect(() => {
    if (searchWindow && searchWindow.conversationId === workConversationId) return;
    if (busy || !transcriptData) return;
    const key = `${workConversationId ?? surface}:${assistantLinkQuery.data?.contactId ?? 'local'}:${JSON.stringify(transcriptData)}`;
    if (key === appliedTranscript.current) return;
    appliedTranscript.current = key;
    nextId.current = Math.max(nextId.current, transcriptData.length + 1);
    setMessages((current) => {
      const notices = current.filter((message) => message.role === 'notice');
      const regular = current.filter((message) => message.role !== 'notice');
      const previousByEntryId = new Map(regular.filter((message) => message.entryId)
        .map((message) => [message.entryId!, message]));
      const refreshed: ChatMessage[] = transcriptData.length
      ? transcriptData.map((item) => {
        const previous = previousByEntryId.get(item.id);
        const sameMessage = previous?.role === item.role && previous.text === item.text;
        const run = item.role === 'assistant' ? item.run ?? (sameMessage ? previous?.run : undefined)
          ?? findReplyRun(surface, item.id, item.text, item.at) : undefined;
        return sameMessage ? { ...previous, entryId: item.id, at: item.at, channel: item.channel, run }
          : { id: nextId.current++, entryId: item.id, role: item.role, text: item.text,
            at: item.at, channel: item.channel, run };
      })
      : workArchived ? [] : surface === 'assistant'
        ? [{ ...initialMessages[0]!, text: assistantGreeting }]
        : initialMessages;
      for (const notice of notices) {
        if (notice.run?.runId && refreshed.some((message) => message.run?.runId === notice.run!.runId)) continue;
        const at = new Date(notice.at ?? '').getTime();
        const index = refreshed.findIndex((message) => Boolean(message.at) && new Date(message.at!).getTime() > at);
        refreshed.splice(index < 0 ? refreshed.length : index, 0, notice);
      }
      return refreshed;
    });
  }, [busy, transcriptData, assistantLinkQuery.data?.contactId, surface, workConversationId, workArchived, searchWindow]);

  useEffect(() => {
    if (!searchWindow || searchWindow.conversationId !== workConversationId) return;
    const frame = window.requestAnimationFrame(() => {
      const target = Array.from(conversationInner.current?.querySelectorAll<HTMLElement>('[data-message-entry-id]') ?? [])
        .find((element) => element.dataset.messageEntryId === searchWindow.entryId);
      target?.scrollIntoView({ block: 'center', behavior: 'smooth' });
      target?.classList.add('work-message-located');
    });
    return () => window.cancelAnimationFrame(frame);
  }, [searchWindow, workConversationId]);

  useEffect(() => {
    try { window.localStorage.setItem(`yuanpu:draft:${surface}`, input); }
    catch { /* Storage can be unavailable in a restricted preview. */ }
  }, [input, surface]);

  useEffect(() => {
    if (!active || !chatPanel.current) return;
    const panel = chatPanel.current;
    const fit = () => {
      const width = panel.getBoundingClientRect().width;
      if (width < 260) return;
      if (surface === 'assistant') {
        // Preserve the split ratio when the window or left list changes size.
        setRightPanelWidth(rightPanelSpace(width, listOpen, false) * assistantPanelRatio.current);
      } else {
        const maximum = maxRightPanelWidth(width, listOpen, true);
        if (maximum < 260) return;
        setRightPanelWidth((current) => Math.min(Math.max(current, 260), maximum));
      }
    };
    fit();
    const observer = new ResizeObserver(fit);
    observer.observe(panel);
    return () => observer.disconnect();
  }, [active, listOpen, surface]);

  useEffect(() => () => rightPanelDragCleanup.current?.(), []);

  useEffect(() => {
    if (surface !== 'work' || !conversationInner.current) return;
    const content = conversationInner.current;
    const update = () => setWatermarkCount(Math.max(1, Math.ceil((content.scrollHeight - 400) / 800)));
    update();
    const observer = new ResizeObserver(update);
    observer.observe(content);
    return () => observer.disconnect();
  }, [surface]);

  useEffect(() => {
    const narrow = window.matchMedia('(max-width: 560px)');
    const compact = window.matchMedia('(max-width: 780px)');
    const handleWidthChange = (event: MediaQueryListEvent) => {
      if (event.matches) setActivityOpen(false);
    };
    narrow.addEventListener('change', handleWidthChange);
    compact.addEventListener('change', handleWidthChange);
    return () => {
      narrow.removeEventListener('change', handleWidthChange);
      compact.removeEventListener('change', handleWidthChange);
    };
  }, []);

  useEffect(() => {
    const dialog = activityDialog.current;
    if (activityOpen && active && dialog) {
      dialog.open = true;
      return () => dialog.close();
    }
    if (!activityOpen && active && restoreActivityFocus.current) {
      activityToggle.current?.focus();
      restoreActivityFocus.current = false;
    }
  }, [activityOpen, active, surface]);

  function recordActivity(runId: string, title: string, tone: ActivityEvent['tone'], detail?: string, at = new Date().toISOString()) {
    const event = { id: nextActivityId.current++, runId, title, tone, detail, at };
    activityEventsRef.current = [...activityEventsRef.current, event];
  }

  function showStoppedNotice(run: AgentRunRecord) {
    if (stoppedRuns.current.has(run.runId)) return;
    stoppedRuns.current.add(run.runId);
    requestedStops.current.delete(run.runId);
    setMessages((current) => [...current, {
      id: nextNoticeId.current--,
      role: 'notice',
      text: stoppedNotice(run),
      process: latestProcess.current.get(run.runId),
      at: run.updatedAt,
      run: {
        runId: run.runId,
        status: run.status,
        createdAt: run.createdAt,
        updatedAt: run.updatedAt,
        events: activityEventsRef.current.filter((event) => event.runId === run.runId),
        tools: run.output?.tools ?? [],
        steps: run.output?.steps ?? run.progress?.steps
          ?? (trajectory?.live?.runId === run.runId ? trajectory.live.steps : undefined),
      },
    }]);
  }

  async function refreshApprovals() {
    if (!desktop) return [];
    const requestedContext = approvalContext;
    const pendingApprovals = await desktop.listCapabilityApprovals();
    const ready = await Promise.all(pendingApprovals.map(async (approval) => {
      if (approval.assistantDelegation) {
        return isVisibleAssistantDelegationApproval(approval, surface)
          ? approval.requestId : undefined;
      }
      if (isVisibleAssistantDelegationApproval(approval, surface)) return approval.requestId;
      if (!approval.runId) return undefined;
      const run = await desktop.getAgentRun(approval.runId).catch(() => undefined);
      if (!run) return undefined;
      const visibleHere = navigationTarget?.runId === run.runId || (
        run.owner.entryPoint === 'desktop'
        && run.context.conversation.conversationId === (surface === 'work' ? workConversationId : 'assistant')
      );
      return visibleHere && run.status === 'waiting_approval' && run.pendingApproval?.approvalRequestId === approval.requestId
        ? approval.requestId : undefined;
    }));
    if (requestedContext !== approvalContextRef.current) return [];
    const visibleIds = new Set(ready.filter((id): id is string => Boolean(id)));
    readyApprovalContextRef.current = requestedContext;
    setBridgeError(false);
    setReadyApprovals(visibleIds);
    const visible = pendingApprovals.filter((approval) => visibleIds.has(approval.requestId)
      && !resolvedApprovals.current.has(approval.requestId));
    setApprovals(visible);
    return visible;
  }

  useEffect(() => {
    if (!desktop || !active) return;
    const refresh = () => void refreshApprovals().catch(() => setBridgeError(true));
    refresh();
    const timer = window.setInterval(refresh, 1_500);
    return () => window.clearInterval(timer);
  }, [desktop, active, surface, workConversationId, navigationTarget?.runId]);

  useEffect(() => {
    const runId = navigationTarget?.runId;
    if (!desktop || !runId) {
      setLocatedRun(undefined);
      setPrivateImSummary(undefined);
      return;
    }
    let cancelled = false;
    setLocatedRun('loading');
    setPrivateImSummary('loading');
    void desktop.getAgentRun(runId).then(
      (run) => { if (!cancelled) setLocatedRun(run); },
      () => { if (!cancelled) setLocatedRun('error'); },
    );
    void desktop.getPrivateImRunSummary(runId).then(
      (summary) => { if (!cancelled) setPrivateImSummary(summary); },
      () => { if (!cancelled) setPrivateImSummary(undefined); },
    );
    return () => { cancelled = true; };
  }, [desktop, navigationTarget?.runId, locationRetry]);

  useEffect(() => {
    if (!desktop || !active || !navigationTarget?.runId || !locatedRun || typeof locatedRun === 'string') return;
    if (['succeeded', 'failed', 'cancelled', 'interrupted', 'result_unknown'].includes(locatedRun.status)) return;
    let stale = false;
    const timer = window.setInterval(() => {
      void desktop.getAgentRun(navigationTarget.runId!).then((run) => {
        if (!stale) setLocatedRun(settledRuns.current.get(run.runId) ?? run);
      }).catch(() => { if (!stale) setLocatedRun('error'); });
    }, 1_200);
    return () => { stale = true; window.clearInterval(timer); };
  }, [desktop, active, navigationTarget?.runId, locatedRun]);

  useEffect(() => {
    if (!desktop || !active || !navigationTarget?.runId || !privateImSummary || privateImSummary === 'loading') return;
    if (!['not_created', 'pending', 'delivering'].includes(privateImSummary.replyDeliveryStatus)) return;
    const timer = window.setInterval(() => {
      void desktop.getPrivateImRunSummary(navigationTarget.runId!).then(setPrivateImSummary).catch(() => undefined);
    }, 1_200);
    return () => window.clearInterval(timer);
  }, [desktop, active, navigationTarget?.runId, privateImSummary]);

  useLayoutEffect(() => {
    const pane = conversation.current;
    if (!pane) return;
    const anchor = prependScrollAnchor.current;
    if (anchor) {
      pane.scrollTop = anchor.top + pane.scrollHeight - anchor.height;
      prependScrollAnchor.current = undefined;
    } else if (followConversationBottom.current) {
      pane.scrollTop = pane.scrollHeight;
    }
  }, [messages, busy, trajectory?.live?.text]);

  useLayoutEffect(() => {
    const pane = conversation.current;
    const content = conversationInner.current;
    if (!pane || !content || typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver(() => {
      if (followConversationBottom.current && !prependScrollAnchor.current) pane.scrollTop = pane.scrollHeight;
    });
    observer.observe(pane);
    observer.observe(content);
    return () => observer.disconnect();
  }, [surface, workConversationId]);

  useEffect(() => {
    if (navigationTarget?.runId && locatedRun && typeof locatedRun !== 'string') {
      conversation.current?.scrollTo({ top: 0, behavior: 'smooth' });
    }
  }, [navigationTarget?.runId, locatedRun]);

  async function observeRun(runId: string, text: string) {
    if (!desktop) return;
    let lastSeenStatus: AgentRunRecord['status'] | undefined;
    try {
      while (true) {
        const fetched = await desktop.getAgentRun(runId);
        const run = settledRuns.current.get(runId) ?? fetched;
        const terminal = ['succeeded', 'failed', 'cancelled', 'interrupted', 'result_unknown'].includes(run.status);
        if (terminal) settledRuns.current.set(runId, run);
        if (terminal) setReviewRefreshKey((value) => value + 1);
        setLastRun(run);
        setActiveRunStatus(run.status);
        if (run.status !== lastSeenStatus) {
          const tone: ActivityEvent['tone'] = run.status === 'succeeded' ? 'done'
            : ['failed', 'interrupted', 'result_unknown'].includes(run.status) ? 'error'
              : ['waiting_approval', 'cancelled'].includes(run.status) ? 'warning' : 'active';
          recordActivity(run.runId, runStatusLabel(run.status), tone, run.failure?.message, run.updatedAt);
          lastSeenStatus = run.status;
        }
        if (terminal) {
          run.output?.tools.forEach((tool) => recordActivity(run.runId,
            tool.status === 'completed' ? '工具调用完成' : '工具调用失败',
            tool.status === 'completed' ? 'done' : 'error', capabilityName(tool.name), run.updatedAt));
          const replyText = run.status === 'succeeded' ? run.output?.message ?? '任务已完成；可在运行记录中查看结果。'
            : run.failure?.message ?? `任务结束：${runStatusLabel(run.status)}`;
          const replyRun = cacheReplyRun(surface, replyText, run,
            activityEventsRef.current.filter((event) => event.runId === run.runId));
          const stoppedByUser = run.status === 'cancelled'
            && (requestedStops.current.has(run.runId) || stoppedRuns.current.has(run.runId));
          const process = latestProcess.current.get(run.runId);
          if (stoppedByUser) showStoppedNotice(run);
          else setMessages((current) => [...current, {
            id: nextId.current++, role: run.status === 'succeeded' ? 'assistant' : 'error',
            text: replyText,
            tools: run.output?.tools.map((tool) => ({ name: tool.name, status: tool.status })),
            process,
            run: replyRun,
          }]);
          latestProcess.current.delete(run.runId);
          if (run.status !== 'succeeded' && !stoppedByUser) setInput((current) => current || text);
          setRunRecovery(undefined);
          setActiveRunId(undefined);
          setActiveRunStatus(undefined);
          return;
        }
        await new Promise((resolveWait) => window.setTimeout(resolveWait, 900));
      }
    } catch (error) {
      // The accepted run may still be executing. Resume observation, never resubmit it.
      setRunRecovery({ runId, text });
      recordActivity(runId, '状态获取失败', 'error', formatError(error));
    }
  }

  async function resumeRun() {
    if (!runRecovery || sending.current) return;
    sending.current = true;
    setBusy(true);
    try { await observeRun(runRecovery.runId, runRecovery.text); }
    finally { sending.current = false; setBusy(false); }
  }

  async function openWorkConversation(item: WorkConversation): Promise<boolean> {
    if (!desktop || workTreeLocked) return false;
    if (item.id === workConversationId) return true;
    setWorkListError('');
    try {
      await desktop.selectWorkConversation(item.id, item.archived ? true : undefined);
      restoreSidebarFor(item.id);
      setWorkConversationId(item.id);
      olderLoadGeneration.current += 1;
      olderLoadingRef.current = false;
      setOlderLoading(false);
      latestWindowRef.current = undefined;
      setOlderMessages([]);
      setOlderHasMore(true);
      followConversationBottom.current = true;
      prependScrollAnchor.current = undefined;
      setSearchWindow(undefined);
      setSearchLocationError('');
      setMessages([]);
      setApprovals([]);
      setReadyApprovals(new Set());
      readyApprovalContextRef.current = '';
      setInput('');
      setAttachments([]);
      setAttachmentError('');
      setLastRun(undefined);
      setActiveRunId(undefined);
      activityEventsRef.current = [];
      appliedTranscript.current = '';
      setFilePreviewPath(undefined);
      setImagePreviewRequest(undefined);
      void queryClient.invalidateQueries({ queryKey: ['work', 'conversations'] });
      return true;
    } catch (error) { setWorkListError(formatError(error)); return false; }
  }

  useEffect(() => {
    if (!assistantWorkConversationId) { sourceNavigationRef.current = undefined; return; }
    if (surface !== 'work' || !active || !desktop || !workConversationsQuery.data
      || sourceNavigationRef.current === assistantWorkConversationId) return;
    sourceNavigationRef.current = assistantWorkConversationId;
    const item = workConversationsQuery.data.find((candidate) => candidate.id === assistantWorkConversationId);
    if (!item) {
      setWorkListError('对应的工作会话不可用。');
      clearAssistantWorkConversation?.();
      return;
    }
    void openWorkConversation(item).finally(() => clearAssistantWorkConversation?.());
  }, [active, assistantWorkConversationId, clearAssistantWorkConversation,
    desktop, surface, workConversationsQuery.data]);

  async function newWorkConversation(folderId?: string) {
    if (!desktop || workTreeLocked) return;
    setWorkListError('');
    try {
      const pendingRequest = pendingWorkCreateRequest.current;
      const requestId = pendingRequest && pendingRequest.folderId === folderId
        ? pendingRequest.requestId : crypto.randomUUID();
      pendingWorkCreateRequest.current = { folderId, requestId };
      const item = await desktop.createWorkConversation(folderId, requestId);
      pendingWorkCreateRequest.current = undefined;
      restoreSidebarFor(item.id);
      setWorkConversationId(item.id);
      olderLoadGeneration.current += 1;
      olderLoadingRef.current = false;
      setOlderLoading(false);
      latestWindowRef.current = undefined;
      setOlderMessages([]);
      setOlderHasMore(true);
      followConversationBottom.current = true;
      prependScrollAnchor.current = undefined;
      setSearchWindow(undefined);
      setMessages([initialMessages[0]!]);
      setApprovals([]);
      setReadyApprovals(new Set());
      readyApprovalContextRef.current = '';
      setInput('');
      setAttachments([]);
      setAttachmentError('');
      setLastRun(undefined);
      setActiveRunId(undefined);
      activityEventsRef.current = [];
      appliedTranscript.current = '';
      setFilePreviewPath(undefined);
      setImagePreviewRequest(undefined);
      void queryClient.invalidateQueries({ queryKey: ['work', 'conversations'] });
    } catch (error) { setWorkListError(formatError(error)); }
  }

  async function saveTopRename() {
    if (!topRenaming || !desktop || !selectedWork || selectedWork.id === 'default') return;
    const title = topRenameDraft.trim();
    if (!title) { setWorkListError('会话名称不能为空。'); setTopRenaming(false); return; }
    setTopRenaming(false);
    if (title === selectedWork.title) return;
    try {
      await desktop.updateWorkConversation(selectedWork.id, { title });
      void queryClient.invalidateQueries({ queryKey: ['work', 'conversations'] });
      setWorkListError('');
    } catch (error) { setWorkListError(`重命名失败：${formatError(error)}`); }
  }

  async function openWorkSearchHit(hit: WorkSearchItem): Promise<void> {
    if (!desktop || workTreeLocked) return;
    const item = workConversationsQuery.data?.find((conversation) => conversation.id === hit.conversationId);
    if (!item || !(await openWorkConversation(item))) return;
    setSearchLocationError('');
    if (!hit.messageEntryId) return;
    try {
      const result = await desktop.getWorkMessageWindow(hit.conversationId, hit.messageEntryId, 20);
      if (result.status !== 'ok') {
        setSearchLocationError('这条历史消息暂时无法定位。可重新搜索或打开该会话查看最近消息。');
        return;
      }
      setSearchWindow({ conversationId: hit.conversationId, entryId: hit.messageEntryId, result });
    } catch (error) { setSearchLocationError(`消息定位失败：${formatError(error)}`); }
  }

  async function sendMessage() {
    if (searchWindow) setSearchWindow(undefined);
    const draft = input.trim();
    const includedAttachments = attachments;
    if ((!draft && !includedAttachments.length) || sending.current || runRecovery || workArchived
      || (surface === 'work' && desktop && !workConversationId)) return;
    const text = includedAttachments.length
      ? `${draft || '请阅读附件。'}\n\n${includedAttachments.map((file) => `附件 ${file.name}：\n${file.contents}`).join('\n\n')}`
      : draft;
    sending.current = true;
    followConversationBottom.current = true;
    setMessages((current) => [...current, { id: nextId.current++, role: 'user', text, at: new Date().toISOString() }]);
    setInput('');
    setAttachments([]);
    setBusy(true);
    setLastRun(undefined);
    setActiveRunId(undefined);
    setSubmittedTask(undefined);
    setActiveRunStatus(undefined);
    try {
      if (!desktop) {
        setMessages((current) => [...current, { id: nextId.current++, role: 'assistant',
          text: '这是浏览器预览回复。通过桌面应用启动后，消息会交给本地助理处理。' }]);
      } else {
        let clientMessageId: string | undefined;
        if (surface === 'assistant') {
          if (!pendingAssistantMessage.current) {
            try {
              const saved = JSON.parse(window.localStorage.getItem('yuanpu:assistant-pending-message') ?? 'null') as
                { text?: unknown; id?: unknown; createdAt?: unknown } | null;
              if (saved?.text === text && typeof saved.id === 'string'
                && /^[A-Za-z0-9_-]{1,128}$/.test(saved.id)
                && typeof saved.createdAt === 'number'
                && Date.now() - saved.createdAt < 300_000 && saved.createdAt <= Date.now()) {
                pendingAssistantMessage.current = { text, id: saved.id, createdAt: saved.createdAt };
              }
            } catch { /* invalid local retry record */ }
          }
          if (pendingAssistantMessage.current?.text !== text
            || Date.now() - pendingAssistantMessage.current.createdAt >= 300_000) {
            pendingAssistantMessage.current = { text, id: crypto.randomUUID(), createdAt: Date.now() };
          }
          clientMessageId = pendingAssistantMessage.current.id;
          try { window.localStorage.setItem('yuanpu:assistant-pending-message',
            JSON.stringify(pendingAssistantMessage.current)); } catch { /* optional retry cache */ }
        }
        const receipt = await desktop.submitDesktopMessage(text, surface,
          surface === 'work' ? workConversationId : undefined, clientMessageId,
          surface === 'work' ? modelSelection : undefined,
          surface === 'work' && unrestrictedConversationId === workConversationId ? 'unrestricted' : undefined);
        if (surface === 'assistant') {
          pendingAssistantMessage.current = undefined;
          try { window.localStorage.removeItem('yuanpu:assistant-pending-message'); } catch { /* optional retry cache */ }
        }
        setSubmittedTask({ runId: receipt.runId, text: draft || '请阅读附件' });
        if (surface === 'work') void queryClient.invalidateQueries({ queryKey: ['work', 'conversations'] });
        setActiveRunId(receipt.runId);
        setActiveRunStatus(receipt.status);
        recordActivity(receipt.runId, '已提交任务', 'done');
        await observeRun(receipt.runId, text);
      }
    } catch (error) {
      setMessages((current) => [...current, { id: nextId.current++, role: 'error', text: formatError(error) }]);
      recordActivity('submission', '提交失败', 'error', formatError(error));
      setInput((current) => current || draft);
      setAttachments(includedAttachments);
    } finally {
      sending.current = false;
      setBusy(false);
      void queryClient.invalidateQueries({ queryKey: ['assistant', 'transcript', surface, workConversationId] });
      if (surface === 'work') void queryClient.invalidateQueries({ queryKey: ['work', 'conversations'] });
      void refreshApprovals().catch(() => setBridgeError(true));
    }
  }

  async function addAttachments(files: FileList | null) {
    const selected = Array.from(files ?? []);
    if (attachmentInput.current) attachmentInput.current.value = '';
    const remaining = Math.max(0, 3 - attachments.length);
    if (selected.length > remaining) setAttachmentError('每条消息最多添加 3 个文本附件。');
    else setAttachmentError('');
    const added: Array<{ name: string; contents: string }> = [];
    for (const file of selected.slice(0, remaining)) {
      if (file.size > 100_000 || !/\.(txt|md|json|csv|ts|tsx|js|jsx|py|yaml|yml|xml|html|css)$/i.test(file.name)) {
        setAttachmentError('仅支持 100 KB 以内的文本附件。');
        continue;
      }
      added.push({ name: file.name, contents: await file.text() });
    }
    if (added.length) setAttachments((current) => [...current, ...added]);
  }

  async function copyUserMessage(message: ChatMessage) {
    try {
      await navigator.clipboard.writeText(message.text);
      setCopiedUserMessageId(message.id);
    } catch { setCopiedUserMessageId(undefined); }
  }

  async function cancelRun(runId: string, needsConfirmation = true) {
    if (!desktop || cancelInFlight.current || (needsConfirmation && !window.confirm('取消这个正在执行的任务？已经发生的外部操作无法撤销。'))) return;
    cancelInFlight.current = true;
    requestedStops.current.add(runId);
    setCancelBusy(true);
    try {
      const receipt = await desktop.cancelAgentRun(runId);
      if (receipt.result === 'not_found') throw new Error('任务不可用或无权取消。');
      if (receipt.result === 'already_terminal') requestedStops.current.delete(runId);
      const run = await desktop.getAgentRun(runId);
      if (['succeeded', 'failed', 'cancelled', 'interrupted', 'result_unknown'].includes(run.status)) {
        settledRuns.current.set(runId, run);
      }
      if (navigationTarget?.runId === runId) setLocatedRun(run);
      if (run.status === 'cancelled' && requestedStops.current.has(runId)) showStoppedNotice(run);
      if (runRecovery?.runId === runId) await resumeRun();
    } catch (error) {
      requestedStops.current.delete(runId);
      setMessages((current) => [...current, { id: nextId.current++, role: 'error', text: `取消失败：${formatError(error)}` }]);
    } finally {
      cancelInFlight.current = false;
      setCancelBusy(false);
    }
  }

  useEffect(() => {
    if (!active || !desktop) return;
    const shortcut = effectiveHotkeyBinding(hotkeyQuery.data, 'conversation.interrupt');
    if (!shortcut) return;
    const onKeyDown = (event: globalThis.KeyboardEvent) => {
      if (event.repeat || event.isComposing || event.defaultPrevented || bindingFromKeyEvent(event) !== shortcut
        || document.querySelector('dialog:modal')
        || (event.target instanceof Element && event.target.closest('[data-hotkey-recording]'))) return;
      const locatedIsRunning = locatedRun && typeof locatedRun !== 'string'
        && ['queued', 'running', 'waiting_approval'].includes(locatedRun.status);
      const runId = navigationTarget?.runId
        ? (locatedIsRunning ? locatedRun.runId : undefined)
        : activeRunId && (!activeRunStatus || ['queued', 'running', 'waiting_approval'].includes(activeRunStatus))
          ? activeRunId : runRecovery?.runId;
      if (!runId || cancelInFlight.current) return;
      event.preventDefault();
      event.stopPropagation();
      void cancelRun(runId, false);
    };
    window.addEventListener('keydown', onKeyDown, true);
    return () => window.removeEventListener('keydown', onKeyDown, true);
  }, [active, desktop, hotkeyQuery.data, activeRunId, activeRunStatus, locatedRun, navigationTarget?.runId, runRecovery?.runId]);

  async function decideApproval(
    approval: CapabilityApprovalSummary,
    decision: 'approved' | 'denied',
  ) {
    if (!desktop || approvalBusy || !readyApprovals.has(approval.requestId)
      || readyApprovalContextRef.current !== approvalContextRef.current) return;
    setApprovalBusy(approval.requestId);
    try {
      const result = await desktop.decideCapabilityApproval(approval.requestId, decision);
      resolvedApprovals.current.add(approval.requestId);
      setApprovals((current) => current.filter((item) => item.requestId !== approval.requestId));
      if (approval.runId && [activeRunId, lastRun?.runId, navigationTarget?.runId].includes(approval.runId)) {
        recordActivity(approval.runId, decision === 'approved' ? '已允许一次' : '已拒绝授权', decision === 'approved' ? 'done' : 'warning', capabilityName(approval.capabilityId));
      }
      if (decision === 'denied') {
        setMessages((current) => [...current, {
          id: nextId.current++,
          role: 'assistant',
          text: approval.assistantDelegation
            ? `已拒绝专业任务 ${approval.assistantDelegation.taskId}，未启动执行。`
            : `已拒绝${capabilityName(approval.capabilityId)}，没有执行这次操作。`,
        }]);
        return;
      }
      if (approval.runId === activeRunId) {
        await refreshApprovals();
        return;
      }
      setMessages((current) => [...current, {
        id: nextId.current++,
        role: 'assistant',
        text: result.message ?? `${capabilityName(approval.capabilityId)}已执行。`,
      }]);
      await refreshApprovals();
    } catch (error) {
      setMessages((current) => [...current, {
        id: nextId.current++,
        role: 'error',
        text: `审批处理失败：${formatError(error)}`,
      }]);
    } finally {
      setApprovalBusy(undefined);
    }
  }

  function handleKeyDown(event: KeyboardEvent<HTMLTextAreaElement>) {
    if (acceptComposerPlaceholder(event, setInput)) return;
    if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing && event.keyCode !== 229) {
      event.preventDefault();
      void sendMessage();
    }
  }

  function openWorkspaceFile(rawPath: string) {
    if (surface !== 'work' || !workConversationId) return;
    setActivityOpen(true);
    setActivityTab('files');
    setFilePreviewPath(normalizeWorkspacePath(rawPath) ?? rawPath);
  }

  const resolveWorkspaceImage = useCallback(async (path: string): Promise<string | null> => {
    if (surface !== 'work' || !desktop || !workConversationId) return null;
    const preview = await desktop.readWorkFile(workConversationId, path);
    if (preview.kind !== 'image' || !/^image\/(?:png|jpeg|gif|webp|avif)$/i.test(preview.mediaType)) return null;
    return `data:${preview.mediaType};base64,${preview.base64}`;
  }, [desktop, surface, workConversationId]);

  function openImageInSidebar(image: ImagePreview) {
    setImagePreviewRequest({ ...image, id: crypto.randomUUID() });
    setActivityOpen(true);
    if (surface === 'work') setActivityTab('files');
  }

  function addPreviewToConversation(markdown: string) {
    setInput((current) => `${current.trimEnd()}${current.trim() ? '\n\n' : ''}${markdown}`);
    composerInput.current?.focus();
  }

  const [browserRequest, setBrowserRequest] = useState<{ conversationId: string; serial: number }>();
  const browserHost: ViewerBrowserHost | undefined = useMemo(() => surface === 'work' && desktop
    ? {
      attachGuest: (payload) => desktop.browserAttachGuest(payload),
      detachGuest: (key) => desktop.browserDetachGuest(key),
      openInSystemBrowser: (url) => desktop.openInSystemBrowser(url),
      onGuestCrashed: () => () => {},
      onSessionRequest: () => () => {},
    }
    : undefined, [desktop, surface]);

  useEffect(() => {
    if (surface !== 'work' || !desktop) return;
    return desktop.onBrowserSessionRequest((conversationId) => {
      if (conversationId !== workConversationId) return;
      setActivityOpen(true);
      setActivityTab('files');
      setBrowserRequest((value) => ({ conversationId, serial: (value?.serial ?? 0) + 1 }));
    });
  }, [desktop, surface, workConversationId]);

  const visibleRun = navigationTarget?.runId
    ? (locatedRun && typeof locatedRun !== 'string' ? locatedRun : undefined)
    : lastRun;
  const currentStatus = visibleRun?.status ?? (navigationTarget?.runId ? undefined : activeRunStatus);
  const activeLive = activeRunId && trajectory?.live?.runId === activeRunId ? trajectory.live : undefined;
  const activeProgress = surface === 'work' ? activeLive
    : lastRun?.runId === activeRunId ? lastRun?.progress : undefined;
  const activeReplyRun: ReplyRunInfo | undefined = lastRun && lastRun.runId === activeRunId ? {
    runId: lastRun.runId, status: lastRun.status, createdAt: lastRun.createdAt, updatedAt: lastRun.updatedAt,
    events: [], tools: [], steps: activeProgress?.steps,
  } : undefined;
  const configuredModel = modelSettingsQuery.data;
  const currentModel = modelSelection ?? (configuredModel?.provider && configuredModel.model
    ? { provider: configuredModel.provider, model: configuredModel.model } : undefined);
  const modelChoices = configuredModel ? [
    { provider: configuredModel.provider, model: configuredModel.model, name: configuredModel.model },
    ...configuredModel.customModels.map((item) => ({ provider: item.provider, model: item.model, name: item.name })),
  ].filter((item, index, all) => all.findIndex((other) => other.provider === item.provider && other.model === item.model) === index) : [];
  function chooseModel(next: NonNullable<AgentRunRequest['modelSelection']>) {
    setModelSelection(next);
    if (workConversationId) {
      try { window.localStorage.setItem(`yuanpu:model:${workConversationId}`, JSON.stringify(next)); }
      catch { /* The selection still applies to this session. */ }
    }
  }
  const fullAccessEnabled = surface === 'work' && unrestrictedConversationId === workConversationId;
  function toggleFullAccess() {
    if (surface !== 'work' || !workConversationId || busy || workArchived || archiveOpen) return;
    const next = !fullAccessEnabled;
    setUnrestrictedConversationId(next ? workConversationId : undefined);
    try {
      if (next) window.localStorage.setItem(`yuanpu:full-access:${workConversationId}`, 'true');
      else window.localStorage.removeItem(`yuanpu:full-access:${workConversationId}`);
    } catch { /* Current session retains the chosen mode. */ }
  }
  const emptyConversation = !archiveOpen && !busy && !locatedRun && !runRecovery
    && approvals.length === 0 && messages.length === 1 && messages[0]?.id === 1
    && messages[0].role === 'assistant'
    && messages[0].text === (surface === 'assistant' ? assistantGreeting : initialMessages[0]?.text);

  useEffect(() => {
    const pane = activityDialog.current;
    const header = workspaceTabHost?.closest<HTMLElement>('.chat-header');
    const controls = header?.querySelector<HTMLElement>('.runtime-meta');
    if (!pane || !header || !controls || !workspaceTabHost) return;
    function alignTabs() {
      if (!pane || !header || !controls || !workspaceTabHost) return;
      const style = getComputedStyle(header);
      const available = header.getBoundingClientRect().right - pane.getBoundingClientRect().left
        - parseFloat(style.paddingRight) - controls.getBoundingClientRect().width - parseFloat(style.columnGap);
      workspaceTabHost.style.flexBasis = `${Math.max(100, available)}px`;
    }
    const observer = new ResizeObserver(alignTabs);
    observer.observe(pane);
    observer.observe(header);
    observer.observe(controls);
    alignTabs();
    return () => observer.disconnect();
  }, [workspaceTabHost, activityOpen, rightPanelMaximized, active]);

  function resizeRightPanel(clientX: number, panel: HTMLElement) {
    const bounds = panel.getBoundingClientRect();
    const result = resizePanel(bounds.right - clientX, maxRightPanelWidth(bounds.width, listOpen, surface === 'work'),
      rightPanelGesture.current.startedAtLimit, rightPanelGesture.current.stopped);
    rightPanelGesture.current.stopped = result.stopped;
    if (surface === 'assistant') assistantPanelRatio.current = result.width / rightPanelSpace(bounds.width, listOpen, false);
    setRightPanelWidth(result.width);
    setRightPanelMaximized(result.maximized);
  }

  return (
    <section ref={chatPanel} className={`chat-panel ${surface}-mode ${emptyConversation && surface === 'work' ? 'is-empty' : ''} ${listOpen ? 'list-open' : 'list-closed'} ${activityOpen ? 'activity-open' : 'activity-closed'} ${rightPanelMaximized ? 'right-panel-maximized' : ''} ${listMaximized ? 'list-maximized' : ''} ${leftPanelResizing ? 'left-panel-resizing' : ''} ${rightPanelResizing ? 'right-panel-resizing' : ''} ${active ? '' : 'view-hidden'}`}
      style={{ '--yp-right-panel-width': `${rightPanelWidth}px`, '--yp-left-panel-width': `${leftPanelWidth}px` } as CSSProperties} aria-hidden={!active}>
      <PageToolbar active={active}><header className={`chat-header ${surface === 'work' && activityOpen ? 'has-workspace-tabs' : ''}`}
        style={{ '--workspace-toolbar-width': rightPanelMaximized ? 'calc(100% - 300px)' : `${rightPanelWidth}px` } as CSSProperties}>
          <div className="chat-heading">
            <button type="button" className="chat-list-toggle" title={`${listOpen ? '收起' : '打开'}${surface === 'work' ? '工作列表' : '会话列表'}`}
              aria-label={`${listOpen ? '收起' : '打开'}${surface === 'work' ? '工作列表' : '会话列表'}`} aria-expanded={listOpen}
              onClick={() => {
                if (listOpen) {
                  setListOpen(false);
                  setListMaximized(false);
                } else {
                  setListOpen(true);
                  setListMaximized(rightPanelMaximized);
                }
              }}><AppIcon name="panel-left" /></button>
            <span className="chat-toolbar-divider" aria-hidden="true" />
            <nav className="chat-breadcrumb" aria-label="会话位置">
              <span>{surface === 'work' ? '工作' : '助理'}</span><AppIcon name="chevron" />
              {surface === 'work' && workPath.map((folder) => <span className="chat-breadcrumb-part" key={folder.id} title={folder.name}>{folder.name}<AppIcon name="chevron" /></span>)}
              {surface === 'work' && topRenaming ? <input className="chat-title-input" autoFocus
                aria-label="会话名称" value={topRenameDraft} onChange={(event) => setTopRenameDraft(event.target.value)}
                onKeyDown={(event) => { if (event.key === 'Enter') event.currentTarget.blur(); if (event.key === 'Escape') setTopRenaming(false); }}
                onBlur={() => void saveTopRename()} />
                : <strong title={surface === 'work' ? (selectedWork?.title || '未命名会话') : undefined}>{surface === 'work'
                  ? workConversationId === 'default' ? '旧工作归档' : selectedWork?.title || '未命名会话'
                  : archiveOpen ? '原桌面会话' : scheduleOrigin ? '定时任务会话' : '当前会话'}</strong>}
            </nav>
            {surface === 'work' && selectedWork?.id !== 'default' && <button type="button" className="chat-rename-preview"
              disabled={!selectedWork || workTreeLocked} title="重命名会话" aria-label="重命名会话"
              onClick={() => { setTopRenameDraft(selectedWork?.title ?? ''); setTopRenaming(true); }}><AppIcon name="edit" /></button>}
          </div>
          {surface === 'work' && activityOpen && <div className="workspace-tab-host" ref={setWorkspaceTabHost} />}
          <div className="runtime-meta">
            {navigationTarget && (
              <span role="status">
                {locatedRun === 'loading' || (locatedRun === 'error' && privateImSummary === 'loading')
                  ? '正在加载任务记录…'
                  : privateImSummary && privateImSummary !== 'loading'
                    ? `企业微信 · ${privateImSummary.runStatus} · ${privateImDeliveryLabel(privateImSummary.replyDeliveryStatus)}`
                  : locatedRun === 'error'
                    ? '任务记录不可用'
                    : locatedRun
                      ? `${locatedRun.owner.entryPoint === 'scheduler' ? '定时任务' : locatedRun.owner.entryPoint === 'im' ? '企业微信' : '桌面'} · ${locatedRun.status} · 会话 ${locatedRun.context.conversation.conversationId}`
                      : `已定位会话 ${navigationTarget.conversationId}`}
              </span>
            )}
            {locatedRun === 'error' && <button type="button" className="runtime-link" onClick={() => setLocationRetry((value) => value + 1)}>重新获取任务</button>}
            {scheduleOrigin && <button type="button" className="runtime-link" onClick={onReturnToSchedules}>返回定时任务</button>}
            {locatedRun && typeof locatedRun !== 'string' && ['queued', 'running', 'waiting_approval'].includes(locatedRun.status) && locatedRun.owner.entryPoint !== 'im' && (
              <button type="button" className="runtime-link" disabled={cancelBusy} onClick={() => void cancelRun(locatedRun.runId)}>取消运行</button>
            )}
            {surface === 'work' && workConversationId && <button type="button" className="ai-ticket-toolbar-entry"
              aria-pressed={activityOpen && activityTab === 'subagents'} onClick={openAiTickets}>
              <AppIcon name="schedules" />任务工单</button>}
            {activityOpen && <button type="button" className="panel-maximize-toggle" title={rightPanelMaximized ? '还原右侧面板' : '铺满右侧面板'}
              aria-label={rightPanelMaximized ? '还原右侧面板' : '铺满右侧面板'} aria-pressed={rightPanelMaximized}
              onClick={() => {
                if (rightPanelMaximized) setRightPanelMaximized(false);
                else {
                  setRightPanelMaximized(true);
                  setListOpen(false);
                  setListMaximized(false);
                }
              }}><AppIcon name={rightPanelMaximized ? 'collapse' : 'expand'} /></button>}
            <button ref={activityToggle} type="button" className="panel-toggle" title={`${activityOpen ? '收起' : '打开'}${surface === 'assistant' ? '助理面板' : '右侧面板'}`}
              aria-label={`${activityOpen ? '收起' : '打开'}${surface === 'assistant' ? '助理面板' : '右侧面板'}`} aria-expanded={activityOpen}
              onClick={() => { restoreActivityFocus.current = true; if (activityOpen) setRightPanelMaximized(false); setActivityOpen((value) => !value); }}><AppIcon name="panel" /><span className="panel-toggle-label">{surface === 'assistant' ? '助理面板' : '右侧面板'}</span></button>
          </div>
          {surface === 'assistant' && <div className="assistant-channel-state" role="status">
            {archiveOpen ? '旧助理会话归档 · 只读' : assistantLinkQuery.data?.linked
              ? '桌面与企业微信消息汇总 · 上下文独立'
              : assistantLinkQuery.error ? '助理连接状态不可用' : '桌面助理 · 可在设置中绑定企业微信私聊'}
            <button type="button" className="runtime-link" onClick={() => setArchiveOpen((value) => !value)}>
              {archiveOpen ? '返回助理对话' : '查看旧助理会话'}
            </button>
          </div>}
      </header></PageToolbar>
      {listOpen && <aside className="conversation-list-preview" aria-label={surface === 'work' ? '工作列表' : '会话列表预览'}>
        {surface !== 'work' && <div className="conversation-list-heading"><strong>会话列表</strong></div>}
        {surface === 'work' ? <>
          {workConversationsQuery.isLoading && <p>正在读取工作列表…</p>}
          {workConversationsQuery.error && <p role="alert">工作列表读取失败：{formatError(workConversationsQuery.error)}</p>}
          {workFoldersQuery.error && <p role="alert">文件夹读取失败：{formatError(workFoldersQuery.error)}</p>}
          {workTagsQuery.error && <p role="alert">标签读取失败：{formatError(workTagsQuery.error)}</p>}
          {workListError && <p role="alert">{workListError}</p>}
          {desktop && <WorkTree desktop={desktop} conversations={workConversationsQuery.data ?? []}
            folders={workFoldersQuery.data ?? []} tags={workTagsQuery.data ?? []}
            currentId={workConversationId} locked={workTreeLocked} maximized={listMaximized}
            onToggleMaximized={() => {
              if (listMaximized) setListMaximized(false);
              else {
                setListMaximized(true);
                setActivityOpen(false);
                setRightPanelMaximized(false);
              }
            }}
            onOpen={openWorkConversation} onCreate={newWorkConversation} onSearchHit={openWorkSearchHit} />}
        </> : <><div className="conversation-list-current"><span>{archiveOpen ? '原桌面会话' : '当前会话'}</span><small>当前</small></div>
          <p>历史会话列表尚未接入，此处为界面预览。</p></>}
        {surface === 'work' && !listMaximized && <div className="left-panel-resize-handle" role="separator" tabIndex={0}
          aria-label="调整工作列表宽度" aria-orientation="vertical" aria-valuemin={240} aria-valuemax={720} aria-valuenow={leftPanelWidth}
          onPointerDown={(event) => {
            if (window.matchMedia('(max-width: 900px)').matches) return;
            event.preventDefault();
            leftPanelDragCleanup.current?.();
            setLeftPanelResizing(true);
            const pointerId = event.pointerId;
            const left = event.currentTarget.closest<HTMLElement>('.chat-panel')?.getBoundingClientRect().left ?? 0;
            const move = (moveEvent: PointerEvent) => {
              if (moveEvent.pointerId === pointerId) setLeftPanelWidth(Math.max(240, Math.min(720, moveEvent.clientX - left)));
            };
            const finish = (finishEvent: PointerEvent) => {
              if (finishEvent.pointerId !== pointerId) return;
              leftPanelDragCleanup.current?.();
              leftPanelDragCleanup.current = null;
              setLeftPanelResizing(false);
            };
            window.addEventListener('pointermove', move);
            window.addEventListener('pointerup', finish);
            window.addEventListener('pointercancel', finish);
            leftPanelDragCleanup.current = () => {
              window.removeEventListener('pointermove', move);
              window.removeEventListener('pointerup', finish);
              window.removeEventListener('pointercancel', finish);
            };
          }}
          onKeyDown={(event) => {
            if (event.key === 'ArrowLeft' || event.key === 'ArrowRight') {
              event.preventDefault();
              setLeftPanelWidth((width) => Math.max(240, Math.min(720, width + (event.key === 'ArrowRight' ? 20 : -20))));
            }
          }} />}
      </aside>}
      <div className="chat-main">
        {surface === 'work' && emptyConversation && <div className="mindlink-work-background" aria-hidden="true">
          <img src={mindlinkSeal} alt="" /><strong>元朴思联</strong><span>MindLink</span>
        </div>}

        <div className="conversation" ref={conversation} aria-live="polite" onScroll={() => {
          const pane = conversation.current;
          if (!pane) return;
          followConversationBottom.current = pane.scrollHeight - pane.scrollTop - pane.clientHeight < 80;
          if (pane.scrollTop < 120) void loadOlderMessages();
        }}>
          <div className="conversation-inner" ref={conversationInner}>
            {canLoadOlder && <button type="button" className="chat-load-older" disabled={olderLoading}
              onClick={() => void loadOlderMessages()}>加载更早消息</button>}
            {surface === 'work' && !emptyConversation && <div className="mindlink-work-background-track" aria-hidden="true">
              {Array.from({ length: watermarkCount }, (_, index) =>
                <div className="mindlink-work-background" key={index} style={{ top: `${400 + index * 800}px` }}>
                  <img src={mindlinkSeal} alt="" /><strong>元朴思联</strong><span>MindLink</span>
                </div>)}
            </div>}
            {emptyConversation && <div className="empty-chat">
              <div className="mindlink-welcome" aria-label="元朴思联 MindLink">
                <img src={mindlinkSeal} alt="" />
                <strong>元朴思联</strong><span>MindLink</span>
              </div>
              <div className="empty-chat-heading">
                {surface === 'assistant' && <div className="empty-chat-mark" aria-hidden="true"><AppIcon name="assistant" /></div>}
                <h1>{emo.welcome[surface].title}</h1>
              </div>
              <p>{messages[0]?.text}</p>
            </div>}
            {locatedRun && typeof locatedRun !== 'string' && (
              <article className="located-run-card">
                <div className="approval-heading"><span>运行详情</span><strong>{locatedRun.owner.entryPoint === 'scheduler' ? '定时任务' : locatedRun.owner.entryPoint === 'im' ? '企业微信会话' : '桌面对话'}</strong></div>
                <dl>
                  <div><dt>运行状态</dt><dd>{locatedRun.status}</dd></div>
                  {locatedRun.owner.entryPoint === 'im' && <div><dt>回复投递</dt><dd>{privateImSummary && privateImSummary !== 'loading' ? privateImDeliveryLabel(privateImSummary.replyDeliveryStatus) : '状态暂不可用'}</dd></div>}
                  <div><dt>会话</dt><dd>{locatedRun.context.conversation.conversationId}</dd></div>
                  <div><dt>工作区</dt><dd>{locatedRun.context.workspaceId}</dd></div>
                </dl>
                {locatedRun.output?.message && <p>{locatedRun.output.message}</p>}
                {locatedRun.failure && <p role="alert">{locatedRun.failure.message}</p>}
                {scheduleOrigin && <button type="button" className="runtime-link" onClick={onReturnToSchedules}>返回关联定时任务</button>}
              </article>
            )}
            {locatedRun === 'error' && privateImSummary && privateImSummary !== 'loading' && (
              <article className="located-run-card">
                <div className="approval-heading"><span>运行详情</span><strong>企业微信私聊</strong></div>
                <dl>
                  <div><dt>运行状态</dt><dd>{privateImSummary.runStatus}</dd></div>
                  <div><dt>回复投递</dt><dd>{privateImDeliveryLabel(privateImSummary.replyDeliveryStatus)}</dd></div>
                </dl>
              </article>
            )}
            {searchLocationError && <p className="archive-notice" role="alert">{searchLocationError}</p>}
            {searchWindow && searchWindow.conversationId === workConversationId && <div className="work-message-window-banner" role="status">
              <span>正在查看搜索命中附近的历史消息。</span>
              <button type="button" onClick={() => setSearchWindow(undefined)}>返回最近消息</button>
            </div>}
            {(archiveOpen ? (archiveQuery.data ?? []).map((item, index): ChatMessage => ({ id: index + 1, role: item.role, text: item.text, at: item.at, run: item.run }))
              : searchWindow && searchWindow.conversationId === workConversationId ? searchWindow.result.messages.map((item, index): ChatMessage => ({ id: index + 1, entryId: item.id, role: item.role, text: item.text, at: item.at, run: item.run }))
                : emptyConversation ? [] : messages).map((message) => message.role === 'notice'
              ? <div key={message.id} className="conversation-stop-run" role="status"><ReplyRunDetails run={message.run} summary={message.text} />
                </div>
              : <article key={message.entryId ?? message.id} data-message-entry-id={message.entryId} className={`message ${message.role}`}>
                {surface === 'assistant' && message.role === 'assistant' && <span className="assistant-message-avatar" role="img" aria-label="助理标识"><img src={new URL('../assets/assistant/portrait-resting.png', import.meta.url).href} alt="" /></span>}
                <div className="message-label">
                  {message.role === 'user' ? '你' : message.role === 'error' ? '运行错误' : 'YuanpuAgent'}
                </div>
                {surface === 'assistant' && message.channel && <span className="assistant-message-channel">
                  {message.channel === 'wecom' ? '企业微信' : '桌面'}
                </span>}
                <div className="message-body">
                  {message.role === 'assistant' || message.role === 'error' ? <AssistantReply text={message.text} surface={surface} run={message.run}
                    onReview={surface === 'work'
                      ? () => { setReviewRunId(message.run?.runId); setActivityOpen(true); setActivityTab('review'); } : undefined}
                    onOpenFilePath={surface === 'work' ? openWorkspaceFile : undefined}
                    resolveWorkspaceImage={surface === 'work' ? resolveWorkspaceImage : undefined}
                    onOpenImageInSidebar={openImageInSidebar}
                    onAddToConversation={archiveOpen || workArchived ? undefined : addPreviewToConversation} /> : <p>{message.text}</p>}
                  {!message.run && !message.process?.length && message.tools?.map((tool) => (
                    <div className={`tool-event ${tool.status}`} key={`${message.id}-${tool.name}`}>
                      <span className="tool-check">{tool.status === 'completed' ? '✓' : '!'}</span>
                      <span>{toolLabel(tool.name)}</span><code>{tool.name}</code>
                      <small>{tool.status === 'completed' ? '已完成' : '失败'}</small>
                    </div>
                  ))}
                </div>
                {message.role === 'user' && <div className="user-message-meta">
                  {messageTime(message.at) && <time dateTime={message.at}>{messageTime(message.at)}</time>}
                  <button type="button" className={copiedUserMessageId === message.id ? 'copied' : ''}
                    aria-label={copiedUserMessageId === message.id ? '已复制这条消息' : '复制这条消息'} data-tooltip={copiedUserMessageId === message.id ? '已复制' : '复制'}
                    onClick={() => void copyUserMessage(message)}><AppIcon name={copiedUserMessageId === message.id ? 'check' : 'copy'} /></button>
                </div>}
              </article>
            )}
            {surface === 'work' && Boolean(workConversationsQuery.data?.find((item) => item.id === workConversationId)?.previousWorkingDirectories?.length)
              && <p className="archive-notice" role="status">工作目录已移动。历史消息中的旧绝对路径保留原文，可能已失效；请从右侧文件树打开当前文件。</p>}
            {archiveOpen && archiveQuery.isLoading && <p className="archive-notice">正在读取原桌面会话…</p>}
            {archiveOpen && archiveQuery.error && <p className="archive-notice" role="alert">原桌面会话读取失败：{formatError(archiveQuery.error)}</p>}
            {archiveOpen && archiveQuery.data?.length === 0 && <p className="archive-notice">原桌面会话还没有消息。</p>}
            {!archiveOpen && surface === 'assistant' && mirrorQuery.data?.length ? <div className="assistant-mirror-status" aria-label="旧企业微信投递记录（只读）">
              <p>旧企业微信投递记录（只读）</p>
              {mirrorQuery.data.map((mirror) => <div key={mirror.mirrorId}>
                <span>{mirror.part === 'user' ? '消息' : '回复'}：{mirror.status === 'accepted' ? '企业微信已接收' : mirror.status === 'pending' ? '等待投递' : mirror.status === 'delivering' ? '正在投递' : mirror.status === 'unknown' ? '结果未知' : '投递失败'}</span>
              </div>)}
            </div> : null}
            {!archiveOpen && !workArchived && busy && (
              <article className="message assistant pending">
                {surface === 'assistant' && <span className="assistant-message-avatar" aria-hidden="true"><img src={new URL('../assets/assistant/portrait-resting.png', import.meta.url).href} alt="" /></span>}
                <div className="message-label">YuanpuAgent</div>
                {activeReplyRun || activeProgress?.text
                  ? <div className="message-body"><AssistantReply text={activeProgress?.text ?? ''} surface={surface}
                    run={activeReplyRun} pending
                    onOpenFilePath={openWorkspaceFile} resolveWorkspaceImage={resolveWorkspaceImage}
                    onOpenImageInSidebar={openImageInSidebar} onAddToConversation={addPreviewToConversation} /></div>
                  : <div className="thinking"><span /><span /><span /> {activeRunStatus === 'waiting_approval' ? '等待授权' : '正在处理'}</div>}
              </article>
            )}
          </div>
        </div>

        <div className="composer-wrap">
          {!archiveOpen && !workArchived && readyApprovalContextRef.current === approvalContext && approvals.map((approval) => (
            <article className="approval-card composer-approval" key={approval.requestId} role="alert">
              <div className="approval-heading">
                <span>等待你的决定</span>
                <strong>{approval.assistantDelegation ? '专业任务授权' : `${capabilitySource(approval.sourceInstanceId)}操作需要授权`}</strong>
              </div>
              {approval.assistantDelegation ? <DelegationApprovalDetails delegation={approval.assistantDelegation} /> : <dl>
                <div><dt>操作</dt><dd>{capabilityName(approval.capabilityId)}</dd></div>
                <div><dt>来源</dt><dd>{capabilitySource(approval.sourceInstanceId)}</dd></div>
              </dl>}
              <p>{approval.assistantDelegation
                ? '仅允许此任务使用列出的来源和能力；具体敏感能力调用仍需单独审批。'
                : '本次决定仅对当前操作生效。'}</p>
              <div className="approval-actions">
                <button type="button" disabled={Boolean(approvalBusy) || !readyApprovals.has(approval.requestId)} onClick={() => void decideApproval(approval, 'denied')}>拒绝</button>
                <button type="button" className="primary" disabled={Boolean(approvalBusy) || !readyApprovals.has(approval.requestId)} onClick={() => void decideApproval(approval, 'approved')}>
                  {approvalBusy === approval.requestId ? '处理中…' : !readyApprovals.has(approval.requestId) ? '正在准备授权…' : '允许这次操作'}
                </button>
              </div>
            </article>
          ))}
          {bridgeError && <p role="status">暂时无法读取授权状态，正在重连…</p>}
          {runRecovery && <div className="run-recovery" role="alert">
            <span>无法获取任务状态。任务可能仍在执行，请恢复查看后再发送。</span>
            <button type="button" disabled={busy} onClick={() => void resumeRun()}>重新获取状态</button>
            <button type="button" disabled={cancelBusy || busy} onClick={() => void cancelRun(runRecovery.runId)}>取消任务</button>
          </div>}
          <div className="composer">
            <input ref={attachmentInput} className="composer-attachment-input" type="file" multiple
              accept=".txt,.md,.json,.csv,.ts,.tsx,.js,.jsx,.py,.yaml,.yml,.xml,.html,.css"
              onChange={(event) => void addAttachments(event.currentTarget.files)} />
            <textarea
              ref={composerInput}
              aria-label="消息"
              value={input}
              onChange={(event) => setInput(event.target.value)}
              onKeyDown={handleKeyDown}
              placeholder={workArchived ? workConversationId === 'default' ? '旧工作归档只读，请选择或新建工作' : '会话已归档，恢复后可继续对话' : archiveOpen ? '归档只读，请返回已绑定会话继续对话' : emo.composer.placeholder}
              disabled={archiveOpen || workArchived || (surface === 'work' && Boolean(desktop) && !workConversationId)}
              rows={2}
            />
            {attachments.length > 0 && <div className="composer-attachments" aria-label="待发送附件">
              {attachments.map((file, index) => <span key={`${file.name}-${index}`}>{file.name}
                <button type="button" aria-label={`移除附件 ${file.name}`} onClick={() => setAttachments((current) => current.filter((_, itemIndex) => itemIndex !== index))}>×</button>
              </span>)}
            </div>}
            <div className="composer-toolbar">
              <div className="composer-actions">
                <button type="button" className="composer-utility" title="添加文本附件" aria-label="添加附件" disabled={archiveOpen || workArchived || busy}
                  onClick={() => attachmentInput.current?.click()}><AppIcon name="plus" /></button>
                {surface === 'work' && <button type="button" className="composer-permission" title={fullAccessEnabled
                  ? '完全权限已开启 · 后续任务无需逐次审批' : '逐次审批 · 点击开启完全权限'}
                  aria-label={fullAccessEnabled ? '关闭完全权限，恢复逐次审批' : '开启完全权限，后续任务无需逐次审批'}
                  aria-pressed={fullAccessEnabled} disabled={!workConversationId || workArchived || archiveOpen || busy}
                  onClick={toggleFullAccess}><AppIcon name={fullAccessEnabled ? 'unlock' : 'lock'} /></button>}
                <button type="button" className="composer-redaction-preview" title={`脱敏${redactionPreviewEnabled ? '已选中' : '未选中'} · 界面预览，尚未生效`}
                  aria-label={`${redactionPreviewEnabled ? '关闭' : '启用'}脱敏（界面预览，尚未生效）`} aria-pressed={redactionPreviewEnabled}
                  onClick={() => setRedactionPreviewEnabled((value) => !value)}><AppIcon name={redactionPreviewEnabled ? 'shield-filled' : 'shield'} /></button>
              </div>
              {(archiveOpen || workArchived) && <span className="composer-hint">{workArchived ? workConversationId === 'default' ? '旧工作归档只读' : '会话已归档 · 只读' : '原桌面会话归档只读'}</span>}
              {surface === 'assistant' && !archiveOpen && <span className="composer-hint">此处发送到桌面助理</span>}
              {surface === 'work' && <ComposerRuntimeControls currentModel={currentModel}
                choices={modelChoices} selectedThinkingLevel={modelSelection?.thinkingLevel}
                context={trajectory?.context} modelOpen={modelOpen} setModelOpen={setModelOpen}
                contextOpen={contextOpen} setContextOpen={setContextOpen} onChooseModel={chooseModel}
                contextActionLabel="打开运行轨迹 →" onContextAction={() => {
                  setActivityOpen(true); setActivityTab('trajectory');
                }} />}
              {busy && activeRunId ? <button type="button" className="composer-stop" onClick={() => void cancelRun(activeRunId, false)}
                disabled={cancelBusy} aria-label="停止任务" title="停止当前任务"><AppIcon name="pause" /></button>
                : <button type="button" onClick={() => void sendMessage()} disabled={archiveOpen || workArchived || (surface === 'work' && Boolean(desktop) && !workConversationId) || (!input.trim() && attachments.length === 0) || busy || Boolean(runRecovery)} aria-label="发送消息"><AppIcon name="send" /></button>}
            </div>
          </div>
          {attachmentError && <p className="composer-attachment-error" role="alert">{attachmentError}</p>}
        </div>
      </div>
      {(surface === 'work' || surface === 'assistant') && (
        <dialog ref={activityDialog} className={`activity-panel ${surface === 'assistant' ? 'assistant-home-panel' : ''}`} aria-label={surface === 'assistant' ? '助理面板' : '当前会话侧栏'} onCancel={() => { setActivityOpen(false); setRightPanelMaximized(false); }}>
          <div className="activity-resize-handle" role="separator" aria-label="调整右侧栏宽度" aria-orientation="vertical" aria-valuemin={260}
            aria-valuemax={maxRightPanelWidth(chatPanel.current?.clientWidth ?? window.innerWidth - 50, listOpen, surface === 'work')} aria-valuenow={rightPanelWidth} tabIndex={0}
            onPointerDown={(event) => {
              if (window.matchMedia('(max-width: 560px)').matches) return;
              const panel = chatPanel.current;
              if (!panel) return;
              event.preventDefault();
              rightPanelDragCleanup.current?.();
              rightPanelGesture.current = {
                startedAtLimit: rightPanelWidth >= maxRightPanelWidth(panel.getBoundingClientRect().width, listOpen, surface === 'work') - 1,
                stopped: false,
              };
              const pointerId = event.pointerId;
              const move = (moveEvent: PointerEvent) => {
                if (moveEvent.pointerId === pointerId) resizeRightPanel(moveEvent.clientX, panel);
              };
              const finish = (finishEvent: PointerEvent) => {
                if (finishEvent.pointerId !== pointerId) return;
                rightPanelDragCleanup.current?.();
                rightPanelDragCleanup.current = null;
                setRightPanelResizing(false);
              };
              window.addEventListener('pointermove', move);
              window.addEventListener('pointerup', finish);
              window.addEventListener('pointercancel', finish);
              rightPanelDragCleanup.current = () => {
                window.removeEventListener('pointermove', move);
                window.removeEventListener('pointerup', finish);
                window.removeEventListener('pointercancel', finish);
              };
              setRightPanelResizing(true);
            }}
            onKeyDown={(event) => {
              if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return;
              event.preventDefault();
              const panel = event.currentTarget.closest<HTMLElement>('.chat-panel');
              if (!panel) return;
              const bounds = panel.getBoundingClientRect();
              rightPanelGesture.current = { startedAtLimit: rightPanelWidth >= maxRightPanelWidth(bounds.width, listOpen, surface === 'work') - 1, stopped: false };
              resizeRightPanel(bounds.right - rightPanelWidth + (event.key === 'ArrowLeft' ? -24 : 24), panel);
            }} />
          {surface === 'assistant' ? imagePreviewRequest
            ? <ImagePreviewPanel image={imagePreviewRequest} onClose={() => setImagePreviewRequest(undefined)} />
            : <AssistantHome active={active && activityOpen} link={assistantLinkQuery.data}
            onOpenWorkConversation={onOpenWorkConversation ?? (() => undefined)}
            linkLoading={assistantLinkQuery.isLoading} linkError={assistantLinkQuery.error} retryLink={() => void assistantLinkQuery.refetch()}
            activity={{
              runId: visibleRun?.runId ?? (navigationTarget?.runId ? undefined : activeRunId),
              status: currentStatus,
              task: submittedTask?.runId === (visibleRun?.runId ?? activeRunId) && !navigationTarget?.runId ? submittedTask?.text : undefined,
              entryPoint: visibleRun?.owner.entryPoint,
              disconnected: Boolean(runRecovery) || locatedRun === 'error',
            }}
            run={visibleRun} archiveOpen={archiveOpen} onToggleArchive={() => setArchiveOpen((value) => !value)} /> : <>
          {mountedWorkspaces.map((sessionId) => {
            const isCurrent = sessionId === workConversationId;
            const session = workConversationsQuery.data?.find((item) => item.id === sessionId);
            const sessionHost: ViewerFileHost | undefined = desktop ? {
              listDirectory: (dirPath, options) => desktop.listWorkFiles(sessionId, dirPath, options),
              readFile: (path) => desktop.readWorkFile(sessionId, path),
              openFile: (path) => desktop.openWorkFile(sessionId, path),
            } : undefined;
            return <div key={sessionId} className="session-sidebar-instance" style={{ display: isCurrent ? undefined : 'none' }}
              aria-hidden={!isCurrent}>
              <FileWorkspace host={sessionHost} browserHost={browserHost}
                browserRequest={isCurrent && browserRequest?.conversationId === sessionId ? browserRequest.serial : undefined}
                scopeKey={sessionId} rootName={session?.workingDirectory?.split(/[\\/]/).filter(Boolean).at(-1) || '工作区'}
                requestPath={isCurrent ? filePreviewPath : undefined} requestImage={isCurrent ? imagePreviewRequest : undefined}
                onActiveFileChange={isCurrent ? setFilePreviewPath : undefined} tabHost={isCurrent ? workspaceTabHost : null}
                view={isCurrent ? activityTab : sidebarSessions.current.get(sessionId)?.tab ?? 'files'}
                onViewChange={isCurrent ? (next) => next === 'subagents' ? openAiTickets() : setActivityTab(next) : () => undefined}
                onClose={isCurrent ? () => { setActivityOpen(false); setRightPanelMaximized(false); setActivityTab('files'); setFilePreviewPath(undefined); setImagePreviewRequest(undefined); } : () => undefined}
                reviewContent={isCurrent ? <ReviewPanel conversationId={sessionId}
                  lastRunId={reviewRunId ?? lastRun?.runId ?? [...messages].reverse().find((message) => message.run?.runId)?.run?.runId}
                  refreshKey={reviewRefreshKey} active={active && activityOpen && activityTab === 'review'} /> : null}
                runContent={isCurrent ? (activityTab === 'trajectory'
                  ? <SessionTrajectoryViewer trajectory={trajectory} title={selectedWork?.title ?? '工作会话'}
                      onRefresh={() => { if (desktop) void desktop.getWorkTrajectory(sessionId).then(setTrajectory); }} />
                  : desktop ? <AiTicketsPanel conversationId={sessionId} desktop={desktop} /> : null) : null} />
            </div>;
          })}</>}
        </dialog>
      )}
    </section>
  );
}

import { randomUUID } from 'node:crypto';
import { constants } from 'node:fs';
import { mkdir, open, readdir, rename, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { createYuanpuChatSession, readYuanpuChatTranscript, readYuanpuSessionTrajectory,
  type CapabilityToolClient, type CreateYuanpuChatOptions, type YuanpuChatSession } from '@yuanpu-agent/runtime-kit';
import type { AgentRunRequest, AiTicket, DesktopTranscriptMessage, RunToolStep } from '@yuanpu-agent/protocol';

export interface AiTicketCompletion {
  ticketId: string;
  parentConversationId: string;
  cwd: string;
  title: string;
  turnId: string;
  status: 'completed' | 'failed' | 'interrupted';
  result: string;
}

interface PendingFeedback {
  turnId: string;
  status?: AiTicketCompletion['status'];
  result: string;
}

interface TicketRecord extends AiTicket {
  cwd: string;
  sessionId: string;
  pendingFeedback?: PendingFeedback[];
}

interface ActiveTicket {
  controller: AbortController;
  done: Promise<void>;
  runId: string;
  ticket: TicketRecord;
  approvalMode: AgentRunRequest['approvalMode'];
  startedAt: string;
  updatedAt: string;
  text: string;
  steps: Map<string, RunToolStep>;
}

const ticketIdPattern = /^[0-9a-f-]{36}$/i;
const parentIdPattern = /^work:[0-9a-f-]{36}$/i;

/** A ticket owns a Pi session. Its lifetime and follow-up turns are independent of the parent prompt. */
export class AiTicketService {
  private readonly active = new Map<string, ActiveTicket>();
  private readonly activeRuns = new Map<string, ActiveTicket>();
  private readonly cache = new Map<string, TicketRecord>();
  private readonly selections = new Map<string, { ticketId?: string; revision: number }>();
  private readonly deliveringFeedback = new Set<string>();

  constructor(private readonly options: {
    root: string;
    sessionsPath: string;
    chat: () => Omit<CreateYuanpuChatOptions, 'capabilityClient' | 'capabilityContext' | 'piSession'>;
    capabilityClient: () => CapabilityToolClient;
    resolveParent: (conversationId: string) => { cwd: string; piSessionId: string } | undefined;
    userId: string;
    createSession?: (options: CreateYuanpuChatOptions) => Promise<YuanpuChatSession>;
    onCompleted?: (completion: AiTicketCompletion) => Promise<void>;
  }) {}

  private file(id: string): string {
    if (!ticketIdPattern.test(id)) throw new Error('无效的任务工单 ID。');
    return join(this.options.root, `${id}.json`);
  }

  private async load(id: string): Promise<TicketRecord> {
    const cached = this.cache.get(id);
    if (cached) return cached;
    const file = await open(this.file(id), constants.O_RDONLY | (constants.O_NOFOLLOW ?? 0));
    try {
      const info = await file.stat();
      if (!info.isFile() || info.size > 128_000) throw new Error('任务工单数据无效。');
      const ticket = JSON.parse(await file.readFile({ encoding: 'utf8' })) as TicketRecord;
      if (ticket.id !== id || !parentIdPattern.test(ticket.parentConversationId)
        || !ticketIdPattern.test(ticket.sessionId) || typeof ticket.task !== 'string'
        || typeof ticket.cwd !== 'string') throw new Error('任务工单数据无效。');
      if (ticket.status === 'running' || ticket.status === 'queued') {
        ticket.status = 'interrupted';
        ticket.updatedAt = new Date().toISOString();
        if (this.options.onCompleted) {
          ticket.pendingFeedback = [...(ticket.pendingFeedback ?? []), {
            turnId: `${ticket.id}:interrupted:${ticket.updatedAt}`,
            status: 'interrupted',
            result: '运行环境退出，工单执行中断；请检查已有交付物和工单记录后决定是否继续。',
          }];
        }
        await this.save(ticket);
      }
      this.cache.set(id, ticket);
      return ticket;
    } finally { await file.close(); }
  }

  private async save(ticket: TicketRecord): Promise<void> {
    await mkdir(this.options.root, { recursive: true, mode: 0o700 });
    const temporary = join(this.options.root, `${ticket.id}.${randomUUID()}.tmp`);
    try {
      await writeFile(temporary, JSON.stringify(ticket), { mode: 0o600 });
      await rename(temporary, this.file(ticket.id));
    } finally {
      // A failed rename leaves only a disposable temporary file.
      await rm(temporary, { force: true }).catch(() => undefined);
    }
  }

  private async owned(parentConversationId: string, id: string): Promise<TicketRecord> {
    const parent = this.options.resolveParent(parentConversationId);
    if (!parent) throw new Error('Unknown parent conversation.');
    const ticket = await this.load(id);
    if (ticket.parentConversationId !== parentConversationId || ticket.cwd !== parent.cwd) {
      throw new Error('任务工单不属于当前会话。');
    }
    return ticket;
  }

  private view(ticket: TicketRecord, includeMessages = false): AiTicket {
    const { cwd: _cwd, sessionId: _sessionId, pendingFeedback: _pendingFeedback,
      liveRun: _liveRun, ...summary } = ticket;
    if (!includeMessages) return summary;
    let messages: DesktopTranscriptMessage[] = [];
    const active = ticket.status === 'running' ? this.active.get(ticket.id) : undefined;
    try {
      messages = readYuanpuChatTranscript(ticket.cwd, ticket.sessionId, this.options.sessionsPath,
        100, false, undefined, true);
      if (active) {
        const last = messages.at(-1);
        if (last?.role === 'assistant' && last.run?.createdAt
          && Date.parse(last.run.createdAt) >= Date.parse(active.startedAt)) messages.pop();
      }
      const firstUser = messages.find((message) => message.role === 'user');
      if (firstUser) firstUser.text = ticket.task;
    } catch { /* A new ticket may not have a session file yet. */ }
    let activity: AiTicket['activity'] = [];
    let context = ticket.context;
    try {
      const trajectory = readYuanpuSessionTrajectory(ticket.cwd, ticket.sessionId, this.options.sessionsPath,
        ticket.id, this.options.chat().agentDir);
      activity = trajectory.rows.slice(-100);
      context = trajectory.context ?? context;
    } catch { /* Activity appears after the Pi session file is created. */ }
    const steps = active ? [...active.steps.values()] : [];
    return { ...summary, messages, activity, context,
      ...(active ? { progress: active.text,
        liveRun: { runId: active.runId, source: 'runtime' as const, status: 'running' as const,
          createdAt: active.startedAt, updatedAt: active.updatedAt, events: [],
          steps, tools: steps.filter((step) => step.status === 'completed' || step.status === 'failed')
            .map((step) => ({ name: step.name, status: step.status as 'completed' | 'failed' })) } } : {}) };
  }

  private async deliverFeedback(ticket: TicketRecord): Promise<void> {
    if (!this.options.onCompleted || !ticket.pendingFeedback?.length || this.deliveringFeedback.has(ticket.id)) return;
    this.deliveringFeedback.add(ticket.id);
    try {
      while (ticket.pendingFeedback?.length) {
        const pending = ticket.pendingFeedback[0];
        if (!pending) break;
        await this.options.onCompleted({ ticketId: ticket.id, parentConversationId: ticket.parentConversationId,
          cwd: ticket.cwd, title: ticket.title || ticket.task.split('\n')[0]?.slice(0, 80) || '任务工单',
          turnId: pending.turnId, status: pending.status ?? 'completed', result: pending.result });
        ticket.pendingFeedback.shift();
        await this.save(ticket);
      }
    } catch (error) {
      console.warn('[ai-ticket-feedback]', error);
    } finally {
      this.deliveringFeedback.delete(ticket.id);
    }
  }

  async reconcilePendingFeedback(): Promise<void> {
    if (!this.options.onCompleted) return;
    await mkdir(this.options.root, { recursive: true, mode: 0o700 });
    const names = (await readdir(this.options.root)).filter((name) => name.endsWith('.json')
      && ticketIdPattern.test(name.slice(0, -5))).slice(0, 500);
    for (const name of names) {
      try {
        const ticket = await this.load(name.slice(0, -5));
        if (!this.active.has(ticket.id) && ticket.pendingFeedback?.length) await this.deliverFeedback(ticket);
      } catch (error) { console.warn('[ai-ticket-feedback]', error); }
    }
  }

  async list(parentConversationId: string): Promise<AiTicket[]> {
    if (!this.options.resolveParent(parentConversationId)) throw new Error('Unknown parent conversation.');
    await mkdir(this.options.root, { recursive: true, mode: 0o700 });
    const names = (await readdir(this.options.root)).filter((name) => ticketIdPattern.test(name.slice(0, -5))
      && name.endsWith('.json')).slice(0, 500);
    const tickets = await Promise.all(names.map(async (name) => {
      try { return await this.load(name.slice(0, -5)); } catch { return undefined; }
    }));
    return tickets.filter((ticket): ticket is TicketRecord => ticket?.parentConversationId === parentConversationId)
      .sort((a, b) => b.createdAt.localeCompare(a.createdAt)).map((ticket) => this.view(ticket));
  }

  async get(parentConversationId: string, id: string): Promise<AiTicket> {
    return this.view(await this.owned(parentConversationId, id), true);
  }

  navigation(parentConversationId: string): { ticketId?: string; revision: number } {
    if (!this.options.resolveParent(parentConversationId)) throw new Error('Unknown parent conversation.');
    return this.selections.get(parentConversationId) ?? { revision: 0 };
  }

  async navigate(parentConversationId: string, id?: string): Promise<void> {
    if (id) await this.owned(parentConversationId, id);
    else if (!this.options.resolveParent(parentConversationId)) throw new Error('Unknown parent conversation.');
    const revision = (this.selections.get(parentConversationId)?.revision ?? 0) + 1;
    this.selections.set(parentConversationId, { ...(id ? { ticketId: id } : {}), revision });
  }

  async create(parentConversationId: string, task: string, inheritedContext?: string,
    details: { title?: string; deliverables?: string[]; acceptanceCriteria?: string[] } = {}): Promise<AiTicket> {
    const parent = this.options.resolveParent(parentConversationId);
    if (!parent) throw new Error('Unknown parent conversation.');
    const trimmed = task.trim();
    if (!trimmed || trimmed.length > 20_000) throw new Error('任务工单内容须为 1–20000 个字符。');
    const title = details.title?.trim();
    if (title && title.length > 120) throw new Error('任务工单标题过长。');
    const checkedList = (items: string[] | undefined, label: string) => {
      if (items === undefined) return undefined;
      if (!Array.isArray(items) || items.length > 20 || items.some((item) =>
        typeof item !== 'string' || !item.trim() || item.length > 500)) throw new Error(`Invalid ${label}.`);
      return items.map((item) => item.trim());
    };
    const deliverables = checkedList(details.deliverables, 'deliverables');
    const acceptanceCriteria = checkedList(details.acceptanceCriteria, 'acceptance criteria');
    const savedContext = readYuanpuChatTranscript(parent.cwd, parent.piSessionId,
      this.options.sessionsPath, 12, true).map((message) =>
      `${message.role === 'user' ? '用户' : '主 Agent'}：${message.text.slice(0, 2_000)}`).join('\n\n');
    const context = (inheritedContext || savedContext).slice(-12_000);
    const now = new Date().toISOString();
    const chat = this.options.chat();
    const ticket: TicketRecord = { id: randomUUID(), parentConversationId, cwd: parent.cwd,
      sessionId: randomUUID(), task: trimmed, ...(title ? { title } : {}),
      ...(deliverables ? { deliverables } : {}), ...(acceptanceCriteria ? { acceptanceCriteria } : {}),
      model: `${chat.provider}/${chat.model}`, status: 'queued', createdAt: now, updatedAt: now };
    await this.save(ticket);
    this.cache.set(ticket.id, ticket);
    this.start(ticket, trimmed, context);
    await this.navigate(parentConversationId, ticket.id);
    return this.view(ticket);
  }

  async message(parentConversationId: string, id: string, message: string,
    options: { modelSelection?: AgentRunRequest['modelSelection'];
      approvalMode?: AgentRunRequest['approvalMode'] } = {}): Promise<AiTicket> {
    const ticket = await this.owned(parentConversationId, id);
    const trimmed = message.trim();
    if (!trimmed || trimmed.length > 350_000) throw new Error('Task ticket message must be 1–350000 characters.');
    if (options.approvalMode !== undefined && options.approvalMode !== 'required'
      && options.approvalMode !== 'unrestricted') throw new Error('Invalid task ticket approval mode.');
    const selection = options.modelSelection;
    if (selection !== undefined && (typeof selection.provider !== 'string' || !selection.provider.trim()
      || selection.provider.length > 256 || typeof selection.model !== 'string' || !selection.model.trim()
      || selection.model.length > 256 || (selection.thinkingLevel !== undefined
        && !['minimal', 'low', 'medium', 'high', 'xhigh', 'max'].includes(selection.thinkingLevel)))) {
      throw new Error('Invalid task ticket model selection.');
    }
    const active = this.active.get(id);
    if (active) {
      if (ticket.status === 'running' || ticket.status === 'queued') throw new Error('任务工单正在执行。');
      await active.done;
    }
    if ((ticket.pendingFeedback?.length ?? 0) >= 8) {
      throw new Error('工单结果仍待回报主会话，请稍后重试。');
    }
    if (selection) {
      ticket.modelSelection = { provider: selection.provider, model: selection.model,
        ...(selection.thinkingLevel ? { thinkingLevel: selection.thinkingLevel } : {}) };
      ticket.model = `${selection.provider}/${selection.model}`;
    }
    this.start(ticket, trimmed, '', options.approvalMode);
    return this.view(ticket, true);
  }

  isUnrestrictedCapabilityRun(runId: string, sessionId: string, workspaceId: string): boolean {
    const active = this.activeRuns.get(runId);
    return Boolean(active && active.approvalMode === 'unrestricted'
      && active.ticket.sessionId === sessionId && active.ticket.cwd === workspaceId
      && active.ticket.status === 'running');
  }

  async cancel(parentConversationId: string, id: string): Promise<void> {
    await this.owned(parentConversationId, id);
    const active = this.active.get(id);
    if (!active) throw new Error('任务工单未在执行。');
    active.controller.abort(new Error('Cancelled by user.'));
    await active.done;
  }

  private start(ticket: TicketRecord, message: string, inheritedContext = '',
    approvalMode: AgentRunRequest['approvalMode'] = 'required'): void {
    const controller = new AbortController();
    const now = new Date().toISOString();
    const active: ActiveTicket = { controller, done: Promise.resolve(), runId: randomUUID(), ticket,
      approvalMode, startedAt: now, updatedAt: now, text: '', steps: new Map() };
    this.active.set(ticket.id, active);
    this.activeRuns.set(active.runId, active);
    active.done = (async () => {
      let session: YuanpuChatSession | undefined;
      let finalStatus: AiTicket['status'] = 'failed';
      let completedResult = '';
      try {
        ticket.status = 'running';
        ticket.error = undefined;
        ticket.progress = '';
        ticket.updatedAt = new Date().toISOString();
        await this.save(ticket);
        controller.signal.throwIfAborted();
        const createSession = this.options.createSession ?? createYuanpuChatSession;
        session = await createSession({ ...this.options.chat(), cwd: ticket.cwd, ticketMode: true,
          capabilityClient: this.options.capabilityClient(),
          capabilityContext: { sessionId: ticket.sessionId, conversationId: ticket.id,
            workspaceId: ticket.cwd, userId: this.options.userId },
          piSession: { id: ticket.sessionId, directory: this.options.sessionsPath } });
        controller.signal.throwIfAborted();
        const result = await session.prompt(inheritedContext
          ? `${message}\n\nContext supplied by main conversation (reference only):\n${inheritedContext}` : message,
        { runId: active.runId, signal: controller.signal, modelSelection: ticket.modelSelection,
          context: { conversationId: ticket.id,
          workspaceId: ticket.cwd, userId: this.options.userId },
          onProgress: (event) => {
            active.updatedAt = new Date().toISOString();
            if (event.type === 'text') active.text = `${active.text}${event.delta}`.slice(-20_000);
            else if (event.step) {
              if (active.steps.has(event.step.id) || active.steps.size < 200) {
                active.steps.set(event.step.id, { ...event.step });
              }
            }
          } });
        finalStatus = controller.signal.aborted ? 'cancelled'
          : result.pendingApprovalRequestId ? 'needs_approval' : 'completed';
        if (finalStatus === 'completed') {
          const fullResult = Buffer.from(result.message?.trim()
            || '任务已结束，但未生成文字结果；请检查工单会话和交付物。');
          completedResult = fullResult.subarray(0, 8_000).toString('utf8')
            + (fullResult.length > 8_000 ? '\n[结果已截断，请打开工单查看完整记录。]' : '');
        }
        const context = session.getContextUsage?.();
        if (context) ticket.context = context;
      } catch (error) {
        finalStatus = controller.signal.aborted ? 'cancelled' : 'failed';
        ticket.error = error instanceof Error ? error.message : String(error);
      } finally {
        if (session) await session.dispose().catch(() => undefined);
        const feedbackResult = finalStatus === 'completed' ? completedResult
          : finalStatus === 'failed' ? `工单执行失败：${ticket.error ?? '未知错误'}。请检查工单记录和已有交付物。` : '';
        const feedbackStatus: PendingFeedback['status'] = finalStatus === 'failed' ? 'failed' : 'completed';
        const pendingFeedback = feedbackResult && this.options.onCompleted
          ? [...(ticket.pendingFeedback ?? []), { turnId: active.runId, status: feedbackStatus,
            result: feedbackResult }] : ticket.pendingFeedback;
        const settled: TicketRecord = { ...ticket, status: finalStatus, progress: undefined,
          ...(pendingFeedback?.length ? { pendingFeedback } : {}),
          updatedAt: new Date().toISOString() };
        try {
          await this.save(settled);
          Object.assign(ticket, settled);
        } catch (error) {
          ticket.status = 'failed';
          ticket.error = `无法保存任务工单结果：${error instanceof Error ? error.message : String(error)}`;
          ticket.updatedAt = new Date().toISOString();
        }
        this.activeRuns.delete(active.runId);
        if (finalStatus === 'completed' || finalStatus === 'failed') await this.deliverFeedback(ticket);
        this.active.delete(ticket.id);
      }
    })();
  }

  async dispose(): Promise<void> {
    for (const active of this.active.values()) active.controller.abort(new Error('Runtime stopped.'));
    await Promise.all([...this.active.values()].map((active) => active.done));
  }
}

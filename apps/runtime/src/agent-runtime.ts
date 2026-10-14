import {
  createYuanpuChatSession,
  type AgentRunExecutionInput,
  type AgentRunExecutionResult,
  type AgentRunExecutor,
  type CapabilityApprovalRecord,
  type CapabilityToolClient,
  type CapturedWorkFileChange,
  type CreateYuanpuChatOptions,
  type YuanpuChatSession,
} from '@yuanpu-agent/runtime-kit';
import { registerWorkWriteArtifact } from './work-artifact.js';
import { settleRunProgress, type RunProgress, type SessionTrajectory } from '@yuanpu-agent/protocol';

interface RuntimeAgentExecutorOptions {
  getCapabilityClient(): CapabilityToolClient;
  approvals: { get(requestId: string): CapabilityApprovalRecord | undefined };
  sessionsPath: string;
  maximumPooledSessions?: number;
  chat: Omit<CreateYuanpuChatOptions, 'capabilityClient' | 'capabilityContext' | 'piSession'>;
  /** 桌面工作会话的 edit/write 文件快照出口；持久化失败由实现方兜底，不阻塞会话。 */
  onWorkFileChanges?: (context: { runId: string; conversationId: string; piSessionId: string },
    changes: readonly CapturedWorkFileChange[]) => void;
}

/** The desktop canvas is one fixed desktop conversation; only its session receives canvas widget tools. */
export const DESKTOP_CANVAS_CONVERSATION_ID = 'canvas';

function isDesktopCanvasRun(run: AgentRunExecutionInput['run']): boolean {
  return run.owner.entryPoint === 'desktop' && run.context.conversation.namespace === 'desktop'
    && run.context.conversation.conversationId === DESKTOP_CANVAS_CONVERSATION_ID;
}

interface PooledSession {
  promise: Promise<YuanpuChatSession>;
  active: number;
  retired: boolean;
}

export class RuntimeAgentExecutor implements AgentRunExecutor {
  readonly #options: RuntimeAgentExecutorOptions;
  readonly #sessions = new Map<string, PooledSession>();
  readonly #retired = new Set<PooledSession>();
  readonly #disposals = new Set<Promise<void>>();
  readonly #maximumPooledSessions: number;
  readonly #live = new Map<string, NonNullable<SessionTrajectory['live']>>();

  constructor(options: RuntimeAgentExecutorOptions) {
    this.#options = options;
    this.#maximumPooledSessions = options.maximumPooledSessions ?? 16;
    if (!Number.isSafeInteger(this.#maximumPooledSessions) || this.#maximumPooledSessions < 1) {
      throw new Error('maximumPooledSessions must be a positive integer.');
    }
  }

  updateChat(chat: RuntimeAgentExecutorOptions['chat']): void {
    this.#options.chat = chat;
    this.reset();
  }

  async observation(sessionId: string): Promise<Pick<SessionTrajectory, 'live' | 'context'>> {
    const pooled = this.#sessions.get(sessionId);
    const context = pooled ? await pooled.promise.then((value) => value.getContextUsage()).catch(() => undefined) : undefined;
    let live = this.#live.get(sessionId);
    if (live?.finishedAt && Date.now() - Date.parse(live.finishedAt) > 30_000) {
      this.#live.delete(sessionId);
      live = undefined;
    }
    return {
      ...(context ? { context } : {}),
      ...(live ? { live: { ...live, tools: live.tools.map((tool) => ({ ...tool })) } } : {}),
    };
  }

  /** Live text and tool steps of one run while it executes (kept briefly after it finishes). */
  progress(runId: string): RunProgress | undefined {
    for (const live of this.#live.values()) {
      if (live.runId !== runId) continue;
      return { text: live.text.slice(-20_000), steps: (live.steps ?? []).map((step) => ({ ...step })) };
    }
    return undefined;
  }

  async subagentRuns(sessionId: string) {
    const pooled = this.#sessions.get(sessionId);
    return pooled ? await pooled.promise.then((value) => value.listSubagentRuns()).catch(() => []) : [];
  }

  async execute(input: AgentRunExecutionInput): Promise<AgentRunExecutionResult> {
    if (!input.run.context.conversation.sessionBindingId) throw new Error('Agent run does not have a persisted Pi session binding.');
    const sessionKey = input.piSessionId;
    let pooled = this.#sessions.get(sessionKey);
    if (!pooled) {
      pooled = {
        active: 0,
        retired: false,
        promise: createYuanpuChatSession({
          ...this.#options.chat,
          cwd: input.run.context.workspaceId,
          browserControlAvailable: input.run.context.conversation.conversationId.startsWith('work:'),
          desktopCanvas: isDesktopCanvasRun(input.run),
          builtinAgentRoot: input.run.context.conversation.conversationId.startsWith('work:')
            ? this.#options.chat.builtinAgentRoot : undefined,
          includeGlobalMemory: input.run.owner.entryPoint === 'desktop'
            && input.run.context.conversation.conversationId === 'assistant',
          capabilityClient: this.#options.getCapabilityClient(),
          capabilityContext: {
            sessionId: input.piSessionId,
            conversationId: input.run.context.conversation.conversationId,
            workspaceId: input.run.context.workspaceId,
            userId: input.run.owner.identity.subjectId,
          },
          piSession: { id: input.piSessionId, directory: this.#options.sessionsPath },
        }),
      };
      this.#sessions.set(sessionKey, pooled);
    } else {
      this.#sessions.delete(sessionKey);
      this.#sessions.set(sessionKey, pooled);
    }
    pooled.active += 1;
    const live: NonNullable<SessionTrajectory['live']> = { runId: input.run.runId, text: '', tools: [], steps: [] };
    this.#live.set(sessionKey, live);
    try {
      const result = await (await pooled.promise).prompt(input.input, {
        runId: input.run.runId,
        signal: input.signal,
        modelSelection: input.run.context.modelSelection,
        onProgress: (event) => {
          if (event.type === 'text') {
            live.text = (live.text + event.delta).slice(-200_000);
            return;
          }
          if (event.step) {
            const index = live.steps!.findIndex((step) => step.id === event.step!.id);
            if (index >= 0) live.steps![index] = { ...event.step };
            else if (live.steps!.length < 200) live.steps!.push({ ...event.step });
          }
          const tool = live.tools.find((item) => item.id === event.id);
          if (tool) tool.status = event.status;
          else live.tools.push({ id: event.id, name: event.name, summary: event.summary ?? event.name,
            category: event.category ?? '工具调用',
            status: event.status, at: new Date().toISOString() });
        },
        context: {
          conversationId: input.run.context.conversation.conversationId,
          workspaceId: input.run.context.workspaceId,
          userId: input.run.owner.identity.subjectId,
        },
      });
      const artifacts = [];
      let artifactBytes = 0;
      if (input.run.owner.entryPoint === 'desktop'
        && input.run.context.conversation.conversationId.startsWith('work:')) {
        for (const candidate of result.artifactCandidates ?? []) {
          const toolResult = result.toolResults?.find((item) =>
            item.toolCallId === candidate.toolCallId && item.status === 'completed');
          if (!toolResult) continue;
          const artifact = registerWorkWriteArtifact(input.run.context.workspaceId,
            candidate.requestedPath, toolResult.entryId, candidate.content);
          if (!artifact || artifactBytes + artifact.size > 256 * 1024 || artifacts.length >= 16) continue;
          artifactBytes += artifact.size;
          artifacts.push({ ...artifact, toolCallId: candidate.toolCallId });
        }
      }
      if (this.#options.onWorkFileChanges && result.fileChanges?.length
        && input.run.owner.entryPoint === 'desktop'
        && input.run.context.conversation.conversationId.startsWith('work:')) {
        this.#options.onWorkFileChanges({
          runId: input.run.runId,
          conversationId: input.run.context.conversation.conversationId,
          piSessionId: input.piSessionId,
        }, result.fileChanges);
      }
      const output = { message: result.message, tools: result.tools, steps: result.steps,
        ...(result.toolResults ? { toolResults: result.toolResults } : {}),
        ...(result.desktopActions && isDesktopCanvasRun(input.run) ? { desktopActions: result.desktopActions } : {}),
        ...(artifacts.length ? { artifacts } : {}) };
      if (result.pendingApprovalRequestId) {
        const approval = this.#options.approvals.get(result.pendingApprovalRequestId);
        if (
          !approval
          || approval.runId !== input.run.runId
          || approval.sessionId !== input.piSessionId
          || approval.workspaceId !== input.run.context.workspaceId
        ) {
          throw new Error('Pending capability approval is not bound to the active Agent run.');
        }
        return {
          kind: 'waiting_approval',
          approval: {
            runId: input.run.runId,
            approvalRequestId: approval.requestId,
            sessionId: approval.sessionId,
            workspaceId: approval.workspaceId,
            expiresAt: approval.expiresAt,
          },
          output,
        };
      }
      return { kind: 'completed', output };
    } finally {
      live.steps = settleRunProgress({ text: live.text, steps: live.steps ?? [] },
        input.signal?.aborted ? 'cancelled' : 'interrupted').steps;
      live.finishedAt = new Date().toISOString();
      pooled.active -= 1;
      this.#disposeIfRetired(pooled);
      this.#retireOverflow();
    }
  }

  async drainSessions(sessionIds: string[]): Promise<void> {
    const selected = sessionIds.flatMap((id) => {
      const pooled = this.#sessions.get(id);
      return pooled ? [{ id, pooled }] : [];
    });
    for (const { pooled } of selected) {
      if (pooled.active) throw new Error('Work session is executing.');
      await (await pooled.promise).assertMovable();
    }
    for (const { id, pooled } of selected) {
      this.#sessions.delete(id);
      this.#live.delete(id);
      await (await pooled.promise).dispose();
    }
    await Promise.all([...this.#disposals]);
  }

  reset(): void {
    for (const pooled of this.#sessions.values()) {
      pooled.retired = true;
      this.#retired.add(pooled);
      this.#disposeIfRetired(pooled);
    }
    this.#sessions.clear();
    this.#live.clear();
  }

  async close(): Promise<void> {
    this.reset();
    await Promise.allSettled([...this.#disposals]);
  }

  #disposeIfRetired(pooled: PooledSession): void {
    if (!pooled.retired || pooled.active > 0 || !this.#retired.delete(pooled)) return;
    const disposal = pooled.promise.then((session) => session.dispose(), () => undefined);
    this.#disposals.add(disposal);
    void disposal.finally(() => this.#disposals.delete(disposal));
  }

  #retireOverflow(): void {
    while (this.#sessions.size > this.#maximumPooledSessions) {
      const idle = [...this.#sessions].find(([, pooled]) => pooled.active === 0);
      if (!idle) return;
      const [bindingId, pooled] = idle;
      this.#sessions.delete(bindingId);
      this.#live.delete(bindingId);
      pooled.retired = true;
      this.#retired.add(pooled);
      this.#disposeIfRetired(pooled);
    }
  }
}

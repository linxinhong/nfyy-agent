import { createHash, randomUUID } from 'node:crypto';
import { constants } from 'node:fs';
import { open } from 'node:fs/promises';

import type { AssistantHostBinding, AssistantHostRequest, AssistantHostStore } from '@yuanpu-agent/runtime-kit';
import {
  ASSISTANT_CONTRACT_VERSION,
  validateAssistantIngress,
  type AgentRunCancellationReceipt,
  type AgentRunReceipt,
  type AgentRunRecord,
  type AssistantRequest,
  type AssistantSourceChange,
  type DesktopTranscriptMessage,
} from '@yuanpu-agent/protocol';
import type { ChannelTransport, NormalizedChannelMessage } from '@yuanpu-agent/runtime-kit';
import type { ChannelInboundReceipt } from '@yuanpu-agent/runtime-kit';
import type { AssistantTaskRecord, AssistantWorkerManager } from './assistant-worker-manager.js';

function mappedStatus(status: AssistantHostRequest['status']): AgentRunRecord['status'] {
  return status === 'accepted' ? 'queued' : status === 'completed' ? 'succeeded' : status;
}

export class AssistantHostService {
  private readonly store: AssistantHostStore;
  private readonly worker: AssistantWorkerManager;
  private readonly workspaceId: string;
  private readonly inFlight = new Map<string, Promise<void>>();
  private readonly transports = new Map<string, ChannelTransport>();
  private readonly deliveries = new Set<Promise<void>>();
  private linkQueue: Promise<void> = Promise.resolve();
  private linkChanges = 0;
  private readonly recoveryTimer: NodeJS.Timeout;
  private readonly proactiveConfigPath?: string;
  private closed = false;

  constructor(store: AssistantHostStore, worker: AssistantWorkerManager, workspaceId: string,
    proactiveConfigPath?: string) {
    this.store = store;
    this.worker = worker;
    this.workspaceId = workspaceId;
    this.proactiveConfigPath = proactiveConfigPath;
    this.store.desktop();
    this.store.markUncertainDeliveries();
    this.store.markUncertainProactiveDeliveries();
    // A revoked or corrupt legacy link must not prevent Work startup or reopen shared sessions.
    try { this.store.migrateLegacyLink(); } catch { /* original records stay untouched for retry */ }
    this.recoveryTimer = setInterval(() => this.recover(), 10_000);
    this.recoveryTimer.unref();
  }

  close(): void { this.closed = true; clearInterval(this.recoveryTimer); }

  async drain(): Promise<void> {
    await Promise.allSettled([...this.inFlight.values(), ...this.deliveries]);
  }

  get link(): AssistantHostBinding | undefined { return this.store.wecomLink(); }

  private async proactiveEnabled(): Promise<boolean> {
    if (!this.proactiveConfigPath) return false;
    try {
      const file = await open(this.proactiveConfigPath,
        constants.O_RDONLY | (constants.O_NOFOLLOW ?? 0));
      try {
        const config: unknown = JSON.parse(await file.readFile('utf8'));
        return Boolean(config && typeof config === 'object'
          && (config as { proactiveWecomEnabled?: unknown }).proactiveWecomEnabled === true);
      } finally { await file.close(); }
    } catch { return false; }
  }

  /** A private, opted-in send with a host-owned receipt. Uncertain sends are never replayed. */
  async deliverSuggestion(suggestionId: string, content: string): Promise<{
    status: 'accepted' | 'failed' | 'unknown' | 'deferred'; ref?: string;
  }> {
    if (this.closed || !(await this.proactiveEnabled()) || this.linkChanges > 0) {
      return { status: 'deferred' };
    }
    const existing = this.store.proactiveDelivery(suggestionId);
    if (existing) return existing.status === 'delivering' ? { status: 'deferred' }
      : { status: existing.status, ref: `assistant-proactive:${suggestionId}` };
    const binding = this.store.wecomLink();
    if (!binding) return { status: 'deferred' };
    const transport = this.transports.get(binding.accountId);
    if (!transport?.sendProactive || !transport.isReady?.()) return { status: 'deferred' };
    const claimed = this.store.beginProactiveDelivery(suggestionId, content, binding);
    if (!claimed.started) return claimed.record.status === 'delivering' ? { status: 'deferred' }
      : { status: claimed.record.status, ref: `assistant-proactive:${suggestionId}` };
    const ref = `assistant-proactive:${suggestionId}`;
    if (this.closed || !(await this.proactiveEnabled()) || this.linkChanges > 0
      || this.store.wecomLink()?.generation !== binding.generation) {
      this.store.finishProactiveDelivery(suggestionId, 'failed', 'binding_or_opt_in_changed');
      return { status: 'failed', ref };
    }
    const sending = transport.sendProactive(binding.externalUserId, content)
      .catch(() => ({ status: 'unknown' as const, code: 'transport_uncertain' }))
      .then((result) => {
        const status = result.status === 'accepted' ? 'accepted'
          : result.status === 'failed' ? 'failed' : 'unknown';
        this.store.finishProactiveDelivery(suggestionId, status,
          'code' in result ? result.code : undefined);
      });
    this.deliveries.add(sending);
    try { await sending; } finally { this.deliveries.delete(sending); }
    return { status: this.store.proactiveDelivery(suggestionId)!.status as
      'accepted' | 'failed' | 'unknown', ref };
  }
  ownsWecomMessage(message: NormalizedChannelMessage): boolean {
    return message.conversationType === 'single'
      && this.store.ownsWecomMessage(message.connectionId, message.senderId, message.conversationId);
  }
  private changeLink<T>(change: () => T): Promise<T> {
    this.linkChanges++;
    const result = this.linkQueue.then(async () => {
      await Promise.allSettled([...this.deliveries]);
      return change();
    });
    this.linkQueue = result.then(() => undefined, () => undefined).finally(() => {
      this.linkChanges--;
    });
    return result;
  }

  linkContact(contactId: string): Promise<AssistantHostBinding> {
    return this.changeLink(() => this.store.linkWecomContact(contactId));
  }

  unlinkContact(): Promise<void> { return this.changeLink(() => this.store.unlinkWecom()); }

  private accept(binding: AssistantHostBinding, input: {
    externalMessageId: string; text: string; providerRequestId?: string;
  }): { record: AssistantHostRequest; duplicate: boolean } {
    const replyTarget = {
      routeId: `${binding.channel}:${binding.conversationId}`,
      channel: binding.channel, accountId: binding.accountId,
      externalConversationId: binding.externalConversationId,
    } as const;
    const request: AssistantRequest = {
      contractVersion: ASSISTANT_CONTRACT_VERSION,
      requestId: randomUUID(), assistantId: binding.assistantId, principalId: binding.principalId,
      conversationId: binding.conversationId, sessionId: binding.sessionId,
      audience: { kind: 'personal', id: binding.principalId },
      inbound: {
        channel: binding.channel, accountId: binding.accountId,
        externalUserId: binding.externalUserId,
        externalConversationId: binding.externalConversationId,
        externalMessageId: input.externalMessageId, receivedAt: new Date().toISOString(), text: input.text,
      },
      replyTarget, acceptedAt: new Date().toISOString(),
    };
    const validated = validateAssistantIngress(request, {
      principalId: binding.principalId, assistantId: binding.assistantId,
      pairingId: binding.contactId ?? 'local-desktop', channel: binding.channel,
      accountId: binding.accountId, externalUserId: binding.externalUserId,
    }, { ...binding, audience: request.audience, replyTarget });
    if (!validated.ok) throw new Error(`Assistant ingress rejected: ${validated.code}`);
    return this.store.accept({
      dedupKey: validated.dedupKey, channel: binding.channel,
      conversationId: binding.conversationId, sessionId: binding.sessionId,
      principalId: binding.principalId, bindingGeneration: binding.generation,
      accountId: binding.accountId,
      externalUserId: binding.externalUserId,
      externalConversationId: binding.externalConversationId,
      externalMessageId: input.externalMessageId,
      ...(input.providerRequestId ? { providerRequestId: input.providerRequestId } : {}),
      text: input.text,
    });
  }

  submitDesktop(text: string, clientMessageId?: string): AgentRunReceipt {
    const accepted = this.accept(this.store.desktop(), { externalMessageId: clientMessageId ?? randomUUID(), text });
    this.dispatch(accepted.record.requestId);
    return { accepted: true, runId: accepted.record.requestId,
      status: mappedStatus(accepted.record.status), duplicate: accepted.duplicate };
  }

  async handleWecom(message: NormalizedChannelMessage, transport: ChannelTransport): Promise<ChannelInboundReceipt> {
    if (message.conversationType !== 'single') return { accepted: false, code: 'group_disabled' };
    const binding = this.store.wecomForMessage(message.connectionId, message.senderId, message.conversationId);
    if (!binding) return { accepted: false, code: 'unpaired' };
    if (message.messageType !== 'text' || !message.text?.trim()) {
      return { accepted: false, code: 'unsupported_message' };
    }
    const accepted = this.accept(binding, {
      externalMessageId: message.providerMessageId, providerRequestId: message.providerRequestId,
      text: message.text.trim(),
    });
    this.transports.set(message.connectionId, transport);
    this.dispatch(accepted.record.requestId);
    return { accepted: true, duplicate: accepted.duplicate, runId: accepted.record.requestId };
  }

  recoverWecom(connectionId: string, transport: ChannelTransport): void {
    this.transports.set(connectionId, transport);
    for (const request of this.store.unsettled()) {
      if (request.channel === 'wecom' && request.accountId === connectionId) this.dispatch(request.requestId);
    }
    for (const request of this.store.pendingReplies(connectionId)) this.dispatch(request.requestId);
    // A completed request with a pending reply may have been interrupted before the first write.
    // Never retry accepted, failed or unknown delivery without an explicit later decision.
  }

  recover(): void {
    for (const request of this.store.unsettled()) this.dispatch(request.requestId);
    for (const connectionId of this.transports.keys()) {
      for (const request of this.store.pendingReplies(connectionId)) this.dispatch(request.requestId);
    }
  }

  private dispatch(requestId: string): void {
    if (this.inFlight.has(requestId)) return;
    const task = this.run(requestId).catch(() => undefined).finally(() => this.inFlight.delete(requestId));
    this.inFlight.set(requestId, task);
  }

  private async run(requestId: string): Promise<void> {
    const request = this.store.get(requestId);
    if (!request) return;
    if (request.status === 'completed') { await this.deliver(request); return; }
    if (request.status !== 'accepted' && request.status !== 'running') return;
    this.store.markRunning(requestId);
    // Worker task IDs are durable. Query first; a lost IPC response never causes a second turn.
    const current = await this.worker.task(requestId);
    const latest = this.store.get(requestId)!;
    if (latest.cancelRequested && !current) {
      this.store.finish(requestId, 'cancelled', undefined, 'cancelled_before_dispatch');
      await this.deliver(this.store.get(requestId)!);
      return;
    }
    if (latest.cancelRequested && current?.status === 'running') this.worker.cancel(requestId);
    const result = current?.status === 'completed' || current?.status === 'failed'
      || current?.status === 'cancelled' || current?.status === 'interrupted'
      ? current
      : await (async () => {
        const pending = this.worker.prompt(requestId, request.text, Date.now() + 300_000, request.sessionId);
        if (this.store.get(requestId)?.cancelRequested) this.worker.cancel(requestId);
        return pending;
      })();
    this.finish(requestId, result);
    await this.deliver(this.store.get(requestId)!);
  }

  private finish(requestId: string, result: AssistantTaskRecord): void {
    if (result.status === 'running') return;
    if (result.status === 'completed') this.store.finish(requestId, 'completed', result.message);
    else this.store.finish(requestId, result.status, undefined, result.error ?? result.status);
  }

  private async deliver(request: AssistantHostRequest): Promise<void> {
    if (request.channel !== 'wecom' || !request.providerRequestId) return;
    if (this.linkChanges > 0) return;
    const transport = this.transports.get(request.accountId);
    if (!transport || !transport.isReady?.()) return;
    const content = request.status === 'completed' ? request.responseText
      : request.status === 'failed' ? '助理处理失败，请在桌面端查看。'
        : request.status === 'cancelled' ? '任务已取消。'
          : request.status === 'interrupted' ? '助理任务已中断，请在桌面端核对。' : undefined;
    if (!content || !this.store.beginDelivery(request.requestId)) return;
    if (!this.store.canDeliver(request) || this.linkChanges > 0) {
      this.store.finishDelivery(request.requestId, 'failed', 'binding_revoked');
      return;
    }
    const sending = transport.reply({ providerRequestId: request.providerRequestId,
      providerMessageId: request.externalMessageId }, request.requestId, content)
      .catch(() => ({ status: 'unknown' as const, code: 'transport_uncertain' }))
      .then((result) => this.store.finishDelivery(request.requestId, result.status,
        result.status === 'accepted' ? undefined : result.code));
    this.deliveries.add(sending);
    try { await sending; } finally { this.deliveries.delete(sending); }
  }

  async getDesktopRun(requestId: string): Promise<AgentRunRecord | undefined> {
    const request = this.store.get(requestId);
    if (!request || request.channel !== 'desktop' || request.principalId !== this.store.desktop().principalId) return undefined;
    const task = await this.worker.task(requestId).catch(() => undefined);
    if (request.status === 'accepted' || request.status === 'running') {
      if (task && task.status !== 'running') this.finish(requestId, task);
      else this.dispatch(requestId);
    }
    const latest = this.store.get(requestId)!;
    return {
      runId: requestId,
      owner: { entryPoint: 'desktop', identity: { kind: 'local_user', subjectId: 'local-user',
        authorityId: 'local-desktop', authenticatedBy: 'electron' } },
      context: { workspaceId: this.workspaceId,
        conversation: { namespace: 'assistant', conversationId: latest.conversationId },
        delivery: { kind: 'desktop' } },
      requestFingerprint: latest.dedupKey,
      inputDigest: createHash('sha256').update(latest.text).digest('hex'),
      status: mappedStatus(latest.status), externalEffectState: 'none',
      createdAt: latest.createdAt, updatedAt: latest.updatedAt,
      ...(task?.progress ? { progress: task.progress } : {}),
      ...(latest.responseText ? { output: { message: latest.responseText, tools: [], steps: task?.progress?.steps } } : {}),
      ...(latest.errorCode ? { failure: { code: latest.errorCode,
        message: latest.errorCode, retryable: false } } : {}),
    };
  }

  transcript(): DesktopTranscriptMessage[] {
    return this.store.transcript(this.store.desktop().conversationId);
  }

  timeline(): DesktopTranscriptMessage[] {
    const desktop = this.transcript().map((message) => ({ ...message, channel: 'desktop' as const }));
    const link = this.store.wecomLink();
    if (!link) return desktop;
    const wecom = this.store.transcript(link.conversationId)
      .map((message) => ({ ...message, channel: 'wecom' as const }));
    return [...desktop, ...wecom].sort((a, b) => a.at.localeCompare(b.at));
  }

  /** IDs come only from the authorized host transcript, never from renderer-supplied task paths. */
  async transcriptWithProgress(all = false): Promise<DesktopTranscriptMessage[]> {
    const messages = all ? this.timeline() : this.transcript();
    const ids = messages.flatMap((message) => message.run?.runId ? [message.run.runId] : []).slice(-200);
    if (!ids.length) return messages;
    const records = await this.worker.taskRecords(ids).catch(() => []);
    const byId = new Map(records.map((record) => [record.id, record]));
    return messages.map((message) => {
      const record = message.run?.runId ? byId.get(message.run.runId) : undefined;
      return message.run && record?.progress
        ? { ...message, run: { ...message.run, steps: record.progress.steps } } : message;
    });
  }

  sourceChanges(afterEventId = 0): Array<{ eventId: number; change: AssistantSourceChange }> {
    return this.store.sourceChanges(afterEventId);
  }

  resolveSourceContent(ref: string, principalId: string):
    { userText: string; assistantText: string } | { transcript: DesktopTranscriptMessage[] } | undefined {
    return this.store.resolveContentRef(ref, principalId);
  }

  cancelDesktop(requestId: string): AgentRunCancellationReceipt {
    const request = this.store.get(requestId);
    if (!request || request.channel !== 'desktop') return { runId: requestId, result: 'not_found' };
    if (['completed', 'failed', 'cancelled', 'interrupted'].includes(request.status)) {
      return { runId: requestId, result: 'already_terminal', status: mappedStatus(request.status) };
    }
    this.store.requestCancel(requestId);
    this.worker.cancel(requestId);
    this.dispatch(requestId);
    return { runId: requestId, result: 'cancellation_requested', status: mappedStatus(request.status) };
  }
}

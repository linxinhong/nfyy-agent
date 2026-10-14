import { spawn, type ChildProcess } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { isSea } from 'node:sea';
import { resolveAssistantModelConfig, type AssistantModelSelection } from './assistant-model.js';
import type { AssistantSourceHost } from '@yuanpu-agent/assistant';
import type { AssistantSuggestion } from '@yuanpu-agent/assistant';
import type { AssistantMemoryView, AssistantWorkspaceSnapshot, RunProgress } from '@yuanpu-agent/protocol';
import type { AssistantDelegationBrief, AssistantDelegationRecord,
  AssistantEvidenceRef } from '@yuanpu-agent/protocol';
import type { AssistantDelegationService } from './assistant-delegation-service.js';

export interface AssistantTaskRecord {
  id: string;
  sessionId?: string;
  status: 'running' | 'completed' | 'cancelled' | 'interrupted' | 'failed';
  message?: string;
  error?: string;
  updatedAt: string;
  progress?: RunProgress;
}

interface WorkerCommand { executable: string; args: string[] }

export interface AssistantWorkerManagerOptions {
  home: string;
  model: AssistantModelSelection;
  sources?: AssistantSourceHost;
  delegations?: AssistantDelegationService;
  deliverSuggestion?: (id: string, content: string) => Promise<{
    status: 'accepted' | 'failed' | 'unknown' | 'deferred'; ref?: string }>;
  command?: WorkerCommand;
  startupTimeoutMs?: number;
  shutdownGraceMs?: number;
  onError?: (error: Error) => void;
}

function command(): WorkerCommand {
  return isSea()
    ? { executable: process.execPath, args: ['--assistant-worker'] }
    : { executable: process.execPath, args: [process.argv[1]!, '--assistant-worker'] };
}

/** The Runtime host owns one Assistant Worker; UI client attachment has no lifecycle authority. */
export class AssistantWorkerManager {
  private child?: ChildProcess;
  private ready = false;
  private shouldRun = false;
  private startPromise?: Promise<void>;
  private restartTimer?: NodeJS.Timeout;
  private restartCount = 0;
  private pending = new Map<string, { resolve(value: AssistantTaskRecord | undefined): void; reject(error: Error): void }>();
  private suggestionPending = new Map<string, { resolve(value: unknown): void; reject(error: Error): void }>();

  constructor(private readonly options: AssistantWorkerManagerOptions) {}

  get workerPid(): number | undefined { return this.ready ? this.child?.pid : undefined; }

  private report(error: unknown): void {
    this.options.onError?.(error instanceof Error ? error : new Error(String(error)));
  }

  private rejectPending(error: Error): void {
    for (const pending of this.pending.values()) pending.reject(error);
    this.pending.clear();
    for (const pending of this.suggestionPending.values()) pending.reject(error);
    this.suggestionPending.clear();
  }

  private async launch(): Promise<void> {
    const selected = this.options.command ?? command();
    const child = spawn(selected.executable, selected.args, {
      stdio: ['ignore', 'pipe', 'pipe', 'ipc'],
      detached: process.platform !== 'win32',
      windowsHide: true,
      env: { ...process.env, PI_CODING_AGENT_DIR: '', PI_CODING_AGENT_SESSION_DIR: '' },
    });
    this.child = child;
    let stderr = '';
    child.stderr?.on('data', (chunk: Buffer) => { if (stderr.length < 4096) stderr += chunk.toString(); });
    const ready = new Promise<void>((resolve, reject) => {
      const timeout = setTimeout(() => reject(new Error('Assistant Worker startup timed out.')),
        this.options.startupTimeoutMs ?? 10_000);
      let settled = false;
      const settle = (callback: () => void) => {
        if (settled) return;
        settled = true;
        clearTimeout(timeout);
        callback();
      };
      child.once('error', (error) => settle(() => reject(error)));
      child.once('exit', (code, signal) => settle(() => reject(new Error(
        `Assistant Worker exited before readiness (${code ?? signal}): ${stderr}`,
      ))));
      child.on('message', (value: unknown) => {
        const message = value as Record<string, unknown>;
        if (!message || typeof message !== 'object') return;
        if (message.kind === 'ready') {
          this.ready = true;
          settle(resolve);
        } else if (message.kind === 'model-request' && typeof message.id === 'string') {
          void resolveAssistantModelConfig(this.options.model).then(
            (config) => child.send?.({ kind: 'model', id: message.id, config }),
            (error) => child.send?.({ kind: 'model', id: message.id,
              error: error instanceof Error ? error.message : String(error) }),
          );
        } else if (message.kind === 'source-request' && typeof message.id === 'string') {
          const request = message as { id: string; method?: unknown; args?: unknown };
          const args = Array.isArray(request.args) ? request.args : [];
          let result: Promise<unknown>;
          const host = this.options.sources;
          try {
            if (!host) throw new Error('Assistant source host is unavailable.');
            if (request.method === 'listChanges' && args.length === 3) {
              result = host.listChanges(args[0], args[1], args[2]);
            } else if (request.method === 'currentSource' && args.length === 2) {
              result = host.currentSource(args[0], args[1]);
            } else if (request.method === 'readSource' && args.length === 5) {
              result = host.readSource(args[0], args[1], args[2], args[3], args[4]);
            } else throw new Error('Invalid Assistant source request.');
          } catch (error) { result = Promise.reject(error); }
          void result.then((value) => child.send?.({ kind: 'source-result', id: request.id, value }),
            (error) => child.send?.({ kind: 'source-result', id: request.id,
              error: error instanceof Error ? error.message : String(error) }));
        } else if (message.kind === 'source-error' && typeof message.error === 'string') {
          this.report(new Error(`Assistant source scan: ${message.error}`));
        } else if (message.kind === 'delegation-request' && typeof message.id === 'string') {
          const request = message as { id: string; method?: unknown; args?: unknown };
          const args = Array.isArray(request.args) ? request.args : [];
          let result: Promise<unknown>;
          const host = this.options.delegations;
          try {
            if (!host) throw new Error('Assistant delegation host is unavailable.');
            if (request.method === 'start' && args.length === 1 && args[0]
              && typeof args[0] === 'object') result = host.start(args[0] as AssistantDelegationBrief);
            else if (request.method === 'status' && args.length === 1 && typeof args[0] === 'string') {
              result = host.status(args[0]);
            } else if (request.method === 'followUp' && args.length === 4
              && args.every((arg) => typeof arg === 'string')) {
              result = host.followUp(args[0] as string, args[1] as string, args[2] as string,
                args[3] as string);
            } else if (request.method === 'cancel' && args.length === 2
              && args.every((arg) => typeof arg === 'string')) {
              result = host.cancel(args[0] as string, args[1] as string);
            }
            else throw new Error('Invalid Assistant delegation request.');
          } catch (error) { result = Promise.reject(error); }
          void result.then((value) => child.send?.({ kind: 'delegation-result', id: request.id, value }),
            (error) => child.send?.({ kind: 'delegation-result', id: request.id,
              error: error instanceof Error ? error.message : String(error) }));
        } else if (message.kind === 'delegation-error' && typeof message.error === 'string') {
          this.report(new Error(`Assistant delegation wake: ${message.error}`));
        } else if (message.kind === 'suggestion-delivery-request' && typeof message.id === 'string') {
          const id = message.id;
          const suggestionId = message.suggestionId;
          const content = message.content;
          const evidence = message.evidence;
          const delivery = typeof suggestionId === 'string' && typeof content === 'string'
            && Array.isArray(evidence) && evidence.length > 0 && evidence.length <= 100
            && evidence.every((ref: unknown) => Boolean(ref && typeof ref === 'object'
              && typeof (ref as AssistantEvidenceRef).sourceId === 'string'
              && typeof (ref as AssistantEvidenceRef).sourceVersion === 'string'))
            && this.options.deliverSuggestion && this.options.sources
            ? (async () => {
              for (const ref of evidence as AssistantEvidenceRef[]) {
                const current = await this.options.sources!.currentSource(ref.sourceId,
                  { kind: 'personal', id: 'local-user' });
                if (current.status !== 'available' || current.sourceVersion !== ref.sourceVersion) {
                  return { status: 'deferred' as const };
                }
              }
              return this.options.deliverSuggestion!(suggestionId, content);
            })()
            : Promise.reject(new Error('Assistant suggestion delivery is unavailable.'));
          void delivery.then((value) => child.send?.({ kind: 'suggestion-delivery-result', id, value }),
            (error) => child.send?.({ kind: 'suggestion-delivery-result', id,
              error: error instanceof Error ? error.message : String(error) }));
        } else if ((message.kind === 'suggestion-result' || message.kind === 'workspace-result')
          && typeof message.correlationId === 'string') {
          const pending = this.suggestionPending.get(message.correlationId);
          if (!pending) return;
          this.suggestionPending.delete(message.correlationId);
          if (typeof message.error === 'string') pending.reject(new Error(message.error));
          else pending.resolve(message.value);
        } else if ((message.kind === 'result' || message.kind === 'task') && typeof message.correlationId === 'string') {
          const pending = this.pending.get(message.correlationId);
          if (!pending) return;
          this.pending.delete(message.correlationId);
          if (typeof message.error === 'string') pending.reject(new Error(message.error));
          else pending.resolve(message.record as AssistantTaskRecord | undefined);
        }
      });
    });
    child.once('exit', (code, signal) => {
      if (this.child !== child) return;
      this.child = undefined;
      this.ready = false;
      this.rejectPending(new Error(`Assistant Worker exited (${code ?? signal}).`));
      if (this.shouldRun) {
        this.report(new Error(`Assistant Worker exited (${code ?? signal}).`));
        this.scheduleRestart();
      }
    });
    try {
      child.send({ kind: 'bootstrap', home: this.options.home, parentPid: process.pid });
      await ready;
      if (!this.shouldRun) throw new Error('Assistant Worker startup was cancelled.');
    } catch (error) {
      await this.terminate(child);
      throw error;
    }
  }

  private scheduleRestart(): void {
    if (this.restartTimer || !this.shouldRun) return;
    if (this.restartCount >= 3) {
      this.report(new Error('Assistant Worker restart budget exhausted.'));
      return;
    }
    const delay = 250 * (2 ** this.restartCount++);
    this.restartTimer = setTimeout(() => {
      this.restartTimer = undefined;
      if (this.shouldRun) void this.start().catch((error) => this.report(error));
    }, delay);
    this.restartTimer.unref();
  }

  async start(): Promise<void> {
    this.shouldRun = true;
    if (this.ready) return;
    this.startPromise ??= this.launch().finally(() => { this.startPromise = undefined; });
    return this.startPromise;
  }

  private request(message: Record<string, unknown>): Promise<AssistantTaskRecord | undefined> {
    if (!this.ready || !this.child?.connected) return Promise.reject(new Error('Assistant Worker is unavailable.'));
    const correlationId = randomUUID();
    return new Promise((resolve, reject) => {
      this.pending.set(correlationId, { resolve, reject });
      this.child!.send({ ...message, correlationId }, (error) => {
        if (error) {
          this.pending.delete(correlationId);
          reject(error);
        }
      });
    });
  }

  async prompt(id: string, text: string, deadlineAt: number, sessionId?: string): Promise<AssistantTaskRecord> {
    const result = await this.request({ kind: 'prompt', id, text, deadlineAt, sessionId });
    if (!result) throw new Error('Assistant Worker did not return a task record.');
    return result;
  }

  task(id: string): Promise<AssistantTaskRecord | undefined> { return this.request({ kind: 'task', id }); }
  taskRecords(ids: string[]): Promise<AssistantTaskRecord[]> {
    return this.suggestionOperation({ kind: 'task-records', ids }) as Promise<AssistantTaskRecord[]>;
  }
  private suggestionOperation(message: Record<string, unknown>): Promise<unknown> {
    if (!this.ready || !this.child?.connected) return Promise.reject(new Error('Assistant Worker is unavailable.'));
    const correlationId = randomUUID();
    return new Promise((resolve, reject) => {
      this.suggestionPending.set(correlationId, { resolve, reject });
      this.child!.send({ ...message, correlationId }, (error) => {
        if (error) { this.suggestionPending.delete(correlationId); reject(error); }
      });
    });
  }
  suggestions(): Promise<{ items: AssistantSuggestion[]; pausedUntil?: string }> {
    return this.suggestionOperation({ kind: 'suggestion-list' }) as
      Promise<{ items: AssistantSuggestion[]; pausedUntil?: string }>;
  }
  suggestionFeedback(id: string, action: 'ignored' | 'snoozed' | 'accepted',
    snoozedUntil?: string): Promise<AssistantSuggestion> {
    return this.suggestionOperation({ kind: 'suggestion-feedback', id, action, snoozedUntil }) as
      Promise<AssistantSuggestion>;
  }
  suggestionPause(until?: string): Promise<{ pausedUntil?: string }> {
    return this.suggestionOperation({ kind: 'suggestion-pause', until }) as
      Promise<{ pausedUntil?: string }>;
  }
  suggestionRead(id: string): Promise<AssistantSuggestion> {
    return this.suggestionOperation({ kind: 'suggestion-read', id }) as Promise<AssistantSuggestion>;
  }
  workspace(memoryLimit?: number): Promise<AssistantWorkspaceSnapshot> {
    return this.suggestionOperation({ kind: 'workspace-snapshot', memoryLimit }) as
      Promise<AssistantWorkspaceSnapshot>;
  }
  correctMemory(id: string, expectedVersion: number, text: string,
    revisionId: string): Promise<AssistantMemoryView> {
    return this.suggestionOperation({ kind: 'workspace-correct', id, expectedVersion, text, revisionId }) as
      Promise<AssistantMemoryView>;
  }
  forgetMemory(id: string): Promise<{ forgottenIds: string[] }> {
    return this.suggestionOperation({ kind: 'workspace-forget', id }) as
      Promise<{ forgottenIds: string[] }>;
  }
  importSavedMemory(savedId: string, surface: 'work' | 'assistant', text: string,
    savedAt: string): Promise<AssistantMemoryView> {
    return this.suggestionOperation({ kind: 'workspace-import', savedId, surface, text, savedAt }) as
      Promise<AssistantMemoryView>;
  }
  pauseOrganizing(until?: string): Promise<{ organizingPausedUntil?: string }> {
    return this.suggestionOperation({ kind: 'workspace-pause', until }) as
      Promise<{ organizingPausedUntil?: string }>;
  }
  followUpDelegation(id: string, text: string): Promise<AssistantDelegationRecord> {
    return this.suggestionOperation({ kind: 'workspace-delegation', id, action: 'follow-up', text }) as
      Promise<AssistantDelegationRecord>;
  }
  cancelDelegation(id: string): Promise<AssistantDelegationRecord> {
    return this.suggestionOperation({ kind: 'workspace-delegation', id, action: 'cancel' }) as
      Promise<AssistantDelegationRecord>;
  }
  refreshSources(): void { if (this.ready) this.child?.send({ kind: 'refresh-sources' }); }
  cancel(id: string): void { if (this.ready) this.child?.send({ kind: 'cancel', id }); }
  notifyDelegation(record: AssistantDelegationRecord): void {
    if (this.ready) this.child?.send({ kind: 'delegation-event', record });
  }

  private async terminate(child: ChildProcess): Promise<void> {
    if (child.exitCode !== null || child.signalCode !== null) return;
    if (child.connected) child.send({ kind: 'shutdown' });
    const exited = await new Promise<boolean>((resolve) => {
      const timeout = setTimeout(() => resolve(false), this.options.shutdownGraceMs ?? 5_000);
      child.once('exit', () => { clearTimeout(timeout); resolve(true); });
    });
    if (exited) return;
    if (process.platform === 'win32') child.kill('SIGKILL');
    else try { process.kill(-child.pid!, 'SIGKILL'); } catch { child.kill('SIGKILL'); }
  }

  async stop(): Promise<void> {
    this.shouldRun = false;
    this.ready = false;
    if (this.restartTimer) clearTimeout(this.restartTimer);
    this.restartTimer = undefined;
    const child = this.child;
    if (child) await this.terminate(child);
    await this.startPromise?.catch(() => undefined);
    const late = this.child;
    if (late && late !== child) await this.terminate(late);
    this.child = undefined;
    this.rejectPending(new Error('Assistant Worker stopped.'));
  }
}

import {
  PROTOCOL_VERSION,
  RUNTIME_ROUTES,
  capabilityApprovalSigningPayload,
  type CapabilityApprovalDecisionInput,
  type CapabilityApprovalDecisionResult,
  type CapabilityApprovalSummary,
  type ChatResponse,
  type DesktopConversationSurface,
  type DesktopTranscriptSurface,
  type DesktopTranscriptMessage,
  type AssistantLinkStatus,
  type AssistantMirrorStatus,
  type InstalledPlugin,
  type LocalSkillList,
  type McpOwnershipConflict,
  type PluginConfigDocument,
  type PluginConfigInput,
  type PluginConfigScope,
  type PluginConfigValidation,
  type PluginSearchResult,
  type RuntimeGreeting,
  type RuntimeInfo,
  type ModelSettings,
  type ModelCatalog,
  type SaveModelSettingsInput,
  type HotkeySettings,
  type SaveHotkeyInput,
  type RuntimeUpdateState,
  type HostEvent,
  type HostEventReceipt,
  type NotificationNavigationTarget,
  type NotificationTargetValidation,
  type AgentRunRecord,
  type AgentRunCancellationReceipt,
  type AgentRunReceipt,
  type PrivateImRunSummary,
  type ScheduleHistoryRecord,
  type ScheduleInput,
  type SchedulePrivateContact,
  type ScheduleRecord,
  type WecomConnectionList,
  type WecomConnectionSummary,
  type WecomConnectionConfigInput,
  type TailscaleConnectionSettings,
  type SaveTailscaleConnectionInput,
} from '@yuanpu-agent/protocol';
import { execFile, spawn, type ChildProcessWithoutNullStreams } from 'node:child_process';
import { generateKeyPairSync, randomBytes, sign } from 'node:crypto';
import { homedir } from 'node:os';
import { join, resolve } from 'node:path';
import { promisify } from 'node:util';

import { RuntimeUpdater, type RuntimeActivation } from './runtime-updater.js';
import { AuthenticatedHostEventClient } from './host-event-client.js';

const DEFAULT_MANIFEST_URL =
  'https://github.com/linxinhong/yuanpu-agent/releases/latest/download/manifest.json';

interface RuntimeReady extends RuntimeInfo {
  event: 'ready';
  host: string;
  port: number;
}

interface RuntimeCommand extends RuntimeActivation {
  args: string[];
}

class RuntimeProtocolIncompatibilityError extends Error {}

export type RuntimeUpdateRecoveryReason = 'incompatible_protocol' | 'activation_failed';

export interface RuntimeManagerOptions {
  command?: { executable: string; args: string[] };
  startupTimeoutMs?: number;
  shutdownGraceMs?: number;
  restartLimit?: number;
  restartWindowMs?: number;
  restartBaseDelayMs?: number;
  activationStabilityMs?: number;
  onError?: (error: Error) => void;
  onUpdateRecovery?: (reason: RuntimeUpdateRecoveryReason) => void;
  /** Loopback browser-control endpoint delivered to the runtime bootstrap. */
  browserControl?: { port: number; token: string };
}

const execFileAsync = promisify(execFile);

export class RuntimeManager {
  private child?: ChildProcessWithoutNullStreams;
  private ready?: RuntimeReady;
  private startPromise?: Promise<RuntimeReady>;
  private stopPromise?: Promise<void>;
  private restartTimer?: NodeJS.Timeout;
  private activationConfirmationTimer?: NodeJS.Timeout;
  private hostEventClient?: AuthenticatedHostEventClient;
  private restartAttempts: number[] = [];
  private shouldRun = false;
  private readonly token = randomBytes(32).toString('hex');
  private readonly approvalKeyPair = generateKeyPairSync('ed25519');
  private readonly updater: RuntimeUpdater;
  private readonly options: Required<Omit<RuntimeManagerOptions, 'command' | 'onError' | 'onUpdateRecovery' | 'browserControl'>>
    & Pick<RuntimeManagerOptions, 'command' | 'onError' | 'onUpdateRecovery' | 'browserControl'>;

  constructor(
    private readonly appPath: string,
    private readonly resourcesPath: string,
    private readonly userDataPath: string,
    private readonly packaged: boolean,
    desktopVersion: string,
    options: RuntimeManagerOptions = {},
  ) {
    const yuanpuHome = resolve(process.env.YUANPU_HOME || join(homedir(), '.yuanpu'));
    this.updater = new RuntimeUpdater({
      runtimeRoot: this.runtimeRoot,
      desktopVersion,
      metadataDatabasePath: join(yuanpuHome, 'workflows', 'automation.sqlite'),
    });
    this.options = {
      command: options.command,
      startupTimeoutMs: options.startupTimeoutMs ?? 15_000,
      shutdownGraceMs: options.shutdownGraceMs ?? 10_000,
      restartLimit: options.restartLimit ?? 3,
      restartWindowMs: options.restartWindowMs ?? 60_000,
      restartBaseDelayMs: options.restartBaseDelayMs ?? 250,
      activationStabilityMs: options.activationStabilityMs ?? 2_000,
      onError: options.onError,
      onUpdateRecovery: options.onUpdateRecovery,
      browserControl: options.browserControl,
    };
  }

  private get runtimeRoot(): string {
    return join(this.userDataPath, 'runtime');
  }

  private packagedExecutable(): string {
    const target = `${process.platform}-${process.arch}`;
    const suffix = process.platform === 'win32' ? '.exe' : '';
    return join(this.resourcesPath, 'runtime', `YuanpuAgentRuntime-${target}${suffix}`);
  }

  private async command(): Promise<RuntimeCommand> {
    if (this.options.command) return { ...this.options.command, pending: false };
    if (!this.packaged) {
      return {
        executable: process.env.YUANPU_NODE_BINARY || 'node',
        args: [resolve(this.appPath, '../runtime/dist/index.cjs')],
        pending: false,
      };
    }
    return { ...(await this.updater.prepareActivation(this.packagedExecutable())), args: [] };
  }

  private capabilityEnvironment(): NodeJS.ProcessEnv {
    if (this.packaged) {
      const root = join(this.resourcesPath, 'capabilities', 'builtin.python.echo', 'YuanpuEchoMcp');
      return {
        YUANPU_BUILTIN_AGENTS_ROOT: join(this.resourcesPath, 'app', 'agents'),
        YUANPU_PYTHON_MCP_EXECUTABLE: join(root, process.platform === 'win32' ? 'YuanpuEchoMcp.exe' : 'YuanpuEchoMcp'),
        YUANPU_PYTHON_MCP_ROOT: root,
        YUANPU_PYTHON_MCP_ARGS: '[]',
        YUANPU_CAPABILITY_TRUST_ROOT_FILE: join(
          this.resourcesPath,
          'capabilities',
          'builtin.python.echo',
          'trust-root.json',
        ),
      };
    }
    const pythonRoot = resolve(this.appPath, '../python-capabilities');
    return {
      YUANPU_BUILTIN_AGENTS_ROOT: resolve(__dirname, '../../app/agents'),
      YUANPU_PYTHON_MCP_EXECUTABLE: process.platform === 'win32'
        ? join(pythonRoot, '.venv', 'Scripts', 'python.exe')
        : join(pythonRoot, '.venv', 'bin', 'python'),
      YUANPU_PYTHON_MCP_ROOT: pythonRoot,
      YUANPU_PYTHON_MCP_ARGS: JSON.stringify(['-m', 'yuanpu_echo_mcp']),
      ...(process.env.YUANPU_CAPABILITY_TRUST_ROOT_FILE
        ? { YUANPU_CAPABILITY_TRUST_ROOT_FILE: process.env.YUANPU_CAPABILITY_TRUST_ROOT_FILE }
        : {}),
    };
  }

  private reportError(error: unknown): void {
    const normalized = error instanceof Error ? error : new Error(String(error));
    if (this.options.onError) this.options.onError(normalized);
    else console.error(normalized.message);
  }

  private async waitForExit(child: ChildProcessWithoutNullStreams, timeoutMs: number): Promise<boolean> {
    if (child.exitCode !== null || child.signalCode !== null) return true;
    return await new Promise<boolean>((resolveExit) => {
      const timeout = setTimeout(() => {
        child.off('exit', onExit);
        resolveExit(false);
      }, timeoutMs);
      const onExit = () => {
        clearTimeout(timeout);
        resolveExit(true);
      };
      child.once('exit', onExit);
    });
  }

  private async forceTerminate(child: ChildProcessWithoutNullStreams): Promise<void> {
    if (!child.pid || child.exitCode !== null || child.signalCode !== null) return;
    if (process.platform === 'win32') {
      await execFileAsync('taskkill', ['/PID', String(child.pid), '/T', '/F'], {
        timeout: 5_000,
        windowsHide: true,
      }).catch(() => child.kill('SIGKILL'));
      return;
    }
    try { process.kill(-child.pid, 'SIGKILL'); } catch { child.kill('SIGKILL'); }
  }

  private async terminate(child: ChildProcessWithoutNullStreams): Promise<void> {
    if (child.exitCode !== null || child.signalCode !== null) return;
    child.kill('SIGTERM');
    if (await this.waitForExit(child, this.options.shutdownGraceMs)) return;
    await this.forceTerminate(child);
    await this.waitForExit(child, 1_000);
  }

  private scheduleRestart(): void {
    if (!this.shouldRun || this.restartTimer) return;
    const now = Date.now();
    const cutoff = now - this.options.restartWindowMs;
    this.restartAttempts = this.restartAttempts.filter((attempt) => attempt >= cutoff);
    if (this.restartAttempts.length >= this.options.restartLimit) {
      this.reportError(new Error('Runtime restart budget is exhausted; restart the App to retry.'));
      return;
    }
    const attempt = this.restartAttempts.length;
    this.restartAttempts.push(now);
    const delay = this.options.restartBaseDelayMs * (2 ** attempt);
    this.restartTimer = setTimeout(() => {
      this.restartTimer = undefined;
      if (!this.shouldRun) return;
      void this.start().catch((error) => {
        this.reportError(error);
        this.scheduleRestart();
      });
    }, delay);
    this.restartTimer.unref();
  }

  private scheduleActivationConfirmation(
    child: ChildProcessWithoutNullStreams,
    executable: string,
  ): void {
    if (this.activationConfirmationTimer) clearTimeout(this.activationConfirmationTimer);
    this.activationConfirmationTimer = setTimeout(() => {
      this.activationConfirmationTimer = undefined;
      if (!this.shouldRun || this.child !== child || !this.ready || child.exitCode !== null) return;
      void this.updater.confirmActivation(executable).catch((error) => this.reportError(error));
    }, this.options.activationStabilityMs);
    this.activationConfirmationTimer.unref();
  }

  private async launch(command: RuntimeCommand): Promise<{ child: ChildProcessWithoutNullStreams; ready: RuntimeReady }> {
    if (!this.shouldRun) throw new Error('Runtime start was cancelled because the App is stopping.');
    const child = await new Promise<ChildProcessWithoutNullStreams>((resolveSpawn, rejectSpawn) => {
      const child = spawn(
        command.executable,
        [...command.args, '--serve', '--port', '0'],
        {
          stdio: ['pipe', 'pipe', 'pipe'],
          detached: process.platform !== 'win32',
          windowsHide: true,
          env: { ...process.env, ...this.capabilityEnvironment() },
        },
      );
      child.once('spawn', () => resolveSpawn(child));
      child.once('error', rejectSpawn);
    });
    this.child = child;
    if (!this.shouldRun) {
      await this.terminate(child);
      throw new Error('Runtime start was cancelled because the App is stopping.');
    }
    let stderr = '';
    child.stderr.on('data', (chunk: Buffer) => {
      if (stderr.length < 64 * 1024) stderr += chunk.toString();
    });
    child.once('exit', (code, signal) => {
      if (this.child !== child) return;
      if (this.activationConfirmationTimer) clearTimeout(this.activationConfirmationTimer);
      this.activationConfirmationTimer = undefined;
      const wasReady = Boolean(this.ready);
      this.child = undefined;
      this.ready = undefined;
      if (code && code !== 0) {
        this.reportError(new Error(`Runtime exited with code ${code}: ${stderr}`));
      } else if (signal && this.shouldRun) {
        this.reportError(new Error(`Runtime exited from signal ${signal}.`));
      }
      if (wasReady && this.shouldRun) this.scheduleRestart();
    });

    try {
      const approvalPublicKey = this.approvalKeyPair.publicKey.export({
        type: 'spki',
        format: 'der',
      }).toString('base64');
      child.stdin.end(`${JSON.stringify({
        token: this.token,
        approvalPublicKey,
        parentPid: process.pid,
        ...(this.options.browserControl ? { browserControl: this.options.browserControl } : {}),
      })}\n`);
      let stdout = '';
      const ready = await new Promise<RuntimeReady>((resolveReady, rejectReady) => {
        let settled = false;
        const settle = (callback: () => void) => {
          if (settled) return;
          settled = true;
          clearTimeout(timeout);
          callback();
        };
        const timeout = setTimeout(() => {
          settle(() => rejectReady(new Error(`Runtime startup timed out: ${stderr}`)));
        }, this.options.startupTimeoutMs);
        child.stdout.on('data', (chunk: Buffer) => {
          if (settled) return;
          stdout += chunk.toString();
          if (stdout.length > 64 * 1024) {
            settle(() => rejectReady(new Error('Runtime startup response exceeded 64 KiB.')));
            return;
          }
          const lineEnd = stdout.indexOf('\n');
          if (lineEnd < 0) return;
          try {
            const value = JSON.parse(stdout.slice(0, lineEnd)) as RuntimeReady;
            if (value.event !== 'ready') throw new Error('Runtime returned an invalid readiness event.');
            if (value.protocolVersion !== PROTOCOL_VERSION) {
              throw new RuntimeProtocolIncompatibilityError(
                `Runtime protocol is incompatible: desktop expects ${PROTOCOL_VERSION}, Runtime reported ${String(value.protocolVersion)}.`,
              );
            }
            if (command.version && value.version !== command.version) {
              throw new Error(
                `Runtime update health check expected version ${command.version}, but Runtime reported ${String(value.version)}.`,
              );
            }
            settle(() => resolveReady(value));
          } catch (error) {
            settle(() => rejectReady(error));
          }
        });
        child.once('error', (error) => settle(() => rejectReady(error)));
        child.once('exit', (code, signal) => settle(() => rejectReady(new Error(
          `Runtime exited before becoming healthy (code=${String(code)}, signal=${String(signal)}): ${stderr}`,
        ))));
      });
      const response = await fetch(`http://${ready.host}:${ready.port}${RUNTIME_ROUTES.health}`, {
        headers: { authorization: `Bearer ${this.token}` },
        signal: AbortSignal.timeout(this.options.startupTimeoutMs),
      });
      if (!response.ok) throw new Error(`Runtime health check failed with HTTP ${response.status}.`);
      const health = await response.json() as RuntimeInfo;
      if (health.protocolVersion !== PROTOCOL_VERSION) {
        throw new RuntimeProtocolIncompatibilityError(
          `Runtime protocol is incompatible: desktop expects ${PROTOCOL_VERSION}, health reported ${String(health.protocolVersion)}.`,
        );
      }
      if (command.version && health.version !== command.version) {
        throw new Error(
          `Runtime update health check expected version ${command.version}, but health reported ${String(health.version)}.`,
        );
      }
      return { child, ready };
    } catch (error) {
      await this.terminate(child);
      throw error;
    }
  }

  private async startInternal(): Promise<RuntimeReady> {
    let command = await this.command();
    if (!this.shouldRun) throw new Error('Runtime start was cancelled because the App is stopping.');
    try {
      const launched = await this.launch(command);
      if (this.child !== launched.child || launched.child.exitCode !== null) {
        throw new Error('Runtime exited before activation health was confirmed.');
      }
      this.ready = launched.ready;
      if (command.pending) {
        this.scheduleActivationConfirmation(launched.child, command.executable);
      }
      return launched.ready;
    } catch (error) {
      if (!command.pending) throw error;
      if (this.child) await this.terminate(this.child);
      const previous = await this.updater.rollbackActivation(command.executable);
      command = {
        executable: previous ?? this.packagedExecutable(),
        args: [],
        pending: false,
      };
      try {
        const launched = await this.launch(command);
        if (this.child !== launched.child || launched.child.exitCode !== null) {
          throw new Error('Restored Runtime exited before health was confirmed.');
        }
        this.ready = launched.ready;
        this.reportError(new Error(
          `Runtime update failed and the previous version was restored: ${error instanceof Error ? error.message : String(error)}`,
        ));
        try {
          this.options.onUpdateRecovery?.(
            error instanceof RuntimeProtocolIncompatibilityError ? 'incompatible_protocol' : 'activation_failed',
          );
        } catch (noticeError) {
          this.reportError(noticeError);
        }
        return launched.ready;
      } catch (rollbackError) {
        throw new AggregateError(
          [error, rollbackError],
          'Runtime update failed and the previous Runtime could not be restored.',
        );
      }
    }
  }

  async start(): Promise<RuntimeReady> {
    if (this.stopPromise) await this.stopPromise;
    this.shouldRun = true;
    if (this.ready) return this.ready;
    if (this.startPromise) return this.startPromise;
    this.startPromise = this.startInternal().finally(() => {
      this.startPromise = undefined;
    });
    return this.startPromise;
  }

  private async request<T>(path: string, init?: RequestInit): Promise<T> {
    const runtime = await this.start();
    const response = await fetch(`http://${runtime.host}:${runtime.port}${path}`, {
      ...init,
      headers: {
        authorization: `Bearer ${this.token}`,
        ...init?.headers,
      },
    });
    if (!response.ok) {
      const body = await response.json().catch(() => ({})) as { error?: string; hint?: string };
      throw new Error([body.error || `Runtime request failed with HTTP ${response.status}`, body.hint]
        .filter(Boolean).join(' '));
    }
    return response.status === 204 ? undefined as T : await response.json() as T;
  }

  info(): Promise<RuntimeInfo> {
    return this.request(RUNTIME_ROUTES.health);
  }

  getModelSettings(): Promise<ModelSettings> {
    return this.request(RUNTIME_ROUTES.modelSettings);
  }

  getHotkeySettings(): Promise<HotkeySettings> {
    return this.request(RUNTIME_ROUTES.hotkeySettings);
  }

  saveHotkeySetting(input: SaveHotkeyInput): Promise<HotkeySettings> {
    return this.request(RUNTIME_ROUTES.hotkeySettings, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(input),
    });
  }

  getModelCatalog(provider?: string): Promise<ModelCatalog> {
    if (typeof RUNTIME_ROUTES.modelCatalog !== 'string') {
      throw new Error('模型列表尚未就绪，请重新启动桌面应用。');
    }
    return this.request(`${RUNTIME_ROUTES.modelCatalog}${provider ? `?provider=${encodeURIComponent(provider)}` : ''}`);
  }

  saveModelSettings(input: SaveModelSettingsInput): Promise<ModelSettings> {
    return this.request(RUNTIME_ROUTES.modelSettings, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(input),
    });
  }

  deleteModelSettings(provider: string, model: string): Promise<ModelSettings> {
    return this.request(RUNTIME_ROUTES.modelSettingsDelete, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ provider, model }),
    });
  }

  connectHostEvents(handler: (event: HostEvent) => Promise<HostEventReceipt>): void {
    if (this.hostEventClient) throw new Error('The Runtime host event connection is already configured.');
    this.hostEventClient = new AuthenticatedHostEventClient(async () => {
      const ready = await this.start();
      return { host: ready.host, port: ready.port, token: this.token };
    }, handler, { onError: (error) => this.reportError(error) });
    this.hostEventClient.start();
  }

  validateNotificationTarget(target: NotificationNavigationTarget): Promise<NotificationTargetValidation> {
    return this.request(RUNTIME_ROUTES.notificationTargetValidation, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(target),
    });
  }

  getAgentRun(runId: string): Promise<AgentRunRecord> {
    if (typeof runId !== 'string' || runId.length < 1 || runId.length > 200) {
      throw new Error('runId must be a non-empty string of at most 200 characters.');
    }
    return this.request(`${RUNTIME_ROUTES.agentRuns}/${encodeURIComponent(runId)}`);
  }

  getPrivateImRunSummary(runId: string): Promise<PrivateImRunSummary> {
    if (typeof runId !== 'string' || runId.length < 1 || runId.length > 200) {
      throw new Error('runId must be a non-empty string of at most 200 characters.');
    }
    return this.request(`${RUNTIME_ROUTES.privateImRuns}/${encodeURIComponent(runId)}`);
  }

  cancelAgentRun(runId: string): Promise<AgentRunCancellationReceipt> {
    if (typeof runId !== 'string' || runId.length < 1 || runId.length > 200) {
      throw new Error('runId must be a non-empty string of at most 200 characters.');
    }
    return this.request(`${RUNTIME_ROUTES.agentRuns}/${encodeURIComponent(runId)}/cancel`, {
      method: 'POST',
    });
  }

  listSchedules(): Promise<ScheduleRecord[]> {
    return this.request(RUNTIME_ROUTES.schedules);
  }

  createSchedule(input: ScheduleInput): Promise<ScheduleRecord> {
    return this.request(RUNTIME_ROUTES.schedules, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(input),
    });
  }

  previewSchedule(input: ScheduleInput): Promise<{ nextTriggerAt?: string }> {
    return this.request(`${RUNTIME_ROUTES.schedules}/preview`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(input),
    });
  }

  updateSchedule(scheduleId: string, input: ScheduleInput): Promise<ScheduleRecord> {
    return this.request(`${RUNTIME_ROUTES.schedules}/${encodeURIComponent(this.validScheduleId(scheduleId))}`, {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(input),
    });
  }

  setScheduleEnabled(scheduleId: string, enabled: boolean): Promise<ScheduleRecord> {
    if (typeof enabled !== 'boolean') throw new Error('enabled must be a boolean.');
    return this.request(
      `${RUNTIME_ROUTES.schedules}/${encodeURIComponent(this.validScheduleId(scheduleId))}/${enabled ? 'enable' : 'disable'}`,
      { method: 'POST' },
    );
  }

  getScheduleHistory(scheduleId: string, limit = 50): Promise<ScheduleHistoryRecord[]> {
    if (!Number.isInteger(limit) || limit < 1 || limit > 200) {
      throw new Error('limit must be an integer from 1 to 200.');
    }
    return this.request(`${RUNTIME_ROUTES.schedules}/${encodeURIComponent(this.validScheduleId(scheduleId))}/history?limit=${limit}`);
  }

  listSchedulePrivateContacts(): Promise<SchedulePrivateContact[]> {
    return this.request(RUNTIME_ROUTES.channelScheduleTargets);
  }

  bindSchedulePrivateContact(contactId: string): Promise<{ routeId: string }> {
    if (typeof contactId !== 'string' || contactId.length < 1 || contactId.length > 200) {
      throw new Error('contactId must be a non-empty string of at most 200 characters.');
    }
    return this.request(RUNTIME_ROUTES.channelScheduleTargets, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ contactId }),
    });
  }

  async revokeSchedulePrivateTarget(routeId: string): Promise<void> {
    if (typeof routeId !== 'string' || routeId.length < 1 || routeId.length > 200) {
      throw new Error('routeId must be a non-empty string of at most 200 characters.');
    }
    await this.request(`${RUNTIME_ROUTES.channelScheduleTargets}/${encodeURIComponent(routeId)}`, {
      method: 'DELETE',
    });
  }

  private validScheduleId(scheduleId: string): string {
    if (typeof scheduleId !== 'string' || scheduleId.length < 1 || scheduleId.length > 200) {
      throw new Error('scheduleId must be a non-empty string of at most 200 characters.');
    }
    return scheduleId;
  }

  listWecomConnections(): Promise<WecomConnectionList> {
    return this.request(RUNTIME_ROUTES.wecomConnections);
  }

  testWecomConnection(connectionId: string): Promise<WecomConnectionSummary> {
    if (typeof connectionId !== 'string' || !/^[A-Za-z0-9._-]{1,128}$/.test(connectionId)) {
      throw new Error('Invalid Enterprise WeChat connectionId.');
    }
    return this.request(`${RUNTIME_ROUTES.wecomConnections}/${encodeURIComponent(connectionId)}/test`, {
      method: 'POST',
    });
  }

  saveWecomConnection(input: WecomConnectionConfigInput): Promise<WecomConnectionSummary> {
    return this.request(RUNTIME_ROUTES.wecomConnections, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(input),
    });
  }

  getTailscaleConnection(): Promise<TailscaleConnectionSettings> {
    return this.request(RUNTIME_ROUTES.tailscaleConnection);
  }

  saveTailscaleConnection(input: SaveTailscaleConnectionInput): Promise<TailscaleConnectionSettings> {
    return this.request(RUNTIME_ROUTES.tailscaleConnection, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(input),
    });
  }

  listCapabilityApprovals(): Promise<CapabilityApprovalSummary[]> {
    return this.request(RUNTIME_ROUTES.capabilityApprovals);
  }

  decideCapabilityApproval(
    requestId: string,
    decision: CapabilityApprovalDecisionInput['decision'],
  ): Promise<CapabilityApprovalDecisionResult> {
    const unsigned = {
      requestId,
      decision,
      issuedAt: Date.now(),
      nonce: randomBytes(16).toString('base64url'),
    };
    const input: CapabilityApprovalDecisionInput = {
      ...unsigned,
      signature: sign(
        null,
        capabilityApprovalSigningPayload(unsigned),
        this.approvalKeyPair.privateKey,
      ).toString('base64url'),
    };
    return this.request(RUNTIME_ROUTES.capabilityApprovalDecision, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(input),
    });
  }

  greeting(name: string): Promise<RuntimeGreeting> {
    return this.request(`${RUNTIME_ROUTES.greeting}?name=${encodeURIComponent(name)}`);
  }

  chat(message: string): Promise<ChatResponse> {
    return this.request(RUNTIME_ROUTES.chat, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ message }),
    });
  }

  submitDesktopMessage(message: string, surface: 'work' | 'assistant' = 'work', conversationId?: string,
    clientMessageId?: string, modelSelection?: import('@yuanpu-agent/protocol').AgentRunRequest['modelSelection'],
    approvalMode?: import('@yuanpu-agent/protocol').AgentRunRequest['approvalMode']): Promise<AgentRunReceipt> {
    if (typeof message !== 'string' || !message.trim()) {
      throw new Error('A non-empty message is required.');
    }
    return this.request(RUNTIME_ROUTES.chatSubmit, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ message, surface, conversationId, clientMessageId, modelSelection, approvalMode }),
    });
  }

  submitDesktopCanvasMessage(message: string,
    widgets: import('@yuanpu-agent/protocol').DesktopCanvasWidgetSummary[]): Promise<AgentRunReceipt> {
    if (typeof message !== 'string' || !message.trim()) throw new Error('A non-empty message is required.');
    return this.request(RUNTIME_ROUTES.desktopCanvasSubmit, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ message, widgets }),
    });
  }

  getDesktopTranscript(surface: DesktopTranscriptSurface, conversationId?: string,
    beforeId?: string, limit?: number): Promise<DesktopTranscriptMessage[]> {
    const query = new URLSearchParams({ surface });
    if (conversationId) query.set('conversationId', conversationId);
    if (beforeId) query.set('before', beforeId);
    if (limit !== undefined) query.set('limit', String(limit));
    return this.request(`${RUNTIME_ROUTES.desktopTranscript}?${query}`);
  }

  listWorkConversations(): Promise<import('@yuanpu-agent/protocol').WorkConversation[]> {
    return this.request(RUNTIME_ROUTES.workConversations);
  }

  createWorkConversation(folderId?: string, requestId?: string): Promise<import('@yuanpu-agent/protocol').WorkConversation> {
    return this.request(RUNTIME_ROUTES.workConversations, {
      method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ folderId, requestId }),
    });
  }

  selectWorkConversation(conversationId: string, previewArchived?: boolean): Promise<import('@yuanpu-agent/protocol').WorkConversation> {
    return this.request(RUNTIME_ROUTES.workConversations, {
      method: 'PUT', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ conversationId, previewArchived }),
    });
  }

  updateWorkConversation(conversationId: string, patch: { title?: string; iconId?: string; archived?: boolean; tagIds?: string[] }) {
    return this.request<import('@yuanpu-agent/protocol').WorkConversation>(RUNTIME_ROUTES.workConversations, {
      method: 'PATCH', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ conversationId, ...patch }),
    });
  }
  moveWorkNode(request: import('@yuanpu-agent/protocol').WorkMoveRequest): Promise<import('@yuanpu-agent/protocol').WorkMoveResult> {
    return this.request(RUNTIME_ROUTES.workMove, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(request) });
  }
  searchWorkConversations(input: import('@yuanpu-agent/protocol').WorkSearchQuery): Promise<import('@yuanpu-agent/protocol').WorkSearchResult> {
    const query = new URLSearchParams({ query: input.query });
    if (input.archive) query.set('archive', input.archive);
    if (input.limit !== undefined) query.set('limit', String(input.limit));
    if (input.cursor) query.set('cursor', input.cursor);
    return this.request(`${RUNTIME_ROUTES.workSearch}?${query}`);
  }
  getWorkMessageWindow(conversationId: string, entryId: string, radius?: number): Promise<import('@yuanpu-agent/protocol').WorkMessageWindowResult> {
    const query = new URLSearchParams({ conversationId, entryId });
    if (radius !== undefined) query.set('radius', String(radius));
    return this.request(`${RUNTIME_ROUTES.workMessageWindow}?${query}`);
  }
  getWorkTrajectory(conversationId: string): Promise<import('@yuanpu-agent/protocol').SessionTrajectory> {
    return this.request(`${RUNTIME_ROUTES.workTrajectory}?conversationId=${encodeURIComponent(conversationId)}`);
  }
  listWorkFolders(): Promise<import('@yuanpu-agent/protocol').WorkFolder[]> { return this.request(RUNTIME_ROUTES.workFolders); }
  createWorkFolder(parentId: string | null, name: string, iconId?: string, requestId?: string): Promise<import('@yuanpu-agent/protocol').WorkFolder> {
    return this.request(RUNTIME_ROUTES.workFolders, { method: 'POST', headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ parentId, name, iconId, requestId }) });
  }
  updateWorkFolder(folderId: string, patch: { name?: string; iconId?: string }): Promise<import('@yuanpu-agent/protocol').WorkFolder> {
    return this.request(RUNTIME_ROUTES.workFolders, { method: 'PATCH', headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ folderId, ...patch }) });
  }
  listWorkTags(): Promise<import('@yuanpu-agent/protocol').WorkTag[]> { return this.request(RUNTIME_ROUTES.workTags); }
  createWorkTag(name: string, color?: string, requestId?: string): Promise<import('@yuanpu-agent/protocol').WorkTag> {
    return this.request(RUNTIME_ROUTES.workTags, { method: 'POST', headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ name, color, requestId }) });
  }
  updateWorkTag(tagId: string, patch: { name?: string; color?: string }): Promise<import('@yuanpu-agent/protocol').WorkTag> {
    return this.request(RUNTIME_ROUTES.workTags, { method: 'PATCH', headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ tagId, ...patch }) });
  }
  reorderWorkSiblings(kind: 'folder' | 'conversation', parentId: string | null, ids: string[]): Promise<void> {
    return this.request(RUNTIME_ROUTES.workOrder, { method: 'PUT', headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ kind, parentId, ids }) });
  }

  listWorkFiles(conversationId: string, dirPath?: string, options?: { recursive?: boolean }): Promise<import('@yuanpu-agent/protocol').WorkDirectoryListing> {
    const query = new URLSearchParams({ conversationId });
    if (dirPath) query.set('path', dirPath);
    if (options?.recursive) query.set('recursive', '1');
    return this.request(`${RUNTIME_ROUTES.workFiles}?${query}`);
  }

  readWorkFile(conversationId: string, filePath: string): Promise<import('@yuanpu-agent/protocol').WorkFilePreview> {
    const query = new URLSearchParams({ conversationId, path: filePath });
    return this.request(`${RUNTIME_ROUTES.workFileContent}?${query}`);
  }

  resolveWorkFilePath(conversationId: string, filePath: string): Promise<{ path: string }> {
    const query = new URLSearchParams({ conversationId, path: filePath });
    return this.request(`${RUNTIME_ROUTES.workFilePath}?${query}`);
  }

  listWorkFileChanges(query: { conversationId: string; runId?: string }): Promise<import('@yuanpu-agent/protocol').WorkFileChangesSummary> {
    const params = new URLSearchParams({ conversationId: query.conversationId });
    if (query.runId) params.set('runId', query.runId);
    return this.request(`${RUNTIME_ROUTES.workFileChanges}?${params}`);
  }

  getAssistantLink(): Promise<AssistantLinkStatus> {
    return this.request(RUNTIME_ROUTES.assistantLink);
  }

  bindAssistantContact(contactId: string): Promise<AssistantLinkStatus> {
    return this.request(RUNTIME_ROUTES.assistantLink, {
      method: 'PUT', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ contactId }),
    });
  }

  unbindAssistantContact(): Promise<AssistantLinkStatus> {
    return this.request(RUNTIME_ROUTES.assistantLink, { method: 'DELETE' });
  }

  listAssistantMirrors(runId: string): Promise<AssistantMirrorStatus[]> {
    return this.request(`${RUNTIME_ROUTES.assistantMirrors}?runId=${encodeURIComponent(runId)}`);
  }

  retryAssistantMirror(mirrorId: string): Promise<AssistantMirrorStatus> {
    return this.request(`${RUNTIME_ROUTES.assistantMirrors}/${encodeURIComponent(mirrorId)}/retry`, { method: 'POST' });
  }

  listAssistantSuggestions(): Promise<import('@yuanpu-agent/protocol').AssistantSuggestionInbox> {
    return this.request(RUNTIME_ROUTES.assistantSuggestions);
  }

  feedbackAssistantSuggestion(id: string, action: 'ignored' | 'snoozed' | 'accepted',
    snoozedUntil?: string): Promise<import('@yuanpu-agent/protocol').AssistantSuggestion> {
    return this.request(RUNTIME_ROUTES.assistantSuggestions, { method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ action: 'feedback', id, feedback: action, snoozedUntil }) });
  }

  setAssistantSuggestionsPaused(until?: string): Promise<{ pausedUntil?: string }> {
    return this.request(RUNTIME_ROUTES.assistantSuggestions, { method: 'POST',
      headers: { 'content-type': 'application/json' }, body: JSON.stringify({ action: 'pause', until }) });
  }

  markAssistantSuggestionRead(id: string): Promise<import('@yuanpu-agent/protocol').AssistantSuggestion> {
    return this.request(RUNTIME_ROUTES.assistantSuggestions, { method: 'POST',
      headers: { 'content-type': 'application/json' }, body: JSON.stringify({ action: 'read', id }) });
  }

  getAssistantWorkspace(memoryLimit?: number): Promise<import('@yuanpu-agent/protocol').AssistantWorkspaceSnapshot> {
    return this.request(memoryLimit === undefined ? RUNTIME_ROUTES.assistantWorkspace
      : `${RUNTIME_ROUTES.assistantWorkspace}?memoryLimit=${encodeURIComponent(memoryLimit)}`);
  }

  revokeAssistantSource(sourceId: string, expectedVersion: string): Promise<import('@yuanpu-agent/protocol').AssistantSourceRevocationReceipt> {
    return this.request(RUNTIME_ROUTES.assistantWorkspace, { method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ action: 'revoke-source', sourceId, expectedVersion }) });
  }

  correctAssistantMemory(id: string, expectedVersion: number, text: string,
    revisionId: string): Promise<import('@yuanpu-agent/protocol').AssistantMemoryView> {
    return this.request(RUNTIME_ROUTES.assistantWorkspace, { method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ action: 'correct-memory', id, expectedVersion, text, revisionId }) });
  }

  forgetAssistantMemory(id: string): Promise<{ forgottenIds: string[] }> {
    return this.request(RUNTIME_ROUTES.assistantWorkspace, { method: 'POST',
      headers: { 'content-type': 'application/json' }, body: JSON.stringify({ action: 'forget-memory', id }) });
  }

  importAssistantSavedMemory(savedId: string, surface: 'work' | 'assistant', text: string,
    savedAt: string): Promise<import('@yuanpu-agent/protocol').AssistantMemoryView> {
    return this.request(RUNTIME_ROUTES.assistantWorkspace, { method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ action: 'import-saved', savedId, surface, text, savedAt }) });
  }

  setAssistantOrganizingPaused(until?: string): Promise<{ organizingPausedUntil?: string }> {
    return this.request(RUNTIME_ROUTES.assistantWorkspace, { method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ action: 'pause-organizing', until }) });
  }

  followUpAssistantDelegation(taskId: string, text: string): Promise<import('@yuanpu-agent/protocol').AssistantDelegationRecord> {
    return this.request(RUNTIME_ROUTES.assistantWorkspace, { method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ action: 'follow-up-delegation', id: taskId, text }) });
  }

  cancelAssistantDelegation(taskId: string): Promise<import('@yuanpu-agent/protocol').AssistantDelegationRecord> {
    return this.request(RUNTIME_ROUTES.assistantWorkspace, { method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ action: 'cancel-delegation', id: taskId }) });
  }

  searchPlugins(query: string): Promise<PluginSearchResult[]> {
    return this.request(`${RUNTIME_ROUTES.pluginSearch}?q=${encodeURIComponent(query)}`);
  }

  listPlugins(): Promise<InstalledPlugin[]> {
    return this.request(RUNTIME_ROUTES.plugins);
  }

  listLocalSkills(): Promise<LocalSkillList> {
    return this.request(RUNTIME_ROUTES.localSkills);
  }

  installPlugin(source: string, artifactManifestDigest?: string): Promise<InstalledPlugin> {
    return this.request(RUNTIME_ROUTES.pluginInstall, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ source, artifactManifestDigest }),
    });
  }

  setPluginEnabled(name: string, enabled: boolean): Promise<InstalledPlugin> {
    return this.request(RUNTIME_ROUTES.pluginState, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ name, enabled }),
    });
  }

  async uninstallPlugin(name: string): Promise<void> {
    await this.request(RUNTIME_ROUTES.pluginUninstall, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ name }),
    });
  }

  getPluginConfig(name: string, scope: PluginConfigScope): Promise<PluginConfigDocument> {
    return this.request(
      `${RUNTIME_ROUTES.pluginConfig}?name=${encodeURIComponent(name)}&scope=${encodeURIComponent(scope)}`,
    );
  }

  validatePluginConfig(input: PluginConfigInput): Promise<PluginConfigValidation> {
    return this.request(RUNTIME_ROUTES.pluginConfigValidate, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(input),
    });
  }

  savePluginConfig(input: PluginConfigInput): Promise<PluginConfigDocument> {
    return this.request(RUNTIME_ROUTES.pluginConfigSave, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(input),
    });
  }

  resetPluginConfig(name: string, scope: PluginConfigScope): Promise<PluginConfigDocument> {
    return this.request(RUNTIME_ROUTES.pluginConfigReset, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ name, scope }),
    });
  }

  rollbackPlugin(name: string, version: string): Promise<InstalledPlugin> {
    return this.request(RUNTIME_ROUTES.pluginRollback, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ name, version }),
    });
  }

  listMcpOwnershipConflicts(
    source: string,
    artifactManifestDigest: string,
  ): Promise<McpOwnershipConflict[]> {
    return this.request(
      `${RUNTIME_ROUTES.pluginMcpConflicts}?source=${encodeURIComponent(source)}&manifestDigest=${encodeURIComponent(artifactManifestDigest)}`,
    );
  }

  async checkForUpdate(): Promise<RuntimeUpdateState> {
    const current = await this.info();
    return this.updater.stage(
      current.version,
      process.env.YUANPU_RUNTIME_MANIFEST_URL || DEFAULT_MANIFEST_URL,
    );
  }

  async stop(): Promise<void> {
    this.shouldRun = false;
    await this.hostEventClient?.stop();
    this.hostEventClient = undefined;
    if (this.restartTimer) clearTimeout(this.restartTimer);
    this.restartTimer = undefined;
    if (this.activationConfirmationTimer) clearTimeout(this.activationConfirmationTimer);
    this.activationConfirmationTimer = undefined;
    if (this.stopPromise) return this.stopPromise;
    this.stopPromise = (async () => {
      const initialChild = this.child;
      if (initialChild) await this.terminate(initialChild);
      await this.startPromise?.catch(() => undefined);
      const lateChild = this.child;
      if (lateChild && lateChild !== initialChild) await this.terminate(lateChild);
      this.child = undefined;
      this.ready = undefined;
    })().finally(() => {
      this.stopPromise = undefined;
    });
    return this.stopPromise;
  }
}

import { createHash, randomUUID } from 'node:crypto';
import { lstat, mkdir, readFile, rename, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import type { AssistantTool } from './tools.js';
import type { AssistantDelegationBrief, AssistantDelegationRecord } from '@yuanpu-agent/protocol';
import { Type, type Static } from 'typebox';

export interface AssistantDelegationHost {
  start(brief: AssistantDelegationBrief): Promise<AssistantDelegationRecord>;
  status(taskId: string): Promise<AssistantDelegationRecord | undefined>;
  followUp(taskId: string, assistantSessionId: string, text: string,
    requestId: string): Promise<AssistantDelegationRecord>;
  cancel(taskId: string, assistantSessionId: string): Promise<AssistantDelegationRecord>;
}

interface Archive {
  taskId: string;
  assistantSessionId: string;
  brief: AssistantDelegationBrief;
  record?: AssistantDelegationRecord;
  verification?: { checkedAt: string; evidenceByCriterion: Record<string, string[]> };
}

const validId = (id: string) => /^[A-Za-z0-9_-]{1,128}$/.test(id);
const result = (value: unknown) => ({ content: [{ type: 'text' as const, text: JSON.stringify(value) }], details: value });

/** The sole writer of readable delegation archives under Assistant Home. */
export class AssistantDelegationCoordinator {
  private readonly directory: string;

  private readonly host: AssistantDelegationHost;

  constructor(assistantHome: string, host: AssistantDelegationHost) {
    this.directory = join(assistantHome, 'delegations');
    this.host = host;
  }

  private file(taskId: string): string {
    if (!validId(taskId)) throw new Error('Invalid delegation task ID.');
    return join(this.directory, `${taskId}.json`);
  }

  private async read(taskId: string): Promise<Archive | undefined> {
    try {
      const file = this.file(taskId);
      const info = await lstat(file);
      if (!info.isFile() || info.isSymbolicLink()) throw new Error('Unsafe delegation archive entry.');
      const archive = JSON.parse(await readFile(file, 'utf8')) as Archive;
      if (archive.taskId !== taskId) throw new Error('Delegation archive ID mismatch.');
      return archive;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === 'ENOENT') return undefined;
      throw error;
    }
  }

  private async save(archive: Archive): Promise<void> {
    await mkdir(this.directory, { recursive: true, mode: 0o700 });
    const temporary = `${this.file(archive.taskId)}.${randomUUID()}.tmp`;
    try {
      await writeFile(temporary, JSON.stringify(archive), { flag: 'wx', mode: 0o600 });
      await rename(temporary, this.file(archive.taskId));
    } finally { await rm(temporary, { force: true }); }
  }

  private async owned(taskId: string, assistantSessionId: string): Promise<Archive> {
    const archive = await this.read(taskId);
    if (!archive || archive.assistantSessionId !== assistantSessionId) {
      throw new Error('Unknown delegation in this Assistant Session.');
    }
    return archive;
  }

  async start(brief: AssistantDelegationBrief): Promise<AssistantDelegationRecord> {
    const previous = await this.read(brief.taskId);
    if (previous && previous.assistantSessionId !== brief.assistantSessionId) throw new Error('Delegation task ID conflict.');
    if (!previous) await this.save({ taskId: brief.taskId, assistantSessionId: brief.assistantSessionId, brief });
    const acceptedBrief = previous?.brief ?? brief;
    try {
      const record = await this.host.start(acceptedBrief);
      await this.save({ taskId: brief.taskId, assistantSessionId: brief.assistantSessionId, brief: acceptedBrief, record });
      return record;
    } catch (error) {
      // The host may have accepted the task before IPC failed. Never assign a new task ID here.
      const recovered = await this.host.status(brief.taskId).catch(() => undefined);
      if (recovered) {
        await this.save({ taskId: brief.taskId, assistantSessionId: brief.assistantSessionId, brief: acceptedBrief, record: recovered });
        return recovered;
      }
      throw error;
    }
  }

  async status(taskId: string, assistantSessionId: string): Promise<AssistantDelegationRecord | undefined> {
    const archive = await this.owned(taskId, assistantSessionId);
    const record = await this.host.status(taskId);
    if (record) await this.save({ ...archive, record });
    return record;
  }

  async followUp(taskId: string, assistantSessionId: string, text: string,
    requestId: string): Promise<AssistantDelegationRecord> {
    const archive = await this.owned(taskId, assistantSessionId);
    const record = await this.host.followUp(taskId, assistantSessionId, text, requestId);
    await this.save({ ...archive, record, verification: undefined });
    return record;
  }

  async cancel(taskId: string, assistantSessionId: string): Promise<AssistantDelegationRecord> {
    const archive = await this.owned(taskId, assistantSessionId);
    const record = await this.host.cancel(taskId, assistantSessionId);
    await this.save({ ...archive, record });
    return record;
  }

  async linkEvidence(taskId: string, assistantSessionId: string,
    checks: Array<{ criterion: string; evidenceRefs: string[] }>): Promise<{ status: 'evidence_linked'; taskId: string }> {
    const archive = await this.owned(taskId, assistantSessionId);
    const current = await this.status(taskId, assistantSessionId);
    if (current?.status !== 'completed' || !current.result?.resultRef) {
      throw new Error('A completed result reference is required before evidence review.');
    }
    const allowed = new Set(current.result.evidenceRefs ?? []);
    const expected = new Set(archive.brief.completionCriteria);
    if (checks.length !== expected.size || new Set(checks.map((check) => check.criterion)).size !== expected.size
      || checks.some((check) => !expected.has(check.criterion)
      || check.evidenceRefs.length === 0 || check.evidenceRefs.some((ref) => !allowed.has(ref)))) {
      throw new Error('Each completion criterion needs an actual returned evidence reference.');
    }
    const evidenceByCriterion = Object.fromEntries(checks.map((check) => [check.criterion, check.evidenceRefs]));
    await this.save({ ...archive, record: current,
      verification: { checkedAt: new Date().toISOString(), evidenceByCriterion } });
    // Linking references is a necessary check, not a semantic or user approval verdict.
    return { status: 'evidence_linked', taskId };
  }
}

const parameters = Type.Object({
  action: Type.Union(['start', 'status', 'follow_up', 'cancel', 'link_evidence'].map((name) => Type.Literal(name))),
  taskId: Type.Optional(Type.String()),
  skillName: Type.Optional(Type.String()),
  goal: Type.Optional(Type.String()),
  completionCriteria: Type.Optional(Type.Array(Type.String())),
  contextRefs: Type.Optional(Type.Array(Type.String())),
  authorizedCapabilities: Type.Optional(Type.Array(Type.String())),
  readOnly: Type.Optional(Type.Boolean()),
  deadlineSeconds: Type.Optional(Type.Integer({ minimum: 1, maximum: 1800 })),
  text: Type.Optional(Type.String()),
  checks: Type.Optional(Type.Array(Type.Object({ criterion: Type.String(), evidenceRefs: Type.Array(Type.String()) }))),
}, { additionalProperties: false });

/** One tool per Assistant Session; task IDs derive from durable tool-call IDs for replay safety. */
export function createAssistantDelegationTool(coordinator: AssistantDelegationCoordinator,
  assistantSessionId: string,
  automationTaskId?: () => string | undefined): AssistantTool<typeof parameters> {
  return {
    name: 'delegate_and_verify', label: 'Delegate and verify',
    description: 'Delegate a bounded professional task to an isolated executor. Start requires a skill, goal, completion criteria, scoped references and authorization. Query or follow up using the same task ID. Completed execution is not proof that the work is verified.',
    parameters, replay: 'never',
    async execute(toolCallId: string, input: Static<typeof parameters>) {
      const scopedTaskId = automationTaskId?.();
      if (scopedTaskId && (input.action !== 'status' || input.taskId !== scopedTaskId)) {
        throw new Error('Automation may only inspect its original delegated task before budget approval.');
      }
      if (input.action === 'start') {
        if (!input.skillName || !input.goal || !input.completionCriteria?.length) throw new Error('Incomplete delegation brief.');
        const taskId = createHash('sha256').update(`${assistantSessionId}:${toolCallId}`).digest('hex');
        const brief: AssistantDelegationBrief = {
          taskId, assistantSessionId, skillName: input.skillName, goal: input.goal,
          completionCriteria: input.completionCriteria, contextRefs: input.contextRefs ?? [],
          authorizedCapabilities: input.authorizedCapabilities ?? [], readOnly: input.readOnly ?? true,
          deadlineAt: new Date(Date.now() + (input.deadlineSeconds ?? 300) * 1000).toISOString(),
        };
        return result(await coordinator.start(brief));
      }
      if (!input.taskId) throw new Error('taskId is required.');
      if (input.action === 'status') return result(await coordinator.status(input.taskId, assistantSessionId));
      if (input.action === 'cancel') return result(await coordinator.cancel(input.taskId, assistantSessionId));
      if (input.action === 'follow_up') {
        if (!input.text) throw new Error('Follow-up text is required.');
        const requestId = createHash('sha256').update(`${assistantSessionId}:${toolCallId}`).digest('hex');
        return result(await coordinator.followUp(input.taskId, assistantSessionId, input.text, requestId));
      }
      if (!input.checks) throw new Error('Evidence checks are required.');
      return result(await coordinator.linkEvidence(input.taskId, assistantSessionId, input.checks));
    },
  };
}

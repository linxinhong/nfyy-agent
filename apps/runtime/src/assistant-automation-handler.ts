import { createHash } from 'node:crypto';
import { parseWorkReviewProposal, parseUnderstandingProposal, parseSuggestionProposal,
  suggestionCandidateKeys, followUpCheckKey, isDirectUserStatement,
  type WorkReviewProposal, type AssistantUserUnderstanding,
  type UnderstandingSnapshot, type UnderstandingProposal, type SuggestionCandidate,
  type SuggestionProposal, type AssistantSuggestionStore } from '@yuanpu-agent/assistant';
import type { AssistantMemoryRepository, AssistantAutomationStore, AutomationHandler,
  AutomationJob, AutomationProposal, AssistantWorkReviewStore, WorkReviewSnapshot,
  AssistantWorkOrganization } from '@yuanpu-agent/assistant';
import type { AssistantDelegationRecord } from '@yuanpu-agent/protocol';

export interface DelegationAutomationHost {
  current(taskId: string): Promise<AssistantDelegationRecord | undefined>;
  notify(record: AssistantDelegationRecord, signal: AbortSignal,
    beforeModel: () => boolean): Promise<{ costUsd: number; message: string }>;
  linkEvidence(record: AssistantDelegationRecord,
    checks: Array<{ criterion: string; evidenceRefs: string[] }>): Promise<void>;
}

export interface WorkReviewAutomationHost {
  store: AssistantWorkReviewStore;
  organization?: AssistantWorkOrganization;
  review(snapshot: WorkReviewSnapshot, signal: AbortSignal,
    beforeModel: () => boolean): Promise<{ costUsd: number; message: string }>;
}

export interface UnderstandingAutomationHost {
  store: AssistantUserUnderstanding;
  understand(snapshot: UnderstandingSnapshot, signal: AbortSignal,
    beforeModel: () => boolean): Promise<{ costUsd: number; message: string }>;
}

export interface SuggestionAutomationHost {
  store: AssistantSuggestionStore;
  reflect(candidates: SuggestionCandidate[], signal: AbortSignal,
    beforeModel: () => boolean): Promise<{ costUsd: number; message: string }>;
}

function reviewFingerprint(snapshot: WorkReviewSnapshot): string {
  return createHash('sha256').update(JSON.stringify({ workId: snapshot.workId,
    materials: snapshot.materials.map((item) => [item.sourceId, item.sourceVersion]),
    unavailable: snapshot.unavailable, truncated: snapshot.truncated })).digest('hex');
}

function unverifiedReview(reason: string): WorkReviewProposal {
  return { goal: 'Work goal requires verification', constraints: [], judgment: 'unverified',
    findings: [], unresolved: [reason],
    followUp: ['Review again when new evidence arrives.'], memoryCandidates: [],
    ledgerCandidates: [] };
}

function parseEvidenceProposal(message: string): Array<{ criterion: string; evidenceRefs: string[] }> {
  if (message.length > 16_000) throw new Error('Delegation verification proposal exceeds budget.');
  const parsed = JSON.parse(message) as { checks?: unknown };
  if (!Array.isArray(parsed.checks) || parsed.checks.length > 12
    || parsed.checks.some((item) => !item || typeof item !== 'object'
      || typeof item.criterion !== 'string' || !Array.isArray(item.evidenceRefs)
      || item.evidenceRefs.some((ref: unknown) => typeof ref !== 'string'))) {
    throw new Error('Invalid delegation verification proposal.');
  }
  return parsed.checks;
}

/** Worker-owned periodic check and one-shot, crash-conservative delegation wake. */
export function assistantAutomationHandler(memory: AssistantMemoryRepository,
  store: AssistantAutomationStore, delegations?: DelegationAutomationHost,
  reviews?: WorkReviewAutomationHost,
  understanding?: UnderstandingAutomationHost,
  suggestions?: SuggestionAutomationHost): AutomationHandler {
  return {
    async lookup(job) {
      if (job.kind === 'review-work') {
        if (store.hasCheckpoint(job.effectId)) return 'applied';
        return reviews?.store.snapshot(job) ? 'absent' : 'deferred';
      }
      if (job.kind === 'understand-user') {
        if (store.hasCheckpoint(job.effectId)) return 'applied';
        return understanding?.store.snapshot(job) ? 'absent' : 'deferred';
      }
      if (job.kind === 'maintain-memory') {
        return store.hasCheckpoint(job.effectId) ? 'applied' : 'absent';
      }
      if (job.kind === 'verify-delegation') {
        if (store.hasCheckpoint(job.effectId)) return 'applied';
        if (store.preparedProposal(job)) return 'absent';
        if (store.hasEffectAttempt(job.effectId)) return 'unknown';
        if (!delegations || !job.delegationId) return 'deferred';
        const record = await delegations.current(job.delegationId);
        return record && ['completed', 'failed', 'cancelled', 'unknown', 'waiting_approval']
          .includes(record.status) ? 'absent' : 'deferred';
      }
      if (job.kind !== 'follow-up-check' && job.kind !== 'daily-check'
        && job.kind !== 'weekly-check') return 'deferred';
      return store.hasCheckpoint(job.effectId) ? 'applied' : 'absent';
    },
    async prepare(job: AutomationJob, signal: AbortSignal): Promise<AutomationProposal> {
      if (signal.aborted) throw signal.reason;
      if (job.kind === 'understand-user') {
        if (!understanding) throw new Error('User understanding host is unavailable.');
        const snapshot = understanding.store.snapshot(job);
        if (!snapshot) throw new Error('User understanding source is unavailable.');
        const fingerprint = createHash('sha256').update(JSON.stringify(snapshot)).digest('hex');
        const prepared = store.preparedProposal(job);
        if (prepared) {
          if ((prepared.value as { fingerprint?: string }).fingerprint === fingerprint) return prepared;
          return { costUsd: 0, value: { fingerprint, parsed: { observations: [] } } };
        }
        if (store.hasEffectAttempt(job.effectId)) {
          return { costUsd: 0, value: { fingerprint, parsed: { observations: [] } } };
        }
        signal.throwIfAborted();
        const billed = await understanding.understand(snapshot, signal,
          () => store.beginEffectAttempt(job));
        signal.throwIfAborted();
        let parsed: UnderstandingProposal;
        try {
          parsed = parseUnderstandingProposal(billed.message);
          parsed = { observations: parsed.observations.filter((item) =>
            isDirectUserStatement(snapshot.userText, item.quote, item.topic)) };
        }
        catch { parsed = { observations: [] }; }
        const proposal = { costUsd: billed.costUsd, value: { fingerprint, parsed } };
        store.savePreparedProposal(job, proposal);
        return proposal;
      }
      if (job.kind === 'maintain-memory') {
        return { costUsd: 0, value: { sourceId: job.sourceId,
          sourceVersion: job.sourceVersion } };
      }
      if (job.kind === 'review-work') {
        if (!reviews) throw new Error('Work review host is unavailable.');
        const snapshot = reviews.store.snapshot(job);
        if (!snapshot) throw new Error('Work review source is unavailable.');
        const prepared = store.preparedProposal(job);
        if (prepared) {
          if ((prepared.value as { fingerprint?: string }).fingerprint === reviewFingerprint(snapshot)) {
            return prepared;
          }
          return { costUsd: 0, value: { fingerprint: reviewFingerprint(snapshot),
            parsed: unverifiedReview('Source material changed after the previous model review.') } };
        }
        if (store.hasEffectAttempt(job.effectId)) {
          // A lost read-only model result must become visible as unknown, without another billed turn.
          return { costUsd: 0, value: { fingerprint: reviewFingerprint(snapshot),
            parsed: unverifiedReview('The previous review model result was interrupted or lost.') } };
        }
        signal.throwIfAborted();
        const billed = await reviews.review(snapshot, signal, () => store.beginEffectAttempt(job));
        signal.throwIfAborted();
        let parsed: WorkReviewProposal;
        try {
          parsed = parseWorkReviewProposal(billed.message);
          const known = new Set(snapshot.materials.map((material) => material.sourceId));
          if (parsed.findings.some((finding) => finding.evidenceRefs.some((ref) => !known.has(ref)))) {
            throw new Error('Review cited unknown evidence.');
          }
        } catch {
          parsed = unverifiedReview('The model review was invalid or cited unknown evidence.');
        }
        const fingerprint = reviewFingerprint(snapshot);
        const proposal = { costUsd: billed.costUsd, value: { parsed, fingerprint } };
        store.savePreparedProposal(job, proposal);
        return proposal;
      }
      if (job.kind === 'verify-delegation') {
        const prepared = store.preparedProposal(job);
        if (prepared) return prepared;
        if (!delegations || !job.delegationId) throw new Error('Delegation automation host is unavailable.');
        const record = await delegations.current(job.delegationId);
        if (!record || !['completed', 'failed', 'cancelled', 'unknown', 'waiting_approval']
          .includes(record.status)) throw new Error('Delegation is not ready for verification.');
        signal.throwIfAborted();
        const billed = await delegations.notify(record, signal, () => store.beginEffectAttempt(job));
        const checks = parseEvidenceProposal(billed.message);
        const proposal = { costUsd: billed.costUsd, value: { taskId: record.taskId,
          status: record.status, updatedAt: record.updatedAt, checks } };
        store.savePreparedProposal(job, proposal);
        return proposal;
      }
      if (job.kind === 'follow-up-check' || job.kind === 'daily-check'
        || job.kind === 'weekly-check') {
        const candidates = await suggestions?.store.candidates() ?? [];
        if (job.kind === 'follow-up-check'
          && job.dedupeKey !== followUpCheckKey(suggestionCandidateKeys(candidates))) {
          return { costUsd: 0, value: { candidates: [], parsed: [], skipped: 'source_changed' } };
        }
        if (!candidates.length) return { costUsd: 0, value: { candidates, parsed: [] } };
        const prepared = store.preparedProposal(job);
        if (prepared) {
          const previous = (prepared.value as { candidateKeys?: unknown;
            parsed?: SuggestionProposal[] }).candidateKeys;
          return JSON.stringify(previous) === JSON.stringify(suggestionCandidateKeys(candidates))
            ? { costUsd: prepared.costUsd, value: { candidates,
              parsed: (prepared.value as { parsed: SuggestionProposal[] }).parsed } }
            : { costUsd: 0, value: { candidates: [], parsed: [], skipped: 'source_changed' } };
        }
        if (store.hasEffectAttempt(job.effectId)) {
          const attempted = suggestions!.store.reflectionAttempt(job.effectId);
          return JSON.stringify(attempted) === JSON.stringify(suggestionCandidateKeys(candidates))
            ? { costUsd: 0, value: { candidates, parsed: [] } }
            : { costUsd: 0, value: { candidates: [], parsed: [], skipped: 'source_changed' } };
        }
        const billed = await suggestions!.reflect(candidates, signal, () => {
          store.database.exec('BEGIN IMMEDIATE');
          try {
            const started = store.beginEffectAttempt(job);
            if (started) suggestions!.store.recordReflectionAttempt(job.effectId, candidates);
            store.database.exec('COMMIT');
            return started;
          } catch (error) { store.database.exec('ROLLBACK'); throw error; }
        });
        signal.throwIfAborted();
        let parsed: SuggestionProposal[];
        try { parsed = parseSuggestionProposal(billed.message, candidates); }
        catch { parsed = []; }
        const proposal = { costUsd: billed.costUsd, value: { candidates, parsed } };
        store.savePreparedProposal(job, { costUsd: billed.costUsd,
          value: { candidateKeys: suggestionCandidateKeys(candidates), parsed } });
        return proposal;
      }
      const database = memory.sources.database;
      const sources = database.prepare(`SELECT COUNT(*) AS total,
        SUM(CASE WHEN availability='available' THEN 1 ELSE 0 END) AS available
        FROM source_current`).get() as { total: number; available: number | null };
      const memories = database.prepare(`SELECT COUNT(*) AS total FROM memory_documents
        WHERE status='active'`).get() as { total: number };
      return { costUsd: 0, value: { kind: job.kind, sourceCount: sources.total,
        availableSourceCount: sources.available ?? 0, activeMemoryCount: memories.total } };
    },
    async apply(job, proposal, commit, signal) {
      if (job.kind === 'understand-user') {
        if (!understanding) throw new Error('User understanding host is unavailable.');
        signal.throwIfAborted();
        const snapshot = understanding.store.snapshot(job);
        const value = proposal.value as { fingerprint: string; parsed: UnderstandingProposal };
        if (!snapshot || createHash('sha256').update(JSON.stringify(snapshot)).digest('hex')
          !== value.fingerprint) throw new Error('Understanding source changed before commit.');
        understanding.store.record(job, snapshot, value.parsed, commit,
          (record) => store.recordCheckpoint(job, record));
        await understanding.store.reconcile(job.audience);
        return;
      }
      if (job.kind === 'maintain-memory') {
        signal.throwIfAborted();
        await understanding?.store.reconcile(job.audience);
        await reviews?.organization?.reconcile();
        commit(() => store.recordCheckpoint(job, proposal.value));
        return;
      }
      if (job.kind === 'review-work') {
        if (!reviews) throw new Error('Work review host is unavailable.');
        signal.throwIfAborted();
        const snapshot = reviews.store.snapshot(job);
        if (!snapshot || reviewFingerprint(snapshot) !== (proposal.value as { fingerprint?: string }).fingerprint) {
          throw new Error('Work review materials changed before commit.');
        }
        const parsed = (proposal.value as { parsed: WorkReviewProposal }).parsed;
        const committed = reviews.store.record(job, snapshot, parsed, commit,
          (value) => store.recordCheckpoint(job, value));
        if (committed) {
          await reviews.store.flushPending();
          await reviews.organization?.reconcile();
        }
        return;
      }
      if (job.kind === 'verify-delegation') {
        if (!delegations || !job.delegationId) throw new Error('Delegation automation host is unavailable.');
        const value = proposal.value as { taskId: string; status: string; updatedAt: string;
          checks: Array<{ criterion: string; evidenceRefs: string[] }> };
        signal.throwIfAborted();
        const current = await delegations.current(job.delegationId);
        signal.throwIfAborted();
        if (!current || current.taskId !== value.taskId || current.updatedAt !== value.updatedAt
          || current.status !== value.status || store.get(job.jobId)?.status !== 'running') {
          throw new Error('Delegation changed before verification commit.');
        }
        if (current.status === 'completed') {
          await delegations.linkEvidence(current, value.checks);
        } else if (value.checks.length) {
          throw new Error('Noncompleted delegation proposed evidence linkage.');
        }
        commit(() => store.recordCheckpoint(job, { taskId: current.taskId,
          status: current.status, updatedAt: current.updatedAt,
          evidenceLinked: current.status === 'completed' }));
        return;
      }
      if (job.kind === 'follow-up-check' || job.kind === 'daily-check'
        || job.kind === 'weekly-check') {
        await understanding?.store.reconcile(job.audience);
        await reviews?.organization?.reconcile();
        if (suggestions) {
          const value = proposal.value as { candidates?: SuggestionCandidate[];
            parsed?: SuggestionProposal[]; skipped?: string };
          if (value.skipped) {
            commit(() => store.recordCheckpoint(job, { skipped: value.skipped }));
            return;
          }
          const current = await suggestions.store.candidates();
          const expected = value.candidates ?? [];
          if (JSON.stringify(current) !== JSON.stringify(expected)) {
            throw new Error('Suggestion evidence changed before commit.');
          }
          commit(() => {
            suggestions.store.record(expected, value.parsed ?? []);
            store.recordCheckpoint(job, { considered: expected.map((item) => item.candidateId) });
          });
          return;
        }
      }
      commit(() => store.recordCheckpoint(job, proposal.value));
    },
  };
}

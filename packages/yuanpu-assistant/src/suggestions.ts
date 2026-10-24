import { createHash } from 'node:crypto';
import type { DatabaseSync } from 'node:sqlite';
import type { AssistantEvidenceRef, AssistantSuggestion } from '@yuanpu-agent/protocol';
export type { AssistantSuggestion } from '@yuanpu-agent/protocol';
import type { AssistantMemoryRepository } from './memory-documents.js';
import { redactReviewText } from './work-review.js';

export interface SuggestionCandidate {
  candidateId: string;
  fingerprint: string;
  context: string;
  text: string;
  evidence: AssistantEvidenceRef[];
}

export type SuggestionCandidateKey = Pick<SuggestionCandidate, 'candidateId' | 'fingerprint'>;

export function suggestionCandidateKeys(candidates: SuggestionCandidate[]): SuggestionCandidateKey[] {
  return candidates.map(({ candidateId, fingerprint }) => ({ candidateId, fingerprint }));
}

export interface SuggestionProposal {
  candidateId: string;
  reason: string;
  nextStep: string;
}

const schema = `
  CREATE TABLE IF NOT EXISTS assistant_suggestions (
    suggestion_id TEXT PRIMARY KEY, candidate_id TEXT NOT NULL, fingerprint TEXT NOT NULL,
    reason TEXT NOT NULL, next_step TEXT NOT NULL, evidence_json TEXT NOT NULL,
    generated_at TEXT NOT NULL,
    feedback TEXT NOT NULL DEFAULT 'none' CHECK(feedback IN ('none','ignored','snoozed','accepted')),
    snoozed_until TEXT,
    delivery_status TEXT NOT NULL DEFAULT 'not_requested'
      CHECK(delivery_status IN ('not_requested','accepted','failed','unknown')),
    delivery_ref TEXT, delivery_attempt_at TEXT, read_at TEXT,
    remind_at TEXT, delivery_generation INTEGER NOT NULL DEFAULT 0,
    UNIQUE(candidate_id,fingerprint)
  ) STRICT;
  CREATE TABLE IF NOT EXISTS assistant_suggestion_delivery_attempts (
    delivery_id TEXT PRIMARY KEY, suggestion_id TEXT NOT NULL,
    attempted_at TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('pending','accepted','failed','unknown'))
  ) STRICT;
  CREATE TABLE IF NOT EXISTS assistant_suggestion_considered (
    candidate_id TEXT NOT NULL, fingerprint TEXT NOT NULL, checked_at TEXT NOT NULL,
    PRIMARY KEY(candidate_id,fingerprint)
  ) STRICT;
  CREATE TABLE IF NOT EXISTS assistant_suggestion_reflections (
    effect_id TEXT PRIMARY KEY, candidates_json TEXT NOT NULL
  ) STRICT;
  CREATE TABLE IF NOT EXISTS assistant_suggestion_settings (
    id INTEGER PRIMARY KEY CHECK(id=1), paused_until TEXT
  ) STRICT;
  INSERT OR IGNORE INTO assistant_suggestion_settings(id) VALUES (1);
`;

function fingerprint(value: unknown): string {
  return createHash('sha256').update(JSON.stringify(value)).digest('hex');
}

function rowSuggestion(row: Record<string, unknown>): AssistantSuggestion {
  return { suggestionId: String(row.suggestion_id), candidateId: String(row.candidate_id),
    fingerprint: String(row.fingerprint), reason: String(row.reason),
    nextStep: String(row.next_step), evidence: JSON.parse(String(row.evidence_json)),
    generatedAt: String(row.generated_at), feedback: row.feedback as AssistantSuggestion['feedback'],
    ...(row.snoozed_until ? { snoozedUntil: String(row.snoozed_until) } : {}),
    deliveryStatus: row.delivery_status as AssistantSuggestion['deliveryStatus'],
    ...(row.delivery_ref ? { deliveryRef: String(row.delivery_ref) } : {}),
    ...(row.read_at ? { readAt: String(row.read_at) } : {}) };
}

/** Assistant Home owns suggestions and feedback; the host owns any channel send receipt. */
export class AssistantSuggestionStore {
  readonly database: DatabaseSync;

  constructor(private readonly memory: AssistantMemoryRepository,
    private readonly now: () => Date = () => new Date()) {
    this.database = memory.sources.database;
    this.database.exec(schema);
  }

  pausedUntil(): string | undefined {
    const row = this.database.prepare('SELECT paused_until FROM assistant_suggestion_settings WHERE id=1')
      .get() as { paused_until: string | null };
    return row.paused_until ?? undefined;
  }

  pause(until?: string): void {
    if (until && (!Number.isFinite(Date.parse(until)) || Date.parse(until) <= this.now().getTime())) {
      throw new Error('Invalid suggestion pause deadline.');
    }
    this.database.prepare('UPDATE assistant_suggestion_settings SET paused_until=? WHERE id=1')
      .run(until ?? null);
  }

  isPaused(): boolean {
    const until = this.pausedUntil();
    return Boolean(until && Date.parse(until) > this.now().getTime());
  }

  private resumeSnoozed(): void {
    this.database.prepare(`UPDATE assistant_suggestions
      SET feedback='none',remind_at=snoozed_until,snoozed_until=NULL,
        delivery_status=CASE WHEN delivery_status='unknown' THEN 'unknown' ELSE 'not_requested' END,
        delivery_ref=CASE WHEN delivery_status='unknown' THEN delivery_ref ELSE NULL END,
        delivery_attempt_at=CASE WHEN delivery_status='unknown' THEN delivery_attempt_at ELSE NULL END,
        delivery_generation=CASE WHEN delivery_status='unknown' THEN delivery_generation
          ELSE delivery_generation+1 END,
        read_at=NULL
      WHERE feedback='snoozed' AND snoozed_until<=?`).run(this.now().toISOString());
  }

  list(limit = 50): AssistantSuggestion[] {
    if (!Number.isSafeInteger(limit) || limit < 1 || limit > 100) throw new Error('Invalid suggestion limit.');
    this.resumeSnoozed();
    const rows = this.database.prepare('SELECT * FROM assistant_suggestions ORDER BY generated_at DESC LIMIT ?')
      .all(limit) as Record<string, unknown>[];
    return rows.map(rowSuggestion);
  }

  get(id: string): AssistantSuggestion | undefined {
    const row = this.database.prepare('SELECT * FROM assistant_suggestions WHERE suggestion_id=?')
      .get(id) as Record<string, unknown> | undefined;
    return row && rowSuggestion(row);
  }

  feedback(id: string, action: 'ignored' | 'snoozed' | 'accepted', snoozedUntil?: string): AssistantSuggestion {
    if (action === 'snoozed' && (!snoozedUntil || !Number.isFinite(Date.parse(snoozedUntil))
      || Date.parse(snoozedUntil) <= this.now().getTime())) throw new Error('Invalid snooze deadline.');
    const changed = this.database.prepare(`UPDATE assistant_suggestions SET feedback=?,snoozed_until=?
      WHERE suggestion_id=?`).run(action, action === 'snoozed' ? snoozedUntil! : null, id);
    if (!changed.changes) throw new Error('Suggestion not found.');
    return this.get(id)!;
  }

  markRead(id: string): AssistantSuggestion {
    const changed = this.database.prepare(`UPDATE assistant_suggestions SET read_at=COALESCE(read_at,?)
      WHERE suggestion_id=?`).run(this.now().toISOString(), id);
    if (!changed.changes) throw new Error('Suggestion not found.');
    return this.get(id)!;
  }

  async candidates(limit = 4): Promise<SuggestionCandidate[]> {
    if (this.isPaused()) return [];
    this.resumeSnoozed();
    const result: SuggestionCandidate[] = [];
    let before = Number.MAX_SAFE_INTEGER;
    while (result.length < limit) {
      const rows = this.database.prepare(`SELECT rowid,id FROM memory_documents
        WHERE section='suggestions' AND id LIKE 'follow-up-%' AND status='active'
          AND rowid<? ORDER BY rowid DESC LIMIT 32`).all(before) as Array<{ rowid: number; id: string }>;
      if (!rows.length) break;
      before = rows.at(-1)!.rowid;
      for (const row of rows) {
      const document = await this.memory.get(row.id);
      if (!document || document.status !== 'active' || !document.evidence.length
        || document.audience.kind !== 'personal' || document.audience.id !== 'local-user') continue;
      if (document.evidence.some((ref) => {
        const current = this.memory.sources.source(ref.sourceId);
        return !current || current.availability !== 'available' || current.sourceVersion !== ref.sourceVersion;
      })) continue;
      const version = fingerprint([document.id, document.context, document.text, document.evidence]);
      if (this.database.prepare(`SELECT 1 FROM assistant_suggestion_considered
        WHERE candidate_id=? AND fingerprint=?`).get(document.id, version)) continue;
      const previous = this.database.prepare(`SELECT feedback,snoozed_until,generated_at
        FROM assistant_suggestions WHERE candidate_id=? ORDER BY generated_at DESC LIMIT 1`)
        .get(document.id) as { feedback: string; snoozed_until: string | null;
          generated_at: string } | undefined;
      if (previous && (['ignored', 'accepted'].includes(previous.feedback)
        || (previous.snoozed_until && Date.parse(previous.snoozed_until) > this.now().getTime())
        || this.now().getTime() - Date.parse(previous.generated_at) < 7 * 86_400_000)) continue;
      result.push({ candidateId: document.id, fingerprint: version,
        context: document.context.slice(0, 500), text: document.text.slice(0, 2_000),
        evidence: document.evidence });
      if (result.length >= limit) break;
      }
    }
    return result;
  }

  record(candidates: SuggestionCandidate[], proposals: SuggestionProposal[]): void {
    const now = this.now().toISOString();
    const selected = new Map(candidates.map((candidate) => [candidate.candidateId, candidate]));
    for (const candidate of candidates) {
      this.database.prepare(`INSERT OR IGNORE INTO assistant_suggestion_considered
        (candidate_id,fingerprint,checked_at) VALUES (?,?,?)`)
        .run(candidate.candidateId, candidate.fingerprint, now);
    }
    for (const item of proposals) {
      const candidate = selected.get(item.candidateId);
      if (!candidate) continue;
      const reason = redactReviewText(item.reason);
      const nextStep = redactReviewText(item.nextStep);
      if (reason !== item.reason || nextStep !== item.nextStep) continue;
      const id = `suggestion-${fingerprint([candidate.candidateId, candidate.fingerprint]).slice(0, 24)}`;
      this.database.prepare(`INSERT OR IGNORE INTO assistant_suggestions
        (suggestion_id,candidate_id,fingerprint,reason,next_step,evidence_json,generated_at)
        VALUES (?,?,?,?,?,?,?)`).run(id, candidate.candidateId, candidate.fingerprint,
          reason, nextStep, JSON.stringify(candidate.evidence), now);
    }
  }

  recordReflectionAttempt(effectId: string, candidates: SuggestionCandidate[]): void {
    this.database.prepare(`INSERT OR IGNORE INTO assistant_suggestion_reflections
      (effect_id,candidates_json) VALUES (?,?)`).run(effectId, JSON.stringify(suggestionCandidateKeys(candidates)));
  }

  reflectionAttempt(effectId: string): SuggestionCandidateKey[] | undefined {
    const row = this.database.prepare(`SELECT candidates_json FROM assistant_suggestion_reflections
      WHERE effect_id=?`).get(effectId) as { candidates_json: string } | undefined;
    return row && JSON.parse(row.candidates_json);
  }

  /** Withdraw obsolete content before displaying or sending it. A transient source outage only pauses sending. */
  async reconcileSources(): Promise<void> {
    const validity = new Map<string, boolean>();
    const isCurrent = async (key: SuggestionCandidateKey): Promise<boolean> => {
      const identity = `${key.candidateId}:${key.fingerprint}`;
      const cached = validity.get(identity);
      if (cached !== undefined) return cached;
      // A read/parse failure is not evidence that the source was withdrawn. Fail the
      // current operation closed and preserve the durable inbox for a later retry.
      const document = await this.memory.get(key.candidateId);
      const current = Boolean(document && document.status === 'active'
        && document.evidence.length > 0
        && fingerprint([document.id, document.context, document.text, document.evidence]) === key.fingerprint
        && document.evidence.every((ref) => {
          const source = this.memory.sources.source(ref.sourceId);
          return source && source.availability !== 'deleted'
            && source.sourceVersion === ref.sourceVersion;
        }));
      validity.set(identity, current);
      return current;
    };
    const rows = this.database.prepare('SELECT * FROM assistant_suggestions')
      .all() as Record<string, unknown>[];
    for (const row of rows) {
      const suggestion = rowSuggestion(row);
      if (!(await isCurrent(suggestion))) {
        this.database.prepare('DELETE FROM assistant_suggestion_delivery_attempts WHERE suggestion_id=?')
          .run(suggestion.suggestionId);
        this.database.prepare('DELETE FROM assistant_suggestions WHERE suggestion_id=?')
          .run(suggestion.suggestionId);
      }
    }
    const hasAutomation = Boolean(this.database.prepare(`SELECT 1 FROM sqlite_master
      WHERE type='table' AND name='automation_prepared_proposals'`).get());
    const prepared = hasAutomation ? this.database.prepare(`SELECT p.effect_id,p.proposal_json
      FROM automation_prepared_proposals p JOIN automation_jobs j ON j.job_id=p.job_id
      WHERE j.kind IN ('follow-up-check','daily-check','weekly-check')`).all() as
      Array<{ effect_id: string; proposal_json: string }> : [];
    for (const row of prepared) {
      const value = JSON.parse(row.proposal_json) as { candidateKeys?: SuggestionCandidateKey[] };
      if (!Array.isArray(value.candidateKeys)) continue;
      if ((await Promise.all(value.candidateKeys.map(isCurrent))).some((current) => !current)) {
        this.database.prepare('DELETE FROM automation_prepared_proposals WHERE effect_id=?').run(row.effect_id);
      }
    }
    const attempts = this.database.prepare(`SELECT effect_id,candidates_json
      FROM assistant_suggestion_reflections`).all() as
      Array<{ effect_id: string; candidates_json: string }>;
    for (const row of attempts) {
      const keys = JSON.parse(row.candidates_json) as SuggestionCandidateKey[];
      if ((await Promise.all(keys.map(isCurrent))).some((current) => !current)) {
        this.database.prepare('DELETE FROM assistant_suggestion_reflections WHERE effect_id=?')
          .run(row.effect_id);
      }
    }
  }

  async nextForDelivery(): Promise<AssistantSuggestion | undefined> {
    if (this.isPaused()) return undefined;
    this.resumeSnoozed();
    await this.reconcileSources();
    const today = new Date(this.now());
    today.setHours(0, 0, 0, 0);
    const sentToday = this.database.prepare(`SELECT 1 FROM assistant_suggestions
      WHERE delivery_attempt_at>=? AND delivery_status IN ('accepted','unknown') LIMIT 1`)
      .get(today.toISOString());
    const historicalSend = this.database.prepare(`SELECT 1 FROM assistant_suggestion_delivery_attempts
      WHERE attempted_at>=? AND status IN ('accepted','unknown') LIMIT 1`).get(today.toISOString());
    if (sentToday || historicalSend) return undefined;
    const recent = new Date(this.now().getTime() - 7 * 86_400_000).toISOString();
    const retryBefore = new Date(this.now().getTime() - 3_600_000).toISOString();
    for (let offset = 0; ; offset += 10) {
      const rows = this.database.prepare(`SELECT * FROM assistant_suggestions
        WHERE delivery_status='not_requested' AND feedback='none'
          AND (generated_at>=? OR remind_at>=?)
          AND (delivery_attempt_at IS NULL OR delivery_attempt_at<=?)
        ORDER BY generated_at DESC,suggestion_id DESC LIMIT 10 OFFSET ?`)
        .all(recent, recent, retryBefore, offset) as Record<string, unknown>[];
      if (!rows.length) break;
      for (const row of rows) {
        const suggestion = rowSuggestion(row);
        if (suggestion.evidence.every((ref) =>
          this.memory.sources.source(ref.sourceId)?.availability === 'available')) return suggestion;
      }
    }
    return undefined;
  }

  noteDeliveryAttempt(id: string): void {
    const deliveryId = this.deliveryId(id);
    this.database.prepare(`UPDATE assistant_suggestions SET delivery_attempt_at=?
      WHERE suggestion_id=? AND delivery_status='not_requested'`)
      .run(this.now().toISOString(), id);
    this.database.prepare(`INSERT OR IGNORE INTO assistant_suggestion_delivery_attempts
      (delivery_id,suggestion_id,attempted_at,status) VALUES (?,?,?,'pending')`)
      .run(deliveryId, id, this.now().toISOString());
  }

  deliveryId(id: string): string {
    const row = this.database.prepare(`SELECT delivery_generation
      FROM assistant_suggestions WHERE suggestion_id=?`).get(id) as
      { delivery_generation: number } | undefined;
    if (!row) throw new Error('Suggestion not found.');
    return row.delivery_generation ? `${id}-r${row.delivery_generation}` : id;
  }

  recordDelivery(id: string, status: 'accepted' | 'failed' | 'unknown', ref: string): void {
    const deliveryId = this.deliveryId(id);
    this.database.prepare(`UPDATE assistant_suggestions SET delivery_status=?,delivery_ref=?
      WHERE suggestion_id=? AND delivery_status='not_requested'`).run(status, ref, id);
    this.database.prepare(`UPDATE assistant_suggestion_delivery_attempts SET status=?
      WHERE delivery_id=?`).run(status, deliveryId);
  }
}

export function parseSuggestionProposal(message: string, candidates: SuggestionCandidate[]): SuggestionProposal[] {
  if (message.length > 12_000) throw new Error('Suggestion proposal exceeds budget.');
  const parsed: unknown = JSON.parse(message);
  const items = parsed && typeof parsed === 'object' && !Array.isArray(parsed)
    ? (parsed as { suggestions?: unknown }).suggestions : undefined;
  if (!Array.isArray(items) || items.length > 2) throw new Error('Invalid suggestion proposal.');
  const known = new Set(candidates.map((item) => item.candidateId));
  if (items.some((item) => !item || typeof item !== 'object'
    || !known.has((item as SuggestionProposal).candidateId)
    || typeof (item as SuggestionProposal).reason !== 'string'
    || !(item as SuggestionProposal).reason.trim() || (item as SuggestionProposal).reason.length > 500
    || typeof (item as SuggestionProposal).nextStep !== 'string'
    || !(item as SuggestionProposal).nextStep.trim() || (item as SuggestionProposal).nextStep.length > 500)) {
    throw new Error('Invalid suggestion proposal.');
  }
  if (new Set(items.map((item) => (item as SuggestionProposal).candidateId)).size !== items.length) {
    throw new Error('Duplicate suggestion candidate.');
  }
  return (items as SuggestionProposal[]).filter((item) =>
    redactReviewText(item.reason) === item.reason
    && redactReviewText(item.nextStep) === item.nextStep);
}

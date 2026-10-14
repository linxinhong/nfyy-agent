import { createHash, randomUUID } from 'node:crypto';
import type { DatabaseSync } from 'node:sqlite';

import type { AssistantSourceChange, DesktopTranscriptMessage } from '@yuanpu-agent/protocol';

export interface AssistantHostBinding {
  channel: 'desktop' | 'wecom';
  accountId: string;
  externalUserId: string;
  externalConversationId: string;
  principalId: string;
  assistantId: string;
  conversationId: string;
  sessionId: string;
  generation: number;
  contactId?: string;
}

export interface AssistantHostRequest {
  requestId: string;
  dedupKey: string;
  channel: 'desktop' | 'wecom';
  conversationId: string;
  sessionId: string;
  principalId: string;
  bindingGeneration: number;
  accountId: string;
  externalUserId: string;
  externalConversationId: string;
  externalMessageId: string;
  providerRequestId?: string;
  text: string;
  cancelRequested: boolean;
  status: 'accepted' | 'running' | 'completed' | 'failed' | 'cancelled' | 'interrupted';
  responseText?: string;
  errorCode?: string;
  createdAt: string;
  updatedAt: string;
}

export interface AssistantHostDelivery {
  status: 'pending' | 'delivering' | 'accepted' | 'failed' | 'unknown';
  failureCode?: string;
}

export interface AssistantProactiveDelivery {
  suggestionId: string;
  status: 'delivering' | 'accepted' | 'failed' | 'unknown';
  failureCode?: string;
}

interface BindingRow {
  channel: AssistantHostBinding['channel'];
  account_id: string;
  external_user_id: string;
  external_conversation_id: string;
  principal_id: string;
  assistant_id: string;
  conversation_id: string;
  session_id: string;
  generation: number;
  contact_id: string | null;
}

interface RequestRow {
  request_id: string;
  dedup_key: string;
  channel: AssistantHostRequest['channel'];
  conversation_id: string;
  session_id: string;
  principal_id: string;
  binding_generation: number;
  account_id: string;
  external_user_id: string;
  external_conversation_id: string;
  external_message_id: string;
  provider_request_id: string | null;
  text: string;
  cancel_requested: number;
  status: AssistantHostRequest['status'];
  response_text: string | null;
  error_code: string | null;
  created_at: string;
  updated_at: string;
}

const ownerPrincipalId = 'local-user';
const assistantId = 'local-assistant';

function bindingFromRow(row: BindingRow): AssistantHostBinding {
  return {
    channel: row.channel, accountId: row.account_id, externalUserId: row.external_user_id,
    externalConversationId: row.external_conversation_id, principalId: row.principal_id,
    assistantId: row.assistant_id, conversationId: row.conversation_id, sessionId: row.session_id,
    generation: row.generation,
    ...(row.contact_id ? { contactId: row.contact_id } : {}),
  };
}

function requestFromRow(row: RequestRow): AssistantHostRequest {
  return {
    requestId: row.request_id, dedupKey: row.dedup_key, channel: row.channel,
    conversationId: row.conversation_id, sessionId: row.session_id, principalId: row.principal_id,
    bindingGeneration: row.binding_generation,
    accountId: row.account_id, externalUserId: row.external_user_id,
    externalConversationId: row.external_conversation_id, externalMessageId: row.external_message_id,
    ...(row.provider_request_id ? { providerRequestId: row.provider_request_id } : {}),
    text: row.text, cancelRequested: row.cancel_requested === 1, status: row.status,
    ...(row.response_text !== null ? { responseText: row.response_text } : {}),
    ...(row.error_code ? { errorCode: row.error_code } : {}),
    createdAt: row.created_at, updatedAt: row.updated_at,
  };
}

/** Host-only identity, request and delivery ledger. Assistant Home never owns platform credentials. */
export class AssistantHostStore {
  constructor(private readonly database: DatabaseSync) {}

  markUncertainProactiveDeliveries(): void {
    this.database.prepare(`UPDATE yp_assistant_proactive_deliveries
      SET status='unknown',failure_code='interrupted',updated_at=?
      WHERE status='delivering'`).run(new Date().toISOString());
  }

  proactiveDelivery(suggestionId: string): AssistantProactiveDelivery | undefined {
    const row = this.database.prepare(`SELECT suggestion_id,status,failure_code
      FROM yp_assistant_proactive_deliveries WHERE suggestion_id=?`).get(suggestionId) as
      { suggestion_id: string; status: AssistantProactiveDelivery['status'];
        failure_code: string | null } | undefined;
    return row && { suggestionId: row.suggestion_id, status: row.status,
      ...(row.failure_code ? { failureCode: row.failure_code } : {}) };
  }

  beginProactiveDelivery(suggestionId: string, content: string,
    binding: AssistantHostBinding): { started: boolean; record: AssistantProactiveDelivery } {
    if (!/^suggestion-[a-f0-9]{24}(?:-r[1-9][0-9]*)?$/u.test(suggestionId) || !content.trim()
      || content.length > 4_000 || binding.channel !== 'wecom') {
      throw new Error('Invalid Assistant proactive delivery.');
    }
    const digest = createHash('sha256').update(content).digest('hex');
    const now = new Date().toISOString();
    const changed = this.database.prepare(`INSERT OR IGNORE INTO yp_assistant_proactive_deliveries
      (suggestion_id,content_digest,account_id,external_user_id,binding_generation,
       status,created_at,updated_at) VALUES (?,?,?,?,?,'delivering',?,?)`)
      .run(suggestionId, digest, binding.accountId, binding.externalUserId,
        binding.generation, now, now);
    const row = this.database.prepare(`SELECT content_digest,account_id,external_user_id,
      binding_generation FROM yp_assistant_proactive_deliveries WHERE suggestion_id=?`)
      .get(suggestionId) as { content_digest: string; account_id: string;
        external_user_id: string; binding_generation: number };
    if (row.content_digest !== digest || row.account_id !== binding.accountId
      || row.external_user_id !== binding.externalUserId
      || row.binding_generation !== binding.generation) {
      throw new Error('Assistant proactive delivery key has different content or target.');
    }
    return { started: changed.changes === 1, record: this.proactiveDelivery(suggestionId)! };
  }

  finishProactiveDelivery(suggestionId: string,
    status: Extract<AssistantProactiveDelivery['status'], 'accepted' | 'failed' | 'unknown'>,
    failureCode?: string): void {
    this.database.prepare(`UPDATE yp_assistant_proactive_deliveries
      SET status=?,failure_code=?,updated_at=? WHERE suggestion_id=? AND status='delivering'`)
      .run(status, failureCode ?? null, new Date().toISOString(), suggestionId);
  }

  desktop(): AssistantHostBinding {
    const row = this.database.prepare(`SELECT * FROM yp_assistant_bindings WHERE channel = 'desktop'
      AND account_id = 'local-desktop' AND external_user_id = 'local-user'
      AND external_conversation_id = 'assistant'`).get() as unknown as BindingRow | undefined;
    if (row) return bindingFromRow(row);
    const now = new Date().toISOString();
    this.database.prepare(`INSERT OR IGNORE INTO yp_assistant_bindings(
      channel, account_id, external_user_id, external_conversation_id, principal_id,
      assistant_id, conversation_id, session_id, created_at
    ) VALUES ('desktop', 'local-desktop', 'local-user', 'assistant', ?, ?, ?, ?, ?)`)
      .run(ownerPrincipalId, assistantId, `assistant:desktop:${randomUUID()}`, randomUUID(), now);
    return this.desktop();
  }

  /** Existing shared-session link is read only. A failed migration leaves the old row intact. */
  migrateLegacyLink(): void {
    const migrated = this.database.prepare(`SELECT value FROM yp_runtime_metadata
      WHERE key = 'assistant_link_migration_v1'`).get() as { value: string } | undefined;
    if (migrated) return;
    const existing = this.wecomLink();
    if (existing) return;
    const row = this.database.prepare('SELECT contact_id FROM yp_desktop_assistant_link WHERE id = 1')
      .get() as { contact_id: string } | undefined;
    if (!row) return;
    this.linkWecomContact(row.contact_id);
  }

  linkWecomContact(contactId: string): AssistantHostBinding {
    this.database.exec('BEGIN IMMEDIATE');
    try {
      const contact = this.database.prepare(`SELECT c.connection_id, c.recipient_id, c.sender_digest
        FROM yp_channel_private_contacts c JOIN yp_channel_pairings p ON p.provider = c.provider
          AND p.connection_id = c.connection_id AND p.sender_digest = c.sender_digest
        WHERE c.contact_id = ? AND c.provider = 'wecom' AND c.recipient_id IS NOT NULL`)
        .get(contactId) as { connection_id: string; recipient_id: string; sender_digest: string } | undefined;
      if (!contact) throw new Error('请选择已有私聊记录的已配对企业微信联系人。');
      this.database.prepare(`UPDATE yp_assistant_bindings SET active = 0 WHERE channel = 'wecom' AND active = 1`).run();
      this.database.prepare(`INSERT INTO yp_assistant_bindings(
        channel, account_id, external_user_id, external_conversation_id, principal_id,
        assistant_id, conversation_id, session_id, contact_id, active, created_at
      ) VALUES ('wecom', ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)
      ON CONFLICT(channel, account_id, external_user_id, external_conversation_id)
      DO UPDATE SET contact_id = excluded.contact_id, active = 1,
        generation = yp_assistant_bindings.generation + 1`)
        .run(contact.connection_id, contact.recipient_id, contact.recipient_id,
          ownerPrincipalId, assistantId, `assistant:wecom:${randomUUID()}`, randomUUID(), contactId,
          new Date().toISOString());
      this.database.prepare(`INSERT INTO yp_runtime_metadata(key, value, updated_at)
        VALUES ('assistant_link_migration_v1', 'complete', ?)
        ON CONFLICT(key) DO UPDATE SET value = 'complete', updated_at = excluded.updated_at`)
        .run(new Date().toISOString());
      this.database.exec('COMMIT');
      return this.wecomLink()!;
    } catch (error) {
      this.database.exec('ROLLBACK');
      throw error;
    }
  }

  unlinkWecom(): void {
    this.database.exec('BEGIN IMMEDIATE');
    try {
      this.database.prepare(`UPDATE yp_assistant_bindings SET active = 0 WHERE channel = 'wecom'`).run();
      this.database.prepare(`INSERT INTO yp_runtime_metadata(key, value, updated_at)
        VALUES ('assistant_link_migration_v1', 'unlinked', ?)
        ON CONFLICT(key) DO UPDATE SET value = 'unlinked', updated_at = excluded.updated_at`)
        .run(new Date().toISOString());
      this.database.exec('COMMIT');
    } catch (error) {
      this.database.exec('ROLLBACK');
      throw error;
    }
  }

  wecomLink(): AssistantHostBinding | undefined {
    const row = this.database.prepare(`SELECT b.* FROM yp_assistant_bindings b
      JOIN yp_channel_private_contacts c ON c.contact_id = b.contact_id
      JOIN yp_channel_pairings p ON p.provider = c.provider AND p.connection_id = c.connection_id
        AND p.sender_digest = c.sender_digest
      WHERE b.channel = 'wecom' AND b.active = 1 AND c.recipient_id = b.external_user_id
      AND c.connection_id = b.account_id LIMIT 1`).get() as unknown as BindingRow | undefined;
    return row ? bindingFromRow(row) : undefined;
  }

  wecomForMessage(connectionId: string, senderId: string, conversationId: string): AssistantHostBinding | undefined {
    const link = this.wecomLink();
    return link?.accountId === connectionId && link.externalUserId === senderId
      && link.externalConversationId === conversationId ? link : undefined;
  }

  ownsWecomMessage(connectionId: string, senderId: string, conversationId: string): boolean {
    if (this.wecomForMessage(connectionId, senderId, conversationId)) return true;
    const marker = this.database.prepare(`SELECT value FROM yp_runtime_metadata
      WHERE key = 'assistant_link_migration_v1'`).get() as { value: string } | undefined;
    if (marker?.value === 'unlinked') return false;
    return Boolean(this.database.prepare(`SELECT 1 FROM yp_desktop_assistant_link l
      JOIN yp_channel_private_contacts c ON c.contact_id = l.contact_id
      WHERE l.connection_id = ? AND c.recipient_id = ? AND c.recipient_id = ?
        AND c.provider = 'wecom'`).get(connectionId, senderId, conversationId));
  }

  accept(input: Omit<AssistantHostRequest, 'requestId' | 'status' | 'cancelRequested' | 'createdAt' | 'updatedAt'>): {
    record: AssistantHostRequest; duplicate: boolean;
  } {
    const now = new Date().toISOString();
    const inserted = this.database.prepare(`INSERT OR IGNORE INTO yp_assistant_requests(
      request_id, dedup_key, channel, conversation_id, session_id, principal_id, binding_generation,
      account_id, external_user_id, external_conversation_id, external_message_id,
      provider_request_id, text, status, created_at, updated_at
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'accepted', ?, ?)`)
      .run(`asst_${randomUUID().replaceAll('-', '_')}`, input.dedupKey, input.channel,
        input.conversationId, input.sessionId, input.principalId, input.bindingGeneration, input.accountId,
        input.externalUserId, input.externalConversationId, input.externalMessageId,
        input.providerRequestId ?? null, input.text, now, now).changes === 1;
    const row = this.database.prepare('SELECT * FROM yp_assistant_requests WHERE dedup_key = ?')
      .get(input.dedupKey) as unknown as RequestRow;
    const record = requestFromRow(row);
    if (!inserted && (record.text !== input.text || record.conversationId !== input.conversationId
      || record.externalUserId !== input.externalUserId)) throw new Error('Conflicting assistant duplicate.');
    return { record, duplicate: !inserted };
  }

  get(requestId: string): AssistantHostRequest | undefined {
    const row = this.database.prepare('SELECT * FROM yp_assistant_requests WHERE request_id = ?')
      .get(requestId) as unknown as RequestRow | undefined;
    return row ? requestFromRow(row) : undefined;
  }

  unsettled(): AssistantHostRequest[] {
    return (this.database.prepare(`SELECT * FROM yp_assistant_requests WHERE status IN ('accepted', 'running')
      ORDER BY created_at`).all() as unknown as RequestRow[]).map(requestFromRow);
  }

  pendingReplies(connectionId: string): AssistantHostRequest[] {
    return (this.database.prepare(`SELECT r.* FROM yp_assistant_requests r
      LEFT JOIN yp_assistant_deliveries d ON d.request_id = r.request_id
      WHERE r.channel = 'wecom' AND r.account_id = ?
        AND r.status IN ('completed', 'failed', 'cancelled', 'interrupted')
        AND (d.request_id IS NULL OR d.status = 'pending') ORDER BY r.created_at`)
      .all(connectionId) as unknown as RequestRow[]).map(requestFromRow);
  }

  canDeliver(request: AssistantHostRequest): boolean {
    if (request.channel !== 'wecom') return false;
    const link = this.wecomLink();
    return Boolean(link && link.conversationId === request.conversationId
      && link.generation === request.bindingGeneration
      && link.accountId === request.accountId && link.externalUserId === request.externalUserId
      && link.externalConversationId === request.externalConversationId);
  }

  markRunning(requestId: string): void {
    this.database.prepare(`UPDATE yp_assistant_requests SET status = 'running', updated_at = ?
      WHERE request_id = ? AND status = 'accepted'`).run(new Date().toISOString(), requestId);
  }

  requestCancel(requestId: string): void {
    this.database.prepare(`UPDATE yp_assistant_requests SET cancel_requested = 1, updated_at = ?
      WHERE request_id = ? AND status IN ('accepted', 'running')`)
      .run(new Date().toISOString(), requestId);
  }

  finish(requestId: string, status: Extract<AssistantHostRequest['status'], 'completed' | 'failed' | 'cancelled' | 'interrupted'>,
    responseText?: string, errorCode?: string): void {
    const now = new Date().toISOString();
    this.database.exec('BEGIN IMMEDIATE');
    try {
      this.database.prepare(`UPDATE yp_assistant_requests SET status = ?, response_text = ?, error_code = ?, updated_at = ?
        WHERE request_id = ? AND status IN ('accepted', 'running')`)
        .run(status, responseText ?? null, errorCode ?? null, now, requestId);
      if (status === 'completed' && responseText) {
        const request = this.get(requestId)!;
        const sourceId = `assistant-turn:${requestId}`;
        const version = createHash('sha256').update(JSON.stringify([request.text, responseText])).digest('hex');
        this.database.prepare(`INSERT OR IGNORE INTO yp_assistant_sources(
          request_id, source_id, source_version, content_ref, audience_id, occurred_at
        ) VALUES (?, ?, ?, ?, ?, ?)`)
          .run(requestId, sourceId, version, `assistant-content:${requestId}`, request.principalId, now);
      }
      this.database.exec('COMMIT');
    } catch (error) {
      this.database.exec('ROLLBACK');
      throw error;
    }
  }

  recordLegacyArchive(sessionId: string, transcript: readonly DesktopTranscriptMessage[]): void {
    if (!transcript.length) return;
    const sourceId = `legacy-assistant:${createHash('sha256').update(sessionId).digest('hex')}`;
    const body = JSON.stringify(transcript);
    const previous = this.database.prepare(`SELECT source_version, legacy_content_json
      FROM yp_assistant_sources WHERE source_id = ? ORDER BY event_id DESC LIMIT 1`)
      .get(sourceId) as { source_version: string; legacy_content_json: string | null } | undefined;
    if (previous?.legacy_content_json === body) return;
    const revision = previous ? Number.parseInt(previous.source_version.split(':')[0]!, 10) + 1 : 1;
    const version = `${revision}:${createHash('sha256').update(body).digest('hex')}`;
    const contentRef = `legacy-assistant-content:${createHash('sha256')
      .update(`${sessionId}\0${version}`).digest('hex')}`;
    this.database.prepare(`INSERT OR IGNORE INTO yp_assistant_sources(
      source_id, source_version, content_ref, audience_id, legacy_content_json, occurred_at
    ) VALUES (?, ?, ?, 'local-user', ?, ?)`).run(sourceId, version, contentRef, body, new Date().toISOString());
  }

  transcript(conversationId: string): DesktopTranscriptMessage[] {
    const rows = this.database.prepare(`SELECT * FROM yp_assistant_requests WHERE conversation_id = ? ORDER BY created_at`)
      .all(conversationId) as unknown as RequestRow[];
    return rows.flatMap((row): DesktopTranscriptMessage[] => {
      const result: DesktopTranscriptMessage[] = [{
        id: `${row.request_id}:user`, role: 'user', text: row.text, at: row.created_at,
      }];
      if (['completed', 'failed', 'cancelled', 'interrupted'].includes(row.status)) result.push({
        id: `${row.request_id}:assistant`, role: 'assistant',
        text: row.response_text ?? (row.status === 'cancelled' ? '任务已取消。' : row.status === 'interrupted' ? '助理任务已中断。' : '助理处理失败。'),
        at: row.updated_at,
        run: { runId: row.request_id, source: 'runtime',
          status: row.status === 'completed' ? 'succeeded' : row.status === 'accepted' ? 'queued' : row.status,
          createdAt: row.created_at, updatedAt: row.updated_at, events: [], tools: [] },
      });
      return result;
    });
  }

  beginDelivery(requestId: string): boolean {
    const now = new Date().toISOString();
    this.database.prepare(`INSERT OR IGNORE INTO yp_assistant_deliveries(request_id, status, updated_at)
      VALUES (?, 'pending', ?)`).run(requestId, now);
    return this.database.prepare(`UPDATE yp_assistant_deliveries SET status = 'delivering', updated_at = ?
      WHERE request_id = ? AND status = 'pending'`).run(now, requestId).changes === 1;
  }

  finishDelivery(requestId: string, status: Extract<AssistantHostDelivery['status'], 'accepted' | 'failed' | 'unknown'>,
    failureCode?: string): void {
    this.database.prepare(`UPDATE yp_assistant_deliveries SET status = ?, failure_code = ?, updated_at = ?
      WHERE request_id = ? AND status = 'delivering'`)
      .run(status, failureCode ?? null, new Date().toISOString(), requestId);
  }

  markUncertainDeliveries(): void {
    this.database.prepare(`UPDATE yp_assistant_deliveries SET status = 'unknown',
      failure_code = 'process_interrupted', updated_at = ? WHERE status = 'delivering'`)
      .run(new Date().toISOString());
  }

  delivery(requestId: string): AssistantHostDelivery | undefined {
    const row = this.database.prepare('SELECT status, failure_code FROM yp_assistant_deliveries WHERE request_id = ?')
      .get(requestId) as { status: AssistantHostDelivery['status']; failure_code: string | null } | undefined;
    return row ? { status: row.status, ...(row.failure_code ? { failureCode: row.failure_code } : {}) } : undefined;
  }

  sourceChanges(afterEventId = 0): Array<{ eventId: number; change: AssistantSourceChange }> {
    const rows = this.database.prepare(`SELECT s.event_id, s.source_id, s.source_version, s.content_ref,
      s.audience_id, s.occurred_at, EXISTS(SELECT 1 FROM yp_assistant_sources p
        WHERE p.source_id = s.source_id AND p.event_id < s.event_id) AS prior
      FROM yp_assistant_sources s WHERE s.event_id > ? ORDER BY s.event_id`)
      .all(afterEventId) as Array<{ event_id: number; source_id: string; source_version: string;
        content_ref: string; audience_id: string; occurred_at: string; prior: number }>;
    return rows.map((row) => ({ eventId: row.event_id, change: {
      sourceId: row.source_id, sourceVersion: row.source_version, kind: row.prior ? 'updated' : 'created',
      audience: { kind: 'personal', id: row.audience_id }, occurredAt: row.occurred_at,
      contentRef: row.content_ref,
    } }));
  }

  sourcePage(afterEventId: number, limit: number,
    audienceId = 'local-user'): Array<{ eventId: number; change: AssistantSourceChange }> {
    if (!Number.isSafeInteger(afterEventId) || afterEventId < 0
      || !Number.isSafeInteger(limit) || limit < 1 || limit > 500) {
      throw new Error('Invalid Assistant source page boundary.');
    }
    const rows = this.database.prepare(`SELECT s.event_id, s.source_id, s.source_version, s.content_ref,
      s.audience_id, s.occurred_at, EXISTS(SELECT 1 FROM yp_assistant_sources p
        WHERE p.source_id = s.source_id AND p.event_id < s.event_id) AS prior
      FROM yp_assistant_sources s WHERE s.event_id > ? AND s.audience_id = ?
      ORDER BY s.event_id LIMIT ?`)
      .all(afterEventId, audienceId, limit) as Array<{ event_id: number; source_id: string; source_version: string;
        content_ref: string; audience_id: string; occurred_at: string; prior: number }>;
    return rows.map((row) => ({ eventId: row.event_id, change: {
      sourceId: row.source_id, sourceVersion: row.source_version, kind: row.prior ? 'updated' : 'created',
      audience: { kind: 'personal', id: row.audience_id }, occurredAt: row.occurred_at,
      contentRef: row.content_ref,
    } }));
  }

  currentSourceChange(sourceId: string): AssistantSourceChange | undefined {
    const row = this.database.prepare(`SELECT source_id,source_version,content_ref,audience_id,occurred_at
      FROM yp_assistant_sources WHERE source_id = ? ORDER BY event_id DESC LIMIT 1`)
      .get(sourceId) as { source_id: string; source_version: string; content_ref: string;
        audience_id: string; occurred_at: string } | undefined;
    return row ? { sourceId: row.source_id, sourceVersion: row.source_version,
      kind: 'updated', audience: { kind: 'personal', id: row.audience_id },
      occurredAt: row.occurred_at, contentRef: row.content_ref } : undefined;
  }

  resolveContentRef(ref: string, principalId: string):
    { userText: string; assistantText: string } | { transcript: DesktopTranscriptMessage[] } | undefined {
    const row = this.database.prepare(`SELECT r.text, r.response_text, s.legacy_content_json
      FROM yp_assistant_sources s
      LEFT JOIN yp_assistant_requests r ON r.request_id = s.request_id
      WHERE s.content_ref = ? AND s.audience_id = ?`).get(ref, principalId) as
      { text: string | null; response_text: string | null; legacy_content_json: string | null } | undefined;
    if (row?.legacy_content_json) return { transcript: JSON.parse(row.legacy_content_json) as DesktopTranscriptMessage[] };
    return row?.text && row.response_text ? { userText: row.text, assistantText: row.response_text } : undefined;
  }
}

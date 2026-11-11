import { chmodSync, closeSync, lstatSync, mkdirSync, openSync } from 'node:fs';
import { dirname } from 'node:path';
import { DatabaseSync } from 'node:sqlite';

import { AgentRunStore } from './agent-run-store.js';
import { AssistantLinkStore } from './assistant-link-store.js';
import { AssistantHostStore } from './assistant-host-store.js';
import { AssistantSourceLifecycleStore } from './assistant-source-lifecycle-store.js';
import { WorkConversationStore } from './work-conversation-store.js';
import { WorkEvidenceStore } from './work-evidence-store.js';
import { WorkFileChangeStore } from './work-file-changes-store.js';
import { ChannelStore } from '../channels/store.js';
import { SchedulerStore } from '../scheduler/store.js';

export * from './agent-run-store.js';
export * from './assistant-link-store.js';
export * from './assistant-host-store.js';
export * from './assistant-source-lifecycle-store.js';
export * from './work-conversation-store.js';
export * from './work-evidence-store.js';
export * from './work-file-changes-store.js';

export const YUANPU_METADATA_SCHEMA_VERSION = 16;
export const YUANPU_SQLITE_DRIVER = 'node:sqlite';

interface Migration {
  version: number;
  sql: string;
  apply?: (database: DatabaseSync) => void;
}

const migrations: readonly Migration[] = [{
  version: 1,
  sql: `
    CREATE TABLE yp_runtime_metadata (
      key TEXT PRIMARY KEY,
      value TEXT NOT NULL,
      updated_at TEXT NOT NULL
    ) STRICT;

    CREATE TABLE yp_conversation_bindings (
      binding_id TEXT PRIMARY KEY,
      entry_point TEXT NOT NULL CHECK (entry_point IN ('desktop', 'im', 'scheduler')),
      authority_id TEXT NOT NULL,
      subject_id TEXT NOT NULL,
      namespace TEXT NOT NULL,
      conversation_id TEXT NOT NULL,
      thread_id TEXT NOT NULL DEFAULT '',
      pi_session_id TEXT NOT NULL,
      workspace_id TEXT NOT NULL,
      created_at TEXT NOT NULL,
      updated_at TEXT NOT NULL,
      UNIQUE (entry_point, authority_id, subject_id, namespace, conversation_id, thread_id),
      UNIQUE (binding_id, entry_point, authority_id, subject_id)
    ) STRICT;

    CREATE TABLE yp_agent_runs (
      run_id TEXT PRIMARY KEY,
      entry_point TEXT NOT NULL CHECK (entry_point IN ('desktop', 'im', 'scheduler')),
      authority_id TEXT NOT NULL,
      subject_id TEXT NOT NULL,
      idempotency_key TEXT NOT NULL,
      request_fingerprint TEXT NOT NULL,
      input_digest TEXT NOT NULL,
      request_metadata_json TEXT NOT NULL,
      binding_id TEXT,
      status TEXT NOT NULL CHECK (status IN (
        'queued', 'running', 'waiting_approval', 'succeeded', 'failed',
        'cancelled', 'interrupted', 'result_unknown'
      )),
      external_effect_state TEXT NOT NULL DEFAULT 'none'
        CHECK (external_effect_state IN ('none', 'possible')),
      approval_request_id TEXT,
      approval_session_id TEXT,
      approval_workspace_id TEXT,
      approval_expires_at TEXT,
      output_digest TEXT,
      failure_code TEXT,
      created_at TEXT NOT NULL,
      updated_at TEXT NOT NULL,
      UNIQUE (entry_point, authority_id, subject_id, idempotency_key),
      UNIQUE (run_id, entry_point, authority_id, subject_id),
      FOREIGN KEY (binding_id, entry_point, authority_id, subject_id)
        REFERENCES yp_conversation_bindings(binding_id, entry_point, authority_id, subject_id),
      CHECK (
        status <> 'waiting_approval'
        OR (
          approval_request_id IS NOT NULL
          AND approval_session_id IS NOT NULL
          AND approval_workspace_id IS NOT NULL
          AND approval_expires_at IS NOT NULL
        )
      ),
      CHECK (status <> 'result_unknown' OR external_effect_state = 'possible')
    ) STRICT;

    CREATE INDEX yp_agent_runs_status_created
      ON yp_agent_runs(status, created_at);

    CREATE TABLE yp_delivery_attempts (
      delivery_id TEXT PRIMARY KEY,
      run_id TEXT NOT NULL REFERENCES yp_agent_runs(run_id),
      idempotency_key TEXT NOT NULL,
      target_json TEXT NOT NULL,
      status TEXT NOT NULL CHECK (status IN (
        'pending', 'delivering', 'delivered', 'failed', 'result_unknown'
      )),
      attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
      last_error TEXT,
      created_at TEXT NOT NULL,
      updated_at TEXT NOT NULL,
      UNIQUE (run_id, idempotency_key)
    ) STRICT;

    CREATE TABLE yp_inbound_deduplication (
      entry_point TEXT NOT NULL,
      authority_id TEXT NOT NULL,
      subject_id TEXT NOT NULL,
      external_message_id TEXT NOT NULL,
      run_id TEXT REFERENCES yp_agent_runs(run_id),
      received_at TEXT NOT NULL,
      PRIMARY KEY (entry_point, authority_id, subject_id, external_message_id),
      FOREIGN KEY (run_id, entry_point, authority_id, subject_id)
        REFERENCES yp_agent_runs(run_id, entry_point, authority_id, subject_id)
    ) STRICT;
  `,
}, {
  version: 2,
  sql: `
    ALTER TABLE yp_agent_runs ADD COLUMN failure_message TEXT;
    ALTER TABLE yp_agent_runs ADD COLUMN failure_retryable INTEGER
      CHECK (failure_retryable IN (0, 1));

    CREATE TABLE yp_agent_run_queue_payloads (
      run_id TEXT PRIMARY KEY REFERENCES yp_agent_runs(run_id),
      input_text TEXT NOT NULL,
      created_at TEXT NOT NULL
    ) STRICT;

    UPDATE yp_agent_runs
    SET status = 'interrupted',
      failure_code = 'migration_payload_unavailable',
      failure_message = 'Queued input was not retained by metadata schema v1 and cannot be resumed.',
      failure_retryable = 1
    WHERE status = 'queued';
  `,
}, {
  version: 3,
  sql: `
    CREATE TABLE yp_agent_run_outputs (
      run_id TEXT PRIMARY KEY REFERENCES yp_agent_runs(run_id),
      output_json TEXT NOT NULL,
      created_at TEXT NOT NULL
    ) STRICT;

    CREATE TABLE yp_schedules (
      schedule_id TEXT PRIMARY KEY,
      revision INTEGER NOT NULL CHECK (revision > 0),
      name TEXT NOT NULL,
      prompt TEXT NOT NULL,
      workspace_id TEXT NOT NULL,
      timing_json TEXT NOT NULL,
      time_zone TEXT NOT NULL,
      enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
      misfire_policy TEXT NOT NULL CHECK (misfire_policy IN ('coalesce', 'skip')),
      maximum_lateness_ms INTEGER NOT NULL CHECK (maximum_lateness_ms >= 0),
      allow_overlap INTEGER NOT NULL CHECK (allow_overlap IN (0, 1)),
      conversation_id TEXT NOT NULL,
      delivery_json TEXT NOT NULL,
      next_trigger_at TEXT,
      created_at TEXT NOT NULL,
      updated_at TEXT NOT NULL
    ) STRICT;

    CREATE INDEX yp_schedules_due
      ON yp_schedules(enabled, next_trigger_at);

    CREATE TABLE yp_schedule_triggers (
      trigger_key TEXT PRIMARY KEY,
      schedule_id TEXT NOT NULL REFERENCES yp_schedules(schedule_id),
      schedule_revision INTEGER NOT NULL,
      scheduled_at TEXT NOT NULL,
      request_json TEXT NOT NULL,
      status TEXT NOT NULL CHECK (status IN (
        'pending_submission', 'submitted', 'submission_failed',
        'skipped_misfire', 'skipped_overlap', 'output_unknown'
      )),
      run_id TEXT REFERENCES yp_agent_runs(run_id),
      last_error TEXT,
      created_at TEXT NOT NULL,
      updated_at TEXT NOT NULL,
      UNIQUE (schedule_id, schedule_revision, scheduled_at)
    ) STRICT;

    CREATE INDEX yp_schedule_triggers_schedule_history
      ON yp_schedule_triggers(schedule_id, scheduled_at DESC);
    CREATE INDEX yp_schedule_triggers_submission
      ON yp_schedule_triggers(status, created_at);
  `,
}, {
  version: 4,
  sql: `
    CREATE TABLE yp_channel_connections (
      provider TEXT NOT NULL,
      connection_id TEXT NOT NULL,
      provider_account_digest TEXT NOT NULL,
      credential_binding_digest TEXT NOT NULL,
      created_at TEXT NOT NULL,
      PRIMARY KEY (provider, connection_id),
      UNIQUE (provider, provider_account_digest)
    ) STRICT;

    CREATE TABLE yp_channel_pairings (
      provider TEXT NOT NULL,
      connection_id TEXT NOT NULL,
      sender_digest TEXT NOT NULL,
      created_at TEXT NOT NULL,
      PRIMARY KEY (provider, connection_id, sender_digest),
      FOREIGN KEY (provider, connection_id)
        REFERENCES yp_channel_connections(provider, connection_id)
    ) STRICT;

    CREATE TABLE yp_channel_inbound (
      inbound_id TEXT PRIMARY KEY,
      provider TEXT NOT NULL,
      connection_id TEXT NOT NULL,
      provider_message_id TEXT NOT NULL,
      provider_request_id TEXT NOT NULL,
      sender_digest TEXT NOT NULL,
      conversation_type TEXT NOT NULL CHECK (conversation_type IN ('single', 'group')),
      conversation_digest TEXT NOT NULL,
      message_type TEXT NOT NULL,
      content_digest TEXT,
      input_text TEXT,
      action TEXT NOT NULL DEFAULT 'run' CHECK (action IN ('run', 'cancel', 'unsupported')),
      cancel_target_run_id TEXT,
      run_id TEXT REFERENCES yp_agent_runs(run_id),
      received_at TEXT NOT NULL,
      UNIQUE (provider, connection_id, provider_message_id),
      FOREIGN KEY (provider, connection_id)
        REFERENCES yp_channel_connections(provider, connection_id),
      CHECK (action <> 'cancel' OR cancel_target_run_id IS NOT NULL)
    ) STRICT;

    CREATE INDEX yp_channel_inbound_conversation
      ON yp_channel_inbound(provider, connection_id, conversation_digest, received_at DESC);

    CREATE TABLE yp_channel_outbound (
      outbound_id TEXT PRIMARY KEY,
      inbound_id TEXT NOT NULL UNIQUE REFERENCES yp_channel_inbound(inbound_id),
      run_id TEXT UNIQUE REFERENCES yp_agent_runs(run_id),
      content_digest TEXT NOT NULL,
      status TEXT NOT NULL CHECK (status IN (
        'pending', 'delivering', 'accepted', 'failed', 'unknown'
      )),
      failure_code TEXT,
      created_at TEXT NOT NULL,
      updated_at TEXT NOT NULL
    ) STRICT;

    CREATE INDEX yp_channel_outbound_status
      ON yp_channel_outbound(status, updated_at);
  `,
}, {
  version: 5,
  sql: `
    CREATE TABLE yp_channel_private_contacts (
      contact_id TEXT PRIMARY KEY,
      provider TEXT NOT NULL,
      connection_id TEXT NOT NULL,
      sender_digest TEXT NOT NULL,
      recipient_id TEXT,
      bound_target_id TEXT UNIQUE,
      last_seen_at TEXT NOT NULL,
      UNIQUE (provider, connection_id, sender_digest),
      FOREIGN KEY (provider, connection_id, sender_digest)
        REFERENCES yp_channel_pairings(provider, connection_id, sender_digest),
      CHECK (bound_target_id IS NULL OR recipient_id IS NOT NULL)
    ) STRICT;

    CREATE INDEX yp_channel_private_contacts_bound_target
      ON yp_channel_private_contacts(bound_target_id);

    CREATE TABLE yp_schedule_notification_receipts (
      run_id TEXT PRIMARY KEY REFERENCES yp_agent_runs(run_id),
      status TEXT NOT NULL CHECK (status IN (
        'pending', 'submitted', 'suppressed', 'unavailable', 'failed', 'result_unknown'
      )),
      created_at TEXT NOT NULL,
      updated_at TEXT NOT NULL
    ) STRICT;
  `,
}, {
  version: 6,
  sql: `
    CREATE TABLE yp_desktop_assistant_link (
      id INTEGER PRIMARY KEY CHECK (id = 1),
      contact_id TEXT NOT NULL,
      connection_id TEXT NOT NULL,
      target_id TEXT NOT NULL,
      previous_pi_session_id TEXT NOT NULL,
      linked_pi_session_id TEXT NOT NULL,
      updated_at TEXT NOT NULL
    ) STRICT;
    CREATE TABLE yp_assistant_mirror (
      mirror_id TEXT PRIMARY KEY,
      run_id TEXT NOT NULL REFERENCES yp_agent_runs(run_id),
      part TEXT NOT NULL CHECK (part IN ('user', 'assistant')),
      target_id TEXT NOT NULL,
      content TEXT,
      content_digest TEXT NOT NULL,
      status TEXT NOT NULL CHECK (status IN ('pending', 'delivering', 'accepted', 'failed', 'unknown')),
      failure_code TEXT,
      created_at TEXT NOT NULL,
      updated_at TEXT NOT NULL,
      UNIQUE (run_id, part)
    ) STRICT;
  `,
}, {
  version: 7,
  sql: `
    CREATE TABLE yp_work_conversations (
      conversation_id TEXT PRIMARY KEY,
      pi_session_id TEXT NOT NULL UNIQUE,
      workspace_id TEXT NOT NULL,
      created_at TEXT NOT NULL,
      updated_at TEXT NOT NULL
    ) STRICT;
    CREATE TABLE yp_work_turn_sources (
      conversation_id TEXT NOT NULL,
      turn_id TEXT NOT NULL,
      run_id TEXT NOT NULL UNIQUE,
      content_ref TEXT NOT NULL UNIQUE,
      source_version TEXT NOT NULL,
      committed_at TEXT NOT NULL,
      user_text TEXT NOT NULL,
      assistant_text TEXT NOT NULL,
      PRIMARY KEY (conversation_id, turn_id)
    ) STRICT;
  `,
}, {
  version: 8,
  sql: `
    CREATE TABLE yp_assistant_bindings (
      channel TEXT NOT NULL CHECK (channel IN ('desktop', 'wecom')),
      account_id TEXT NOT NULL,
      external_user_id TEXT NOT NULL,
      external_conversation_id TEXT NOT NULL,
      principal_id TEXT NOT NULL,
      assistant_id TEXT NOT NULL,
      conversation_id TEXT NOT NULL UNIQUE,
      session_id TEXT NOT NULL UNIQUE,
      contact_id TEXT,
      generation INTEGER NOT NULL DEFAULT 1 CHECK (generation > 0),
      active INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
      created_at TEXT NOT NULL,
      PRIMARY KEY (channel, account_id, external_user_id, external_conversation_id)
    ) STRICT;
    CREATE UNIQUE INDEX yp_assistant_active_wecom
      ON yp_assistant_bindings(channel) WHERE channel = 'wecom' AND active = 1;
    CREATE TABLE yp_assistant_requests (
      request_id TEXT PRIMARY KEY,
      dedup_key TEXT NOT NULL UNIQUE,
      channel TEXT NOT NULL CHECK (channel IN ('desktop', 'wecom')),
      conversation_id TEXT NOT NULL REFERENCES yp_assistant_bindings(conversation_id),
      session_id TEXT NOT NULL,
      principal_id TEXT NOT NULL,
      binding_generation INTEGER NOT NULL,
      account_id TEXT NOT NULL,
      external_user_id TEXT NOT NULL,
      external_conversation_id TEXT NOT NULL,
      external_message_id TEXT NOT NULL,
      provider_request_id TEXT,
      text TEXT NOT NULL,
      cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0, 1)),
      status TEXT NOT NULL CHECK (status IN ('accepted', 'running', 'completed', 'failed', 'cancelled', 'interrupted')),
      response_text TEXT,
      error_code TEXT,
      created_at TEXT NOT NULL,
      updated_at TEXT NOT NULL
    ) STRICT;
    CREATE INDEX yp_assistant_requests_conversation
      ON yp_assistant_requests(conversation_id, created_at);
    CREATE TABLE yp_assistant_deliveries (
      request_id TEXT PRIMARY KEY REFERENCES yp_assistant_requests(request_id),
      status TEXT NOT NULL CHECK (status IN ('pending', 'delivering', 'accepted', 'failed', 'unknown')),
      failure_code TEXT,
      updated_at TEXT NOT NULL
    ) STRICT;
    CREATE TABLE yp_assistant_sources (
      event_id INTEGER PRIMARY KEY AUTOINCREMENT,
      request_id TEXT UNIQUE REFERENCES yp_assistant_requests(request_id),
      source_id TEXT NOT NULL,
      source_version TEXT NOT NULL,
      content_ref TEXT NOT NULL UNIQUE,
      audience_id TEXT NOT NULL,
      legacy_content_json TEXT,
      occurred_at TEXT NOT NULL,
      UNIQUE (source_id, source_version),
      CHECK ((request_id IS NULL) <> (legacy_content_json IS NULL))
    ) STRICT;
  `,
}, {
  // Another development line used v8 for this Work directory column. Detect
  // the shape instead of assuming a migration number uniquely identifies it.
  version: 9,
  sql: '',
  apply: ensureWorkDirectorySchema,
}, {
  // Keep the repair above both historical v8 variants and a possible v9 Work
  // database. No earlier migration number is rewritten or reset.
  version: 10,
  sql: '',
  apply(database) {
    ensureWorkDirectorySchema(database);
    ensureAssistantHostSchema(database);
  },
}, {
  version: 11,
  sql: '',
  apply(database) {
    const columns = database.prepare('PRAGMA table_info(yp_work_turn_sources)').all() as
      Array<{ name: string; type: string }>;
    const eventId = columns.find((column) => column.name === 'event_id');
    if (eventId && eventId.type !== 'INTEGER') throw new Error('Incompatible Work source event ID.');
    if (!eventId) database.exec('ALTER TABLE yp_work_turn_sources ADD COLUMN event_id INTEGER');
    database.exec(`UPDATE yp_work_turn_sources SET event_id = rowid WHERE event_id IS NULL;
      CREATE UNIQUE INDEX IF NOT EXISTS yp_work_turn_sources_event_id ON yp_work_turn_sources(event_id);
      CREATE TABLE IF NOT EXISTS yp_work_source_event_sequence (
        id INTEGER PRIMARY KEY CHECK(id = 1), next_id INTEGER NOT NULL CHECK(next_id >= 0)
      ) STRICT;
      INSERT OR IGNORE INTO yp_work_source_event_sequence(id,next_id)
        SELECT 1, COALESCE(MAX(event_id), 0) FROM yp_work_turn_sources;
      UPDATE yp_work_source_event_sequence SET next_id = MAX(next_id,
        (SELECT COALESCE(MAX(event_id), 0) FROM yp_work_turn_sources)) WHERE id = 1;
      DROP TRIGGER IF EXISTS yp_work_turn_sources_assign_event_id;
      CREATE TRIGGER yp_work_turn_sources_assign_event_id
        AFTER INSERT ON yp_work_turn_sources WHEN NEW.event_id IS NULL BEGIN
          UPDATE yp_work_source_event_sequence SET next_id = next_id + 1 WHERE id = 1;
          UPDATE yp_work_turn_sources SET event_id =
            (SELECT next_id FROM yp_work_source_event_sequence WHERE id = 1)
            WHERE rowid = NEW.rowid;
        END;`);
    assertWorkSourceEventSchema(database);
  },
}, {
  version: 12,
  sql: `
    CREATE TABLE IF NOT EXISTS yp_assistant_source_deletions (
      event_id INTEGER PRIMARY KEY AUTOINCREMENT,
      feed_id TEXT NOT NULL CHECK(feed_id IN ('work','assistant')),
      source_id TEXT NOT NULL,
      source_version TEXT NOT NULL,
      audience_id TEXT NOT NULL,
      occurred_at TEXT NOT NULL,
      UNIQUE(feed_id,source_id)
    ) STRICT;
    CREATE TABLE IF NOT EXISTS yp_assistant_legacy_memory_events (
      event_id INTEGER PRIMARY KEY AUTOINCREMENT,
      source_version TEXT NOT NULL,
      occurred_at TEXT NOT NULL
    ) STRICT;
  `,
}, {
  version: 13,
  sql: '',
  apply(database) {
    const columns = new Set((database.prepare('PRAGMA table_info(yp_work_conversations)').all() as
      Array<{ name: string }>).map((column) => column.name));
    for (const [name, definition] of [
      ['title', "TEXT NOT NULL DEFAULT ''"], ['icon_id', "TEXT NOT NULL DEFAULT 'chat'"],
      ['folder_id', 'TEXT'], ['sort_order', 'INTEGER NOT NULL DEFAULT 0'], ['archived_at', 'TEXT'],
      ['request_id', 'TEXT'],
    ] as const) {
      if (!columns.has(name)) database.exec(`ALTER TABLE yp_work_conversations ADD COLUMN ${name} ${definition}`);
    }
    database.exec(`
    CREATE TABLE IF NOT EXISTS yp_work_folders (
      folder_id TEXT PRIMARY KEY,
      workspace_id TEXT NOT NULL,
      parent_id TEXT,
      name TEXT NOT NULL,
      icon_id TEXT NOT NULL,
      relative_directory TEXT NOT NULL,
      sort_order INTEGER NOT NULL,
      created_at TEXT NOT NULL,
      updated_at TEXT NOT NULL,
      request_id TEXT,
      UNIQUE(workspace_id, relative_directory),
      FOREIGN KEY(parent_id) REFERENCES yp_work_folders(folder_id)
    ) STRICT;
    CREATE INDEX IF NOT EXISTS yp_work_folders_siblings ON yp_work_folders(workspace_id,parent_id,sort_order);
    CREATE TABLE IF NOT EXISTS yp_work_tags (
      tag_id TEXT PRIMARY KEY,
      workspace_id TEXT NOT NULL,
      name TEXT NOT NULL,
      color TEXT NOT NULL,
      created_at TEXT NOT NULL,
      updated_at TEXT NOT NULL,
      request_id TEXT,
      UNIQUE(workspace_id,name)
    ) STRICT;
    CREATE TABLE IF NOT EXISTS yp_work_conversation_tags (
      conversation_id TEXT NOT NULL REFERENCES yp_work_conversations(conversation_id) ON DELETE CASCADE,
      tag_id TEXT NOT NULL REFERENCES yp_work_tags(tag_id) ON DELETE CASCADE,
      PRIMARY KEY(conversation_id,tag_id)
    ) STRICT;
    UPDATE yp_work_conversations SET sort_order = rowid WHERE sort_order = 0;
    CREATE UNIQUE INDEX IF NOT EXISTS yp_work_conversations_request ON yp_work_conversations(workspace_id,request_id)
      WHERE request_id IS NOT NULL;
    CREATE UNIQUE INDEX IF NOT EXISTS yp_work_folders_request ON yp_work_folders(workspace_id,request_id)
      WHERE request_id IS NOT NULL;
    CREATE UNIQUE INDEX IF NOT EXISTS yp_work_tags_request ON yp_work_tags(workspace_id,request_id)
      WHERE request_id IS NOT NULL;
    CREATE TABLE IF NOT EXISTS yp_work_create_intents (
      node_id TEXT PRIMARY KEY,
      workspace_id TEXT NOT NULL,
      relative_directory TEXT NOT NULL,
      kind TEXT NOT NULL CHECK(kind IN ('folder','conversation'))
    ) STRICT;
    `);
  },
}, {
  version: 14,
  sql: `
    CREATE TABLE IF NOT EXISTS yp_work_evidence_sources (
      event_id INTEGER PRIMARY KEY AUTOINCREMENT,
      source_id TEXT NOT NULL UNIQUE,
      content_ref TEXT NOT NULL UNIQUE,
      source_version TEXT NOT NULL,
      kind TEXT NOT NULL CHECK(kind IN ('tool_result','artifact')),
      conversation_id TEXT NOT NULL,
      pi_session_id TEXT NOT NULL,
      run_id TEXT,
      entry_id TEXT NOT NULL,
      tool_name TEXT NOT NULL,
      result_status TEXT NOT NULL CHECK(result_status IN ('completed','failed')),
      text_content TEXT,
      relative_path TEXT,
      file_sha256 TEXT,
      file_size INTEGER,
      committed_at TEXT NOT NULL,
      audience_id TEXT NOT NULL CHECK(audience_id='local-user'),
      UNIQUE(kind,pi_session_id,entry_id)
    ) STRICT;
    CREATE INDEX IF NOT EXISTS yp_work_evidence_conversation
      ON yp_work_evidence_sources(conversation_id,event_id);
  `,
}, {
  version: 15,
  sql: `
    CREATE TABLE IF NOT EXISTS yp_assistant_proactive_deliveries (
      suggestion_id TEXT PRIMARY KEY,
      content_digest TEXT NOT NULL,
      account_id TEXT NOT NULL,
      external_user_id TEXT NOT NULL,
      binding_generation INTEGER NOT NULL,
      status TEXT NOT NULL CHECK(status IN ('delivering','accepted','failed','unknown')),
      failure_code TEXT,
      created_at TEXT NOT NULL,
      updated_at TEXT NOT NULL
    ) STRICT;
  `,
}, {
  version: 16,
  sql: `
    CREATE TABLE IF NOT EXISTS yp_work_file_changes (
      change_id INTEGER PRIMARY KEY AUTOINCREMENT,
      conversation_id TEXT NOT NULL,
      pi_session_id TEXT NOT NULL,
      run_id TEXT NOT NULL,
      tool_call_id TEXT NOT NULL,
      tool_name TEXT NOT NULL CHECK(tool_name IN ('edit','write')),
      relative_path TEXT NOT NULL,
      before_content TEXT,
      before_sha256 TEXT,
      before_size INTEGER,
      before_truncated INTEGER NOT NULL DEFAULT 0,
      after_content TEXT NOT NULL,
      after_sha256 TEXT NOT NULL,
      after_size INTEGER NOT NULL,
      after_truncated INTEGER NOT NULL DEFAULT 0,
      created_at TEXT NOT NULL,
      UNIQUE(run_id, tool_call_id)
    ) STRICT;
    CREATE INDEX IF NOT EXISTS yp_work_file_changes_conversation
      ON yp_work_file_changes(conversation_id, change_id);
  `,
}];

function assertWorkSourceEventSchema(database: DatabaseSync): void {
  const column = (database.prepare('PRAGMA table_info(yp_work_turn_sources)').all() as
    Array<{ name: string; type: string }>).find((item) => item.name === 'event_id');
  const objects = database.prepare(`SELECT name FROM sqlite_master WHERE name IN
    ('yp_work_turn_sources_event_id','yp_work_turn_sources_assign_event_id',
     'yp_work_source_event_sequence')`).all() as
    Array<{ name: string }>;
  const missing = database.prepare('SELECT 1 FROM yp_work_turn_sources WHERE event_id IS NULL LIMIT 1').get();
  const sequence = database.prepare('SELECT next_id FROM yp_work_source_event_sequence WHERE id = 1')
    .get() as { next_id: number } | undefined;
  const max = database.prepare('SELECT COALESCE(MAX(event_id),0) AS id FROM yp_work_turn_sources')
    .get() as { id: number };
  if (column?.type !== 'INTEGER' || objects.length !== 3 || missing
    || !sequence || sequence.next_id < max.id) {
    throw new Error('Incompatible Work source event cursor schema.');
  }
}

function workDirectoryColumn(database: DatabaseSync): boolean {
  const columns = database.prepare('PRAGMA table_info(yp_work_conversations)').all() as
    Array<{ name: string; type: string; notnull: number }>;
  if (!columns.some((column) => column.name === 'conversation_id')) {
    throw new Error('Work conversation schema is missing; cannot repair metadata.');
  }
  const workspace = columns.find((column) => column.name === 'workspace_id');
  if (!workspace || workspace.type !== 'TEXT' || workspace.notnull !== 1) {
    throw new Error('Incompatible Work workspace_id column.');
  }
  const workingDirectory = columns.find((column) => column.name === 'working_directory');
  if (!workingDirectory) return false;
  if (workingDirectory.type !== 'TEXT' || workingDirectory.notnull !== 1) {
    throw new Error('Incompatible Work working_directory column.');
  }
  return true;
}

function ensureWorkDirectorySchema(database: DatabaseSync): void {
  if (workDirectoryColumn(database)) return;
  database.exec(`ALTER TABLE yp_work_conversations ADD COLUMN working_directory TEXT NOT NULL DEFAULT '';
    UPDATE yp_work_conversations SET working_directory = workspace_id;`);
}

const assistantHostSchemaSql = migrations.find((migration) => migration.version === 8)!.sql;
const assistantHostObjects = [
  'yp_assistant_bindings', 'yp_assistant_active_wecom', 'yp_assistant_requests',
  'yp_assistant_requests_conversation', 'yp_assistant_deliveries', 'yp_assistant_sources',
] as const;

function normalizedSchema(sql: string): string {
  return sql.replace(/\bIF NOT EXISTS\b/gi, '').replace(/\s+/g, ' ').trim().toUpperCase();
}

function assertCompatibleAssistantHostSchema(database: DatabaseSync, requireAll = false): void {
  const expected = new DatabaseSync(':memory:');
  try {
    expected.exec(assistantHostSchemaSql);
    const query = 'SELECT type, sql FROM sqlite_master WHERE name = ?';
    for (const name of assistantHostObjects) {
      const actual = database.prepare(query).get(name) as { type: string; sql: string } | undefined;
      if (!actual) {
        if (requireAll) throw new Error(`Missing Assistant Host schema object: ${name}.`);
        continue;
      }
      const reference = expected.prepare(query).get(name) as { type: string; sql: string };
      if (actual.type !== reference.type || normalizedSchema(actual.sql) !== normalizedSchema(reference.sql)) {
        throw new Error(`Incompatible Assistant Host schema object: ${name}.`);
      }
    }
  } finally {
    expected.close();
  }
}

function ensureAssistantHostSchema(database: DatabaseSync): void {
  assertCompatibleAssistantHostSchema(database);
  database.exec(assistantHostSchemaSql
    .replace(/\bCREATE TABLE\b/g, 'CREATE TABLE IF NOT EXISTS')
    .replace(/\bCREATE (UNIQUE )?INDEX\b/g, (_match, unique: string | undefined) =>
      `CREATE ${unique ?? ''}INDEX IF NOT EXISTS`));
  assertCompatibleAssistantHostSchema(database, true);
}

function applyMigrations(database: DatabaseSync): number {
  database.exec(`
    CREATE TABLE IF NOT EXISTS yp_schema_migrations (
      version INTEGER PRIMARY KEY,
      applied_at TEXT NOT NULL
    ) STRICT;
  `);
  const row = database.prepare(
    'SELECT COALESCE(MAX(version), 0) AS version FROM yp_schema_migrations',
  ).get() as { version: number };
  if (row.version > YUANPU_METADATA_SCHEMA_VERSION) {
    throw new Error(
      `MindLink metadata schema ${row.version} is newer than supported ${YUANPU_METADATA_SCHEMA_VERSION}.`,
    );
  }
  const pending = migrations.filter((migration) => migration.version > row.version);
  if (pending.length) {
    database.exec('BEGIN IMMEDIATE');
    try {
      for (const migration of pending) {
        if (migration.sql) database.exec(migration.sql);
        migration.apply?.(database);
        database.prepare(
          'INSERT INTO yp_schema_migrations(version, applied_at) VALUES (?, ?)',
        ).run(migration.version, new Date().toISOString());
        database.exec(`PRAGMA user_version = ${migration.version}`);
      }
      database.exec('COMMIT');
    } catch (error) {
      database.exec('ROLLBACK');
      throw error;
    }
  }
  if (!workDirectoryColumn(database)) throw new Error('Missing Work working_directory column.');
  const workColumns = new Set((database.prepare('PRAGMA table_info(yp_work_conversations)').all() as
    Array<{ name: string }>).map((column) => column.name));
  if (['title', 'icon_id', 'folder_id', 'sort_order', 'archived_at', 'request_id'].some((name) => !workColumns.has(name))) {
    throw new Error('Missing Work tree metadata columns.');
  }
  for (const name of ['yp_work_folders', 'yp_work_tags', 'yp_work_conversation_tags', 'yp_work_create_intents']) {
    const object = database.prepare('SELECT type FROM sqlite_master WHERE name=?').get(name) as
      { type: string } | undefined;
    if (object?.type !== 'table') throw new Error(`Missing Work tree metadata table: ${name}.`);
  }
  assertCompatibleAssistantHostSchema(database, true);
  assertWorkSourceEventSchema(database);
  const deletions = database.prepare(`SELECT type FROM sqlite_master
    WHERE name='yp_assistant_source_deletions'`).get() as { type: string } | undefined;
  if (deletions?.type !== 'table') throw new Error('Missing Assistant source deletion ledger.');
  const legacyEvents = database.prepare(`SELECT type FROM sqlite_master
    WHERE name='yp_assistant_legacy_memory_events'`).get() as { type: string } | undefined;
  if (legacyEvents?.type !== 'table') throw new Error('Missing legacy memory source ledger.');
  return YUANPU_METADATA_SCHEMA_VERSION;
}

export class YuanpuMetadataDatabase {
  readonly driver = YUANPU_SQLITE_DRIVER;
  readonly schemaVersion: number;
  readonly agentRuns: AgentRunStore;
  readonly assistantLink: AssistantLinkStore;
  readonly assistantHost: AssistantHostStore;
  readonly assistantSourceLifecycle: AssistantSourceLifecycleStore;
  readonly workConversations: WorkConversationStore;
  readonly workEvidence: WorkEvidenceStore;
  readonly workFileChanges: WorkFileChangeStore;
  readonly channels: ChannelStore;
  readonly schedules: SchedulerStore;

  constructor(private readonly database: DatabaseSync) {
    this.schemaVersion = applyMigrations(database);
    this.agentRuns = new AgentRunStore(database);
    this.assistantLink = new AssistantLinkStore(database);
    this.assistantHost = new AssistantHostStore(database);
    this.assistantSourceLifecycle = new AssistantSourceLifecycleStore(database);
    this.workConversations = new WorkConversationStore(database);
    this.workEvidence = new WorkEvidenceStore(database);
    this.workFileChanges = new WorkFileChangeStore(database);
    this.channels = new ChannelStore(database);
    this.schedules = new SchedulerStore(database);
  }

  getMetadata(key: string): string | undefined {
    const row = this.database.prepare(
      'SELECT value FROM yp_runtime_metadata WHERE key = ?',
    ).get(key) as { value: string } | undefined;
    return row?.value;
  }

  setMetadata(key: string, value: string): void {
    this.database.prepare(`
      INSERT INTO yp_runtime_metadata(key, value, updated_at) VALUES (?, ?, ?)
      ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
    `).run(key, value, new Date().toISOString());
  }

  incrementMetadataCounter(key: string): number {
    this.database.exec('BEGIN IMMEDIATE');
    try {
      const current = Number.parseInt(this.getMetadata(key) ?? '0', 10);
      const next = Number.isSafeInteger(current) ? current + 1 : 1;
      this.setMetadata(key, String(next));
      this.database.exec('COMMIT');
      return next;
    } catch (error) {
      this.database.exec('ROLLBACK');
      throw error;
    }
  }

  close(): void {
    this.database.close();
  }
}

export function openYuanpuMetadataDatabase(path: string): YuanpuMetadataDatabase {
  if (path !== ':memory:') {
    mkdirSync(dirname(path), { recursive: true, mode: 0o700 });
    try {
      if (lstatSync(path).isSymbolicLink()) {
        throw new Error('Refusing to open a symbolic link as the MindLink metadata database.');
      }
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error;
    }
    closeSync(openSync(path, 'a', 0o600));
    if (process.platform !== 'win32') chmodSync(path, 0o600);
  }
  const database = new DatabaseSync(path);
  try {
    database.exec('PRAGMA foreign_keys = ON');
    database.exec('PRAGMA busy_timeout = 5000');
    if (path !== ':memory:') database.exec('PRAGMA journal_mode = WAL');
    return new YuanpuMetadataDatabase(database);
  } catch (error) {
    database.close();
    throw error;
  }
}

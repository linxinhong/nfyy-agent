-- Trusted B/S memory tables are deliberately separate from the legacy
-- `memories` table. Legacy rows remain isolated until an explicit manifest
-- maps every row to a complete authority scope.

CREATE TABLE IF NOT EXISTS agent_runtime_memory_policies (
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    generation INTEGER NOT NULL,
    limits_json TEXT NOT NULL,
    PRIMARY KEY (namespace, project_id)
);

CREATE TABLE IF NOT EXISTS agent_runtime_memory_subjects (
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    generation INTEGER NOT NULL,
    disabled INTEGER NOT NULL CHECK (disabled IN (0, 1)),
    PRIMARY KEY (namespace, project_id, subject_id)
);

CREATE TABLE IF NOT EXISTS agent_runtime_memory_collections (
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    collection_id TEXT NOT NULL,
    physical_records INTEGER NOT NULL,
    physical_bytes INTEGER NOT NULL,
    record_identities INTEGER NOT NULL,
    candidate_batches INTEGER NOT NULL,
    proposal_epoch INTEGER NOT NULL,
    PRIMARY KEY (namespace, project_id, subject_id, collection_id)
);

CREATE TABLE IF NOT EXISTS agent_runtime_memory_sources (
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    collection_id TEXT NOT NULL,
    source_ref TEXT NOT NULL,
    source_version TEXT NOT NULL,
    digest TEXT NOT NULL,
    source_epoch INTEGER NOT NULL,
    expires_at_ns INTEGER NOT NULL,
    verifier TEXT NOT NULL,
    revoked INTEGER NOT NULL CHECK (revoked IN (0, 1)),
    PRIMARY KEY (namespace, project_id, subject_id, collection_id, source_ref)
);

CREATE TABLE IF NOT EXISTS agent_runtime_memory_records (
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    collection_id TEXT NOT NULL,
    memory_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    subject_generation INTEGER NOT NULL,
    policy_generation INTEGER NOT NULL,
    proposal_epoch INTEGER NOT NULL,
    kind TEXT NOT NULL,
    content TEXT NOT NULL,
    topics_json TEXT NOT NULL,
    status TEXT NOT NULL,
    source_ref TEXT NOT NULL,
    source_version TEXT NOT NULL,
    source_epoch INTEGER NOT NULL,
    producer TEXT NOT NULL,
    policy_version TEXT NOT NULL,
    decision_actor TEXT NOT NULL,
    decision_basis TEXT NOT NULL,
    decided_at_ns INTEGER NOT NULL,
    created_at_ns INTEGER NOT NULL,
    updated_at_ns INTEGER NOT NULL,
    expires_at_ns INTEGER NOT NULL,
    PRIMARY KEY (namespace, project_id, subject_id, collection_id, memory_id)
);

CREATE INDEX IF NOT EXISTS idx_memory_records_scope_visible_keyset
ON agent_runtime_memory_records (
    namespace, project_id, subject_id, collection_id,
    subject_generation, status, updated_at_ns DESC, memory_id DESC
);

CREATE INDEX IF NOT EXISTS idx_memory_records_scope_expiry
ON agent_runtime_memory_records (
    namespace, project_id, subject_id, collection_id,
    subject_generation, expires_at_ns, updated_at_ns DESC, memory_id DESC
);

CREATE INDEX IF NOT EXISTS idx_memory_records_scope_kind_keyset
ON agent_runtime_memory_records (
    namespace, project_id, subject_id, collection_id,
    subject_generation, status, kind, updated_at_ns DESC, memory_id DESC
);

CREATE TABLE IF NOT EXISTS agent_runtime_memory_record_topics (
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    collection_id TEXT NOT NULL,
    memory_id TEXT NOT NULL,
    topic TEXT NOT NULL,
    PRIMARY KEY (namespace, project_id, subject_id, collection_id, memory_id, topic),
    FOREIGN KEY (namespace, project_id, subject_id, collection_id, memory_id)
        REFERENCES agent_runtime_memory_records(namespace, project_id, subject_id, collection_id, memory_id)
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_memory_record_topics_scope_topic
ON agent_runtime_memory_record_topics (
    namespace, project_id, subject_id, collection_id, topic, memory_id
);

CREATE TABLE IF NOT EXISTS agent_runtime_memory_candidate_batches (
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    collection_id TEXT NOT NULL,
    source_ref TEXT NOT NULL,
    source_version TEXT NOT NULL,
    producer TEXT NOT NULL,
    model_config_version TEXT NOT NULL,
    subject_generation INTEGER NOT NULL,
    policy_generation INTEGER NOT NULL,
    proposal_epoch INTEGER NOT NULL,
    command_digest BLOB NOT NULL,
    result_json TEXT NOT NULL,
    PRIMARY KEY (
        namespace, project_id, subject_id, collection_id,
        source_ref, source_version, producer, model_config_version,
        subject_generation, policy_generation, proposal_epoch
    )
);

CREATE TABLE IF NOT EXISTS agent_runtime_memory_purge_jobs (
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    owner_token TEXT NOT NULL,
    lease_epoch INTEGER NOT NULL,
    lease_expires_at_ns INTEGER NOT NULL,
    selection_cutoff_ns INTEGER NOT NULL,
    purge_before_generation INTEGER NOT NULL,
    cursor TEXT NOT NULL,
    PRIMARY KEY (namespace, project_id, subject_id)
);

CREATE TABLE IF NOT EXISTS agent_runtime_memory_receipts (
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    collection_id TEXT NOT NULL,
    action TEXT NOT NULL,
    operation_id TEXT NOT NULL,
    command_digest BLOB NOT NULL,
    expires_at_ns INTEGER NOT NULL,
    result_json TEXT NOT NULL,
    PRIMARY KEY (namespace, project_id, subject_id, collection_id, action, operation_id)
);

CREATE INDEX IF NOT EXISTS idx_memory_receipts_scope_expiry
ON agent_runtime_memory_receipts (
    namespace, project_id, subject_id, collection_id, expires_at_ns
);

CREATE TABLE IF NOT EXISTS agent_runtime_memory_audits (
    audit_id INTEGER PRIMARY KEY AUTOINCREMENT,
    operation_id TEXT NOT NULL,
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    collection_id TEXT NOT NULL,
    action TEXT NOT NULL,
    actor TEXT NOT NULL,
    issuer TEXT NOT NULL,
    memory_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    generation INTEGER NOT NULL,
    occurred_at_ns INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_memory_audits_scope_time
ON agent_runtime_memory_audits (
    namespace, project_id, subject_id, collection_id, occurred_at_ns, audit_id
);

CREATE TABLE IF NOT EXISTS agent_runtime_memory_migration_manifests (
    manifest_hash TEXT PRIMARY KEY,
    source_name TEXT NOT NULL,
    manifest_json TEXT NOT NULL,
    next_entry INTEGER NOT NULL,
    imported_count INTEGER NOT NULL,
    completed INTEGER NOT NULL CHECK (completed IN (0, 1)),
    created_at_ns INTEGER NOT NULL,
    updated_at_ns INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS agent_runtime_memory_migration_items (
    manifest_hash TEXT NOT NULL,
    source_name TEXT NOT NULL,
    source_memory_id TEXT NOT NULL,
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    collection_id TEXT NOT NULL,
    imported_memory_id TEXT NOT NULL,
    outcome TEXT NOT NULL,
    PRIMARY KEY (manifest_hash, source_memory_id),
    UNIQUE (source_name, source_memory_id),
    FOREIGN KEY (manifest_hash) REFERENCES agent_runtime_memory_migration_manifests(manifest_hash) ON DELETE CASCADE
);

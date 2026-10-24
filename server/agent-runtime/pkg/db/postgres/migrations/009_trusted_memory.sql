-- Trusted, fully-scoped Memory storage.  The legacy agent_runtime_memories
-- table is intentionally left untouched and is only read by an explicit
-- manifest migration.

CREATE TABLE IF NOT EXISTS agent_runtime_memory_policies (
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    generation BIGINT NOT NULL CHECK (generation > 0),
    limits JSONB NOT NULL,
    PRIMARY KEY (namespace, project_id)
);

CREATE TABLE IF NOT EXISTS agent_runtime_memory_subjects (
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    generation BIGINT NOT NULL CHECK (generation > 0),
    disabled BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (namespace, project_id, subject_id),
    FOREIGN KEY (namespace, project_id)
        REFERENCES agent_runtime_memory_policies(namespace, project_id)
);

CREATE TABLE IF NOT EXISTS agent_runtime_memory_collections (
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    collection_id TEXT NOT NULL,
    records_count INTEGER NOT NULL DEFAULT 0 CHECK (records_count >= 0),
    content_bytes BIGINT NOT NULL DEFAULT 0 CHECK (content_bytes >= 0),
    record_identities INTEGER NOT NULL DEFAULT 0 CHECK (record_identities >= 0),
    candidate_batches INTEGER NOT NULL DEFAULT 0 CHECK (candidate_batches >= 0),
    proposal_epoch BIGINT NOT NULL DEFAULT 0 CHECK (proposal_epoch >= 0),
    PRIMARY KEY (namespace, project_id, subject_id, collection_id),
    FOREIGN KEY (namespace, project_id, subject_id)
        REFERENCES agent_runtime_memory_subjects(namespace, project_id, subject_id)
);

CREATE TABLE IF NOT EXISTS agent_runtime_memory_sources (
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    collection_id TEXT NOT NULL,
    source_ref TEXT NOT NULL,
    source_version TEXT NOT NULL,
    source_digest TEXT NOT NULL,
    source_epoch BIGINT NOT NULL CHECK (source_epoch > 0),
    expires_at TIMESTAMPTZ NOT NULL,
    verifier TEXT NOT NULL,
    revoked BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (namespace, project_id, subject_id, collection_id, source_ref),
    FOREIGN KEY (namespace, project_id, subject_id, collection_id)
        REFERENCES agent_runtime_memory_collections(namespace, project_id, subject_id, collection_id)
);

CREATE TABLE IF NOT EXISTS agent_runtime_memory_records (
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    collection_id TEXT NOT NULL,
    memory_id TEXT NOT NULL,
    revision BIGINT NOT NULL CHECK (revision > 0),
    subject_generation BIGINT NOT NULL CHECK (subject_generation > 0),
    policy_generation BIGINT NOT NULL CHECK (policy_generation > 0),
    proposal_epoch BIGINT NOT NULL DEFAULT 0 CHECK (proposal_epoch >= 0),
    kind TEXT NOT NULL CHECK (kind IN ('user_statement','preference','verified_fact')),
    content TEXT NOT NULL,
    topics JSONB NOT NULL DEFAULT '[]'::jsonb,
    status TEXT NOT NULL CHECK (status IN ('candidate','active','retracted')),
    source_ref TEXT NOT NULL DEFAULT '',
    source_version TEXT NOT NULL DEFAULT '',
    source_epoch BIGINT NOT NULL DEFAULT 0 CHECK (source_epoch >= 0),
    producer TEXT NOT NULL,
    policy_version TEXT NOT NULL,
    decision_actor TEXT NOT NULL DEFAULT '',
    decision_basis TEXT NOT NULL DEFAULT '',
    decided_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (namespace, project_id, subject_id, collection_id, memory_id),
    FOREIGN KEY (namespace, project_id, subject_id, collection_id)
        REFERENCES agent_runtime_memory_collections(namespace, project_id, subject_id, collection_id)
);

CREATE INDEX IF NOT EXISTS idx_runtime_memory_records_visible
    ON agent_runtime_memory_records (
        namespace, project_id, subject_id, collection_id,
        subject_generation, status, updated_at DESC, memory_id DESC
    )
    INCLUDE (revision, policy_generation, proposal_epoch, kind, expires_at,
             source_ref, source_version, source_epoch, created_at)
    WHERE content <> '';

CREATE TABLE IF NOT EXISTS agent_runtime_memory_candidate_batches (
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    collection_id TEXT NOT NULL,
    source_ref TEXT NOT NULL,
    source_version TEXT NOT NULL,
    producer TEXT NOT NULL,
    model_config_version TEXT NOT NULL,
    subject_generation BIGINT NOT NULL,
    policy_generation BIGINT NOT NULL,
    proposal_epoch BIGINT NOT NULL,
    command_digest BYTEA NOT NULL,
    result JSONB NOT NULL,
    PRIMARY KEY (
        namespace, project_id, subject_id, collection_id,
        source_ref, source_version, producer, model_config_version,
        subject_generation, policy_generation, proposal_epoch
    ),
    FOREIGN KEY (namespace, project_id, subject_id, collection_id)
        REFERENCES agent_runtime_memory_collections(namespace, project_id, subject_id, collection_id)
);

CREATE TABLE IF NOT EXISTS agent_runtime_memory_purge_jobs (
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    owner_token TEXT NOT NULL,
    lease_epoch BIGINT NOT NULL CHECK (lease_epoch > 0),
    lease_expires_at TIMESTAMPTZ NOT NULL,
    selection_cutoff TIMESTAMPTZ NOT NULL,
    purge_before_generation BIGINT NOT NULL CHECK (purge_before_generation > 0),
    cursor TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (namespace, project_id, subject_id),
    FOREIGN KEY (namespace, project_id, subject_id)
        REFERENCES agent_runtime_memory_subjects(namespace, project_id, subject_id)
);

CREATE TABLE IF NOT EXISTS agent_runtime_memory_receipts (
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    collection_id TEXT NOT NULL DEFAULT '',
    action TEXT NOT NULL,
    operation_id TEXT NOT NULL,
    command_digest BYTEA NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    result JSONB NOT NULL,
    PRIMARY KEY (namespace, project_id, subject_id, collection_id, action, operation_id),
    FOREIGN KEY (namespace, project_id, subject_id)
        REFERENCES agent_runtime_memory_subjects(namespace, project_id, subject_id)
);

CREATE INDEX IF NOT EXISTS idx_runtime_memory_receipts_expiry
    ON agent_runtime_memory_receipts(namespace, project_id, subject_id, collection_id, expires_at);

CREATE TABLE IF NOT EXISTS agent_runtime_memory_audits (
    audit_id BIGSERIAL PRIMARY KEY,
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    collection_id TEXT NOT NULL DEFAULT '',
    operation_id TEXT NOT NULL,
    action TEXT NOT NULL,
    actor TEXT NOT NULL,
    issuer TEXT NOT NULL,
    memory_id TEXT NOT NULL DEFAULT '',
    revision BIGINT NOT NULL DEFAULT 0,
    generation BIGINT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    UNIQUE (namespace, project_id, subject_id, collection_id, action, operation_id)
);

CREATE INDEX IF NOT EXISTS idx_runtime_memory_audits_scope_time
    ON agent_runtime_memory_audits(namespace, project_id, subject_id, collection_id, occurred_at DESC);

CREATE TABLE IF NOT EXISTS agent_runtime_memory_legacy_migrations (
    source_id TEXT PRIMARY KEY,
    manifest_hash TEXT NOT NULL,
    next_index INTEGER NOT NULL DEFAULT 0 CHECK (next_index >= 0),
    imported_count INTEGER NOT NULL DEFAULT 0 CHECK (imported_count >= 0),
    completed BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS agent_runtime_memory_legacy_assignments (
    source_table TEXT NOT NULL,
    legacy_memory_id TEXT NOT NULL,
    source_id TEXT NOT NULL,
    manifest_hash TEXT NOT NULL,
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    collection_id TEXT NOT NULL,
    assigned_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (source_table, legacy_memory_id),
    FOREIGN KEY (source_id)
        REFERENCES agent_runtime_memory_legacy_migrations(source_id)
);

CREATE INDEX IF NOT EXISTS idx_runtime_memory_legacy_assignments_manifest
    ON agent_runtime_memory_legacy_assignments(source_id, manifest_hash, legacy_memory_id);

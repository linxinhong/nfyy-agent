CREATE TABLE IF NOT EXISTS agent_runtime_memory_lifecycle_jobs (
    task_id TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('forget','retract','retract_source','expiry')),
    namespace TEXT NOT NULL,
    project_id TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    collection_id TEXT NOT NULL DEFAULT '',
    source_ref TEXT NOT NULL DEFAULT '',
    source_epoch BIGINT NOT NULL DEFAULT 0,
    memory_id TEXT NOT NULL DEFAULT '',
    expected_revision BIGINT NOT NULL DEFAULT 0,
    purge_before_generation BIGINT NOT NULL CHECK (purge_before_generation > 0),
    selection_cutoff TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending','leased','failed','completed')),
    owner_token TEXT NOT NULL DEFAULT '',
    lease_epoch BIGINT NOT NULL DEFAULT 0,
    lease_expires_at TIMESTAMPTZ,
    cursor_collection_id TEXT NOT NULL DEFAULT '',
    cursor_memory_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    last_failure_code TEXT NOT NULL DEFAULT '',
    CHECK (length(task_id) <= 128),
    CHECK (length(owner_token) <= 256),
    CHECK (length(last_failure_code) <= 128),
    FOREIGN KEY (namespace, project_id, subject_id)
        REFERENCES agent_runtime_memory_subjects(namespace, project_id, subject_id)
);

CREATE INDEX IF NOT EXISTS idx_runtime_memory_lifecycle_claim
ON agent_runtime_memory_lifecycle_jobs(status, lease_expires_at, created_at, task_id);

CREATE INDEX IF NOT EXISTS idx_runtime_memory_records_expiry_discovery
ON agent_runtime_memory_records(expires_at, namespace, project_id, subject_id, collection_id, memory_id, revision)
WHERE content <> '';

CREATE INDEX IF NOT EXISTS idx_runtime_memory_records_lifecycle_subject
ON agent_runtime_memory_records(namespace, project_id, subject_id, collection_id, memory_id)
WHERE content <> '';

CREATE INDEX IF NOT EXISTS idx_runtime_memory_records_lifecycle_source
ON agent_runtime_memory_records(namespace, project_id, subject_id, collection_id, source_ref, source_epoch, memory_id)
WHERE content <> '' AND source_ref <> '';

CREATE INDEX IF NOT EXISTS idx_runtime_memory_records_lifecycle_reclaim
ON agent_runtime_memory_records(updated_at, namespace, project_id, subject_id, collection_id, memory_id)
WHERE content = '' AND status <> 'candidate';

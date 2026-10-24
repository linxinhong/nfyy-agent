-- Migration 2026080904: dispatch queue, attempts, leases, cancellation,
-- and outbox. Forward-only.

CREATE TABLE IF NOT EXISTS agent_runtime_dispatch_attempts (
    attempt_id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL,
    namespace TEXT NOT NULL,
    state TEXT NOT NULL,
    fencing_token BIGINT NOT NULL,
    attempt_number INTEGER NOT NULL,
    idempotency_key TEXT,
    worker_id TEXT,
    started_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ,
    error TEXT
);
CREATE INDEX IF NOT EXISTS idx_dispatch_attempts_namespace_run
    ON agent_runtime_dispatch_attempts(namespace, run_id);

CREATE TABLE IF NOT EXISTS agent_runtime_outbox (
    event_id TEXT PRIMARY KEY,
    namespace TEXT NOT NULL,
    aggregate_id TEXT NOT NULL,
    sequence BIGINT NOT NULL,
    type TEXT NOT NULL,
    payload JSONB,
    claimed_by TEXT,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_outbox_namespace_claimed
    ON agent_runtime_outbox(namespace, claimed_by);

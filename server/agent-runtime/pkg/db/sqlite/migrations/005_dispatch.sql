-- Migration 2026080904: dispatch queue, attempts, leases, cancellation,
-- and outbox. Forward-only.

CREATE TABLE IF NOT EXISTS dispatch_attempts (
    attempt_id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL,
    namespace TEXT NOT NULL,
    state TEXT NOT NULL,
    fencing_token INTEGER NOT NULL,
    attempt_number INTEGER NOT NULL,
    idempotency_key TEXT,
    worker_id TEXT,
    started_at TEXT NOT NULL,
    finished_at TEXT,
    error TEXT
);
CREATE INDEX IF NOT EXISTS idx_dispatch_attempts_namespace_run ON dispatch_attempts(namespace, run_id);

CREATE TABLE IF NOT EXISTS outbox_events (
    event_id TEXT PRIMARY KEY,
    namespace TEXT NOT NULL,
    aggregate_id TEXT NOT NULL,
    sequence INTEGER NOT NULL,
    type TEXT NOT NULL,
    payload TEXT,
    claimed_by TEXT,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_outbox_namespace_claimed ON outbox_events(namespace, claimed_by);

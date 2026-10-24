-- Migration 2026080904 (lease): cross-instance run ownership leases with
-- per-run monotonic fencing tokens. Forward-only.

CREATE TABLE IF NOT EXISTS dispatch_leases (
    run_id TEXT NOT NULL,
    namespace TEXT NOT NULL,
    worker_id TEXT NOT NULL,
    fencing_token INTEGER NOT NULL,
    lease_expires_at TEXT NOT NULL,
    PRIMARY KEY (namespace, run_id)
);

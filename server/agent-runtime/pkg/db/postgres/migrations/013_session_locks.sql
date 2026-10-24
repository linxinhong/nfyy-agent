-- Migration 013 (session locks): leased per-session locks that serialize
-- runs on one session across replicas. A lock whose holder stops renewing
-- expires and can be taken over.
CREATE TABLE IF NOT EXISTS agent_runtime_session_locks (
    lock_key TEXT PRIMARY KEY,
    holder TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);

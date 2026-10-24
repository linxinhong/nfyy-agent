-- Migration 012 (cluster admission): leased run slots shared by every
-- replica. A slot whose holder stops renewing expires and is reclaimed.
CREATE TABLE IF NOT EXISTS agent_runtime_admission_slots (
    slot_id TEXT PRIMARY KEY,
    tenant TEXT NOT NULL DEFAULT '',
    holder TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_runtime_admission_slots_tenant
    ON agent_runtime_admission_slots(tenant, expires_at);

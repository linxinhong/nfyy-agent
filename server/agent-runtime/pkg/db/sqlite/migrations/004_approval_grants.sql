-- Migration 2026080903: approval grants partitioned by namespace.
-- Forward-only: down migration requires restoring a pre-migration backup.

CREATE TABLE IF NOT EXISTS approval_grants (
    grant_id TEXT PRIMARY KEY,
    namespace TEXT NOT NULL,
    plan_id TEXT NOT NULL,
    plan_version TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    arguments_hash TEXT NOT NULL,
    registry_revision TEXT NOT NULL,
    descriptor_policy_revision TEXT NOT NULL,
    authorization_policy_revision TEXT NOT NULL,
    requester TEXT NOT NULL,
    approver TEXT NOT NULL,
    decision TEXT NOT NULL,
    nonce TEXT NOT NULL,
    issued_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    UNIQUE (namespace, plan_id)
);
CREATE INDEX IF NOT EXISTS idx_approval_grants_namespace ON approval_grants(namespace, plan_id);

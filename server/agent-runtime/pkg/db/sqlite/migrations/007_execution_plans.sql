-- Migration 2026080902: durable execution plans partitioned by namespace.
-- Forward-only.

CREATE TABLE IF NOT EXISTS execution_plans (
    plan_id TEXT NOT NULL,
    namespace TEXT NOT NULL,
    version TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    arguments TEXT NOT NULL,
    arguments_hash TEXT NOT NULL,
    registry_revision TEXT NOT NULL,
    descriptor_policy_revision TEXT NOT NULL,
    authorization_policy_revision TEXT NOT NULL,
    authorization_hash TEXT NOT NULL,
    identity_binding_hash TEXT NOT NULL,
    principal_json TEXT NOT NULL,
    context_json TEXT NOT NULL,
    idempotency_key TEXT,
    approval_required INTEGER NOT NULL DEFAULT 0,
    approval_kind TEXT,
    prepared_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    PRIMARY KEY(namespace, plan_id)
);

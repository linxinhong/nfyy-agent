-- Migration 2026080902: durable execution plans partitioned by namespace.
-- Forward-only: down migration requires restoring a pre-migration backup.

CREATE TABLE IF NOT EXISTS agent_runtime_execution_plans (
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
    approval_required BOOLEAN NOT NULL DEFAULT FALSE,
    approval_kind TEXT,
    prepared_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (namespace, plan_id)
);

-- Migration 2026080901: add namespace partition column to runtime tables.
-- Forward-only: down migration requires restoring a pre-migration backup
-- with the previous binary.

ALTER TABLE agent_runtime_sessions ADD COLUMN namespace TEXT NOT NULL DEFAULT 'legacy';
ALTER TABLE agent_runtime_runs ADD COLUMN namespace TEXT NOT NULL DEFAULT 'legacy';

CREATE INDEX IF NOT EXISTS idx_runtime_sessions_namespace
    ON agent_runtime_sessions(namespace, kind, component_id);
CREATE INDEX IF NOT EXISTS idx_runtime_runs_namespace
    ON agent_runtime_runs(namespace, session_id);

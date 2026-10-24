-- Migration 2026080901: add namespace partition column to runtime tables.
-- Forward-only: down migration requires restoring a pre-migration backup
-- with the previous binary.

ALTER TABLE sessions ADD COLUMN namespace TEXT NOT NULL DEFAULT 'legacy';
ALTER TABLE runs ADD COLUMN namespace TEXT NOT NULL DEFAULT 'legacy';
ALTER TABLE run_messages ADD COLUMN namespace TEXT NOT NULL DEFAULT 'legacy';
ALTER TABLE team_sessions ADD COLUMN namespace TEXT NOT NULL DEFAULT 'legacy';
ALTER TABLE team_runs ADD COLUMN namespace TEXT NOT NULL DEFAULT 'legacy';
ALTER TABLE team_run_messages ADD COLUMN namespace TEXT NOT NULL DEFAULT 'legacy';
ALTER TABLE workflow_sessions ADD COLUMN namespace TEXT NOT NULL DEFAULT 'legacy';
ALTER TABLE workflow_runs ADD COLUMN namespace TEXT NOT NULL DEFAULT 'legacy';
ALTER TABLE workflow_run_messages ADD COLUMN namespace TEXT NOT NULL DEFAULT 'legacy';
ALTER TABLE memories ADD COLUMN namespace TEXT NOT NULL DEFAULT 'legacy';

CREATE INDEX IF NOT EXISTS idx_sessions_namespace ON sessions(namespace, session_id);
CREATE INDEX IF NOT EXISTS idx_runs_namespace ON runs(namespace, session_id);
CREATE INDEX IF NOT EXISTS idx_memories_namespace ON memories(namespace, memory_id);

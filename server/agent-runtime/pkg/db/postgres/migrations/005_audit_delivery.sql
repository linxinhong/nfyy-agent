-- Migration 2026080905 (reserved): audit delivery state. Forward-only.
ALTER TABLE agent_runtime_outbox ADD COLUMN delivered_at TIMESTAMPTZ;

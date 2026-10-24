-- Migration 011 (run leases): lets run.ExpireStaleRuns find leased running
-- runs without scanning every run. Partial, so it only holds in-flight runs.
--
-- Migrations run inside a transaction, so this cannot use CONCURRENTLY and
-- blocks writes to agent_runtime_runs while it builds. On large tables,
-- create it beforehand outside a transaction with the same name:
--   CREATE INDEX CONCURRENTLY idx_runtime_runs_running_lease
--     ON agent_runtime_runs(updated_at)
--     WHERE payload->>'status' = 'running' AND payload ? 'lease_timeout';
-- and this statement becomes a no-op.

CREATE INDEX IF NOT EXISTS idx_runtime_runs_running_lease
    ON agent_runtime_runs(updated_at)
    WHERE payload->>'status' = 'running' AND payload ? 'lease_timeout';

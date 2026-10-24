-- Migration 2026090601 (managed run fencing): preserve an independent Run
-- content revision for atomic status CAS. Existing rows start at revision 1.

ALTER TABLE agent_runtime_runs
    ADD COLUMN IF NOT EXISTS revision BIGINT NOT NULL DEFAULT 1;

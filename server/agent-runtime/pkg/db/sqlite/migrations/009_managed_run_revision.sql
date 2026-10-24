-- Migration 2026090601 (managed run fencing): preserve an independent Run
-- content revision for atomic status CAS. Existing rows start at revision 1.

ALTER TABLE runs ADD COLUMN revision INTEGER NOT NULL DEFAULT 1;
ALTER TABLE team_runs ADD COLUMN revision INTEGER NOT NULL DEFAULT 1;
ALTER TABLE workflow_runs ADD COLUMN revision INTEGER NOT NULL DEFAULT 1;

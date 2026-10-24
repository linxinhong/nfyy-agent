ALTER TABLE workflow_runs ADD COLUMN workflow_payload TEXT;
ALTER TABLE runs ADD COLUMN parent_run_id TEXT;
ALTER TABLE runs ADD COLUMN node_id TEXT;
ALTER TABLE team_runs ADD COLUMN parent_run_id TEXT;
ALTER TABLE team_runs ADD COLUMN node_id TEXT;

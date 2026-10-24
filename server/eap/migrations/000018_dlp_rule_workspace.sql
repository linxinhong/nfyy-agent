-- TASK-031: workspace-scoped DLP rules. '*' (default) keeps every existing
-- rule global; loading merges global rules with the requesting workspace's.

ALTER TABLE dlp_rules ADD COLUMN IF NOT EXISTS workspace_id text NOT NULL DEFAULT '*';
CREATE INDEX IF NOT EXISTS dlp_rules_workspace_idx ON dlp_rules (workspace_id);

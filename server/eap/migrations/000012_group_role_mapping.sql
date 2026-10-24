-- TASK-025: the IdP-group mapping carries not just the workspace but the
-- roles that group grants in it (ADR-0009: groups are claims mapped through
-- an explicit operator table — never client input). Default preserves the
-- historic hardcoded behavior.

ALTER TABLE idp_group_workspaces ADD COLUMN roles text[] NOT NULL DEFAULT '{developer}';

-- TASK-035 / ADR-0024: optional server-side workspace binding on employee
-- API keys. NULL keeps a key workspace-unbound: selection stays header-only
-- and fails closed without a header, exactly as before. The binding only
-- nominates a workspace — membership is still resolved on every request.

ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS workspace_id text;
CREATE INDEX IF NOT EXISTS api_keys_workspace_idx ON api_keys (workspace_id);

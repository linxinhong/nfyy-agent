-- TASK-028: reversible DLP redaction. Sensitive values matched by
-- tokenize-class rules are replaced with [rule:token] pseudonyms before
-- content reaches models or indexes; originals live ONLY here. Reveal is a
-- governed, audited API call scoped to the owning workspace. Deterministic:
-- the same value in the same workspace keeps one stable token.

CREATE TABLE IF NOT EXISTS dlp_tokens (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id   text REFERENCES workspaces(id), -- NULL = created on a path without workspace context
    rule_name      text NOT NULL,
    token          text NOT NULL,
    original_value text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, rule_name, original_value),
    UNIQUE (token)
);
CREATE INDEX IF NOT EXISTS dlp_tokens_workspace_idx ON dlp_tokens (workspace_id, token);

-- TASK-010: versioned Policy and DLP control data (ADR-0010).
-- Effects are canonical: allow | allow_with_redaction | local_model_only |
-- require_approval | deny. Rule ids are stable so decisions are reproducible
-- and auditable against a specific version.

ALTER TABLE providers ADD COLUMN IF NOT EXISTS local_only boolean NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS policy_rules (
    id           bigserial PRIMARY KEY,
    rule_id      text NOT NULL UNIQUE,
    workspace_id text NOT NULL DEFAULT '*',   -- '*' matches any workspace
    model_alias  text NOT NULL DEFAULT '*',   -- '*' matches any alias
    effect       text NOT NULL,
    priority     integer NOT NULL DEFAULT 100,
    enabled      boolean NOT NULL DEFAULT true,
    version      integer NOT NULL DEFAULT 1,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS policy_rules_lookup_idx ON policy_rules (workspace_id, model_alias, priority);

CREATE TABLE IF NOT EXISTS dlp_rules (
    name        text PRIMARY KEY,
    pattern     text NOT NULL,
    action      text NOT NULL DEFAULT 'redact',  -- deny | redact
    replacement text NOT NULL DEFAULT '[REDACTED]',
    enabled     boolean NOT NULL DEFAULT true,
    version     integer NOT NULL DEFAULT 1,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- Custom dictionaries: terms compile into one case-insensitive alternation.
CREATE TABLE IF NOT EXISTS dlp_dictionary_terms (
    rule_name text NOT NULL REFERENCES dlp_rules(name),
    term      text NOT NULL,
    PRIMARY KEY (rule_name, term)
);

INSERT INTO dlp_rules (name, pattern, action, replacement) VALUES
    ('openai_key',   'sk-[A-Za-z0-9]{20,}',        'deny',   '[REDACTED:openai_key]'),
    ('eap_api_key',  'agk_(live|test)_[A-Za-z0-9]{16,}', 'deny', '[REDACTED:eap_api_key]'),
    ('bearer_token', '(?i)bearer\s+[A-Za-z0-9\-._~+/]{20,}', 'deny', '[REDACTED:bearer_token]'),
    ('password_field', '(?i)(password|passwd|pwd)\s*[:=]\s*\S{4,}', 'redact', '[REDACTED:password]'),
    ('private_key',  '(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----', 'deny', '[REDACTED:private_key]'),
    ('db_conn_string', '(?i)(postgres|postgresql|mysql|mongodb)://[^\s\"]+:[^\s\"]+@', 'deny', '[REDACTED:db_conn_string]'),
    ('internal_phone', '\b1[3-9]\d{9}\b', 'redact', '[REDACTED:phone]'),
    ('internal_ipv4', '\b(10|172\.(1[6-9]|2\d|3[01])|192\.168)\.\d{1,3}\.\d{1,3}\b', 'redact', '[REDACTED:internal_ip]')
ON CONFLICT DO NOTHING;

INSERT INTO dlp_dictionary_terms (rule_name, term) VALUES
    ('internal_phone', '内部手机号')
ON CONFLICT DO NOTHING;

INSERT INTO policy_rules (rule_id, workspace_id, model_alias, effect, priority) VALUES
    ('pol-daily-redact',  'secops', 'daily-fast',  'allow_with_redaction', 100),
    ('pol-coding-local',  'secops', 'coding-high', 'local_model_only',     100),
    ('pol-deny-demo',     '*',      'restricted-*','deny',                 100)
ON CONFLICT DO NOTHING;

UPDATE providers SET local_only = true WHERE name = 'demo-backup';

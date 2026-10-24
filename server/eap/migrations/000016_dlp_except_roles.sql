-- TASK-029: subject-aware DLP. Roles listed in except_roles see ORIGINALS on
-- employee-facing surfaces (knowledge results, LLM responses, MCP responses,
-- reveal); provider-facing egress never applies exemptions.

ALTER TABLE dlp_rules ADD COLUMN IF NOT EXISTS except_roles text[] NOT NULL DEFAULT '{}';

-- TASK-001 baseline: migration bookkeeping only.
-- Business schemas (identity, api keys, workspaces, MCP registry, knowledge)
-- arrive with their owning slices and must be added as new numbered files.
-- PostgreSQL runs this file on first `make dev-up` via docker-entrypoint-initdb.d.

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    text PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version)
VALUES ('000001_baseline')
ON CONFLICT DO NOTHING;

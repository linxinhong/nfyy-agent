-- TASK-004 MCP slice: governed tool registry and server-side credential
-- references. Tools are workspace-scoped: discovery and execution apply the
-- same membership filter (xingding-mcpa semantics, ADR-0005).

CREATE TABLE IF NOT EXISTS mcp_tools (
    name           text PRIMARY KEY,
    server         text NOT NULL,
    description    text NOT NULL DEFAULT '',
    risk_level     text NOT NULL,
    workspaces     text[] NOT NULL,
    endpoint_url   text NOT NULL,
    endpoint_method text NOT NULL DEFAULT 'GET',
    credential_ref text,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS credential_refs (
    ref        text PRIMARY KEY,
    kind       text NOT NULL DEFAULT 'env',
    created_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO credential_refs (ref, kind) VALUES ('env:EAP_MCP_GIT_TOKEN', 'env')
ON CONFLICT DO NOTHING;

-- Fixtures required by TASK-004: one Git read tool and one read-only
-- internal-data tool, both scoped to the secops workspace. Endpoints are
-- internal placeholders wired to the executing environment; the agent can
-- never choose or see them.
INSERT INTO mcp_tools (name, server, description, risk_level, workspaces, endpoint_url, endpoint_method, credential_ref) VALUES
    ('git_read_file',    'gitlab', 'Read a file from an authorized repository.', 'R1',
     ARRAY['secops'], 'http://eap-internal-gitstub.invalid/repos/{repo}/contents/{path}', 'GET', 'env:EAP_MCP_GIT_TOKEN'),
    ('internal_data_read', 'eap',  'Read-only query over the internal data fixture.', 'R1',
     ARRAY['secops'], 'http://eap-internal-data.invalid/query', 'GET', NULL)
ON CONFLICT DO NOTHING;

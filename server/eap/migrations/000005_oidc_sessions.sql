-- TASK-009 production OIDC boundary: gateway-side sessions (the browser
-- holds only the opaque session id in a HttpOnly cookie) and single-use
-- OIDC flow states. No OIDC token material is persisted beyond the minimal
-- claims snapshot needed to rebuild the Principal.

CREATE TABLE IF NOT EXISTS portal_sessions (
    session_id   text PRIMARY KEY,
    user_id      text NOT NULL REFERENCES users(id),
    email        text NOT NULL,
    display_name text NOT NULL,
    idp_subject  text NOT NULL,
    workspaces   text[] NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    revoked_at   timestamptz
);
CREATE INDEX IF NOT EXISTS portal_sessions_expiry_idx ON portal_sessions (expires_at);

CREATE TABLE IF NOT EXISTS oidc_flow_states (
    state        text PRIMARY KEY,
    nonce        text NOT NULL,
    code_verifier text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Explicit IdP group -> internal workspace mapping. Group names are claims,
-- never resource ids (ADR-0009).
CREATE TABLE IF NOT EXISTS idp_group_workspaces (
    idp_group    text NOT NULL,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    PRIMARY KEY (idp_group, workspace_id)
);

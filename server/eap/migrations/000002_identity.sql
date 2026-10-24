-- TASK-003 identity slice: organizations, departments, users, workspaces and
-- memberships, employee API keys, internal-token signing keys, audit events.

CREATE TABLE IF NOT EXISTS organizations (
    id         text PRIMARY KEY,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS departments (
    id              text PRIMARY KEY,
    organization_id text NOT NULL REFERENCES organizations(id),
    name            text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS users (
    id              text PRIMARY KEY DEFAULT 'usr_' || substr(replace(gen_random_uuid()::text, '-', ''), 1, 16),
    idp_subject     text NOT NULL UNIQUE,
    email           text NOT NULL,
    display_name    text NOT NULL,
    organization_id text NOT NULL REFERENCES organizations(id),
    department_id   text REFERENCES departments(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS users_email_lower_idx ON users (lower(email));

CREATE TABLE IF NOT EXISTS workspaces (
    id              text PRIMARY KEY,
    organization_id text NOT NULL REFERENCES organizations(id),
    name            text NOT NULL,
    is_default      boolean NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS workspace_members (
    workspace_id text NOT NULL REFERENCES workspaces(id),
    user_id      text NOT NULL REFERENCES users(id),
    roles        text[] NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, user_id)
);

CREATE TABLE IF NOT EXISTS api_keys (
    key_id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    key_hash     text NOT NULL UNIQUE,
    prefix       text NOT NULL,
    user_id      text NOT NULL REFERENCES users(id),
    name         text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz,
    last_used_at timestamptz,
    last_ip      text,
    client_id    text,
    revoked_at   timestamptz
);
CREATE INDEX IF NOT EXISTS api_keys_user_idx ON api_keys (user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS signing_keys (
    kid           text PRIMARY KEY,
    private_pkcs8 bytea NOT NULL,
    active        boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    retired_at    timestamptz
);

CREATE TABLE IF NOT EXISTS audit_events (
    id            text PRIMARY KEY,
    type          text NOT NULL,
    outcome       text NOT NULL,
    actor_user_id text,
    workspace_id  text,
    trace_id      text,
    request_id    text,
    resource      text,
    detail        jsonb NOT NULL DEFAULT '{}',
    time          timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS audit_events_time_idx ON audit_events (time DESC);
CREATE INDEX IF NOT EXISTS audit_events_actor_idx ON audit_events (actor_user_id, time DESC);

-- Local development seed: default organization, one department and two
-- workspaces. The non-member workspace exists so cross-workspace denial is
-- demonstrable end to end.
INSERT INTO organizations (id, name) VALUES ('org_default', 'Example Organization')
ON CONFLICT DO NOTHING;

INSERT INTO departments (id, organization_id, name) VALUES ('dept_platform', 'org_default', 'Platform')
ON CONFLICT DO NOTHING;

INSERT INTO workspaces (id, organization_id, name, is_default) VALUES
    ('secops',  'org_default', '安全运营', true),
    ('finance', 'org_default', '财务', false)
ON CONFLICT DO NOTHING;

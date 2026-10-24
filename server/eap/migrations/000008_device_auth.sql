-- TASK-015: RFC 8628 device authorization flow and versioned CLI bootstrap.
-- Refresh credentials are stored hashed; rotation supersedes the prior
-- token; approval is an explicit control-plane action.

CREATE TABLE IF NOT EXISTS device_authorizations (
    device_code  text PRIMARY KEY,
    user_code    text NOT NULL UNIQUE,
    status       text NOT NULL DEFAULT 'pending', -- pending | approved | denied
    user_id      text REFERENCES users(id),
    workspace_id text,
    poll_count   integer NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    decided_at   timestamptz
);
CREATE INDEX IF NOT EXISTS device_auth_expiry_idx ON device_authorizations (expires_at);

CREATE TABLE IF NOT EXISTS device_credentials (
    id            bigserial PRIMARY KEY,
    refresh_hash  text NOT NULL UNIQUE,
    user_id       text NOT NULL REFERENCES users(id),
    client        text NOT NULL DEFAULT 'eap-cli',
    status        text NOT NULL DEFAULT 'active', -- active | rotated | revoked
    rotated_to    bigint REFERENCES device_credentials(id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS device_cred_user_idx ON device_credentials (user_id);

-- TASK-027: built-in local credential fallback (ADR-0021). Passwords are
-- bcrypt hashes; the username is the login anchor and the row binds to the
-- canonical employee. This is NOT a full IAM — the primary enterprise path
-- remains OIDC.

CREATE TABLE IF NOT EXISTS local_credentials (
    username      text PRIMARY KEY,
    user_id       text NOT NULL REFERENCES users(id),
    password_hash text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id)
);

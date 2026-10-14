CREATE TABLE yuanpu_teams (
    id text PRIMARY KEY,
    name text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE yuanpu_users (
    id text PRIMARY KEY,
    email text NOT NULL UNIQUE,
    name text NOT NULL,
    role text NOT NULL CHECK (role IN ('admin', 'reviewer', 'team_lead', 'member')),
    team_id text REFERENCES yuanpu_teams(id),
    password_hash text NOT NULL,
    active boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT yuanpu_users_email_normalized CHECK (email = lower(email))
);

CREATE INDEX yuanpu_users_team_idx ON yuanpu_users(team_id);

CREATE TABLE yuanpu_sessions (
    token_hash bytea PRIMARY KEY,
    user_id text NOT NULL REFERENCES yuanpu_users(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);

CREATE INDEX yuanpu_sessions_user_idx ON yuanpu_sessions(user_id);

CREATE TABLE yuanpu_identity_audit (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor_id text REFERENCES yuanpu_users(id),
    subject_id text REFERENCES yuanpu_users(id),
    action text NOT NULL,
    details jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

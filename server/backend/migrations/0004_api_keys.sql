CREATE TABLE yuanpu_api_keys (
    id text PRIMARY KEY,
    user_id text NOT NULL REFERENCES yuanpu_users(id) ON DELETE CASCADE,
    issued_by text NOT NULL REFERENCES yuanpu_users(id),
    name text NOT NULL,
    key_prefix text NOT NULL,
    key_hash bytea NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    last_used_at timestamptz,
    revoked_at timestamptz
);

CREATE INDEX yuanpu_api_keys_user_idx ON yuanpu_api_keys(user_id, created_at DESC);

ALTER TABLE yuanpu_sessions ADD COLUMN api_key_id text REFERENCES yuanpu_api_keys(id);
CREATE INDEX yuanpu_sessions_api_key_idx ON yuanpu_sessions(api_key_id) WHERE api_key_id IS NOT NULL;

CREATE TABLE yuanpu_operation_logs (
    id text PRIMARY KEY,
    actor_id text NOT NULL REFERENCES yuanpu_users(id),
    idempotency_key text NOT NULL,
    resource_id text REFERENCES yuanpu_resources(id),
    run_id text,
    kind text NOT NULL CHECK (kind IN ('agent_run', 'tool_step', 'resource_use')),
    outcome text NOT NULL CHECK (outcome IN ('succeeded', 'failed', 'cancelled')),
    duration_ms integer NOT NULL CHECK (duration_ms >= 0 AND duration_ms <= 86400000),
    occurred_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (actor_id, idempotency_key)
);
CREATE INDEX yuanpu_operation_logs_resource_idx ON yuanpu_operation_logs(resource_id, received_at DESC);

CREATE TABLE yuanpu_data_uploads (
    id text PRIMARY KEY,
    actor_id text NOT NULL REFERENCES yuanpu_users(id),
    resource_id text REFERENCES yuanpu_resources(id),
    filename text NOT NULL,
    content_type text NOT NULL,
    byte_size integer NOT NULL CHECK (byte_size BETWEEN 1 AND 2097152),
    sha256 text NOT NULL,
    payload bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX yuanpu_data_uploads_resource_idx ON yuanpu_data_uploads(resource_id, created_at DESC);

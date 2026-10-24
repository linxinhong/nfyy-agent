-- TASK-008 provider governance: server-managed provider topology, logical
-- model aliases, routes with priority/weight/fallback, and pricing. The
-- public model name is a logical alias; provider-native names and credential
-- references never leave the server. Versioned rows make routing and cost
-- decisions reproducible (ADR-0008).

CREATE TABLE IF NOT EXISTS providers (
    name           text PRIMARY KEY,
    base_url       text NOT NULL,
    credential_ref text NOT NULL,
    enabled        boolean NOT NULL DEFAULT true,
    version        integer NOT NULL DEFAULT 1,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS model_aliases (
    alias      text PRIMARY KEY,
    enabled    boolean NOT NULL DEFAULT true,
    version    integer NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS model_routes (
    id           bigserial PRIMARY KEY,
    alias        text NOT NULL REFERENCES model_aliases(alias),
    provider     text NOT NULL REFERENCES providers(name),
    model        text NOT NULL,
    priority     integer NOT NULL DEFAULT 100,
    weight       integer NOT NULL DEFAULT 1,
    enabled      boolean NOT NULL DEFAULT true
);
CREATE INDEX IF NOT EXISTS model_routes_alias_idx ON model_routes (alias, priority);

CREATE TABLE IF NOT EXISTS model_pricing (
    alias            text NOT NULL REFERENCES model_aliases(alias),
    provider         text NOT NULL REFERENCES providers(name),
    currency         text NOT NULL DEFAULT 'USD',
    input_per_mt     numeric(12,6) NOT NULL,
    output_per_mt    numeric(12,6) NOT NULL,
    PRIMARY KEY (alias, provider)
);

-- Durable usage/cost records. One row per (request, attempt); idempotent by
-- (trace_id, request_id, attempt) so retries and stream terminations never
-- double-charge. Pricing snapshot keeps cost auditable against control data.
CREATE TABLE IF NOT EXISTS usage_records (
    id                bigserial PRIMARY KEY,
    trace_id          text NOT NULL,
    request_id        text NOT NULL,
    attempt           integer NOT NULL DEFAULT 0,
    user_id           text NOT NULL,
    workspace_id      text NOT NULL,
    alias             text NOT NULL,
    provider          text NOT NULL,
    provider_model    text NOT NULL,
    prompt_tokens     integer NOT NULL DEFAULT 0,
    completion_tokens integer NOT NULL DEFAULT 0,
    total_tokens      integer NOT NULL DEFAULT 0,
    currency          text NOT NULL,
    cost_micros       bigint NOT NULL,
    pricing_version   integer NOT NULL,
    streaming         boolean NOT NULL DEFAULT false,
    cost_known        boolean NOT NULL DEFAULT true,
    created_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (trace_id, request_id, attempt)
);
CREATE INDEX IF NOT EXISTS usage_records_ws_idx ON usage_records (workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS usage_records_user_idx ON usage_records (user_id, created_at DESC);

-- Demo control data: two independently configured provider stubs and one
-- logical alias with deterministic failover. Endpoints point at the demo
-- stubs (scripts/demo_stubs.py); credential refs resolve server-side only.
INSERT INTO providers (name, base_url, credential_ref) VALUES
    ('demo-primary', 'http://127.0.0.1:19099', 'env:EAP_LLM_PRIMARY_KEY'),
    ('demo-backup',  'http://127.0.0.1:19097', 'env:EAP_LLM_BACKUP_KEY')
ON CONFLICT DO NOTHING;

INSERT INTO model_aliases (alias) VALUES ('daily-fast'), ('coding-high')
ON CONFLICT DO NOTHING;

INSERT INTO model_routes (alias, provider, model, priority, weight) VALUES
    ('daily-fast',  'demo-primary', 'daily-fast@primary', 100, 1),
    ('daily-fast',  'demo-backup',  'daily-fast@backup',  200, 1),
    ('coding-high', 'demo-primary', 'coding-high@primary', 100, 1)
ON CONFLICT DO NOTHING;

INSERT INTO model_pricing (alias, provider, input_per_mt, output_per_mt) VALUES
    ('daily-fast',  'demo-primary', 2.500000, 10.000000),
    ('daily-fast',  'demo-backup',  3.000000, 12.000000),
    ('coding-high', 'demo-primary', 5.000000, 20.000000)
ON CONFLICT DO NOTHING;

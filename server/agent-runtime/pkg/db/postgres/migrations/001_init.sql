CREATE TABLE IF NOT EXISTS agent_runtime_sessions (
    session_id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    component_id TEXT NOT NULL,
    user_id TEXT NOT NULL DEFAULT '',
    payload JSONB NOT NULL,
    revision BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_runtime_sessions_lookup
    ON agent_runtime_sessions(kind, component_id, user_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS agent_runtime_runs (
    run_id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES agent_runtime_sessions(session_id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    user_id TEXT NOT NULL DEFAULT '',
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_runtime_runs_session
    ON agent_runtime_runs(session_id, created_at);
CREATE INDEX IF NOT EXISTS idx_runtime_runs_owner
    ON agent_runtime_runs(run_id, user_id);

CREATE TABLE IF NOT EXISTS agent_runtime_memories (
    memory_id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL DEFAULT '',
    agent_id TEXT NOT NULL DEFAULT '',
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_runtime_memories_lookup
    ON agent_runtime_memories(user_id, agent_id, created_at DESC);

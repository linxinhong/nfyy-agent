-- Migration 014 (stream journal): SSE frames shared by every replica so a
-- client can resume a stream on any of them with Last-Event-ID.
CREATE TABLE IF NOT EXISTS agent_runtime_stream_runs (
    run_id TEXT PRIMARY KEY,
    subject TEXT NOT NULL DEFAULT '',
    isolated BOOLEAN NOT NULL DEFAULT FALSE,
    namespace TEXT NOT NULL DEFAULT '',
    tenant BOOLEAN NOT NULL DEFAULT FALSE,
    done BOOLEAN NOT NULL DEFAULT FALSE,
    finished_at TIMESTAMPTZ,
    subscriber_seen_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_runtime_stream_runs_finished
    ON agent_runtime_stream_runs(finished_at) WHERE done;

CREATE TABLE IF NOT EXISTS agent_runtime_stream_frames (
    run_id TEXT NOT NULL REFERENCES agent_runtime_stream_runs(run_id) ON DELETE CASCADE,
    seq BIGINT NOT NULL,
    event TEXT NOT NULL,
    data TEXT NOT NULL,
    PRIMARY KEY (run_id, seq)
);

-- Curation layer (docs/knowledge-platform-architecture.md §5): durable jobs
-- for the curation worker, structured proposals, page flags and relations.
--
-- Invariants:
--   - A job is enqueued in the same transaction as the contribution version
--     or withdrawal that causes it; (contribution, version, kind) is unique,
--     so a replayed upload never queues twice.
--   - Jobs are leased with FOR UPDATE SKIP LOCKED; an expired lease returns
--     the job to the queue, attempts are capped and failures keep last_error.
--   - Proposals are rows, never direct edits: auto proposals are applied by
--     the server and keep enough state in `revert` to undo; review proposals
--     become submitted draft revisions owned by the curator service member
--     and are published only through the normal review queue.

CREATE TABLE IF NOT EXISTS curation_jobs (
    id                   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    workspace_id         text NOT NULL REFERENCES workspaces(id),
    kind                 text NOT NULL CHECK (kind IN ('contribution', 'withdrawal')),
    contribution_id      text NOT NULL REFERENCES contributions(id) ON DELETE CASCADE,
    contribution_version integer NOT NULL,
    status               text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'done', 'failed')),
    attempts             integer NOT NULL DEFAULT 0,
    lease_owner          text,
    lease_until          timestamptz,
    available_at         timestamptz NOT NULL DEFAULT now(),
    last_error           text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (contribution_id, contribution_version, kind)
);
CREATE INDEX IF NOT EXISTS curation_jobs_ready_idx ON curation_jobs (available_at) WHERE status IN ('queued', 'running');

CREATE TABLE IF NOT EXISTS curation_proposals (
    id                 text PRIMARY KEY,
    workspace_id       text NOT NULL REFERENCES workspaces(id),
    job_id             bigint REFERENCES curation_jobs(id) ON DELETE SET NULL,
    kind               text NOT NULL,
    risk               text NOT NULL CHECK (risk IN ('auto', 'review')),
    status             text NOT NULL CHECK (status IN ('auto_applied', 'pending', 'accepted', 'rejected', 'reverted')),
    target_resource_id text REFERENCES content_resources(id) ON DELETE SET NULL,
    target_path        text NOT NULL DEFAULT '',
    payload            jsonb NOT NULL,
    sources            jsonb NOT NULL,
    rationale          text NOT NULL,
    applied_revision   integer,
    revert             jsonb,
    created_at         timestamptz NOT NULL DEFAULT now(),
    decided_at         timestamptz,
    decided_by         text REFERENCES users(id)
);
CREATE INDEX IF NOT EXISTS curation_proposals_status_idx ON curation_proposals (workspace_id, status, created_at DESC);

CREATE TABLE IF NOT EXISTS content_flags (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    resource_id text NOT NULL REFERENCES content_resources(id) ON DELETE CASCADE,
    kind        text NOT NULL CHECK (kind IN ('duplicate', 'sensitive', 'conflict', 'source_withdrawn')),
    proposal_id text REFERENCES curation_proposals(id) ON DELETE SET NULL,
    detail      jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at  timestamptz NOT NULL DEFAULT now(),
    cleared_at  timestamptz
);
CREATE INDEX IF NOT EXISTS content_flags_open_idx ON content_flags (resource_id) WHERE cleared_at IS NULL;

CREATE TABLE IF NOT EXISTS content_relations (
    resource_id        text NOT NULL REFERENCES content_resources(id) ON DELETE CASCADE,
    target_resource_id text NOT NULL REFERENCES content_resources(id) ON DELETE CASCADE,
    proposal_id        text REFERENCES curation_proposals(id) ON DELETE SET NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (resource_id, target_resource_id)
);

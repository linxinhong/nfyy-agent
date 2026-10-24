-- Inbox layer for local-agent uploads (docs/knowledge-platform-architecture.md §3).
-- A contribution is one local page (author + local_id) inside a Workspace;
-- every distinct content becomes an append-only version. The curation layer
-- reads versions and never rewrites them.
--
-- Invariants:
--   - (workspace_id, author_id, local_id) names one contribution; a local id
--     is never shared across authors.
--   - content_hash is computed by the server over the normalized fields; the
--     same content never produces a second version (UNIQUE per contribution).
--   - Idempotency keys are per author: a replay returns the recorded version,
--     a reused key with a different body is refused (request_hash differs).
--   - Team scope names a group of the same Workspace; the group cannot be
--     deleted while a contribution still scopes to it.

CREATE TABLE IF NOT EXISTS contributions (
    id              text PRIMARY KEY,
    workspace_id    text NOT NULL REFERENCES workspaces(id),
    author_id       text NOT NULL REFERENCES users(id),
    local_id        text NOT NULL,
    current_version integer NOT NULL CHECK (current_version >= 1),
    status          text NOT NULL CHECK (status IN ('received', 'withdrawn')),
    scope           text NOT NULL CHECK (scope IN ('personal', 'team', 'organization')),
    group_id        text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    withdrawn_at    timestamptz,
    UNIQUE (workspace_id, author_id, local_id),
    CONSTRAINT contribution_team_has_group CHECK ((scope = 'team') = (group_id IS NOT NULL)),
    CONSTRAINT contribution_group_in_workspace FOREIGN KEY (group_id, workspace_id)
        REFERENCES workspace_groups(id, workspace_id) ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS contributions_author_updated_idx
    ON contributions (workspace_id, author_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS contribution_versions (
    contribution_id text NOT NULL REFERENCES contributions(id) ON DELETE CASCADE,
    version         integer NOT NULL CHECK (version >= 1),
    content_hash    text NOT NULL,
    title           text NOT NULL,
    summary         text NOT NULL,
    markdown        text NOT NULL,
    details         jsonb NOT NULL,
    links           jsonb NOT NULL,
    source_refs     jsonb NOT NULL,
    attachments     jsonb NOT NULL,
    scope           text NOT NULL,
    group_id        text,
    upload_mode     text NOT NULL,
    client          jsonb NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (contribution_id, version),
    UNIQUE (contribution_id, content_hash)
);

CREATE TABLE IF NOT EXISTS contribution_idempotency (
    author_id       text NOT NULL REFERENCES users(id),
    idempotency_key text NOT NULL,
    request_hash    text NOT NULL,
    contribution_id text NOT NULL REFERENCES contributions(id) ON DELETE CASCADE,
    version         integer NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (author_id, idempotency_key)
);

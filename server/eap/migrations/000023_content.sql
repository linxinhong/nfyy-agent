-- Shared content (news, Wiki, capability notes) with draft → review → publish
-- (docs/knowledge-platform-architecture.md §4, §8). Ported from the MindLink
-- center's 0003_content.sql onto EAP identity: rows belong to one Workspace,
-- "team" visibility names a workspace_groups row of that same Workspace.
--
-- Invariants:
--   - Revisions are append-only content; only status/reviewer columns change.
--   - published_revision points at an approved revision of the same resource.
--   - A group that still scopes content cannot be deleted (RESTRICT): team
--     content must never silently widen or orphan.
--   - Business audit goes to audit_events in the same transaction.

CREATE TABLE IF NOT EXISTS content_resources (
    id                 text PRIMARY KEY,
    workspace_id       text NOT NULL REFERENCES workspaces(id),
    kind               text NOT NULL CHECK (kind IN ('news', 'wiki', 'capability')),
    owner_id           text NOT NULL REFERENCES users(id),
    visibility         text NOT NULL CHECK (visibility IN ('team', 'organization')),
    group_id           text,
    published_revision integer,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT content_team_has_group CHECK ((visibility = 'team') = (group_id IS NOT NULL)),
    CONSTRAINT content_group_in_workspace FOREIGN KEY (group_id, workspace_id)
        REFERENCES workspace_groups(id, workspace_id) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS content_revisions (
    resource_id text NOT NULL REFERENCES content_resources(id) ON DELETE CASCADE,
    revision    integer NOT NULL CHECK (revision >= 1),
    title       text NOT NULL,
    summary     text NOT NULL,
    body        text NOT NULL,
    details     jsonb NOT NULL DEFAULT '{}'::jsonb,
    status      text NOT NULL CHECK (status IN ('draft', 'submitted', 'approved', 'rejected')),
    created_by  text NOT NULL REFERENCES users(id),
    reviewed_by text REFERENCES users(id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    reviewed_at timestamptz,
    PRIMARY KEY (resource_id, revision)
);

ALTER TABLE content_resources DROP CONSTRAINT IF EXISTS content_published_revision_fk;
ALTER TABLE content_resources ADD CONSTRAINT content_published_revision_fk
    FOREIGN KEY (id, published_revision)
    REFERENCES content_revisions(resource_id, revision)
    DEFERRABLE INITIALLY DEFERRED;

CREATE INDEX IF NOT EXISTS content_resources_published_idx
    ON content_resources (workspace_id, visibility, group_id, updated_at DESC)
    WHERE published_revision IS NOT NULL;

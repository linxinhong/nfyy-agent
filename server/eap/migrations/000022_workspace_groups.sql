-- Workspace groups (MindLink teams, docs/knowledge-platform-architecture.md §8.5):
-- an organization runs one Workspace and its teams are groups inside it.
-- Group membership hangs off workspace_members, so removing a member from the
-- Workspace drops the group row in the same statement (ON DELETE CASCADE) and
-- no orphan grant can outlive membership. One group per member per Workspace
-- for now (UNIQUE (workspace_id, user_id)); dropping that constraint is the
-- whole change needed for multi-team membership later.

CREATE TABLE IF NOT EXISTS workspace_groups (
    id           text PRIMARY KEY DEFAULT 'grp_' || substr(replace(gen_random_uuid()::text, '-', ''), 1, 16),
    workspace_id text NOT NULL REFERENCES workspaces(id),
    name         text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 80),
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name),
    UNIQUE (id, workspace_id)
);

CREATE TABLE IF NOT EXISTS workspace_group_members (
    group_id     text NOT NULL,
    workspace_id text NOT NULL,
    user_id      text NOT NULL,
    is_lead      boolean NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, user_id),
    UNIQUE (workspace_id, user_id),
    FOREIGN KEY (group_id, workspace_id) REFERENCES workspace_groups(id, workspace_id) ON DELETE CASCADE,
    FOREIGN KEY (workspace_id, user_id) REFERENCES workspace_members(workspace_id, user_id) ON DELETE CASCADE
);

-- Admin-issued one-time passwords must be replaced before a session is issued.
ALTER TABLE local_credentials
    ADD COLUMN IF NOT EXISTS must_change_password boolean NOT NULL DEFAULT false;

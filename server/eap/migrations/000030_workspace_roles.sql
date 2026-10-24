-- Editable roles (packages/rbac): a Workspace stores only its edits of the
-- built-in roles and its custom roles; built-ins themselves live in code so
-- every Workspace has them without seeding.
--
-- Invariants:
--   - workspace_members.roles keeps holding role keys, so Policy, DLP
--     except_roles and IdP group mappings keyed on them keep working.
--   - A team role is granted on the member's group row (team_roles) and so
--     disappears with the group membership; is_lead becomes team role 'lead'.

CREATE TABLE IF NOT EXISTS workspace_roles (
    workspace_id text NOT NULL REFERENCES workspaces(id),
    key          text NOT NULL CHECK (key ~ '^[a-z][a-z0-9_-]{0,31}$'),
    name         text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 40),
    scope        text NOT NULL CHECK (scope IN ('workspace', 'team')),
    permissions  text[] NOT NULL DEFAULT '{}',
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, key)
);

ALTER TABLE workspace_group_members ADD COLUMN IF NOT EXISTS team_roles text[] NOT NULL DEFAULT '{}';

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_name = 'workspace_group_members' AND column_name = 'is_lead') THEN
        UPDATE workspace_group_members SET team_roles = ARRAY['lead'] WHERE is_lead;
        ALTER TABLE workspace_group_members DROP COLUMN is_lead;
    END IF;
END $$;

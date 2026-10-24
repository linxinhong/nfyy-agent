-- Skill sharing (packages/skillshare): skills are shared at a scope like
-- knowledge, their SKILL.md can be kept out of the downloadable archive, and
-- members enable skills or have them required by a team or the Workspace.
--
-- Invariants:
--   - Existing registry rows stay organization-wide and unprotected.
--   - A team skill names a team of its own Workspace; deleting a team that
--     still scopes skills is refused like for team content.
--   - protected_body is read only by the gateway's model-side injection and
--     by authors/reviewers in the Portal; no download path returns it.
--   - Requirements and enablements disappear with the team or membership.

ALTER TABLE skill_packages ADD COLUMN IF NOT EXISTS visibility text NOT NULL DEFAULT 'organization';
ALTER TABLE skill_packages ADD COLUMN IF NOT EXISTS group_id text;
ALTER TABLE skill_packages ADD COLUMN IF NOT EXISTS description text NOT NULL DEFAULT '';
ALTER TABLE skill_packages ADD COLUMN IF NOT EXISTS protected_body text;
ALTER TABLE skill_packages ADD COLUMN IF NOT EXISTS reject_reason text;

ALTER TABLE skill_packages DROP CONSTRAINT IF EXISTS skill_packages_visibility_check;
ALTER TABLE skill_packages ADD CONSTRAINT skill_packages_visibility_check
    CHECK (visibility IN ('personal', 'team', 'organization') AND (visibility = 'team') = (group_id IS NOT NULL));
ALTER TABLE skill_packages DROP CONSTRAINT IF EXISTS skill_packages_group_fk;
ALTER TABLE skill_packages ADD CONSTRAINT skill_packages_group_fk
    FOREIGN KEY (group_id, workspace_id) REFERENCES workspace_groups(id, workspace_id);

CREATE TABLE IF NOT EXISTS skill_requirements (
    workspace_id text NOT NULL REFERENCES workspaces(id),
    name         text NOT NULL,
    group_id     text,
    set_by       text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (group_id, workspace_id) REFERENCES workspace_groups(id, workspace_id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS skill_requirements_one
    ON skill_requirements (workspace_id, name, COALESCE(group_id, ''));

CREATE TABLE IF NOT EXISTS skill_enablements (
    workspace_id text NOT NULL,
    user_id      text NOT NULL,
    name         text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, user_id, name),
    FOREIGN KEY (workspace_id, user_id) REFERENCES workspace_members(workspace_id, user_id) ON DELETE CASCADE
);

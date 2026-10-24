-- TASK-016: governed Skill Registry with server-signed releases.
-- name@version is immutable; lifecycle is pending_review -> approved ->
-- released -> deprecated/revoked; signatures use purpose-separated package
-- keys (kid namespace pkg-*, never internal JWT keys).

CREATE TABLE IF NOT EXISTS skill_packages (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name         text NOT NULL,
    version      text NOT NULL,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    status       text NOT NULL DEFAULT 'pending_review',
    digest       text NOT NULL,
    payload      bytea NOT NULL,
    signed_by    text,
    signature    text,
    published_by text NOT NULL,
    reviewed_by  text,
    released_by  text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    released_at  timestamptz,
    revoked_at   timestamptz,
    UNIQUE (name, version)
);

CREATE TABLE IF NOT EXISTS skill_package_keys (
    kid        text PRIMARY KEY,
    public_key text NOT NULL,
    active     boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    retired_at timestamptz
);

-- TASK-019: Agent client releases with same-domain download tickets.

CREATE TABLE IF NOT EXISTS agent_releases (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    client       text NOT NULL,
    version      text NOT NULL,
    os           text NOT NULL,
    arch         text NOT NULL,
    minimum_version text NOT NULL DEFAULT '0.0.0',
    notes        text NOT NULL DEFAULT '',
    digest       text NOT NULL,
    payload      bytea NOT NULL,
    signature    text NOT NULL,
    signed_by    text NOT NULL,
    workspace_id text NOT NULL,
    status       text NOT NULL DEFAULT 'released', -- released | revoked
    created_at   timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz,
    UNIQUE (client, os, arch, version)
);

CREATE TABLE IF NOT EXISTS download_tickets (
    ticket       text PRIMARY KEY,
    release_id   uuid NOT NULL REFERENCES agent_releases(id),
    user_id      text NOT NULL,
    workspace_id text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    redeemed_at  timestamptz
);
CREATE INDEX IF NOT EXISTS download_tickets_release_idx ON download_tickets (release_id);

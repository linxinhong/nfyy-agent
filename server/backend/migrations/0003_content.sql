CREATE TABLE yuanpu_resources (
    id text PRIMARY KEY,
    kind text NOT NULL CHECK (kind IN ('news', 'wiki', 'capability')),
    owner_id text NOT NULL REFERENCES yuanpu_users(id),
    team_id text REFERENCES yuanpu_teams(id),
    visibility text NOT NULL CHECK (visibility IN ('team', 'organization')),
    published_revision integer,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT yuanpu_team_visibility CHECK (visibility <> 'team' OR team_id IS NOT NULL)
);

CREATE TABLE yuanpu_resource_revisions (
    resource_id text NOT NULL REFERENCES yuanpu_resources(id) ON DELETE CASCADE,
    revision integer NOT NULL CHECK (revision >= 1),
    title text NOT NULL,
    summary text NOT NULL,
    body text NOT NULL,
    details jsonb NOT NULL DEFAULT '{}'::jsonb,
    status text NOT NULL CHECK (status IN ('draft', 'submitted', 'approved', 'rejected')),
    created_by text NOT NULL REFERENCES yuanpu_users(id),
    reviewed_by text REFERENCES yuanpu_users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    reviewed_at timestamptz,
    PRIMARY KEY (resource_id, revision)
);

ALTER TABLE yuanpu_resources ADD CONSTRAINT yuanpu_published_revision_fk
    FOREIGN KEY (id, published_revision)
    REFERENCES yuanpu_resource_revisions(resource_id, revision)
    DEFERRABLE INITIALLY DEFERRED;

CREATE INDEX yuanpu_resources_published_idx ON yuanpu_resources(visibility, team_id, updated_at DESC)
    WHERE published_revision IS NOT NULL;

CREATE TABLE yuanpu_resource_audit (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    resource_id text NOT NULL REFERENCES yuanpu_resources(id),
    revision integer NOT NULL,
    actor_id text NOT NULL REFERENCES yuanpu_users(id),
    action text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

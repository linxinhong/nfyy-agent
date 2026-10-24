-- Knowledge layer (docs/knowledge-platform-architecture.md §4): a fixed
-- shelf → book → chapter tree per Workspace, Wiki pages as content resources
-- placed under a chapter, tags, internal links and the source chain from
-- page revisions back to contribution versions.
--
-- Invariants:
--   - Levels are fixed: a shelf has no parent, a book's parent is a shelf, a
--     chapter's parent is a book (enforced in packages/wiki; the trigger-free
--     schema only guarantees same-Workspace parents).
--   - A page path is shelf/book/chapter/page slugs; slugs are unique among
--     siblings, so a path names at most one page.
--   - body_text is the rendered plain text of the revision, written with the
--     revision and used by search; HTML is rendered and sanitized per read so
--     a sanitizer change applies to every stored revision at once.
--   - The source chain is append-only like revisions and points at immutable
--     contribution versions; withdrawing a contribution does not delete it
--     (curation decides what the next revision drops).

CREATE TABLE IF NOT EXISTS wiki_nodes (
    id           text PRIMARY KEY DEFAULT 'wnd_' || substr(replace(gen_random_uuid()::text, '-', ''), 1, 16),
    workspace_id text NOT NULL REFERENCES workspaces(id),
    parent_id    text,
    level        text NOT NULL CHECK (level IN ('shelf', 'book', 'chapter')),
    slug         text NOT NULL,
    title        text NOT NULL CHECK (length(btrim(title)) BETWEEN 1 AND 120),
    sort         integer NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, workspace_id),
    CONSTRAINT wiki_node_parent_in_workspace FOREIGN KEY (parent_id, workspace_id)
        REFERENCES wiki_nodes(id, workspace_id) ON DELETE RESTRICT,
    CONSTRAINT wiki_node_shelf_is_root CHECK ((level = 'shelf') = (parent_id IS NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS wiki_nodes_sibling_slug_idx
    ON wiki_nodes (workspace_id, COALESCE(parent_id, ''), slug);

ALTER TABLE content_resources ADD COLUMN IF NOT EXISTS node_id text;
ALTER TABLE content_resources ADD COLUMN IF NOT EXISTS slug text;
ALTER TABLE content_resources DROP CONSTRAINT IF EXISTS content_wiki_node_in_workspace;
ALTER TABLE content_resources ADD CONSTRAINT content_wiki_node_in_workspace
    FOREIGN KEY (node_id, workspace_id) REFERENCES wiki_nodes(id, workspace_id) ON DELETE RESTRICT;
ALTER TABLE content_resources DROP CONSTRAINT IF EXISTS content_wiki_placement;
ALTER TABLE content_resources ADD CONSTRAINT content_wiki_placement
    CHECK ((node_id IS NULL) = (slug IS NULL) AND (node_id IS NULL OR kind = 'wiki'));
CREATE UNIQUE INDEX IF NOT EXISTS content_resources_page_slug_idx
    ON content_resources (node_id, slug) WHERE node_id IS NOT NULL;

ALTER TABLE content_revisions ADD COLUMN IF NOT EXISTS body_text text NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS content_tags (
    resource_id text NOT NULL REFERENCES content_resources(id) ON DELETE CASCADE,
    tag         text NOT NULL CHECK (length(tag) BETWEEN 1 AND 40),
    PRIMARY KEY (resource_id, tag)
);
CREATE INDEX IF NOT EXISTS content_tags_tag_idx ON content_tags (tag);

CREATE TABLE IF NOT EXISTS content_revision_links (
    resource_id text NOT NULL,
    revision    integer NOT NULL,
    target_path text NOT NULL,
    PRIMARY KEY (resource_id, revision, target_path),
    FOREIGN KEY (resource_id, revision) REFERENCES content_revisions(resource_id, revision) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS content_revision_links_target_idx ON content_revision_links (target_path);

CREATE TABLE IF NOT EXISTS content_revision_sources (
    resource_id          text NOT NULL,
    revision             integer NOT NULL,
    contribution_id      text NOT NULL,
    contribution_version integer NOT NULL,
    anchor               text NOT NULL DEFAULT '',
    PRIMARY KEY (resource_id, revision, contribution_id, contribution_version, anchor),
    FOREIGN KEY (resource_id, revision) REFERENCES content_revisions(resource_id, revision) ON DELETE CASCADE,
    FOREIGN KEY (contribution_id, contribution_version) REFERENCES contribution_versions(contribution_id, version)
);
CREATE INDEX IF NOT EXISTS content_revision_sources_contribution_idx
    ON content_revision_sources (contribution_id);

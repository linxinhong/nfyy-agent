-- Old wiki page paths keep resolving after a page moves or a shelf, book or
-- chapter is renamed (docs/knowledge-platform-architecture.md §4.2).
--
-- Why triggers: a page path changes in three code paths (resource update,
-- curation classify, curation undo) and through node renames; recording the
-- old path in the same statement covers every one of them atomically.
--
-- Invariants:
--   - The current path always wins: ResolvePath consults aliases only when
--     no page has the path now. A later move of another page off the same
--     old path replaces the alias (last move wins).
--   - Aliases are lookup data only; reading still goes through content
--     authorization on the target page.

CREATE TABLE IF NOT EXISTS wiki_path_aliases (
    workspace_id text NOT NULL REFERENCES workspaces(id),
    path         text NOT NULL,
    resource_id  text NOT NULL REFERENCES content_resources(id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, path)
);

CREATE INDEX IF NOT EXISTS wiki_path_aliases_resource ON wiki_path_aliases (resource_id);

CREATE OR REPLACE FUNCTION wiki_alias_page_move() RETURNS trigger AS $$
BEGIN
    INSERT INTO wiki_path_aliases (workspace_id, path, resource_id)
    SELECT OLD.workspace_id, sh.slug || '/' || bk.slug || '/' || ch.slug || '/' || OLD.slug, OLD.id
    FROM wiki_nodes ch
    JOIN wiki_nodes bk ON bk.id = ch.parent_id
    JOIN wiki_nodes sh ON sh.id = bk.parent_id
    WHERE ch.id = OLD.node_id
    ON CONFLICT (workspace_id, path) DO UPDATE SET resource_id = EXCLUDED.resource_id, created_at = now();
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS wiki_alias_page_move ON content_resources;
CREATE TRIGGER wiki_alias_page_move
    AFTER UPDATE OF node_id, slug ON content_resources
    FOR EACH ROW
    WHEN (OLD.node_id IS NOT NULL AND OLD.slug IS NOT NULL
          AND (OLD.node_id IS DISTINCT FROM NEW.node_id OR OLD.slug IS DISTINCT FROM NEW.slug))
    EXECUTE FUNCTION wiki_alias_page_move();

CREATE OR REPLACE FUNCTION wiki_alias_node_rename() RETURNS trigger AS $$
BEGIN
    -- The row already carries the new slug; rebuild the old path with OLD.slug.
    INSERT INTO wiki_path_aliases (workspace_id, path, resource_id)
    SELECT r.workspace_id,
           (CASE WHEN sh.id = OLD.id THEN OLD.slug ELSE sh.slug END) || '/' ||
           (CASE WHEN bk.id = OLD.id THEN OLD.slug ELSE bk.slug END) || '/' ||
           (CASE WHEN ch.id = OLD.id THEN OLD.slug ELSE ch.slug END) || '/' || r.slug,
           r.id
    FROM content_resources r
    JOIN wiki_nodes ch ON ch.id = r.node_id
    JOIN wiki_nodes bk ON bk.id = ch.parent_id
    JOIN wiki_nodes sh ON sh.id = bk.parent_id
    WHERE r.slug IS NOT NULL AND OLD.id IN (sh.id, bk.id, ch.id)
    ON CONFLICT (workspace_id, path) DO UPDATE SET resource_id = EXCLUDED.resource_id, created_at = now();
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS wiki_alias_node_rename ON wiki_nodes;
CREATE TRIGGER wiki_alias_node_rename
    AFTER UPDATE OF slug ON wiki_nodes
    FOR EACH ROW
    WHEN (OLD.slug IS DISTINCT FROM NEW.slug)
    EXECUTE FUNCTION wiki_alias_node_rename();

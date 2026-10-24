-- TASK-012: Workspace-scoped Knowledge vertical slice. Documents are
-- versioned; chunks carry a workspace FK and a deterministic embedding so
-- retrieval filters by workspace INSIDE the query (never post-filter).

CREATE TABLE IF NOT EXISTS knowledge_documents (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id text NOT NULL REFERENCES workspaces(id),
    title        text NOT NULL,
    version      integer NOT NULL DEFAULT 1,
    status       text NOT NULL DEFAULT 'processing', -- processing | ready | failed
    content_hash text NOT NULL,
    created_by   text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS knowledge_documents_ws_idx ON knowledge_documents (workspace_id, updated_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS knowledge_documents_active_idx
    ON knowledge_documents (workspace_id, title, version);

CREATE TABLE IF NOT EXISTS knowledge_chunks (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id  uuid NOT NULL REFERENCES knowledge_documents(id) ON DELETE CASCADE,
    workspace_id text NOT NULL REFERENCES workspaces(id),
    seq          integer NOT NULL,
    content      text NOT NULL,
    embedding    double precision[] NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (document_id, seq)
);
CREATE INDEX IF NOT EXISTS knowledge_chunks_ws_idx ON knowledge_chunks (workspace_id);

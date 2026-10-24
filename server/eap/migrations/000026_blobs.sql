-- Attachment blobs for contributions (docs/knowledge-platform-architecture.md §3.2).
-- Content-addressed per Workspace: the same bytes uploaded twice are one row
-- and one file. The bytes live under EAP_BLOB_ROOT; this table is the index.
--
-- Invariants:
--   - sha256 is verified by the server while streaming; a row exists only
--     after the file was written and renamed into place.
--   - scan_status is 'unscanned' until a malware scanner is wired in; it
--     never claims 'clean' for bytes nobody scanned. 'blocked' blobs cannot
--     be referenced by a contribution.

CREATE TABLE IF NOT EXISTS blobs (
    workspace_id text NOT NULL REFERENCES workspaces(id),
    sha256       text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    size         bigint NOT NULL CHECK (size > 0),
    media_type   text NOT NULL,
    scan_status  text NOT NULL CHECK (scan_status IN ('unscanned', 'clean', 'blocked')),
    uploaded_by  text NOT NULL REFERENCES users(id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, sha256)
);

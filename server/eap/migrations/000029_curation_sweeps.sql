-- Low-frequency global re-curation (docs/knowledge-platform-architecture.md
-- §5.5): a 'sweep' job covers a whole Workspace's published pages instead of
-- one contribution, so it carries no contribution.
--
-- Invariants:
--   - At most one open (queued or running) sweep per Workspace, so several
--     curator processes scheduling at once enqueue it once.
--   - Contribution and withdrawal jobs still name their contribution version.

ALTER TABLE curation_jobs DROP CONSTRAINT IF EXISTS curation_jobs_kind_check;
ALTER TABLE curation_jobs ADD CONSTRAINT curation_jobs_kind_check CHECK (kind IN ('contribution', 'withdrawal', 'sweep'));
ALTER TABLE curation_jobs ALTER COLUMN contribution_id DROP NOT NULL;
ALTER TABLE curation_jobs ALTER COLUMN contribution_version DROP NOT NULL;
ALTER TABLE curation_jobs DROP CONSTRAINT IF EXISTS curation_jobs_contribution_shape;
ALTER TABLE curation_jobs ADD CONSTRAINT curation_jobs_contribution_shape
    CHECK ((kind = 'sweep') = (contribution_id IS NULL AND contribution_version IS NULL));

CREATE UNIQUE INDEX IF NOT EXISTS curation_jobs_one_open_sweep
    ON curation_jobs (workspace_id) WHERE kind = 'sweep' AND status IN ('queued', 'running');

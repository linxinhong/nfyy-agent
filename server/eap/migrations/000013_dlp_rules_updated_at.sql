-- TASK-026: DLP rule management needs a change-tracking timestamp for the
-- audited action/enabled toggles.

ALTER TABLE dlp_rules ADD COLUMN IF NOT EXISTS updated_at timestamptz NOT NULL DEFAULT now();

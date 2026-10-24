-- TASK-023: the IdP subject is the primary identity anchor. A directory
-- email is an optional profile attribute (Feishu tenant directories often
-- carry none): subject-only JIT provisioning must not violate the unique
-- email constraint, so the unique index becomes partial (non-empty emails
-- only). Historic rows keep their emails; no backfill.

ALTER TABLE users ALTER COLUMN email DROP NOT NULL;

DROP INDEX IF EXISTS users_email_lower_idx;
CREATE UNIQUE INDEX IF NOT EXISTS users_email_lower_idx
    ON users (lower(email)) WHERE email <> '';

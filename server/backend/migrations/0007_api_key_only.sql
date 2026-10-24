-- API key is the only login credential. Drop the legacy password verifier and
-- reject any sessions issued before an API key was attached to the session.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM yuanpu_users) AND NOT EXISTS (
        SELECT 1 FROM yuanpu_users u JOIN yuanpu_api_keys k ON k.user_id=u.id
        WHERE u.role='admin' AND u.active AND k.revoked_at IS NULL AND k.expires_at>now()
    ) THEN
        RAISE EXCEPTION 'issue an active administrator API key before schema 7 migration';
    END IF;
END $$;
DELETE FROM yuanpu_sessions WHERE api_key_id IS NULL;
ALTER TABLE yuanpu_sessions ALTER COLUMN api_key_id SET NOT NULL;
ALTER TABLE yuanpu_users DROP COLUMN password_hash;

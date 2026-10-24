-- Browser accounts can sign in with a password. API-key sessions remain tied
-- to their key, while password sessions have a NULL api_key_id.
ALTER TABLE yuanpu_users ADD COLUMN password_hash text;
ALTER TABLE yuanpu_sessions ALTER COLUMN api_key_id DROP NOT NULL;

package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// LocalCredential binds one login username to the canonical employee row.
// Only the bcrypt hash is stored — never the password.
type LocalCredential struct {
	Username string
	UserID   string
}

// UpsertLocalCredential creates or resets the credential for a username,
// binding it to the given employee. Admin-managed (TASK-027).
func (s *Store) UpsertLocalCredential(ctx context.Context, username, userID, passwordHash string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO local_credentials (username, user_id, password_hash)
		VALUES ($1, $2, $3)
		ON CONFLICT (username) DO UPDATE
		    SET user_id = EXCLUDED.user_id,
		        password_hash = EXCLUDED.password_hash,
		        updated_at = now()`,
		username, userID, passwordHash)
	if err != nil {
		return fmt.Errorf("local credential upsert: %w", err)
	}
	return nil
}

// GetLocalCredential returns (userID, passwordHash) for a username.
// pgx.ErrNoRows means the username does not exist — callers must produce an
// identical error shape for unknown users and wrong passwords.
func (s *Store) GetLocalCredential(ctx context.Context, username string) (string, string, error) {
	var userID, hash string
	err := s.pool.QueryRow(ctx,
		`SELECT user_id, password_hash FROM local_credentials WHERE username = $1`,
		username).Scan(&userID, &hash)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", "", pgx.ErrNoRows
		}
		return "", "", fmt.Errorf("local credential lookup: %w", err)
	}
	return userID, hash, nil
}

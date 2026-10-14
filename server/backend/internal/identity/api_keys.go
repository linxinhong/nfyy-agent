package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type APIKey struct {
	ID         string     `json:"id"`
	UserID     string     `json:"userId"`
	IssuedBy   string     `json:"issuedBy"`
	Name       string     `json:"name"`
	KeyPrefix  string     `json:"keyPrefix"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
}

func (s *Store) IssueAPIKey(ctx context.Context, actor User, userID, name string, expiresInDays int) (APIKey, string, error) {
	if actor.Role != "admin" || !actor.Active {
		return APIKey{}, "", ErrUnauthorized
	}
	name = strings.TrimSpace(name)
	if userID == "" || name == "" || len(name) > 120 || expiresInDays < 1 || expiresInDays > 365 {
		return APIKey{}, "", ErrInvalid
	}
	if s.pool == nil {
		return APIKey{}, "", ErrUnavailable
	}
	id, err := newID()
	if err != nil {
		return APIKey{}, "", ErrUnavailable
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return APIKey{}, "", ErrUnavailable
	}
	plain := "ypk_" + base64.RawURLEncoding.EncodeToString(secret)
	hash := sha256.Sum256([]byte(plain))
	expiresAt := time.Now().UTC().AddDate(0, 0, expiresInDays)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return APIKey{}, "", ErrUnavailable
	}
	defer tx.Rollback(context.Background())
	var active bool
	err = tx.QueryRow(ctx, `SELECT active FROM yuanpu_users WHERE id=$1 FOR UPDATE`, userID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIKey{}, "", ErrNotFound
	}
	if err != nil {
		return APIKey{}, "", ErrUnavailable
	}
	if !active {
		return APIKey{}, "", ErrNotFound
	}
	var key APIKey
	err = tx.QueryRow(ctx, `INSERT INTO yuanpu_api_keys (id,user_id,issued_by,name,key_prefix,key_hash,expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING id,user_id,issued_by,name,key_prefix,created_at,expires_at,last_used_at,revoked_at`,
		id, userID, actor.ID, name, plain[:12], hash[:], expiresAt,
	).Scan(&key.ID, &key.UserID, &key.IssuedBy, &key.Name, &key.KeyPrefix,
		&key.CreatedAt, &key.ExpiresAt, &key.LastUsedAt, &key.RevokedAt)
	if err != nil {
		return APIKey{}, "", databaseError(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO yuanpu_identity_audit (actor_id,subject_id,action,details)
		VALUES ($1,$2,'issue_api_key',jsonb_build_object('keyId',$3::text,'expiresAt',$4::timestamptz))`,
		actor.ID, userID, id, expiresAt); err != nil {
		return APIKey{}, "", ErrUnavailable
	}
	if err := tx.Commit(ctx); err != nil {
		return APIKey{}, "", ErrUnavailable
	}
	return key, plain, nil
}

func (s *Store) ListAPIKeys(ctx context.Context, actor User) ([]APIKey, error) {
	if actor.Role != "admin" || !actor.Active {
		return nil, ErrUnauthorized
	}
	if s.pool == nil {
		return nil, ErrUnavailable
	}
	rows, err := s.pool.Query(ctx, `SELECT id,user_id,issued_by,name,key_prefix,created_at,expires_at,last_used_at,revoked_at
		FROM yuanpu_api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rows.Close()
	keys := make([]APIKey, 0)
	for rows.Next() {
		var key APIKey
		if err := rows.Scan(&key.ID, &key.UserID, &key.IssuedBy, &key.Name, &key.KeyPrefix,
			&key.CreatedAt, &key.ExpiresAt, &key.LastUsedAt, &key.RevokedAt); err != nil {
			return nil, ErrUnavailable
		}
		keys = append(keys, key)
	}
	if rows.Err() != nil {
		return nil, ErrUnavailable
	}
	return keys, nil
}

func (s *Store) RevokeAPIKey(ctx context.Context, actor User, id string) error {
	if actor.Role != "admin" || !actor.Active {
		return ErrUnauthorized
	}
	if id == "" {
		return ErrInvalid
	}
	if s.pool == nil {
		return ErrUnavailable
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ErrUnavailable
	}
	defer tx.Rollback(context.Background())
	var userID string
	err = tx.QueryRow(ctx, `SELECT user_id FROM yuanpu_api_keys WHERE id=$1 FOR UPDATE`, id).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return ErrUnavailable
	}
	if _, err := tx.Exec(ctx, `UPDATE yuanpu_api_keys SET revoked_at=COALESCE(revoked_at,now()) WHERE id=$1`, id); err != nil {
		return ErrUnavailable
	}
	if _, err := tx.Exec(ctx, `UPDATE yuanpu_sessions SET revoked_at=now() WHERE api_key_id=$1 AND revoked_at IS NULL`, id); err != nil {
		return ErrUnavailable
	}
	if _, err := tx.Exec(ctx, `INSERT INTO yuanpu_identity_audit (actor_id,subject_id,action,details)
		VALUES ($1,$2,'revoke_api_key',jsonb_build_object('keyId',$3::text))`, actor.ID, userID, id); err != nil {
		return ErrUnavailable
	}
	if err := tx.Commit(ctx); err != nil {
		return ErrUnavailable
	}
	return nil
}

func (s *Store) LoginWithAPIKey(ctx context.Context, plain string) (User, string, error) {
	if s.pool == nil {
		return User{}, "", ErrUnavailable
	}
	if !strings.HasPrefix(plain, "ypk_") || len(plain) != 47 {
		return User{}, "", ErrUnauthorized
	}
	secret, err := base64.RawURLEncoding.DecodeString(plain[4:])
	if err != nil || len(secret) != 32 {
		return User{}, "", ErrUnauthorized
	}
	hash := sha256.Sum256([]byte(plain))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, "", ErrUnavailable
	}
	defer tx.Rollback(context.Background())
	var user User
	var keyID string
	err = tx.QueryRow(ctx, `SELECT k.id,u.id,u.email,u.name,u.role,u.team_id,u.active
		FROM yuanpu_api_keys k JOIN yuanpu_users u ON u.id=k.user_id
		WHERE k.key_hash=$1 AND k.revoked_at IS NULL AND k.expires_at>now() AND u.active
		FOR UPDATE OF k`, hash[:],
	).Scan(&keyID, &user.ID, &user.Email, &user.Name, &user.Role, &user.TeamID, &user.Active)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, "", ErrUnauthorized
	}
	if err != nil {
		return User{}, "", ErrUnavailable
	}
	sessionSecret := make([]byte, 32)
	if _, err := rand.Read(sessionSecret); err != nil {
		return User{}, "", ErrUnavailable
	}
	token := base64.RawURLEncoding.EncodeToString(sessionSecret)
	tokenHash := sha256.Sum256(sessionSecret)
	if _, err := tx.Exec(ctx, `INSERT INTO yuanpu_sessions (token_hash,user_id,api_key_id,expires_at)
		VALUES ($1,$2,$3,$4)`, tokenHash[:], user.ID, keyID, time.Now().Add(12*time.Hour)); err != nil {
		return User{}, "", ErrUnavailable
	}
	if _, err := tx.Exec(ctx, `UPDATE yuanpu_api_keys SET last_used_at=now() WHERE id=$1`, keyID); err != nil {
		return User{}, "", ErrUnavailable
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, "", ErrUnavailable
	}
	return user, token, nil
}

package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrUnavailable     = errors.New("identity database unavailable")
	ErrUnauthorized    = errors.New("invalid credentials")
	ErrConflict        = errors.New("identity already exists")
	ErrInvalid         = errors.New("invalid identity input")
	ErrNotFound        = errors.New("identity not found")
	ErrBootstrapClosed = errors.New("bootstrap is available only before the first account")
)

type User struct {
	ID     string  `json:"id"`
	Email  string  `json:"email"`
	Name   string  `json:"name"`
	Role   string  `json:"role"`
	TeamID *string `json:"teamId"`
	Active bool    `json:"active"`
}

type Team struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Store struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func newID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func validRole(role string) bool {
	switch role {
	case "admin", "reviewer", "team_lead", "member":
		return true
	}
	return false
}

func normalizedEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

func validateUser(user User) bool {
	return len(user.Email) >= 3 && len(user.Email) <= 254 && strings.Contains(user.Email, "@") &&
		len(strings.TrimSpace(user.Name)) > 0 && len(user.Name) <= 120 && validRole(user.Role) &&
		(user.Role != "team_lead" || user.TeamID != nil)
}

func databaseError(err error) error {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && (pgerr.Code == "23505" || pgerr.Code == "23503" || pgerr.Code == "23514") {
		return ErrConflict
	}
	return ErrUnavailable
}

func (s *Store) BootstrapAdmin(ctx context.Context, email, name, password string) (User, error) {
	user := User{Email: normalizedEmail(email), Name: strings.TrimSpace(name), Role: "admin", Active: true}
	if !validateUser(user) {
		return User{}, ErrInvalid
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, ErrInvalid
	}
	if s.pool == nil {
		return User{}, ErrUnavailable
	}
	user.ID, err = newID()
	if err != nil {
		return User{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, ErrUnavailable
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(796873211)"); err != nil {
		return User{}, ErrUnavailable
	}
	var count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM yuanpu_users").Scan(&count); err != nil {
		return User{}, ErrUnavailable
	}
	if count != 0 {
		return User{}, ErrBootstrapClosed
	}
	if _, err := tx.Exec(ctx, `INSERT INTO yuanpu_users (id,email,name,role,password_hash) VALUES ($1,$2,$3,'admin',$4)`, user.ID, user.Email, user.Name, hash); err != nil {
		return User{}, databaseError(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO yuanpu_identity_audit (subject_id,action) VALUES ($1,'bootstrap_admin')`, user.ID); err != nil {
		return User{}, ErrUnavailable
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, ErrUnavailable
	}
	return user, nil
}

func (s *Store) CreateTeam(ctx context.Context, actor User, name string) (Team, error) {
	if actor.Role != "admin" || !actor.Active {
		return Team{}, ErrUnauthorized
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 120 {
		return Team{}, ErrInvalid
	}
	if s.pool == nil {
		return Team{}, ErrUnavailable
	}
	id, err := newID()
	if err != nil {
		return Team{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Team{}, ErrUnavailable
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `INSERT INTO yuanpu_teams (id,name) VALUES ($1,$2)`, id, name); err != nil {
		return Team{}, databaseError(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO yuanpu_identity_audit (actor_id,action,details) VALUES ($1,'create_team',jsonb_build_object('teamId',$2::text))`, actor.ID, id); err != nil {
		return Team{}, ErrUnavailable
	}
	if err := tx.Commit(ctx); err != nil {
		return Team{}, ErrUnavailable
	}
	return Team{ID: id, Name: name}, nil
}

func (s *Store) CreateUser(ctx context.Context, actor User, email, name, role string, teamID *string) (User, string, error) {
	if actor.Role != "admin" || !actor.Active {
		return User{}, "", ErrUnauthorized
	}
	user := User{Email: normalizedEmail(email), Name: strings.TrimSpace(name), Role: role, TeamID: teamID, Active: true}
	if !validateUser(user) {
		return User{}, "", ErrInvalid
	}
	if s.pool == nil {
		return User{}, "", ErrUnavailable
	}
	var err error
	user.ID, err = newID()
	if err != nil {
		return User{}, "", err
	}
	secret := make([]byte, 24)
	if _, err := rand.Read(secret); err != nil {
		return User{}, "", err
	}
	initialPassword := base64.RawURLEncoding.EncodeToString(secret)
	hash, err := HashPassword(initialPassword)
	if err != nil {
		return User{}, "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, "", ErrUnavailable
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `INSERT INTO yuanpu_users (id,email,name,role,team_id,password_hash) VALUES ($1,$2,$3,$4,$5,$6)`, user.ID, user.Email, user.Name, user.Role, user.TeamID, hash); err != nil {
		return User{}, "", databaseError(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO yuanpu_identity_audit (actor_id,subject_id,action) VALUES ($1,$2,'create_user')`, actor.ID, user.ID); err != nil {
		return User{}, "", ErrUnavailable
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, "", ErrUnavailable
	}
	return user, initialPassword, nil
}

func (s *Store) Login(ctx context.Context, email, password string) (User, string, error) {
	if s.pool == nil {
		return User{}, "", ErrUnavailable
	}
	var user User
	var hash string
	err := s.pool.QueryRow(ctx, `SELECT id,email,name,role,team_id,active,password_hash FROM yuanpu_users WHERE email=$1`, normalizedEmail(email)).Scan(&user.ID, &user.Email, &user.Name, &user.Role, &user.TeamID, &user.Active, &hash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return User{}, "", ErrUnavailable
	}
	if err != nil || !user.Active || !VerifyPassword(hash, password) {
		return User{}, "", ErrUnauthorized
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return User{}, "", err
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	tokenHash := sha256.Sum256(secret)
	if _, err := s.pool.Exec(ctx, `INSERT INTO yuanpu_sessions (token_hash,user_id,expires_at) VALUES ($1,$2,$3)`, tokenHash[:], user.ID, time.Now().Add(12*time.Hour)); err != nil {
		return User{}, "", ErrUnavailable
	}
	return user, token, nil
}

func (s *Store) SessionUser(ctx context.Context, token string) (User, error) {
	if s.pool == nil {
		return User{}, ErrUnavailable
	}
	secret, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(secret) != 32 {
		return User{}, ErrUnauthorized
	}
	hash := sha256.Sum256(secret)
	var user User
	err = s.pool.QueryRow(ctx, `SELECT u.id,u.email,u.name,u.role,u.team_id,u.active
		FROM yuanpu_sessions s
		JOIN yuanpu_users u ON u.id=s.user_id
		LEFT JOIN yuanpu_api_keys k ON k.id=s.api_key_id
		WHERE s.token_hash=$1 AND s.revoked_at IS NULL AND s.expires_at>now() AND u.active
		AND (s.api_key_id IS NULL OR (k.revoked_at IS NULL AND k.expires_at>now()))`,
		hash[:]).Scan(&user.ID, &user.Email, &user.Name, &user.Role, &user.TeamID, &user.Active)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUnauthorized
	}
	if err != nil {
		return User{}, ErrUnavailable
	}
	return user, nil
}

func (s *Store) RevokeSession(ctx context.Context, token string) error {
	if s.pool == nil {
		return ErrUnavailable
	}
	secret, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(secret) != 32 {
		return ErrUnauthorized
	}
	hash := sha256.Sum256(secret)
	if _, err := s.pool.Exec(ctx, `UPDATE yuanpu_sessions SET revoked_at=now() WHERE token_hash=$1`, hash[:]); err != nil {
		return ErrUnavailable
	}
	return nil
}

func (s *Store) ListUsers(ctx context.Context, actor User) ([]User, error) {
	if actor.Role != "admin" || !actor.Active {
		return nil, ErrUnauthorized
	}
	if s.pool == nil {
		return nil, ErrUnavailable
	}
	rows, err := s.pool.Query(ctx, `SELECT id,email,name,role,team_id,active FROM yuanpu_users ORDER BY name,email`)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rows.Close()
	users := make([]User, 0)
	for rows.Next() {
		var user User
		if err := rows.Scan(&user.ID, &user.Email, &user.Name, &user.Role, &user.TeamID, &user.Active); err != nil {
			return nil, ErrUnavailable
		}
		users = append(users, user)
	}
	if rows.Err() != nil {
		return nil, ErrUnavailable
	}
	return users, nil
}

func (s *Store) ListTeams(ctx context.Context, actor User) ([]Team, error) {
	if actor.Role != "admin" || !actor.Active {
		return nil, ErrUnauthorized
	}
	if s.pool == nil {
		return nil, ErrUnavailable
	}
	rows, err := s.pool.Query(ctx, `SELECT id,name FROM yuanpu_teams ORDER BY name`)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rows.Close()
	teams := make([]Team, 0)
	for rows.Next() {
		var team Team
		if err := rows.Scan(&team.ID, &team.Name); err != nil {
			return nil, ErrUnavailable
		}
		teams = append(teams, team)
	}
	if rows.Err() != nil {
		return nil, ErrUnavailable
	}
	return teams, nil
}

func (s *Store) UpdateUser(ctx context.Context, actor User, id, role string, teamID *string, active bool) (User, error) {
	if actor.Role != "admin" || !actor.Active {
		return User{}, ErrUnauthorized
	}
	if id == "" || !validRole(role) || (role == "team_lead" && teamID == nil) {
		return User{}, ErrInvalid
	}
	if s.pool == nil {
		return User{}, ErrUnavailable
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, ErrUnavailable
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(796873212)"); err != nil {
		return User{}, ErrUnavailable
	}
	var oldRole string
	var oldActive bool
	err = tx.QueryRow(ctx, `SELECT role,active FROM yuanpu_users WHERE id=$1 FOR UPDATE`, id).Scan(&oldRole, &oldActive)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, ErrUnavailable
	}
	if oldRole == "admin" && oldActive && (role != "admin" || !active) {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM yuanpu_users WHERE role='admin' AND active`).Scan(&count); err != nil {
			return User{}, ErrUnavailable
		}
		if count <= 1 {
			return User{}, ErrConflict
		}
	}
	var user User
	err = tx.QueryRow(ctx, `UPDATE yuanpu_users SET role=$2,team_id=$3,active=$4,updated_at=now() WHERE id=$1 RETURNING id,email,name,role,team_id,active`, id, role, teamID, active).Scan(&user.ID, &user.Email, &user.Name, &user.Role, &user.TeamID, &user.Active)
	if err != nil {
		return User{}, databaseError(err)
	}
	if !active {
		if _, err := tx.Exec(ctx, `UPDATE yuanpu_sessions SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, id); err != nil {
			return User{}, ErrUnavailable
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO yuanpu_identity_audit (actor_id,subject_id,action,details) VALUES ($1,$2,'update_user',jsonb_build_object('role',$3::text,'active',$4::boolean,'teamId',$5::text))`, actor.ID, id, role, active, teamID); err != nil {
		return User{}, ErrUnavailable
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, ErrUnavailable
	}
	return user, nil
}

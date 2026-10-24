// Package store is the PostgreSQL persistence layer of the identity slice:
// users, departments, workspaces and memberships, API keys, signing keys and
// the synchronous security-audit writer. It is the single source of truth for
// revocation: every authentication re-reads the key row, so revocation is
// effective immediately (acceptance window zero, ADR-0002).
package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/governance"
	"enterprise-agent-platform/packages/identity"
)

// Store is a pgx-backed store. All methods are safe for concurrent use via
// the pgxpool.
type Store struct {
	pool       *pgxpool.Pool
	Governance *governance.ControlData
	// tokenizeMemo is the bounded process-local vault memo (TASK-043).
	tokenizeMemo *tokenizeMemo
}

// New wraps a pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, Governance: governance.NewControlData(pool), tokenizeMemo: newTokenizeMemo()}
}

// User is a synced employee identity.
type User struct {
	ID             string
	Email          string
	DisplayName    string
	OrganizationID string
	DepartmentID   string
}

// IDClaims is the normalized identity-provider claim set that drives
// user/department sync, regardless of which adapter produced it.
type IDClaims struct {
	Subject        string // stable IdP subject
	Email          string
	DisplayName    string
	OrganizationID string
	DepartmentID   string
}

// UpsertUser syncs one employee from IdP claims (TASK-023). The IdP subject
// is the primary anchor: a first login with no directory email JIT-provisions
// a subject-only row. A verified IdP email that already belongs to an
// employee row links the incoming subject to it (directory reconciliation).
// The email is IdP-asserted — the browser never supplies it.
func (s *Store) UpsertUser(ctx context.Context, c IDClaims) (User, error) {
	if c.Subject == "" {
		return User{}, errors.New("idp claims require subject")
	}
	if c.OrganizationID == "" {
		c.OrganizationID = "org_default"
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, fmt.Errorf("upsert user: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Subject-first upsert: idempotent per subject. An IdP-asserted email is
	// only written when present (empty stays empty — it never overwrites a
	// provisioned address); email-based linking applies only when the claims
	// carry one.
	var u User
	if c.Email != "" {
		var linkedID string
		err = tx.QueryRow(ctx,
			`SELECT id FROM users WHERE lower(email) = lower($1) AND idp_subject <> $2 LIMIT 1`,
			c.Email, c.Subject).Scan(&linkedID)
		switch {
		case err == nil:
			if _, err := tx.Exec(ctx, `
				UPDATE users SET idp_subject = $1, display_name = $2,
					organization_id = $3, department_id = $4, updated_at = now()
				WHERE id = $5`,
				c.Subject, c.DisplayName, c.OrganizationID, c.DepartmentID, linkedID); err != nil {
				return User{}, fmt.Errorf("upsert user: %w", err)
			}
		case errors.Is(err, pgx.ErrNoRows):
			// fall through to the idempotent subject upsert below
		default:
			return User{}, fmt.Errorf("upsert user: %w", err)
		}
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO users (idp_subject, email, display_name, organization_id, department_id)
		VALUES ($1, COALESCE(NULLIF(lower($2), ''), ''), $3, $4, $5)
		ON CONFLICT (idp_subject) DO UPDATE
		    SET display_name = EXCLUDED.display_name,
		        organization_id = EXCLUDED.organization_id,
		        department_id = EXCLUDED.department_id,
		        email = CASE WHEN EXCLUDED.email = '' THEN users.email ELSE EXCLUDED.email END,
		        updated_at = now()
		RETURNING id, email, display_name, organization_id, department_id`,
		c.Subject, c.Email, c.DisplayName, c.OrganizationID, c.DepartmentID)
	err = row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.OrganizationID, &u.DepartmentID)
	if err != nil {
		return User{}, fmt.Errorf("upsert user: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, fmt.Errorf("upsert user: %w", err)
	}
	return u, nil
}

// GetUser fetches one user.
func (s *Store) GetUser(ctx context.Context, id string) (User, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, email, display_name, organization_id, department_id FROM users WHERE id = $1`, id)
	var u User
	if err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.OrganizationID, &u.DepartmentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, fmt.Errorf("user %s not found: %w", id, ErrNotFound)
		}
		return User{}, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

// ErrNotFound marks missing rows.
var ErrNotFound = errors.New("not found")

// IsMember implements identity.MembershipResolver against workspace_members.
// No row means NOT a member (removed/deprovisioned) — a clean denial, not an
// error. A storage error must fail the request closed upstream.
func (s *Store) IsMember(ctx context.Context, userID, workspaceID string) (identity.Membership, error) {
	var exists bool
	var roles []string
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM workspace_members WHERE user_id = $1 AND workspace_id = $2
		) AS is_member,
		(SELECT roles FROM workspace_members WHERE user_id = $1 AND workspace_id = $2 LIMIT 1)`,
		userID, workspaceID).Scan(&exists, &roles)
	if err != nil {
		return identity.Membership{}, fmt.Errorf("membership check: %w", err)
	}
	return identity.Membership{Member: exists, Roles: roles}, nil
}

// Membership pairs a workspace with the user's roles in it.
type Membership struct {
	WorkspaceID string
	Name        string
	Default     bool
	Roles       []string
}

// ListMemberships returns every workspace the user belongs to, default first.
func (s *Store) ListMemberships(ctx context.Context, userID string) ([]Membership, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT w.id, w.name, COALESCE(w.is_default, false), COALESCE(m.roles, '{}')
		FROM workspace_members m
		JOIN workspaces w ON w.id = m.workspace_id
		WHERE m.user_id = $1
		ORDER BY w.is_default DESC, w.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list memberships: %w", err)
	}
	defer rows.Close()
	var out []Membership
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.WorkspaceID, &m.Name, &m.Default, &m.Roles); err != nil {
			return nil, fmt.Errorf("scan membership: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SyncMemberships replaces the user's workspace memberships with the given
// workspace ids (driven by IdP group claims via the adapter boundary).
// Roles merge additively (existing ∪ provided) so platform-level grants
// such as admin survive a membership re-sync; unlisted workspaces are
// revoked.
func (s *Store) SyncMemberships(ctx context.Context, userID string, workspaceIDs []string, roles []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin membership sync: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, ws := range workspaceIDs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workspace_members (workspace_id, user_id, roles)
			VALUES ($1, $2, $3)
			ON CONFLICT (workspace_id, user_id) DO UPDATE
			    SET roles = (
			        SELECT array_agg(DISTINCT r) FROM unnest(
			            workspace_members.roles || $3) AS r
			    )`,
			ws, userID, roles); err != nil {
			return fmt.Errorf("membership upsert %s: %w", ws, err)
		}
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM workspace_members
		WHERE user_id = $1 AND NOT (workspace_id = ANY($2))`,
		userID, workspaceIDs); err != nil {
		return fmt.Errorf("revoke unlisted memberships: %w", err)
	}
	return tx.Commit(ctx)
}

// APIKey is stored key metadata. The hash never leaves this package's callers
// as a display value and the full key exists only at creation time.
type APIKey struct {
	KeyID      string
	UserID     string
	Prefix     string
	Name       string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	LastIP     *string
	ClientID   *string
	RevokedAt  *time.Time
	// WorkspaceID is the optional server-side workspace binding (ADR-0024).
	// nil keeps the key unbound: workspace selection stays header-only.
	WorkspaceID *string
}

// CreateAPIKey inserts the key row and its security audit event atomically:
// either both commit or neither does, so no key exists without audit evidence.
// keyHash is the sha256 hex of the full key value. workspaceID optionally
// binds the key to a workspace (ADR-0024); the caller must have verified the
// owner's membership server-side before passing it.
func (s *Store) CreateAPIKey(ctx context.Context, userID, keyHash, prefix, name, workspaceID string, expiresAt *time.Time, created audit.Event) (APIKey, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return APIKey{}, fmt.Errorf("begin create api key: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx, `
		INSERT INTO api_keys (key_hash, prefix, user_id, name, workspace_id, expires_at)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6)
		RETURNING key_id, user_id, prefix, name, created_at, expires_at, last_used_at, last_ip, client_id, revoked_at, workspace_id`,
		keyHash, prefix, userID, name, workspaceID, expiresAt)
	k, err := scanAPIKey(row)
	if err != nil {
		return APIKey{}, err
	}
	created.Resource = "api-key/" + k.KeyID
	created.WorkspaceID = ""
	if created.Actor == nil {
		created.Actor = &identity.Principal{UserID: userID}
	}
	if err := insertAuditEvent(ctx, tx, created); err != nil {
		return APIKey{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return APIKey{}, fmt.Errorf("commit create api key: %w", err)
	}
	return k, nil
}

// APIKeyAuth is the single-round-trip auth-chain result (TASK-038): the
// presented key row and its owner resolved together. Revocation/expiry are
// still read from live columns on every request — nothing about the auth
// contract changes, only the round-trip count.
type APIKeyAuth struct {
	Key  APIKey
	User User
	// Memberships carries the live membership rows for the two candidate
	// workspaces of this request (the client header workspace and the key's
	// binding workspace), fetched in the same query (TASK-043). An absent
	// entry means "not a member" — the gateway still applies the frozen
	// header -> binding -> fail-closed precedence in Go; nothing about
	// selection or revocation semantics moves into SQL.
	Memberships map[string]identity.Membership
}

// GetAPIKeyAuthByHash resolves a presented credential to its key + owner +
// the live memberships of both candidate workspaces in ONE query (TASK-043
// extends TASK-038). headerWorkspace selects which client-declared workspace
// membership to fetch; the key's binding workspace is joined from the key row
// itself. An unknown key and a key whose owner vanished are both ErrNotFound
// (fail closed upstream). Revocation/expiry/membership are still judged from
// these live rows on every request.
func (s *Store) GetAPIKeyAuthByHash(ctx context.Context, keyHash, headerWorkspace string) (APIKeyAuth, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT k.key_id, k.user_id, k.prefix, k.name, k.created_at, k.expires_at, k.last_used_at, k.last_ip, k.client_id, k.revoked_at, k.workspace_id,
		       u.id, u.email, u.display_name, u.organization_id, u.department_id,
		       hm.roles, bm.roles
		FROM api_keys k
		JOIN users u ON u.id = k.user_id
		LEFT JOIN workspace_members hm ON hm.user_id = k.user_id AND hm.workspace_id = NULLIF($2, '')
		LEFT JOIN workspace_members bm ON bm.user_id = k.user_id AND bm.workspace_id = k.workspace_id
		WHERE k.key_hash = $1`, keyHash, strings.TrimSpace(headerWorkspace))
	var a APIKeyAuth
	var hmRoles, bmRoles []string
	err := row.Scan(&a.Key.KeyID, &a.Key.UserID, &a.Key.Prefix, &a.Key.Name, &a.Key.CreatedAt, &a.Key.ExpiresAt,
		&a.Key.LastUsedAt, &a.Key.LastIP, &a.Key.ClientID, &a.Key.RevokedAt, &a.Key.WorkspaceID,
		&a.User.ID, &a.User.Email, &a.User.DisplayName, &a.User.OrganizationID, &a.User.DepartmentID,
		&hmRoles, &bmRoles)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return APIKeyAuth{}, ErrNotFound
		}
		return APIKeyAuth{}, fmt.Errorf("get api key auth: %w", err)
	}
	a.Memberships = map[string]identity.Membership{}
	if hm := strings.TrimSpace(headerWorkspace); hm != "" && hmRoles != nil {
		a.Memberships[hm] = identity.Membership{Member: true, Roles: hmRoles}
	}
	if a.Key.WorkspaceID != nil && *a.Key.WorkspaceID != "" && bmRoles != nil {
		a.Memberships[*a.Key.WorkspaceID] = identity.Membership{Member: true, Roles: bmRoles}
	}
	return a, nil
}

// GetAPIKeyByHash resolves a presented credential to its row.
func (s *Store) GetAPIKeyByHash(ctx context.Context, keyHash string) (APIKey, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT key_id, user_id, prefix, name, created_at, expires_at, last_used_at, last_ip, client_id, revoked_at, workspace_id
		FROM api_keys WHERE key_hash = $1`, keyHash)
	return scanAPIKey(row)
}

// ListAPIKeys returns the owner's keys, newest first.
func (s *Store) ListAPIKeys(ctx context.Context, userID string) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT key_id, user_id, prefix, name, created_at, expires_at, last_used_at, last_ip, client_id, revoked_at, workspace_id
		FROM api_keys WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey revokes one of the user's own keys and records the audit event
// in the same transaction, so a revocation can never exist without evidence.
// Returns whether a live key row was updated.
func (s *Store) RevokeAPIKey(ctx context.Context, userID, keyID string, revoked audit.Event) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin revoke api key: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		`UPDATE api_keys SET revoked_at = now() WHERE key_id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		keyID, userID)
	if err != nil {
		return false, fmt.Errorf("revoke api key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	revoked.Resource = "api-key/" + keyID
	if revoked.Actor == nil {
		revoked.Actor = &identity.Principal{UserID: userID}
	}
	if err := insertAuditEvent(ctx, tx, revoked); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit revoke api key: %w", err)
	}
	return true, nil
}

// TouchAPIKey records usage metadata. Best-effort: callers log errors.
func (s *Store) TouchAPIKey(ctx context.Context, keyID, lastIP, clientID string) {
	_, _ = s.pool.Exec(ctx,
		`UPDATE api_keys SET last_used_at = now(), last_ip = $2, client_id = $3 WHERE key_id = $1`,
		keyID, lastIP, clientID)
}

type rowScanner interface{ Scan(dest ...any) error }

func scanAPIKey(row rowScanner) (APIKey, error) {
	var k APIKey
	err := row.Scan(&k.KeyID, &k.UserID, &k.Prefix, &k.Name, &k.CreatedAt, &k.ExpiresAt,
		&k.LastUsedAt, &k.LastIP, &k.ClientID, &k.RevokedAt, &k.WorkspaceID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return APIKey{}, ErrNotFound
		}
		return APIKey{}, fmt.Errorf("scan api key: %w", err)
	}
	return k, nil
}

// SigningKey is the active internal-token signing material loaded from the
// server-side store. Private keys live in PostgreSQL (Zone B secret, managed
// reference candidate for Vault/KMS later, ADR-0002).
type SigningKey struct {
	Kid        string
	PrivateKey ed25519.PrivateKey
	PublicKey  ed25519.PublicKey
	CreatedAt  time.Time
}

// EnsureSigningKey returns the active signing key, generating and persisting
// one when none exists. generate must return (kid, PKCS8 DER private key).
func (s *Store) EnsureSigningKey(ctx context.Context, generate func() (string, []byte, error)) (SigningKey, error) {
	row := s.pool.QueryRow(ctx, `SELECT kid, private_pkcs8, created_at FROM signing_keys WHERE active ORDER BY created_at DESC LIMIT 1`)
	var sk SigningKey
	var der []byte
	err := row.Scan(&sk.Kid, &der, &sk.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		kid, newDER, gerr := generate()
		if gerr != nil {
			return SigningKey{}, fmt.Errorf("generate signing key: %w", gerr)
		}
		if _, ierr := s.pool.Exec(ctx,
			`INSERT INTO signing_keys (kid, private_pkcs8, active) VALUES ($1, $2, true)`, kid, newDER); ierr != nil {
			return SigningKey{}, fmt.Errorf("persist signing key: %w", ierr)
		}
		sk.Kid, sk.CreatedAt = kid, time.Now().UTC()
		der = newDER
	} else if err != nil {
		return SigningKey{}, fmt.Errorf("load signing key: %w", err)
	}

	key, kerr := x509.ParsePKCS8PrivateKey(der)
	if kerr != nil {
		return SigningKey{}, fmt.Errorf("parse signing key: %w", kerr)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return SigningKey{}, errors.New("stored signing key is not ed25519")
	}
	sk.PrivateKey = priv
	sk.PublicKey = priv.Public().(ed25519.PublicKey)
	return sk, nil
}

// ListSigningPublicKeys returns all public keys (active and recent) for JWKS.
func (s *Store) ListSigningPublicKeys(ctx context.Context) ([]SigningKey, error) {
	rows, err := s.pool.Query(ctx, `SELECT kid, private_pkcs8, created_at FROM signing_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list signing keys: %w", err)
	}
	defer rows.Close()
	var out []SigningKey
	for rows.Next() {
		var sk SigningKey
		var der []byte
		if err := rows.Scan(&sk.Kid, &der, &sk.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan signing key: %w", err)
		}
		key, kerr := x509.ParsePKCS8PrivateKey(der)
		if kerr != nil {
			return nil, fmt.Errorf("parse signing key %s: %w", sk.Kid, kerr)
		}
		priv, ok := key.(ed25519.PrivateKey)
		if !ok {
			continue
		}
		sk.PrivateKey = priv
		sk.PublicKey = priv.Public().(ed25519.PublicKey)
		out = append(out, sk)
	}
	return out, rows.Err()
}

// Write implements audit.Writer as the reliable synchronous path: the INSERT
// happens inside the request; a storage failure surfaces as an error so
// RequireWrite can fail the operation closed.
// WriteGovernanceBatch delivers the audit event set AND the usage ledger
// row of one request in a single transaction (TASK-041, ADR-0027): one
// durable commit instead of two. All-or-nothing — usage inherits the
// audit's fail-closed durability, which the practical failure envelope
// already implied (a database down fails both).
func (s *Store) WriteGovernanceBatch(ctx context.Context, events []audit.Event, usage *governance.UsageRecord) error {
	if len(events) == 0 && usage == nil {
		return nil
	}
	if usage == nil {
		return s.WriteBatch(ctx, events)
	}
	// TASK-043: BEGIN + audit batch INSERT + usage INSERT + COMMIT travel as
	// ONE pgx batch (single flush, one round trip) instead of a four-round-
	// -trip explicit transaction. Statements stay parameterized; semantics
	// stay all-or-nothing — any failure rolls the batch back.
	auditSQL, auditArgs, err := auditBatchStmt(events)
	if err != nil {
		return err
	}
	usageSQL, usageArgs := governance.UsageInsertSQL(*usage)

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("governance batch acquire: %w", err)
	}
	defer conn.Release()

	batch := &pgx.Batch{}
	batch.Queue("BEGIN")
	batch.Queue(auditSQL, auditArgs...)
	batch.Queue(usageSQL, usageArgs...)
	batch.Queue("COMMIT")

	br := conn.SendBatch(ctx, batch)
	var batchErr error
	for i := 0; i < batch.Len(); i++ {
		if _, batchErr = br.Exec(); batchErr != nil {
			break
		}
	}
	if closeErr := br.Close(); batchErr == nil {
		batchErr = closeErr
	}
	if batchErr != nil {
		if _, rbErr := conn.Exec(ctx, "ROLLBACK"); rbErr != nil {
			return fmt.Errorf("governance batch failed and rollback failed: %w (rollback: %v)", batchErr, rbErr)
		}
		return fmt.Errorf("governance batch: %w", batchErr)
	}
	return nil
}

// WriteBatchTx delivers the audit event set inside a caller-owned
// transaction (TASK-041 governance batch).
func (s *Store) WriteBatchTx(ctx context.Context, tx pgx.Tx, events []audit.Event) error {
	return writeAuditBatch(ctx, tx, events)
}

// WriteBatch delivers multiple audit events in ONE statement (TASK-038).
// It is all-or-nothing: any failure means the caller must treat the whole
// set as undelivered (fail closed). Event order is preserved.
func (s *Store) WriteBatch(ctx context.Context, events []audit.Event) error {
	return writeAuditBatch(ctx, s.pool, events)
}

func writeAuditBatch(ctx context.Context, q querier, events []audit.Event) error {
	if len(events) == 0 {
		return nil
	}
	sql, args, err := auditBatchStmt(events)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, sql, args...)
	return err
}

// auditBatchStmt renders the multi-row audit insert with bound arguments so
// both the standalone batch (TASK-038) and the single-round-trip governance
// batch (TASK-043) share one statement shape.
func auditBatchStmt(events []audit.Event) (string, []any, error) {
	if len(events) == 0 {
		return "", nil, fmt.Errorf("audit batch: no events")
	}
	var sb strings.Builder
	sb.WriteString(`INSERT INTO audit_events (id, type, outcome, actor_user_id, workspace_id, trace_id, request_id, resource, detail, time) VALUES `)
	args := make([]any, 0, len(events)*10)
	for i, e := range events {
		if i > 0 {
			sb.WriteByte(',')
		}
		b := i * 10
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d::jsonb,$%d)",
			b+1, b+2, b+3, b+4, b+5, b+6, b+7, b+8, b+9, b+10)
		var actor *string
		if e.Actor != nil {
			actor = &e.Actor.UserID
		}
		detail := "{}"
		if e.Detail != nil {
			if jb, jerr := jsonMarshal(e.Detail); jerr == nil {
				detail = jb
			}
		}
		args = append(args, e.ID, string(e.Type), string(e.Outcome), actor, e.WorkspaceID, e.TraceID, e.RequestID, e.Resource, detail, e.Time)
	}
	return sb.String(), args, nil
}

func (s *Store) Write(ctx context.Context, e audit.Event) error {
	if err := insertAuditEvent(ctx, s.pool, e); err != nil {
		return fmt.Errorf("write audit event: %w", err)
	}
	return nil
}

// querier matches both *pgxpool.Pool and pgx.Tx for shared statement helpers.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insertAuditEvent(ctx context.Context, q querier, e audit.Event) error {
	var actor *string
	if e.Actor != nil {
		actor = &e.Actor.UserID
	}
	detail := "{}"
	if e.Detail != nil {
		if b, err := jsonMarshal(e.Detail); err == nil {
			detail = b
		}
	}
	_, err := q.Exec(ctx, `
		INSERT INTO audit_events (id, type, outcome, actor_user_id, workspace_id, trace_id, request_id, resource, detail, time)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10)`,
		e.ID, string(e.Type), string(e.Outcome), actor, e.WorkspaceID, e.TraceID, e.RequestID, e.Resource, detail, e.Time)
	return err
}

// GenerateSigningKeyDER is the default key generator: fresh Ed25519 keypair,
// kid = "k-" + 8 random hex chars, private key as PKCS8 DER.
func GenerateSigningKeyDER() (string, []byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", nil, err
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, err
	}
	kid := fmt.Sprintf("k-%x", b)
	return kid, der, nil
}

// Migrate applies migrations/*.sql in filename order, tracking applied files
// in schema_migrations.
func Migrate(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT exists(SELECT 1 FROM schema_migrations WHERE version = $1)`, name).Scan(&exists); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if exists {
			continue
		}
		sqlBytes, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

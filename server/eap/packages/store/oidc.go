package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"enterprise-agent-platform/packages/errs"
)

// Pool exposes the underlying pgx pool for gateway session/flow SQL.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// ResolveWorkspacesForGroups maps IdP group names to internal workspace ids
// through the explicit idp_group_workspaces mapping. An unmapped group never
// becomes workspace authority (ADR-0009).
// GroupBinding is one mapped workspace and the roles the IdP groups grant
// in it (union across every mapped group that reaches the workspace).
type GroupBinding struct {
	WorkspaceID string
	Roles       []string
}

// ResolveGroupBindings maps IdP group claims through the operator table to
// per-workspace role bindings. Unmapped groups never become authority
// (ADR-0009); roles aggregate additively across overlapping groups.
func (s *Store) ResolveGroupBindings(ctx context.Context, groups []string) ([]GroupBinding, error) {
	if len(groups) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT workspace_id, roles FROM idp_group_workspaces WHERE idp_group = ANY($1)`, groups)
	if err != nil {
		return nil, errs.Wrap(err, errs.Internal, "group mapping lookup failed")
	}
	defer rows.Close()
	order := []string{}
	bindings := map[string]*GroupBinding{}
	for rows.Next() {
		var ws string
		var roles []string
		if err := rows.Scan(&ws, &roles); err != nil {
			return nil, errs.Wrap(err, errs.Internal, "group mapping scan failed")
		}
		b, ok := bindings[ws]
		if !ok {
			b = &GroupBinding{WorkspaceID: ws, Roles: []string{}}
			bindings[ws] = b
			order = append(order, ws)
		}
		seen := map[string]bool{}
		for _, r := range b.Roles {
			seen[r] = true
		}
		for _, r := range roles {
			if !seen[r] {
				b.Roles = append(b.Roles, r)
				seen[r] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.Internal, "group mapping iteration failed")
	}
	out := make([]GroupBinding, 0, len(order))
	for _, ws := range order {
		out = append(out, *bindings[ws])
	}
	return out, nil
}

// SyncGroupBindings applies the IdP-derived bindings additively: mapped
// workspaces are upserted with roles = existing ∪ mapped (an out-of-band
// grant such as admin survives re-login); a workspace that no longer has a
// mapped binding is revoked. Roles never downgrade.
func (s *Store) SyncGroupBindings(ctx context.Context, userID string, bindings []GroupBinding) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("group binding sync: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	keep := map[string]bool{}
	for _, b := range bindings {
		keep[b.WorkspaceID] = true
		var existing []string
		err := tx.QueryRow(ctx,
			`SELECT roles FROM workspace_members WHERE workspace_id = $1 AND user_id = $2`,
			b.WorkspaceID, userID).Scan(&existing)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("membership lookup %s: %w", b.WorkspaceID, err)
		}
		merged := map[string]bool{}
		for _, r := range append(existing, b.Roles...) {
			merged[r] = true
		}
		final := make([]string, 0, len(merged))
		for r := range merged {
			final = append(final, r)
		}
		sort.Strings(final)
		if _, err := tx.Exec(ctx, `
			INSERT INTO workspace_members (workspace_id, user_id, roles)
			VALUES ($1, $2, $3)
			ON CONFLICT (workspace_id, user_id) DO UPDATE
			    SET roles = EXCLUDED.roles`,
			b.WorkspaceID, userID, final); err != nil {
			return fmt.Errorf("membership upsert %s: %w", b.WorkspaceID, err)
		}
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM workspace_members
		WHERE user_id = $1 AND NOT (workspace_id = ANY($2))`,
		userID, workspaceIDsFromBindings(bindings)); err != nil {
		return fmt.Errorf("revoke unmapped memberships: %w", err)
	}
	return tx.Commit(ctx)
}

func workspaceIDsFromBindings(bindings []GroupBinding) []string {
	out := make([]string, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, b.WorkspaceID)
	}
	return out
}

// InsertFlowState persists a single-use OIDC flow state.
func (s *Store) InsertFlowState(ctx context.Context, state, nonce, verifier string, createdAt time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO oidc_flow_states (state, nonce, code_verifier, created_at)
		VALUES ($1, $2, $3, $4)`, state, nonce, verifier, createdAt)
	if err != nil {
		return errs.Wrap(err, errs.Internal, "flow state persist failed")
	}
	return nil
}

// ConsumeFlowState atomically deletes and returns one flow state (single use;
// replay or unknown state surfaces as unauthenticated).
func (s *Store) ConsumeFlowState(ctx context.Context, state string) (nonce, verifier string, createdAt time.Time, err error) {
	err = s.pool.QueryRow(ctx, `
		DELETE FROM oidc_flow_states WHERE state = $1
		RETURNING nonce, code_verifier, created_at`, state).Scan(&nonce, &verifier, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", time.Time{}, errs.New(errs.Unauthenticated, "unknown or replayed oidc state")
	}
	if err != nil {
		return "", "", time.Time{}, errs.Wrap(err, errs.Internal, "flow state consume failed")
	}
	return nonce, verifier, createdAt, nil
}

// CreatePortalSession persists the gateway session row.
func (s *Store) CreatePortalSession(ctx context.Context, sessionID, userID, email, displayName, idpSubject string, workspaces []string, expires time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO portal_sessions (session_id, user_id, email, display_name, idp_subject, workspaces, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		sessionID, userID, email, displayName, idpSubject, workspaces, expires)
	if err != nil {
		return errs.Wrap(err, errs.Internal, "session persist failed")
	}
	return nil
}

// ValidPortalSession returns the session identity when live.
func (s *Store) ValidPortalSession(ctx context.Context, sessionID string, now time.Time) (userID, displayName string, workspaces []string, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT user_id, display_name, workspaces FROM portal_sessions
		WHERE session_id = $1 AND revoked_at IS NULL AND expires_at > $2`,
		sessionID, now).Scan(&userID, &displayName, &workspaces)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil, errs.New(errs.Unauthenticated, "portal session is invalid or expired")
	}
	if err != nil {
		return "", "", nil, errs.Wrap(err, errs.Internal, "session lookup failed")
	}
	return userID, displayName, workspaces, nil
}

// RevokePortalSession marks one session revoked.
func (s *Store) RevokePortalSession(ctx context.Context, sessionID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE portal_sessions SET revoked_at = now() WHERE session_id = $1 AND revoked_at IS NULL`, sessionID)
	if err != nil {
		return errs.Wrap(err, errs.Internal, "session revoke failed")
	}
	return nil
}

var _ = fmt.Sprintf

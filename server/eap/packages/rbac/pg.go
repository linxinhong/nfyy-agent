package rbac

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Querier is the pgx surface Load needs (pool or transaction).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Catalog loads the Workspace's roles: built-ins merged with stored edits.
func Catalog(ctx context.Context, q Querier, workspaceID string) ([]Role, error) {
	rows, err := q.Query(ctx, `
		SELECT key, name, scope, permissions FROM workspace_roles
		WHERE workspace_id = $1 ORDER BY key`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("load roles: %w", err)
	}
	defer rows.Close()
	var stored []Role
	for rows.Next() {
		var r Role
		var perms []string
		if err := rows.Scan(&r.Key, &r.Name, &r.Scope, &perms); err != nil {
			return nil, fmt.Errorf("scan role: %w", err)
		}
		for _, p := range perms {
			r.Permissions = append(r.Permissions, Permission(p))
		}
		stored = append(stored, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return Merge(stored), nil
}

// Load resolves a member's grants; roles are the Workspace role keys from the
// caller's live membership check.
func Load(ctx context.Context, q Querier, workspaceID, userID string, roles []string) (Grants, error) {
	catalog, err := Catalog(ctx, q, workspaceID)
	if err != nil {
		return Grants{}, err
	}
	var groupID string
	var teamRoles []string
	err = q.QueryRow(ctx,
		`SELECT group_id, team_roles FROM workspace_group_members WHERE workspace_id = $1 AND user_id = $2`,
		workspaceID, userID).Scan(&groupID, &teamRoles)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Grants{}, fmt.Errorf("resolve group: %w", err)
	}
	return Resolve(catalog, roles, teamRoles, groupID), nil
}

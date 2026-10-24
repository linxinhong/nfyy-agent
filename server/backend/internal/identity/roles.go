package identity

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

type RolePermission struct {
	Role   string `json:"role"`
	Action string `json:"action"`
	Scope  string `json:"scope"`
}

func validPermission(action, scope string) bool {
	switch action {
	case "create", "read", "edit", "submit", "review", "withdraw":
	default:
		return false
	}
	switch scope {
	case "none", "own", "team", "all":
		return true
	}
	return false
}

func (s *Store) rolePermissions(ctx context.Context, role string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT action,scope FROM yuanpu_role_permissions WHERE role=$1`, role)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rows.Close()
	permissions := make(map[string]string)
	for rows.Next() {
		var action, scope string
		if err := rows.Scan(&action, &scope); err != nil {
			return nil, ErrUnavailable
		}
		permissions[action] = scope
	}
	if rows.Err() != nil {
		return nil, ErrUnavailable
	}
	return permissions, nil
}

func (s *Store) ListRolePermissions(ctx context.Context, actor User) ([]RolePermission, error) {
	if actor.Role != "admin" || !actor.Active {
		return nil, ErrUnauthorized
	}
	if s.pool == nil {
		return nil, ErrUnavailable
	}
	rows, err := s.pool.Query(ctx, `SELECT role,action,scope FROM yuanpu_role_permissions ORDER BY role,action`)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rows.Close()
	items := make([]RolePermission, 0, 24)
	for rows.Next() {
		var item RolePermission
		if err := rows.Scan(&item.Role, &item.Action, &item.Scope); err != nil {
			return nil, ErrUnavailable
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		return nil, ErrUnavailable
	}
	return items, nil
}

func (s *Store) UpdateRolePermission(ctx context.Context, actor User, role, action, scope string) (RolePermission, error) {
	if actor.Role != "admin" || !actor.Active {
		return RolePermission{}, ErrUnauthorized
	}
	if !validRole(role) || !validPermission(action, scope) {
		return RolePermission{}, ErrInvalid
	}
	if s.pool == nil {
		return RolePermission{}, ErrUnavailable
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RolePermission{}, ErrUnavailable
	}
	defer tx.Rollback(context.Background())
	var previous string
	err = tx.QueryRow(ctx, `SELECT scope FROM yuanpu_role_permissions WHERE role=$1 AND action=$2 FOR UPDATE`, role, action).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return RolePermission{}, ErrNotFound
	}
	if err != nil {
		return RolePermission{}, ErrUnavailable
	}
	if _, err := tx.Exec(ctx, `UPDATE yuanpu_role_permissions SET scope=$3,updated_at=now() WHERE role=$1 AND action=$2`, role, action, scope); err != nil {
		return RolePermission{}, ErrUnavailable
	}
	if _, err := tx.Exec(ctx, `INSERT INTO yuanpu_identity_audit(actor_id,action,details)
		VALUES ($1,'update_role_permission',jsonb_build_object('role',$2::text,'permission',$3::text,'from',$4::text,'to',$5::text))`,
		actor.ID, role, action, previous, scope); err != nil {
		return RolePermission{}, ErrUnavailable
	}
	if err := tx.Commit(ctx); err != nil {
		return RolePermission{}, ErrUnavailable
	}
	return RolePermission{Role: role, Action: action, Scope: scope}, nil
}

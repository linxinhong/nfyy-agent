package store

// Workspace member and group administration (docs/knowledge-platform-architecture.md §8.4).
//
// One organization runs one Workspace; teams are workspace_groups inside it.
// Roles are packages/rbac roles: Workspace roles on the membership row, team
// roles on the group row, so "a team reviewer reviews only their own team"
// stays expressible.
//
// Invariants:
//   - Every mutation commits its audit event in the same transaction: no
//     membership, role or credential change exists without audit evidence.
//   - A Workspace never loses its last admin through this API (ErrLastAdmin).
//   - Deleting a custom role strips its key from every assignment in the
//     same transaction; a built-in role can only be reset to its defaults.
//   - Removing a member deletes the workspace_members row; group rows cascade,
//     and live membership checks deny every credential on the next request.
//     When no membership remains, the member's API keys are revoked too.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/rbac"
)

var (
	// ErrConflict marks a uniqueness clash (username, email, group name).
	ErrConflict = errors.New("conflict")
	// ErrLastAdmin refuses a change that would leave a Workspace without admin.
	ErrLastAdmin = errors.New("workspace would have no admin")
	// ErrGroupInUse refuses deleting a group that still scopes team content.
	ErrGroupInUse = errors.New("group still scopes content")
	// ErrNoGroup refuses team roles for a member outside every team.
	ErrNoGroup = errors.New("team roles need a team")
)

// WorkspaceGroup is a team inside one Workspace.
type WorkspaceGroup struct {
	ID          string
	WorkspaceID string
	Name        string
	Members     int
}

// WorkspaceMember is one member's administrative view.
type WorkspaceMember struct {
	UserID      string
	Email       string
	DisplayName string
	Username    string // empty for members without a local login
	Roles       []string
	GroupID     string
	TeamRoles   []string
}

// NewLocalMember is an admin-created member with a one-time password.
type NewLocalMember struct {
	WorkspaceID  string
	Username     string
	Email        string
	DisplayName  string
	Roles        []string
	GroupID      string
	TeamRoles    []string
	PasswordHash string
}

// MemberUpdate carries optional changes; nil fields stay as they are.
// GroupID "" clears the group (and its team roles); a group change without
// TeamRoles drops the old team roles.
type MemberUpdate struct {
	Roles     *[]string
	GroupID   *string
	TeamRoles *[]string
}

// withAuditTx runs fn and commits ev in the same transaction. ev is a pointer
// so fn can fill in fields only known after its writes (e.g. a new row id).
func (s *Store) withAuditTx(ctx context.Context, ev *audit.Event, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := insertAuditEvent(ctx, tx, *ev); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ListGroups returns the Workspace's groups with member counts.
func (s *Store) ListGroups(ctx context.Context, workspaceID string) ([]WorkspaceGroup, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT g.id, g.workspace_id, g.name, count(m.user_id)
		FROM workspace_groups g
		LEFT JOIN workspace_group_members m ON m.group_id = g.id
		WHERE g.workspace_id = $1
		GROUP BY g.id
		ORDER BY g.name`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer rows.Close()
	out := []WorkspaceGroup{}
	for rows.Next() {
		var g WorkspaceGroup
		if err := rows.Scan(&g.ID, &g.WorkspaceID, &g.Name, &g.Members); err != nil {
			return nil, fmt.Errorf("scan group: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CreateGroup adds a named group to the Workspace.
func (s *Store) CreateGroup(ctx context.Context, workspaceID, name string, ev audit.Event) (WorkspaceGroup, error) {
	g := WorkspaceGroup{WorkspaceID: workspaceID, Name: name}
	err := s.withAuditTx(ctx, &ev, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`INSERT INTO workspace_groups (workspace_id, name) VALUES ($1, $2) RETURNING id`,
			workspaceID, name).Scan(&g.ID)
		if isUniqueViolation(err) {
			return ErrConflict
		}
		if err != nil {
			return fmt.Errorf("create group: %w", err)
		}
		ev.Resource = "workspace-group/" + g.ID
		return nil
	})
	return g, err
}

// RenameGroup renames a group of this Workspace.
func (s *Store) RenameGroup(ctx context.Context, workspaceID, groupID, name string, ev audit.Event) error {
	return s.withAuditTx(ctx, &ev, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE workspace_groups SET name = $3 WHERE id = $1 AND workspace_id = $2`,
			groupID, workspaceID, name)
		if isUniqueViolation(err) {
			return ErrConflict
		}
		if err != nil {
			return fmt.Errorf("rename group: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DeleteGroup removes a group; its members stay in the Workspace ungrouped.
// Team content scoped to the group blocks deletion (ErrGroupInUse).
func (s *Store) DeleteGroup(ctx context.Context, workspaceID, groupID string, ev audit.Event) error {
	return s.withAuditTx(ctx, &ev, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM workspace_groups WHERE id = $1 AND workspace_id = $2`, groupID, workspaceID)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return ErrGroupInUse
		}
		if err != nil {
			return fmt.Errorf("delete group: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ListMembers returns every member of the Workspace.
func (s *Store) ListMembers(ctx context.Context, workspaceID string) ([]WorkspaceMember, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, COALESCE(u.email, ''), u.display_name, COALESCE(c.username, ''),
		       COALESCE(m.roles, '{}'), COALESCE(g.group_id, ''), COALESCE(g.team_roles, '{}')
		FROM workspace_members m
		JOIN users u ON u.id = m.user_id
		LEFT JOIN local_credentials c ON c.user_id = u.id
		LEFT JOIN workspace_group_members g ON g.workspace_id = m.workspace_id AND g.user_id = m.user_id
		WHERE m.workspace_id = $1
		ORDER BY u.display_name, u.id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer rows.Close()
	out := []WorkspaceMember{}
	for rows.Next() {
		var m WorkspaceMember
		if err := rows.Scan(&m.UserID, &m.Email, &m.DisplayName, &m.Username, &m.Roles, &m.GroupID, &m.TeamRoles); err != nil {
			return nil, fmt.Errorf("scan member: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetMember returns one member of the Workspace or ErrNotFound.
func (s *Store) GetMember(ctx context.Context, workspaceID, userID string) (WorkspaceMember, error) {
	members, err := s.ListMembers(ctx, workspaceID)
	if err != nil {
		return WorkspaceMember{}, err
	}
	for _, m := range members {
		if m.UserID == userID {
			return m, nil
		}
	}
	return WorkspaceMember{}, ErrNotFound
}

// CreateLocalMember provisions a user, Workspace membership, optional group
// and a local login whose password must be changed before first use.
func (s *Store) CreateLocalMember(ctx context.Context, in NewLocalMember, ev audit.Event) (string, error) {
	var userID string
	err := s.withAuditTx(ctx, &ev, func(tx pgx.Tx) error {
		var orgID string
		if err := tx.QueryRow(ctx, `SELECT organization_id FROM workspaces WHERE id = $1`, in.WorkspaceID).Scan(&orgID); err != nil {
			return fmt.Errorf("workspace lookup: %w", err)
		}
		err := tx.QueryRow(ctx, `
			INSERT INTO users (idp_subject, email, display_name, organization_id, department_id)
			VALUES ($1, lower($2), $3, $4, 'dept_platform')
			RETURNING id`,
			"local:"+in.Username, in.Email, in.DisplayName, orgID).Scan(&userID)
		if isUniqueViolation(err) {
			return ErrConflict
		}
		if err != nil {
			return fmt.Errorf("create user: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO workspace_members (workspace_id, user_id, roles) VALUES ($1, $2, $3)`,
			in.WorkspaceID, userID, in.Roles); err != nil {
			return fmt.Errorf("create membership: %w", err)
		}
		if in.GroupID != "" {
			if err := setGroup(ctx, tx, in.WorkspaceID, userID, in.GroupID, in.TeamRoles); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO local_credentials (username, user_id, password_hash, must_change_password)
			VALUES ($1, $2, $3, true)`, in.Username, userID, in.PasswordHash)
		if isUniqueViolation(err) {
			return ErrConflict
		}
		if err != nil {
			return fmt.Errorf("create local credential: %w", err)
		}
		return nil
	})
	return userID, err
}

func setGroup(ctx context.Context, tx pgx.Tx, workspaceID, userID, groupID string, teamRoles []string) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM workspace_group_members WHERE workspace_id = $1 AND user_id = $2`,
		workspaceID, userID); err != nil {
		return fmt.Errorf("clear group: %w", err)
	}
	if groupID == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO workspace_group_members (group_id, workspace_id, user_id, team_roles)
		VALUES ($1, $2, $3, $4)`, groupID, workspaceID, userID, nonNil(teamRoles))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return ErrNotFound // group not in this Workspace
	}
	if err != nil {
		return fmt.Errorf("set group: %w", err)
	}
	return nil
}

// otherAdmins counts admins of the Workspace other than userID, locking the
// membership rows so two concurrent demotions cannot both pass the check.
func otherAdmins(ctx context.Context, tx pgx.Tx, workspaceID, userID string) (int, error) {
	rows, err := tx.Query(ctx, `
		SELECT user_id FROM workspace_members
		WHERE workspace_id = $1 AND user_id <> $2 AND 'admin' = ANY(roles)
		FOR UPDATE`, workspaceID, userID)
	if err != nil {
		return 0, fmt.Errorf("count admins: %w", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}

func hasRole(roles []string, role string) bool {
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

// UpdateMember changes roles and/or group of an existing member.
func (s *Store) UpdateMember(ctx context.Context, workspaceID, userID string, u MemberUpdate, ev audit.Event) error {
	return s.withAuditTx(ctx, &ev, func(tx pgx.Tx) error {
		var roles []string
		err := tx.QueryRow(ctx,
			`SELECT roles FROM workspace_members WHERE workspace_id = $1 AND user_id = $2 FOR UPDATE`,
			workspaceID, userID).Scan(&roles)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("load member: %w", err)
		}
		if u.Roles != nil {
			if hasRole(roles, "admin") && !hasRole(*u.Roles, "admin") {
				n, err := otherAdmins(ctx, tx, workspaceID, userID)
				if err != nil {
					return err
				}
				if n == 0 {
					return ErrLastAdmin
				}
			}
			if _, err := tx.Exec(ctx,
				`UPDATE workspace_members SET roles = $3 WHERE workspace_id = $1 AND user_id = $2`,
				workspaceID, userID, *u.Roles); err != nil {
				return fmt.Errorf("update roles: %w", err)
			}
		}
		if u.GroupID == nil && u.TeamRoles == nil {
			return nil
		}
		var groupID string
		var teamRoles []string
		err = tx.QueryRow(ctx,
			`SELECT group_id, team_roles FROM workspace_group_members WHERE workspace_id = $1 AND user_id = $2`,
			workspaceID, userID).Scan(&groupID, &teamRoles)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("load group: %w", err)
		}
		if u.GroupID != nil && *u.GroupID != groupID {
			groupID, teamRoles = *u.GroupID, nil
		}
		if u.TeamRoles != nil {
			teamRoles = *u.TeamRoles
		}
		if groupID == "" {
			if len(teamRoles) > 0 && u.TeamRoles != nil {
				return ErrNoGroup
			}
			teamRoles = nil
		}
		return setGroup(ctx, tx, workspaceID, userID, groupID, teamRoles)
	})
}

// RemoveMember deletes the Workspace membership (group rows cascade). When
// the user has no membership left, every active API key is revoked as well.
func (s *Store) RemoveMember(ctx context.Context, workspaceID, userID string, ev audit.Event) error {
	return s.withAuditTx(ctx, &ev, func(tx pgx.Tx) error {
		var roles []string
		err := tx.QueryRow(ctx,
			`SELECT roles FROM workspace_members WHERE workspace_id = $1 AND user_id = $2 FOR UPDATE`,
			workspaceID, userID).Scan(&roles)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("load member: %w", err)
		}
		if hasRole(roles, "admin") {
			n, err := otherAdmins(ctx, tx, workspaceID, userID)
			if err != nil {
				return err
			}
			if n == 0 {
				return ErrLastAdmin
			}
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM workspace_members WHERE workspace_id = $1 AND user_id = $2`,
			workspaceID, userID); err != nil {
			return fmt.Errorf("remove member: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE api_keys SET revoked_at = now()
			WHERE user_id = $1 AND revoked_at IS NULL
			  AND NOT EXISTS (SELECT 1 FROM workspace_members WHERE user_id = $1)`, userID); err != nil {
			return fmt.Errorf("revoke keys: %w", err)
		}
		return nil
	})
}

// LocalPasswordChangeRequired reports whether a local login must replace its
// one-time password before a session may be issued.
func (s *Store) LocalPasswordChangeRequired(ctx context.Context, username string) (bool, error) {
	var required bool
	err := s.pool.QueryRow(ctx,
		`SELECT must_change_password FROM local_credentials WHERE username = $1`, username).Scan(&required)
	if err != nil {
		return false, fmt.Errorf("local credential lookup: %w", err)
	}
	return required, nil
}

// RequireLocalPasswordChange makes the next login of username replace its
// password before a session is issued.
func (s *Store) RequireLocalPasswordChange(ctx context.Context, username string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE local_credentials SET must_change_password = true, updated_at = now() WHERE username = $1`, username)
	return err
}

// ChangeLocalPassword replaces the hash and clears the must-change flag.
func (s *Store) ChangeLocalPassword(ctx context.Context, username, passwordHash string, ev audit.Event) error {
	return s.withAuditTx(ctx, &ev, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE local_credentials
			SET password_hash = $2, must_change_password = false, updated_at = now()
			WHERE username = $1`, username, passwordHash)
		if err != nil {
			return fmt.Errorf("change password: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// Roles returns the Workspace's role catalog (built-ins merged with edits).
func (s *Store) Roles(ctx context.Context, workspaceID string) ([]rbac.Role, error) {
	return rbac.Catalog(ctx, s.pool, workspaceID)
}

// Grants resolves a member's permissions; roles come from the live
// membership check.
func (s *Store) Grants(ctx context.Context, workspaceID, userID string, roles []string) (rbac.Grants, error) {
	return rbac.Load(ctx, s.pool, workspaceID, userID, roles)
}

// PutRole creates a custom role or edits one (built-in edits are stored as
// overrides). The catalog is read inside the transaction, so validation sees
// the roles the write lands among.
func (s *Store) PutRole(ctx context.Context, workspaceID string, r rbac.Role, ev audit.Event) (rbac.Role, error) {
	r = rbac.Normalize(r)
	err := s.withAuditTx(ctx, &ev, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('workspace_roles:' || $1))`, workspaceID); err != nil {
			return fmt.Errorf("lock roles: %w", err)
		}
		catalog, err := rbac.Catalog(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		if err := rbac.Validate(catalog, r); err != nil {
			return err
		}
		perms := make([]string, 0, len(r.Permissions))
		for _, p := range r.Permissions {
			perms = append(perms, string(p))
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO workspace_roles (workspace_id, key, name, scope, permissions)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (workspace_id, key) DO UPDATE
			SET name = excluded.name, permissions = excluded.permissions, updated_at = now()`,
			workspaceID, r.Key, r.Name, r.Scope, perms)
		if err != nil {
			return fmt.Errorf("put role: %w", err)
		}
		return nil
	})
	if err != nil {
		return rbac.Role{}, err
	}
	catalog, err := s.Roles(ctx, workspaceID)
	if err != nil {
		return rbac.Role{}, err
	}
	out, _ := rbac.Find(catalog, r.Key)
	return out, nil
}

// DeleteRole resets a built-in role to its defaults, or deletes a custom role
// and removes it from every member and team assignment.
func (s *Store) DeleteRole(ctx context.Context, workspaceID, key string, ev audit.Event) error {
	return s.withAuditTx(ctx, &ev, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('workspace_roles:' || $1))`, workspaceID); err != nil {
			return fmt.Errorf("lock roles: %w", err)
		}
		builtin, ok := rbac.Find(rbac.Builtin(), key)
		if ok && builtin.Locked {
			return rbac.ErrLocked
		}
		tag, err := tx.Exec(ctx, `DELETE FROM workspace_roles WHERE workspace_id = $1 AND key = $2`, workspaceID, key)
		if err != nil {
			return fmt.Errorf("delete role: %w", err)
		}
		if ok {
			return nil
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `UPDATE workspace_members SET roles = array_remove(roles, $2)
			WHERE workspace_id = $1 AND $2 = ANY(roles)`, workspaceID, key); err != nil {
			return fmt.Errorf("unassign role: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE workspace_group_members SET team_roles = array_remove(team_roles, $2)
			WHERE workspace_id = $1 AND $2 = ANY(team_roles)`, workspaceID, key); err != nil {
			return fmt.Errorf("unassign team role: %w", err)
		}
		return nil
	})
}

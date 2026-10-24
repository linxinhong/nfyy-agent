// Package identity defines the canonical EAP Principal and the Workspace
// selection contract. These types are the single identity vocabulary for the
// whole platform: gateway, LLM, MCP and Knowledge services must all speak
// them. Tenant, Team, Customer or App types from sibling projects are
// explicitly not canonical and must never leak into these declarations.
//
// Security model: client headers only SELECT context. Authority is established
// exclusively server-side, either by a membership check at the gateway or by
// verifying an internal JWT that carries the workspace claim.
package identity

import (
	"context"
	"strings"

	"enterprise-agent-platform/packages/errs"
)

// Client-side context headers (development plan §7). All of them only SELECT
// or annotate context; none of them grants authority.
const (
	HeaderWorkspace     = "X-Agent-Workspace"
	HeaderClient        = "X-Agent-Client"
	HeaderClientVersion = "X-Agent-Client-Version"
	HeaderDevice        = "X-Agent-Device"
)

// Principal is the canonical actor identity for every request. It is a value
// type: services may embed it in audit events and traces.
type Principal struct {
	UserID         string   `json:"user_id"`
	OrganizationID string   `json:"organization_id,omitempty"`
	DepartmentID   string   `json:"department_id,omitempty"`
	Roles          []string `json:"roles,omitempty"`
	Scopes         []string `json:"scopes,omitempty"`
	ClientID       string   `json:"client_id,omitempty"`
	DeviceID       string   `json:"device_id,omitempty"`
}

// Validate requires an authenticated user; everything else is context.
func (p Principal) Validate() error {
	if strings.TrimSpace(p.UserID) == "" {
		return errs.New(errs.Unauthenticated, "principal has no user id")
	}
	return nil
}

// WorkspaceSource records how workspace authority was established. Anything
// outside the two server-validated sources carries no authority.
type WorkspaceSource string

const (
	// WorkspaceSourceMembership: the gateway validated membership at selection time.
	WorkspaceSourceMembership WorkspaceSource = "server_membership"
	// WorkspaceSourceInternalJWT: a downstream service verified a signed internal
	// JWT whose claims carry the workspace.
	WorkspaceSourceInternalJWT WorkspaceSource = "internal_jwt"
)

// WorkspaceContext is the server-validated workspace bound to a session.
type WorkspaceContext struct {
	WorkspaceID string          `json:"workspace_id"`
	Source      WorkspaceSource `json:"source"`
	Roles       []string        `json:"roles,omitempty"`
}

// Session is the authorized request context: a validated Principal plus the
// Workspace authority that was established for it.
type Session struct {
	Principal Principal        `json:"principal"`
	Workspace WorkspaceContext `json:"workspace"`
}

// Validate fails closed unless the session carries an explicit user identity
// and server-validated workspace authority.
func (s Session) Validate() error {
	if err := s.Principal.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(s.Workspace.WorkspaceID) == "" {
		return errs.WorkspaceFailClosed("session has no workspace id")
	}
	switch s.Workspace.Source {
	case WorkspaceSourceMembership, WorkspaceSourceInternalJWT:
		return nil
	default:
		return errs.WorkspaceFailClosed("workspace source is not server-validated: " + string(s.Workspace.Source))
	}
}

// Membership is the server-side answer to "is this user in this workspace".
type Membership struct {
	Member bool
	Roles  []string
}

// MembershipResolver answers membership questions from platform data.
// Implementations live in later slices (PostgreSQL-backed from TASK-003).
type MembershipResolver interface {
	IsMember(ctx context.Context, userID, workspaceID string) (Membership, error)
}

// SelectWorkspace resolves the client-selected workspace into an authorized
// Session. It fails closed when: the principal is invalid, no workspace was
// selected, the resolver itself fails, or the principal is not a member.
// Resolver errors are denials, never bypasses.
func SelectWorkspace(ctx context.Context, p Principal, headerWorkspace string, r MembershipResolver) (Session, error) {
	if err := p.Validate(); err != nil {
		return Session{}, err
	}
	ws := strings.TrimSpace(headerWorkspace)
	if ws == "" {
		return Session{}, errs.WorkspaceFailClosed("no workspace selected via " + HeaderWorkspace)
	}
	if r == nil {
		return Session{}, errs.New(errs.Internal, "workspace membership resolver is not configured")
	}
	m, err := r.IsMember(ctx, p.UserID, ws)
	if err != nil {
		return Session{}, errs.Wrap(err, errs.WorkspaceForbidden, "workspace membership could not be established (fail closed)")
	}
	if !m.Member {
		return Session{}, errs.New(errs.WorkspaceForbidden, "principal is not a member of workspace "+ws)
	}
	return Session{
		Principal: p,
		Workspace: WorkspaceContext{WorkspaceID: ws, Source: WorkspaceSourceMembership, Roles: m.Roles},
	}, nil
}

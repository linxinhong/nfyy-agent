package identity_test

import (
	"context"
	"errors"
	"testing"

	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
)

type resolverFunc func(ctx context.Context, userID, workspaceID string) (identity.Membership, error)

func (f resolverFunc) IsMember(ctx context.Context, userID, workspaceID string) (identity.Membership, error) {
	return f(ctx, userID, workspaceID)
}

var okPrincipal = identity.Principal{UserID: "user_123", OrganizationID: "org_default"}

// Security regression: a request without server-validated workspace authority
// must be denied in every failure mode.
func TestWorkspaceAuthorityFailClosed(t *testing.T) {
	t.Run("missing workspace selection", func(t *testing.T) {
		_, err := identity.SelectWorkspace(context.Background(), okPrincipal, "", resolverFunc(
			func(context.Context, string, string) (identity.Membership, error) {
				t.Fatal("resolver must not be called without a workspace selection")
				return identity.Membership{}, nil
			}))
		p, ok := errs.From(err)
		if !ok || p.Code != errs.WorkspaceForbidden {
			t.Fatalf("expected workspace_forbidden, got %v", err)
		}
	})

	t.Run("not a member", func(t *testing.T) {
		_, err := identity.SelectWorkspace(context.Background(), okPrincipal, "finance", resolverFunc(
			func(context.Context, string, string) (identity.Membership, error) {
				return identity.Membership{Member: false}, nil
			}))
		p, ok := errs.From(err)
		if !ok || p.Code != errs.WorkspaceForbidden {
			t.Fatalf("expected workspace_forbidden, got %v", err)
		}
	})

	// The resolver itself failing must deny, never bypass the check.
	t.Run("resolver failure", func(t *testing.T) {
		storeDown := errors.New("membership store unavailable")
		_, err := identity.SelectWorkspace(context.Background(), okPrincipal, "secops", resolverFunc(
			func(context.Context, string, string) (identity.Membership, error) {
				return identity.Membership{}, storeDown
			}))
		p, ok := errs.From(err)
		if !ok || p.Code != errs.WorkspaceForbidden {
			t.Fatalf("resolver failure must fail closed with workspace_forbidden, got %v", err)
		}
		if !errors.Is(err, storeDown) {
			t.Fatalf("denial must preserve the resolver cause for logs")
		}
	})

	t.Run("unauthenticated principal", func(t *testing.T) {
		_, err := identity.SelectWorkspace(context.Background(), identity.Principal{}, "secops", resolverFunc(
			func(context.Context, string, string) (identity.Membership, error) {
				return identity.Membership{Member: true}, nil
			}))
		p, ok := errs.From(err)
		if !ok || p.Code != errs.Unauthenticated {
			t.Fatalf("expected unauthenticated, got %v", err)
		}
	})
}

func TestSelectWorkspaceHappyPath(t *testing.T) {
	session, err := identity.SelectWorkspace(context.Background(), okPrincipal, "secops", resolverFunc(
		func(_ context.Context, userID, ws string) (identity.Membership, error) {
			if userID != "user_123" || ws != "secops" {
				t.Fatalf("resolver received %s/%s", userID, ws)
			}
			return identity.Membership{Member: true, Roles: []string{"developer"}}, nil
		}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := session.Validate(); err != nil {
		t.Fatalf("authorized session must validate: %v", err)
	}
	if session.Workspace.WorkspaceID != "secops" || session.Workspace.Source != identity.WorkspaceSourceMembership {
		t.Fatalf("unexpected workspace context: %+v", session.Workspace)
	}
}

// Sessions whose workspace authority is not server-validated are invalid.
func TestSessionValidateRejectsClientDeclaredAuthority(t *testing.T) {
	cases := []identity.Session{
		{Principal: okPrincipal}, // no workspace at all
		{Principal: okPrincipal, Workspace: identity.WorkspaceContext{WorkspaceID: "secops", Source: "client-header"}},
		{Principal: okPrincipal, Workspace: identity.WorkspaceContext{WorkspaceID: "secops", Source: ""}},
		{Workspace: identity.WorkspaceContext{WorkspaceID: "secops", Source: identity.WorkspaceSourceMembership}}, // no user
	}
	for i, s := range cases {
		if err := s.Validate(); err == nil {
			t.Errorf("case %d: session without server-validated workspace authority must not validate", i)
		}
	}
	valid := identity.Session{
		Principal: okPrincipal,
		Workspace: identity.WorkspaceContext{WorkspaceID: "secops", Source: identity.WorkspaceSourceInternalJWT},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("jwt-sourced session must validate: %v", err)
	}
}

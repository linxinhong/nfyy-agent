package token

import (
	"strings"

	"enterprise-agent-platform/packages/errs"
)

// AuthorizeCapability re-checks verified claims against a capability
// audience's contract: the audience must match exactly and the claims must
// carry non-empty workspace authority. Capability services call this on
// every request (defense in depth on top of transport-level verification).
// A failure is an explicit authorization denial — never a generic error.
func (c *Claims) AuthorizeCapability(expected Audience) error {
	if c == nil {
		return errs.New(errs.Unauthenticated, "no verified token claims")
	}
	if c.Audience != expected {
		return errs.New(errs.PermissionDenied, "token audience is not valid for this capability service")
	}
	if strings.TrimSpace(c.WorkspaceID) == "" {
		return errs.WorkspaceFailClosed("token carries no workspace authority")
	}
	return nil
}

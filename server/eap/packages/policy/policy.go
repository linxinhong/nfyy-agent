// Package policy provides the baseline EAP policy engine: deny by default,
// allow only what an explicit allowlist names. The M5 governance milestone
// replaces the rule source with the policy tables; the decision vocabulary
// and the fail-closed contract stay frozen (llmkernel consumes it).
package policy

import (
	"context"
	"slices"

	"enterprise-agent-platform/packages/llmkernel"
)

// Allowlist decides from explicit workspace and model allowlists. nil/empty
// model list allows all models; an empty workspace list denies everything
// (no implicit trust).
type Allowlist struct {
	Workspaces []string
	Models     []string
}

func (a *Allowlist) Decide(_ context.Context, input llmkernel.PolicyInput) (llmkernel.Decision, error) {
	if !slices.Contains(a.Workspaces, input.Session.SessionWorkspaceID()) {
		return llmkernel.Decision{Allowed: false, Reason: "workspace is not on the model policy allowlist"}, nil
	}
	if len(a.Models) > 0 && !slices.Contains(a.Models, input.Model) {
		return llmkernel.Decision{Allowed: false, Reason: "model is not on the allowlist"}, nil
	}
	return llmkernel.Decision{Allowed: true}, nil
}

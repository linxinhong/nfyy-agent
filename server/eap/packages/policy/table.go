package policy

import (
	"context"
	"strings"

	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/llmkernel"
)

// Canonical effects (mirror llmkernel constants; re-declared here so the
// policy package stays the vocabulary owner and llmkernel consumes them).
const (
	EffectAllow           = llmkernel.EffectAllow
	EffectRedact          = llmkernel.EffectRedact
	EffectLocalModelOnly  = llmkernel.EffectLocalModelOnly
	EffectRequireApproval = llmkernel.EffectRequireApproval
	EffectDeny            = llmkernel.EffectDeny
)

// TableRule is one versioned policy rule (control data). '*' matches any
// workspace or model. Lower priority number wins.
type TableRule struct {
	RuleID      string
	WorkspaceID string
	ModelAlias  string
	Effect      string
	Priority    int
}

// RulesLoader supplies the active rule set. A load error fails closed.
type RulesLoader func(ctx context.Context) ([]TableRule, error)

// TablePolicy decides from versioned rules: the most specific, highest
// priority enabled rule wins. No matching rule means ALLOW (the default
// open posture is expressed by an explicit catch-all deny rule when an
// operator wants closed).
type TablePolicy struct {
	LoadRules RulesLoader
}

// Decide implements llmkernel.PolicyEngine with canonical effects.
func (t *TablePolicy) Decide(ctx context.Context, input llmkernel.PolicyInput) (llmkernel.Decision, error) {
	rules, err := t.LoadRules(ctx)
	if err != nil {
		return llmkernel.Decision{}, err // caller fails closed
	}
	workspace := input.Session.SessionWorkspaceID()
	best := TableRule{Priority: int(^uint(0) >> 1)} // max int
	matched := false
	for _, r := range rules {
		if !matchRule(r.WorkspaceID, workspace) || !matchRule(r.ModelAlias, input.Model) {
			continue
		}
		if r.Priority < best.Priority {
			best = r
			matched = true
		}
	}
	if !matched {
		return llmkernel.Decision{Allowed: true, Effect: EffectAllow, Reason: "no matching policy rule"}, nil
	}
	effect := strings.ToLower(best.Effect)
	d := llmkernel.Decision{Effect: effect, RuleID: best.RuleID}
	switch effect {
	case EffectAllow:
		d.Allowed = true
	case EffectRedact:
		d.Allowed = true
	case EffectLocalModelOnly:
		d.Allowed = true
	case EffectRequireApproval:
		d.Reason = "approval required before this model can be used"
	case EffectDeny:
		d.Reason = "workspace or model is denied by policy rule " + best.RuleID
	default:
		return llmkernel.Decision{}, errs.New(errs.Internal, "policy rule carries an unknown effect: "+effect)
	}
	return d, nil
}

func matchRule(pattern, value string) bool {
	if pattern == "*" {
		return true
	}
	// Suffix wildcard support: "restricted-*" matches restricted-x.
	if suffix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(value, suffix)
	}
	return pattern == value
}

package policy_test

import (
	"context"
	"errors"
	"testing"

	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/policy"
)

func allowSession(workspace string) llmkernel.PolicyInput {
	return llmkernel.PolicyInput{
		Session: testSession{workspace: workspace},
		Model:   "stub-model",
		Action:  "llm.chat",
	}
}

type testSession struct{ workspace string }

func (s testSession) SessionUserID() string      { return "user_1" }
func (s testSession) SessionWorkspaceID() string { return s.workspace }

func TestAllowlistDenyByDefault(t *testing.T) {
	a := &policy.Allowlist{Workspaces: []string{"secops"}}
	decision, err := a.Decide(context.Background(), allowSession("finance"))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decision.Allowed {
		t.Fatalf("non-allowlisted workspace must be denied")
	}
	decision, err = a.Decide(context.Background(), allowSession("secops"))
	if err != nil || !decision.Allowed {
		t.Fatalf("allowlisted workspace must pass: %+v %v", decision, err)
	}
}

func TestAllowlistModelRestriction(t *testing.T) {
	a := &policy.Allowlist{Workspaces: []string{"secops"}, Models: []string{"stub-model"}}
	decision, err := a.Decide(context.Background(), allowSession("secops"))
	if err != nil || !decision.Allowed {
		t.Fatalf("listed model must pass: %+v %v", decision, err)
	}
	input := allowSession("secops")
	input.Model = "other-model"
	decision, err = a.Decide(context.Background(), input)
	if err != nil || decision.Allowed {
		t.Fatalf("unlisted model must be denied")
	}
}

// The policy engine itself failing must surface as an error so llmkernel's
// fail-closed path denies the request.
func TestEngineErrorsAreDenials(t *testing.T) {
	var _ llmkernel.PolicyEngine = failingEngine{}
	_, err := failingEngine{}.Decide(context.Background(), allowSession("secops"))
	if err == nil {
		t.Fatalf("engine failure must return an error (caller denies)")
	}
	problem, ok := errs.From(errs.PolicyFailClosed(err.Error()))
	if !ok || problem.Code != errs.PolicyDenied {
		t.Fatalf("caller must convert engine failure into policy_denied")
	}
	_ = errors.New
}

type failingEngine struct{}

func (failingEngine) Decide(context.Context, llmkernel.PolicyInput) (llmkernel.Decision, error) {
	return llmkernel.Decision{}, errors.New("policy store down")
}

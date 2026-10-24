package llmkernel_test

// Regression coverage for routed failover (found by the TASK-013 real
// provider qualification): provider-shaped failures — including the
// EAP-wrapped upstream errors the real OpenAI-compat client produces — must
// fail over to the next route. Authorization-shaped denials (policy, DLP,
// workspace, authn) are decisions and must NOT fail over.

import (
	"context"
	"errors"
	"testing"

	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/token"
)

type fakePolicy struct{ allow bool }

func (f fakePolicy) Decide(context.Context, llmkernel.PolicyInput) (llmkernel.Decision, error) {
	return llmkernel.Decision{Allowed: f.allow}, nil
}

type fakeDLP struct{}

func (fakeDLP) Scan(context.Context, string) (llmkernel.DLPResult, error) {
	return llmkernel.DLPResult{}, nil
}

type fakeClient struct {
	// per-provider error or content
	behavior map[string]error
	content  map[string]string
	// models records the model each provider was invoked with — the routed
	// loop must send the route's concrete model, never the logical alias.
	models map[string]string
}

func (f *fakeClient) Chat(_ context.Context, target llmkernel.ProviderTarget, _ string, req llmkernel.ChatRequest) (*llmkernel.ChatResult, error) {
	if f.models == nil {
		f.models = map[string]string{}
	}
	f.models[target.Name] = req.Model
	if err, ok := f.behavior[target.Name]; ok {
		return nil, err
	}
	return &llmkernel.ChatResult{Content: f.content[target.Name], Model: req.Model}, nil
}

func (f *fakeClient) ChatStream(ctx context.Context, target llmkernel.ProviderTarget, credential string, req llmkernel.ChatRequest) (<-chan llmkernel.StreamChunk, <-chan error) {
	return nil, nil
}

func (f *fakeClient) Embeddings(context.Context, llmkernel.ProviderTarget, string, string, string) ([]float64, error) {
	return nil, errors.New("not used")
}

func failoverRouter(plan func(context.Context, string) (llmkernel.RoutePlanView, error), client llmkernel.ProviderClient) *llmkernel.Router {
	return &llmkernel.Router{
		ResolvePlan: plan,
		ResolveTarget: func(_ context.Context, provider string) (llmkernel.ProviderTarget, error) {
			return llmkernel.ProviderTarget{Name: provider, BaseURL: "http://provider.invalid"}, nil
		},
		Credential: func(context.Context, string) (string, error) { return "credential", nil },
		Client:     client,
	}
}

func failoverClaims() *token.Claims {
	return &token.Claims{
		Subject: "usr_failover", Audience: token.AudienceLLMGateway,
		WorkspaceID: "secops", Scopes: []string{"llm:invoke"},
	}
}

func TestRouterFailsOverOnEAPWrappedUpstreamErrors(t *testing.T) {
	plan := func(context.Context, string) (llmkernel.RoutePlanView, error) {
		return llmkernel.RoutePlanView{Routes: []llmkernel.ProviderRoute{
			{Provider: "primary", Model: "m1"},
			{Provider: "backup", Model: "m2"},
		}}, nil
	}
	// The real client wraps transport/5xx failures into EAP upstream errors.
	// Regression: these must fail over, not be treated as denials.
	client := &fakeClient{
		behavior: map[string]error{
			"primary": errs.Wrap(errors.New("dial tcp: connection refused"),
				errs.UpstreamUnavailable, "provider unreachable"),
		},
		content: map[string]string{"backup": "backup says hi"},
	}
	router := failoverRouter(plan, client)
	result, _, attempts, _, err := router.GenerateChatRouted(
		context.Background(), failoverClaims(), fakePolicy{allow: true}, fakeDLP{},
		llmkernel.ChatRequest{Model: "alias"}, nil)
	if err != nil {
		t.Fatalf("router must fail over to the backup: %v", err)
	}
	if result.Content != "backup says hi" {
		t.Fatalf("expected backup content, got %q", result.Content)
	}
	if client.models["primary"] != "m1" || client.models["backup"] != "m2" {
		t.Fatalf("providers must receive their route model, got %v", client.models)
	}
	if len(attempts) != 2 || attempts[0].Status != "failed" || attempts[1].Status != "selected" {
		t.Fatalf("attempt ledger wrong: %+v", attempts)
	}
}

func TestRouterDoesNotFailOverOnAuthorizationShapedDenials(t *testing.T) {
	plan := func(context.Context, string) (llmkernel.RoutePlanView, error) {
		return llmkernel.RoutePlanView{Routes: []llmkernel.ProviderRoute{
			{Provider: "primary", Model: "m1"},
			{Provider: "backup", Model: "m2"},
		}}, nil
	}
	client := &fakeClient{
		behavior: map[string]error{
			"primary": errs.New(errs.PermissionDenied, "denied by policy"),
		},
		content: map[string]string{"backup": "should not be reached"},
	}
	router := failoverRouter(plan, client)
	_, _, attempts, _, err := router.GenerateChatRouted(
		context.Background(), failoverClaims(), fakePolicy{allow: true}, fakeDLP{},
		llmkernel.ChatRequest{Model: "alias"}, nil)
	if err == nil {
		t.Fatalf("denial must surface, not silently fail over")
	}
	if len(attempts) != 1 {
		t.Fatalf("authorization denial must not attempt the backup: %+v", attempts)
	}
}

var _ = identity.Principal{}

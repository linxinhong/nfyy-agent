package registry

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

// fakeModel implements models.Model.
type fakeModel struct{ id string }

func (m fakeModel) Response(ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
	return &models.ModelResponse{Content: "ok"}, nil
}
func (m fakeModel) ResponseStream(ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error) {
	ch := make(chan models.StreamEvent, 1)
	ch <- models.StreamEvent{Done: true}
	close(ch)
	return ch, nil
}
func (m fakeModel) ID() string       { return m.id }
func (m fakeModel) Provider() string { return "fake" }

func TestValidateEndpoint(t *testing.T) {
	if err := ValidateEndpoint("https://api.example.com/v1", false); err != nil {
		t.Fatalf("https rejected: %v", err)
	}
	if err := ValidateEndpoint("http://api.example.com/v1", false); err == nil {
		t.Fatalf("http outside loopback must be rejected")
	}
	if err := ValidateEndpoint("http://localhost:8080/v1", true); err != nil {
		t.Fatalf("loopback http must be allowed in tests: %v", err)
	}
	if err := ValidateEndpoint("https://user:pass@example.com/v1", false); err == nil {
		t.Fatalf("userinfo must be rejected")
	}
}

func TestStaticRegistryRejectsUnapproved(t *testing.T) {
	reg := NewStaticRegistry(nil)
	_, _, err := reg.Resolve(context.Background(), "unknown")
	if !errors.Is(err, ErrEndpointNotApproved) {
		t.Fatalf("err = %v, want not-approved", err)
	}
}

func TestStaticRegistryResolve(t *testing.T) {
	reg := NewStaticRegistry(nil)
	if err := reg.Add(EndpointMetadata{Provider: "openai", Model: "gpt", BaseURL: "https://api.example.com/v1"}, fakeModel{id: "gpt"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	meta, model, err := reg.Resolve(context.Background(), "gpt")
	if err != nil || model == nil || meta.Model != "gpt" {
		t.Fatalf("resolve = %+v, %v, %v", meta, model, err)
	}
}

func TestStaticRegistryRejectsInsecureAdd(t *testing.T) {
	reg := NewStaticRegistry(nil)
	if err := reg.Add(EndpointMetadata{Model: "bad", BaseURL: "http://api.example.com/v1"}, fakeModel{}); err == nil {
		t.Fatalf("insecure endpoint must be rejected at add time")
	}
}

func TestFallbackStaysInApprovedSet(t *testing.T) {
	reg := NewStaticRegistry(nil)
	_ = reg.Add(EndpointMetadata{Model: "a", BaseURL: "https://a.example.com/v1"}, fakeModel{})
	_ = reg.Add(EndpointMetadata{Model: "b", BaseURL: "https://b.example.com/v1"}, fakeModel{})

	if !FallbackAllowed(reg, EndpointMetadata{Model: "b", BaseURL: "https://b.example.com/v1"}) {
		t.Fatalf("approved fallback must be allowed")
	}
	if FallbackAllowed(reg, EndpointMetadata{Model: "b", BaseURL: "https://evil.example.com/v1"}) {
		t.Fatalf("fallback escaped the approved set")
	}
	if FallbackAllowed(reg, EndpointMetadata{Model: "z", BaseURL: "https://z.example.com/v1"}) {
		t.Fatalf("unknown model fallback must be rejected")
	}
}

func TestEndpointMetadataNoSensitiveFields(t *testing.T) {
	meta := EndpointMetadata{Provider: "p", Model: "m", Region: "r", Revision: "v1", BaseURL: "https://x"}
	if strings.Contains(meta.BaseURL, "@") {
		t.Fatalf("endpoint must not carry credentials")
	}
}

// TestAddLoopbackGatewayOptIn: AddLoopback accepts an explicit http loopback
// entry (same-host gateway) while non-loopback http stays rejected.
func TestAddLoopbackGatewayOptIn(t *testing.T) {
	r := NewStaticRegistry(nil)
	stub := fakeModel{"m"}
	if err := r.AddLoopback(EndpointMetadata{Provider: "openai", Model: "m1", BaseURL: "http://127.0.0.1:8080/v1"}, stub); err != nil {
		t.Fatalf("loopback add: %v", err)
	}
	if err := r.AddLoopback(EndpointMetadata{Provider: "openai", Model: "m2", BaseURL: "http://llmg.internal:8080/v1"}, stub); err == nil {
		t.Fatal("non-loopback http must stay rejected in AddLoopback")
	}
	if err := r.Add(EndpointMetadata{Provider: "openai", Model: "m3", BaseURL: "http://127.0.0.1:8080/v1"}, stub); err == nil {
		t.Fatal("plain Add must keep rejecting loopback http")
	}
}

package providers

import (
	"math"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

func TestRegistryResolve(t *testing.T) {
	r := NewRegistry()
	r.Register(Provider{
		ID:        "test",
		BaseURL:   "http://example.com/v1",
		APIKeyEnv: []string{"TEST_API_KEY"},
		Models: []Model{
			{ID: "small", API: APIOpenAICompletions, ContextWindow: 8_000},
		},
	})
	r.SetAPIKey("test", "key")

	m, err := r.Resolve("test:small")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.ID() != "small" || m.Provider() != "test" {
		t.Errorf("resolved model = %s/%s, want small/test", m.Provider(), m.ID())
	}
}

func TestRegistryResolveMissingKey(t *testing.T) {
	r := NewRegistry()
	r.Register(Provider{
		ID:        "needskey",
		APIKeyEnv: []string{"NEEDSKEY_API_KEY"},
		Models:    []Model{{ID: "m", API: APIOpenAICompletions}},
	})
	if _, err := r.Resolve("needskey:m"); err == nil {
		t.Fatal("expected error for missing key")
	}
}

func TestRegistrySynthesizedModel(t *testing.T) {
	r := NewRegistry()
	r.Register(Provider{
		ID:        "openrouter",
		BaseURL:   "https://openrouter.ai/api/v1",
		APIKeyEnv: []string{"OPENROUTER_API_KEY"},
	})
	r.SetAPIKey("openrouter", "key")
	m, err := r.Resolve("openrouter:openai/gpt-4o")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if m.ID() != "openai/gpt-4o" {
		t.Errorf("id = %q, want openai/gpt-4o", m.ID())
	}
}

func TestRegistryModels(t *testing.T) {
	r := NewRegistry(
		Provider{ID: "a", Models: []Model{{ID: "one"}}},
		Provider{ID: "b", Models: []Model{{ID: "two"}}},
	)
	all := r.Models()
	if len(all) != 2 {
		t.Fatalf("models = %d, want 2", len(all))
	}
	if all[0].Ref() != "a:one" {
		t.Errorf("first = %q, want a:one", all[0].Ref())
	}
}

func TestCalculateCost(t *testing.T) {
	m := Model{Cost: Cost{Input: 2.50, Output: 10.00}}
	cost := CalculateCost(m, &models.MessageMetrics{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if cost != 12.50 {
		t.Errorf("cost = %.4f, want 12.50", cost)
	}
}

func TestCalculateCostBillsCacheHitsAtCacheReadPrice(t *testing.T) {
	m := Model{Cost: Cost{Input: 0.28, Output: 0.42, CacheRead: 0.028}}
	cost := CalculateCost(m, &models.MessageMetrics{InputTokens: 1_000_000, CachedInputTokens: 800_000, OutputTokens: 0})
	if want := 0.2*0.28 + 0.8*0.028; math.Abs(cost-want) > 1e-9 {
		t.Errorf("cost = %.6f, want %.6f", cost, want)
	}
}

package governance_test

import (
	"math"
	"testing"

	"enterprise-agent-platform/packages/governance"
)

func TestCostMicrosExactMath(t *testing.T) {
	cases := []struct {
		name  string
		rec   governance.UsageRecord
		want  int64
		known bool
	}{
		{
			name: "backup sheet 3.0/12.0 at 100/50 tokens = 900 micros",
			rec: governance.UsageRecord{
				PromptTokens: 100, CompletionTokens: 50,
				Pricing: governance.Pricing{Currency: "USD", InputPerMT: 3.0, OutputPerMT: 12.0}, PricingKnown: true,
			},
			want: 900, known: true,
		},
		{
			name: "zero tokens zero cost",
			rec: governance.UsageRecord{
				Pricing: governance.Pricing{InputPerMT: 5.0, OutputPerMT: 20.0}, PricingKnown: true,
			},
			want: 0, known: true,
		},
		{
			name: "unknown pricing is never fabricated",
			rec:  governance.UsageRecord{PromptTokens: 1000},
			want: 0, known: false,
		},
		{
			name: "fractional prices",
			rec: governance.UsageRecord{
				PromptTokens: 1_000_000, CompletionTokens: 0,
				Pricing: governance.Pricing{InputPerMT: 0.000001}, PricingKnown: true,
			},
			want: 1, known: true, // 0.000001 * 1e6 tokens = 1 micro
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, known := tc.rec.CostMicros()
			if known != tc.known {
				t.Fatalf("known=%t, want %t", known, tc.known)
			}
			if got != tc.want {
				t.Fatalf("micros=%d, want %d", got, tc.want)
			}
		})
	}
}

// Routing determinism mirrors the SQL ordering: (priority, provider, model).
func TestSortRoutesDeterministic(t *testing.T) {
	routes := []governance.Route{
		{Provider: "b", Model: "m2", Priority: 100},
		{Provider: "a", Model: "m1", Priority: 200},
		{Provider: "a", Model: "m0", Priority: 100},
	}
	governance.SortRoutes(routes)
	want := []governance.Route{
		{Provider: "a", Model: "m0", Priority: 100},
		{Provider: "b", Model: "m2", Priority: 100},
		{Provider: "a", Model: "m1", Priority: 200},
	}
	for i, r := range routes {
		if r != want[i] {
			t.Fatalf("route %d = %+v, want %+v", i, r, want[i])
		}
	}
}

// Cost math must never go negative or NaN through float parsing.
func TestCostMicrosSanity(t *testing.T) {
	rec := governance.UsageRecord{
		PromptTokens: math.MaxInt32, CompletionTokens: math.MaxInt32,
		Pricing: governance.Pricing{InputPerMT: 2.5, OutputPerMT: 10.0}, PricingKnown: true,
	}
	got, known := rec.CostMicros()
	if !known || got <= 0 {
		t.Fatalf("large usage must price positive, got %d known=%t", got, known)
	}
}

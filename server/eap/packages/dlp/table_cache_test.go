package dlp

import (
	"context"
	"sync/atomic"
	"testing"
)

// Regression (TASK-045): the compile cache is keyed per workspace. The old
// constant "all" key served the first workspace's rule set to every other
// workspace until TTL expiry — cross-workspace rule leakage that
// TestWorkspaceScopedRules caught on the integration surface.
func TestScannerCacheIsolatesWorkspaces(t *testing.T) {
	var loads int64
	s := NewTableScanner(func(ctx context.Context) ([]Rule, error) {
		atomic.AddInt64(&loads, 1)
		switch WorkspaceFrom(ctx) {
		case "finance":
			return []Rule{{Name: "fin_salary", Pattern: "TESTRULE", Action: "deny"}}, nil
		default:
			return []Rule{}, nil
		}
	})
	// Default 60s cache: enabled.
	fin := ContextWithWorkspace(context.Background(), "finance")
	sec := ContextWithWorkspace(context.Background(), "secops")

	res, err := s.Scan(fin, "薪资机密词TESTRULE")
	if err != nil || !res.Blocked {
		t.Fatalf("finance must hit its rule: %v %+v", err, res)
	}
	res, err = s.Scan(sec, "薪资机密词TESTRULE")
	if err != nil {
		t.Fatalf("secops scan: %v", err)
	}
	for _, name := range res.RuleNames {
		if name == "fin_salary" {
			t.Fatalf("finance rule leaked into secops via the cache: %+v", res.RuleNames)
		}
	}
	// Finance keeps its own cached set: still enforced on the second scan.
	res, err = s.Scan(fin, "薪资机密词TESTRULE")
	if err != nil || !res.Blocked {
		t.Fatalf("finance second scan must still hit: %v %+v", err, res)
	}
	// Per-workspace keys: two loads total (finance, secops), the third scan
	// served from finance's cache.
	if n := atomic.LoadInt64(&loads); n != 2 {
		t.Fatalf("loads = %d, want 2 (one per workspace)", n)
	}
}

// CacheTTL<=0 means the cache is DISABLED (controlcache contract: <=0 is
// always reload) — not a silently-60s cache. Every scan reloads the rules.
func TestScannerDisabledCacheReloadsEveryScan(t *testing.T) {
	var loads int64
	s := NewTableScanner(func(ctx context.Context) ([]Rule, error) {
		atomic.AddInt64(&loads, 1)
		return []Rule{}, nil
	})
	s.CacheTTL = 0 // explicit disable
	for i := 0; i < 3; i++ {
		if _, err := s.Scan(ContextWithWorkspace(context.Background(), "finance"), "x"); err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}
	}
	if n := atomic.LoadInt64(&loads); n != 3 {
		t.Fatalf("loads = %d, want 3 (reload every scan)", n)
	}
}

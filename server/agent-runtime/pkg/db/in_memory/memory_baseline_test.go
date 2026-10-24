package in_memory_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

// BenchmarkLegacyMemoryReadAlgorithm freezes the pre-MEM-001 workload used
// to document baseline 3152f00. It deliberately exercises the legacy API;
// the strict adapter contract is benchmarked separately by the final gate.
func BenchmarkLegacyMemoryReadAlgorithm(b *testing.B) {
	store := in_memory.New()
	ctx := context.Background()
	base := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 100; i++ {
		if err := store.CreateMemory(ctx, &memory.UserMemory{MemoryID: fmt.Sprintf("target-%04d", i), Memory: "target", Namespace: "target", UserID: "user", AgentID: "agent", CreatedAt: base.Add(time.Duration(i))}); err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < 1000; i++ {
		if err := store.CreateMemory(ctx, &memory.UserMemory{MemoryID: fmt.Sprintf("noise-%04d", i), Memory: "noise", Namespace: "other", UserID: "other", AgentID: "agent", CreatedAt: base.Add(time.Duration(i))}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := store.GetMemories(ctx, "user", "agent", 10)
		if err != nil || len(got) != 10 {
			b.Fatalf("GetMemories = %d, %v", len(got), err)
		}
	}
}

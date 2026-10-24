package agent_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

// RunDurabilityBenchmark measures one persisted run with a tool round
// (create, model, tool, model, finalize) with the durability features on
// (write-ahead and round checkpoints, run lease) and off, on fresh
// sessions in parallel. Store adapters call it with their own store.
func RunDurabilityBenchmark(b *testing.B, newStore func() db.AgentStore) {
	for _, durable := range []bool{false, true} {
		b.Run(fmt.Sprintf("durable=%v", durable), func(b *testing.B) {
			store := newStore()
			cfg := agent.AgentConfig{
				ID: "durability-bench", Model: &baselineModel{toolCall: true}, DB: store, UserID: "bench",
				Tools: []tools.Tool{&baselineTool{}},
			}
			if !durable {
				cfg.DisableRoundCheckpoints = true
				cfg.RunLeaseTimeout = -1
			}
			value, err := agent.New(cfg)
			if err != nil {
				b.Fatal(err)
			}
			var seq atomic.Int64
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					req := session.RunRequest{Input: session.RunInput{Message: fmt.Sprintf("q-%d", seq.Add(1))}}
					if _, err := value.RunWith(context.Background(), req); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}

func BenchmarkAgentDurabilityInMemory(b *testing.B) {
	RunDurabilityBenchmark(b, func() db.AgentStore { return in_memory.New() })
}

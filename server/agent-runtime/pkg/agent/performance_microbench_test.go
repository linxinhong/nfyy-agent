//go:build go1.24

package agent_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// BenchmarkPerfAgentRun freezes direct synchronous and fixed-worker execution
// separately from the original per-operation goroutine-batch benchmark.
func BenchmarkPerfAgentRun(b *testing.B) {
	request := session.RunRequest{
		RunID:     "perf-fixed-run",
		SessionID: "perf-fixed-session",
		Input:     session.RunInput{Message: "fixed-input"},
	}

	b.Run("synchronous", func(b *testing.B) {
		value := newBaselineAgent(b, &baselineModel{}, nil)
		b.ReportAllocs()
		for b.Loop() {
			if _, err := value.RunWith(context.Background(), request); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("fixed_workers_32", func(b *testing.B) {
		const workers = 32
		value := newBaselineAgent(b, &baselineModel{}, nil)
		jobs := make(chan struct{}, workers)
		var group sync.WaitGroup
		var failed atomic.Int64
		group.Add(workers)
		for worker := 0; worker < workers; worker++ {
			go func() {
				defer group.Done()
				for range jobs {
					if _, err := value.RunWith(context.Background(), request); err != nil {
						failed.Add(1)
					}
				}
			}()
		}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			jobs <- struct{}{}
		}
		close(jobs)
		group.Wait()
		b.StopTimer()
		if got := failed.Load(); got != 0 {
			b.Fatalf("failed runs = %d", got)
		}
	})
}

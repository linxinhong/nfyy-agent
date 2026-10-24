//go:build go1.24

package run

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
)

func BenchmarkPerfAdmission(b *testing.B) {
	b.Run("uncontended_legacy", func(b *testing.B) {
		controller, err := NewAdmissionController(AdmissionConfig{Total: 4, Queue: perfAdmissionQueue, PerTenant: 2})
		if err != nil {
			b.Fatal(err)
		}
		ctx := context.Background()
		b.ReportAllocs()
		for b.Loop() {
			_, permit, acquireErr := controller.Acquire(ctx)
			if acquireErr != nil {
				b.Fatal(acquireErr)
			}
			permit.Release()
		}
	})

	b.Run("uncontended_tenant", func(b *testing.B) {
		controller, err := NewAdmissionController(AdmissionConfig{Total: 4, Queue: perfAdmissionQueue, PerTenant: 2})
		if err != nil {
			b.Fatal(err)
		}
		ctx := execution.ContextWithNamespace(context.Background(), "perf-tenant")
		b.ReportAllocs()
		for b.Loop() {
			_, permit, acquireErr := controller.Acquire(ctx)
			if acquireErr != nil {
				b.Fatal(acquireErr)
			}
			permit.Release()
		}
	})

	b.Run("fixed_workers_8_tenants_4", func(b *testing.B) {
		const workers = 8
		controller, err := NewAdmissionController(AdmissionConfig{
			Total: 4, Queue: perfAdmissionQueue, PerTenant: 2, WaitTimeout: time.Second,
		})
		if err != nil {
			b.Fatal(err)
		}
		contexts := []context.Context{
			execution.ContextWithNamespace(context.Background(), "perf-a"),
			execution.ContextWithNamespace(context.Background(), "perf-b"),
			execution.ContextWithNamespace(context.Background(), "perf-c"),
			execution.ContextWithNamespace(context.Background(), "perf-d"),
		}
		jobs := make(chan int, workers)
		var group sync.WaitGroup
		var failed atomic.Int64
		group.Add(workers)
		for worker := 0; worker < workers; worker++ {
			go func() {
				defer group.Done()
				for tenant := range jobs {
					_, permit, acquireErr := controller.Acquire(contexts[tenant])
					if acquireErr != nil {
						failed.Add(1)
						continue
					}
					permit.Release()
				}
			}()
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := range b.N {
			jobs <- i % len(contexts)
		}
		close(jobs)
		group.Wait()
		b.StopTimer()
		if got := failed.Load(); got != 0 {
			b.Fatalf("failed acquisitions = %d", got)
		}
	})

	b.Run("head_tenant_full_eligible_tail_q256", func(b *testing.B) {
		fixture := newPerfEligibleTailFixture(perfAdmissionQueue)
		b.ReportAllocs()
		for b.Loop() {
			fixture.Reset()
			if permit := fixture.Dispatch(); permit == nil {
				b.Fatal("eligible tail waiter did not receive permit")
			}
		}
		b.ReportMetric(perfAdmissionQueue, "waiters/op")
	})

	b.Run("cancel_storm_reverse_scan_q256", func(b *testing.B) {
		fixture := newPerfCancelStormFixture(perfAdmissionQueue)
		b.ReportAllocs()
		for b.Loop() {
			fixture.Reset()
			for i := perfAdmissionQueue - 1; i >= 0; i-- {
				fixture.Cancel(i)
			}
		}
		b.ReportMetric(perfAdmissionQueue, "cancelled-waiters/op")
	})

	b.Run("cancel_storm_fifo_move_q256", func(b *testing.B) {
		fixture := newPerfCancelStormFixture(perfAdmissionQueue)
		b.ReportAllocs()
		for b.Loop() {
			fixture.Reset()
			for i := 0; i < perfAdmissionQueue; i++ {
				fixture.Cancel(i)
			}
		}
		b.ReportMetric(perfAdmissionQueue, "cancelled-waiters/op")
	})
}

package run

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
)

// TestAdmissionFixedDurationOverloadGate is a bounded, fixed-duration load
// harness for the final reliability gate. Its verbose output is intentionally
// machine-readable raw evidence: elapsed_ms,rss_bytes,goroutines,active,
// queued,tenant_a_active,tenant_b_active.
func TestAdmissionFixedDurationOverloadGate(t *testing.T) {
	const (
		totalLimit   = 4
		queueLimit   = 8
		tenantLimit  = 2
		loadWorkers  = 64
		loadDuration = 500 * time.Millisecond
	)
	controller, err := NewAdmissionController(AdmissionConfig{
		Total: totalLimit, Queue: queueLimit, PerTenant: tenantLimit, WaitTimeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewAdmissionController: %v", err)
	}

	loadCtx, stopLoad := context.WithCancel(context.Background())
	defer stopLoad()
	start := make(chan struct{})
	var workers sync.WaitGroup
	var activeWorkers, tenantAActive, tenantBActive atomic.Int32
	var maxActive, maxQueued, maxTenantA, maxTenantB atomic.Int32
	updateMax := func(target *atomic.Int32, value int32) {
		for old := target.Load(); value > old && !target.CompareAndSwap(old, value); old = target.Load() {
		}
	}

	for i := 0; i < loadWorkers; i++ {
		tenantIndex := i % 2
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			namespace := "gate-tenant-a"
			tenantActive := &tenantAActive
			tenantMax := &maxTenantA
			if tenantIndex == 1 {
				namespace = "gate-tenant-b"
				tenantActive = &tenantBActive
				tenantMax = &maxTenantB
			}
			ctx := execution.ContextWithNamespace(loadCtx, namespace)
			for ctx.Err() == nil {
				_, permit, acquireErr := controller.Acquire(ctx)
				if acquireErr != nil {
					if errors.Is(acquireErr, ErrAdmissionQueueFull) || errors.Is(acquireErr, ErrAdmissionTimeout) || errors.Is(acquireErr, context.Canceled) {
						continue
					}
					t.Errorf("Acquire(%s): %v", namespace, acquireErr)
					return
				}
				active := activeWorkers.Add(1)
				tenant := tenantActive.Add(1)
				updateMax(&maxActive, active)
				updateMax(tenantMax, tenant)
				hold := time.NewTimer(2 * time.Millisecond)
				select {
				case <-hold.C:
				case <-ctx.Done():
					if !hold.Stop() {
						<-hold.C
					}
				}
				tenantActive.Add(-1)
				activeWorkers.Add(-1)
				permit.Release()
			}
		}()
	}

	started := time.Now()
	close(start)
	ticker := time.NewTicker(10 * time.Millisecond)
	deadline := time.NewTimer(loadDuration)
	defer ticker.Stop()
	defer deadline.Stop()
	for {
		select {
		case <-ticker.C:
			stats := controller.Stats()
			updateMax(&maxQueued, int32(stats.Queued))
			rss, rssErr := currentRSSBytes()
			if rssErr != nil {
				t.Fatalf("sample RSS: %v", rssErr)
			}
			t.Logf("resource_sample,%d,%d,%d,%d,%d,%d,%d",
				time.Since(started).Milliseconds(), rss, runtime.NumGoroutine(), stats.Active, stats.Queued,
				tenantAActive.Load(), tenantBActive.Load())
			if stats.Active > totalLimit || stats.Queued > queueLimit {
				t.Fatalf("controller escaped bounds: %+v", stats)
			}
			if tenantAActive.Load() > tenantLimit || tenantBActive.Load() > tenantLimit {
				t.Fatalf("tenant escaped bound: a=%d b=%d", tenantAActive.Load(), tenantBActive.Load())
			}
		case <-deadline.C:
			stopLoad()
			goto stopped
		}
	}

stopped:
	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("overload workers did not stop")
	}
	stats := controller.Stats()
	rss, rssErr := currentRSSBytes()
	if rssErr != nil {
		t.Fatalf("sample final RSS: %v", rssErr)
	}
	t.Logf("resource_final,%d,%d,%d,%d,%d,%d,%d",
		time.Since(started).Milliseconds(), rss, runtime.NumGoroutine(), stats.Active, stats.Queued,
		tenantAActive.Load(), tenantBActive.Load())
	if stats.Active != 0 || stats.Queued != 0 || activeWorkers.Load() != 0 || tenantAActive.Load() != 0 || tenantBActive.Load() != 0 {
		t.Fatalf("resources not reclaimed: stats=%+v workers=%d tenantA=%d tenantB=%d",
			stats, activeWorkers.Load(), tenantAActive.Load(), tenantBActive.Load())
	}
	if maxActive.Load() > totalLimit || maxQueued.Load() > queueLimit || maxTenantA.Load() > tenantLimit || maxTenantB.Load() > tenantLimit {
		t.Fatalf("observed maxima escaped limits: active=%d queued=%d tenantA=%d tenantB=%d",
			maxActive.Load(), maxQueued.Load(), maxTenantA.Load(), maxTenantB.Load())
	}
	if stats.Rejected == 0 {
		t.Fatal("fixed-duration load did not reach overload")
	}
	t.Logf("resource_max,active=%d,queued=%d,tenant_a=%d,tenant_b=%d,rejected=%d",
		maxActive.Load(), maxQueued.Load(), maxTenantA.Load(), maxTenantB.Load(), stats.Rejected)
}

func currentRSSBytes() (uint64, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0, err
	}
	kib, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, err
	}
	return kib * 1024, nil
}

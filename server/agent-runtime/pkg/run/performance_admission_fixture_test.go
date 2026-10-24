package run

import (
	"slices"
	"testing"
)

const perfAdmissionQueue = 256

// These test-only fixtures isolate the frozen BenchmarkPerf workload from the
// controller's current slice representation. PERF-004 may update this seam if
// the representation changes, but must preserve the exact active counts,
// tenant order, queue depth, cancellation order, and one-operation metrics in
// performance_benchmark_test.go.

type perfEligibleTailFixture struct {
	controller *AdmissionController
	order      []*admissionWaiter
	eligible   *admissionWaiter
}

func newPerfEligibleTailFixture(depth int) *perfEligibleTailFixture {
	controller := &AdmissionController{
		cfg:     AdmissionConfig{Total: 3, Queue: depth, PerTenant: 1},
		active:  2,
		tenants: map[string]int{"perf-a": 1, "perf-c": 1},
	}
	order := make([]*admissionWaiter, depth)
	for i := 0; i < depth-1; i++ {
		order[i] = &admissionWaiter{tenant: "perf-a", grant: make(chan *AdmissionPermit, 1), queued: true, index: i}
	}
	eligible := &admissionWaiter{tenant: "perf-b", grant: make(chan *AdmissionPermit, 1), queued: true, index: depth - 1}
	order[depth-1] = eligible
	return &perfEligibleTailFixture{controller: controller, order: order, eligible: eligible}
}

func (f *perfEligibleTailFixture) Reset() {
	f.controller.active = 2
	delete(f.controller.tenants, "perf-b")
	// Dispatch removes only the eligible tail. The preceding capped waiters
	// remain continuously queued, so restoring the frozen fixture state only
	// needs to restore that tail's queue metadata rather than rebuilding a
	// different workload representation each iteration.
	f.controller.waiters = append(f.controller.waiters[:0], f.order...)
	f.controller.queued = len(f.order)
	f.eligible.queued = true
	f.eligible.index = len(f.order) - 1
}

func (f *perfEligibleTailFixture) Dispatch() *AdmissionPermit {
	f.controller.mu.Lock()
	f.controller.dispatchLocked()
	f.controller.mu.Unlock()
	return <-f.eligible.grant
}

func (f *perfEligibleTailFixture) QueuedTenants() []string {
	tenants := make([]string, 0, f.controller.queued)
	for _, waiter := range f.controller.waiters {
		if waiter == nil {
			continue
		}
		tenants = append(tenants, waiter.tenant)
	}
	return tenants
}

func (f *perfEligibleTailFixture) BlockedGrantCount() int {
	count := 0
	for _, waiter := range f.order[:len(f.order)-1] {
		count += len(waiter.grant)
	}
	return count
}

type perfCancelStormFixture struct {
	controller *AdmissionController
	order      []*admissionWaiter
}

func newPerfCancelStormFixture(depth int) *perfCancelStormFixture {
	controller := &AdmissionController{
		cfg:     AdmissionConfig{Total: 1, Queue: depth, PerTenant: 1},
		active:  1,
		tenants: map[string]int{"perf-active": 1},
	}
	order := make([]*admissionWaiter, depth)
	for i := range order {
		order[i] = &admissionWaiter{tenant: "perf-a", queued: true}
	}
	return &perfCancelStormFixture{controller: controller, order: order}
}

func (f *perfCancelStormFixture) Reset() {
	f.controller.waiters = f.controller.waiters[:0]
	f.controller.queued = 0
	for _, waiter := range f.order {
		f.controller.appendWaiterLocked(waiter)
	}
}

func (f *perfCancelStormFixture) Cancel(index int) {
	f.controller.mu.Lock()
	f.controller.removeWaiterLocked(f.order[index])
	f.controller.dispatchLocked()
	f.controller.mu.Unlock()
}

func (f *perfCancelStormFixture) QueueDepth() int {
	return f.controller.queued
}

func TestPerfAdmissionFixtures(t *testing.T) {
	t.Run("eligible_tail", func(t *testing.T) {
		fixture := newPerfEligibleTailFixture(perfAdmissionQueue)
		fixture.Reset()
		before := fixture.QueuedTenants()
		if len(before) != perfAdmissionQueue || before[len(before)-1] != "perf-b" {
			t.Fatalf("reset queue = len %d tail %q", len(before), before[len(before)-1])
		}
		for i, tenant := range before[:len(before)-1] {
			if tenant != "perf-a" {
				t.Fatalf("reset queue[%d] = %q, want perf-a", i, tenant)
			}
		}
		permit := fixture.Dispatch()
		if permit == nil || permit.tenant != "perf-b" {
			t.Fatalf("dispatched permit = %#v, want perf-b", permit)
		}
		if got := fixture.BlockedGrantCount(); got != 0 {
			t.Fatalf("blocked tenant grants = %d, want 0", got)
		}
		wantRemaining := slices.Repeat([]string{"perf-a"}, perfAdmissionQueue-1)
		if got := fixture.QueuedTenants(); !slices.Equal(got, wantRemaining) {
			t.Fatalf("remaining queue changed order: %v", got)
		}
	})

	for _, tc := range []struct {
		name    string
		indices func(int) []int
	}{
		{name: "reverse", indices: func(depth int) []int {
			indices := make([]int, depth)
			for i := range indices {
				indices[i] = depth - 1 - i
			}
			return indices
		}},
		{name: "fifo", indices: func(depth int) []int {
			indices := make([]int, depth)
			for i := range indices {
				indices[i] = i
			}
			return indices
		}},
	} {
		t.Run("cancel_"+tc.name, func(t *testing.T) {
			fixture := newPerfCancelStormFixture(perfAdmissionQueue)
			fixture.Reset()
			if got := fixture.QueueDepth(); got != perfAdmissionQueue {
				t.Fatalf("reset depth = %d, want %d", got, perfAdmissionQueue)
			}
			for _, index := range tc.indices(perfAdmissionQueue) {
				fixture.Cancel(index)
			}
			if got := fixture.QueueDepth(); got != 0 {
				t.Fatalf("final depth = %d, want 0", got)
			}
		})
	}
}

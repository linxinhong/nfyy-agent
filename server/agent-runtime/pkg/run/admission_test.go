package run

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
)

func admissionContext(namespace string) context.Context {
	if namespace == "" {
		return context.Background()
	}
	return execution.ContextWithNamespace(context.Background(), namespace)
}

func TestAdmissionControllerBoundsTotalQueueAndTenant(t *testing.T) {
	controller, err := NewAdmissionController(AdmissionConfig{Total: 4, Queue: 8, PerTenant: 2, WaitTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewAdmissionController: %v", err)
	}
	var permits []*AdmissionPermit
	for _, namespace := range []string{"tenant-a", "tenant-a", "tenant-b", "tenant-b"} {
		_, permit, acquireErr := controller.Acquire(admissionContext(namespace))
		if acquireErr != nil {
			t.Fatalf("acquire %s: %v", namespace, acquireErr)
		}
		permits = append(permits, permit)
	}

	waiterCtx, cancelWaiter := context.WithCancel(admissionContext("tenant-a"))
	defer cancelWaiter()
	waiterResult := make(chan error, 1)
	go func() {
		_, permit, acquireErr := controller.Acquire(waiterCtx)
		if permit != nil {
			permit.Release()
		}
		waiterResult <- acquireErr
	}()
	waitForAdmissionStats(t, controller, 4, 1)

	var queued sync.WaitGroup
	queued.Add(7)
	for i := 0; i < 7; i++ {
		go func() {
			defer queued.Done()
			ctx, cancel := context.WithCancel(admissionContext("tenant-b"))
			defer cancel()
			_, permit, acquireErr := controller.Acquire(ctx)
			if permit != nil {
				permit.Release()
			}
			if acquireErr != nil && !errors.Is(acquireErr, context.Canceled) {
				t.Errorf("queued acquire: %v", acquireErr)
			}
		}()
	}
	waitForAdmissionStats(t, controller, 4, 8)
	if _, _, err := controller.Acquire(admissionContext("tenant-c")); !errors.Is(err, ErrAdmissionQueueFull) {
		t.Fatalf("full queue acquire = %v, want ErrAdmissionQueueFull", err)
	}

	cancelWaiter()
	if err := <-waiterResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter = %v, want context.Canceled", err)
	}
	waitForAdmissionStats(t, controller, 4, 7)
	for _, permit := range permits {
		permit.Release()
	}
	queued.Wait()
	waitForAdmissionStats(t, controller, 0, 0)
	if stats := controller.Stats(); stats.Rejected != 1 {
		t.Fatalf("rejected = %d, want 1", stats.Rejected)
	}
}

func TestAdmissionControllerSkipsCappedTenantAndReleasedPermitIsNotReusable(t *testing.T) {
	controller, err := NewAdmissionController(AdmissionConfig{Total: 3, Queue: 3, PerTenant: 2, WaitTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewAdmissionController: %v", err)
	}
	ctxA := admissionContext("tenant-a")
	_, first, err := controller.Acquire(ctxA)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	_, second, err := controller.Acquire(ctxA)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	blockedA := make(chan *AdmissionPermit, 1)
	go func() {
		_, permit, _ := controller.Acquire(ctxA)
		blockedA <- permit
	}()
	waitForAdmissionStats(t, controller, 2, 1)

	// tenant-b is allowed despite tenant-a being first in the queue and capped.
	_, tenantB, err := controller.Acquire(admissionContext("tenant-b"))
	if err != nil {
		t.Fatalf("tenant-b acquire: %v", err)
	}
	if stats := controller.Stats(); stats.Active != 3 || stats.Queued != 1 {
		t.Fatalf("stats after tenant-b = %+v, want active 3 queued 1", stats)
	}
	tenantB.Release()
	first.Release()
	var third *AdmissionPermit
	select {
	case third = <-blockedA:
	case <-time.After(time.Second):
		t.Fatal("capped tenant waiter was not admitted after release")
	}
	if third == nil {
		t.Fatal("capped tenant acquire failed")
	}

	// A released permit embedded in an old context cannot be reused as a live
	// slot: Acquire creates a fresh permit and stale Release cannot return it.
	ctxWithThird := context.WithValue(ctxA, admissionContextKey{}, third)
	third.Release()
	_, fresh, err := controller.Acquire(ctxWithThird)
	if err != nil {
		t.Fatalf("fresh acquire after release: %v", err)
	}
	third.Release()
	if stats := controller.Stats(); stats.Active != 2 {
		t.Fatalf("stale release changed active stats: %+v", stats)
	}
	fresh.Release()
	second.Release()
	waitForAdmissionStats(t, controller, 0, 0)
}

func TestAdmissionControllerTimeoutRemovesWaiter(t *testing.T) {
	controller, err := NewAdmissionController(AdmissionConfig{Total: 1, Queue: 1, PerTenant: 1, WaitTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewAdmissionController: %v", err)
	}
	_, held, err := controller.Acquire(admissionContext("tenant-a"))
	if err != nil {
		t.Fatalf("held acquire: %v", err)
	}
	if _, _, err := controller.Acquire(admissionContext("tenant-b")); !errors.Is(err, ErrAdmissionTimeout) {
		t.Fatalf("timed acquire = %v, want ErrAdmissionTimeout", err)
	}
	waitForAdmissionStats(t, controller, 1, 0)
	held.Release()
}

func TestAdmissionControllerRejectsCancelledInvalidAndMismatchedPermitContexts(t *testing.T) {
	controller, err := NewAdmissionController(AdmissionConfig{Total: 1, Queue: 1, PerTenant: 1})
	if err != nil {
		t.Fatalf("NewAdmissionController: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := controller.Acquire(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire = %v, want context.Canceled", err)
	}
	if _, _, err := controller.Acquire(execution.ContextWithNamespace(context.Background(), execution.ReservedNamespace)); !errors.Is(err, execution.ErrNamespaceReserved) {
		t.Fatalf("invalid namespace acquire = %v, want reserved namespace error", err)
	}
	ctxA := admissionContext("tenant-a")
	ctxA, permit, err := controller.Acquire(ctxA)
	if err != nil {
		t.Fatalf("tenant-a acquire: %v", err)
	}
	defer permit.Release()
	ctxB := execution.ContextWithNamespace(ctxA, "tenant-b")
	if _, borrowed, err := controller.Acquire(ctxB); !errors.Is(err, ErrAdmissionPermitTenantMismatch) || borrowed != nil {
		t.Fatalf("mismatched borrowed context = permit:%v err:%v", borrowed, err)
	}
	if stats := controller.Stats(); stats.Active != 1 || stats.Queued != 0 {
		t.Fatalf("mismatched context changed capacity: %+v", stats)
	}
}

func TestAdmissionQueueCancellationKeepsTenantFIFO(t *testing.T) {
	controller, err := NewAdmissionController(AdmissionConfig{Total: 1, Queue: 3, PerTenant: 1, WaitTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewAdmissionController: %v", err)
	}
	first := &admissionWaiter{tenant: "tenant-a", grant: make(chan *AdmissionPermit, 1)}
	cancelled := &admissionWaiter{tenant: "tenant-b", grant: make(chan *AdmissionPermit, 1)}
	second := &admissionWaiter{tenant: "tenant-a", grant: make(chan *AdmissionPermit, 1)}
	controller.mu.Lock()
	controller.appendWaiterLocked(first)
	controller.appendWaiterLocked(cancelled)
	controller.appendWaiterLocked(second)
	controller.removeWaiterLocked(cancelled)
	controller.dispatchLocked()
	controller.mu.Unlock()

	select {
	case permit := <-first.grant:
		permit.Release()
	case <-time.After(time.Second):
		t.Fatal("first tenant waiter was not granted")
	}
	select {
	case permit := <-second.grant:
		permit.Release()
	case <-time.After(time.Second):
		t.Fatal("second tenant waiter was not granted after first released")
	}
	if stats := controller.Stats(); stats.Active != 0 || stats.Queued != 0 {
		t.Fatalf("queue did not drain: %+v", stats)
	}
}

func TestAdmissionControllerCancelGrantRaceReturnsAllCapacity(t *testing.T) {
	for attempt := 0; attempt < 100; attempt++ {
		controller, err := NewAdmissionController(AdmissionConfig{Total: 1, Queue: 1, PerTenant: 1, WaitTimeout: time.Second})
		if err != nil {
			t.Fatalf("NewAdmissionController: %v", err)
		}
		_, held, err := controller.Acquire(context.Background())
		if err != nil {
			t.Fatalf("hold admission: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan struct {
			permit *AdmissionPermit
			err    error
		}, 1)
		go func() {
			_, permit, acquireErr := controller.Acquire(ctx)
			result <- struct {
				permit *AdmissionPermit
				err    error
			}{permit, acquireErr}
		}()
		waitForAdmissionStats(t, controller, 1, 1)
		start := make(chan struct{})
		go func() { <-start; held.Release() }()
		go func() { <-start; cancel() }()
		close(start)
		outcome := <-result
		if outcome.permit != nil {
			outcome.permit.Release()
		}
		if outcome.err != nil && !errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("attempt %d acquire = %v", attempt, outcome.err)
		}
		waitForAdmissionStats(t, controller, 0, 0)
	}
}

func TestAdmissionQueueTombstonesCompactAndRemainBounded(t *testing.T) {
	const queue = 4
	controller, err := NewAdmissionController(AdmissionConfig{Total: 1, Queue: queue, PerTenant: 1})
	if err != nil {
		t.Fatalf("NewAdmissionController: %v", err)
	}
	controller.active = 1 // prevent fixture waiters from being granted
	for cycle := 0; cycle < 100; cycle++ {
		waiters := make([]*admissionWaiter, 0, queue)
		controller.mu.Lock()
		for i := 0; i < queue; i++ {
			waiter := &admissionWaiter{tenant: "tenant-a"}
			controller.appendWaiterLocked(waiter)
			waiters = append(waiters, waiter)
		}
		for _, waiter := range waiters {
			controller.removeWaiterLocked(waiter)
			if waiter.queued || waiter.index != -1 {
				t.Fatalf("cycle %d removed waiter retained queue state", cycle)
			}
		}
		// This append crosses the full physical slice with no live waiters,
		// forcing a bounded compaction before it enters the new queue.
		fresh := &admissionWaiter{tenant: "tenant-b"}
		controller.appendWaiterLocked(fresh)
		if controller.queued != 1 || len(controller.waiters) != 1 || fresh.index != 0 {
			controller.mu.Unlock()
			t.Fatalf("cycle %d compacted queue = queued:%d len:%d fresh:%d", cycle, controller.queued, len(controller.waiters), fresh.index)
		}
		if cap(controller.waiters) > 2*queue {
			controller.mu.Unlock()
			t.Fatalf("cycle %d physical queue grew beyond bounded multiple: cap=%d", cycle, cap(controller.waiters))
		}
		for _, waiter := range controller.waiters[1:cap(controller.waiters)] {
			if waiter != nil {
				controller.mu.Unlock()
				t.Fatalf("cycle %d retained tombstoned waiter reference", cycle)
			}
		}
		controller.removeWaiterLocked(fresh)
		controller.mu.Unlock()
	}
	if stats := controller.Stats(); stats.Active != 1 || stats.Queued != 0 {
		t.Fatalf("final queue state = %+v", stats)
	}
}

func TestAdmissionQueueDrainClearsPhysicalQueueForFastReuse(t *testing.T) {
	controller, err := NewAdmissionController(AdmissionConfig{Total: 1, Queue: 4, PerTenant: 1})
	if err != nil {
		t.Fatalf("NewAdmissionController: %v", err)
	}
	controller.mu.Lock()
	controller.active = 1 // keep the fixture waiters queued until all are removed
	for i := 0; i < controller.cfg.Queue; i++ {
		controller.appendWaiterLocked(&admissionWaiter{tenant: "tenant-a"})
	}
	for _, waiter := range append([]*admissionWaiter(nil), controller.waiters...) {
		controller.removeWaiterLocked(waiter)
	}
	if controller.queued != 0 || len(controller.waiters) != 0 {
		controller.mu.Unlock()
		t.Fatalf("drained queue retained physical work: queued=%d len=%d", controller.queued, len(controller.waiters))
	}
	controller.active = 0
	controller.mu.Unlock()

	// A drained controller remains immediately reusable without historical
	// tombstones becoming logical queue work.
	_, permit, err := controller.Acquire(context.Background())
	if err != nil {
		t.Fatalf("reuse acquire: %v", err)
	}
	permit.Release()
	if stats := controller.Stats(); stats.Active != 0 || stats.Queued != 0 {
		t.Fatalf("reuse stats = %+v", stats)
	}
}

func waitForAdmissionStats(t *testing.T, controller *AdmissionController, active, queued int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		stats := controller.Stats()
		if stats.Active == active && stats.Queued == queued {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("stats = %+v, want active=%d queued=%d", controller.Stats(), active, queued)
}

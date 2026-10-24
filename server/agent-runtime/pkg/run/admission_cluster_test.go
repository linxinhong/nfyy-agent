package run

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newClusterController(t *testing.T, cluster ClusterAdmission, queue int, wait time.Duration) *AdmissionController {
	t.Helper()
	c, err := NewAdmissionController(AdmissionConfig{Total: 10, PerTenant: 10, Queue: queue, WaitTimeout: wait, Cluster: cluster})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClusterAdmissionIsSharedAcrossControllers(t *testing.T) {
	cluster := NewSharedAdmission(1, 0)
	a := newClusterController(t, cluster, 4, 2*time.Second)
	b := newClusterController(t, cluster, 4, 2*time.Second)

	_, permitA, err := a.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	granted := make(chan error, 1)
	go func() {
		_, permitB, err := b.Acquire(context.Background())
		if err == nil {
			permitB.Release()
		}
		granted <- err
	}()
	select {
	case err := <-granted:
		t.Fatalf("second replica admitted while the cluster was full: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	permitA.Release()
	permitA.Release() // idempotent: must not free a second cluster slot
	if err := <-granted; err != nil {
		t.Fatalf("second replica not admitted after release: %v", err)
	}
	if _, p, err := a.Acquire(context.Background()); err != nil {
		t.Fatalf("slot leaked: %v", err)
	} else {
		p.Release()
	}
}

func TestClusterAdmissionTimeoutAndQueueBound(t *testing.T) {
	cluster := NewSharedAdmission(1, 0)
	holder := newClusterController(t, cluster, 1, time.Second)
	_, permit, err := holder.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer permit.Release()

	waiter := newClusterController(t, cluster, 1, 50*time.Millisecond)
	if _, _, err := waiter.Acquire(context.Background()); !errors.Is(err, ErrAdmissionTimeout) {
		t.Fatalf("err = %v, want ErrAdmissionTimeout", err)
	}
	noQueue := newClusterController(t, cluster, 0, time.Second)
	if _, _, err := noQueue.Acquire(context.Background()); !errors.Is(err, ErrAdmissionQueueFull) {
		t.Fatalf("err = %v, want ErrAdmissionQueueFull", err)
	}
}

func TestClusterSlotReturnedWhenLocalAdmissionFails(t *testing.T) {
	cluster := NewSharedAdmission(5, 0)
	c, err := NewAdmissionController(AdmissionConfig{Total: 1, PerTenant: 1, Queue: 0, Cluster: cluster})
	if err != nil {
		t.Fatal(err)
	}
	_, permit, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer permit.Release()
	if _, _, err := c.Acquire(context.Background()); !errors.Is(err, ErrAdmissionQueueFull) {
		t.Fatalf("err = %v", err)
	}
	if active := cluster.(*sharedAdmission).active; active != 1 {
		t.Fatalf("cluster slots in use = %d, want 1 (the refused caller's slot returned)", active)
	}
}

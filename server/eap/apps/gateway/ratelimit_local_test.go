package main

import (
	"context"
	"testing"
	"time"
)

func newLocalWindow(limit int64, max int) *LocalWindow {
	return &LocalWindow{Limit: limit, Window: time.Minute, MaxSubjects: max, Now: func() time.Time { return time.Unix(0, 0) }}
}

// A burst over the configured limit is rejected within the window and
// admitted again after the window rotates.
func TestLocalWindowBurstTripsLimit(t *testing.T) {
	w := newLocalWindow(3, 100)
	for i := 0; i < 3; i++ {
		if ok, err := w.Allow(context.Background(), "k1"); err != nil || !ok {
			t.Fatalf("request %d must pass: %v %v", i+1, ok, err)
		}
	}
	if ok, err := w.Allow(context.Background(), "k1"); err != nil || ok {
		t.Fatalf("4th request in window must be rejected: %v %v", ok, err)
	}
	w.Now = func() time.Time { return time.Unix(121, 0) } // next window
	if ok, err := w.Allow(context.Background(), "k1"); err != nil || !ok {
		t.Fatalf("next window must admit again: %v %v", ok, err)
	}
}

// Subjects are independent.
func TestLocalWindowPerSubject(t *testing.T) {
	w := newLocalWindow(1, 100)
	if ok, _ := w.Allow(context.Background(), "a"); !ok {
		t.Fatalf("a first request must pass")
	}
	if ok, _ := w.Allow(context.Background(), "b"); !ok {
		t.Fatalf("b is a different subject and must pass")
	}
	if ok, _ := w.Allow(context.Background(), "a"); ok {
		t.Fatalf("a second request must be rejected")
	}
}

// Memory is bounded: pushing more distinct subjects than MaxSubjects evicts
// instead of growing the map without limit.
func TestLocalWindowBoundedMemory(t *testing.T) {
	w := newLocalWindow(1000, 4)
	for i := 0; i < 50; i++ {
		_, _ = w.Allow(context.Background(), string(rune('a'+i%26))+"-"+string(rune('A'+i%26))+string(rune(i)))
	}
	w.mu.Lock()
	n := len(w.buckets)
	w.mu.Unlock()
	if n > 4 {
		t.Fatalf("bucket map grew to %d, want <= MaxSubjects", n)
	}
}

// The nil/degenerate limiter is fail-open by contract.
func TestLocalWindowFailOpen(t *testing.T) {
	var w *LocalWindow
	if ok, err := w.Allow(context.Background(), "k"); err != nil || !ok {
		t.Fatalf("nil limiter must allow: %v %v", ok, err)
	}
	if ok, err := (&LocalWindow{Limit: 0, Window: time.Minute}).Allow(context.Background(), "k"); err != nil || !ok {
		t.Fatalf("zero limit must allow: %v %v", ok, err)
	}
}

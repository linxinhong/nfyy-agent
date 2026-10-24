package controlcache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheServesFreshWithoutReload(t *testing.T) {
	var loads atomic.Int64
	clock := 0
	c := New[int](func() time.Duration { return time.Minute })
	c.Now = func() time.Time { return time.Unix(int64(clock), 0) }
	load := func(context.Context) (int, error) {
		loads.Add(1)
		return 7, nil
	}
	for i := 0; i < 50; i++ {
		v, err := c.Get(context.Background(), "k", load)
		if err != nil || v != 7 {
			t.Fatalf("get %d: %v %v", i, v, err)
		}
	}
	if loads.Load() != 1 {
		t.Fatalf("fresh hits must not reload: %d loads", loads.Load())
	}
	// Past TTL the value reloads.
	clock = int(time.Minute.Seconds()) + 1
	if _, err := c.Get(context.Background(), "k", load); err != nil {
		t.Fatal(err)
	}
	if loads.Load() != 2 {
		t.Fatalf("expired entry must reload: %d loads", loads.Load())
	}
}

func TestCacheNonPositiveTTLAlwaysReloads(t *testing.T) {
	var loads atomic.Int64
	c := New[int](func() time.Duration { return 0 })
	load := func(context.Context) (int, error) {
		loads.Add(1)
		return 1, nil
	}
	for i := 0; i < 3; i++ {
		if _, err := c.Get(context.Background(), "k", load); err != nil {
			t.Fatal(err)
		}
	}
	if loads.Load() != 3 {
		t.Fatalf("ttl<=0 must always reload: %d loads", loads.Load())
	}
}

func TestCacheSharesOneInFlightLoad(t *testing.T) {
	var loads atomic.Int64
	release := make(chan struct{})
	c := New[string](func() time.Duration { return time.Minute })
	load := func(context.Context) (string, error) {
		loads.Add(1)
		<-release
		return "v", nil
	}

	first := make(chan error, 1)
	go func() {
		_, err := c.Get(context.Background(), "k", load)
		first <- err
	}()
	// Wait until the loader is inside the in-flight load, then stack joiners.
	for loads.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.Get(context.Background(), "k", load)
			if err != nil || v != "v" {
				t.Errorf("joiner got %q %v", v, err)
			}
		}()
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if loads.Load() != 1 {
		t.Fatalf("concurrent Gets must share one load: %d", loads.Load())
	}
}

func TestCacheFailureDoesNotStick(t *testing.T) {
	var loads atomic.Int64
	boom := errors.New("load failed")
	c := New[int](func() time.Duration { return time.Minute })
	fail := true
	load := func(context.Context) (int, error) {
		n := loads.Add(1)
		if fail && n == 1 {
			return 0, boom
		}
		return 5, nil
	}
	if _, err := c.Get(context.Background(), "k", load); !errors.Is(err, boom) {
		t.Fatalf("load error must propagate: %v", err)
	}
	v, err := c.Get(context.Background(), "k", load)
	if err != nil || v != 5 {
		t.Fatalf("next Get must retry after failure: %v %v", v, err)
	}
	if loads.Load() != 2 {
		t.Fatalf("unexpected load count: %d", loads.Load())
	}
}

func TestCacheWaiterCancelledWhileLoading(t *testing.T) {
	release := make(chan struct{})
	var entered atomic.Int64
	c := New[int](func() time.Duration { return time.Minute })
	load := func(context.Context) (int, error) {
		entered.Add(1)
		<-release
		return 1, nil
	}
	loaderErr := make(chan error, 1)
	go func() {
		_, err := c.Get(context.Background(), "k", load)
		loaderErr <- err
	}()
	for entered.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Get(ctx, "k", load); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter must fail fast: %v", err)
	}
	close(release)
	if err := <-loaderErr; err != nil {
		t.Fatal(err)
	}
	// The cancelled waiter must not have poisoned the cache.
	if v, err := c.Get(context.Background(), "k", load); err != nil || v != 1 {
		t.Fatalf("post-cancel Get: %v %v", v, err)
	}
}

func TestCacheKeysAreIndependent(t *testing.T) {
	c := New[int](func() time.Duration { return time.Minute })
	load := func(ctx context.Context) (int, error) {
		return 1, nil
	}
	if _, err := c.Get(context.Background(), "a", load); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), "b", load); err != nil {
		t.Fatal(err)
	}
	if len(c.entries) != 2 {
		t.Fatalf("keys must not collide: %d entries", len(c.entries))
	}
}

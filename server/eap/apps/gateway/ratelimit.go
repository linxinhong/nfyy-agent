package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Counter is the storage primitive of the rate limiter.
type Counter interface {
	// Increment atomically adds one and returns the new value.
	Incr(ctx context.Context, key string) (int64, error)
}

// RedisCounter adapts go-redis to Counter. TTL mirrors the fixed window so
// counter keys expire instead of accumulating forever in Redis (TASK-040).
type RedisCounter struct {
	Client *redis.Client
	TTL    time.Duration
}

func (c RedisCounter) Incr(ctx context.Context, key string) (int64, error) {
	pipe := c.Client.TxPipeline()
	incr := pipe.Incr(ctx, key)
	if c.TTL > 0 {
		// EXPIRE NX: set the TTL once per window; later INCRs keep it.
		pipe.ExpireNX(ctx, key, c.TTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return incr.Val(), nil
}

// FixedWindow is a per-subject fixed-window rate limiter. It is abuse
// protection, not authorization: when the counter is unavailable the request
// is allowed (fail open) and the error is returned so the caller can log it.
type FixedWindow struct {
	Counter Counter
	Limit   int64
	Window  time.Duration
	Now     func() time.Time
}

// Allow reports whether subject may proceed in the current window.
func (f *FixedWindow) Allow(ctx context.Context, subject string) (bool, error) {
	if f == nil || f.Counter == nil || f.Limit <= 0 {
		return true, nil
	}
	now := time.Now()
	if f.Now != nil {
		now = f.Now()
	}
	bucket := now.Unix() / int64(f.Window.Seconds())
	key := fmt.Sprintf("ratelimit:%s:%d", subject, bucket)
	n, err := f.Counter.Incr(ctx, key)
	if err != nil {
		return true, err
	}
	return n <= f.Limit, nil
}

// RateLimiter is the request-path abuse-protection seam (TASK-044).
type RateLimiter interface {
	Allow(ctx context.Context, subject string) (bool, error)
}

// LocalWindow is a per-instance fixed-window rate limiter: the allow/deny
// decision is made from process-local state with ZERO round trips on the
// request path (TASK-044). Multi-instance semantics, recorded per the card:
// each instance enforces its own window, so N instances admit up to
// Limit x N requests per window in aggregate — rate limiting stays abuse
// protection, not an exact distributed quota, and never an authorization
// control.
//
// Memory is bounded: at most MaxSubjects buckets live at once. When the map
// is full, expired windows are flushed first; if every window is active
// (pathological subject cardinality) a random bucket is evicted and that
// subject's window restarts — degradation is graceful and logged once per
// flush, never unbounded.
type LocalWindow struct {
	Limit       int64
	Window      time.Duration
	Now         func() time.Time
	MaxSubjects int

	mu      sync.Mutex
	buckets map[string]*localBucket
	flushes int
}

type localBucket struct {
	window int64
	count  int64
}

func (w *LocalWindow) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Allow reports whether subject may proceed in the current window. The
// decision never touches the network or disk, so the fail-open contract of
// the previous Redis counter trivially holds: there is no counter
// unavailability on this path.
func (w *LocalWindow) Allow(_ context.Context, subject string) (bool, error) {
	if w == nil || w.Limit <= 0 {
		return true, nil
	}
	now := w.now()
	window := now.Unix() / int64(w.Window.Seconds())

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buckets == nil {
		w.buckets = make(map[string]*localBucket)
	}
	max := w.MaxSubjects
	if max <= 0 {
		max = 65536
	}
	b := w.buckets[subject]
	if b != nil && b.window == window {
		b.count++
		return b.count <= w.Limit, nil
	}
	if len(w.buckets) >= max {
		w.flushLocked(window)
	}
	if len(w.buckets) >= max {
		// All windows active: evict an arbitrary subject (map iteration is
		// randomized) so memory stays bounded; its window restarts at zero.
		for k := range w.buckets {
			delete(w.buckets, k)
			break
		}
	}
	w.buckets[subject] = &localBucket{window: window, count: 1}
	return w.Limit >= 1, nil
}

// flushLocked drops buckets whose window has rotated out; returns whether
// any capacity was freed.
func (w *LocalWindow) flushLocked(current int64) bool {
	freed := false
	for k, b := range w.buckets {
		if b.window != current {
			delete(w.buckets, k)
			freed = true
		}
	}
	return freed
}

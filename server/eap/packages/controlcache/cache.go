// Package controlcache provides the bounded process-local TTL cache used for
// read-mostly control data on the request path (policy rules, route plans,
// provider targets, pricing, DLP rules). Contract (ADR-0025):
//
//   - values younger than TTL are served without a load;
//   - the cache never holds its lock across a load — concurrent Gets for the
//     same expired key share one in-flight load instead of stampeding the
//     database behind a mutex;
//   - a load failure is returned to the caller and dropped from the cache, so
//     the next Get retries: stale values are never served after a failure and
//     failures never stick (fail closed);
//   - cached values are shared between callers and MUST be treated as
//     immutable (return copies at the seam if mutation is needed).
package controlcache

import (
	"context"
	"sync"
	"time"
)

// entry is one cached key. done is closed exactly once after value/err are
// written; receiving from it establishes the happens-before edge waiters need.
type entry[V any] struct {
	done     chan struct{}
	value    V
	err      error
	loadedAt time.Time
}

// Cache is a TTL cache with per-key single-flight loads. TTL is evaluated per
// Get so callers can keep it dynamic (<= 0 means always reload); Now allows a
// fake clock in tests.
type Cache[V any] struct {
	TTL func() time.Duration
	Now func() time.Time

	mu      sync.Mutex
	entries map[string]*entry[V]
}

// New builds a cache governed by the per-Get ttl function.
func New[V any](ttl func() time.Duration) *Cache[V] {
	return &Cache[V]{TTL: ttl, entries: make(map[string]*entry[V])}
}

func (c *Cache[V]) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Cache[V]) ttl() time.Duration {
	if c.TTL == nil {
		return 0
	}
	return c.TTL()
}

// Get returns the cached value when it is younger than TTL; otherwise it
// loads once and every concurrent Get for the same key shares that load.
func (c *Cache[V]) Get(ctx context.Context, key string, load func(context.Context) (V, error)) (V, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		select {
		case <-e.done: // settled: serve while fresh
			if ttl := c.ttl(); ttl > 0 && c.now().Sub(e.loadedAt) < ttl {
				value := e.value
				c.mu.Unlock()
				return value, nil
			}
		default: // a load is in flight: join it instead of reloading
			done := e.done
			c.mu.Unlock()
			select {
			case <-done:
				return e.value, e.err
			case <-ctx.Done():
				var zero V
				return zero, ctx.Err()
			}
		}
	}
	// Absent or expired: install a fresh loading entry and load outside the
	// lock so other keys (and joining readers) never queue behind the load.
	e := &entry[V]{done: make(chan struct{})}
	c.entries[key] = e
	c.mu.Unlock()

	// Control-data loads are shared by every request that hits this key:
	// one caller's cancellation must not fail the load for all of them
	// (a cancel storm would otherwise turn into fail-closed denials for
	// innocent joiners). The load runs detached from the request; when the
	// database is saturated the load simply queues like any other query
	// instead of timing out into fail-closed denials.
	value, err := load(context.WithoutCancel(ctx))
	c.mu.Lock()
	if err != nil {
		// Failures never stick: the next Get starts a fresh load.
		delete(c.entries, key)
	} else {
		e.value = value
		e.loadedAt = c.now()
		c.entries[key] = e
	}
	close(e.done)
	c.mu.Unlock()
	return value, err
}

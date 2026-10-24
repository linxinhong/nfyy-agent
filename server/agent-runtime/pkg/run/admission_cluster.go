package run

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ClusterAdmission claims capacity shared by every replica, on top of the
// process-local limits of an AdmissionController. TryAcquire never blocks
// waiting for capacity: it reports ok=false when the cluster is full and
// the controller retries with backoff within its WaitTimeout. release
// returns the slot and must be safe to call once from any goroutine.
// Implementations should lease slots so a crashed replica's slots expire
// (see postgres.DB.ClusterAdmission).
type ClusterAdmission interface {
	TryAcquire(ctx context.Context, tenant string) (release func(), ok bool, err error)
}

const (
	clusterPollMin = 20 * time.Millisecond
	clusterPollMax = time.Second
)

// acquireCluster claims a cluster slot, polling until WaitTimeout. Waiters
// count against Queue like local waiters do.
func (c *AdmissionController) acquireCluster(ctx context.Context, tenant string) (func(), error) {
	release, ok, err := c.cfg.Cluster.TryAcquire(ctx, tenant)
	if err != nil || ok {
		return release, err
	}
	c.mu.Lock()
	if c.clusterWaiting >= c.cfg.Queue {
		c.rejected++
		c.mu.Unlock()
		return nil, ErrAdmissionQueueFull
	}
	c.clusterWaiting++
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.clusterWaiting--
		c.mu.Unlock()
	}()

	waitCtx, cancel := context.WithTimeout(ctx, c.cfg.WaitTimeout)
	defer cancel()
	delay := clusterPollMin
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-waitCtx.Done():
			if ctx.Err() == nil && errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return nil, ErrAdmissionTimeout
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
		release, ok, err := c.cfg.Cluster.TryAcquire(waitCtx, tenant)
		if err != nil || ok {
			return release, err
		}
		if delay *= 2; delay > clusterPollMax {
			delay = clusterPollMax
		}
		timer.Reset(delay)
	}
}

// NewSharedAdmission returns an in-process ClusterAdmission with total and
// per-tenant limits (zero is unlimited). It lets several controllers in
// one process share capacity and serves as a reference implementation.
func NewSharedAdmission(total, perTenant int) ClusterAdmission {
	return &sharedAdmission{total: total, perTenant: perTenant, tenants: make(map[string]int)}
}

type sharedAdmission struct {
	total, perTenant int

	mu      sync.Mutex
	active  int
	tenants map[string]int
}

func (s *sharedAdmission) TryAcquire(_ context.Context, tenant string) (func(), bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if (s.total > 0 && s.active >= s.total) || (tenant != "" && s.perTenant > 0 && s.tenants[tenant] >= s.perTenant) {
		return nil, false, nil
	}
	s.active++
	s.tenants[tenant]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.active--
			if s.tenants[tenant]--; s.tenants[tenant] <= 0 {
				delete(s.tenants, tenant)
			}
		})
	}, true, nil
}

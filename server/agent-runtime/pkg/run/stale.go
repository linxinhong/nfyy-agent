package run

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
)

// StaleRunReason is the error recorded on runs expired by ExpireStaleRuns.
const StaleRunReason = "run lease expired: owning process stopped sending heartbeats (crash or restart)"

// ExpireStaleRuns marks leased running runs whose owner stopped
// heartbeating as failed, so clients polling them see a terminal status and
// sessions stop counting them as in flight. It never re-executes a run:
// side effects of a crashed tool loop are unknown and must not repeat.
// Stores without db.RunLeaseStore expire nothing.
func ExpireStaleRuns(ctx context.Context, store any, limit int) ([]string, error) {
	leases, ok := store.(db.RunLeaseStore)
	if !ok {
		return nil, nil
	}
	ids, err := leases.ExpireStaleRuns(ctx, time.Now(), StaleRunReason, limit)
	if len(ids) > 0 {
		staleRunsExpired.Add(context.WithoutCancel(ctx), int64(len(ids)))
	}
	return ids, err
}

// RunStaleRunReaper calls ExpireStaleRuns at startup and then every interval
// until ctx is done. Several replicas may run it concurrently.
func RunStaleRunReaper(ctx context.Context, store any, interval time.Duration, onError func(error)) {
	if interval <= 0 {
		interval = time.Minute
	}
	reap := func() {
		for {
			ids, err := ExpireStaleRuns(ctx, store, 100)
			if err != nil {
				if onError != nil && ctx.Err() == nil {
					onError(err)
				}
				return
			}
			if len(ids) < 100 {
				return
			}
		}
	}
	reap()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reap()
		}
	}
}

// StartRunHeartbeat refreshes a leased run every interval until stop is
// called or ctx is done. When the store reports that the run is no longer
// running (cancelled or expired elsewhere), onLost is called once and the
// heartbeat ends.
func StartRunHeartbeat(ctx context.Context, store db.RunLeaseStore, query db.RunQuery, interval time.Duration, onLost func()) (stop func()) {
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			touchCtx, cancel := context.WithTimeout(ctx, interval)
			err := store.TouchRun(touchCtx, query)
			cancel()
			if errors.Is(err, db.ErrConflict) {
				onLost()
				return
			}
			if errors.Is(err, db.ErrNotFound) {
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-finished
		})
	}
}

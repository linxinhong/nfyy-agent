package run

import (
	"context"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
)

// DeleteIdleSessions deletes sessions (with their runs) not updated for
// maxAge and without in-flight runs. Stores without
// db.SessionRetentionStore delete nothing.
func DeleteIdleSessions(ctx context.Context, store any, maxAge time.Duration, limit int) (int, error) {
	retention, ok := store.(db.SessionRetentionStore)
	if !ok || maxAge <= 0 {
		return 0, nil
	}
	n, err := retention.DeleteIdleSessions(ctx, time.Now().Add(-maxAge), limit)
	if n > 0 {
		sessionsDeleted.Add(context.WithoutCancel(ctx), int64(n))
	}
	return n, err
}

// RunSessionJanitor applies a session retention period at startup and then
// every interval until ctx is done. Several replicas may run it.
func RunSessionJanitor(ctx context.Context, store any, maxAge, interval time.Duration, onError func(error)) {
	if interval <= 0 {
		interval = time.Hour
	}
	sweep := func() {
		for ctx.Err() == nil {
			n, err := DeleteIdleSessions(ctx, store, maxAge, 500)
			if err != nil {
				if onError != nil && ctx.Err() == nil {
					onError(err)
				}
				return
			}
			if n < 500 {
				return
			}
		}
	}
	sweep()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

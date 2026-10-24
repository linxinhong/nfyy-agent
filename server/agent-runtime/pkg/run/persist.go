package run

import (
	"context"
	"errors"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// Finalize durably transitions a run, retrying transient storage failures.
// A concurrent writer that already applied the same terminal state is treated
// as an idempotent success; a different winner remains a conflict.
func Finalize(ctx context.Context, store db.RunStore, output *session.RunOutput, expected session.RunStatus) error {
	if store == nil {
		return nil
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		output.UpdatedAt = time.Now().UTC()
		err := store.UpdateRun(persistCtx, output, expected)
		if err == nil {
			return nil
		}
		if errors.Is(err, db.ErrConflict) {
			winner, getErr := store.GetRun(persistCtx, db.RunQuery{RunID: output.RunID, UserID: output.UserID})
			if getErr != nil {
				return getErr
			}
			if winner != nil && winner.Status == output.Status {
				return nil
			}
			return err
		}
		lastErr = err
		if attempt < 2 {
			timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
			select {
			case <-timer.C:
			case <-persistCtx.Done():
				timer.Stop()
				return errors.Join(lastErr, persistCtx.Err())
			}
		}
	}
	return lastErr
}

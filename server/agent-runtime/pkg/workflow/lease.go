package workflow

import (
	"context"
	"errors"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

const defaultRunLeaseTimeout = 2 * time.Minute

// leaseStore returns the lease capability of the workflow store.
func (w *Workflow) leaseStore() (db.RunLeaseStore, time.Duration) {
	if w.cfg.DB == nil || w.cfg.RunLeaseTimeout < 0 {
		return nil, 0
	}
	store, ok := w.cfg.DB.(db.RunLeaseStore)
	if !ok {
		return nil, 0
	}
	if w.cfg.RunLeaseTimeout > 0 {
		return store, w.cfg.RunLeaseTimeout
	}
	return store, defaultRunLeaseTimeout
}

// markLease records this process's lease on a run about to be persisted
// or claimed; a process without leases clears a stale one.
func (w *Workflow) markLease(out *session.RunOutput) {
	out.LeaseTimeout = 0
	if store, timeout := w.leaseStore(); store != nil {
		out.LeaseTimeout = timeout
	}
}

// guardLease heartbeats a durable running run. The returned context is
// cancelled with agent.ErrRunCancelledExternally when the run stops being
// running elsewhere; cancel lets checkpoints report the same.
func (w *Workflow) guardLease(ctx context.Context, out *session.RunOutput) (context.Context, context.CancelCauseFunc, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	stop := func() {}
	if store, timeout := w.leaseStore(); store != nil && out.LeaseTimeout > 0 {
		query := db.RunQuery{RunID: out.RunID, UserID: out.UserID, Namespace: out.Namespace}
		stop = runmanager.StartRunHeartbeat(ctx, store, query, timeout/4, func() { cancel(agent.ErrRunCancelledExternally) })
	}
	return ctx, cancel, func() {
		stop()
		cancel(nil)
	}
}

// settledElsewhere reports whether a failed final update lost only to a
// write that already recorded the same status (e.g. both cancelled).
func settledElsewhere(ctx context.Context, store db.RunStore, out *session.RunOutput, err error) bool {
	if store == nil || !errors.Is(err, db.ErrConflict) {
		return false
	}
	stored, getErr := store.GetRun(context.WithoutCancel(ctx), db.RunQuery{RunID: out.RunID, UserID: out.UserID})
	return getErr == nil && stored != nil && stored.Status == out.Status
}

// cancelledError is the error recorded for a cancelled run: the cause, so
// cancellation from another replica is distinguishable.
func cancelledError(ctx context.Context) error {
	return context.Cause(ctx)
}

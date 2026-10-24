package run

import (
	"context"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
)

// Reconciler is the host-runnable orphan-recovery component (RC-08): it
// periodically marks stale invoking attempts as unknown (reconciliation) and
// drains the outbox through an AuditExporter (at-least-once, deduplicated).
type Reconciler struct {
	journal  ExecutionJournalStore
	exporter AuditExporter
	scope    execution.NamespaceScope
	workerID string
}

// NewReconciler creates a reconciler for one namespace. A nil exporter skips
// the outbox drain (reconciliation only).
func NewReconciler(journal ExecutionJournalStore, exporter AuditExporter, scope execution.NamespaceScope, workerID string) *Reconciler {
	return &Reconciler{journal: journal, exporter: exporter, scope: scope, workerID: workerID}
}

// ReconcileOnce performs one reconciliation pass and one outbox drain.
// It never re-runs business side effects: unknown attempts are only marked,
// and outbox delivery is at-least-once with stable event IDs.
func (r *Reconciler) ReconcileOnce(ctx context.Context, olderThan time.Duration, limit int) (unknown int, exported int, err error) {
	if r.journal == nil {
		return 0, 0, nil
	}
	unknown, err = r.journal.ReconcileUnknown(ctx, r.scope, olderThan)
	if err != nil {
		return unknown, 0, err
	}
	if r.exporter != nil {
		exported, _, err = ExportAndAck(ctx, r.journal, r.scope, r.workerID, r.exporter, limit)
		if err != nil {
			return unknown, exported, err
		}
	}
	return unknown, exported, nil
}

// Run loops ReconcileOnce until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context, interval, olderThan time.Duration, limit int) {
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _, _ = r.ReconcileOnce(ctx, olderThan, limit)
		}
	}
}

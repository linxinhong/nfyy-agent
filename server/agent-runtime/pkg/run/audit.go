package run

import (
	"context"
	"errors"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
)

// AuditEvent is the versioned outcome record (RC-10). It extends the
// AuditRequestedRecord fields with outcome and reason; no bodies, no
// arguments, no free-form extension maps.
type AuditEvent struct {
	EventID       string
	Version       int
	ExecutionID   string
	AttemptID     string
	RunID         string
	Namespace     string
	Subject       string
	ToolName      string
	RequirementID string

	Phase   string
	Outcome string // requested|succeeded|failed|cancelled|unknown
	Reason  string // stable reason code

	RegistryRevision            string
	DescriptorPolicyRevision    string
	AuthorizationPolicyRevision string

	ArgumentsHash      string
	IdempotencyKeyHash string
	Timestamp          time.Time
}

// AuditOutcome constants.
const (
	AuditOutcomeRequested = "requested"
	AuditOutcomeSucceeded = "succeeded"
	AuditOutcomeFailed    = "failed"
	AuditOutcomeCancelled = "cancelled"
	AuditOutcomeUnknown   = "unknown"
)

// AuditExporter consumes audit events asynchronously with at-least-once
// delivery: events carry stable IDs and consumers deduplicate. Export
// failures must not roll back or re-run business side effects; the outbox
// retains undelivered events.
type AuditExporter interface {
	// Export delivers one event. A nil/error return leaves the event in
	// the outbox for retry.
	Export(ctx context.Context, event AuditEvent) error
}

// MemoryAuditExporter is a test/reference exporter recording delivered
// events and reporting failures on demand.
type MemoryAuditExporter struct {
	delivered map[string]AuditEvent
	failOn    func(AuditEvent) bool
}

// NewMemoryAuditExporter creates an in-memory exporter.
func NewMemoryAuditExporter() *MemoryAuditExporter {
	return &MemoryAuditExporter{delivered: make(map[string]AuditEvent)}
}

// SetFailOn configures a failure predicate for tests.
func (e *MemoryAuditExporter) SetFailOn(f func(AuditEvent) bool) *MemoryAuditExporter {
	e.failOn = f
	return e
}

func (e *MemoryAuditExporter) Export(ctx context.Context, event AuditEvent) error {
	if e.failOn != nil && e.failOn(event) {
		return errors.New("audit: export failed")
	}
	e.delivered[event.EventID] = event
	return nil
}

// Delivered returns the delivered events keyed by stable event ID.
func (e *MemoryAuditExporter) Delivered() map[string]AuditEvent {
	out := make(map[string]AuditEvent, len(e.delivered))
	for id, event := range e.delivered {
		out[id] = event
	}
	return out
}

// TransactionalAuditLedger is the narrow capability view over
// ExecutionJournalStore: requested is written before any tool call; outcome
// appends without overwriting requested; stale invoking attempts reconcile
// to unknown.
type TransactionalAuditLedger interface {
	// Requested persists the audit-intent record transactionally with the
	// attempt. A failure here means the managed side effect must not run.
	Requested(ctx context.Context, scope execution.NamespaceScope, attempt *Attempt, event AuditEvent) error
	// Outcome appends a terminal/unknown audit outcome.
	Outcome(ctx context.Context, scope execution.NamespaceScope, attemptID string, outcome string, reason string, event AuditEvent) error
	// ReconcileUnknown marks stale invoking attempts as unknown.
	ReconcileUnknown(ctx context.Context, scope execution.NamespaceScope, olderThan time.Duration) (int, error)
}

// journalAuditLedger adapts an ExecutionJournalStore into the narrow ledger
// view.
type journalAuditLedger struct {
	journal ExecutionJournalStore
}

// NewTransactionalAuditLedger wraps a journal store.
func NewTransactionalAuditLedger(journal ExecutionJournalStore) TransactionalAuditLedger {
	return &journalAuditLedger{journal: journal}
}

func (l *journalAuditLedger) Requested(ctx context.Context, scope execution.NamespaceScope, attempt *Attempt, event AuditEvent) error {
	requested := &AuditRequestedRecord{
		EventID:                     event.EventID,
		ExecutionID:                 event.ExecutionID,
		AttemptID:                   event.AttemptID,
		RunID:                       event.RunID,
		Namespace:                   event.Namespace,
		Subject:                     event.Subject,
		ToolName:                    event.ToolName,
		RequirementID:               event.RequirementID,
		Phase:                       event.Phase,
		RegistryRevision:            event.RegistryRevision,
		DescriptorPolicyRevision:    event.DescriptorPolicyRevision,
		AuthorizationPolicyRevision: event.AuthorizationPolicyRevision,
		ArgumentsHash:               event.ArgumentsHash,
		IdempotencyKeyHash:          event.IdempotencyKeyHash,
		Timestamp:                   event.Timestamp,
	}
	outbox := &OutboxEvent{
		EventID:     event.EventID,
		Namespace:   scope.Namespace,
		AggregateID: event.RunID,
		Type:        "audit." + event.Phase + "." + AuditOutcomeRequested,
	}
	return l.journal.BeginAttemptAndAppendRequested(ctx, scope, attempt, requested, outbox)
}

func (l *journalAuditLedger) Outcome(ctx context.Context, scope execution.NamespaceScope, attemptID string, outcome string, reason string, event AuditEvent) error {
	var state AttemptState
	switch outcome {
	case AuditOutcomeSucceeded:
		state = AttemptSucceeded
	case AuditOutcomeFailed:
		state = AttemptFailed
	case AuditOutcomeUnknown:
		state = AttemptUnknown
	case AuditOutcomeCancelled:
		state = AttemptFailed // cancellation before invoking is a failed attempt
	default:
		return errors.New("audit: unknown outcome")
	}
	outbox := &OutboxEvent{
		EventID:     event.EventID,
		Namespace:   scope.Namespace,
		AggregateID: event.RunID,
		Type:        "audit." + event.Phase + "." + outcome,
	}
	return l.journal.AppendOutcome(ctx, scope, attemptID, state, outbox)
}

func (l *journalAuditLedger) ReconcileUnknown(ctx context.Context, scope execution.NamespaceScope, olderThan time.Duration) (int, error) {
	return l.journal.ReconcileUnknown(ctx, scope, olderThan)
}

// ExportAndAck drains the outbox: claim, export, and ack each event.
// Export failures leave the event unacked for the next drain (at-least-once
// with stable-ID deduplication).
func ExportAndAck(ctx context.Context, journal ExecutionJournalStore, scope execution.NamespaceScope, workerID string, exporter AuditExporter, limit int) (exported, failed int, err error) {
	events, err := journal.ClaimOutbox(ctx, scope, workerID, limit)
	if err != nil {
		return 0, 0, err
	}
	for _, event := range events {
		auditEvent := AuditEvent{
			EventID:   event.EventID,
			RunID:     event.AggregateID,
			Phase:     "export",
			Outcome:   "delivered",
			Timestamp: event.CreatedAt,
		}
		if exportErr := exporter.Export(ctx, auditEvent); exportErr != nil {
			failed++
			continue
		}
		if err := journal.AckOutbox(ctx, scope, event.EventID); err != nil {
			return exported, failed, err
		}
		exported++
	}
	return exported, failed, nil
}

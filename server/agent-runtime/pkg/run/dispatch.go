// Package run implements the durable dispatch contracts: attempt state
// machine (D-9), per-run fencing tokens, the transactional execution
// journal (attempt + audit-requested + outbox in one transaction), and the
// at-least-once outbox contract. Worker/lease orchestration is built on
// these contracts by concrete stores.
package run

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
)

// DispatchStatus is the internal worker-ownership state, kept separate from
// the public RunStatus so existing consumers are unaffected.
type DispatchStatus string

const (
	DispatchQueued   DispatchStatus = "queued"
	DispatchClaimed  DispatchStatus = "claimed"
	DispatchRunning  DispatchStatus = "running"
	DispatchTerminal DispatchStatus = "terminal"
)

// AttemptState follows D-9: prepared -> claimed -> invoking ->
// succeeded|failed|unknown. An attempt that loses its trusted result after
// invoking becomes unknown/reconciliation_required; cancellation after
// invocation is only recorded as cancellation_requested, never as a
// fabricated terminal state.
type AttemptState string

const (
	AttemptPrepared  AttemptState = "prepared"
	AttemptClaimed   AttemptState = "claimed"
	AttemptInvoking  AttemptState = "invoking"
	AttemptSucceeded AttemptState = "succeeded"
	AttemptFailed    AttemptState = "failed"
	AttemptUnknown   AttemptState = "unknown"
)

// validAttemptTransitions enforces the D-9 state machine.
var validAttemptTransitions = map[AttemptState]map[AttemptState]bool{
	AttemptPrepared:  {AttemptClaimed: true},
	AttemptClaimed:   {AttemptInvoking: true, AttemptFailed: true},
	AttemptInvoking:  {AttemptSucceeded: true, AttemptFailed: true, AttemptUnknown: true},
	AttemptUnknown:   {},
	AttemptSucceeded: {},
	AttemptFailed:    {},
}

// AttemptTransition reports whether from -> to is legal under D-9.
func AttemptTransition(from, to AttemptState) bool {
	return validAttemptTransitions[from][to]
}

// Attempt records one dispatch attempt of a run.
type Attempt struct {
	AttemptID      string
	RunID          string
	Namespace      string
	State          AttemptState
	FencingToken   uint64
	AttemptNumber  int
	IdempotencyKey string
	WorkerID       string
	StartedAt      time.Time
	FinishedAt     *time.Time
	Error          string
}

// FencingToken returns the next monotonically increasing token for a run.
// Every reclaim of the same run must produce a strictly greater token.
func FencingToken(current uint64) uint64 { return current + 1 }

// AuditRequestedRecord is the dependency-free, versioned, typed allowlist
// of audit intent (RC-08 definition, reused by RC-10). No bodies, no
// arguments, no free-form extension maps.
type AuditRequestedRecord struct {
	EventID       string
	ExecutionID   string
	AttemptID     string
	RunID         string
	Namespace     string
	Subject       string
	ToolName      string
	RequirementID string
	Phase         string

	RegistryRevision            string
	DescriptorPolicyRevision    string
	AuthorizationPolicyRevision string

	ArgumentsHash      string
	IdempotencyKeyHash string
	Timestamp          time.Time
}

// OutboxEvent is one at-least-once delivery unit with a stable ID and an
// aggregate-local sequence; consumers must deduplicate by EventID.
type OutboxEvent struct {
	EventID     string
	Namespace   string
	AggregateID string
	Sequence    uint64
	Type        string
	Payload     json.RawMessage
	ClaimedBy   string
	CreatedAt   time.Time
}

// ExecutionJournalStore is the transactional contract: attempt, requested
// audit, and outbox writes commit together; outcomes append without
// overwriting requested.
type ExecutionJournalStore interface {
	// BeginAttemptAndAppendRequested writes the attempt, the minimal
	// AuditRequestedRecord, and the outbox event in one transaction. Any
	// failure means the managed side effect must not proceed.
	BeginAttemptAndAppendRequested(ctx context.Context, scope execution.NamespaceScope, attempt *Attempt, requested *AuditRequestedRecord, event *OutboxEvent) error

	// AppendOutcome records a terminal/unknown attempt outcome plus an
	// outbox event, in one transaction.
	AppendOutcome(ctx context.Context, scope execution.NamespaceScope, attemptID string, state AttemptState, event *OutboxEvent) error

	// ClaimOutbox claims up to limit events for a publisher worker.
	ClaimOutbox(ctx context.Context, scope execution.NamespaceScope, workerID string, limit int) ([]OutboxEvent, error)

	// AckOutbox marks an event delivered; the consumer deduplicates by
	// EventID (at-least-once).
	AckOutbox(ctx context.Context, scope execution.NamespaceScope, eventID string) error

	// ReconcileUnknown marks invoking attempts without an outcome older
	// than olderThan as unknown/reconciliation_required.
	ReconcileUnknown(ctx context.Context, scope execution.NamespaceScope, olderThan time.Duration) (int, error)
}

// Sentinel errors.
var (
	ErrInvalidAttemptTransition = errors.New("run: invalid attempt transition")
	ErrAttemptNotFound          = errors.New("run: attempt not found")
	ErrStaleFencingToken        = errors.New("run: stale fencing token")
)

// ValidateAttempt applies the D-9 state machine.
func ValidateAttempt(from, to AttemptState) error {
	if !AttemptTransition(from, to) {
		return ErrInvalidAttemptTransition
	}
	return nil
}

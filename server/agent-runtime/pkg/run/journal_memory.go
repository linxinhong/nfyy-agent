package run

import (
	"context"
	"sync"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
)

// MemoryExecutionJournal is the single-process reference implementation of
// ExecutionJournalStore. It does not provide cross-worker atomicity (that
// is PostgreSQL's job in managed multi-worker mode) but exercises the full
// contract including transactional begin/append and outbox claim/ack.
type MemoryExecutionJournal struct {
	mu       sync.Mutex
	attempts map[string]*Attempt
	outbox   []OutboxEvent
	acked    map[string]bool
	clock    func() time.Time
}

// NewMemoryExecutionJournal creates an in-memory journal.
func NewMemoryExecutionJournal() *MemoryExecutionJournal {
	return &MemoryExecutionJournal{
		attempts: make(map[string]*Attempt),
		acked:    make(map[string]bool),
		clock:    func() time.Time { return time.Now().UTC() },
	}
}

// SetClock injects a clock for reconcile tests.
func (j *MemoryExecutionJournal) SetClock(clock func() time.Time) *MemoryExecutionJournal {
	if clock != nil {
		j.clock = clock
	}
	return j
}

func (j *MemoryExecutionJournal) BeginAttemptAndAppendRequested(ctx context.Context, scope execution.NamespaceScope, attempt *Attempt, requested *AuditRequestedRecord, event *OutboxEvent) error {
	if attempt == nil || attempt.AttemptID == "" {
		return ErrAttemptNotFound
	}
	now := j.clock()
	attempt.StartedAt = now
	attempt.Namespace = scope.Namespace
	if event != nil {
		event.Namespace = scope.Namespace
		if event.CreatedAt.IsZero() {
			event.CreatedAt = now
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.attempts[attempt.AttemptID] = cloneAttempt(attempt)
	if event != nil {
		j.outbox = append(j.outbox, *event)
	}
	return nil
}

func (j *MemoryExecutionJournal) AppendOutcome(ctx context.Context, scope execution.NamespaceScope, attemptID string, state AttemptState, event *OutboxEvent) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	attempt, ok := j.attempts[attemptID]
	if !ok {
		return ErrAttemptNotFound
	}
	if err := ValidateAttempt(attempt.State, state); err != nil {
		return err
	}
	attempt.State = state
	now := j.clock()
	attempt.FinishedAt = &now
	if event != nil {
		event.Namespace = scope.Namespace
		if event.CreatedAt.IsZero() {
			event.CreatedAt = now
		}
		j.outbox = append(j.outbox, *event)
	}
	return nil
}

func (j *MemoryExecutionJournal) ClaimOutbox(ctx context.Context, scope execution.NamespaceScope, workerID string, limit int) ([]OutboxEvent, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var claimed []OutboxEvent
	for i := range j.outbox {
		if len(claimed) >= limit {
			break
		}
		event := j.outbox[i]
		if event.Namespace != scope.Namespace {
			continue
		}
		// Only acked events are delivered; unacked events stay claimable
		// (at-least-once, consumers deduplicate by EventID).
		if j.acked[event.EventID] {
			continue
		}
		event.ClaimedBy = workerID
		j.outbox[i] = event
		claimed = append(claimed, event)
	}
	return claimed, nil
}

func (j *MemoryExecutionJournal) AckOutbox(ctx context.Context, scope execution.NamespaceScope, eventID string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.acked[eventID] = true
	return nil
}

func (j *MemoryExecutionJournal) ReconcileUnknown(ctx context.Context, scope execution.NamespaceScope, olderThan time.Duration) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.clock()
	count := 0
	for id, attempt := range j.attempts {
		if attempt.Namespace != scope.Namespace {
			continue
		}
		if attempt.State != AttemptInvoking {
			continue
		}
		if attempt.StartedAt.Add(olderThan).After(now) {
			continue
		}
		attempt.State = AttemptUnknown
		j.attempts[id] = attempt
		count++
	}
	return count, nil
}

func cloneAttempt(a *Attempt) *Attempt {
	copy := *a
	return &copy
}

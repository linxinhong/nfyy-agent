// Package run coordinates background execution and cancellation of runs.
package run

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/observability"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// ErrNotCancellable reports that a run has already reached a terminal state.
var ErrNotCancellable = errors.New("run is not cancellable")

// ErrStoreRequired reports that durable storage is required for background runs.
var ErrStoreRequired = errors.New("background runs require durable storage")

// ErrStartTimeout reports that a run was not durably registered in time.
var ErrStartTimeout = errors.New("run did not start before the registration timeout")

// ErrExecutorRequired reports a missing background executor.
var ErrExecutorRequired = errors.New("background run executor is required")

// Executor executes one request using its preallocated RunID.
type Executor func(context.Context, session.RunRequest) (*session.RunOutput, error)

type entry struct {
	output *session.RunOutput
	cancel context.CancelFunc
	owner  *ManagedRunOwner
	err    error
}

// Manager owns process-local cancellation functions while durable run state
// remains in the configured store. When a Dispatcher is set, background runs
// are additionally claimed with a cross-instance lease and released on
// completion, so only one worker executes a given run.
type Manager struct {
	store     db.RunStore
	timeout   time.Duration
	admission *AdmissionController
	// admissionCancel only interrupts this Manager's queued StartContext
	// callers during Shutdown. It never closes a shared controller.
	admissionCtx  context.Context
	admissionDone context.CancelFunc

	// dispatcher/workerID/lease/namespace wire cross-instance ownership.
	// A nil dispatcher keeps the legacy process-local behavior.
	dispatcher Dispatcher
	workerID   string
	lease      time.Duration
	namespace  string
	managed    *ManagedConfig

	mu      sync.RWMutex
	entries map[string]*entry
	closed  bool
}

// New creates a background run manager.
func New(store db.RunStore, timeout time.Duration) *Manager {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	admissionCtx, admissionDone := context.WithCancel(context.Background())
	return &Manager{store: store, timeout: timeout, admissionCtx: admissionCtx, admissionDone: admissionDone, entries: make(map[string]*entry)}
}

// SetAdmission installs the optional process-local controller used by this
// Manager. The same controller may also be passed to Agents; a context-held
// permit then prevents a background Manager from counting an Agent twice.
func (m *Manager) SetAdmission(controller *AdmissionController) *Manager {
	m.admission = controller
	return m
}

// NewManaged creates an explicitly strict Manager. Unlike New and
// SetDispatcher, every Start on this Manager acquires a claim from the same
// ManagedStore that atomically fences Run commits. Missing capability or an
// invalid worker/scope/lease fails before execution can start.
func NewManaged(store db.RunStore, timeout time.Duration, cfg ManagedConfig) (*Manager, error) {
	managed, err := RequireManagedStore(store)
	if err != nil {
		return nil, err
	}
	cfg.Store = managed
	if cfg.Worker.WorkerID == "" || cfg.Lease <= 0 ||
		execution.ValidateNamespace(cfg.Scope.Namespace) != nil ||
		execution.ValidateNamespaceLength(cfg.Scope.Namespace) != nil {
		return nil, ErrInvalidClaim
	}
	m := New(managed, timeout)
	m.managed = &cfg
	return m, nil
}

// SetDispatcher wires cross-instance run ownership. A nil dispatcher (or an
// empty workerID) keeps the process-local behavior.
func (m *Manager) SetDispatcher(dispatcher Dispatcher, workerID, namespace string, lease time.Duration) *Manager {
	if dispatcher == nil || workerID == "" || lease <= 0 {
		return m
	}
	m.dispatcher = dispatcher
	m.workerID = workerID
	m.namespace = namespace
	m.lease = lease
	return m
}

// Shutdown stops accepting new runs and waits (bounded by ctx) for active
// runs to finish, releasing their leases as they complete. It is the SIGTERM
// drain path.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	m.closed = true
	if m.admissionDone != nil {
		m.admissionDone()
	}
	m.mu.Unlock()
	for {
		m.mu.RLock()
		n := len(m.entries)
		m.mu.RUnlock()
		if n == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Start allocates a direct run ID and executes the request independently of
// the initiating HTTP request context.
func (m *Manager) Start(request session.RunRequest, executor Executor) (*session.RunOutput, error) {
	return m.StartContext(context.Background(), request, executor)
}

// StartManaged is the explicit strict-start spelling. It rejects legacy
// Managers instead of silently falling back to unfenced execution.
func (m *Manager) StartManaged(request session.RunRequest, executor Executor) (*session.RunOutput, error) {
	return m.StartManagedContext(context.Background(), request, executor)
}

// StartManagedContext starts only when this Manager was built by NewManaged.
func (m *Manager) StartManagedContext(parent context.Context, request session.RunRequest, executor Executor) (*session.RunOutput, error) {
	if m.managed == nil {
		return nil, ErrManagedStoreRequired
	}
	return m.StartContext(parent, request, executor)
}

// StartContext starts a background run while retaining caller context values.
// Cancellation is intentionally detached; Manager timeout and Cancel govern execution.
func (m *Manager) StartContext(parent context.Context, request session.RunRequest, executor Executor) (*session.RunOutput, error) {
	if m.store == nil {
		return nil, ErrStoreRequired
	}
	if executor == nil {
		return nil, ErrExecutorRequired
	}
	m.mu.RLock()
	closed := m.closed
	m.mu.RUnlock()
	if closed {
		return nil, errors.New("run: manager is shut down")
	}
	if request.RunID == "" {
		request.RunID = uuid.NewString()
	}
	if request.Metadata == nil {
		request.Metadata, _ = session.RunMetadataFromContext(parent)
	}
	metadata, err := session.CloneRunMetadata(request.Metadata)
	if err != nil {
		return nil, err
	}
	request.Metadata = metadata
	baseCtx := context.WithoutCancel(parent)
	baseCtx = session.ContextWithRunMetadata(baseCtx, metadata)
	admissionParent := parent
	if m.managed != nil {
		resolved, resolveErr := db.NamespaceFor(parent, m.managed.Scope.Namespace)
		if resolveErr != nil || resolved != m.managed.Scope.Namespace {
			if resolveErr != nil {
				return nil, resolveErr
			}
			return nil, db.ErrNotFound
		}
		baseCtx = execution.ContextWithNamespace(baseCtx, resolved)
		admissionParent = execution.ContextWithNamespace(admissionParent, resolved)
	}
	// Queue admission observes caller cancellation while waiting, then the
	// resulting permit is carried into the detached execution context. Shutdown
	// cancels only Manager-owned waiters; it does not close a shared controller.
	var admissionPermit *AdmissionPermit
	if m.admission != nil {
		waitCtx, waitCancel := context.WithCancel(admissionParent)
		stopShutdown := context.AfterFunc(m.admissionCtx, waitCancel)
		// A background Start detaches from its caller, so it is a new admission
		// root even when the caller itself holds a permit. The returned context
		// then lets the executor's in-process Agent borrow this new permit.
		admittedCtx, permit, admissionErr := m.admission.AcquireRoot(waitCtx)
		stopShutdown()
		waitCancel()
		if admissionErr != nil {
			return nil, admissionErr
		}
		// Retain the permit as a value while preserving the historical detached
		// background execution cancellation policy.
		baseCtx = context.WithoutCancel(admittedCtx)
		baseCtx = session.ContextWithRunMetadata(baseCtx, metadata)
		admissionPermit = permit
	}
	permitOwned := admissionPermit != nil
	defer func() {
		if permitOwned {
			admissionPermit.Release()
		}
	}()
	registrationTimeout := 5 * time.Second
	if m.timeout < registrationTimeout {
		registrationTimeout = m.timeout
	}
	registrationCtx, registrationCancel := context.WithTimeout(baseCtx, registrationTimeout)
	defer registrationCancel()
	for {
		existing, err := m.getRun(registrationCtx, db.RunQuery{RunID: request.RunID})
		if err == nil {
			if existing != nil {
				return nil, db.ErrConflict
			}
			break
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-timer.C:
		case <-registrationCtx.Done():
			timer.Stop()
			return nil, errors.Join(ErrStartTimeout, err)
		}
	}
	now := time.Now().UTC()
	pending := &session.RunOutput{
		RunID:     request.RunID,
		UserID:    request.UserID,
		Input:     request.Input,
		Metadata:  metadata,
		Status:    session.RunStatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if m.managed != nil {
		pending.Namespace = m.managed.Scope.Namespace
	}
	ctx, cancel := context.WithTimeout(baseCtx, m.timeout)

	m.mu.Lock()
	if _, exists := m.entries[request.RunID]; exists {
		m.mu.Unlock()
		cancel()
		return nil, db.ErrConflict
	}
	m.entries[request.RunID] = &entry{output: pending, cancel: cancel}
	m.mu.Unlock()

	go func() {
		defer admissionPermit.Release()
		// Cross-instance ownership: claim before executing. Strict managed
		// execution also installs the claim-bound Run committer in execCtx.
		execCtx := ctx
		var owner *ManagedRunOwner
		var claim *Claim
		var stopHeartbeat chan struct{}
		var heartbeatDone chan struct{}
		scope := execution.NamespaceScope{Namespace: m.namespace}
		if m.managed != nil {
			managedCtx, lifecycleSpan := observability.Tracer("xingding.run").Start(ctx, "managed.lifecycle")
			lifecycleSpan.SetAttributes(observability.SafeAttrs(
				attribute.String(observability.FieldRunID, request.RunID),
				attribute.String("namespace", m.managed.Scope.Namespace),
				attribute.String("worker_id", m.managed.Worker.WorkerID),
			)...)
			configured := *m.managed
			var claimErr error
			owner, claimErr = AcquireManagedRunOwner(managedCtx, request.RunID, configured)
			if claimErr != nil {
				lifecycleSpan.RecordError(claimErr)
				lifecycleSpan.End()
				cancel()
				output := cloneOutput(pending)
				output.Status = session.RunStatusCancelled
				output.Error = claimErr.Error()
				output.UpdatedAt = time.Now().UTC()
				m.finishWithoutReconcile(request.RunID, output, claimErr, false)
				return
			}
			execCtx = owner.Context()
			m.mu.Lock()
			if current := m.entries[request.RunID]; current != nil {
				current.owner = owner
			}
			m.mu.Unlock()
		} else if m.dispatcher != nil {
			_, claimSpan := observability.Tracer("xingding.run").Start(ctx, "dispatcher.claim")
			claimSpan.SetAttributes(observability.SafeAttrs(
				attribute.String(observability.FieldRunID, request.RunID),
				attribute.String("namespace", m.namespace),
				attribute.String("worker_id", m.workerID),
			)...)
			c, claimErr := m.dispatcher.Claim(ctx, scope, request.RunID, WorkerIdentity{WorkerID: m.workerID}, m.lease)
			claimSpan.End()
			if claimErr != nil {
				cancel()
				output := cloneOutput(pending)
				output.Status = session.RunStatusCancelled
				output.Error = "run lease is held by another worker"
				output.UpdatedAt = time.Now().UTC()
				m.finish(request.RunID, output, claimErr)
				return
			}
			claim = c
			stopHeartbeat = make(chan struct{})
			heartbeatDone = make(chan struct{})
			go func() {
				defer close(heartbeatDone)
				interval := m.lease / 2
				if interval <= 0 {
					interval = time.Second
				}
				ticker := time.NewTicker(interval)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						hbCtx, hbCancel := context.WithTimeout(ctx, interval)
						hbErr := m.dispatcher.Heartbeat(hbCtx, scope, claim, m.lease)
						hbCancel()
						if hbErr != nil {
							cancel()
							return
						}
					case <-ctx.Done():
						return
					case <-stopHeartbeat:
						return
					}
				}
			}()
		}

		output, err := executor(execCtx, request)
		if output == nil {
			output = cloneOutput(pending)
		}
		if err != nil && !isTerminal(output.Status) {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				output.Status = session.RunStatusCancelled
			} else {
				output.Status = session.RunStatusError
			}
		}
		if err != nil && output.Error == "" {
			output.Error = err.Error()
		}
		output.UpdatedAt = time.Now().UTC()

		if owner != nil {
			final, durable, managedErr := m.reconcileManaged(owner, output)
			err = errors.Join(err, managedErr, owner.OwnershipError())
			cancel()
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), minDuration(5*time.Second, m.timeout))
			if durable {
				err = errors.Join(err, owner.Release(cleanupCtx))
			} else {
				err = errors.Join(err, owner.StopHeartbeat(cleanupCtx))
			}
			cleanupCancel()
			if err != nil {
				lifecycleSpan := oteltrace.SpanFromContext(owner.Context())
				lifecycleSpan.RecordError(err)
				lifecycleSpan.End()
			} else {
				oteltrace.SpanFromContext(owner.Context()).End()
			}
			m.finishWithoutReconcile(request.RunID, final, err, durable)
			return
		}

		// Legacy SetDispatcher remains a compatibility path. It now observes
		// heartbeat failure and joins its worker, but its writes are not fenced.
		m.finish(request.RunID, output, err)
		cancel()
		if claim != nil {
			close(stopHeartbeat)
			<-heartbeatDone
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), minDuration(5*time.Second, m.timeout))
			_ = m.dispatcher.Release(releaseCtx, scope, claim)
			releaseCancel()
		}
	}()
	permitOwned = false

	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	var lastReadErr error
	for {
		output, err := m.getRun(registrationCtx, db.RunQuery{RunID: request.RunID, UserID: request.UserID})
		if output != nil {
			return output, nil
		}
		if err != nil {
			lastReadErr = err
		}

		m.mu.RLock()
		current := m.entries[request.RunID]
		finished := current != nil && current.cancel == nil
		var finishedOutput *session.RunOutput
		var finishedErr error
		if finished {
			finishedOutput = cloneOutput(current.output)
			finishedErr = current.err
		}
		m.mu.RUnlock()
		if finished {
			m.mu.Lock()
			delete(m.entries, request.RunID)
			m.mu.Unlock()
			return nil, errors.Join(errors.New("run failed before durable registration: "+finishedOutput.Error), finishedErr)
		}

		select {
		case <-ticker.C:
		case <-registrationCtx.Done():
			cancel()
			return nil, errors.Join(ErrStartTimeout, lastReadErr)
		}
	}
}

// Get returns durable state when available and otherwise the process-local
// pending state created before the executor persisted the run.
func (m *Manager) Get(ctx context.Context, query db.RunQuery) (*session.RunOutput, error) {
	if m.store != nil {
		output, err := m.getRun(ctx, query)
		if err != nil || output != nil {
			return output, err
		}
	}
	m.mu.RLock()
	e := m.entries[query.RunID]
	if e == nil || (query.UserID != "" && e.output.UserID != query.UserID) {
		m.mu.RUnlock()
		return nil, nil
	}
	output := cloneOutput(e.output)
	m.mu.RUnlock()
	return output, nil
}

// Cancel requests cancellation. Repeating cancellation of an active or
// already-cancelled run is safe.
func (m *Manager) Cancel(ctx context.Context, query db.RunQuery) (*session.RunOutput, error) {
	if m.managed != nil {
		return m.cancelManaged(ctx, query)
	}
	if m.store == nil {
		return nil, db.ErrNotFound
	}
	persistTimeout := 5 * time.Second
	if m.timeout < persistTimeout {
		persistTimeout = m.timeout
	}
	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer persistCancel()
	output, err := m.store.GetRun(persistCtx, query)
	if err != nil {
		return nil, err
	}
	if output == nil {
		return nil, db.ErrNotFound
	}
	if output.Status == session.RunStatusCancelled {
		return output, nil
	}
	if output.Status != session.RunStatusRunning && output.Status != session.RunStatusPending && output.Status != session.RunStatusPaused {
		return output, ErrNotCancellable
	}

	cancelled := cloneOutput(output)
	cancelled.Status = session.RunStatusCancelled
	cancelled.Error = context.Canceled.Error()
	cancelled.UpdatedAt = time.Now().UTC()
	if err := m.store.UpdateRun(persistCtx, cancelled, output.Status); err != nil {
		if !errors.Is(err, db.ErrConflict) {
			return nil, err
		}
		winner, getErr := m.store.GetRun(persistCtx, query)
		if getErr != nil {
			return nil, getErr
		}
		if winner != nil && winner.Status == session.RunStatusCancelled {
			return winner, nil
		}
		return winner, ErrNotCancellable
	}

	m.mu.Lock()
	if e := m.entries[query.RunID]; e != nil && (query.UserID == "" || e.output.UserID == query.UserID) {
		if e.cancel != nil {
			e.cancel()
		}
		e.output = cloneOutput(cancelled)
	}
	m.mu.Unlock()
	return cancelled, nil
}

func (m *Manager) cancelManaged(ctx context.Context, query db.RunQuery) (*session.RunOutput, error) {
	if query.Namespace != "" && query.Namespace != m.managed.Scope.Namespace {
		return nil, ErrManagedLocalClaimRequired
	}
	m.mu.RLock()
	current := m.entries[query.RunID]
	var owner *ManagedRunOwner
	if current != nil {
		owner = current.owner
	}
	m.mu.RUnlock()
	if owner == nil {
		return nil, ErrManagedLocalClaimRequired
	}
	owner.Cancel(context.Canceled)
	snapshot, err := owner.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	if query.UserID != "" && snapshot.Run.UserID != query.UserID {
		return nil, db.ErrNotFound
	}
	if snapshot.Run.Status == session.RunStatusCancelled {
		return snapshot.Run, nil
	}
	if snapshot.Run.Status != session.RunStatusRunning && snapshot.Run.Status != session.RunStatusPending && snapshot.Run.Status != session.RunStatusPaused {
		return snapshot.Run, ErrNotCancellable
	}
	cancelled := cloneOutput(snapshot.Run)
	cancelled.Status = session.RunStatusCancelled
	cancelled.Error = context.Canceled.Error()
	cancelled.UpdatedAt = time.Now().UTC()
	if _, err := owner.Update(ctx, cancelled, snapshot.Run.Status); err != nil {
		if errors.Is(err, db.ErrConflict) {
			winner, getErr := owner.Snapshot(ctx)
			if getErr != nil {
				return nil, getErr
			}
			if winner.Run.Status == session.RunStatusCancelled {
				return winner.Run, nil
			}
			return winner.Run, ErrNotCancellable
		}
		return nil, err
	}
	m.mu.Lock()
	if active := m.entries[query.RunID]; active != nil {
		active.output = cloneOutput(cancelled)
	}
	m.mu.Unlock()
	return cancelled, nil
}

func (m *Manager) getRun(ctx context.Context, query db.RunQuery) (*session.RunOutput, error) {
	if m.managed == nil {
		return m.store.GetRun(ctx, query)
	}
	if query.Namespace != "" && query.Namespace != m.managed.Scope.Namespace {
		return nil, db.ErrNotFound
	}
	snapshot, err := m.managed.Store.GetManagedRun(ctx, m.managed.Scope, query.RunID)
	if errors.Is(err, db.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if snapshot == nil || snapshot.Run == nil || query.UserID != "" && snapshot.Run.UserID != query.UserID {
		return nil, nil
	}
	return snapshot.Run, nil
}

func cloneOutput(output *session.RunOutput) *session.RunOutput {
	if output == nil {
		return nil
	}
	clone := *output
	clone.Metadata, _ = session.CloneRunMetadata(output.Metadata)
	return &clone
}

func (m *Manager) finish(runID string, output *session.RunOutput, runErr error) {
	final, durable := m.reconcileTerminal(output)
	m.finishWithoutReconcile(runID, final, runErr, durable)
}

func (m *Manager) finishWithoutReconcile(runID string, final *session.RunOutput, runErr error, durable bool) {
	m.mu.Lock()
	if durable {
		delete(m.entries, runID)
	} else {
		m.entries[runID] = &entry{output: cloneOutput(final), err: runErr}
	}
	m.mu.Unlock()
	if !durable {
		time.AfterFunc(5*time.Minute, func() {
			m.mu.Lock()
			if current := m.entries[runID]; current != nil && current.cancel == nil {
				delete(m.entries, runID)
			}
			m.mu.Unlock()
		})
	}
}

func (m *Manager) reconcileManaged(owner *ManagedRunOwner, output *session.RunOutput) (*session.RunOutput, bool, error) {
	if output.Status != session.RunStatusPaused && !isTerminal(output.Status) {
		output.Status = session.RunStatusError
		if output.Error == "" {
			output.Error = ErrManagedExecutorIncomplete.Error()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), minDuration(5*time.Second, m.timeout))
	defer cancel()
	snapshot, err := owner.Snapshot(ctx)
	if errors.Is(err, db.ErrNotFound) {
		return output, false, ErrManagedCommitRequired
	}
	if err != nil {
		return output, false, err
	}
	if snapshot.Run.Status == output.Status {
		return snapshot.Run, true, nil
	}
	if isTerminal(snapshot.Run.Status) || !snapshot.Run.Status.CanTransitionTo(output.Status) {
		return snapshot.Run, true, db.ErrConflict
	}
	if _, err := owner.Update(ctx, output, snapshot.Run.Status); err != nil {
		return output, false, err
	}
	return output, true, nil
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func (m *Manager) reconcileTerminal(output *session.RunOutput) (*session.RunOutput, bool) {
	if output == nil || m.store == nil || !isTerminal(output.Status) {
		return output, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	query := db.RunQuery{RunID: output.RunID, UserID: output.UserID}
	for attempt := 0; attempt < 3; attempt++ {
		existing, err := m.store.GetRun(ctx, query)
		if err != nil {
			if attempt < 2 {
				time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
				continue
			}
			return output, false
		}
		if existing == nil {
			return output, false
		}
		if isTerminal(existing.Status) {
			return existing, true
		}
		if !existing.Status.CanTransitionTo(output.Status) {
			return output, false
		}
		candidate := cloneOutput(output)
		candidate.UpdatedAt = time.Now().UTC()
		err = m.store.UpdateRun(ctx, candidate, existing.Status)
		if err == nil {
			return candidate, true
		}
		if errors.Is(err, db.ErrConflict) {
			continue
		}
		if attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
			continue
		}
	}
	winner, err := m.store.GetRun(ctx, query)
	if err == nil && winner != nil && isTerminal(winner.Status) {
		return winner, true
	}
	return output, false
}

func isTerminal(status session.RunStatus) bool {
	return status == session.RunStatusCompleted || status == session.RunStatusError || status == session.RunStatusCancelled
}

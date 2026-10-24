package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

var (
	// ErrSessionBusy reports that another run held the session longer than
	// AgentConfig.SessionLockTimeout.
	ErrSessionBusy = errors.New("agent: session is busy with another run")
	// ErrRunCancelledExternally reports that the durable run left the
	// running state while this process was executing it, typically because
	// run.Manager.Cancel was called on another replica.
	ErrRunCancelledExternally = errors.New("agent: run was cancelled by another process")
	// ErrSessionLockLost reports that the run's session lock could not be
	// renewed and may have passed to another run, so the run was stopped
	// rather than let two runs write the same session.
	ErrSessionLockLost = errors.New("agent: session lock lost")
)

const defaultRunLeaseTimeout = 2 * time.Minute

// SessionLocker serializes work on one session. LockSession blocks until
// the session is free or ctx is done, and returns a function that releases
// it. Keys are printable text. Implementations must tolerate unlock being
// called more than once. postgres.DB.SessionLocker shares locks across
// replicas.
type SessionLocker interface {
	LockSession(ctx context.Context, key string) (unlock func(), err error)
}

// SessionLockWatcher is implemented by SessionLockers whose locks can be
// lost while held, such as leases that failed to renew. lost is closed when
// the lock may have passed to another holder; the Agent then cancels the
// run with ErrSessionLockLost.
type SessionLockWatcher interface {
	LockSessionWatch(ctx context.Context, key string) (unlock func(), lost <-chan struct{}, err error)
}

// NewSessionLocker returns an in-process SessionLocker. Share one instance
// across Agents that may serve the same sessions within a process.
func NewSessionLocker() SessionLocker {
	return &localSessionLocker{locks: make(map[string]*sessionLockEntry)}
}

type localSessionLocker struct {
	mu    sync.Mutex
	locks map[string]*sessionLockEntry
}

type sessionLockEntry struct {
	held chan struct{}
	refs int
}

func (l *localSessionLocker) LockSession(ctx context.Context, key string) (func(), error) {
	l.mu.Lock()
	entry := l.locks[key]
	if entry == nil {
		entry = &sessionLockEntry{held: make(chan struct{}, 1)}
		l.locks[key] = entry
	}
	entry.refs++
	l.mu.Unlock()

	select {
	case entry.held <- struct{}{}:
	case <-ctx.Done():
		l.release(key, entry, false)
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() { once.Do(func() { l.release(key, entry, true) }) }, nil
}

func (l *localSessionLocker) release(key string, entry *sessionLockEntry, held bool) {
	if held {
		<-entry.held
	}
	l.mu.Lock()
	entry.refs--
	if entry.refs == 0 {
		delete(l.locks, key)
	}
	l.mu.Unlock()
}

type heldSessionsKey struct{}

// lockSession acquires the session lock unless this call chain already
// holds it (an agent tool re-entering the same session must not deadlock).
func (a *Agent) lockSession(ctx context.Context, sessionID string) (context.Context, func(), error) {
	// Without a store, runs share no session state (each starts from an
	// empty in-memory session), so there is nothing to serialize.
	if sessionID == "" || a.sessionLocker == nil || a.cfg.DB == nil {
		return ctx, func() {}, nil
	}
	namespace, _ := execution.NamespaceFromContext(ctx)
	// Length-prefixed so keys are unambiguous yet plain text, which
	// distributed lockers store in databases or Redis.
	key := fmt.Sprintf("session:%d:%s:%s", len(namespace), namespace, sessionID)
	held, _ := ctx.Value(heldSessionsKey{}).(map[string]bool)
	if held[key] {
		return ctx, func() {}, nil
	}
	lockCtx := ctx
	if a.cfg.SessionLockTimeout > 0 {
		var cancel context.CancelFunc
		lockCtx, cancel = context.WithTimeout(ctx, a.cfg.SessionLockTimeout)
		defer cancel()
	}
	started := time.Now()
	var unlock func()
	var lost <-chan struct{}
	var err error
	if watcher, ok := a.sessionLocker.(SessionLockWatcher); ok {
		unlock, lost, err = watcher.LockSessionWatch(lockCtx, key)
	} else {
		unlock, err = a.sessionLocker.LockSession(lockCtx, key)
	}
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			recordSessionLockWait(ctx, started, "busy")
			return ctx, nil, ErrSessionBusy
		}
		recordSessionLockWait(ctx, started, "failed")
		return ctx, nil, fmt.Errorf("lock session: %w", err)
	}
	recordSessionLockWait(ctx, started, "acquired")
	next := make(map[string]bool, len(held)+1)
	for k := range held {
		next[k] = true
	}
	next[key] = true
	ctx = context.WithValue(ctx, heldSessionsKey{}, next)
	if lost == nil {
		return ctx, unlock, nil
	}
	ctx, cancel := context.WithCancelCause(ctx)
	stop := make(chan struct{})
	go func() {
		select {
		case <-lost:
			cancelledExternally.Add(context.WithoutCancel(ctx), 1, attrs(attribute.String("detection", "session_lock_lost")))
			cancel(ErrSessionLockLost)
		case <-stop:
		case <-ctx.Done():
		}
	}()
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			close(stop)
			unlock()
			cancel(nil)
		})
	}, nil
}

// leaseStore returns the store's lease capability when this run is
// persisted through AgentConfig.DB rather than a managed owner.
func (a *Agent) leaseStore(ctx context.Context, output *session.RunOutput) (db.RunLeaseStore, time.Duration) {
	if a.cfg.DB == nil || a.cfg.RunLeaseTimeout < 0 {
		return nil, 0
	}
	if _, managed, _ := managedRootOwner(ctx, output); managed {
		return nil, 0
	}
	store, ok := a.cfg.DB.(db.RunLeaseStore)
	if !ok {
		return nil, 0
	}
	timeout := a.cfg.RunLeaseTimeout
	if timeout == 0 {
		timeout = defaultRunLeaseTimeout
	}
	return store, timeout
}

// startHeartbeat refreshes the run lease every quarter of timeout until
// stop is called. When the store reports the run is no longer running, the
// run context is cancelled with ErrRunCancelledExternally.
func startHeartbeat(ctx context.Context, store db.RunLeaseStore, timeout time.Duration, output *session.RunOutput, cancel context.CancelCauseFunc) (stop func()) {
	query := db.RunQuery{RunID: output.RunID, UserID: output.UserID, Namespace: output.Namespace}
	return runmanager.StartRunHeartbeat(ctx, store, query, timeout/4, func() {
		cancelledExternally.Add(context.WithoutCancel(ctx), 1, attrs(attribute.String("detection", "heartbeat")))
		cancel(ErrRunCancelledExternally)
	})
}

// guardRun attaches lease heartbeats to a run that is already durable and
// running. The returned context is cancelled with ErrRunCancelledExternally
// when another process cancels the run.
func (a *Agent) guardRun(ctx context.Context, st *runState) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	st.cancel = cancel
	stopHeartbeat := func() {}
	if store, timeout := a.leaseStore(ctx, st.output); store != nil && st.output.LeaseTimeout > 0 {
		stopHeartbeat = startHeartbeat(ctx, store, timeout, st.output, cancel)
	}
	return ctx, func() {
		stopHeartbeat()
		cancel(nil)
	}
}

// checkpoint persists the in-progress transcript after a tool round. A
// conflict means the durable run is no longer running, so the local run is
// cancelled; other storage errors are tolerated because the final update
// still records the outcome.
func (a *Agent) checkpoint(ctx context.Context, st *runState) error {
	if a.cfg.DB == nil || a.cfg.DisableRoundCheckpoints {
		return nil
	}
	if _, managed, _ := managedRootOwner(ctx, st.output); managed {
		return nil
	}
	output := st.output
	output.Messages = st.runMessages
	output.UpdatedAt = time.Now().UTC()
	persistCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := a.cfg.DB.UpdateRun(persistCtx, output, session.RunStatusRunning)
	if errors.Is(err, db.ErrConflict) {
		cancelledExternally.Add(context.WithoutCancel(ctx), 1, attrs(attribute.String("detection", "checkpoint")))
		if st.cancel != nil {
			st.cancel(ErrRunCancelledExternally)
		}
		return ErrRunCancelledExternally
	}
	return nil
}

// ExpireStaleRuns fails runs of crashed processes in the Agent's store; see
// run.ExpireStaleRuns. Hosts usually run run.RunStaleRunReaper instead.
func (a *Agent) ExpireStaleRuns(ctx context.Context) ([]string, error) {
	return runmanager.ExpireStaleRuns(ctx, a.cfg.DB, 100)
}

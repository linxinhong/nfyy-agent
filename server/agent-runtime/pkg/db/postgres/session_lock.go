package postgres

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

// SessionLockConfig configures DB.SessionLocker.
type SessionLockConfig struct {
	// LeaseTTL is how long a lock survives without renewal; holders renew
	// every third of it. Default 30s. A replica that crashes holds its
	// sessions for at most this long.
	LeaseTTL time.Duration
	// PollInterval caps the wait between attempts on a busy session.
	// Default 500ms (attempts start at 20ms and back off).
	PollInterval time.Duration
}

// SessionLocker returns a lock shared by every replica using the database,
// satisfying agent.SessionLocker: pass it as AgentConfig.SessionLocker.
// Locks are leased rows rather than advisory locks, so a run holds no
// database connection while it executes.
func (d *DB) SessionLocker(cfg SessionLockConfig) *SessionLocker {
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 30 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 500 * time.Millisecond
	}
	return &SessionLocker{db: d, cfg: cfg}
}

// SessionLocker is a lease-based distributed session lock.
type SessionLocker struct {
	db  *DB
	cfg SessionLockConfig
}

// tryLock takes the lock when it is free or its lease expired.
func (l *SessionLocker) tryLock(ctx context.Context, key, holder string) (bool, error) {
	var got string
	err := l.db.db.QueryRowContext(ctx, `INSERT INTO agent_runtime_session_locks (lock_key, holder, expires_at)
		VALUES ($1, $2, now() + $3 * interval '1 second')
		ON CONFLICT (lock_key) DO UPDATE SET holder = EXCLUDED.holder, expires_at = EXCLUDED.expires_at
		WHERE agent_runtime_session_locks.expires_at < now()
		RETURNING holder`, key, holder, l.cfg.LeaseTTL.Seconds()).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && got == holder, err
}

// LockSession blocks until the session lock is held or ctx is done.
func (l *SessionLocker) LockSession(ctx context.Context, key string) (func(), error) {
	unlock, _, err := l.LockSessionWatch(ctx, key)
	return unlock, err
}

// LockSessionWatch is LockSession that also reports, by closing lost, when
// the lock may have passed to another holder: a renewal found the lease
// taken, or no renewal succeeded for a whole LeaseTTL (e.g. the database
// was unreachable). It satisfies agent.SessionLockWatcher.
func (l *SessionLocker) LockSessionWatch(ctx context.Context, key string) (func(), <-chan struct{}, error) {
	holder := uuid.NewString()
	delay := 20 * time.Millisecond
	for {
		attempted := time.Now()
		ok, err := l.tryLock(ctx, key, holder)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			return l.hold(key, holder, attempted)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, nil, ctx.Err()
		case <-timer.C:
		}
		if delay *= 2; delay > l.cfg.PollInterval {
			delay = l.cfg.PollInterval
		}
	}
}

// hold renews a lock taken at acquired until it is released or lost.
func (l *SessionLocker) hold(key, holder string, acquired time.Time) (func(), <-chan struct{}, error) {
	done := make(chan struct{})
	finished := make(chan struct{})
	lost := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(l.cfg.LeaseTTL / 3)
		defer ticker.Stop()
		// The lease runs from the start of the last successful renewal.
		renewed := acquired
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}
			attempted := time.Now()
			renewCtx, cancel := context.WithTimeout(context.Background(), l.cfg.LeaseTTL/3)
			result, err := l.db.db.ExecContext(renewCtx, `UPDATE agent_runtime_session_locks
				SET expires_at = now() + $3 * interval '1 second' WHERE lock_key = $1 AND holder = $2`,
				key, holder, l.cfg.LeaseTTL.Seconds())
			cancel()
			if err == nil {
				if n, rowsErr := result.RowsAffected(); rowsErr == nil && n == 0 {
					close(lost)
					return
				}
				renewed = attempted
				continue
			}
			if time.Since(renewed) >= l.cfg.LeaseTTL {
				close(lost)
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-finished
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// A failed delete only delays the next holder until expiry.
			_, _ = l.db.db.ExecContext(releaseCtx, `DELETE FROM agent_runtime_session_locks WHERE lock_key = $1 AND holder = $2`, key, holder)
		})
	}, lost, nil
}

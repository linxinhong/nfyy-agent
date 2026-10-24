package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	MaxPurgeBatch       = 100
	MaxPurgeRound       = 1000
	MaxPurgeRoundTime   = time.Second
	MinPurgeLease       = time.Microsecond
	MaxPurgeLease       = time.Hour
	PurgeLagObjective   = 24 * time.Hour
	LifecycleRetention  = 7 * 24 * time.Hour
	MaxPurgeFailureCode = 128
)

// PurgeLeaseDeadline canonicalizes lease timestamps to the coarsest precision
// shared by all supported stores (PostgreSQL microseconds).
func PurgeLeaseDeadline(now time.Time, leaseDuration time.Duration) (time.Time, error) {
	if leaseDuration < MinPurgeLease || leaseDuration > MaxPurgeLease {
		return time.Time{}, ErrInvalid
	}
	deadline := now.UTC().Add(leaseDuration).Truncate(time.Microsecond)
	if !deadline.After(now) {
		return time.Time{}, ErrInvalid
	}
	return deadline, nil
}

type PurgeJobKind string

const (
	PurgeJobForget        PurgeJobKind = "forget"
	PurgeJobRetract       PurgeJobKind = "retract"
	PurgeJobRetractSource PurgeJobKind = "retract_source"
	PurgeJobExpiry        PurgeJobKind = "expiry"
)

type PurgeJobStatus string

const (
	PurgeJobPending   PurgeJobStatus = "pending"
	PurgeJobLeased    PurgeJobStatus = "leased"
	PurgeJobFailed    PurgeJobStatus = "failed"
	PurgeJobCompleted PurgeJobStatus = "completed"
)

// PurgeJob contains lifecycle metadata only. Content must never be copied into
// this value, task rows, observations, or failure codes.
type PurgeJob struct {
	TaskID                string
	Kind                  PurgeJobKind
	Subject               SubjectScope
	Scope                 Scope
	SourceRef             string
	SourceEpoch           uint64
	MemoryID              string
	ExpectedRevision      uint64
	PurgeBeforeGeneration uint64
	SelectionCutoff       time.Time
	Status                PurgeJobStatus
	OwnerToken            string
	LeaseEpoch            uint64
	LeaseExpiresAt        time.Time
	CursorCollectionID    string
	CursorMemoryID        string
	CreatedAt             time.Time
	CompletedAt           time.Time
	LastFailureCode       string
}

type PurgeBatchResult struct {
	Job              PurgeJob
	ProcessedTargets int
	PurgedRecords    int
	ReleasedBytes    int
	Completed        bool
	StoreNow         time.Time
}

type PurgeLagSnapshot struct {
	StoreNow        time.Time
	PendingJobs     int
	FailedJobs      int
	OldestCreatedAt time.Time
	OldestLag       time.Duration
	ObjectiveMissed bool
}

type LifecycleReclaimResult struct {
	StoreNow         time.Time
	ExaminedControls int
	ReclaimedRecords int
	ReclaimedJobs    int
	ReleasedControls int
}

// LifecycleTaskID derives an opaque stable ID from metadata-only fields. The
// length-delimited JSON encoding avoids ambiguous delimiter-based scope keys.
func LifecycleTaskID(kind PurgeJobKind, subject SubjectScope, scope Scope, sourceRef, memoryID string, revision, generation uint64) string {
	payload, _ := json.Marshal(struct {
		Kind       PurgeJobKind
		Subject    SubjectScope
		Scope      Scope
		SourceRef  string
		MemoryID   string
		Revision   uint64
		Generation uint64
	}{kind, subject, scope, sourceRef, memoryID, revision, generation})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func LifecycleAuditID(job PurgeJob, processed int) string {
	payload, _ := json.Marshal(struct {
		TaskID             string
		LeaseEpoch         uint64
		CursorCollectionID string
		CursorMemoryID     string
		Processed          int
	}{job.TaskID, job.LeaseEpoch, job.CursorCollectionID, job.CursorMemoryID, processed})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// LifecycleStore is the shared, deliberately narrow durable-work seam. The
// store owns discovery, task identity, target selection, fencing, time and the
// transaction that clears content, releases quota and advances progress.
type LifecycleStore interface {
	DiscoverExpired(context.Context, int) (int, error)
	ClaimNextPurge(context.Context, string, time.Duration) (PurgeJob, error)
	RenewPurgeLease(context.Context, PurgeJob, time.Duration) (PurgeJob, error)
	PurgeBatch(context.Context, PurgeJob, int) (PurgeBatchResult, error)
	FailPurge(context.Context, PurgeJob, string) error
	PurgeLag(context.Context) (PurgeLagSnapshot, error)
	ReclaimLifecycle(context.Context, int) (LifecycleReclaimResult, error)
}

type LifecycleWorkerConfig struct {
	Store         LifecycleStore
	OwnerToken    string
	LeaseDuration time.Duration
	BatchSize     int
	RoundLimit    int
	RoundTimeout  time.Duration
}

type LifecycleRoundResult struct {
	Discovered       int
	ClaimedTaskID    string
	ExaminedControls int
	ProcessedTargets int
	PurgedRecords    int
	ReleasedBytes    int
	Completed        bool
	ReclaimedRecords int
	ReclaimedJobs    int
	ReleasedControls int
}

// LifecycleWorker runs at most one bounded round per instance. Scheduling the
// calls is a host concern; this type is intentionally not a broker or daemon.
type LifecycleWorker struct {
	store         LifecycleStore
	owner         string
	leaseDuration time.Duration
	batchSize     int
	roundLimit    int
	roundTimeout  time.Duration
	mu            sync.Mutex
	running       bool
}

func NewLifecycleWorker(cfg LifecycleWorkerConfig) (*LifecycleWorker, error) {
	if cfg.Store == nil || cfg.OwnerToken == "" || validateIdentifier(cfg.OwnerToken) != nil {
		return nil, fmt.Errorf("%w: lifecycle store and owner", ErrInvalid)
	}
	if cfg.LeaseDuration < MinPurgeLease || cfg.LeaseDuration > MaxPurgeLease {
		return nil, fmt.Errorf("%w: purge lease", ErrInvalid)
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = MaxPurgeBatch
	}
	if cfg.RoundLimit == 0 {
		cfg.RoundLimit = MaxPurgeRound
	}
	if cfg.RoundTimeout == 0 {
		cfg.RoundTimeout = MaxPurgeRoundTime
	}
	if cfg.BatchSize <= 0 || cfg.BatchSize > MaxPurgeBatch || cfg.RoundLimit <= 0 || cfg.RoundLimit > MaxPurgeRound || cfg.RoundTimeout <= 0 || cfg.RoundTimeout > MaxPurgeRoundTime {
		return nil, fmt.Errorf("%w: finite purge worker limits", ErrInvalid)
	}
	return &LifecycleWorker{store: cfg.Store, owner: cfg.OwnerToken, leaseDuration: cfg.LeaseDuration, batchSize: cfg.BatchSize, roundLimit: cfg.RoundLimit, roundTimeout: cfg.RoundTimeout}, nil
}

func (w *LifecycleWorker) RunOnce(ctx context.Context) (LifecycleRoundResult, error) {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return LifecycleRoundResult{}, ErrOverloaded
	}
	w.running = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()

	roundCtx, cancel := context.WithTimeout(ctx, w.roundTimeout)
	defer cancel()
	result := LifecycleRoundResult{}
	reclaimLimit := w.batchSize
	if reclaimLimit > w.roundLimit {
		reclaimLimit = w.roundLimit
	}
	reclaimed, err := w.store.ReclaimLifecycle(roundCtx, reclaimLimit)
	if err != nil {
		return result, err
	}
	result.ExaminedControls = reclaimed.ExaminedControls
	result.ReclaimedRecords = reclaimed.ReclaimedRecords
	result.ReclaimedJobs = reclaimed.ReclaimedJobs
	result.ReleasedControls = reclaimed.ReleasedControls
	if result.ExaminedControls >= w.roundLimit {
		return result, nil
	}
	discovered, err := w.store.DiscoverExpired(roundCtx, w.batchSize)
	if err != nil {
		return result, err
	}
	result.Discovered = discovered
	job, err := w.store.ClaimNextPurge(roundCtx, w.owner, w.leaseDuration)
	if errors.Is(err, ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.ClaimedTaskID = job.TaskID
	for result.ExaminedControls+result.ProcessedTargets < w.roundLimit {
		limit := w.batchSize
		if remaining := w.roundLimit - result.ExaminedControls - result.ProcessedTargets; remaining < limit {
			limit = remaining
		}
		batch, batchErr := w.store.PurgeBatch(roundCtx, job, limit)
		if batchErr != nil {
			if roundCtx.Err() != nil || errors.Is(batchErr, ErrConflict) {
				return result, batchErr
			}
			failCtx, failCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			_ = w.store.FailPurge(failCtx, job, "purge_batch_failed")
			failCancel()
			return result, batchErr
		}
		job = batch.Job
		result.ProcessedTargets += batch.ProcessedTargets
		result.PurgedRecords += batch.PurgedRecords
		result.ReleasedBytes += batch.ReleasedBytes
		if batch.Completed {
			result.Completed = true
			return result, nil
		}
		if batch.ProcessedTargets <= 0 || batch.ProcessedTargets > limit {
			return result, fmt.Errorf("%w: purge made no progress", ErrUnavailable)
		}
		if job.LeaseExpiresAt.Sub(batch.StoreNow) <= w.leaseDuration/2 {
			job, err = w.store.RenewPurgeLease(roundCtx, job, w.leaseDuration)
			if err != nil {
				return result, err
			}
		}
	}
	return result, nil
}

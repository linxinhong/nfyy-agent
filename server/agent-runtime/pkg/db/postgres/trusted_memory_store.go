package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

// MemoryStore is the durable PostgreSQL implementation of memory.AtomicStore.
// It intentionally does not implement or wrap the legacy db.MemoryStore.
type MemoryStore struct {
	db *sql.DB

	mu                     sync.RWMutex
	limits                 memory.Limits
	clock                  func() time.Time
	auditFailure           func(memory.AuditEvent) error
	afterCommitFailure     func() error
	lifecycleCommitFailure func() error
	// afterListControl is an in-package test seam. It runs after the combined
	// ListRecords control query and before the candidate query, making the
	// repeatable-read snapshot boundary testable without changing AtomicStore.
	afterListControl func()
	contentReads     atomic.Uint64
}

type memoryPolicyState struct {
	Generation uint64
	Limits     memory.Limits
}

type memorySubjectState struct {
	Generation uint64
	Disabled   bool
}

type memoryCollectionState struct {
	Records          int
	Bytes            int
	RecordIdentities int
	CandidateBatches int
	ProposalEpoch    uint64
}

type memorySourceState struct {
	Version   string
	Digest    string
	Epoch     uint64
	ExpiresAt time.Time
	Verifier  string
	Revoked   bool
}

func NewMemoryStore(db *DB) *MemoryStore {
	return &MemoryStore{db: db.db, limits: memory.DefaultLimits()}
}

func (s *MemoryStore) SetClock(clock func() time.Time) *MemoryStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = clock
	return s
}

func (s *MemoryStore) SetLimits(limits memory.Limits) *MemoryStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limits = limits
	return s
}

func (s *MemoryStore) SetAuditFailure(f func(memory.AuditEvent) error) *MemoryStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditFailure = f
	return s
}

// SetAfterCommitFailure is test-only fault injection for a lost commit response.
func (s *MemoryStore) SetAfterCommitFailure(f func() error) *MemoryStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.afterCommitFailure = f
	return s
}

// SetLifecycleCommitFailure injects a test-only failure after lifecycle row
// changes and before commit, proving content/quota/cursor rollback together.
func (s *MemoryStore) SetLifecycleCommitFailure(f func() error) *MemoryStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lifecycleCommitFailure = f
	return s
}

func (s *MemoryStore) ContentReadCount() uint64 { return s.contentReads.Load() }

func (s *MemoryStore) ReceiptContentBytes() int {
	rows, err := s.db.Query(`SELECT result FROM agent_runtime_memory_receipts`)
	if err != nil {
		return -1
	}
	defer rows.Close()
	total := 0
	for rows.Next() {
		var raw []byte
		var result memory.MutationResult
		if rows.Scan(&raw) == nil && json.Unmarshal(raw, &result) == nil {
			if result.Record != nil {
				total += len(result.Record.Content)
				for _, topic := range result.Record.Topics {
					total += len(topic)
				}
			}
			for _, record := range result.Records {
				total += len(record.Content)
				for _, topic := range record.Topics {
					total += len(topic)
				}
			}
		}
	}
	return total
}

type sqlQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *MemoryStore) storeNow(ctx context.Context, q sqlQueryer) (time.Time, error) {
	s.mu.RLock()
	clock := s.clock
	s.mu.RUnlock()
	if clock != nil {
		return clock().UTC(), nil
	}
	var now time.Time
	if err := q.QueryRowContext(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, err
	}
	return now.UTC(), nil
}

func (s *MemoryStore) configuredLimits() memory.Limits {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.limits
}

// snapshotTimeOverride preserves the explicit store clock used by deterministic
// callers and tests. The returned value is assigned directly to StoreNow after
// the query, avoiding PostgreSQL timestamptz's microsecond precision loss.
func (s *MemoryStore) snapshotTimeOverride() (time.Time, bool) {
	s.mu.RLock()
	clock := s.clock
	s.mu.RUnlock()
	if clock == nil {
		return time.Time{}, false
	}
	return clock().UTC(), true
}

func (s *MemoryStore) SnapshotSubject(ctx context.Context, subject memory.SubjectScope) (memory.ControlSnapshot, error) {
	if err := subject.Validate(); err != nil {
		return memory.ControlSnapshot{}, err
	}
	defaults, err := json.Marshal(s.configuredLimits())
	if err != nil {
		return memory.ControlSnapshot{}, wrapStoreErr(err)
	}

	// A single statement has one PostgreSQL MVCC snapshot.  That provides the
	// same coherent token-issuance view as the former repeatable-read
	// transaction without paying three sequential control-query round trips.
	// CommitMutation still locks and compares both generations before a write.
	clockNow, hasClock := s.snapshotTimeOverride()
	var clockArg any
	if hasClock {
		clockArg = clockNow
	}
	var snapshot memory.ControlSnapshot
	var encoded []byte
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE($5::timestamptz,transaction_timestamp()),
		COALESCE(p.generation,1),COALESCE(p.limits,$4::jsonb),
		COALESCE(s.generation,1),COALESCE(s.disabled,false)
		FROM (VALUES (1)) AS anchor(n)
		LEFT JOIN agent_runtime_memory_policies p
		  ON p.namespace=$1 AND p.project_id=$2
		LEFT JOIN agent_runtime_memory_subjects s
		  ON s.namespace=$1 AND s.project_id=$2 AND s.subject_id=$3`,
		subject.Namespace, subject.ProjectID, subject.SubjectID, string(defaults), clockArg,
	).Scan(&snapshot.StoreNow, &snapshot.PolicyGeneration, &encoded, &snapshot.SubjectGeneration, &snapshot.SubjectDisabled)
	if err != nil {
		return memory.ControlSnapshot{}, wrapStoreErr(err)
	}
	if err := json.Unmarshal(encoded, &snapshot.Policy); err != nil {
		return memory.ControlSnapshot{}, wrapStoreErr(err)
	}
	if err := memory.ValidateLimits(snapshot.Policy); err != nil {
		return memory.ControlSnapshot{}, fmt.Errorf("%w: invalid persisted memory policy", memory.ErrUnavailable)
	}
	if hasClock {
		snapshot.StoreNow = clockNow
	} else {
		snapshot.StoreNow = snapshot.StoreNow.UTC()
	}
	return snapshot, nil
}

func (s *MemoryStore) Snapshot(ctx context.Context, scope memory.Scope, sourceRef string) (memory.ControlSnapshot, error) {
	if err := scope.Validate(); err != nil {
		return memory.ControlSnapshot{}, err
	}
	defaults, err := json.Marshal(s.configuredLimits())
	if err != nil {
		return memory.ControlSnapshot{}, wrapStoreErr(err)
	}

	// Keep all token claims in one statement-level snapshot.  This query is
	// deliberately not a cache: every issuance observes current generations,
	// policy, collection proposal epoch, and source state.
	clockNow, hasClock := s.snapshotTimeOverride()
	var clockArg any
	if hasClock {
		clockArg = clockNow
	}
	var snapshot memory.ControlSnapshot
	var encoded []byte
	var sourceFound bool
	var sourceVersion, sourceDigest, sourceVerifier string
	var sourceExpiresAt sql.NullTime
	var sourceRevoked bool
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE($7::timestamptz,transaction_timestamp()),
		COALESCE(p.generation,1),COALESCE(p.limits,$5::jsonb),
		COALESCE(s.generation,1),COALESCE(s.disabled,false),COALESCE(c.proposal_epoch,0),
		(src.source_ref IS NOT NULL),COALESCE(src.source_version,''),COALESCE(src.source_digest,''),
		COALESCE(src.source_epoch,0),src.expires_at,COALESCE(src.verifier,''),COALESCE(src.revoked,false)
		FROM (VALUES (1)) AS anchor(n)
		LEFT JOIN agent_runtime_memory_policies p
		  ON p.namespace=$1 AND p.project_id=$2
		LEFT JOIN agent_runtime_memory_subjects s
		  ON s.namespace=$1 AND s.project_id=$2 AND s.subject_id=$3
		LEFT JOIN agent_runtime_memory_collections c
		  ON c.namespace=$1 AND c.project_id=$2 AND c.subject_id=$3 AND c.collection_id=$4
		LEFT JOIN agent_runtime_memory_sources src
		  ON $6<>'' AND src.namespace=$1 AND src.project_id=$2 AND src.subject_id=$3 AND src.collection_id=$4 AND src.source_ref=$6`,
		scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID, string(defaults), sourceRef, clockArg,
	).Scan(&snapshot.StoreNow, &snapshot.PolicyGeneration, &encoded, &snapshot.SubjectGeneration, &snapshot.SubjectDisabled, &snapshot.CollectionProposalEpoch,
		&sourceFound, &sourceVersion, &sourceDigest, &snapshot.SourceEpoch, &sourceExpiresAt, &sourceVerifier, &sourceRevoked)
	if err != nil {
		return memory.ControlSnapshot{}, wrapStoreErr(err)
	}
	if err := json.Unmarshal(encoded, &snapshot.Policy); err != nil {
		return memory.ControlSnapshot{}, wrapStoreErr(err)
	}
	if err := memory.ValidateLimits(snapshot.Policy); err != nil {
		return memory.ControlSnapshot{}, fmt.Errorf("%w: invalid persisted memory policy", memory.ErrUnavailable)
	}
	if hasClock {
		snapshot.StoreNow = clockNow
	} else {
		snapshot.StoreNow = snapshot.StoreNow.UTC()
	}
	if sourceFound {
		snapshot.Source = &memory.SourceControl{Version: sourceVersion, Digest: sourceDigest, Epoch: snapshot.SourceEpoch, ExpiresAt: sourceExpiresAt.Time.UTC(), Verifier: sourceVerifier, Revoked: sourceRevoked}
	}
	return snapshot, nil
}

func (s *MemoryStore) ReadRecord(ctx context.Context, scope memory.Scope, id string) (*memory.Record, memory.ControlSnapshot, error) {
	if err := scope.Validate(); err != nil {
		return nil, memory.ControlSnapshot{}, err
	}
	if id == "" {
		return nil, memory.ControlSnapshot{}, fmt.Errorf("%w: memory id", memory.ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, memory.ControlSnapshot{}, wrapStoreErr(err)
	}
	defer tx.Rollback()
	now, err := s.storeNow(ctx, tx)
	if err != nil {
		return nil, memory.ControlSnapshot{}, wrapStoreErr(err)
	}
	policy, err := s.readPolicy(ctx, tx, scope.Namespace, scope.ProjectID, false)
	if err != nil {
		return nil, memory.ControlSnapshot{}, err
	}
	state, err := readSubject(ctx, tx, scope.Subject(), false)
	if err != nil {
		return nil, memory.ControlSnapshot{}, err
	}
	collection, err := readCollection(ctx, tx, scope, false)
	if err != nil {
		return nil, memory.ControlSnapshot{}, err
	}
	snapshot := memory.ControlSnapshot{SubjectGeneration: state.Generation, SubjectDisabled: state.Disabled, PolicyGeneration: policy.Generation, Policy: policy.Limits, CollectionProposalEpoch: collection.ProposalEpoch, StoreNow: now}
	var record *memory.Record
	if !state.Disabled {
		record, err = readVisibleRecord(ctx, tx, scope, id, state.Generation, now)
		if err != nil {
			return nil, memory.ControlSnapshot{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, memory.ControlSnapshot{}, wrapStoreErr(err)
	}
	if record != nil {
		s.contentReads.Add(1)
	}
	return record, snapshot, nil
}

func (s *MemoryStore) ListRecords(ctx context.Context, req memory.ListRequest) (memory.ListResult, error) {
	if err := req.Scope.Validate(); err != nil {
		return memory.ListResult{}, err
	}
	hasCursor := !req.AfterUpdatedAt.IsZero() || req.AfterMemoryID != "" || req.AfterScope != (memory.Scope{})
	if hasCursor && (req.AfterUpdatedAt.IsZero() || req.AfterMemoryID == "" || req.AfterScope != req.Scope) {
		return memory.ListResult{}, fmt.Errorf("%w: scope-bound keyset cursor", memory.ErrInvalid)
	}
	if !validMemoryListFilters(req.Kinds, req.Topics) {
		return memory.ListResult{}, fmt.Errorf("%w: list filters", memory.ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return memory.ListResult{}, wrapStoreErr(err)
	}
	defer tx.Rollback()
	now, policy, state, err := s.listControlSnapshot(ctx, tx, req.Scope)
	if err != nil {
		return memory.ListResult{}, err
	}
	if req.Limit <= 0 || req.Limit > policy.Limits.MaxRecallCandidates || req.CandidateLimit <= 0 || req.CandidateLimit > policy.Limits.MaxRecallCandidates || req.CandidateBytes <= 0 || req.CandidateBytes > policy.Limits.MaxCandidateBytes {
		return memory.ListResult{}, fmt.Errorf("%w: finite list limit", memory.ErrInvalid)
	}
	result := memory.ListResult{StoreNow: now, Generation: state.Generation}
	if state.Disabled {
		if err := tx.Commit(); err != nil {
			return result, wrapStoreErr(err)
		}
		return result, nil
	}
	s.mu.RLock()
	afterListControl := s.afterListControl
	s.mu.RUnlock()
	if afterListControl != nil {
		afterListControl()
	}
	target := req.Limit
	if target > req.CandidateLimit {
		target = req.CandidateLimit
	}
	kinds := make([]string, len(req.Kinds))
	for i := range req.Kinds {
		kinds[i] = string(req.Kinds[i])
	}
	var kindArg, topicArg any
	if len(kinds) > 0 {
		kindArg = kinds
	}
	if len(req.Topics) > 0 {
		topicArg = req.Topics
	}
	args := []any{req.Scope.Namespace, req.Scope.ProjectID, req.Scope.SubjectID, req.Scope.CollectionID, state.Generation, now, target, kindArg, topicArg}
	cursorSQL := ""
	if hasCursor {
		cursorSQL = ` AND (r.updated_at, r.memory_id) < ($10, $11)`
		args = append(args, req.AfterUpdatedAt, req.AfterMemoryID)
	}
	// Bind source scope directly as well: a hashed EXISTS subplan can otherwise
	// materialize valid sources from every scope before matching outer rows.
	rows, err := tx.QueryContext(ctx, `SELECT r.memory_id,r.revision,r.subject_generation,r.policy_generation,r.proposal_epoch,r.kind,r.content,r.topics,r.status,
		r.source_ref,r.source_version,r.source_epoch,r.producer,r.policy_version,r.decision_actor,r.decision_basis,r.decided_at,r.created_at,r.updated_at,r.expires_at
		FROM agent_runtime_memory_records r
		WHERE r.namespace=$1 AND r.project_id=$2 AND r.subject_id=$3 AND r.collection_id=$4
		  AND r.subject_generation=$5 AND r.status='active' AND r.content<>'' AND r.expires_at>$6
		  AND ($8::text[] IS NULL OR r.kind=ANY($8::text[]))
		  AND ($9::text[] IS NULL OR r.topics ?| $9::text[])
		  AND (r.source_ref='' OR EXISTS (SELECT 1 FROM agent_runtime_memory_sources s
		    WHERE s.namespace=$1 AND s.project_id=$2 AND s.subject_id=$3 AND s.collection_id=$4
		      AND s.source_ref=r.source_ref AND NOT s.revoked AND s.source_epoch=r.source_epoch AND s.source_version=r.source_version AND s.expires_at>$6))`+cursorSQL+`
		ORDER BY r.updated_at DESC,r.memory_id DESC LIMIT $7`, args...)
	if err != nil {
		return memory.ListResult{}, wrapStoreErr(err)
	}
	defer rows.Close()
	used := 2
	var candidateJSON bytes.Buffer
	candidateEncoder := json.NewEncoder(&candidateJSON)
	for rows.Next() {
		record, err := scanRecord(rows, req.Scope)
		if err != nil {
			return memory.ListResult{}, wrapStoreErr(err)
		}
		candidateJSON.Reset()
		if err := candidateEncoder.Encode(record); err != nil {
			return memory.ListResult{}, wrapStoreErr(err)
		}
		size := candidateJSON.Len() - 1 // Encoder appends one newline beyond Marshal's JSON.
		if len(result.Records) > 0 {
			size++
		}
		if used+size > req.CandidateBytes {
			result.Truncated = true
			break
		}
		if result.Records == nil {
			// Allocate only after the first candidate fits; empty reads stay cheap.
			result.Records = make([]memory.Record, 0, target)
		}
		result.Records = append(result.Records, *record)
		used += size
		s.contentReads.Add(1)
	}
	if err := rows.Err(); err != nil {
		return memory.ListResult{}, wrapStoreErr(err)
	}
	if len(result.Records) == target {
		result.Truncated = true
	}
	if err := tx.Commit(); err != nil {
		return result, wrapStoreErr(err)
	}
	return result, nil
}

// listControlSnapshot reads the three controls needed by ListRecords in one
// statement-level MVCC snapshot.  ListRecords retains its read-only
// repeatable-read transaction so this control row and the candidate query see
// the same database snapshot.  It is deliberately separate from token
// snapshots: a list also needs transaction-local StoreNow for expiry checks.
func (s *MemoryStore) listControlSnapshot(ctx context.Context, tx *sql.Tx, scope memory.Scope) (time.Time, memoryPolicyState, memorySubjectState, error) {
	defaults, err := json.Marshal(s.configuredLimits())
	if err != nil {
		return time.Time{}, memoryPolicyState{}, memorySubjectState{}, wrapStoreErr(err)
	}
	clockNow, hasClock := s.snapshotTimeOverride()
	var clockArg any
	if hasClock {
		clockArg = clockNow
	}
	var now time.Time
	var policy memoryPolicyState
	var subject memorySubjectState
	var encoded []byte
	err = tx.QueryRowContext(ctx, `SELECT COALESCE($5::timestamptz,transaction_timestamp()),
		COALESCE(p.generation,1),COALESCE(p.limits,$4::jsonb),
		COALESCE(s.generation,1),COALESCE(s.disabled,false)
		FROM (VALUES (1)) AS anchor(n)
		LEFT JOIN agent_runtime_memory_policies p
		  ON p.namespace=$1 AND p.project_id=$2
		LEFT JOIN agent_runtime_memory_subjects s
		  ON s.namespace=$1 AND s.project_id=$2 AND s.subject_id=$3`,
		scope.Namespace, scope.ProjectID, scope.SubjectID, string(defaults), clockArg,
	).Scan(&now, &policy.Generation, &encoded, &subject.Generation, &subject.Disabled)
	if err != nil {
		return time.Time{}, memoryPolicyState{}, memorySubjectState{}, wrapStoreErr(err)
	}
	if err := json.Unmarshal(encoded, &policy.Limits); err != nil {
		return time.Time{}, memoryPolicyState{}, memorySubjectState{}, wrapStoreErr(err)
	}
	if err := memory.ValidateLimits(policy.Limits); err != nil {
		return time.Time{}, memoryPolicyState{}, memorySubjectState{}, fmt.Errorf("%w: invalid persisted memory policy", memory.ErrUnavailable)
	}
	if hasClock {
		return clockNow, policy, subject, nil
	}
	return now.UTC(), policy, subject, nil
}

func validMemoryListFilters(kinds []memory.Kind, topics []string) bool {
	if len(kinds) > 3 || len(topics) > 32 {
		return false
	}
	for _, kind := range kinds {
		if kind != memory.KindUserStatement && kind != memory.KindPreference && kind != memory.KindVerifiedFact {
			return false
		}
	}
	for _, topic := range topics {
		if topic == "" || !utf8.ValidString(topic) || len(topic) > 128 {
			return false
		}
	}
	return true
}

func (s *MemoryStore) readPolicy(ctx context.Context, q sqlQueryer, namespace, project string, lock bool) (memoryPolicyState, error) {
	query := `SELECT generation,limits FROM agent_runtime_memory_policies WHERE namespace=$1 AND project_id=$2`
	if lock {
		query += ` FOR UPDATE`
	}
	var generation uint64
	var raw []byte
	err := q.QueryRowContext(ctx, query, namespace, project).Scan(&generation, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return memoryPolicyState{Generation: 1, Limits: s.configuredLimits()}, nil
	}
	if err != nil {
		return memoryPolicyState{}, wrapStoreErr(err)
	}
	var limits memory.Limits
	if err := json.Unmarshal(raw, &limits); err != nil {
		return memoryPolicyState{}, wrapStoreErr(err)
	}
	if err := memory.ValidateLimits(limits); err != nil {
		return memoryPolicyState{}, fmt.Errorf("%w: invalid persisted memory policy", memory.ErrUnavailable)
	}
	return memoryPolicyState{Generation: generation, Limits: limits}, nil
}

func readSubject(ctx context.Context, q sqlQueryer, subject memory.SubjectScope, lock bool) (memorySubjectState, error) {
	query := `SELECT generation,disabled FROM agent_runtime_memory_subjects WHERE namespace=$1 AND project_id=$2 AND subject_id=$3`
	if lock {
		query += ` FOR UPDATE`
	}
	var state memorySubjectState
	err := q.QueryRowContext(ctx, query, subject.Namespace, subject.ProjectID, subject.SubjectID).Scan(&state.Generation, &state.Disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return memorySubjectState{Generation: 1}, nil
	}
	if err != nil {
		return memorySubjectState{}, wrapStoreErr(err)
	}
	return state, nil
}

func readCollection(ctx context.Context, q sqlQueryer, scope memory.Scope, lock bool) (memoryCollectionState, error) {
	query := `SELECT records_count,content_bytes,record_identities,candidate_batches,proposal_epoch FROM agent_runtime_memory_collections WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4`
	if lock {
		query += ` FOR UPDATE`
	}
	var state memoryCollectionState
	err := q.QueryRowContext(ctx, query, scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID).Scan(&state.Records, &state.Bytes, &state.RecordIdentities, &state.CandidateBatches, &state.ProposalEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return memoryCollectionState{}, nil
	}
	if err != nil {
		return memoryCollectionState{}, wrapStoreErr(err)
	}
	return state, nil
}

func readSource(ctx context.Context, q sqlQueryer, scope memory.Scope, ref string, lock bool) (memorySourceState, bool, error) {
	query := `SELECT source_version,source_digest,source_epoch,expires_at,verifier,revoked FROM agent_runtime_memory_sources WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4 AND source_ref=$5`
	if lock {
		query += ` FOR UPDATE`
	}
	var state memorySourceState
	err := q.QueryRowContext(ctx, query, scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID, ref).Scan(&state.Version, &state.Digest, &state.Epoch, &state.ExpiresAt, &state.Verifier, &state.Revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return memorySourceState{}, false, nil
	}
	if err != nil {
		return memorySourceState{}, false, wrapStoreErr(err)
	}
	return state, true, nil
}

type rowScanner interface{ Scan(...any) error }

func scanRecord(row rowScanner, scope memory.Scope) (*memory.Record, error) {
	r := &memory.Record{Scope: scope}
	var topics []byte
	var decided sql.NullTime
	if err := row.Scan(&r.MemoryID, &r.Revision, &r.SubjectGeneration, &r.PolicyGeneration, &r.CollectionProposalEpoch, &r.Kind, &r.Content, &topics, &r.Status,
		&r.Provenance.SourceRef, &r.Provenance.SourceVersion, &r.Provenance.SourceEpoch, &r.Provenance.Producer, &r.Provenance.PolicyVersion,
		&r.Decision.Actor, &r.Decision.Basis, &decided, &r.CreatedAt, &r.UpdatedAt, &r.ExpiresAt); err != nil {
		return nil, err
	}
	if decided.Valid {
		r.Decision.DecidedAt = decided.Time.UTC()
	}
	if err := json.Unmarshal(topics, &r.Topics); err != nil {
		return nil, err
	}
	r.CreatedAt = r.CreatedAt.UTC()
	r.UpdatedAt = r.UpdatedAt.UTC()
	r.ExpiresAt = r.ExpiresAt.UTC()
	return r, nil
}

func readRecordForUpdate(ctx context.Context, tx *sql.Tx, scope memory.Scope, id string) (*memory.Record, error) {
	return readStoredRecord(ctx, tx, scope, id, true)
}

func readStoredRecord(ctx context.Context, tx *sql.Tx, scope memory.Scope, id string, lock bool) (*memory.Record, error) {
	query := `SELECT memory_id,revision,subject_generation,policy_generation,proposal_epoch,kind,content,topics,status,
		source_ref,source_version,source_epoch,producer,policy_version,decision_actor,decision_basis,decided_at,created_at,updated_at,expires_at
		FROM agent_runtime_memory_records WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4 AND memory_id=$5`
	if lock {
		query += ` FOR UPDATE`
	}
	row := tx.QueryRowContext(ctx, query,
		scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID, id)
	r, err := scanRecord(row, scope)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, wrapStoreErr(err)
}

func readVisibleRecord(ctx context.Context, tx *sql.Tx, scope memory.Scope, id string, generation uint64, now time.Time) (*memory.Record, error) {
	row := tx.QueryRowContext(ctx, `SELECT r.memory_id,r.revision,r.subject_generation,r.policy_generation,r.proposal_epoch,r.kind,r.content,r.topics,r.status,
		r.source_ref,r.source_version,r.source_epoch,r.producer,r.policy_version,r.decision_actor,r.decision_basis,r.decided_at,r.created_at,r.updated_at,r.expires_at
		FROM agent_runtime_memory_records r WHERE r.namespace=$1 AND r.project_id=$2 AND r.subject_id=$3 AND r.collection_id=$4 AND r.memory_id=$5
		AND r.subject_generation=$6 AND r.status='active' AND r.content<>'' AND r.expires_at>$7
		AND (r.source_ref='' OR EXISTS(SELECT 1 FROM agent_runtime_memory_sources s WHERE s.namespace=r.namespace AND s.project_id=r.project_id AND s.subject_id=r.subject_id AND s.collection_id=r.collection_id AND s.source_ref=r.source_ref AND NOT s.revoked AND s.source_epoch=r.source_epoch AND s.source_version=r.source_version AND s.expires_at>$7))`, scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID, id, generation, now)
	r, err := scanRecord(row, scope)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, wrapStoreErr(err)
}

func wrapStoreErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			return fmt.Errorf("%w: duplicate", memory.ErrConflict)
		case "40001", "40P01":
			return fmt.Errorf("%w: retry transaction", memory.ErrConflict)
		}
	}
	return fmt.Errorf("%w: postgres: %v", memory.ErrUnavailable, err)
}

func validateStoredContent(content string, topics []string, retention time.Duration, limits memory.Limits) error {
	if retention <= 0 || retention > limits.MaxRetention {
		return memory.ErrInvalid
	}
	return memory.ValidatePersistableContent(content, topics, limits)
}

func validateRecordJSON(record *memory.Record, maxBytes int) error {
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(b) > maxBytes {
		return fmt.Errorf("%w: encoded record exceeds %d bytes", memory.ErrInvalid, maxBytes)
	}
	return nil
}

func expectedAction(kind memory.MutationType) memory.Action {
	switch kind {
	case memory.MutationCreate:
		return memory.ActionCreate
	case memory.MutationAttestCreate:
		return memory.ActionCreateVerifiedFact
	case memory.MutationCreateCandidateBatch:
		return memory.ActionPropose
	case memory.MutationConfirmCandidate:
		return memory.ActionConfirm
	case memory.MutationRevise:
		return memory.ActionRevise
	case memory.MutationRetract:
		return memory.ActionRetract
	case memory.MutationForget:
		return memory.ActionForgetSubject
	case memory.MutationEnableSubject:
		return memory.ActionEnableSubject
	case memory.MutationRetractSource:
		return memory.ActionRetractSource
	case memory.MutationUpdatePolicy:
		return memory.ActionUpdatePolicy
	case memory.MutationClaimPurgeLease:
		return memory.ActionClaimPurge
	case memory.MutationPurge:
		return memory.ActionPurge
	default:
		return ""
	}
}

func mutationHasSingleRecord(kind memory.MutationType) bool {
	return kind == memory.MutationCreate || kind == memory.MutationAttestCreate || kind == memory.MutationConfirmCandidate || kind == memory.MutationRevise || kind == memory.MutationRetract
}
func disabledMutationAllowed(kind memory.MutationType) bool {
	return kind == memory.MutationForget || kind == memory.MutationEnableSubject || kind == memory.MutationRetractSource || kind == memory.MutationClaimPurgeLease || kind == memory.MutationPurge || kind == memory.MutationUpdatePolicy
}

func receiptResult(r memory.MutationResult) memory.MutationResult {
	out := memory.MutationResult{SubjectGeneration: r.SubjectGeneration, PolicyGeneration: r.PolicyGeneration, StoreNow: r.StoreNow, PurgeLease: r.PurgeLease, PurgedRecords: r.PurgedRecords, ReleasedBytes: r.ReleasedBytes}
	if r.Record != nil {
		out.Record = &memory.Record{MemoryID: r.Record.MemoryID, Revision: r.Record.Revision, SubjectGeneration: r.Record.SubjectGeneration}
	}
	for _, record := range r.Records {
		out.Records = append(out.Records, memory.Record{MemoryID: record.MemoryID, Revision: record.Revision, SubjectGeneration: record.SubjectGeneration})
	}
	return out
}

func sourceEligible(source memorySourceState, found bool, record *memory.Record, now time.Time) bool {
	if record.Provenance.SourceRef == "" {
		return true
	}
	return found && !source.Revoked && source.Epoch == record.Provenance.SourceEpoch && source.Version == record.Provenance.SourceVersion && source.ExpiresAt.After(now)
}

func sameInstant(a, b time.Time) bool { return a.Equal(b) }
func policyVersion(g uint64) string   { return strconv.FormatUint(g, 10) }

var _ memory.AtomicStore = (*MemoryStore)(nil)

// Keep imports used by the mutation implementation in this file explicit.
var _ = bytes.Equal
var _ = sort.SliceIsSorted

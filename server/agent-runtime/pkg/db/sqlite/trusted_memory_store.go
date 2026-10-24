package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

// MemoryStore is the durable SQLite implementation of memory.AtomicStore.
// It is intentionally separate from DB's legacy db.MemoryStore methods.
type MemoryStore struct {
	db           *DB
	limits       memory.Limits
	now          func() time.Time
	contentReads atomic.Uint64
	mu           sync.RWMutex
	auditFailure func(memory.AuditEvent) error
	commitFault  func(CommitStage) error
}

// CommitStage identifies deterministic test-only fault injection points.
type CommitStage string

const (
	CommitBefore CommitStage = "before_commit"
	CommitAfter  CommitStage = "after_commit"
)

func NewMemoryStore(db *DB) *MemoryStore {
	return &MemoryStore{db: db, limits: memory.DefaultLimits(), now: func() time.Time { return time.Now().UTC() }}
}

func (s *MemoryStore) SetClock(clock func() time.Time) *MemoryStore {
	if clock != nil {
		s.mu.Lock()
		s.now = clock
		s.mu.Unlock()
	}
	return s
}

func (s *MemoryStore) SetLimits(limits memory.Limits) *MemoryStore {
	s.mu.Lock()
	s.limits = limits
	s.mu.Unlock()
	return s
}

func (s *MemoryStore) SetAuditFailure(f func(memory.AuditEvent) error) *MemoryStore {
	s.mu.Lock()
	s.auditFailure = f
	s.mu.Unlock()
	return s
}

// SetCommitFault is a test seam for proving rollback-before-commit and
// commit-unknown replay. Production construction leaves it nil.
func (s *MemoryStore) SetCommitFault(f func(CommitStage) error) *MemoryStore {
	s.mu.Lock()
	s.commitFault = f
	s.mu.Unlock()
	return s
}

func (s *MemoryStore) ContentReadCount() uint64 { return s.contentReads.Load() }

func (s *MemoryStore) ReceiptContentBytes() int {
	rows, err := s.db.db.Query(`SELECT result_json FROM agent_runtime_memory_receipts`)
	if err != nil {
		return -1
	}
	defer rows.Close()
	total := 0
	for rows.Next() {
		var raw string
		var result memory.MutationResult
		if rows.Scan(&raw) != nil || json.Unmarshal([]byte(raw), &result) != nil {
			return -1
		}
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
	return total
}

func (s *MemoryStore) storeNow() time.Time {
	s.mu.RLock()
	clock := s.now
	s.mu.RUnlock()
	return clock().UTC()
}

func (s *MemoryStore) currentLimits() memory.Limits {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.limits
}

type sqliteQueryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type sqlPolicyState struct {
	Generation uint64
	Limits     memory.Limits
}

type sqlSubjectState struct {
	Generation uint64
	Disabled   bool
}

type sqlCollectionState struct {
	Records          int
	Bytes            int
	RecordIdentities int
	CandidateBatches int
	ProposalEpoch    uint64
}

type sqlSourceState struct {
	Found     bool
	Version   string
	Digest    string
	Epoch     uint64
	ExpiresAt time.Time
	Verifier  string
	Revoked   bool
}

func (s *MemoryStore) policyFor(ctx context.Context, q sqliteQueryer, namespace, project string) (sqlPolicyState, error) {
	var generation uint64
	var raw string
	err := q.QueryRowContext(ctx, `SELECT generation, limits_json FROM agent_runtime_memory_policies WHERE namespace=? AND project_id=?`, namespace, project).Scan(&generation, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlPolicyState{Generation: 1, Limits: s.currentLimits()}, nil
	}
	if err != nil {
		return sqlPolicyState{}, mapSQLiteError(err)
	}
	var limits memory.Limits
	if err := json.Unmarshal([]byte(raw), &limits); err != nil || memory.ValidateLimits(limits) != nil {
		return sqlPolicyState{}, fmt.Errorf("%w: corrupt memory policy", memory.ErrUnavailable)
	}
	return sqlPolicyState{Generation: generation, Limits: limits}, nil
}

func subjectFor(ctx context.Context, q sqliteQueryer, subject memory.SubjectScope) (sqlSubjectState, error) {
	var generation uint64
	var disabled bool
	err := q.QueryRowContext(ctx, `SELECT generation, disabled FROM agent_runtime_memory_subjects WHERE namespace=? AND project_id=? AND subject_id=?`, subject.Namespace, subject.ProjectID, subject.SubjectID).Scan(&generation, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlSubjectState{Generation: 1}, nil
	}
	if err != nil {
		return sqlSubjectState{}, mapSQLiteError(err)
	}
	return sqlSubjectState{Generation: generation, Disabled: disabled}, nil
}

func collectionFor(ctx context.Context, q sqliteQueryer, scope memory.Scope) (sqlCollectionState, error) {
	var out sqlCollectionState
	err := q.QueryRowContext(ctx, `SELECT physical_records, physical_bytes, record_identities, candidate_batches, proposal_epoch FROM agent_runtime_memory_collections WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=?`, scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID).Scan(&out.Records, &out.Bytes, &out.RecordIdentities, &out.CandidateBatches, &out.ProposalEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlCollectionState{}, nil
	}
	if err != nil {
		return sqlCollectionState{}, mapSQLiteError(err)
	}
	return out, nil
}

func sourceFor(ctx context.Context, q sqliteQueryer, scope memory.Scope, ref string) (sqlSourceState, error) {
	if ref == "" {
		return sqlSourceState{}, nil
	}
	var out sqlSourceState
	var expires int64
	err := q.QueryRowContext(ctx, `SELECT source_version, digest, source_epoch, expires_at_ns, verifier, revoked FROM agent_runtime_memory_sources WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND source_ref=?`, scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID, ref).Scan(&out.Version, &out.Digest, &out.Epoch, &expires, &out.Verifier, &out.Revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlSourceState{}, nil
	}
	if err != nil {
		return sqlSourceState{}, mapSQLiteError(err)
	}
	out.ExpiresAt = fromNS(expires)
	out.Found = true
	return out, nil
}

func (s *MemoryStore) SnapshotSubject(ctx context.Context, subject memory.SubjectScope) (memory.ControlSnapshot, error) {
	if err := subject.Validate(); err != nil {
		return memory.ControlSnapshot{}, err
	}
	tx, err := s.db.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return memory.ControlSnapshot{}, mapSQLiteError(err)
	}
	defer tx.Rollback()
	state, err := subjectFor(ctx, tx, subject)
	if err != nil {
		return memory.ControlSnapshot{}, err
	}
	policy, err := s.policyFor(ctx, tx, subject.Namespace, subject.ProjectID)
	if err != nil {
		return memory.ControlSnapshot{}, err
	}
	return memory.ControlSnapshot{SubjectGeneration: state.Generation, SubjectDisabled: state.Disabled, PolicyGeneration: policy.Generation, Policy: policy.Limits, StoreNow: s.storeNow()}, nil
}

func (s *MemoryStore) Snapshot(ctx context.Context, scope memory.Scope, sourceRef string) (memory.ControlSnapshot, error) {
	if err := scope.Validate(); err != nil {
		return memory.ControlSnapshot{}, err
	}
	tx, err := s.db.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return memory.ControlSnapshot{}, mapSQLiteError(err)
	}
	defer tx.Rollback()
	subject, err := subjectFor(ctx, tx, scope.Subject())
	if err != nil {
		return memory.ControlSnapshot{}, err
	}
	policy, err := s.policyFor(ctx, tx, scope.Namespace, scope.ProjectID)
	if err != nil {
		return memory.ControlSnapshot{}, err
	}
	collection, err := collectionFor(ctx, tx, scope)
	if err != nil {
		return memory.ControlSnapshot{}, err
	}
	source, err := sourceFor(ctx, tx, scope, sourceRef)
	if err != nil {
		return memory.ControlSnapshot{}, err
	}
	snapshot := memory.ControlSnapshot{SubjectGeneration: subject.Generation, SubjectDisabled: subject.Disabled, PolicyGeneration: policy.Generation, CollectionProposalEpoch: collection.ProposalEpoch, SourceEpoch: source.Epoch, StoreNow: s.storeNow(), Policy: policy.Limits}
	if source.Found {
		snapshot.Source = &memory.SourceControl{Version: source.Version, Digest: source.Digest, Epoch: source.Epoch, ExpiresAt: source.ExpiresAt, Verifier: source.Verifier, Revoked: source.Revoked}
	}
	return snapshot, nil
}

const recordColumns = `revision, subject_generation, policy_generation, proposal_epoch, kind, content, topics_json, status, source_ref, source_version, source_epoch, producer, policy_version, decision_actor, decision_basis, decided_at_ns, created_at_ns, updated_at_ns, expires_at_ns`

type rowScanner interface{ Scan(...any) error }

func scanTrustedRecord(scanner rowScanner, scope memory.Scope, id string) (*memory.Record, error) {
	var r memory.Record
	var topics string
	var decided, created, updated, expires int64
	r.Scope, r.MemoryID = scope, id
	if err := scanner.Scan(&r.Revision, &r.SubjectGeneration, &r.PolicyGeneration, &r.CollectionProposalEpoch, &r.Kind, &r.Content, &topics, &r.Status, &r.Provenance.SourceRef, &r.Provenance.SourceVersion, &r.Provenance.SourceEpoch, &r.Provenance.Producer, &r.Provenance.PolicyVersion, &r.Decision.Actor, &r.Decision.Basis, &decided, &created, &updated, &expires); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(topics), &r.Topics); err != nil {
		return nil, fmt.Errorf("%w: corrupt memory topics", memory.ErrUnavailable)
	}
	r.Decision.DecidedAt, r.CreatedAt, r.UpdatedAt, r.ExpiresAt = fromNS(decided), fromNS(created), fromNS(updated), fromNS(expires)
	return &r, nil
}

func (s *MemoryStore) ReadRecord(ctx context.Context, scope memory.Scope, id string) (*memory.Record, memory.ControlSnapshot, error) {
	if err := scope.Validate(); err != nil {
		return nil, memory.ControlSnapshot{}, err
	}
	if id == "" {
		return nil, memory.ControlSnapshot{}, fmt.Errorf("%w: memory id", memory.ErrInvalid)
	}
	tx, err := s.db.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, memory.ControlSnapshot{}, mapSQLiteError(err)
	}
	defer tx.Rollback()
	now := s.storeNow()
	subject, err := subjectFor(ctx, tx, scope.Subject())
	if err != nil {
		return nil, memory.ControlSnapshot{}, err
	}
	policy, err := s.policyFor(ctx, tx, scope.Namespace, scope.ProjectID)
	if err != nil {
		return nil, memory.ControlSnapshot{}, err
	}
	collection, err := collectionFor(ctx, tx, scope)
	if err != nil {
		return nil, memory.ControlSnapshot{}, err
	}
	snapshot := memory.ControlSnapshot{SubjectGeneration: subject.Generation, SubjectDisabled: subject.Disabled, PolicyGeneration: policy.Generation, CollectionProposalEpoch: collection.ProposalEpoch, StoreNow: now, Policy: policy.Limits}
	if subject.Disabled {
		return nil, snapshot, nil
	}
	query := `SELECT ` + recordColumns + ` FROM agent_runtime_memory_records r WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND memory_id=? AND subject_generation=? AND status=? AND expires_at_ns>? AND (source_ref='' OR EXISTS (SELECT 1 FROM agent_runtime_memory_sources s WHERE s.namespace=r.namespace AND s.project_id=r.project_id AND s.subject_id=r.subject_id AND s.collection_id=r.collection_id AND s.source_ref=r.source_ref AND s.source_epoch=r.source_epoch AND s.source_version=r.source_version AND s.revoked=0 AND s.expires_at_ns>?))`
	r, err := scanTrustedRecord(tx.QueryRowContext(ctx, query, scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID, id, subject.Generation, memory.StatusActive, toNS(now), toNS(now)), scope, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, snapshot, nil
	}
	if err != nil {
		return nil, memory.ControlSnapshot{}, mapSQLiteError(err)
	}
	s.contentReads.Add(1)
	return r, snapshot, nil
}

func (s *MemoryStore) ListRecords(ctx context.Context, req memory.ListRequest) (memory.ListResult, error) {
	if err := req.Scope.Validate(); err != nil {
		return memory.ListResult{}, err
	}
	hasCursor := !req.AfterUpdatedAt.IsZero() || req.AfterMemoryID != "" || req.AfterScope != (memory.Scope{})
	if hasCursor && (req.AfterUpdatedAt.IsZero() || req.AfterMemoryID == "" || req.AfterScope != req.Scope) {
		return memory.ListResult{}, fmt.Errorf("%w: scope-bound keyset cursor", memory.ErrInvalid)
	}
	tx, err := s.db.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return memory.ListResult{}, mapSQLiteError(err)
	}
	defer tx.Rollback()
	policy, err := s.policyFor(ctx, tx, req.Scope.Namespace, req.Scope.ProjectID)
	if err != nil {
		return memory.ListResult{}, err
	}
	// AtomicStore serves both public List (the Module caps that at
	// MaxListLimit) and internal Recall candidate reads. The storage seam may
	// therefore materialize at most MaxRecallCandidates, never an unbounded
	// public page.
	if req.Limit <= 0 || req.Limit > policy.Limits.MaxRecallCandidates || req.CandidateLimit <= 0 || req.CandidateLimit > policy.Limits.MaxRecallCandidates || req.CandidateBytes <= 0 || req.CandidateBytes > policy.Limits.MaxCandidateBytes {
		return memory.ListResult{}, fmt.Errorf("%w: finite list limit", memory.ErrInvalid)
	}
	if !validSQLiteMemoryListFilters(req.Kinds, req.Topics) {
		return memory.ListResult{}, fmt.Errorf("%w: list filters", memory.ErrInvalid)
	}
	subject, err := subjectFor(ctx, tx, req.Scope.Subject())
	if err != nil {
		return memory.ListResult{}, err
	}
	now := s.storeNow()
	result := memory.ListResult{StoreNow: now, Generation: subject.Generation}
	if subject.Disabled {
		return result, nil
	}
	target := req.Limit
	if target > req.CandidateLimit {
		target = req.CandidateLimit
	}
	args := []any{req.Scope.Namespace, req.Scope.ProjectID, req.Scope.SubjectID, req.Scope.CollectionID, subject.Generation, memory.StatusActive, toNS(now), toNS(now)}
	where := `namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND subject_generation=? AND status=? AND expires_at_ns>? AND (source_ref='' OR EXISTS (SELECT 1 FROM agent_runtime_memory_sources s WHERE s.namespace=agent_runtime_memory_records.namespace AND s.project_id=agent_runtime_memory_records.project_id AND s.subject_id=agent_runtime_memory_records.subject_id AND s.collection_id=agent_runtime_memory_records.collection_id AND s.source_ref=agent_runtime_memory_records.source_ref AND s.source_epoch=agent_runtime_memory_records.source_epoch AND s.source_version=agent_runtime_memory_records.source_version AND s.revoked=0 AND s.expires_at_ns>?))`
	if len(req.Kinds) > 0 {
		where += ` AND kind IN (` + sqlPlaceholders(len(req.Kinds)) + `)`
		for _, kind := range req.Kinds {
			args = append(args, kind)
		}
	}
	if len(req.Topics) > 0 {
		where += ` AND EXISTS (SELECT 1 FROM agent_runtime_memory_record_topics mt WHERE mt.namespace=agent_runtime_memory_records.namespace AND mt.project_id=agent_runtime_memory_records.project_id AND mt.subject_id=agent_runtime_memory_records.subject_id AND mt.collection_id=agent_runtime_memory_records.collection_id AND mt.memory_id=agent_runtime_memory_records.memory_id AND mt.topic IN (` + sqlPlaceholders(len(req.Topics)) + `))`
		for _, topic := range req.Topics {
			args = append(args, topic)
		}
	}
	filteredWhere := where
	filteredArgs := append([]any(nil), args...)
	if hasCursor {
		where += ` AND (updated_at_ns<? OR (updated_at_ns=? AND memory_id<?))`
		args = append(args, toNS(req.AfterUpdatedAt), toNS(req.AfterUpdatedAt), req.AfterMemoryID)
	}
	args = append(args, target)
	rows, err := tx.QueryContext(ctx, `SELECT memory_id, `+recordColumns+` FROM agent_runtime_memory_records INDEXED BY idx_memory_records_scope_visible_keyset WHERE `+where+` ORDER BY updated_at_ns DESC, memory_id DESC LIMIT ?`, args...)
	if err != nil {
		return memory.ListResult{}, mapSQLiteError(err)
	}
	defer rows.Close()
	used := 2
	for rows.Next() {
		var id string
		var r memory.Record
		var topics string
		var decided, created, updated, expires int64
		r.Scope = req.Scope
		if err := rows.Scan(&id, &r.Revision, &r.SubjectGeneration, &r.PolicyGeneration, &r.CollectionProposalEpoch, &r.Kind, &r.Content, &topics, &r.Status, &r.Provenance.SourceRef, &r.Provenance.SourceVersion, &r.Provenance.SourceEpoch, &r.Provenance.Producer, &r.Provenance.PolicyVersion, &r.Decision.Actor, &r.Decision.Basis, &decided, &created, &updated, &expires); err != nil {
			return memory.ListResult{}, mapSQLiteError(err)
		}
		r.MemoryID = id
		if err := json.Unmarshal([]byte(topics), &r.Topics); err != nil {
			return memory.ListResult{}, fmt.Errorf("%w: corrupt memory topics", memory.ErrUnavailable)
		}
		r.Decision.DecidedAt, r.CreatedAt, r.UpdatedAt, r.ExpiresAt = fromNS(decided), fromNS(created), fromNS(updated), fromNS(expires)
		encoded, err := json.Marshal(r)
		if err != nil {
			return memory.ListResult{}, err
		}
		size := len(encoded)
		if len(result.Records) > 0 {
			size++
		}
		if used+size > req.CandidateBytes {
			result.Truncated = true
			break
		}
		result.Records = append(result.Records, r)
		s.contentReads.Add(1)
		used += size
	}
	if err := rows.Err(); err != nil {
		return memory.ListResult{}, mapSQLiteError(err)
	}
	if !result.Truncated && len(result.Records) == target {
		last := result.Records[len(result.Records)-1]
		extraArgs := append(filteredArgs, toNS(last.UpdatedAt), toNS(last.UpdatedAt), last.MemoryID)
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM agent_runtime_memory_records INDEXED BY idx_memory_records_scope_visible_keyset WHERE `+filteredWhere+` AND (updated_at_ns<? OR (updated_at_ns=? AND memory_id<?)) LIMIT 1`, extraArgs...).Scan(&one)
		result.Truncated = err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return memory.ListResult{}, mapSQLiteError(err)
		}
	}
	return result, nil
}

func validSQLiteMemoryListFilters(kinds []memory.Kind, topics []string) bool {
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

func sqlPlaceholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func toNS(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixNano()
}
func fromNS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(0, v).UTC()
}

func mapSQLiteError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "database is locked") || strings.Contains(msg, "database is busy") || strings.Contains(msg, "sqlite_busy") || strings.Contains(msg, "sqlite_locked") {
		return fmt.Errorf("%w: sqlite busy", memory.ErrOverloaded)
	}
	return err
}

var _ memory.AtomicStore = (*MemoryStore)(nil)

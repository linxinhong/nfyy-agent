package in_memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

// MemoryStore is the non-durable reference adapter for memory.AtomicStore. It
// intentionally does not share the legacy DB memory maps or interfaces.
type MemoryStore struct {
	mu                 sync.RWMutex
	now                func() time.Time
	limits             memory.Limits
	records            map[memoryRecordKey]*memory.Record
	recordOrder        []memoryRecordKey
	recordOrderIndex   map[memoryRecordKey]int
	recordOrderFree    []int
	expiryScanOffset   int
	reclaimQueue       []memoryLifecycleReclaimEntry
	reclaimQueueFree   []int
	reclaimScanOffset  int
	reclaimRecordIndex map[memoryRecordKey]int
	reclaimJobIndex    map[string]int
	reclaimJobRefs     map[string]int
	candidateScopes    map[memory.Scope]*memoryCandidateScopeIndex
	candidateRecords   map[memoryRecordKey]memoryCandidateIndexRecord
	scopeRecordIDs     map[memory.Scope][]string
	subjectCollections map[memory.SubjectScope][]string
	subjects           map[memory.SubjectScope]memorySubjectState
	collections        map[memory.Scope]memoryCollectionState
	policies           map[memoryProjectKey]memoryPolicyState
	sources            map[memorySourceKey]memorySourceState
	candidateBatches   map[memoryCandidateBatchKey]memoryCandidateBatch
	purgeJobs          map[memory.SubjectScope]memory.PurgeLease
	lifecycleJobs      map[string]memory.PurgeJob
	receipts           map[memoryReceiptKey]memoryReceipt
	receiptIndex       map[memoryReceiptScope]map[memoryReceiptKey]struct{}
	sourceCounts       map[memory.Scope]int
	audits             []memory.AuditEvent
	auditFailure       func(memory.AuditEvent) error
	contentReads       atomic.Uint64
	listExamined       atomic.Uint64
	lifecycleExamined  atomic.Uint64
}

type memoryRecordKey struct {
	Scope memory.Scope
	ID    string
}
type memoryLifecycleReclaimEntry struct {
	RecordKey memoryRecordKey
	TaskID    string
	JobOnly   bool
}
type memorySourceKey struct {
	Scope memory.Scope
	Ref   string
}
type memoryProjectKey struct {
	Namespace string
	ProjectID string
}
type memoryCandidateBatchKey struct {
	Scope                   memory.Scope
	SourceRef               string
	SourceVersion           string
	Producer                string
	ModelConfigVersion      string
	SubjectGeneration       uint64
	PolicyGeneration        uint64
	CollectionProposalEpoch uint64
}
type memoryCandidateBatch struct {
	Digest []byte
	Result memory.MutationResult
}
type memoryReceiptKey struct {
	Subject     memory.SubjectScope
	Scope       memory.Scope
	Action      memory.Action
	OperationID string
}
type memoryReceiptScope struct {
	Subject memory.SubjectScope
	Scope   memory.Scope
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
type memoryPolicyState struct {
	Generation uint64
	Limits     memory.Limits
}
type memorySourceState struct {
	Version   string
	Digest    string
	Epoch     uint64
	ExpiresAt time.Time
	Verifier  string
	Revoked   bool
}
type memoryReceipt struct {
	Digest    []byte
	ExpiresAt time.Time
	Result    memory.MutationResult
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		now: func() time.Time { return time.Now().UTC() }, limits: memory.DefaultLimits(),
		records: make(map[memoryRecordKey]*memory.Record), recordOrderIndex: make(map[memoryRecordKey]int),
		candidateScopes: make(map[memory.Scope]*memoryCandidateScopeIndex), candidateRecords: make(map[memoryRecordKey]memoryCandidateIndexRecord),
		scopeRecordIDs: make(map[memory.Scope][]string), subjectCollections: make(map[memory.SubjectScope][]string),
		subjects: make(map[memory.SubjectScope]memorySubjectState), collections: make(map[memory.Scope]memoryCollectionState), policies: make(map[memoryProjectKey]memoryPolicyState),
		sources: make(map[memorySourceKey]memorySourceState), candidateBatches: make(map[memoryCandidateBatchKey]memoryCandidateBatch), receipts: make(map[memoryReceiptKey]memoryReceipt),
		purgeJobs:          make(map[memory.SubjectScope]memory.PurgeLease),
		lifecycleJobs:      make(map[string]memory.PurgeJob),
		reclaimRecordIndex: make(map[memoryRecordKey]int),
		reclaimJobIndex:    make(map[string]int),
		reclaimJobRefs:     make(map[string]int),
		receiptIndex:       make(map[memoryReceiptScope]map[memoryReceiptKey]struct{}), sourceCounts: make(map[memory.Scope]int),
	}
}

func (s *MemoryStore) SetClock(clock func() time.Time) *MemoryStore {
	if clock == nil {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = clock
	return s
}

func (s *MemoryStore) SetLimits(limits memory.Limits) *MemoryStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limits = limits
	return s
}

// SetAuditFailure injects an adapter-local audit persistence failure. It is
// used by the shared contract to prove no mutation is partially committed.
func (s *MemoryStore) SetAuditFailure(f func(memory.AuditEvent) error) *MemoryStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditFailure = f
	return s
}

func (s *MemoryStore) ContentReadCount() uint64       { return s.contentReads.Load() }
func (s *MemoryStore) LifecycleExaminedCount() uint64 { return s.lifecycleExamined.Load() }

func (s *MemoryStore) listExaminedCount() uint64 { return s.listExamined.Load() }

// ReceiptContentBytes returns the amount of record content retained in
// idempotency receipts. It must remain zero; exposed for contract evidence.
func (s *MemoryStore) ReceiptContentBytes() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := 0
	for _, receipt := range s.receipts {
		if receipt.Result.Record != nil {
			total += len(receipt.Result.Record.Content)
			for _, topic := range receipt.Result.Record.Topics {
				total += len(topic)
			}
		}
		for i := range receipt.Result.Records {
			total += len(receipt.Result.Records[i].Content)
			for _, topic := range receipt.Result.Records[i].Topics {
				total += len(topic)
			}
		}
	}
	return total
}

func (s *MemoryStore) Audits() []memory.AuditEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]memory.AuditEvent(nil), s.audits...)
}

func (s *MemoryStore) SnapshotSubject(ctx context.Context, subject memory.SubjectScope) (memory.ControlSnapshot, error) {
	if err := subject.Validate(); err != nil {
		return memory.ControlSnapshot{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return memory.ControlSnapshot{}, err
	}
	state := s.subjects[subject]
	policy := s.policyFor(subject.Namespace, subject.ProjectID)
	return memory.ControlSnapshot{SubjectGeneration: generationOrInitial(state.Generation), SubjectDisabled: state.Disabled, PolicyGeneration: policy.Generation, Policy: policy.Limits, StoreNow: s.storeNowLocked()}, nil
}

func (s *MemoryStore) Snapshot(ctx context.Context, scope memory.Scope, sourceRef string) (memory.ControlSnapshot, error) {
	if err := scope.Validate(); err != nil {
		return memory.ControlSnapshot{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return memory.ControlSnapshot{}, err
	}
	subject := s.subjects[scope.Subject()]
	collection := s.collections[scope]
	source, sourceFound := s.sources[memorySourceKey{Scope: scope, Ref: sourceRef}]
	policy := s.policyFor(scope.Namespace, scope.ProjectID)
	snapshot := memory.ControlSnapshot{
		SubjectGeneration: generationOrInitial(subject.Generation), SubjectDisabled: subject.Disabled,
		PolicyGeneration: policy.Generation, Policy: policy.Limits, CollectionProposalEpoch: collection.ProposalEpoch, SourceEpoch: source.Epoch,
		StoreNow: s.storeNowLocked(),
	}
	if sourceFound {
		snapshot.Source = &memory.SourceControl{Version: source.Version, Digest: source.Digest, Epoch: source.Epoch, ExpiresAt: source.ExpiresAt, Verifier: source.Verifier, Revoked: source.Revoked}
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, memory.ControlSnapshot{}, err
	}
	now := s.storeNowLocked()
	subject := s.subjects[scope.Subject()]
	policy := s.policyFor(scope.Namespace, scope.ProjectID)
	snapshot := memory.ControlSnapshot{SubjectGeneration: generationOrInitial(subject.Generation), SubjectDisabled: subject.Disabled, PolicyGeneration: policy.Generation, Policy: policy.Limits, CollectionProposalEpoch: s.collections[scope].ProposalEpoch, StoreNow: now}
	record := s.records[memoryRecordKey{Scope: scope, ID: id}]
	if record == nil || subject.Disabled || record.SubjectGeneration != snapshot.SubjectGeneration || record.Status != memory.StatusActive || record.Expired(now) || !s.sourceEligible(record, now) {
		return nil, snapshot, nil
	}
	s.contentReads.Add(1)
	return cloneTrustedRecord(record), snapshot, nil
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return memory.ListResult{}, err
	}
	now := s.storeNowLocked()
	policy := s.policyFor(req.Scope.Namespace, req.Scope.ProjectID)
	// Limit is the internal materialization target. Public List applies its
	// smaller MaxListLimit before this seam; Recall may use the full candidate
	// window so filtering/ranking never happens after a premature list cap.
	if req.Limit <= 0 || req.Limit > policy.Limits.MaxRecallCandidates || req.CandidateLimit <= 0 || req.CandidateLimit > policy.Limits.MaxRecallCandidates || req.CandidateBytes <= 0 || req.CandidateBytes > policy.Limits.MaxCandidateBytes {
		return memory.ListResult{}, fmt.Errorf("%w: finite list limit", memory.ErrInvalid)
	}
	subject := s.subjects[req.Scope.Subject()]
	result := memory.ListResult{StoreNow: now, Generation: generationOrInitial(subject.Generation)}
	if subject.Disabled {
		return result, nil
	}
	target := req.Limit
	if target > req.CandidateLimit {
		target = req.CandidateLimit
	}
	if target > memoryCandidateMaterializationLimit {
		target = memoryCandidateMaterializationLimit
	}
	result.Records = make([]memory.Record, 0, target)
	usedBytes := 2 // JSON array delimiters.
	var cursor *memoryCandidateOrderKey
	if hasCursor {
		cursor = &memoryCandidateOrderKey{UpdatedAt: req.AfterUpdatedAt, MemoryID: req.AfterMemoryID}
	}
	iterator := newMemoryCandidateMergedIterator(s, req.Scope, s.candidateSets(req), cursor, req.Kinds, now, memoryCandidateVisitLimit(req.CandidateLimit), &s.listExamined)
	defer iterator.recordExamined()
	for {
		if err := ctx.Err(); err != nil {
			return memory.ListResult{}, err
		}
		id, ok := iterator.next()
		if !ok {
			break
		}
		if len(result.Records) == target {
			result.Truncated = true
			break
		}
		record := s.records[memoryRecordKey{Scope: req.Scope, ID: id}]
		// The ordered indexes are maintained atomically with records. Keep these
		// checks as a fail-closed guard against future maintenance regressions.
		if record == nil || record.SubjectGeneration != result.Generation || record.Status != memory.StatusActive || record.Content == "" || record.Expired(now) || !s.sourceEligible(record, now) || !matchesMemoryListFilters(record, req.Kinds, req.Topics) {
			continue
		}
		size, err := encodedRecordBytes(record)
		if err != nil {
			return memory.ListResult{}, err
		}
		if len(result.Records) > 0 {
			size++ // JSON array separator.
		}
		if usedBytes+size > req.CandidateBytes {
			result.Truncated = true
			break
		}
		s.contentReads.Add(1)
		result.Records = append(result.Records, *cloneTrustedRecord(record))
		usedBytes += size
	}
	if iterator.exhausted() {
		result.Truncated = true
	}
	return result, nil
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

func matchesMemoryListFilters(record *memory.Record, kinds []memory.Kind, topics []string) bool {
	if len(kinds) > 0 {
		matched := false
		for _, kind := range kinds {
			if record.Kind == kind {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if len(topics) > 0 {
		for _, want := range topics {
			for _, got := range record.Topics {
				if got == want {
					return true
				}
			}
		}
		return false
	}
	return true
}

func encodedRecordBytes(record *memory.Record) (int, error) {
	encoded, err := json.Marshal(record)
	if err != nil {
		return 0, fmt.Errorf("%w: encode record: %v", memory.ErrInvalid, err)
	}
	return len(encoded), nil
}

func validateRecordJSON(record *memory.Record, maxBytes int) error {
	encodedBytes, err := encodedRecordBytes(record)
	if err != nil {
		return err
	}
	if encodedBytes > maxBytes {
		return fmt.Errorf("%w: encoded record exceeds %d bytes", memory.ErrInvalid, maxBytes)
	}
	return nil
}

func validateStoredContent(content string, topics []string, retention time.Duration, limits memory.Limits) error {
	if retention <= 0 || retention > limits.MaxRetention {
		return memory.ErrInvalid
	}
	return memory.ValidatePersistableContent(content, topics, limits)
}

func (s *MemoryStore) CommitMutation(ctx context.Context, cmd memory.Mutation) (memory.MutationResult, error) {
	if cmd.Attestation != nil {
		att := memory.CanonicalSourceAttestation(*cmd.Attestation)
		cmd.Attestation = &att
	}
	if err := ctx.Err(); err != nil {
		return memory.MutationResult{}, err
	}
	subjectMutation := cmd.Type == memory.MutationForget || cmd.Type == memory.MutationEnableSubject
	if subjectMutation {
		if err := cmd.Subject.Validate(); err != nil {
			return memory.MutationResult{}, err
		}
	} else if err := cmd.Scope.Validate(); err != nil {
		return memory.MutationResult{}, err
	}
	expected := expectedAction(cmd.Type)
	if expected == "" || expected != cmd.Operation.Action || cmd.Operation.Subject != cmd.Subject {
		return memory.MutationResult{}, fmt.Errorf("%w: mutation operation binding", memory.ErrInvalid)
	}
	if subjectMutation {
		if cmd.Operation.Scope != (memory.Scope{}) {
			return memory.MutationResult{}, fmt.Errorf("%w: subject operation scope", memory.ErrInvalid)
		}
		if cmd.Audit.Scope != (memory.Scope{}) {
			return memory.MutationResult{}, fmt.Errorf("%w: subject audit scope", memory.ErrInvalid)
		}
	} else if cmd.Subject != cmd.Scope.Subject() || cmd.Operation.Scope != cmd.Scope {
		return memory.MutationResult{}, fmt.Errorf("%w: full mutation scope", memory.ErrInvalid)
	}
	if cmd.Audit.OperationID != cmd.Operation.ID || cmd.Audit.Action != cmd.Operation.Action || cmd.Audit.Subject != cmd.Subject {
		return memory.MutationResult{}, fmt.Errorf("%w: audit operation binding", memory.ErrInvalid)
	}
	if !subjectMutation && cmd.Audit.Scope != cmd.Scope {
		return memory.MutationResult{}, fmt.Errorf("%w: audit scope binding", memory.ErrInvalid)
	}
	if cmd.Operation.ID == "" || cmd.Operation.KeyID == "" || len(cmd.Operation.CommandDigest) == 0 || cmd.Audit.Actor == "" || cmd.Audit.Issuer == "" {
		return memory.MutationResult{}, fmt.Errorf("%w: verified operation", memory.ErrInvalid)
	}
	if mutationHasSingleRecord(cmd.Type) && cmd.Audit.MemoryID != cmd.MemoryID {
		return memory.MutationResult{}, fmt.Errorf("%w: audit memory binding", memory.ErrInvalid)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return memory.MutationResult{}, err
	}
	now := s.storeNowLocked()
	// Token time is adapter-authoritative and precedes receipt lookup. A
	// cleared receipt can therefore never turn an expired token into a write.
	if cmd.Operation.IssuedAt.After(now) || !cmd.Operation.ExpiresAt.After(now) || cmd.Operation.ExpiresAt.Sub(cmd.Operation.IssuedAt) > 7*24*time.Hour {
		return memory.MutationResult{}, memory.ErrExpired
	}
	receiptScope := memoryReceiptScope{Subject: cmd.Subject, Scope: cmd.Scope}
	s.cleanupReceipts(receiptScope, now)
	receiptKey := memoryReceiptKey{Subject: cmd.Subject, Scope: cmd.Scope, Action: cmd.Operation.Action, OperationID: cmd.Operation.ID}
	receipt, replayFound := s.receipts[receiptKey]
	if replayFound {
		if !bytes.Equal(receipt.Digest, cmd.Operation.CommandDigest) {
			return memory.MutationResult{}, fmt.Errorf("%w: idempotency command mismatch", memory.ErrConflict)
		}
	}

	state := s.subjects[cmd.Subject]
	currentGeneration := generationOrInitial(state.Generation)
	policy := s.policyFor(cmd.Subject.Namespace, cmd.Subject.ProjectID)
	if replayFound && cmd.Type == memory.MutationUpdatePolicy && receipt.Result.PolicyGeneration == policy.Generation && cmd.Preconditions.PolicyGeneration+1 == policy.Generation && cmd.Operation.SubjectGeneration == currentGeneration && cmd.Preconditions.SubjectGeneration == currentGeneration {
		result := cloneMutationResult(receipt.Result)
		result.Replayed = true
		return result, nil
	}
	if cmd.Preconditions.PolicyGeneration != policy.Generation {
		return memory.MutationResult{}, fmt.Errorf("%w: policy generation", memory.ErrConflict)
	}
	if replayFound && cmd.Type == memory.MutationForget && receipt.Result.SubjectGeneration == currentGeneration && cmd.Operation.SubjectGeneration+1 == currentGeneration {
		result := cloneMutationResult(receipt.Result)
		result.Replayed = true
		return result, nil
	}
	if replayFound && cmd.Type == memory.MutationEnableSubject && receipt.Result.SubjectGeneration == currentGeneration && cmd.Operation.SubjectGeneration+1 == currentGeneration {
		result := cloneMutationResult(receipt.Result)
		result.Replayed = true
		return result, nil
	}
	if currentGeneration != cmd.Preconditions.SubjectGeneration || cmd.Operation.SubjectGeneration != cmd.Preconditions.SubjectGeneration {
		return memory.MutationResult{}, fmt.Errorf("%w: subject generation", memory.ErrConflict)
	}
	if replayFound {
		result := cloneMutationResult(receipt.Result)
		result.Replayed = true
		return result, nil
	}
	if cmd.Type == memory.MutationPurge {
		if _, err := s.validatePurgeLease(cmd.Subject, cmd.PurgeLease, now, true); err != nil {
			return memory.MutationResult{}, err
		}
	}
	if state.Disabled && !disabledMutationAllowed(cmd.Type) {
		return memory.MutationResult{}, fmt.Errorf("%w: subject disabled", memory.ErrConflict)
	}
	if (cmd.Type == memory.MutationCreate || cmd.Type == memory.MutationCreateCandidateBatch || cmd.Type == memory.MutationConfirmCandidate) && s.collections[cmd.Scope].ProposalEpoch != cmd.Preconditions.CollectionProposalEpoch {
		return memory.MutationResult{}, fmt.Errorf("%w: collection proposal epoch", memory.ErrConflict)
	}

	if s.receiptCount(cmd.Subject, cmd.Scope) >= policy.Limits.MaxReceiptsPerScope {
		return memory.MutationResult{}, memory.ErrQuota
	}
	if s.auditFailure != nil {
		if err := s.auditFailure(cmd.Audit); err != nil {
			return memory.MutationResult{}, fmt.Errorf("%w: %v", memory.ErrAudit, err)
		}
	}

	var result memory.MutationResult
	var commitErr error
	switch cmd.Type {
	case memory.MutationCreate, memory.MutationAttestCreate:
		result, commitErr = s.commitCreate(cmd, now, currentGeneration, policy)
	case memory.MutationCreateCandidateBatch:
		result, commitErr = s.commitCandidateBatch(cmd, now, currentGeneration, policy)
	case memory.MutationConfirmCandidate:
		result, commitErr = s.commitConfirm(cmd, now, currentGeneration, policy)
	case memory.MutationRevise:
		result, commitErr = s.commitRevise(cmd, now, currentGeneration)
	case memory.MutationRetract:
		result, commitErr = s.commitRetract(cmd, now, currentGeneration)
	case memory.MutationForget:
		result = s.commitForget(cmd, now, currentGeneration)
	case memory.MutationEnableSubject:
		result, commitErr = s.commitEnable(cmd, currentGeneration)
	case memory.MutationRetractSource:
		result, commitErr = s.commitRetractSource(cmd, now, currentGeneration)
	case memory.MutationUpdatePolicy:
		result, commitErr = s.commitPolicy(cmd, currentGeneration, policy)
	case memory.MutationClaimPurgeLease:
		result, commitErr = s.commitClaimPurge(cmd, now, currentGeneration)
	case memory.MutationPurge:
		result, commitErr = s.commitPurge(cmd, now, currentGeneration)
	default:
		return memory.MutationResult{}, fmt.Errorf("%w: mutation type", memory.ErrInvalid)
	}
	if commitErr != nil {
		return memory.MutationResult{}, commitErr
	}
	if result.Record == nil && result.SubjectGeneration == 0 {
		return memory.MutationResult{}, memory.ErrConflict
	}
	result.StoreNow = now
	if result.Record != nil {
		cmd.Audit.Revision = result.Record.Revision
	}
	cmd.Audit.Generation = result.SubjectGeneration
	cmd.Audit.OccurredAt = now
	s.audits = append(s.audits, cmd.Audit)
	s.receipts[receiptKey] = memoryReceipt{Digest: append([]byte(nil), cmd.Operation.CommandDigest...), ExpiresAt: cmd.Operation.ExpiresAt, Result: receiptResult(result)}
	if s.receiptIndex[receiptScope] == nil {
		s.receiptIndex[receiptScope] = make(map[memoryReceiptKey]struct{})
	}
	s.receiptIndex[receiptScope][receiptKey] = struct{}{}
	return cloneMutationResult(result), nil
}

func (s *MemoryStore) commitCreate(cmd memory.Mutation, now time.Time, generation uint64, policy memoryPolicyState) (memory.MutationResult, error) {
	key := memoryRecordKey{Scope: cmd.Scope, ID: cmd.MemoryID}
	if cmd.MemoryID == "" || s.records[key] != nil {
		return memory.MutationResult{}, memory.ErrConflict
	}
	collection := s.collections[cmd.Scope]
	if err := validateStoredContent(cmd.Content, cmd.Topics, cmd.Retention, policy.Limits); err != nil {
		return memory.MutationResult{}, err
	}
	newSource := 0
	if cmd.Type == memory.MutationAttestCreate && cmd.Attestation != nil && s.sources[memorySourceKey{Scope: cmd.Scope, Ref: cmd.Attestation.SourceRef}].Epoch == 0 {
		newSource = 1
	}
	if collection.Records+1 > policy.Limits.MaxRecordsPerScope || collection.Bytes+len(cmd.Content) > policy.Limits.MaxContentPerScope || s.sourceCounts[cmd.Scope]+collection.RecordIdentities+collection.CandidateBatches+1+newSource > policy.Limits.MaxControlsPerScope {
		return memory.MutationResult{}, memory.ErrQuota
	}
	provenance := memory.Provenance{Producer: cmd.Audit.Issuer, PolicyVersion: fmt.Sprint(policy.Generation)}
	var sourceKey memorySourceKey
	var nextSource memorySourceState
	if cmd.Type == memory.MutationAttestCreate {
		att := cmd.Attestation
		if att == nil || att.Scope != cmd.Scope || att.Validate() != nil || !att.ExpiresAt.After(now) {
			return memory.MutationResult{}, memory.ErrInvalid
		}
		sourceKey = memorySourceKey{Scope: cmd.Scope, Ref: att.SourceRef}
		current := s.sources[sourceKey]
		if current.Epoch > att.SourceEpoch || (current.Epoch == att.SourceEpoch && current.Epoch != 0 && (current.Version != att.Version || current.Digest != att.Digest || current.Verifier != att.Verifier || current.Revoked || !current.ExpiresAt.Equal(att.ExpiresAt))) {
			return memory.MutationResult{}, memory.ErrConflict
		}
		nextSource = memorySourceState{Version: att.Version, Digest: att.Digest, Epoch: att.SourceEpoch, ExpiresAt: att.ExpiresAt, Verifier: att.Verifier}
		provenance = memory.Provenance{SourceRef: att.SourceRef, SourceVersion: att.Version, SourceEpoch: att.SourceEpoch, Producer: att.Verifier, PolicyVersion: fmt.Sprint(policy.Generation)}
	}
	record := &memory.Record{Scope: cmd.Scope, MemoryID: cmd.MemoryID, Revision: 1, SubjectGeneration: generation, PolicyGeneration: policy.Generation, Kind: cmd.Kind, Content: cmd.Content, Topics: append([]string(nil), cmd.Topics...), Status: memory.StatusActive, Provenance: provenance, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(cmd.Retention)}
	if cmd.Attestation != nil && record.ExpiresAt.After(cmd.Attestation.ExpiresAt) {
		record.ExpiresAt = cmd.Attestation.ExpiresAt
	}
	record.Decision = memory.Decision{Actor: cmd.Audit.Actor, Basis: string(cmd.Audit.Action), DecidedAt: now}
	if err := validateRecordJSON(record, policy.Limits.MaxRecordBytes); err != nil {
		return memory.MutationResult{}, err
	}
	if cmd.Type == memory.MutationAttestCreate {
		if newSource == 1 {
			s.sourceCounts[cmd.Scope]++
		}
		current := s.sources[sourceKey]
		if current.Epoch != 0 && current.Epoch < nextSource.Epoch {
			s.enqueueLifecycle(memory.PurgeJob{TaskID: memory.LifecycleTaskID(memory.PurgeJobRetractSource, cmd.Subject, cmd.Scope, cmd.Attestation.SourceRef, "", current.Epoch, generation), Kind: memory.PurgeJobRetractSource, Subject: cmd.Subject, Scope: cmd.Scope, SourceRef: cmd.Attestation.SourceRef, SourceEpoch: current.Epoch, PurgeBeforeGeneration: generation, SelectionCutoff: now, CreatedAt: now})
		}
		s.sources[sourceKey] = nextSource
	}
	s.records[key] = record
	s.indexRecordOrder(key)
	s.indexLifecycleRecord(key)
	s.indexCandidateRecord(record)
	collection.Records++
	collection.Bytes += len(cmd.Content)
	collection.RecordIdentities++
	s.collections[cmd.Scope] = collection
	if s.subjects[cmd.Subject].Generation == 0 {
		s.subjects[cmd.Subject] = memorySubjectState{Generation: generation}
	}
	return memory.MutationResult{Record: cloneTrustedRecord(record), SubjectGeneration: generation, PolicyGeneration: policy.Generation}, nil
}

func (s *MemoryStore) commitCandidateBatch(cmd memory.Mutation, now time.Time, generation uint64, policy memoryPolicyState) (memory.MutationResult, error) {
	batch := cmd.CandidateBatch
	att := cmd.Attestation
	if batch == nil || batch.Validate() != nil || batch.Producer != cmd.Audit.Issuer || att == nil || att.Scope != cmd.Scope || att.SourceRef != batch.SourceRef || att.Version != batch.SourceVersion || att.Digest == "" || att.SourceEpoch == 0 || cmd.Preconditions.SourceEpoch != att.SourceEpoch || att.Verifier == "" || !att.ExpiresAt.After(now) || len(cmd.Candidates) == 0 || len(cmd.Candidates) > policy.Limits.MaxProposeCandidates {
		return memory.MutationResult{}, memory.ErrInvalid
	}
	sourceKey := memorySourceKey{Scope: cmd.Scope, Ref: att.SourceRef}
	currentSource := s.sources[sourceKey]
	if currentSource.Epoch > att.SourceEpoch || (currentSource.Epoch == att.SourceEpoch && currentSource.Epoch != 0 && (currentSource.Version != att.Version || currentSource.Digest != att.Digest || currentSource.Verifier != att.Verifier || currentSource.Revoked || !currentSource.ExpiresAt.Equal(att.ExpiresAt))) {
		return memory.MutationResult{}, memory.ErrConflict
	}
	newSource := 0
	if currentSource.Epoch == 0 {
		newSource = 1
	}
	batchKey := memoryCandidateBatchKey{
		Scope: cmd.Scope, SourceRef: batch.SourceRef, SourceVersion: batch.SourceVersion,
		Producer: batch.Producer, ModelConfigVersion: batch.ModelConfigVersion,
		SubjectGeneration: generation, PolicyGeneration: policy.Generation,
		CollectionProposalEpoch: cmd.Preconditions.CollectionProposalEpoch,
	}
	if prior, ok := s.candidateBatches[batchKey]; ok {
		if !bytes.Equal(prior.Digest, cmd.Operation.CommandDigest) {
			return memory.MutationResult{}, fmt.Errorf("%w: candidate batch mismatch", memory.ErrConflict)
		}
		result := cloneMutationResult(prior.Result)
		result.Replayed = true
		return result, nil
	}
	collection := s.collections[cmd.Scope]
	seen := make(map[string]struct{}, len(cmd.Candidates))
	prepared := make([]*memory.Record, 0, len(cmd.Candidates))
	addedBytes := 0
	for _, draft := range cmd.Candidates {
		if draft.MemoryID == "" || draft.Kind == memory.KindVerifiedFact {
			return memory.MutationResult{}, memory.ErrInvalid
		}
		if _, duplicate := seen[draft.MemoryID]; duplicate || s.records[memoryRecordKey{Scope: cmd.Scope, ID: draft.MemoryID}] != nil {
			return memory.MutationResult{}, memory.ErrConflict
		}
		seen[draft.MemoryID] = struct{}{}
		if err := validateStoredContent(draft.Content, draft.Topics, draft.Retention, policy.Limits); err != nil {
			return memory.MutationResult{}, err
		}
		addedBytes += len(draft.Content)
		expiresAt := now.Add(draft.Retention)
		if expiresAt.After(att.ExpiresAt) {
			expiresAt = att.ExpiresAt
		}
		provenance := memory.Provenance{SourceRef: att.SourceRef, SourceVersion: att.Version, SourceEpoch: att.SourceEpoch, Producer: batch.Producer, PolicyVersion: fmt.Sprint(policy.Generation)}
		record := &memory.Record{Scope: cmd.Scope, MemoryID: draft.MemoryID, Revision: 1, SubjectGeneration: generation, PolicyGeneration: policy.Generation, CollectionProposalEpoch: cmd.Preconditions.CollectionProposalEpoch, Kind: draft.Kind, Content: draft.Content, Topics: append([]string(nil), draft.Topics...), Status: memory.StatusCandidate, Provenance: provenance, CreatedAt: now, UpdatedAt: now, ExpiresAt: expiresAt}
		if err := validateRecordJSON(record, policy.Limits.MaxRecordBytes); err != nil {
			return memory.MutationResult{}, err
		}
		prepared = append(prepared, record)
	}
	if collection.Records+len(cmd.Candidates) > policy.Limits.MaxRecordsPerScope || collection.Bytes+addedBytes > policy.Limits.MaxContentPerScope || s.sourceCounts[cmd.Scope]+collection.RecordIdentities+collection.CandidateBatches+len(cmd.Candidates)+1+newSource > policy.Limits.MaxControlsPerScope {
		return memory.MutationResult{}, memory.ErrQuota
	}
	result := memory.MutationResult{SubjectGeneration: generation, PolicyGeneration: policy.Generation, Records: make([]memory.Record, 0, len(cmd.Candidates))}
	for _, record := range prepared {
		key := memoryRecordKey{Scope: cmd.Scope, ID: record.MemoryID}
		s.records[key] = record
		s.indexRecordOrder(key)
		s.indexLifecycleRecord(key)
		result.Records = append(result.Records, *cloneTrustedRecord(record))
	}
	collection.Records += len(cmd.Candidates)
	collection.RecordIdentities += len(cmd.Candidates)
	collection.CandidateBatches++
	collection.Bytes += addedBytes
	s.collections[cmd.Scope] = collection
	if newSource == 1 {
		s.sourceCounts[cmd.Scope]++
	}
	if currentSource.Epoch != 0 && currentSource.Epoch < att.SourceEpoch {
		s.enqueueLifecycle(memory.PurgeJob{TaskID: memory.LifecycleTaskID(memory.PurgeJobRetractSource, cmd.Subject, cmd.Scope, att.SourceRef, "", currentSource.Epoch, generation), Kind: memory.PurgeJobRetractSource, Subject: cmd.Subject, Scope: cmd.Scope, SourceRef: att.SourceRef, SourceEpoch: currentSource.Epoch, PurgeBeforeGeneration: generation, SelectionCutoff: now, CreatedAt: now})
	}
	s.sources[sourceKey] = memorySourceState{Version: att.Version, Digest: att.Digest, Epoch: att.SourceEpoch, ExpiresAt: att.ExpiresAt, Verifier: att.Verifier}
	s.candidateBatches[batchKey] = memoryCandidateBatch{Digest: append([]byte(nil), cmd.Operation.CommandDigest...), Result: receiptResult(result)}
	if s.subjects[cmd.Subject].Generation == 0 {
		s.subjects[cmd.Subject] = memorySubjectState{Generation: generation}
	}
	return result, nil
}

func (s *MemoryStore) commitConfirm(cmd memory.Mutation, now time.Time, generation uint64, policy memoryPolicyState) (memory.MutationResult, error) {
	candidate := s.records[memoryRecordKey{Scope: cmd.Scope, ID: cmd.MemoryID}]
	if candidate == nil {
		return memory.MutationResult{}, memory.ErrNotFound
	}
	if candidate.Status != memory.StatusCandidate || candidate.Revision != cmd.Preconditions.ExpectedRevision || candidate.SubjectGeneration != generation || candidate.CollectionProposalEpoch != cmd.Preconditions.CollectionProposalEpoch || candidate.Expired(now) {
		return memory.MutationResult{}, memory.ErrConflict
	}
	if err := validateStoredContent(candidate.Content, candidate.Topics, candidate.ExpiresAt.Sub(now), policy.Limits); err != nil {
		return memory.MutationResult{}, fmt.Errorf("%w: candidate fails current policy", memory.ErrConflict)
	}
	if candidate.Provenance.SourceRef == "" {
		if cmd.Preconditions.SourceEpoch != 0 {
			return memory.MutationResult{}, memory.ErrConflict
		}
	} else {
		source := s.sources[memorySourceKey{Scope: cmd.Scope, Ref: candidate.Provenance.SourceRef}]
		if source.Revoked || !source.ExpiresAt.After(now) || source.Epoch != cmd.Preconditions.SourceEpoch || source.Epoch != candidate.Provenance.SourceEpoch || source.Version != candidate.Provenance.SourceVersion {
			return memory.MutationResult{}, memory.ErrConflict
		}
	}
	var replaced *memory.Record
	if cmd.ReplacedMemoryID != "" {
		replaced = s.records[memoryRecordKey{Scope: cmd.Scope, ID: cmd.ReplacedMemoryID}]
		if replaced == nil || replaced.Status != memory.StatusActive || replaced.Revision != cmd.Preconditions.ExpectedReplacedRevision || replaced.SubjectGeneration != generation {
			return memory.MutationResult{}, memory.ErrConflict
		}
	} else if cmd.Preconditions.ExpectedReplacedRevision != 0 {
		return memory.MutationResult{}, memory.ErrInvalid
	}
	var replacementNext *memory.Record
	if replaced != nil {
		replacementNext = cloneTrustedRecord(replaced)
		replacementNext.Revision++
		replacementNext.Status = memory.StatusRetracted
		replacementNext.UpdatedAt = now
	}
	next := cloneTrustedRecord(candidate)
	next.Revision++
	next.Status = memory.StatusActive
	next.PolicyGeneration = policy.Generation
	next.Provenance.PolicyVersion = fmt.Sprint(policy.Generation)
	next.UpdatedAt = now
	next.Decision = memory.Decision{Actor: cmd.Audit.Actor, Basis: string(memory.ActionConfirm), DecidedAt: now}
	if err := validateRecordJSON(next, policy.Limits.MaxRecordBytes); err != nil {
		return memory.MutationResult{}, fmt.Errorf("%w: candidate fails current policy", memory.ErrConflict)
	}
	if replacementNext != nil {
		s.deindexCandidateRecord(memoryRecordKey{Scope: cmd.Scope, ID: cmd.ReplacedMemoryID})
		s.records[memoryRecordKey{Scope: cmd.Scope, ID: cmd.ReplacedMemoryID}] = replacementNext
	}
	key := memoryRecordKey{Scope: cmd.Scope, ID: cmd.MemoryID}
	s.records[key] = next
	s.indexCandidateRecord(next)
	return memory.MutationResult{Record: cloneTrustedRecord(next), SubjectGeneration: generation, PolicyGeneration: next.PolicyGeneration}, nil
}

func (s *MemoryStore) commitRevise(cmd memory.Mutation, now time.Time, generation uint64) (memory.MutationResult, error) {
	key := memoryRecordKey{Scope: cmd.Scope, ID: cmd.MemoryID}
	current := s.records[key]
	if current == nil || current.Status != memory.StatusActive || current.Content == "" || current.Expired(now) || current.SubjectGeneration != generation || current.Revision != cmd.Preconditions.ExpectedRevision {
		if current == nil {
			return memory.MutationResult{}, memory.ErrNotFound
		}
		return memory.MutationResult{}, memory.ErrConflict
	}
	if current.Kind == memory.KindVerifiedFact {
		return memory.MutationResult{}, fmt.Errorf("%w: verified facts require a trusted revision capability", memory.ErrUnauthorized)
	}
	if !s.sourceEligible(current, now) {
		return memory.MutationResult{}, fmt.Errorf("%w: source is no longer eligible", memory.ErrConflict)
	}
	collection := s.collections[cmd.Scope]
	policy := s.policyFor(cmd.Scope.Namespace, cmd.Scope.ProjectID)
	if err := validateStoredContent(cmd.Content, cmd.Topics, cmd.Retention, policy.Limits); err != nil {
		return memory.MutationResult{}, err
	}
	delta := len(cmd.Content) - len(current.Content)
	if collection.Bytes+delta > policy.Limits.MaxContentPerScope {
		return memory.MutationResult{}, memory.ErrQuota
	}
	next := cloneTrustedRecord(current)
	next.Revision++
	next.Content = cmd.Content
	next.Topics = append([]string(nil), cmd.Topics...)
	next.PolicyGeneration = policy.Generation
	next.Provenance = memory.Provenance{Producer: cmd.Audit.Issuer, PolicyVersion: fmt.Sprint(policy.Generation)}
	next.Decision = memory.Decision{Actor: cmd.Audit.Actor, Basis: string(memory.ActionRevise), DecidedAt: now}
	next.UpdatedAt = now
	next.ExpiresAt = now.Add(cmd.Retention)
	if err := validateRecordJSON(next, policy.Limits.MaxRecordBytes); err != nil {
		return memory.MutationResult{}, err
	}
	s.deindexCandidateRecord(key)
	s.records[key] = next
	s.indexCandidateRecord(next)
	collection.Bytes += delta
	s.collections[cmd.Scope] = collection
	return memory.MutationResult{Record: cloneTrustedRecord(next), SubjectGeneration: generation, PolicyGeneration: next.PolicyGeneration}, nil
}

func (s *MemoryStore) commitRetract(cmd memory.Mutation, now time.Time, generation uint64) (memory.MutationResult, error) {
	key := memoryRecordKey{Scope: cmd.Scope, ID: cmd.MemoryID}
	current := s.records[key]
	if current == nil || current.Status == memory.StatusRetracted || current.Content == "" || current.SubjectGeneration != generation || current.Revision != cmd.Preconditions.ExpectedRevision {
		if current == nil {
			return memory.MutationResult{}, memory.ErrNotFound
		}
		return memory.MutationResult{}, memory.ErrConflict
	}
	next := cloneTrustedRecord(current)
	next.Revision++
	next.Status = memory.StatusRetracted
	next.UpdatedAt = now
	s.deindexCandidateRecord(key)
	s.records[key] = next
	collection := s.collections[cmd.Scope]
	collection.ProposalEpoch++
	s.collections[cmd.Scope] = collection
	s.enqueueLifecycle(memory.PurgeJob{TaskID: memory.LifecycleTaskID(memory.PurgeJobRetract, cmd.Subject, cmd.Scope, "", next.MemoryID, next.Revision, generation), Kind: memory.PurgeJobRetract, Subject: cmd.Subject, Scope: cmd.Scope, MemoryID: next.MemoryID, ExpectedRevision: next.Revision, PurgeBeforeGeneration: generation, SelectionCutoff: now, CreatedAt: now})
	return memory.MutationResult{Record: cloneTrustedRecord(next), SubjectGeneration: generation, PolicyGeneration: next.PolicyGeneration}, nil
}

func (s *MemoryStore) commitForget(cmd memory.Mutation, now time.Time, generation uint64) memory.MutationResult {
	next := memorySubjectState{Generation: generation + 1, Disabled: true}
	s.subjects[cmd.Subject] = next
	s.enqueueLifecycle(memory.PurgeJob{TaskID: memory.LifecycleTaskID(memory.PurgeJobForget, cmd.Subject, memory.Scope{}, "", "", 0, generation), Kind: memory.PurgeJobForget, Subject: cmd.Subject, PurgeBeforeGeneration: generation, SelectionCutoff: now, CreatedAt: now})
	return memory.MutationResult{SubjectGeneration: next.Generation}
}

func (s *MemoryStore) commitEnable(cmd memory.Mutation, generation uint64) (memory.MutationResult, error) {
	state := s.subjects[cmd.Subject]
	if !state.Disabled || generation == 0 {
		return memory.MutationResult{}, memory.ErrConflict
	}
	state.Generation = generation + 1
	state.Disabled = false
	s.subjects[cmd.Subject] = state
	return memory.MutationResult{SubjectGeneration: state.Generation, PolicyGeneration: s.policyFor(cmd.Subject.Namespace, cmd.Subject.ProjectID).Generation}, nil
}

func (s *MemoryStore) commitRetractSource(cmd memory.Mutation, now time.Time, generation uint64) (memory.MutationResult, error) {
	if cmd.SourceRef == "" || cmd.Preconditions.SourceEpoch == 0 {
		return memory.MutationResult{}, memory.ErrInvalid
	}
	key := memorySourceKey{Scope: cmd.Scope, Ref: cmd.SourceRef}
	source, ok := s.sources[key]
	if !ok {
		return memory.MutationResult{}, memory.ErrNotFound
	}
	if source.Revoked || source.Epoch != cmd.Preconditions.SourceEpoch {
		return memory.MutationResult{}, memory.ErrConflict
	}
	oldEpoch := source.Epoch
	source.Epoch++
	source.Revoked = true
	s.sources[key] = source
	s.enqueueLifecycle(memory.PurgeJob{TaskID: memory.LifecycleTaskID(memory.PurgeJobRetractSource, cmd.Subject, cmd.Scope, cmd.SourceRef, "", oldEpoch, generation), Kind: memory.PurgeJobRetractSource, Subject: cmd.Subject, Scope: cmd.Scope, SourceRef: cmd.SourceRef, SourceEpoch: oldEpoch, PurgeBeforeGeneration: generation, SelectionCutoff: now, CreatedAt: now})
	return memory.MutationResult{SubjectGeneration: generation, PolicyGeneration: s.policyFor(cmd.Scope.Namespace, cmd.Scope.ProjectID).Generation}, nil
}

func (s *MemoryStore) commitPolicy(cmd memory.Mutation, generation uint64, current memoryPolicyState) (memory.MutationResult, error) {
	if cmd.PolicyUpdate == nil || memory.ValidateLimits(cmd.PolicyUpdate.Limits) != nil {
		return memory.MutationResult{}, memory.ErrInvalid
	}
	if cmd.PolicyUpdate.ExpectedGeneration != current.Generation {
		return memory.MutationResult{}, fmt.Errorf("%w: policy generation", memory.ErrConflict)
	}
	next := memoryPolicyState{Generation: current.Generation + 1, Limits: cmd.PolicyUpdate.Limits}
	s.policies[memoryProjectKey{Namespace: cmd.Scope.Namespace, ProjectID: cmd.Scope.ProjectID}] = next
	return memory.MutationResult{SubjectGeneration: generation, PolicyGeneration: next.Generation}, nil
}

func (s *MemoryStore) commitClaimPurge(cmd memory.Mutation, now time.Time, generation uint64) (memory.MutationResult, error) {
	requested := cmd.PurgeLease
	if requested.TaskID == "" || requested.OwnerToken == "" || requested.PurgeBeforeGeneration == 0 || requested.PurgeBeforeGeneration > generation || cmd.LeaseDuration <= 0 || cmd.LeaseDuration > time.Hour {
		return memory.MutationResult{}, memory.ErrInvalid
	}
	current, exists := s.purgeJobs[cmd.Subject]
	if exists && current.TaskID == requested.TaskID && current.PurgeBeforeGeneration != requested.PurgeBeforeGeneration {
		return memory.MutationResult{}, fmt.Errorf("%w: purge generation cutoff", memory.ErrConflict)
	}
	if exists && current.LeaseExpiresAt.After(now) && (current.TaskID != requested.TaskID || current.OwnerToken != requested.OwnerToken || current.PurgeBeforeGeneration != requested.PurgeBeforeGeneration) {
		return memory.MutationResult{}, fmt.Errorf("%w: active purge lease", memory.ErrConflict)
	}
	if exists && !current.LeaseExpiresAt.After(now) && current.TaskID != requested.TaskID {
		current = memory.PurgeLease{}
		exists = false
	}
	epoch := current.LeaseEpoch + 1
	if epoch == 0 {
		epoch = 1
	}
	selectionCutoff := current.SelectionCutoff
	if !exists {
		selectionCutoff = now
	}
	lease := memory.PurgeLease{TaskID: requested.TaskID, OwnerToken: requested.OwnerToken, LeaseEpoch: epoch, LeaseExpiresAt: now.Add(cmd.LeaseDuration), SelectionCutoff: selectionCutoff, PurgeBeforeGeneration: requested.PurgeBeforeGeneration, Cursor: current.Cursor}
	s.purgeJobs[cmd.Subject] = lease
	return memory.MutationResult{SubjectGeneration: generation, PolicyGeneration: s.policyFor(cmd.Subject.Namespace, cmd.Subject.ProjectID).Generation, PurgeLease: lease}, nil
}

func (s *MemoryStore) validatePurgeLease(subject memory.SubjectScope, provided memory.PurgeLease, now time.Time, requireCurrentCursor bool) (memory.PurgeLease, error) {
	if provided.TaskID == "" || provided.OwnerToken == "" || provided.LeaseEpoch == 0 || provided.SelectionCutoff.IsZero() {
		return memory.PurgeLease{}, memory.ErrInvalid
	}
	current, ok := s.purgeJobs[subject]
	if !ok || current.TaskID != provided.TaskID || current.OwnerToken != provided.OwnerToken || current.LeaseEpoch != provided.LeaseEpoch || current.PurgeBeforeGeneration != provided.PurgeBeforeGeneration || !current.SelectionCutoff.Equal(provided.SelectionCutoff) || !current.LeaseExpiresAt.Equal(provided.LeaseExpiresAt) || !current.LeaseExpiresAt.After(now) || (requireCurrentCursor && current.Cursor != provided.Cursor) {
		return memory.PurgeLease{}, memory.ErrConflict
	}
	return current, nil
}

func (s *MemoryStore) commitPurge(cmd memory.Mutation, now time.Time, generation uint64) (memory.MutationResult, error) {
	lease, err := s.validatePurgeLease(cmd.Subject, cmd.PurgeLease, now, true)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if len(cmd.PurgeTargets) == 0 || len(cmd.PurgeTargets) > 100 || cmd.PurgeCursor == "" {
		return memory.MutationResult{}, memory.ErrInvalid
	}
	targets := append([]memory.PurgeTarget(nil), cmd.PurgeTargets...)
	if !sort.SliceIsSorted(targets, func(i, j int) bool { return targets[i].MemoryID < targets[j].MemoryID }) {
		return memory.MutationResult{}, memory.ErrInvalid
	}
	for i := 1; i < len(targets); i++ {
		if targets[i-1].MemoryID == targets[i].MemoryID {
			return memory.MutationResult{}, fmt.Errorf("%w: duplicate purge target", memory.ErrInvalid)
		}
	}
	collection := s.collections[cmd.Scope]
	for _, target := range targets {
		key := memoryRecordKey{Scope: cmd.Scope, ID: target.MemoryID}
		record := s.records[key]
		if target.MemoryID == "" || target.ExpectedRevision == 0 || !target.SelectedBefore.Equal(lease.SelectionCutoff) {
			return memory.MutationResult{}, memory.ErrInvalid
		}
		if record == nil || record.Revision != target.ExpectedRevision || record.SubjectGeneration > lease.PurgeBeforeGeneration {
			return memory.MutationResult{}, memory.ErrConflict
		}
		if record.Content == "" {
			continue
		}
		if record.UpdatedAt.After(target.SelectedBefore) || (record.Status != memory.StatusRetracted && record.ExpiresAt.After(target.SelectedBefore) && record.SubjectGeneration >= generation) {
			return memory.MutationResult{}, memory.ErrConflict
		}
	}
	result := memory.MutationResult{SubjectGeneration: generation, PolicyGeneration: s.policyFor(cmd.Scope.Namespace, cmd.Scope.ProjectID).Generation, PurgeLease: lease}
	for _, target := range targets {
		key := memoryRecordKey{Scope: cmd.Scope, ID: target.MemoryID}
		record := s.records[key]
		if record.Content == "" {
			continue
		}
		collection.Bytes -= len(record.Content)
		collection.Records--
		result.PurgedRecords++
		result.ReleasedBytes += len(record.Content)
		s.deindexCandidateRecord(key)
		record = cloneTrustedRecord(record)
		record.Content = ""
		record.Topics = nil
		record.UpdatedAt = now
		s.records[key] = record
	}
	s.collections[cmd.Scope] = collection
	lease.Cursor = cmd.PurgeCursor
	s.purgeJobs[cmd.Subject] = lease
	result.PurgeLease = lease
	return result, nil
}

func (s *MemoryStore) sourceEligible(record *memory.Record, now time.Time) bool {
	if record.Provenance.SourceRef == "" {
		return true
	}
	state, ok := s.sources[memorySourceKey{Scope: record.Scope, Ref: record.Provenance.SourceRef}]
	return ok && !state.Revoked && state.Epoch == record.Provenance.SourceEpoch && state.Version == record.Provenance.SourceVersion && state.ExpiresAt.After(now)
}

func (s *MemoryStore) cleanupReceipts(scope memoryReceiptScope, now time.Time) {
	index := s.receiptIndex[scope]
	for key := range index {
		receipt := s.receipts[key]
		if !receipt.ExpiresAt.After(now) {
			delete(s.receipts, key)
			delete(index, key)
		}
	}
	if len(index) == 0 {
		delete(s.receiptIndex, scope)
	}
}

func (s *MemoryStore) receiptCount(subject memory.SubjectScope, scope memory.Scope) int {
	return len(s.receiptIndex[memoryReceiptScope{Subject: subject, Scope: scope}])
}

// storeNowLocked may only be called while s.mu is held for reading or writing.
// SetClock takes the write lock, keeping the function pointer race-free without
// recursively locking sync.RWMutex.
func (s *MemoryStore) storeNowLocked() time.Time { return s.now().UTC() }
func (s *MemoryStore) policyFor(namespace, projectID string) memoryPolicyState {
	state := s.policies[memoryProjectKey{Namespace: namespace, ProjectID: projectID}]
	if state.Generation == 0 {
		return memoryPolicyState{Generation: 1, Limits: s.limits}
	}
	return state
}
func generationOrInitial(v uint64) uint64 {
	if v == 0 {
		return 1
	}
	return v
}
func cloneTrustedRecord(r *memory.Record) *memory.Record {
	if r == nil {
		return nil
	}
	c := *r
	c.Topics = append([]string(nil), r.Topics...)
	return &c
}
func cloneMutationResult(r memory.MutationResult) memory.MutationResult {
	r.Record = cloneTrustedRecord(r.Record)
	r.Records = append([]memory.Record(nil), r.Records...)
	for i := range r.Records {
		r.Records[i] = *cloneTrustedRecord(&r.Records[i])
	}
	return r
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

var _ memory.AtomicStore = (*MemoryStore)(nil)

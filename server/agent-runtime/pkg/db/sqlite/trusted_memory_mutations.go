package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

type memoryWriteTx struct {
	store      *MemoryStore
	conn       *sql.Conn
	now        time.Time
	cmd        memory.Mutation
	subject    sqlSubjectState
	policy     sqlPolicyState
	collection sqlCollectionState
}

func (s *MemoryStore) CommitMutation(ctx context.Context, cmd memory.Mutation) (memory.MutationResult, error) {
	if cmd.Attestation != nil {
		att := memory.CanonicalSourceAttestation(*cmd.Attestation)
		cmd.Attestation = &att
	}
	if err := validateMutationBinding(cmd); err != nil {
		return memory.MutationResult{}, err
	}
	conn, err := s.db.db.Conn(ctx)
	if err != nil {
		return memory.MutationResult{}, mapSQLiteError(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=25`); err != nil {
		return memory.MutationResult{}, mapSQLiteError(err)
	}
	if err := beginImmediate(ctx, conn); err != nil {
		return memory.MutationResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	w := &memoryWriteTx{store: s, conn: conn, now: s.storeNow(), cmd: cmd}
	if cmd.Operation.IssuedAt.After(w.now) || !cmd.Operation.ExpiresAt.After(w.now) || cmd.Operation.ExpiresAt.Sub(cmd.Operation.IssuedAt) > 7*24*time.Hour {
		return memory.MutationResult{}, memory.ErrExpired
	}
	if err := w.cleanupReceipts(ctx); err != nil {
		return memory.MutationResult{}, err
	}
	receipt, found, err := w.loadReceipt(ctx)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if found && !bytes.Equal(receipt.Digest, cmd.Operation.CommandDigest) {
		return memory.MutationResult{}, fmt.Errorf("%w: idempotency command mismatch", memory.ErrConflict)
	}
	w.subject, err = subjectFor(ctx, conn, cmd.Subject)
	if err != nil {
		return memory.MutationResult{}, err
	}
	w.policy, err = s.policyFor(ctx, conn, cmd.Subject.Namespace, cmd.Subject.ProjectID)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if err := w.ensurePolicy(ctx); err != nil {
		return memory.MutationResult{}, err
	}
	if cmd.Type != memory.MutationForget && cmd.Type != memory.MutationEnableSubject {
		w.collection, err = collectionFor(ctx, conn, cmd.Scope)
		if err != nil {
			return memory.MutationResult{}, err
		}
	}
	currentGeneration := w.subject.Generation
	if found && cmd.Type == memory.MutationUpdatePolicy && receipt.Result.PolicyGeneration == w.policy.Generation && cmd.Preconditions.PolicyGeneration+1 == w.policy.Generation && cmd.Operation.SubjectGeneration == currentGeneration && cmd.Preconditions.SubjectGeneration == currentGeneration {
		return replayResult(receipt.Result), w.finishReplay(ctx, &committed)
	}
	if cmd.Preconditions.PolicyGeneration != w.policy.Generation {
		return memory.MutationResult{}, fmt.Errorf("%w: policy generation", memory.ErrConflict)
	}
	if found && cmd.Type == memory.MutationForget && receipt.Result.SubjectGeneration == currentGeneration && cmd.Operation.SubjectGeneration+1 == currentGeneration {
		return replayResult(receipt.Result), w.finishReplay(ctx, &committed)
	}
	if found && cmd.Type == memory.MutationEnableSubject && receipt.Result.SubjectGeneration == currentGeneration && cmd.Operation.SubjectGeneration+1 == currentGeneration {
		return replayResult(receipt.Result), w.finishReplay(ctx, &committed)
	}
	if currentGeneration != cmd.Preconditions.SubjectGeneration || cmd.Operation.SubjectGeneration != cmd.Preconditions.SubjectGeneration {
		return memory.MutationResult{}, fmt.Errorf("%w: subject generation", memory.ErrConflict)
	}
	if found {
		return replayResult(receipt.Result), w.finishReplay(ctx, &committed)
	}
	if w.subject.Disabled && !disabledMutationAllowed(cmd.Type) {
		return memory.MutationResult{}, fmt.Errorf("%w: subject disabled", memory.ErrConflict)
	}
	if (cmd.Type == memory.MutationCreate || cmd.Type == memory.MutationCreateCandidateBatch || cmd.Type == memory.MutationConfirmCandidate) && w.collection.ProposalEpoch != cmd.Preconditions.CollectionProposalEpoch {
		return memory.MutationResult{}, fmt.Errorf("%w: collection proposal epoch", memory.ErrConflict)
	}
	count, err := w.receiptCount(ctx)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if count >= w.policy.Limits.MaxReceiptsPerScope {
		return memory.MutationResult{}, memory.ErrQuota
	}
	s.mu.RLock()
	auditFailure, commitFault := s.auditFailure, s.commitFault
	s.mu.RUnlock()
	if auditFailure != nil {
		if err := auditFailure(cmd.Audit); err != nil {
			return memory.MutationResult{}, fmt.Errorf("%w: %v", memory.ErrAudit, err)
		}
	}

	var result memory.MutationResult
	switch cmd.Type {
	case memory.MutationCreate, memory.MutationAttestCreate:
		result, err = w.commitCreate(ctx)
	case memory.MutationCreateCandidateBatch:
		result, err = w.commitCandidateBatch(ctx)
	case memory.MutationConfirmCandidate:
		result, err = w.commitConfirm(ctx)
	case memory.MutationRevise:
		result, err = w.commitRevise(ctx)
	case memory.MutationRetract:
		result, err = w.commitRetract(ctx)
	case memory.MutationForget:
		result, err = w.commitForget(ctx)
	case memory.MutationEnableSubject:
		result, err = w.commitEnable(ctx)
	case memory.MutationRetractSource:
		result, err = w.commitRetractSource(ctx)
	case memory.MutationUpdatePolicy:
		result, err = w.commitPolicy(ctx)
	case memory.MutationClaimPurgeLease:
		result, err = w.commitClaimPurge(ctx)
	case memory.MutationPurge:
		result, err = w.commitPurge(ctx)
	default:
		err = fmt.Errorf("%w: mutation type", memory.ErrInvalid)
	}
	if err != nil {
		return memory.MutationResult{}, err
	}
	if result.Record == nil && result.SubjectGeneration == 0 {
		return memory.MutationResult{}, memory.ErrConflict
	}
	result.StoreNow = w.now
	if result.Record != nil {
		cmd.Audit.Revision = result.Record.Revision
	}
	cmd.Audit.Generation = result.SubjectGeneration
	cmd.Audit.OccurredAt = w.now
	if err := w.insertAudit(ctx, cmd.Audit); err != nil {
		return memory.MutationResult{}, fmt.Errorf("%w: %v", memory.ErrAudit, err)
	}
	if err := w.insertReceipt(ctx, receiptResult(result)); err != nil {
		return memory.MutationResult{}, err
	}
	if commitFault != nil {
		if err := commitFault(CommitBefore); err != nil {
			return memory.MutationResult{}, err
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return memory.MutationResult{}, fmt.Errorf("%w: %v", memory.ErrCommitUnknown, mapSQLiteError(err))
	}
	committed = true
	if commitFault != nil {
		if err := commitFault(CommitAfter); err != nil {
			return memory.MutationResult{}, fmt.Errorf("%w: %v", memory.ErrCommitUnknown, err)
		}
	}
	return cloneMutationResult(result), nil
}

func (w *memoryWriteTx) ensurePolicy(ctx context.Context) error {
	raw, err := json.Marshal(w.policy.Limits)
	if err != nil {
		return err
	}
	_, err = w.conn.ExecContext(ctx, `INSERT OR IGNORE INTO agent_runtime_memory_policies(namespace,project_id,generation,limits_json) VALUES(?,?,?,?)`, w.cmd.Subject.Namespace, w.cmd.Subject.ProjectID, w.policy.Generation, string(raw))
	if err != nil {
		return mapSQLiteError(err)
	}
	// The writer transaction serializes first materialization. Reloading also
	// protects adapters configured with different local defaults: the durable
	// project policy is authoritative after the first mutation.
	w.policy, err = w.store.policyFor(ctx, w.conn, w.cmd.Subject.Namespace, w.cmd.Subject.ProjectID)
	return err
}

func (w *memoryWriteTx) finishReplay(ctx context.Context, committed *bool) error {
	if _, err := w.conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("%w: %v", memory.ErrCommitUnknown, mapSQLiteError(err))
	}
	*committed = true
	return nil
}

func beginImmediate(ctx context.Context, conn *sql.Conn) error {
	for attempt := 0; attempt < 4; attempt++ {
		_, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE")
		if err == nil {
			return nil
		}
		mapped := mapSQLiteError(err)
		if errors.Is(mapped, context.DeadlineExceeded) && attempt > 0 {
			return fmt.Errorf("%w: sqlite write lock deadline", memory.ErrOverloaded)
		}
		if !errors.Is(mapped, memory.ErrOverloaded) || attempt == 3 {
			return mapped
		}
		delay := time.Duration(attempt+1) * 2 * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return memory.ErrOverloaded
}

type storedReceipt struct {
	Digest []byte
	Result memory.MutationResult
}

func (w *memoryWriteTx) receiptScope() (string, string, string, string) {
	return w.cmd.Subject.Namespace, w.cmd.Subject.ProjectID, w.cmd.Subject.SubjectID, w.cmd.Scope.CollectionID
}

func (w *memoryWriteTx) cleanupReceipts(ctx context.Context) error {
	ns, project, subject, collection := w.receiptScope()
	_, err := w.conn.ExecContext(ctx, `DELETE FROM agent_runtime_memory_receipts WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND expires_at_ns<=?`, ns, project, subject, collection, toNS(w.now))
	return mapSQLiteError(err)
}

func (w *memoryWriteTx) loadReceipt(ctx context.Context) (storedReceipt, bool, error) {
	ns, project, subject, collection := w.receiptScope()
	var out storedReceipt
	var raw string
	err := w.conn.QueryRowContext(ctx, `SELECT command_digest, result_json FROM agent_runtime_memory_receipts WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND action=? AND operation_id=?`, ns, project, subject, collection, w.cmd.Operation.Action, w.cmd.Operation.ID).Scan(&out.Digest, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return storedReceipt{}, false, nil
	}
	if err != nil {
		return storedReceipt{}, false, mapSQLiteError(err)
	}
	if err := json.Unmarshal([]byte(raw), &out.Result); err != nil {
		return storedReceipt{}, false, fmt.Errorf("%w: corrupt receipt", memory.ErrUnavailable)
	}
	return out, true, nil
}

func (w *memoryWriteTx) receiptCount(ctx context.Context) (int, error) {
	ns, project, subject, collection := w.receiptScope()
	var count int
	err := w.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_runtime_memory_receipts WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=?`, ns, project, subject, collection).Scan(&count)
	return count, mapSQLiteError(err)
}

func (w *memoryWriteTx) insertReceipt(ctx context.Context, result memory.MutationResult) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	ns, project, subject, collection := w.receiptScope()
	_, err = w.conn.ExecContext(ctx, `INSERT INTO agent_runtime_memory_receipts(namespace,project_id,subject_id,collection_id,action,operation_id,command_digest,expires_at_ns,result_json) VALUES(?,?,?,?,?,?,?,?,?)`, ns, project, subject, collection, w.cmd.Operation.Action, w.cmd.Operation.ID, w.cmd.Operation.CommandDigest, toNS(w.cmd.Operation.ExpiresAt), string(raw))
	return mapSQLiteError(err)
}

func (w *memoryWriteTx) insertAudit(ctx context.Context, a memory.AuditEvent) error {
	collection := a.Scope.CollectionID
	_, err := w.conn.ExecContext(ctx, `INSERT INTO agent_runtime_memory_audits(operation_id,namespace,project_id,subject_id,collection_id,action,actor,issuer,memory_id,revision,generation,occurred_at_ns) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, a.OperationID, a.Subject.Namespace, a.Subject.ProjectID, a.Subject.SubjectID, collection, a.Action, a.Actor, a.Issuer, a.MemoryID, a.Revision, a.Generation, toNS(a.OccurredAt))
	return mapSQLiteError(err)
}

func validateMutationBinding(cmd memory.Mutation) error {
	subjectMutation := cmd.Type == memory.MutationForget || cmd.Type == memory.MutationEnableSubject
	if subjectMutation {
		if err := cmd.Subject.Validate(); err != nil {
			return err
		}
	} else if err := cmd.Scope.Validate(); err != nil {
		return err
	}
	expected := expectedAction(cmd.Type)
	if expected == "" || expected != cmd.Operation.Action || cmd.Operation.Subject != cmd.Subject {
		return fmt.Errorf("%w: mutation operation binding", memory.ErrInvalid)
	}
	if subjectMutation {
		if cmd.Operation.Scope != (memory.Scope{}) || cmd.Audit.Scope != (memory.Scope{}) {
			return fmt.Errorf("%w: subject operation scope", memory.ErrInvalid)
		}
	} else if cmd.Subject != cmd.Scope.Subject() || cmd.Operation.Scope != cmd.Scope || cmd.Audit.Scope != cmd.Scope {
		return fmt.Errorf("%w: full mutation scope", memory.ErrInvalid)
	}
	if cmd.Audit.OperationID != cmd.Operation.ID || cmd.Audit.Action != cmd.Operation.Action || cmd.Audit.Subject != cmd.Subject {
		return fmt.Errorf("%w: audit operation binding", memory.ErrInvalid)
	}
	if cmd.Operation.ID == "" || cmd.Operation.KeyID == "" || len(cmd.Operation.CommandDigest) == 0 || cmd.Audit.Actor == "" || cmd.Audit.Issuer == "" {
		return fmt.Errorf("%w: verified operation", memory.ErrInvalid)
	}
	if mutationHasSingleRecord(cmd.Type) && cmd.Audit.MemoryID != cmd.MemoryID {
		return fmt.Errorf("%w: audit memory binding", memory.ErrInvalid)
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

func cloneMutationResult(r memory.MutationResult) memory.MutationResult {
	if r.Record != nil {
		copy := *r.Record
		copy.Topics = append([]string(nil), r.Record.Topics...)
		r.Record = &copy
	}
	r.Records = append([]memory.Record(nil), r.Records...)
	for i := range r.Records {
		r.Records[i].Topics = append([]string(nil), r.Records[i].Topics...)
	}
	return r
}

func replayResult(result memory.MutationResult) memory.MutationResult {
	result = cloneMutationResult(result)
	result.Replayed = true
	return result
}

func validateStoredContent(content string, topics []string, retention time.Duration, limits memory.Limits) error {
	if retention <= 0 || retention > limits.MaxRetention {
		return memory.ErrInvalid
	}
	return memory.ValidatePersistableContent(content, topics, limits)
}

func validateRecordJSON(record *memory.Record, max int) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(raw) > max {
		return fmt.Errorf("%w: encoded record exceeds %d bytes", memory.ErrInvalid, max)
	}
	return nil
}

func (w *memoryWriteTx) record(ctx context.Context, id string) (*memory.Record, error) {
	if id == "" {
		return nil, nil
	}
	r, err := scanTrustedRecord(w.conn.QueryRowContext(ctx, `SELECT `+recordColumns+` FROM agent_runtime_memory_records WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND memory_id=?`, w.cmd.Scope.Namespace, w.cmd.Scope.ProjectID, w.cmd.Scope.SubjectID, w.cmd.Scope.CollectionID, id), w.cmd.Scope, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, mapSQLiteError(err)
}

func (w *memoryWriteTx) writeRecord(ctx context.Context, r *memory.Record) error {
	topics, err := json.Marshal(r.Topics)
	if err != nil {
		return err
	}
	_, err = w.conn.ExecContext(ctx, `INSERT INTO agent_runtime_memory_records(namespace,project_id,subject_id,collection_id,memory_id,revision,subject_generation,policy_generation,proposal_epoch,kind,content,topics_json,status,source_ref,source_version,source_epoch,producer,policy_version,decision_actor,decision_basis,decided_at_ns,created_at_ns,updated_at_ns,expires_at_ns) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(namespace,project_id,subject_id,collection_id,memory_id) DO UPDATE SET revision=excluded.revision,subject_generation=excluded.subject_generation,policy_generation=excluded.policy_generation,proposal_epoch=excluded.proposal_epoch,kind=excluded.kind,content=excluded.content,topics_json=excluded.topics_json,status=excluded.status,source_ref=excluded.source_ref,source_version=excluded.source_version,source_epoch=excluded.source_epoch,producer=excluded.producer,policy_version=excluded.policy_version,decision_actor=excluded.decision_actor,decision_basis=excluded.decision_basis,decided_at_ns=excluded.decided_at_ns,created_at_ns=excluded.created_at_ns,updated_at_ns=excluded.updated_at_ns,expires_at_ns=excluded.expires_at_ns`, r.Scope.Namespace, r.Scope.ProjectID, r.Scope.SubjectID, r.Scope.CollectionID, r.MemoryID, r.Revision, r.SubjectGeneration, r.PolicyGeneration, r.CollectionProposalEpoch, r.Kind, r.Content, string(topics), r.Status, r.Provenance.SourceRef, r.Provenance.SourceVersion, r.Provenance.SourceEpoch, r.Provenance.Producer, r.Provenance.PolicyVersion, r.Decision.Actor, r.Decision.Basis, toNS(r.Decision.DecidedAt), toNS(r.CreatedAt), toNS(r.UpdatedAt), toNS(r.ExpiresAt))
	if err != nil {
		return mapSQLiteError(err)
	}
	if _, err := w.conn.ExecContext(ctx, `DELETE FROM agent_runtime_memory_record_topics WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND memory_id=?`, r.Scope.Namespace, r.Scope.ProjectID, r.Scope.SubjectID, r.Scope.CollectionID, r.MemoryID); err != nil {
		return mapSQLiteError(err)
	}
	for _, topic := range r.Topics {
		if _, err := w.conn.ExecContext(ctx, `INSERT OR IGNORE INTO agent_runtime_memory_record_topics(namespace,project_id,subject_id,collection_id,memory_id,topic) VALUES(?,?,?,?,?,?)`, r.Scope.Namespace, r.Scope.ProjectID, r.Scope.SubjectID, r.Scope.CollectionID, r.MemoryID, topic); err != nil {
			return mapSQLiteError(err)
		}
	}
	return nil
}

func (w *memoryWriteTx) writeSubject(ctx context.Context, state sqlSubjectState) error {
	_, err := w.conn.ExecContext(ctx, `INSERT INTO agent_runtime_memory_subjects(namespace,project_id,subject_id,generation,disabled) VALUES(?,?,?,?,?) ON CONFLICT(namespace,project_id,subject_id) DO UPDATE SET generation=excluded.generation,disabled=excluded.disabled`, w.cmd.Subject.Namespace, w.cmd.Subject.ProjectID, w.cmd.Subject.SubjectID, state.Generation, state.Disabled)
	return mapSQLiteError(err)
}

func (w *memoryWriteTx) writeCollection(ctx context.Context, state sqlCollectionState) error {
	_, err := w.conn.ExecContext(ctx, `INSERT INTO agent_runtime_memory_collections(namespace,project_id,subject_id,collection_id,physical_records,physical_bytes,record_identities,candidate_batches,proposal_epoch) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(namespace,project_id,subject_id,collection_id) DO UPDATE SET physical_records=excluded.physical_records,physical_bytes=excluded.physical_bytes,record_identities=excluded.record_identities,candidate_batches=excluded.candidate_batches,proposal_epoch=excluded.proposal_epoch`, w.cmd.Scope.Namespace, w.cmd.Scope.ProjectID, w.cmd.Scope.SubjectID, w.cmd.Scope.CollectionID, state.Records, state.Bytes, state.RecordIdentities, state.CandidateBatches, state.ProposalEpoch)
	return mapSQLiteError(err)
}

func (w *memoryWriteTx) writeSource(ctx context.Context, ref string, state sqlSourceState) error {
	_, err := w.conn.ExecContext(ctx, `INSERT INTO agent_runtime_memory_sources(namespace,project_id,subject_id,collection_id,source_ref,source_version,digest,source_epoch,expires_at_ns,verifier,revoked) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(namespace,project_id,subject_id,collection_id,source_ref) DO UPDATE SET source_version=excluded.source_version,digest=excluded.digest,source_epoch=excluded.source_epoch,expires_at_ns=excluded.expires_at_ns,verifier=excluded.verifier,revoked=excluded.revoked`, w.cmd.Scope.Namespace, w.cmd.Scope.ProjectID, w.cmd.Scope.SubjectID, w.cmd.Scope.CollectionID, ref, state.Version, state.Digest, state.Epoch, toNS(state.ExpiresAt), state.Verifier, state.Revoked)
	return mapSQLiteError(err)
}

func (w *memoryWriteTx) sourceCount(ctx context.Context) (int, error) {
	var count int
	err := w.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_runtime_memory_sources WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=?`, w.cmd.Scope.Namespace, w.cmd.Scope.ProjectID, w.cmd.Scope.SubjectID, w.cmd.Scope.CollectionID).Scan(&count)
	return count, mapSQLiteError(err)
}

func cloneRecord(r *memory.Record) *memory.Record {
	if r == nil {
		return nil
	}
	out := *r
	out.Topics = append([]string(nil), r.Topics...)
	return &out
}

func sourceEligible(r *memory.Record, source sqlSourceState, now time.Time) bool {
	if r.Provenance.SourceRef == "" {
		return true
	}
	return source.Epoch != 0 && !source.Revoked && source.Epoch == r.Provenance.SourceEpoch && source.Version == r.Provenance.SourceVersion && source.ExpiresAt.After(now)
}

func (w *memoryWriteTx) enqueueSupersededSource(ctx context.Context, sourceRef string, oldEpoch, newEpoch uint64) error {
	if oldEpoch == 0 || oldEpoch >= newEpoch {
		return nil
	}
	return insertSQLiteLifecycleJob(ctx, w.conn, memory.PurgeJob{
		TaskID:                memory.LifecycleTaskID(memory.PurgeJobRetractSource, w.cmd.Subject, w.cmd.Scope, sourceRef, "", oldEpoch, w.subject.Generation),
		Kind:                  memory.PurgeJobRetractSource,
		Subject:               w.cmd.Subject,
		Scope:                 w.cmd.Scope,
		SourceRef:             sourceRef,
		SourceEpoch:           oldEpoch,
		PurgeBeforeGeneration: w.subject.Generation,
		SelectionCutoff:       w.now,
		CreatedAt:             w.now,
	})
}

func (w *memoryWriteTx) commitCreate(ctx context.Context) (memory.MutationResult, error) {
	cmd := w.cmd
	if cmd.MemoryID == "" {
		return memory.MutationResult{}, memory.ErrConflict
	}
	existing, err := w.record(ctx, cmd.MemoryID)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if existing != nil {
		return memory.MutationResult{}, memory.ErrConflict
	}
	if err := validateStoredContent(cmd.Content, cmd.Topics, cmd.Retention, w.policy.Limits); err != nil {
		return memory.MutationResult{}, err
	}
	sourceCount, err := w.sourceCount(ctx)
	if err != nil {
		return memory.MutationResult{}, err
	}
	newSource := 0
	oldSourceEpoch := uint64(0)
	var source sqlSourceState
	provenance := memory.Provenance{Producer: cmd.Audit.Issuer, PolicyVersion: fmt.Sprint(w.policy.Generation)}
	if cmd.Type == memory.MutationAttestCreate {
		att := cmd.Attestation
		if att == nil || att.Scope != cmd.Scope || att.Validate() != nil || !att.ExpiresAt.After(w.now) {
			return memory.MutationResult{}, memory.ErrInvalid
		}
		source, err = sourceFor(ctx, w.conn, cmd.Scope, att.SourceRef)
		if err != nil {
			return memory.MutationResult{}, err
		}
		if source.Epoch > att.SourceEpoch || (source.Epoch == att.SourceEpoch && source.Epoch != 0 && (source.Version != att.Version || source.Digest != att.Digest || source.Verifier != att.Verifier || source.Revoked || !source.ExpiresAt.Equal(att.ExpiresAt))) {
			return memory.MutationResult{}, memory.ErrConflict
		}
		if source.Epoch == 0 {
			newSource = 1
		}
		oldSourceEpoch = source.Epoch
		source = sqlSourceState{Version: att.Version, Digest: att.Digest, Epoch: att.SourceEpoch, ExpiresAt: att.ExpiresAt, Verifier: att.Verifier}
		provenance = memory.Provenance{SourceRef: att.SourceRef, SourceVersion: att.Version, SourceEpoch: att.SourceEpoch, Producer: att.Verifier, PolicyVersion: fmt.Sprint(w.policy.Generation)}
	}
	if w.collection.Records+1 > w.policy.Limits.MaxRecordsPerScope || w.collection.Bytes+len(cmd.Content) > w.policy.Limits.MaxContentPerScope || sourceCount+w.collection.RecordIdentities+w.collection.CandidateBatches+1+newSource > w.policy.Limits.MaxControlsPerScope {
		return memory.MutationResult{}, memory.ErrQuota
	}
	record := &memory.Record{Scope: cmd.Scope, MemoryID: cmd.MemoryID, Revision: 1, SubjectGeneration: w.subject.Generation, PolicyGeneration: w.policy.Generation, Kind: cmd.Kind, Content: cmd.Content, Topics: append([]string(nil), cmd.Topics...), Status: memory.StatusActive, Provenance: provenance, Decision: memory.Decision{Actor: cmd.Audit.Actor, Basis: string(cmd.Audit.Action), DecidedAt: w.now}, CreatedAt: w.now, UpdatedAt: w.now, ExpiresAt: w.now.Add(cmd.Retention)}
	if cmd.Attestation != nil && record.ExpiresAt.After(cmd.Attestation.ExpiresAt) {
		record.ExpiresAt = cmd.Attestation.ExpiresAt
	}
	if err := validateRecordJSON(record, w.policy.Limits.MaxRecordBytes); err != nil {
		return memory.MutationResult{}, err
	}
	if cmd.Type == memory.MutationAttestCreate {
		if err := w.enqueueSupersededSource(ctx, cmd.Attestation.SourceRef, oldSourceEpoch, source.Epoch); err != nil {
			return memory.MutationResult{}, err
		}
		if err := w.writeSource(ctx, cmd.Attestation.SourceRef, source); err != nil {
			return memory.MutationResult{}, err
		}
	}
	if err := w.writeRecord(ctx, record); err != nil {
		return memory.MutationResult{}, err
	}
	w.collection.Records++
	w.collection.Bytes += len(cmd.Content)
	w.collection.RecordIdentities++
	if err := w.writeCollection(ctx, w.collection); err != nil {
		return memory.MutationResult{}, err
	}
	if err := w.writeSubject(ctx, w.subject); err != nil {
		return memory.MutationResult{}, err
	}
	return memory.MutationResult{Record: cloneRecord(record), SubjectGeneration: w.subject.Generation, PolicyGeneration: w.policy.Generation}, nil
}

type candidateBatchKey struct {
	SourceRef, SourceVersion, Producer, ModelConfigVersion string
	SubjectGeneration, PolicyGeneration, ProposalEpoch     uint64
}

func (w *memoryWriteTx) loadCandidateBatch(ctx context.Context, key candidateBatchKey) ([]byte, memory.MutationResult, bool, error) {
	var digest []byte
	var raw string
	err := w.conn.QueryRowContext(ctx, `SELECT command_digest,result_json FROM agent_runtime_memory_candidate_batches WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND source_ref=? AND source_version=? AND producer=? AND model_config_version=? AND subject_generation=? AND policy_generation=? AND proposal_epoch=?`, w.cmd.Scope.Namespace, w.cmd.Scope.ProjectID, w.cmd.Scope.SubjectID, w.cmd.Scope.CollectionID, key.SourceRef, key.SourceVersion, key.Producer, key.ModelConfigVersion, key.SubjectGeneration, key.PolicyGeneration, key.ProposalEpoch).Scan(&digest, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, memory.MutationResult{}, false, nil
	}
	if err != nil {
		return nil, memory.MutationResult{}, false, mapSQLiteError(err)
	}
	var result memory.MutationResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, memory.MutationResult{}, false, fmt.Errorf("%w: corrupt candidate receipt", memory.ErrUnavailable)
	}
	return digest, result, true, nil
}

func (w *memoryWriteTx) writeCandidateBatch(ctx context.Context, key candidateBatchKey, result memory.MutationResult) error {
	raw, err := json.Marshal(receiptResult(result))
	if err != nil {
		return err
	}
	_, err = w.conn.ExecContext(ctx, `INSERT INTO agent_runtime_memory_candidate_batches(namespace,project_id,subject_id,collection_id,source_ref,source_version,producer,model_config_version,subject_generation,policy_generation,proposal_epoch,command_digest,result_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, w.cmd.Scope.Namespace, w.cmd.Scope.ProjectID, w.cmd.Scope.SubjectID, w.cmd.Scope.CollectionID, key.SourceRef, key.SourceVersion, key.Producer, key.ModelConfigVersion, key.SubjectGeneration, key.PolicyGeneration, key.ProposalEpoch, w.cmd.Operation.CommandDigest, string(raw))
	return mapSQLiteError(err)
}

func (w *memoryWriteTx) commitCandidateBatch(ctx context.Context) (memory.MutationResult, error) {
	cmd, batch, att := w.cmd, w.cmd.CandidateBatch, w.cmd.Attestation
	if batch == nil || batch.Validate() != nil || batch.Producer != cmd.Audit.Issuer || att == nil || att.Scope != cmd.Scope || att.SourceRef != batch.SourceRef || att.Version != batch.SourceVersion || att.Digest == "" || att.SourceEpoch == 0 || cmd.Preconditions.SourceEpoch != att.SourceEpoch || att.Verifier == "" || !att.ExpiresAt.After(w.now) || len(cmd.Candidates) == 0 || len(cmd.Candidates) > w.policy.Limits.MaxProposeCandidates {
		return memory.MutationResult{}, memory.ErrInvalid
	}
	source, err := sourceFor(ctx, w.conn, cmd.Scope, att.SourceRef)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if source.Epoch > att.SourceEpoch || (source.Epoch == att.SourceEpoch && source.Epoch != 0 && (source.Version != att.Version || source.Digest != att.Digest || source.Verifier != att.Verifier || source.Revoked || !source.ExpiresAt.Equal(att.ExpiresAt))) {
		return memory.MutationResult{}, memory.ErrConflict
	}
	newSource := 0
	if source.Epoch == 0 {
		newSource = 1
	}
	key := candidateBatchKey{batch.SourceRef, batch.SourceVersion, batch.Producer, batch.ModelConfigVersion, w.subject.Generation, w.policy.Generation, cmd.Preconditions.CollectionProposalEpoch}
	digest, prior, found, err := w.loadCandidateBatch(ctx, key)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if found {
		if !bytes.Equal(digest, cmd.Operation.CommandDigest) {
			return memory.MutationResult{}, fmt.Errorf("%w: candidate batch mismatch", memory.ErrConflict)
		}
		return replayResult(prior), nil
	}
	seen := make(map[string]struct{}, len(cmd.Candidates))
	prepared := make([]*memory.Record, 0, len(cmd.Candidates))
	addedBytes := 0
	for _, draft := range cmd.Candidates {
		if draft.MemoryID == "" || draft.Kind == memory.KindVerifiedFact {
			return memory.MutationResult{}, memory.ErrInvalid
		}
		if _, duplicate := seen[draft.MemoryID]; duplicate {
			return memory.MutationResult{}, memory.ErrConflict
		}
		seen[draft.MemoryID] = struct{}{}
		existing, err := w.record(ctx, draft.MemoryID)
		if err != nil {
			return memory.MutationResult{}, err
		}
		if existing != nil {
			return memory.MutationResult{}, memory.ErrConflict
		}
		if err := validateStoredContent(draft.Content, draft.Topics, draft.Retention, w.policy.Limits); err != nil {
			return memory.MutationResult{}, err
		}
		addedBytes += len(draft.Content)
		expires := w.now.Add(draft.Retention)
		if expires.After(att.ExpiresAt) {
			expires = att.ExpiresAt
		}
		r := &memory.Record{Scope: cmd.Scope, MemoryID: draft.MemoryID, Revision: 1, SubjectGeneration: w.subject.Generation, PolicyGeneration: w.policy.Generation, CollectionProposalEpoch: cmd.Preconditions.CollectionProposalEpoch, Kind: draft.Kind, Content: draft.Content, Topics: append([]string(nil), draft.Topics...), Status: memory.StatusCandidate, Provenance: memory.Provenance{SourceRef: att.SourceRef, SourceVersion: att.Version, SourceEpoch: att.SourceEpoch, Producer: batch.Producer, PolicyVersion: fmt.Sprint(w.policy.Generation)}, CreatedAt: w.now, UpdatedAt: w.now, ExpiresAt: expires}
		if err := validateRecordJSON(r, w.policy.Limits.MaxRecordBytes); err != nil {
			return memory.MutationResult{}, err
		}
		prepared = append(prepared, r)
	}
	sourceCount, err := w.sourceCount(ctx)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if w.collection.Records+len(prepared) > w.policy.Limits.MaxRecordsPerScope || w.collection.Bytes+addedBytes > w.policy.Limits.MaxContentPerScope || sourceCount+w.collection.RecordIdentities+w.collection.CandidateBatches+len(prepared)+1+newSource > w.policy.Limits.MaxControlsPerScope {
		return memory.MutationResult{}, memory.ErrQuota
	}
	result := memory.MutationResult{SubjectGeneration: w.subject.Generation, PolicyGeneration: w.policy.Generation, Records: make([]memory.Record, 0, len(prepared))}
	for _, r := range prepared {
		if err := w.writeRecord(ctx, r); err != nil {
			return memory.MutationResult{}, err
		}
		result.Records = append(result.Records, *cloneRecord(r))
	}
	w.collection.Records += len(prepared)
	w.collection.RecordIdentities += len(prepared)
	w.collection.CandidateBatches++
	w.collection.Bytes += addedBytes
	if err := w.writeCollection(ctx, w.collection); err != nil {
		return memory.MutationResult{}, err
	}
	if err := w.writeSubject(ctx, w.subject); err != nil {
		return memory.MutationResult{}, err
	}
	if err := w.enqueueSupersededSource(ctx, att.SourceRef, source.Epoch, att.SourceEpoch); err != nil {
		return memory.MutationResult{}, err
	}
	if err := w.writeSource(ctx, att.SourceRef, sqlSourceState{Version: att.Version, Digest: att.Digest, Epoch: att.SourceEpoch, ExpiresAt: att.ExpiresAt, Verifier: att.Verifier}); err != nil {
		return memory.MutationResult{}, err
	}
	if err := w.writeCandidateBatch(ctx, key, result); err != nil {
		return memory.MutationResult{}, err
	}
	return result, nil
}

func (w *memoryWriteTx) commitConfirm(ctx context.Context) (memory.MutationResult, error) {
	cmd := w.cmd
	candidate, err := w.record(ctx, cmd.MemoryID)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if candidate == nil {
		return memory.MutationResult{}, memory.ErrNotFound
	}
	if candidate.Status != memory.StatusCandidate || candidate.Revision != cmd.Preconditions.ExpectedRevision || candidate.SubjectGeneration != w.subject.Generation || candidate.CollectionProposalEpoch != cmd.Preconditions.CollectionProposalEpoch || candidate.Expired(w.now) {
		return memory.MutationResult{}, memory.ErrConflict
	}
	if err := validateStoredContent(candidate.Content, candidate.Topics, candidate.ExpiresAt.Sub(w.now), w.policy.Limits); err != nil {
		return memory.MutationResult{}, fmt.Errorf("%w: candidate fails current policy", memory.ErrConflict)
	}
	if candidate.Provenance.SourceRef == "" {
		if cmd.Preconditions.SourceEpoch != 0 {
			return memory.MutationResult{}, memory.ErrConflict
		}
	} else {
		source, err := sourceFor(ctx, w.conn, cmd.Scope, candidate.Provenance.SourceRef)
		if err != nil {
			return memory.MutationResult{}, err
		}
		if source.Revoked || !source.ExpiresAt.After(w.now) || source.Epoch != cmd.Preconditions.SourceEpoch || source.Epoch != candidate.Provenance.SourceEpoch || source.Version != candidate.Provenance.SourceVersion {
			return memory.MutationResult{}, memory.ErrConflict
		}
	}
	if cmd.ReplacedMemoryID != "" {
		replaced, err := w.record(ctx, cmd.ReplacedMemoryID)
		if err != nil {
			return memory.MutationResult{}, err
		}
		if replaced == nil || replaced.Status != memory.StatusActive || replaced.Revision != cmd.Preconditions.ExpectedReplacedRevision || replaced.SubjectGeneration != w.subject.Generation {
			return memory.MutationResult{}, memory.ErrConflict
		}
		next := cloneRecord(replaced)
		next.Revision++
		next.Status = memory.StatusRetracted
		next.UpdatedAt = w.now
		if err := w.writeRecord(ctx, next); err != nil {
			return memory.MutationResult{}, err
		}
	} else if cmd.Preconditions.ExpectedReplacedRevision != 0 {
		return memory.MutationResult{}, memory.ErrInvalid
	}
	next := cloneRecord(candidate)
	next.Revision++
	next.Status = memory.StatusActive
	next.PolicyGeneration = w.policy.Generation
	next.Provenance.PolicyVersion = fmt.Sprint(w.policy.Generation)
	next.UpdatedAt = w.now
	next.Decision = memory.Decision{Actor: cmd.Audit.Actor, Basis: string(memory.ActionConfirm), DecidedAt: w.now}
	if err := validateRecordJSON(next, w.policy.Limits.MaxRecordBytes); err != nil {
		return memory.MutationResult{}, fmt.Errorf("%w: candidate fails current policy", memory.ErrConflict)
	}
	if err := w.writeRecord(ctx, next); err != nil {
		return memory.MutationResult{}, err
	}
	return memory.MutationResult{Record: cloneRecord(next), SubjectGeneration: w.subject.Generation, PolicyGeneration: next.PolicyGeneration}, nil
}

func (w *memoryWriteTx) commitRevise(ctx context.Context) (memory.MutationResult, error) {
	cmd := w.cmd
	current, err := w.record(ctx, cmd.MemoryID)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if current == nil {
		return memory.MutationResult{}, memory.ErrNotFound
	}
	if current.Status != memory.StatusActive || current.Content == "" || current.Expired(w.now) || current.SubjectGeneration != w.subject.Generation || current.Revision != cmd.Preconditions.ExpectedRevision {
		return memory.MutationResult{}, memory.ErrConflict
	}
	if current.Kind == memory.KindVerifiedFact {
		return memory.MutationResult{}, fmt.Errorf("%w: verified facts require a trusted revision capability", memory.ErrUnauthorized)
	}
	if current.Provenance.SourceRef != "" {
		source, err := sourceFor(ctx, w.conn, cmd.Scope, current.Provenance.SourceRef)
		if err != nil {
			return memory.MutationResult{}, err
		}
		if !sourceEligible(current, source, w.now) {
			return memory.MutationResult{}, fmt.Errorf("%w: source is no longer eligible", memory.ErrConflict)
		}
	}
	if err := validateStoredContent(cmd.Content, cmd.Topics, cmd.Retention, w.policy.Limits); err != nil {
		return memory.MutationResult{}, err
	}
	delta := len(cmd.Content) - len(current.Content)
	if w.collection.Bytes+delta > w.policy.Limits.MaxContentPerScope {
		return memory.MutationResult{}, memory.ErrQuota
	}
	next := cloneRecord(current)
	next.Revision++
	next.Content = cmd.Content
	next.Topics = append([]string(nil), cmd.Topics...)
	next.PolicyGeneration = w.policy.Generation
	next.Provenance = memory.Provenance{Producer: cmd.Audit.Issuer, PolicyVersion: fmt.Sprint(w.policy.Generation)}
	next.Decision = memory.Decision{Actor: cmd.Audit.Actor, Basis: string(memory.ActionRevise), DecidedAt: w.now}
	next.UpdatedAt = w.now
	next.ExpiresAt = w.now.Add(cmd.Retention)
	if err := validateRecordJSON(next, w.policy.Limits.MaxRecordBytes); err != nil {
		return memory.MutationResult{}, err
	}
	if err := w.writeRecord(ctx, next); err != nil {
		return memory.MutationResult{}, err
	}
	w.collection.Bytes += delta
	if err := w.writeCollection(ctx, w.collection); err != nil {
		return memory.MutationResult{}, err
	}
	return memory.MutationResult{Record: cloneRecord(next), SubjectGeneration: w.subject.Generation, PolicyGeneration: next.PolicyGeneration}, nil
}

func (w *memoryWriteTx) commitRetract(ctx context.Context) (memory.MutationResult, error) {
	cmd := w.cmd
	current, err := w.record(ctx, cmd.MemoryID)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if current == nil {
		return memory.MutationResult{}, memory.ErrNotFound
	}
	if current.Status == memory.StatusRetracted || current.Content == "" || current.SubjectGeneration != w.subject.Generation || current.Revision != cmd.Preconditions.ExpectedRevision {
		return memory.MutationResult{}, memory.ErrConflict
	}
	next := cloneRecord(current)
	next.Revision++
	next.Status = memory.StatusRetracted
	next.UpdatedAt = w.now
	if err := w.writeRecord(ctx, next); err != nil {
		return memory.MutationResult{}, err
	}
	w.collection.ProposalEpoch++
	if err := w.writeCollection(ctx, w.collection); err != nil {
		return memory.MutationResult{}, err
	}
	if err := insertSQLiteLifecycleJob(ctx, w.conn, memory.PurgeJob{TaskID: memory.LifecycleTaskID(memory.PurgeJobRetract, cmd.Subject, cmd.Scope, "", next.MemoryID, next.Revision, w.subject.Generation), Kind: memory.PurgeJobRetract, Subject: cmd.Subject, Scope: cmd.Scope, MemoryID: next.MemoryID, ExpectedRevision: next.Revision, PurgeBeforeGeneration: w.subject.Generation, SelectionCutoff: w.now, CreatedAt: w.now}); err != nil {
		return memory.MutationResult{}, err
	}
	return memory.MutationResult{Record: cloneRecord(next), SubjectGeneration: w.subject.Generation, PolicyGeneration: next.PolicyGeneration}, nil
}

func (w *memoryWriteTx) commitForget(ctx context.Context) (memory.MutationResult, error) {
	next := sqlSubjectState{Generation: w.subject.Generation + 1, Disabled: true}
	if err := w.writeSubject(ctx, next); err != nil {
		return memory.MutationResult{}, err
	}
	if err := insertSQLiteLifecycleJob(ctx, w.conn, memory.PurgeJob{TaskID: memory.LifecycleTaskID(memory.PurgeJobForget, w.cmd.Subject, memory.Scope{}, "", "", 0, w.subject.Generation), Kind: memory.PurgeJobForget, Subject: w.cmd.Subject, PurgeBeforeGeneration: w.subject.Generation, SelectionCutoff: w.now, CreatedAt: w.now}); err != nil {
		return memory.MutationResult{}, err
	}
	w.subject = next
	return memory.MutationResult{SubjectGeneration: next.Generation, PolicyGeneration: w.policy.Generation}, nil
}

func (w *memoryWriteTx) commitEnable(ctx context.Context) (memory.MutationResult, error) {
	if !w.subject.Disabled || w.subject.Generation == 0 {
		return memory.MutationResult{}, memory.ErrConflict
	}
	w.subject.Generation++
	w.subject.Disabled = false
	if err := w.writeSubject(ctx, w.subject); err != nil {
		return memory.MutationResult{}, err
	}
	return memory.MutationResult{SubjectGeneration: w.subject.Generation, PolicyGeneration: w.policy.Generation}, nil
}

func (w *memoryWriteTx) commitRetractSource(ctx context.Context) (memory.MutationResult, error) {
	cmd := w.cmd
	if cmd.SourceRef == "" || cmd.Preconditions.SourceEpoch == 0 {
		return memory.MutationResult{}, memory.ErrInvalid
	}
	source, err := sourceFor(ctx, w.conn, cmd.Scope, cmd.SourceRef)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if source.Epoch == 0 {
		return memory.MutationResult{}, memory.ErrNotFound
	}
	if source.Revoked || source.Epoch != cmd.Preconditions.SourceEpoch {
		return memory.MutationResult{}, memory.ErrConflict
	}
	oldEpoch := source.Epoch
	source.Epoch++
	source.Revoked = true
	if err := w.writeSource(ctx, cmd.SourceRef, source); err != nil {
		return memory.MutationResult{}, err
	}
	if err := insertSQLiteLifecycleJob(ctx, w.conn, memory.PurgeJob{TaskID: memory.LifecycleTaskID(memory.PurgeJobRetractSource, cmd.Subject, cmd.Scope, cmd.SourceRef, "", oldEpoch, w.subject.Generation), Kind: memory.PurgeJobRetractSource, Subject: cmd.Subject, Scope: cmd.Scope, SourceRef: cmd.SourceRef, SourceEpoch: oldEpoch, PurgeBeforeGeneration: w.subject.Generation, SelectionCutoff: w.now, CreatedAt: w.now}); err != nil {
		return memory.MutationResult{}, err
	}
	return memory.MutationResult{SubjectGeneration: w.subject.Generation, PolicyGeneration: w.policy.Generation}, nil
}

func (w *memoryWriteTx) commitPolicy(ctx context.Context) (memory.MutationResult, error) {
	update := w.cmd.PolicyUpdate
	if update == nil || memory.ValidateLimits(update.Limits) != nil {
		return memory.MutationResult{}, memory.ErrInvalid
	}
	if update.ExpectedGeneration != w.policy.Generation {
		return memory.MutationResult{}, fmt.Errorf("%w: policy generation", memory.ErrConflict)
	}
	raw, err := json.Marshal(update.Limits)
	if err != nil {
		return memory.MutationResult{}, err
	}
	next := w.policy.Generation + 1
	_, err = w.conn.ExecContext(ctx, `INSERT INTO agent_runtime_memory_policies(namespace,project_id,generation,limits_json) VALUES(?,?,?,?) ON CONFLICT(namespace,project_id) DO UPDATE SET generation=excluded.generation,limits_json=excluded.limits_json`, w.cmd.Scope.Namespace, w.cmd.Scope.ProjectID, next, string(raw))
	if err != nil {
		return memory.MutationResult{}, mapSQLiteError(err)
	}
	return memory.MutationResult{SubjectGeneration: w.subject.Generation, PolicyGeneration: next}, nil
}

func (w *memoryWriteTx) loadPurgeLease(ctx context.Context) (memory.PurgeLease, bool, error) {
	var out memory.PurgeLease
	var leaseExpires, cutoff int64
	err := w.conn.QueryRowContext(ctx, `SELECT task_id,owner_token,lease_epoch,lease_expires_at_ns,selection_cutoff_ns,purge_before_generation,cursor FROM agent_runtime_memory_purge_jobs WHERE namespace=? AND project_id=? AND subject_id=?`, w.cmd.Subject.Namespace, w.cmd.Subject.ProjectID, w.cmd.Subject.SubjectID).Scan(&out.TaskID, &out.OwnerToken, &out.LeaseEpoch, &leaseExpires, &cutoff, &out.PurgeBeforeGeneration, &out.Cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return memory.PurgeLease{}, false, nil
	}
	if err != nil {
		return memory.PurgeLease{}, false, mapSQLiteError(err)
	}
	out.LeaseExpiresAt, out.SelectionCutoff = fromNS(leaseExpires), fromNS(cutoff)
	return out, true, nil
}

func (w *memoryWriteTx) writePurgeLease(ctx context.Context, lease memory.PurgeLease) error {
	_, err := w.conn.ExecContext(ctx, `INSERT INTO agent_runtime_memory_purge_jobs(namespace,project_id,subject_id,task_id,owner_token,lease_epoch,lease_expires_at_ns,selection_cutoff_ns,purge_before_generation,cursor) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(namespace,project_id,subject_id) DO UPDATE SET task_id=excluded.task_id,owner_token=excluded.owner_token,lease_epoch=excluded.lease_epoch,lease_expires_at_ns=excluded.lease_expires_at_ns,selection_cutoff_ns=excluded.selection_cutoff_ns,purge_before_generation=excluded.purge_before_generation,cursor=excluded.cursor`, w.cmd.Subject.Namespace, w.cmd.Subject.ProjectID, w.cmd.Subject.SubjectID, lease.TaskID, lease.OwnerToken, lease.LeaseEpoch, toNS(lease.LeaseExpiresAt), toNS(lease.SelectionCutoff), lease.PurgeBeforeGeneration, lease.Cursor)
	return mapSQLiteError(err)
}

func (w *memoryWriteTx) commitClaimPurge(ctx context.Context) (memory.MutationResult, error) {
	requested := w.cmd.PurgeLease
	if requested.TaskID == "" || requested.OwnerToken == "" || requested.PurgeBeforeGeneration == 0 || requested.PurgeBeforeGeneration > w.subject.Generation || w.cmd.LeaseDuration <= 0 || w.cmd.LeaseDuration > time.Hour {
		return memory.MutationResult{}, memory.ErrInvalid
	}
	current, exists, err := w.loadPurgeLease(ctx)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if exists && current.TaskID == requested.TaskID && current.PurgeBeforeGeneration != requested.PurgeBeforeGeneration {
		return memory.MutationResult{}, fmt.Errorf("%w: purge generation cutoff", memory.ErrConflict)
	}
	if exists && current.LeaseExpiresAt.After(w.now) && (current.TaskID != requested.TaskID || current.OwnerToken != requested.OwnerToken || current.PurgeBeforeGeneration != requested.PurgeBeforeGeneration) {
		return memory.MutationResult{}, fmt.Errorf("%w: active purge lease", memory.ErrConflict)
	}
	if exists && !current.LeaseExpiresAt.After(w.now) && current.TaskID != requested.TaskID {
		current, exists = memory.PurgeLease{}, false
	}
	epoch := current.LeaseEpoch + 1
	if epoch == 0 {
		epoch = 1
	}
	selection := current.SelectionCutoff
	if !exists {
		selection = w.now
	}
	lease := memory.PurgeLease{TaskID: requested.TaskID, OwnerToken: requested.OwnerToken, LeaseEpoch: epoch, LeaseExpiresAt: w.now.Add(w.cmd.LeaseDuration), SelectionCutoff: selection, PurgeBeforeGeneration: requested.PurgeBeforeGeneration, Cursor: current.Cursor}
	if err := w.writePurgeLease(ctx, lease); err != nil {
		return memory.MutationResult{}, err
	}
	return memory.MutationResult{SubjectGeneration: w.subject.Generation, PolicyGeneration: w.policy.Generation, PurgeLease: lease}, nil
}

func (w *memoryWriteTx) validatePurgeLease(ctx context.Context, provided memory.PurgeLease, requireCursor bool) (memory.PurgeLease, error) {
	if provided.TaskID == "" || provided.OwnerToken == "" || provided.LeaseEpoch == 0 || provided.SelectionCutoff.IsZero() {
		return memory.PurgeLease{}, memory.ErrInvalid
	}
	current, ok, err := w.loadPurgeLease(ctx)
	if err != nil {
		return memory.PurgeLease{}, err
	}
	if !ok || current.TaskID != provided.TaskID || current.OwnerToken != provided.OwnerToken || current.LeaseEpoch != provided.LeaseEpoch || current.PurgeBeforeGeneration != provided.PurgeBeforeGeneration || !current.SelectionCutoff.Equal(provided.SelectionCutoff) || !current.LeaseExpiresAt.Equal(provided.LeaseExpiresAt) || !current.LeaseExpiresAt.After(w.now) || (requireCursor && current.Cursor != provided.Cursor) {
		return memory.PurgeLease{}, memory.ErrConflict
	}
	return current, nil
}

func (w *memoryWriteTx) commitPurge(ctx context.Context) (memory.MutationResult, error) {
	lease, err := w.validatePurgeLease(ctx, w.cmd.PurgeLease, true)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if len(w.cmd.PurgeTargets) == 0 || len(w.cmd.PurgeTargets) > 100 || w.cmd.PurgeCursor == "" {
		return memory.MutationResult{}, memory.ErrInvalid
	}
	targets := append([]memory.PurgeTarget(nil), w.cmd.PurgeTargets...)
	if !sort.SliceIsSorted(targets, func(i, j int) bool { return targets[i].MemoryID < targets[j].MemoryID }) {
		return memory.MutationResult{}, memory.ErrInvalid
	}
	for i := 1; i < len(targets); i++ {
		if targets[i-1].MemoryID == targets[i].MemoryID {
			return memory.MutationResult{}, fmt.Errorf("%w: duplicate purge target", memory.ErrInvalid)
		}
	}
	records := make([]*memory.Record, 0, len(targets))
	for _, target := range targets {
		if target.MemoryID == "" || target.ExpectedRevision == 0 || !target.SelectedBefore.Equal(lease.SelectionCutoff) {
			return memory.MutationResult{}, memory.ErrInvalid
		}
		record, err := w.record(ctx, target.MemoryID)
		if err != nil {
			return memory.MutationResult{}, err
		}
		if record == nil || record.Revision != target.ExpectedRevision || record.SubjectGeneration > lease.PurgeBeforeGeneration {
			return memory.MutationResult{}, memory.ErrConflict
		}
		if record.Content != "" && (record.UpdatedAt.After(target.SelectedBefore) || (record.Status != memory.StatusRetracted && record.ExpiresAt.After(target.SelectedBefore) && record.SubjectGeneration >= w.subject.Generation)) {
			return memory.MutationResult{}, memory.ErrConflict
		}
		records = append(records, record)
	}
	result := memory.MutationResult{SubjectGeneration: w.subject.Generation, PolicyGeneration: w.policy.Generation, PurgeLease: lease}
	for _, record := range records {
		if record.Content == "" {
			continue
		}
		w.collection.Bytes -= len(record.Content)
		w.collection.Records--
		result.PurgedRecords++
		result.ReleasedBytes += len(record.Content)
		next := cloneRecord(record)
		next.Content, next.Topics, next.UpdatedAt = "", nil, w.now
		if err := w.writeRecord(ctx, next); err != nil {
			return memory.MutationResult{}, err
		}
	}
	if err := w.writeCollection(ctx, w.collection); err != nil {
		return memory.MutationResult{}, err
	}
	lease.Cursor = w.cmd.PurgeCursor
	if err := w.writePurgeLease(ctx, lease); err != nil {
		return memory.MutationResult{}, err
	}
	result.PurgeLease = lease
	return result, nil
}

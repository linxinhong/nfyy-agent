package postgres

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

func (s *MemoryStore) CommitMutation(ctx context.Context, cmd memory.Mutation) (memory.MutationResult, error) {
	if cmd.Attestation != nil {
		att := memory.CanonicalSourceAttestation(*cmd.Attestation)
		cmd.Attestation = &att
	}
	if err := validateMutationBindings(cmd); err != nil {
		return memory.MutationResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return memory.MutationResult{}, wrapStoreErr(err)
	}
	defer tx.Rollback()
	now, err := s.storeNow(ctx, tx)
	if err != nil {
		return memory.MutationResult{}, wrapStoreErr(err)
	}
	if cmd.Operation.IssuedAt.After(now) || !cmd.Operation.ExpiresAt.After(now) || cmd.Operation.ExpiresAt.Sub(cmd.Operation.IssuedAt) > 7*24*time.Hour {
		return memory.MutationResult{}, memory.ErrExpired
	}

	// Stable global lock order: shared project policy guard, subject guard,
	// collection guard, source/records (inside mutation), then receipt/audit.
	policy, err := s.ensurePolicy(ctx, tx, cmd.Subject.Namespace, cmd.Subject.ProjectID, cmd.Type == memory.MutationUpdatePolicy)
	if err != nil {
		return memory.MutationResult{}, err
	}
	state, err := ensureSubject(ctx, tx, cmd.Subject)
	if err != nil {
		return memory.MutationResult{}, err
	}
	var collection memoryCollectionState
	if cmd.Type != memory.MutationForget && cmd.Type != memory.MutationEnableSubject {
		collection, err = ensureCollection(ctx, tx, cmd.Scope)
		if err != nil {
			return memory.MutationResult{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM agent_runtime_memory_receipts WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4 AND expires_at<=$5`, cmd.Subject.Namespace, cmd.Subject.ProjectID, cmd.Subject.SubjectID, receiptCollection(cmd), now); err != nil {
		return memory.MutationResult{}, wrapStoreErr(err)
	}
	replayed, found, err := readReceipt(ctx, tx, cmd)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if found && !bytes.Equal(replayed.digest, cmd.Operation.CommandDigest) {
		return memory.MutationResult{}, fmt.Errorf("%w: idempotency command mismatch", memory.ErrConflict)
	}
	if found && cmd.Type == memory.MutationUpdatePolicy && replayed.result.PolicyGeneration == policy.Generation && cmd.Preconditions.PolicyGeneration+1 == policy.Generation && cmd.Operation.SubjectGeneration == state.Generation && cmd.Preconditions.SubjectGeneration == state.Generation {
		return replay(replayed.result), nil
	}
	if cmd.Preconditions.PolicyGeneration != policy.Generation {
		return memory.MutationResult{}, fmt.Errorf("%w: policy generation", memory.ErrConflict)
	}
	if found && cmd.Type == memory.MutationForget && replayed.result.SubjectGeneration == state.Generation && cmd.Operation.SubjectGeneration+1 == state.Generation {
		return replay(replayed.result), nil
	}
	if found && cmd.Type == memory.MutationEnableSubject && replayed.result.SubjectGeneration == state.Generation && cmd.Operation.SubjectGeneration+1 == state.Generation {
		return replay(replayed.result), nil
	}
	if state.Generation != cmd.Preconditions.SubjectGeneration || cmd.Operation.SubjectGeneration != cmd.Preconditions.SubjectGeneration {
		return memory.MutationResult{}, fmt.Errorf("%w: subject generation", memory.ErrConflict)
	}
	if found {
		return replay(replayed.result), nil
	}
	if state.Disabled && !disabledMutationAllowed(cmd.Type) {
		return memory.MutationResult{}, fmt.Errorf("%w: subject disabled", memory.ErrConflict)
	}
	if (cmd.Type == memory.MutationCreate || cmd.Type == memory.MutationCreateCandidateBatch || cmd.Type == memory.MutationConfirmCandidate) && collection.ProposalEpoch != cmd.Preconditions.CollectionProposalEpoch {
		return memory.MutationResult{}, fmt.Errorf("%w: collection proposal epoch", memory.ErrConflict)
	}
	var receiptCount int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_runtime_memory_receipts WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4`, cmd.Subject.Namespace, cmd.Subject.ProjectID, cmd.Subject.SubjectID, receiptCollection(cmd)).Scan(&receiptCount); err != nil {
		return memory.MutationResult{}, wrapStoreErr(err)
	}
	if receiptCount >= policy.Limits.MaxReceiptsPerScope {
		return memory.MutationResult{}, memory.ErrQuota
	}

	var result memory.MutationResult
	switch cmd.Type {
	case memory.MutationCreate, memory.MutationAttestCreate:
		result, err = s.commitCreate(ctx, tx, cmd, now, state.Generation, policy, collection)
	case memory.MutationCreateCandidateBatch:
		result, err = s.commitCandidateBatch(ctx, tx, cmd, now, state.Generation, policy, collection)
	case memory.MutationConfirmCandidate:
		result, err = s.commitConfirm(ctx, tx, cmd, now, state.Generation, policy)
	case memory.MutationRevise:
		result, err = s.commitRevise(ctx, tx, cmd, now, state.Generation, policy, collection)
	case memory.MutationRetract:
		result, err = s.commitRetract(ctx, tx, cmd, now, state.Generation)
	case memory.MutationForget:
		result, err = s.commitForget(ctx, tx, cmd, now, state.Generation)
	case memory.MutationEnableSubject:
		result, err = s.commitEnable(ctx, tx, cmd, state, policy)
	case memory.MutationRetractSource:
		result, err = s.commitRetractSource(ctx, tx, cmd, now, state.Generation, policy)
	case memory.MutationUpdatePolicy:
		result, err = s.commitPolicy(ctx, tx, cmd, state.Generation, policy)
	case memory.MutationClaimPurgeLease:
		result, err = s.commitClaimPurge(ctx, tx, cmd, now, state.Generation, policy)
	case memory.MutationPurge:
		result, err = s.commitPurge(ctx, tx, cmd, now, state.Generation, policy, collection)
	default:
		err = fmt.Errorf("%w: mutation type", memory.ErrInvalid)
	}
	if err != nil {
		return memory.MutationResult{}, err
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
	s.mu.RLock()
	auditFailure := s.auditFailure
	s.mu.RUnlock()
	if auditFailure != nil {
		if err := auditFailure(cmd.Audit); err != nil {
			return memory.MutationResult{}, fmt.Errorf("%w: %v", memory.ErrAudit, err)
		}
	}
	if err := insertAudit(ctx, tx, cmd.Audit); err != nil {
		return memory.MutationResult{}, err
	}
	if err := insertReceipt(ctx, tx, cmd, result); err != nil {
		return memory.MutationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return memory.MutationResult{}, fmt.Errorf("%w: %v", memory.ErrCommitUnknown, err)
	}
	s.mu.RLock()
	after := s.afterCommitFailure
	s.mu.RUnlock()
	if after != nil {
		if err := after(); err != nil {
			return memory.MutationResult{}, fmt.Errorf("%w: %v", memory.ErrCommitUnknown, err)
		}
	}
	return result, nil
}

func validateMutationBindings(cmd memory.Mutation) error {
	if err := cmd.Subject.Validate(); err != nil {
		return err
	}
	subjectMutation := cmd.Type == memory.MutationForget || cmd.Type == memory.MutationEnableSubject
	if !subjectMutation {
		if err := cmd.Scope.Validate(); err != nil {
			return err
		}
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

func (s *MemoryStore) ensurePolicy(ctx context.Context, tx *sql.Tx, namespace, project string, exclusive bool) (memoryPolicyState, error) {
	raw, err := json.Marshal(s.configuredLimits())
	if err != nil {
		return memoryPolicyState{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_runtime_memory_policies(namespace,project_id,generation,limits) VALUES($1,$2,1,$3::jsonb) ON CONFLICT DO NOTHING`, namespace, project, string(raw)); err != nil {
		return memoryPolicyState{}, wrapStoreErr(err)
	}
	query := `SELECT generation,limits FROM agent_runtime_memory_policies WHERE namespace=$1 AND project_id=$2 FOR SHARE`
	if exclusive {
		query = `SELECT generation,limits FROM agent_runtime_memory_policies WHERE namespace=$1 AND project_id=$2 FOR UPDATE`
	}
	var state memoryPolicyState
	var encoded []byte
	if err := tx.QueryRowContext(ctx, query, namespace, project).Scan(&state.Generation, &encoded); err != nil {
		return state, wrapStoreErr(err)
	}
	if err := json.Unmarshal(encoded, &state.Limits); err != nil {
		return state, wrapStoreErr(err)
	}
	if err := memory.ValidateLimits(state.Limits); err != nil {
		return state, fmt.Errorf("%w: invalid persisted memory policy", memory.ErrUnavailable)
	}
	return state, nil
}

func ensureSubject(ctx context.Context, tx *sql.Tx, subject memory.SubjectScope) (memorySubjectState, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_runtime_memory_subjects(namespace,project_id,subject_id,generation,disabled) VALUES($1,$2,$3,1,false) ON CONFLICT DO NOTHING`, subject.Namespace, subject.ProjectID, subject.SubjectID); err != nil {
		return memorySubjectState{}, wrapStoreErr(err)
	}
	return readSubject(ctx, tx, subject, true)
}

func ensureCollection(ctx context.Context, tx *sql.Tx, scope memory.Scope) (memoryCollectionState, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_runtime_memory_collections(namespace,project_id,subject_id,collection_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID); err != nil {
		return memoryCollectionState{}, wrapStoreErr(err)
	}
	return readCollection(ctx, tx, scope, true)
}

type receiptRow struct {
	digest []byte
	result memory.MutationResult
}

func receiptCollection(cmd memory.Mutation) string {
	if cmd.Type == memory.MutationForget || cmd.Type == memory.MutationEnableSubject {
		return ""
	}
	return cmd.Scope.CollectionID
}
func readReceipt(ctx context.Context, tx *sql.Tx, cmd memory.Mutation) (receiptRow, bool, error) {
	var row receiptRow
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT command_digest,result FROM agent_runtime_memory_receipts WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4 AND action=$5 AND operation_id=$6 FOR UPDATE`, cmd.Subject.Namespace, cmd.Subject.ProjectID, cmd.Subject.SubjectID, receiptCollection(cmd), cmd.Operation.Action, cmd.Operation.ID).Scan(&row.digest, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return row, false, nil
	}
	if err != nil {
		return row, false, wrapStoreErr(err)
	}
	if err := json.Unmarshal(raw, &row.result); err != nil {
		return row, false, wrapStoreErr(err)
	}
	return row, true, nil
}
func replay(r memory.MutationResult) memory.MutationResult { r.Replayed = true; return r }

func insertReceipt(ctx context.Context, tx *sql.Tx, cmd memory.Mutation, result memory.MutationResult) error {
	raw, err := json.Marshal(receiptResult(result))
	if err != nil {
		return wrapStoreErr(err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_runtime_memory_receipts(namespace,project_id,subject_id,collection_id,action,operation_id,command_digest,expires_at,result) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb)`, cmd.Subject.Namespace, cmd.Subject.ProjectID, cmd.Subject.SubjectID, receiptCollection(cmd), cmd.Operation.Action, cmd.Operation.ID, cmd.Operation.CommandDigest, cmd.Operation.ExpiresAt, string(raw))
	return wrapStoreErr(err)
}
func insertAudit(ctx context.Context, tx *sql.Tx, a memory.AuditEvent) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO agent_runtime_memory_audits(namespace,project_id,subject_id,collection_id,operation_id,action,actor,issuer,memory_id,revision,generation,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, a.Subject.Namespace, a.Subject.ProjectID, a.Subject.SubjectID, a.Scope.CollectionID, a.OperationID, a.Action, a.Actor, a.Issuer, a.MemoryID, a.Revision, a.Generation, a.OccurredAt)
	return wrapStoreErr(err)
}

func scopeArgs(scope memory.Scope) []any {
	return []any{scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID}
}

func sourceCount(ctx context.Context, tx *sql.Tx, scope memory.Scope) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_runtime_memory_sources WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4`, scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID).Scan(&n)
	return n, wrapStoreErr(err)
}

func enqueueSupersededPostgresSource(ctx context.Context, tx *sql.Tx, cmd memory.Mutation, now time.Time, generation, oldEpoch, newEpoch uint64) error {
	if oldEpoch == 0 || oldEpoch >= newEpoch {
		return nil
	}
	return insertPostgresLifecycleJob(ctx, tx, memory.PurgeJob{
		TaskID:                memory.LifecycleTaskID(memory.PurgeJobRetractSource, cmd.Subject, cmd.Scope, cmd.Attestation.SourceRef, "", oldEpoch, generation),
		Kind:                  memory.PurgeJobRetractSource,
		Subject:               cmd.Subject,
		Scope:                 cmd.Scope,
		SourceRef:             cmd.Attestation.SourceRef,
		SourceEpoch:           oldEpoch,
		PurgeBeforeGeneration: generation,
		SelectionCutoff:       now,
		CreatedAt:             now,
	})
}

func (s *MemoryStore) commitCreate(ctx context.Context, tx *sql.Tx, cmd memory.Mutation, now time.Time, generation uint64, policy memoryPolicyState, collection memoryCollectionState) (memory.MutationResult, error) {
	if cmd.MemoryID == "" {
		return memory.MutationResult{}, memory.ErrConflict
	}
	if err := validateStoredContent(cmd.Content, cmd.Topics, cmd.Retention, policy.Limits); err != nil {
		return memory.MutationResult{}, err
	}
	if prior, err := readRecordForUpdate(ctx, tx, cmd.Scope, cmd.MemoryID); err != nil {
		return memory.MutationResult{}, err
	} else if prior != nil {
		return memory.MutationResult{}, memory.ErrConflict
	}
	sources, err := sourceCount(ctx, tx, cmd.Scope)
	if err != nil {
		return memory.MutationResult{}, err
	}
	newSource := 0
	oldSourceEpoch := uint64(0)
	provenance := memory.Provenance{Producer: cmd.Audit.Issuer, PolicyVersion: policyVersion(policy.Generation)}
	if cmd.Type == memory.MutationAttestCreate {
		att := cmd.Attestation
		if att == nil || att.Scope != cmd.Scope || att.Validate() != nil || !att.ExpiresAt.After(now) {
			return memory.MutationResult{}, memory.ErrInvalid
		}
		current, found, err := readSource(ctx, tx, cmd.Scope, att.SourceRef, true)
		if err != nil {
			return memory.MutationResult{}, err
		}
		if !found {
			newSource = 1
		}
		oldSourceEpoch = current.Epoch
		if current.Epoch > att.SourceEpoch || (current.Epoch == att.SourceEpoch && current.Epoch != 0 && (current.Version != att.Version || current.Digest != att.Digest || current.Verifier != att.Verifier || current.Revoked || !sameInstant(current.ExpiresAt, att.ExpiresAt))) {
			return memory.MutationResult{}, memory.ErrConflict
		}
		provenance = memory.Provenance{SourceRef: att.SourceRef, SourceVersion: att.Version, SourceEpoch: att.SourceEpoch, Producer: att.Verifier, PolicyVersion: policyVersion(policy.Generation)}
	}
	if collection.Records+1 > policy.Limits.MaxRecordsPerScope || collection.Bytes+len(cmd.Content) > policy.Limits.MaxContentPerScope || sources+collection.RecordIdentities+collection.CandidateBatches+1+newSource > policy.Limits.MaxControlsPerScope {
		return memory.MutationResult{}, memory.ErrQuota
	}
	r := &memory.Record{Scope: cmd.Scope, MemoryID: cmd.MemoryID, Revision: 1, SubjectGeneration: generation, PolicyGeneration: policy.Generation, Kind: cmd.Kind, Content: cmd.Content, Topics: append([]string(nil), cmd.Topics...), Status: memory.StatusActive, Provenance: provenance, Decision: memory.Decision{Actor: cmd.Audit.Actor, Basis: string(cmd.Audit.Action), DecidedAt: now}, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(cmd.Retention)}
	if cmd.Attestation != nil && r.ExpiresAt.After(cmd.Attestation.ExpiresAt) {
		r.ExpiresAt = cmd.Attestation.ExpiresAt
	}
	if err := validateRecordJSON(r, policy.Limits.MaxRecordBytes); err != nil {
		return memory.MutationResult{}, err
	}
	if cmd.Type == memory.MutationAttestCreate {
		if err := enqueueSupersededPostgresSource(ctx, tx, cmd, now, generation, oldSourceEpoch, cmd.Attestation.SourceEpoch); err != nil {
			return memory.MutationResult{}, err
		}
		if err := upsertSource(ctx, tx, *cmd.Attestation); err != nil {
			return memory.MutationResult{}, err
		}
	}
	if err := insertRecord(ctx, tx, r); err != nil {
		return memory.MutationResult{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE agent_runtime_memory_collections SET records_count=records_count+1,content_bytes=content_bytes+$5,record_identities=record_identities+1 WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4`, cmd.Scope.Namespace, cmd.Scope.ProjectID, cmd.Scope.SubjectID, cmd.Scope.CollectionID, len(cmd.Content))
	if err != nil {
		return memory.MutationResult{}, wrapStoreErr(err)
	}
	return memory.MutationResult{Record: r, SubjectGeneration: generation, PolicyGeneration: policy.Generation}, nil
}

func upsertSource(ctx context.Context, tx *sql.Tx, att memory.SourceAttestation) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO agent_runtime_memory_sources(namespace,project_id,subject_id,collection_id,source_ref,source_version,source_digest,source_epoch,expires_at,verifier,revoked) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,false) ON CONFLICT(namespace,project_id,subject_id,collection_id,source_ref) DO UPDATE SET source_version=EXCLUDED.source_version,source_digest=EXCLUDED.source_digest,source_epoch=EXCLUDED.source_epoch,expires_at=EXCLUDED.expires_at,verifier=EXCLUDED.verifier,revoked=false`, att.Scope.Namespace, att.Scope.ProjectID, att.Scope.SubjectID, att.Scope.CollectionID, att.SourceRef, att.Version, att.Digest, att.SourceEpoch, att.ExpiresAt, att.Verifier)
	return wrapStoreErr(err)
}

func insertRecord(ctx context.Context, tx *sql.Tx, r *memory.Record) error {
	topics, err := json.Marshal(r.Topics)
	if err != nil {
		return err
	}
	var decided any
	if !r.Decision.DecidedAt.IsZero() {
		decided = r.Decision.DecidedAt
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_runtime_memory_records(namespace,project_id,subject_id,collection_id,memory_id,revision,subject_generation,policy_generation,proposal_epoch,kind,content,topics,status,source_ref,source_version,source_epoch,producer,policy_version,decision_actor,decision_basis,decided_at,created_at,updated_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::jsonb,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)`, r.Scope.Namespace, r.Scope.ProjectID, r.Scope.SubjectID, r.Scope.CollectionID, r.MemoryID, r.Revision, r.SubjectGeneration, r.PolicyGeneration, r.CollectionProposalEpoch, r.Kind, r.Content, string(topics), r.Status, r.Provenance.SourceRef, r.Provenance.SourceVersion, r.Provenance.SourceEpoch, r.Provenance.Producer, r.Provenance.PolicyVersion, r.Decision.Actor, r.Decision.Basis, decided, r.CreatedAt, r.UpdatedAt, r.ExpiresAt)
	return wrapStoreErr(err)
}

func updateRecord(ctx context.Context, tx *sql.Tx, r *memory.Record, expected uint64) error {
	topics, err := json.Marshal(r.Topics)
	if err != nil {
		return err
	}
	var decided any
	if !r.Decision.DecidedAt.IsZero() {
		decided = r.Decision.DecidedAt
	}
	res, err := tx.ExecContext(ctx, `UPDATE agent_runtime_memory_records SET revision=$6,subject_generation=$7,policy_generation=$8,proposal_epoch=$9,kind=$10,content=$11,topics=$12::jsonb,status=$13,source_ref=$14,source_version=$15,source_epoch=$16,producer=$17,policy_version=$18,decision_actor=$19,decision_basis=$20,decided_at=$21,updated_at=$22,expires_at=$23 WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4 AND memory_id=$5 AND revision=$24`, r.Scope.Namespace, r.Scope.ProjectID, r.Scope.SubjectID, r.Scope.CollectionID, r.MemoryID, r.Revision, r.SubjectGeneration, r.PolicyGeneration, r.CollectionProposalEpoch, r.Kind, r.Content, string(topics), r.Status, r.Provenance.SourceRef, r.Provenance.SourceVersion, r.Provenance.SourceEpoch, r.Provenance.Producer, r.Provenance.PolicyVersion, r.Decision.Actor, r.Decision.Basis, decided, r.UpdatedAt, r.ExpiresAt, expected)
	if err != nil {
		return wrapStoreErr(err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return memory.ErrConflict
	}
	return nil
}

func cloneRecord(r *memory.Record) *memory.Record {
	if r == nil {
		return nil
	}
	out := *r
	out.Topics = append([]string(nil), r.Topics...)
	return &out
}

func (s *MemoryStore) commitCandidateBatch(ctx context.Context, tx *sql.Tx, cmd memory.Mutation, now time.Time, generation uint64, policy memoryPolicyState, collection memoryCollectionState) (memory.MutationResult, error) {
	batch, att := cmd.CandidateBatch, cmd.Attestation
	if batch == nil || batch.Validate() != nil || batch.Producer != cmd.Audit.Issuer || att == nil || att.Scope != cmd.Scope || att.SourceRef != batch.SourceRef || att.Version != batch.SourceVersion || att.Digest == "" || att.SourceEpoch == 0 || cmd.Preconditions.SourceEpoch != att.SourceEpoch || att.Verifier == "" || !att.ExpiresAt.After(now) || len(cmd.Candidates) == 0 || len(cmd.Candidates) > policy.Limits.MaxProposeCandidates {
		return memory.MutationResult{}, memory.ErrInvalid
	}
	current, found, err := readSource(ctx, tx, cmd.Scope, att.SourceRef, true)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if current.Epoch > att.SourceEpoch || (current.Epoch == att.SourceEpoch && current.Epoch != 0 && (current.Version != att.Version || current.Digest != att.Digest || current.Verifier != att.Verifier || current.Revoked || !sameInstant(current.ExpiresAt, att.ExpiresAt))) {
		return memory.MutationResult{}, memory.ErrConflict
	}
	var priorDigest, raw []byte
	err = tx.QueryRowContext(ctx, `SELECT command_digest,result FROM agent_runtime_memory_candidate_batches WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4 AND source_ref=$5 AND source_version=$6 AND producer=$7 AND model_config_version=$8 AND subject_generation=$9 AND policy_generation=$10 AND proposal_epoch=$11 FOR UPDATE`, cmd.Scope.Namespace, cmd.Scope.ProjectID, cmd.Scope.SubjectID, cmd.Scope.CollectionID, batch.SourceRef, batch.SourceVersion, batch.Producer, batch.ModelConfigVersion, generation, policy.Generation, cmd.Preconditions.CollectionProposalEpoch).Scan(&priorDigest, &raw)
	if err == nil {
		if !bytes.Equal(priorDigest, cmd.Operation.CommandDigest) {
			return memory.MutationResult{}, fmt.Errorf("%w: candidate batch mismatch", memory.ErrConflict)
		}
		var result memory.MutationResult
		if json.Unmarshal(raw, &result) != nil {
			return memory.MutationResult{}, memory.ErrUnavailable
		}
		return replay(result), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return memory.MutationResult{}, wrapStoreErr(err)
	}
	sources, err := sourceCount(ctx, tx, cmd.Scope)
	if err != nil {
		return memory.MutationResult{}, err
	}
	newSource := 0
	if !found {
		newSource = 1
	}
	seen := map[string]struct{}{}
	prepared := make([]*memory.Record, 0, len(cmd.Candidates))
	addedBytes := 0
	for _, draft := range cmd.Candidates {
		if draft.MemoryID == "" || draft.Kind == memory.KindVerifiedFact {
			return memory.MutationResult{}, memory.ErrInvalid
		}
		if _, ok := seen[draft.MemoryID]; ok {
			return memory.MutationResult{}, memory.ErrConflict
		}
		seen[draft.MemoryID] = struct{}{}
		if prior, err := readRecordForUpdate(ctx, tx, cmd.Scope, draft.MemoryID); err != nil {
			return memory.MutationResult{}, err
		} else if prior != nil {
			return memory.MutationResult{}, memory.ErrConflict
		}
		if err := validateStoredContent(draft.Content, draft.Topics, draft.Retention, policy.Limits); err != nil {
			return memory.MutationResult{}, err
		}
		addedBytes += len(draft.Content)
		expiry := now.Add(draft.Retention)
		if expiry.After(att.ExpiresAt) {
			expiry = att.ExpiresAt
		}
		r := &memory.Record{Scope: cmd.Scope, MemoryID: draft.MemoryID, Revision: 1, SubjectGeneration: generation, PolicyGeneration: policy.Generation, CollectionProposalEpoch: cmd.Preconditions.CollectionProposalEpoch, Kind: draft.Kind, Content: draft.Content, Topics: append([]string(nil), draft.Topics...), Status: memory.StatusCandidate, Provenance: memory.Provenance{SourceRef: att.SourceRef, SourceVersion: att.Version, SourceEpoch: att.SourceEpoch, Producer: batch.Producer, PolicyVersion: policyVersion(policy.Generation)}, CreatedAt: now, UpdatedAt: now, ExpiresAt: expiry}
		if err := validateRecordJSON(r, policy.Limits.MaxRecordBytes); err != nil {
			return memory.MutationResult{}, err
		}
		prepared = append(prepared, r)
	}
	if collection.Records+len(prepared) > policy.Limits.MaxRecordsPerScope || collection.Bytes+addedBytes > policy.Limits.MaxContentPerScope || sources+collection.RecordIdentities+collection.CandidateBatches+len(prepared)+1+newSource > policy.Limits.MaxControlsPerScope {
		return memory.MutationResult{}, memory.ErrQuota
	}
	if err := enqueueSupersededPostgresSource(ctx, tx, cmd, now, generation, current.Epoch, att.SourceEpoch); err != nil {
		return memory.MutationResult{}, err
	}
	if err := upsertSource(ctx, tx, *att); err != nil {
		return memory.MutationResult{}, err
	}
	result := memory.MutationResult{SubjectGeneration: generation, PolicyGeneration: policy.Generation}
	for _, r := range prepared {
		if err := insertRecord(ctx, tx, r); err != nil {
			return memory.MutationResult{}, err
		}
		result.Records = append(result.Records, *r)
	}
	_, err = tx.ExecContext(ctx, `UPDATE agent_runtime_memory_collections SET records_count=records_count+$5,content_bytes=content_bytes+$6,record_identities=record_identities+$5,candidate_batches=candidate_batches+1 WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4`, cmd.Scope.Namespace, cmd.Scope.ProjectID, cmd.Scope.SubjectID, cmd.Scope.CollectionID, len(prepared), addedBytes)
	if err != nil {
		return memory.MutationResult{}, wrapStoreErr(err)
	}
	encoded, err := json.Marshal(receiptResult(result))
	if err != nil {
		return memory.MutationResult{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_runtime_memory_candidate_batches(namespace,project_id,subject_id,collection_id,source_ref,source_version,producer,model_config_version,subject_generation,policy_generation,proposal_epoch,command_digest,result) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13::jsonb)`, cmd.Scope.Namespace, cmd.Scope.ProjectID, cmd.Scope.SubjectID, cmd.Scope.CollectionID, batch.SourceRef, batch.SourceVersion, batch.Producer, batch.ModelConfigVersion, generation, policy.Generation, cmd.Preconditions.CollectionProposalEpoch, cmd.Operation.CommandDigest, string(encoded))
	if err != nil {
		return memory.MutationResult{}, wrapStoreErr(err)
	}
	return result, nil
}

func (s *MemoryStore) commitConfirm(ctx context.Context, tx *sql.Tx, cmd memory.Mutation, now time.Time, generation uint64, policy memoryPolicyState) (memory.MutationResult, error) {
	observed, err := readStoredRecord(ctx, tx, cmd.Scope, cmd.MemoryID, false)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if observed == nil {
		return memory.MutationResult{}, memory.ErrNotFound
	}
	if observed.Provenance.SourceRef == "" {
		if cmd.Preconditions.SourceEpoch != 0 {
			return memory.MutationResult{}, memory.ErrConflict
		}
	} else {
		source, found, err := readSource(ctx, tx, cmd.Scope, observed.Provenance.SourceRef, true)
		if err != nil {
			return memory.MutationResult{}, err
		}
		if !sourceEligible(source, found, observed, now) || source.Epoch != cmd.Preconditions.SourceEpoch {
			return memory.MutationResult{}, memory.ErrConflict
		}
	}
	ids := []string{cmd.MemoryID}
	if cmd.ReplacedMemoryID != "" && cmd.ReplacedMemoryID != cmd.MemoryID {
		ids = append(ids, cmd.ReplacedMemoryID)
	}
	sort.Strings(ids)
	locked := make(map[string]*memory.Record, len(ids))
	for _, id := range ids {
		record, err := readRecordForUpdate(ctx, tx, cmd.Scope, id)
		if err != nil {
			return memory.MutationResult{}, err
		}
		locked[id] = record
	}
	candidate := locked[cmd.MemoryID]
	if candidate == nil || candidate.Status != memory.StatusCandidate || candidate.Revision != cmd.Preconditions.ExpectedRevision || candidate.SubjectGeneration != generation || candidate.CollectionProposalEpoch != cmd.Preconditions.CollectionProposalEpoch || candidate.Expired(now) || candidate.Provenance != observed.Provenance {
		return memory.MutationResult{}, memory.ErrConflict
	}
	if err := validateStoredContent(candidate.Content, candidate.Topics, candidate.ExpiresAt.Sub(now), policy.Limits); err != nil {
		return memory.MutationResult{}, fmt.Errorf("%w: candidate fails current policy", memory.ErrConflict)
	}
	if cmd.ReplacedMemoryID != "" {
		replaced := locked[cmd.ReplacedMemoryID]
		if replaced == nil || replaced.Status != memory.StatusActive || replaced.Revision != cmd.Preconditions.ExpectedReplacedRevision || replaced.SubjectGeneration != generation {
			return memory.MutationResult{}, memory.ErrConflict
		}
		next := cloneRecord(replaced)
		next.Revision++
		next.Status = memory.StatusRetracted
		next.UpdatedAt = now
		if err := updateRecord(ctx, tx, next, replaced.Revision); err != nil {
			return memory.MutationResult{}, err
		}
	} else if cmd.Preconditions.ExpectedReplacedRevision != 0 {
		return memory.MutationResult{}, memory.ErrInvalid
	}
	next := cloneRecord(candidate)
	next.Revision++
	next.Status = memory.StatusActive
	next.PolicyGeneration = policy.Generation
	next.Provenance.PolicyVersion = policyVersion(policy.Generation)
	next.UpdatedAt = now
	next.Decision = memory.Decision{Actor: cmd.Audit.Actor, Basis: string(memory.ActionConfirm), DecidedAt: now}
	if err := validateRecordJSON(next, policy.Limits.MaxRecordBytes); err != nil {
		return memory.MutationResult{}, fmt.Errorf("%w: candidate fails current policy", memory.ErrConflict)
	}
	if err := updateRecord(ctx, tx, next, candidate.Revision); err != nil {
		return memory.MutationResult{}, err
	}
	return memory.MutationResult{Record: next, SubjectGeneration: generation, PolicyGeneration: policy.Generation}, nil
}

func (s *MemoryStore) commitRevise(ctx context.Context, tx *sql.Tx, cmd memory.Mutation, now time.Time, generation uint64, policy memoryPolicyState, collection memoryCollectionState) (memory.MutationResult, error) {
	observed, err := readStoredRecord(ctx, tx, cmd.Scope, cmd.MemoryID, false)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if observed == nil {
		return memory.MutationResult{}, memory.ErrNotFound
	}
	if observed.Kind == memory.KindVerifiedFact {
		return memory.MutationResult{}, fmt.Errorf("%w: verified facts require a trusted revision capability", memory.ErrUnauthorized)
	}
	if observed.Provenance.SourceRef != "" {
		source, found, err := readSource(ctx, tx, cmd.Scope, observed.Provenance.SourceRef, true)
		if err != nil {
			return memory.MutationResult{}, err
		}
		if !sourceEligible(source, found, observed, now) {
			return memory.MutationResult{}, fmt.Errorf("%w: source is no longer eligible", memory.ErrConflict)
		}
	}
	current, err := readRecordForUpdate(ctx, tx, cmd.Scope, cmd.MemoryID)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if current == nil || current.Status != memory.StatusActive || current.Content == "" || current.Expired(now) || current.SubjectGeneration != generation || current.Revision != cmd.Preconditions.ExpectedRevision || current.Provenance != observed.Provenance {
		return memory.MutationResult{}, memory.ErrConflict
	}
	if err := validateStoredContent(cmd.Content, cmd.Topics, cmd.Retention, policy.Limits); err != nil {
		return memory.MutationResult{}, err
	}
	delta := len(cmd.Content) - len(current.Content)
	if collection.Bytes+delta > policy.Limits.MaxContentPerScope {
		return memory.MutationResult{}, memory.ErrQuota
	}
	next := cloneRecord(current)
	next.Revision++
	next.Content = cmd.Content
	next.Topics = append([]string(nil), cmd.Topics...)
	next.PolicyGeneration = policy.Generation
	next.Provenance = memory.Provenance{Producer: cmd.Audit.Issuer, PolicyVersion: policyVersion(policy.Generation)}
	next.Decision = memory.Decision{Actor: cmd.Audit.Actor, Basis: string(memory.ActionRevise), DecidedAt: now}
	next.UpdatedAt = now
	next.ExpiresAt = now.Add(cmd.Retention)
	if err := validateRecordJSON(next, policy.Limits.MaxRecordBytes); err != nil {
		return memory.MutationResult{}, err
	}
	if err := updateRecord(ctx, tx, next, current.Revision); err != nil {
		return memory.MutationResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_runtime_memory_collections SET content_bytes=content_bytes+$5 WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4`, cmd.Scope.Namespace, cmd.Scope.ProjectID, cmd.Scope.SubjectID, cmd.Scope.CollectionID, delta); err != nil {
		return memory.MutationResult{}, wrapStoreErr(err)
	}
	return memory.MutationResult{Record: next, SubjectGeneration: generation, PolicyGeneration: policy.Generation}, nil
}

func (s *MemoryStore) commitRetract(ctx context.Context, tx *sql.Tx, cmd memory.Mutation, now time.Time, generation uint64) (memory.MutationResult, error) {
	current, err := readRecordForUpdate(ctx, tx, cmd.Scope, cmd.MemoryID)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if current == nil {
		return memory.MutationResult{}, memory.ErrNotFound
	}
	if current.Status == memory.StatusRetracted || current.Content == "" || current.SubjectGeneration != generation || current.Revision != cmd.Preconditions.ExpectedRevision {
		return memory.MutationResult{}, memory.ErrConflict
	}
	next := cloneRecord(current)
	next.Revision++
	next.Status = memory.StatusRetracted
	next.UpdatedAt = now
	if err := updateRecord(ctx, tx, next, current.Revision); err != nil {
		return memory.MutationResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_runtime_memory_collections SET proposal_epoch=proposal_epoch+1 WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4`, cmd.Scope.Namespace, cmd.Scope.ProjectID, cmd.Scope.SubjectID, cmd.Scope.CollectionID); err != nil {
		return memory.MutationResult{}, wrapStoreErr(err)
	}
	if err := insertPostgresLifecycleJob(ctx, tx, memory.PurgeJob{TaskID: memory.LifecycleTaskID(memory.PurgeJobRetract, cmd.Subject, cmd.Scope, "", next.MemoryID, next.Revision, generation), Kind: memory.PurgeJobRetract, Subject: cmd.Subject, Scope: cmd.Scope, MemoryID: next.MemoryID, ExpectedRevision: next.Revision, PurgeBeforeGeneration: generation, SelectionCutoff: now, CreatedAt: now}); err != nil {
		return memory.MutationResult{}, err
	}
	return memory.MutationResult{Record: next, SubjectGeneration: generation, PolicyGeneration: next.PolicyGeneration}, nil
}

func (s *MemoryStore) commitForget(ctx context.Context, tx *sql.Tx, cmd memory.Mutation, now time.Time, generation uint64) (memory.MutationResult, error) {
	_, err := tx.ExecContext(ctx, `UPDATE agent_runtime_memory_subjects SET generation=generation+1,disabled=true WHERE namespace=$1 AND project_id=$2 AND subject_id=$3`, cmd.Subject.Namespace, cmd.Subject.ProjectID, cmd.Subject.SubjectID)
	if err != nil {
		return memory.MutationResult{}, wrapStoreErr(err)
	}
	if err := insertPostgresLifecycleJob(ctx, tx, memory.PurgeJob{TaskID: memory.LifecycleTaskID(memory.PurgeJobForget, cmd.Subject, memory.Scope{}, "", "", 0, generation), Kind: memory.PurgeJobForget, Subject: cmd.Subject, PurgeBeforeGeneration: generation, SelectionCutoff: now, CreatedAt: now}); err != nil {
		return memory.MutationResult{}, err
	}
	return memory.MutationResult{SubjectGeneration: generation + 1}, nil
}

func (s *MemoryStore) commitEnable(ctx context.Context, tx *sql.Tx, cmd memory.Mutation, state memorySubjectState, policy memoryPolicyState) (memory.MutationResult, error) {
	if !state.Disabled || state.Generation == 0 {
		return memory.MutationResult{}, memory.ErrConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_runtime_memory_subjects SET generation=generation+1,disabled=false WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND generation=$4 AND disabled=true`, cmd.Subject.Namespace, cmd.Subject.ProjectID, cmd.Subject.SubjectID, state.Generation)
	if err != nil {
		return memory.MutationResult{}, wrapStoreErr(err)
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return memory.MutationResult{}, memory.ErrConflict
	}
	return memory.MutationResult{SubjectGeneration: state.Generation + 1, PolicyGeneration: policy.Generation}, nil
}

func (s *MemoryStore) commitRetractSource(ctx context.Context, tx *sql.Tx, cmd memory.Mutation, now time.Time, generation uint64, policy memoryPolicyState) (memory.MutationResult, error) {
	if cmd.SourceRef == "" || cmd.Preconditions.SourceEpoch == 0 {
		return memory.MutationResult{}, memory.ErrInvalid
	}
	source, found, err := readSource(ctx, tx, cmd.Scope, cmd.SourceRef, true)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if !found {
		return memory.MutationResult{}, memory.ErrNotFound
	}
	if source.Revoked || source.Epoch != cmd.Preconditions.SourceEpoch {
		return memory.MutationResult{}, memory.ErrConflict
	}
	oldEpoch := source.Epoch
	_, err = tx.ExecContext(ctx, `UPDATE agent_runtime_memory_sources SET source_epoch=source_epoch+1,revoked=true WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4 AND source_ref=$5 AND source_epoch=$6 AND NOT revoked`, cmd.Scope.Namespace, cmd.Scope.ProjectID, cmd.Scope.SubjectID, cmd.Scope.CollectionID, cmd.SourceRef, source.Epoch)
	if err != nil {
		return memory.MutationResult{}, wrapStoreErr(err)
	}
	if err := insertPostgresLifecycleJob(ctx, tx, memory.PurgeJob{TaskID: memory.LifecycleTaskID(memory.PurgeJobRetractSource, cmd.Subject, cmd.Scope, cmd.SourceRef, "", oldEpoch, generation), Kind: memory.PurgeJobRetractSource, Subject: cmd.Subject, Scope: cmd.Scope, SourceRef: cmd.SourceRef, SourceEpoch: oldEpoch, PurgeBeforeGeneration: generation, SelectionCutoff: now, CreatedAt: now}); err != nil {
		return memory.MutationResult{}, err
	}
	return memory.MutationResult{SubjectGeneration: generation, PolicyGeneration: policy.Generation}, nil
}

func (s *MemoryStore) commitPolicy(ctx context.Context, tx *sql.Tx, cmd memory.Mutation, generation uint64, current memoryPolicyState) (memory.MutationResult, error) {
	if cmd.PolicyUpdate == nil || memory.ValidateLimits(cmd.PolicyUpdate.Limits) != nil {
		return memory.MutationResult{}, memory.ErrInvalid
	}
	if cmd.PolicyUpdate.ExpectedGeneration != current.Generation {
		return memory.MutationResult{}, fmt.Errorf("%w: policy generation", memory.ErrConflict)
	}
	raw, _ := json.Marshal(cmd.PolicyUpdate.Limits)
	_, err := tx.ExecContext(ctx, `UPDATE agent_runtime_memory_policies SET generation=generation+1,limits=$3::jsonb WHERE namespace=$1 AND project_id=$2 AND generation=$4`, cmd.Scope.Namespace, cmd.Scope.ProjectID, string(raw), current.Generation)
	if err != nil {
		return memory.MutationResult{}, wrapStoreErr(err)
	}
	return memory.MutationResult{SubjectGeneration: generation, PolicyGeneration: current.Generation + 1}, nil
}

func readPurgeLease(ctx context.Context, tx *sql.Tx, subject memory.SubjectScope, lock bool) (memory.PurgeLease, bool, error) {
	query := `SELECT task_id,owner_token,lease_epoch,lease_expires_at,selection_cutoff,purge_before_generation,cursor FROM agent_runtime_memory_purge_jobs WHERE namespace=$1 AND project_id=$2 AND subject_id=$3`
	if lock {
		query += ` FOR UPDATE`
	}
	var l memory.PurgeLease
	err := tx.QueryRowContext(ctx, query, subject.Namespace, subject.ProjectID, subject.SubjectID).Scan(&l.TaskID, &l.OwnerToken, &l.LeaseEpoch, &l.LeaseExpiresAt, &l.SelectionCutoff, &l.PurgeBeforeGeneration, &l.Cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return l, false, nil
	}
	if err != nil {
		return l, false, wrapStoreErr(err)
	}
	l.LeaseExpiresAt = l.LeaseExpiresAt.UTC()
	l.SelectionCutoff = l.SelectionCutoff.UTC()
	return l, true, nil
}

func (s *MemoryStore) commitClaimPurge(ctx context.Context, tx *sql.Tx, cmd memory.Mutation, now time.Time, generation uint64, policy memoryPolicyState) (memory.MutationResult, error) {
	r := cmd.PurgeLease
	if r.TaskID == "" || r.OwnerToken == "" || r.PurgeBeforeGeneration == 0 || r.PurgeBeforeGeneration > generation || cmd.LeaseDuration <= 0 || cmd.LeaseDuration > time.Hour {
		return memory.MutationResult{}, memory.ErrInvalid
	}
	current, exists, err := readPurgeLease(ctx, tx, cmd.Subject, true)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if exists && current.TaskID == r.TaskID && current.PurgeBeforeGeneration != r.PurgeBeforeGeneration {
		return memory.MutationResult{}, fmt.Errorf("%w: purge generation cutoff", memory.ErrConflict)
	}
	if exists && current.LeaseExpiresAt.After(now) && (current.TaskID != r.TaskID || current.OwnerToken != r.OwnerToken || current.PurgeBeforeGeneration != r.PurgeBeforeGeneration) {
		return memory.MutationResult{}, fmt.Errorf("%w: active purge lease", memory.ErrConflict)
	}
	if exists && !current.LeaseExpiresAt.After(now) && current.TaskID != r.TaskID {
		current = memory.PurgeLease{}
		exists = false
	}
	epoch := current.LeaseEpoch + 1
	if epoch == 0 {
		epoch = 1
	}
	cutoff := current.SelectionCutoff
	if !exists {
		cutoff = now
	}
	lease := memory.PurgeLease{TaskID: r.TaskID, OwnerToken: r.OwnerToken, LeaseEpoch: epoch, LeaseExpiresAt: now.Add(cmd.LeaseDuration), SelectionCutoff: cutoff, PurgeBeforeGeneration: r.PurgeBeforeGeneration, Cursor: current.Cursor}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_runtime_memory_purge_jobs(namespace,project_id,subject_id,task_id,owner_token,lease_epoch,lease_expires_at,selection_cutoff,purge_before_generation,cursor) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(namespace,project_id,subject_id) DO UPDATE SET task_id=EXCLUDED.task_id,owner_token=EXCLUDED.owner_token,lease_epoch=EXCLUDED.lease_epoch,lease_expires_at=EXCLUDED.lease_expires_at,selection_cutoff=EXCLUDED.selection_cutoff,purge_before_generation=EXCLUDED.purge_before_generation,cursor=EXCLUDED.cursor`, cmd.Subject.Namespace, cmd.Subject.ProjectID, cmd.Subject.SubjectID, lease.TaskID, lease.OwnerToken, lease.LeaseEpoch, lease.LeaseExpiresAt, lease.SelectionCutoff, lease.PurgeBeforeGeneration, lease.Cursor)
	if err != nil {
		return memory.MutationResult{}, wrapStoreErr(err)
	}
	return memory.MutationResult{SubjectGeneration: generation, PolicyGeneration: policy.Generation, PurgeLease: lease}, nil
}

func validateLease(current memory.PurgeLease, found bool, provided memory.PurgeLease, now time.Time) error {
	if provided.TaskID == "" || provided.OwnerToken == "" || provided.LeaseEpoch == 0 || provided.SelectionCutoff.IsZero() {
		return memory.ErrInvalid
	}
	if !found || current.TaskID != provided.TaskID || current.OwnerToken != provided.OwnerToken || current.LeaseEpoch != provided.LeaseEpoch || current.PurgeBeforeGeneration != provided.PurgeBeforeGeneration || !sameInstant(current.SelectionCutoff, provided.SelectionCutoff) || !sameInstant(current.LeaseExpiresAt, provided.LeaseExpiresAt) || !current.LeaseExpiresAt.After(now) || current.Cursor != provided.Cursor {
		return fmt.Errorf("%w: purge lease", memory.ErrConflict)
	}
	return nil
}

func (s *MemoryStore) commitPurge(ctx context.Context, tx *sql.Tx, cmd memory.Mutation, now time.Time, generation uint64, policy memoryPolicyState, collection memoryCollectionState) (memory.MutationResult, error) {
	lease, found, err := readPurgeLease(ctx, tx, cmd.Subject, true)
	if err != nil {
		return memory.MutationResult{}, err
	}
	if err := validateLease(lease, found, cmd.PurgeLease, now); err != nil {
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
	for _, t := range targets {
		if t.MemoryID == "" || t.ExpectedRevision == 0 || !sameInstant(t.SelectedBefore, lease.SelectionCutoff) {
			return memory.MutationResult{}, memory.ErrInvalid
		}
		r, err := readRecordForUpdate(ctx, tx, cmd.Scope, t.MemoryID)
		if err != nil {
			return memory.MutationResult{}, err
		}
		if r == nil || r.Revision != t.ExpectedRevision || r.SubjectGeneration > lease.PurgeBeforeGeneration {
			return memory.MutationResult{}, memory.ErrConflict
		}
		if r.Content != "" && (r.UpdatedAt.After(t.SelectedBefore) || (r.Status != memory.StatusRetracted && r.ExpiresAt.After(t.SelectedBefore) && r.SubjectGeneration >= generation)) {
			return memory.MutationResult{}, memory.ErrConflict
		}
	}
	result := memory.MutationResult{SubjectGeneration: generation, PolicyGeneration: policy.Generation, PurgeLease: lease}
	for _, t := range targets {
		r, err := readRecordForUpdate(ctx, tx, cmd.Scope, t.MemoryID)
		if err != nil {
			return memory.MutationResult{}, err
		}
		if r.Content == "" {
			continue
		}
		released := len(r.Content)
		r.Content = ""
		r.Topics = nil
		r.UpdatedAt = now
		if err := updateRecord(ctx, tx, r, t.ExpectedRevision); err != nil {
			return memory.MutationResult{}, err
		}
		collection.Bytes -= released
		collection.Records--
		result.PurgedRecords++
		result.ReleasedBytes += released
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_runtime_memory_collections SET records_count=$5,content_bytes=$6 WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4`, cmd.Scope.Namespace, cmd.Scope.ProjectID, cmd.Scope.SubjectID, cmd.Scope.CollectionID, collection.Records, collection.Bytes); err != nil {
		return memory.MutationResult{}, wrapStoreErr(err)
	}
	lease.Cursor = cmd.PurgeCursor
	if _, err := tx.ExecContext(ctx, `UPDATE agent_runtime_memory_purge_jobs SET cursor=$4 WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND lease_epoch=$5`, cmd.Subject.Namespace, cmd.Subject.ProjectID, cmd.Subject.SubjectID, lease.Cursor, lease.LeaseEpoch); err != nil {
		return memory.MutationResult{}, wrapStoreErr(err)
	}
	result.PurgeLease = lease
	return result, nil
}

package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

const maxLegacyManifestEntries = 10000

// LegacyMemoryManifest is an explicit, trusted ownership map. No field is
// inferred from legacy user_id, agent_id, or payload data.
type LegacyMemoryManifest struct {
	SourceID           string
	ManifestHash       string
	CandidateRetention time.Duration
	Entries            []LegacyMemoryManifestEntry
}

type LegacyMemoryManifestEntry struct {
	LegacyMemoryID string
	Scope          memory.Scope
}

type LegacyMemoryMigrationReport struct {
	Total        int
	Ready        int
	Missing      int
	Conflicts    int
	Invalid      int
	OverQuota    int
	Imported     int
	NextIndex    int
	Completed    bool
	ManifestHash string
}

// LegacyMemoryManifestHash returns the deterministic hash a trusted operator
// must place in the manifest before dry-run or import. It binds the source,
// retention policy, and a canonical mapping order.
func LegacyMemoryManifestHash(sourceID string, retention time.Duration, entries []LegacyMemoryManifestEntry) (string, error) {
	if err := validateLegacyManifestInputs(sourceID, retention, entries); err != nil {
		return "", err
	}
	if len(entries) == 0 || len(entries) > maxLegacyManifestEntries {
		return "", fmt.Errorf("%w: finite legacy manifest", memory.ErrInvalid)
	}
	canonical := canonicalLegacyEntries(entries)
	raw, err := json.Marshal(struct {
		Version        int                         `json:"version"`
		SourceID       string                      `json:"source_id"`
		RetentionNanos int64                       `json:"retention_nanos"`
		Entries        []LegacyMemoryManifestEntry `json:"entries"`
	}{Version: 1, SourceID: sourceID, RetentionNanos: int64(retention), Entries: canonical})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func validateLegacyManifest(manifest LegacyMemoryManifest) error {
	if manifest.ManifestHash == "" {
		return fmt.Errorf("%w: complete legacy manifest", memory.ErrInvalid)
	}
	want, err := LegacyMemoryManifestHash(manifest.SourceID, manifest.CandidateRetention, manifest.Entries)
	if err != nil {
		return err
	}
	if !constantStringEqual(want, manifest.ManifestHash) {
		return fmt.Errorf("%w: legacy manifest hash", memory.ErrConflict)
	}
	return nil
}

func validateLegacyManifestInputs(sourceID string, retention time.Duration, entries []LegacyMemoryManifestEntry) error {
	if err := validateLegacyIdentifier(sourceID, "source id"); err != nil {
		return err
	}
	if retention <= 0 || len(entries) == 0 || len(entries) > maxLegacyManifestEntries {
		return fmt.Errorf("%w: finite legacy manifest", memory.ErrInvalid)
	}
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if err := validateLegacyIdentifier(entry.LegacyMemoryID, "memory id"); err != nil {
			return err
		}
		if err := entry.Scope.Validate(); err != nil {
			return err
		}
		if _, ok := seen[entry.LegacyMemoryID]; ok {
			return fmt.Errorf("%w: duplicate legacy mapping", memory.ErrInvalid)
		}
		seen[entry.LegacyMemoryID] = struct{}{}
	}
	return nil
}

func validateLegacyIdentifier(value, label string) error {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) {
		return fmt.Errorf("%w: invalid legacy %s", memory.ErrInvalid, label)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: invalid legacy %s", memory.ErrInvalid, label)
		}
	}
	return nil
}

func canonicalLegacyEntries(entries []LegacyMemoryManifestEntry) []LegacyMemoryManifestEntry {
	out := append([]LegacyMemoryManifestEntry(nil), entries...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].LegacyMemoryID != out[j].LegacyMemoryID {
			return out[i].LegacyMemoryID < out[j].LegacyMemoryID
		}
		a, b := out[i].Scope, out[j].Scope
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.ProjectID != b.ProjectID {
			return a.ProjectID < b.ProjectID
		}
		if a.SubjectID != b.SubjectID {
			return a.SubjectID < b.SubjectID
		}
		return a.CollectionID < b.CollectionID
	})
	return out
}

func legacyAttestation(manifest LegacyMemoryManifest, entry LegacyMemoryManifestEntry, legacy memory.UserMemory, now time.Time) memory.SourceAttestation {
	idDigest := sha256.Sum256([]byte(entry.LegacyMemoryID))
	contentDigest := sha256.Sum256([]byte(legacy.Memory))
	return memory.SourceAttestation{Scope: entry.Scope, SourceRef: "legacy-" + manifest.ManifestHash[:16] + "-" + hex.EncodeToString(idDigest[:8]), Version: manifest.ManifestHash, Digest: hex.EncodeToString(contentDigest[:]), SourceEpoch: 1, ExpiresAt: now.Add(manifest.CandidateRetention), Verifier: "legacy-migration"}
}

func sameSourceAttestation(current memorySourceState, att memory.SourceAttestation) bool {
	return current.Epoch == att.SourceEpoch && current.Version == att.Version && current.Digest == att.Digest && current.Verifier == att.Verifier && !current.Revoked && sameInstant(current.ExpiresAt, att.ExpiresAt)
}

func constantStringEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// DryRunLegacyMemoryMigration returns metadata-only counts and never returns
// or logs legacy content.
func (s *MemoryStore) DryRunLegacyMemoryMigration(ctx context.Context, manifest LegacyMemoryManifest) (LegacyMemoryMigrationReport, error) {
	if err := validateLegacyManifest(manifest); err != nil {
		return LegacyMemoryMigrationReport{}, err
	}
	report := LegacyMemoryMigrationReport{Total: len(manifest.Entries), ManifestHash: manifest.ManifestHash}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return report, wrapStoreErr(err)
	}
	defer tx.Rollback()
	now, err := s.storeNow(ctx, tx)
	if err != nil {
		return report, wrapStoreErr(err)
	}
	type plannedState struct {
		collection                                      memoryCollectionState
		sources, addedRecords, addedBytes, addedSources int
	}
	planned := make(map[memory.Scope]plannedState)
	subjects := make(map[memory.SubjectScope]memorySubjectState)
	for _, entry := range canonicalLegacyEntries(manifest.Entries) {
		var assignedSource, assignedHash, assignedNamespace, assignedProject, assignedSubject, assignedCollection string
		err := tx.QueryRowContext(ctx, `SELECT source_id,manifest_hash,namespace,project_id,subject_id,collection_id FROM agent_runtime_memory_legacy_assignments WHERE source_table='agent_runtime_memories' AND legacy_memory_id=$1`, entry.LegacyMemoryID).Scan(&assignedSource, &assignedHash, &assignedNamespace, &assignedProject, &assignedSubject, &assignedCollection)
		if err == nil && (assignedSource != manifest.SourceID || assignedHash != manifest.ManifestHash || assignedNamespace != entry.Scope.Namespace || assignedProject != entry.Scope.ProjectID || assignedSubject != entry.Scope.SubjectID || assignedCollection != entry.Scope.CollectionID) {
			report.Conflicts++
			continue
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return report, wrapStoreErr(err)
		}
		var raw []byte
		err = tx.QueryRowContext(ctx, `SELECT payload FROM agent_runtime_memories WHERE memory_id=$1`, entry.LegacyMemoryID).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			report.Missing++
			continue
		}
		if err != nil {
			return report, wrapStoreErr(err)
		}
		var legacy memory.UserMemory
		if json.Unmarshal(raw, &legacy) != nil || legacy.Memory == "" {
			report.Invalid++
			continue
		}
		policy, err := s.readPolicy(ctx, tx, entry.Scope.Namespace, entry.Scope.ProjectID, false)
		if err != nil {
			return report, err
		}
		if manifest.CandidateRetention > policy.Limits.MaxRetention || memory.ValidatePersistableContent(legacy.Memory, legacy.Topics, policy.Limits) != nil {
			report.Invalid++
			continue
		}
		subject, ok := subjects[entry.Scope.Subject()]
		if !ok {
			subject, err = readSubject(ctx, tx, entry.Scope.Subject(), false)
			if err != nil {
				return report, err
			}
			subjects[entry.Scope.Subject()] = subject
		}
		if subject.Disabled {
			report.Conflicts++
			continue
		}
		att := legacyAttestation(manifest, entry, legacy, now)
		source, sourceFound, err := readSource(ctx, tx, entry.Scope, att.SourceRef, false)
		if err != nil {
			return report, err
		}
		if sourceFound && !sameSourceAttestation(source, att) {
			report.Conflicts++
			continue
		}
		state, ok := planned[entry.Scope]
		if !ok {
			state.collection, err = readCollection(ctx, tx, entry.Scope, false)
			if err != nil {
				return report, err
			}
			state.sources, err = sourceCount(ctx, tx, entry.Scope)
			if err != nil {
				return report, err
			}
		}
		candidate := &memory.Record{Scope: entry.Scope, MemoryID: entry.LegacyMemoryID, Revision: 1, SubjectGeneration: subject.Generation, PolicyGeneration: policy.Generation, CollectionProposalEpoch: state.collection.ProposalEpoch, Kind: memory.KindUserStatement, Content: legacy.Memory, Topics: legacy.Topics, Status: memory.StatusCandidate, Provenance: memory.Provenance{SourceRef: att.SourceRef, SourceVersion: att.Version, SourceEpoch: att.SourceEpoch, Producer: att.Verifier, PolicyVersion: policyVersion(policy.Generation)}, CreatedAt: now, UpdatedAt: now, ExpiresAt: att.ExpiresAt}
		if validateRecordJSON(candidate, policy.Limits.MaxRecordBytes) != nil {
			report.Invalid++
			continue
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_runtime_memory_records WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4 AND memory_id=$5)`, entry.Scope.Namespace, entry.Scope.ProjectID, entry.Scope.SubjectID, entry.Scope.CollectionID, entry.LegacyMemoryID).Scan(&exists); err != nil {
			return report, wrapStoreErr(err)
		}
		if exists {
			report.Conflicts++
			continue
		}
		newSource := 0
		if !sourceFound {
			newSource = 1
		}
		if state.collection.Records+state.addedRecords+1 > policy.Limits.MaxRecordsPerScope || state.collection.Bytes+state.addedBytes+len(legacy.Memory) > policy.Limits.MaxContentPerScope || state.sources+state.addedSources+state.collection.RecordIdentities+state.collection.CandidateBatches+state.addedRecords+1+newSource > policy.Limits.MaxControlsPerScope {
			report.OverQuota++
			continue
		}
		state.addedRecords++
		state.addedBytes += len(legacy.Memory)
		state.addedSources += newSource
		planned[entry.Scope] = state
		report.Ready++
	}
	if err := tx.Commit(); err != nil {
		return report, wrapStoreErr(err)
	}
	return report, nil
}

// MigrateLegacyMemories imports at most maxEntries candidates. Progress and
// each imported candidate commit atomically, so interruption resumes at the
// first uncommitted manifest entry. The legacy table is never modified.
func (s *MemoryStore) MigrateLegacyMemories(ctx context.Context, manifest LegacyMemoryManifest, maxEntries int) (LegacyMemoryMigrationReport, error) {
	if err := validateLegacyManifest(manifest); err != nil {
		return LegacyMemoryMigrationReport{}, err
	}
	if maxEntries <= 0 || maxEntries > 100 {
		return LegacyMemoryMigrationReport{}, fmt.Errorf("%w: migration batch size", memory.ErrInvalid)
	}
	report := LegacyMemoryMigrationReport{Total: len(manifest.Entries), ManifestHash: manifest.ManifestHash}
	for processed := 0; processed < maxEntries; processed++ {
		index, imported, completed, err := s.migrateOneLegacy(ctx, manifest)
		if err != nil {
			return report, err
		}
		report.NextIndex, report.Imported, report.Completed = index, imported, completed
		if completed {
			break
		}
	}
	return report, nil
}

func (s *MemoryStore) migrateOneLegacy(ctx context.Context, manifest LegacyMemoryManifest) (int, int, bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, 0, false, wrapStoreErr(err)
	}
	defer tx.Rollback()
	now, err := s.storeNow(ctx, tx)
	if err != nil {
		return 0, 0, false, wrapStoreErr(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_runtime_memory_legacy_migrations(source_id,manifest_hash,next_index,imported_count,completed,updated_at) VALUES($1,$2,0,0,false,$3) ON CONFLICT DO NOTHING`, manifest.SourceID, manifest.ManifestHash, now); err != nil {
		return 0, 0, false, wrapStoreErr(err)
	}
	var hash string
	var index, imported int
	var completed bool
	if err := tx.QueryRowContext(ctx, `SELECT manifest_hash,next_index,imported_count,completed FROM agent_runtime_memory_legacy_migrations WHERE source_id=$1 FOR UPDATE`, manifest.SourceID).Scan(&hash, &index, &imported, &completed); err != nil {
		return 0, 0, false, wrapStoreErr(err)
	}
	if hash != manifest.ManifestHash {
		return index, imported, completed, fmt.Errorf("%w: source id already bound to another manifest", memory.ErrConflict)
	}
	if completed || index >= len(manifest.Entries) {
		if !completed {
			if _, err := tx.ExecContext(ctx, `UPDATE agent_runtime_memory_legacy_migrations SET completed=true,updated_at=$2 WHERE source_id=$1`, manifest.SourceID, now); err != nil {
				return index, imported, false, wrapStoreErr(err)
			}
			completed = true
		}
		if err := tx.Commit(); err != nil {
			return index, imported, completed, fmt.Errorf("%w: %v", memory.ErrCommitUnknown, err)
		}
		return index, imported, completed, nil
	}
	entry := canonicalLegacyEntries(manifest.Entries)[index]
	if err := reserveLegacyAssignment(ctx, tx, manifest, entry, now); err != nil {
		return index, imported, false, err
	}
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM agent_runtime_memories WHERE memory_id=$1`, entry.LegacyMemoryID).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return index, imported, false, fmt.Errorf("%w: manifest legacy id %q", memory.ErrNotFound, entry.LegacyMemoryID)
		}
		return index, imported, false, wrapStoreErr(err)
	}
	var legacy memory.UserMemory
	if err := json.Unmarshal(raw, &legacy); err != nil || legacy.Memory == "" {
		return index, imported, false, fmt.Errorf("%w: malformed legacy memory %q", memory.ErrInvalid, entry.LegacyMemoryID)
	}
	policy, err := s.ensurePolicy(ctx, tx, entry.Scope.Namespace, entry.Scope.ProjectID, false)
	if err != nil {
		return index, imported, false, err
	}
	if manifest.CandidateRetention > policy.Limits.MaxRetention {
		return index, imported, false, memory.ErrInvalid
	}
	state, err := ensureSubject(ctx, tx, entry.Scope.Subject())
	if err != nil {
		return index, imported, false, err
	}
	if state.Disabled {
		return index, imported, false, memory.ErrConflict
	}
	collection, err := ensureCollection(ctx, tx, entry.Scope)
	if err != nil {
		return index, imported, false, err
	}
	if err := memory.ValidatePersistableContent(legacy.Memory, legacy.Topics, policy.Limits); err != nil {
		return index, imported, false, err
	}
	if prior, err := readRecordForUpdate(ctx, tx, entry.Scope, entry.LegacyMemoryID); err != nil {
		return index, imported, false, err
	} else if prior != nil {
		return index, imported, false, fmt.Errorf("%w: migration target exists", memory.ErrConflict)
	}
	att := legacyAttestation(manifest, entry, legacy, now)
	currentSource, sourceFound, err := readSource(ctx, tx, entry.Scope, att.SourceRef, true)
	if err != nil {
		return index, imported, false, err
	}
	if sourceFound && !sameSourceAttestation(currentSource, att) {
		return index, imported, false, fmt.Errorf("%w: migration source", memory.ErrConflict)
	}
	sources, err := sourceCount(ctx, tx, entry.Scope)
	if err != nil {
		return index, imported, false, err
	}
	newSource := 0
	if !sourceFound {
		newSource = 1
	}
	if collection.Records+1 > policy.Limits.MaxRecordsPerScope || collection.Bytes+len(legacy.Memory) > policy.Limits.MaxContentPerScope || sources+collection.RecordIdentities+collection.CandidateBatches+1+newSource > policy.Limits.MaxControlsPerScope {
		return index, imported, false, memory.ErrQuota
	}
	r := &memory.Record{Scope: entry.Scope, MemoryID: entry.LegacyMemoryID, Revision: 1, SubjectGeneration: state.Generation, PolicyGeneration: policy.Generation, CollectionProposalEpoch: collection.ProposalEpoch, Kind: memory.KindUserStatement, Content: legacy.Memory, Topics: append([]string(nil), legacy.Topics...), Status: memory.StatusCandidate, Provenance: memory.Provenance{SourceRef: att.SourceRef, SourceVersion: att.Version, SourceEpoch: att.SourceEpoch, Producer: att.Verifier, PolicyVersion: policyVersion(policy.Generation)}, CreatedAt: now, UpdatedAt: now, ExpiresAt: att.ExpiresAt}
	if err := validateRecordJSON(r, policy.Limits.MaxRecordBytes); err != nil {
		return index, imported, false, err
	}
	if err := upsertSource(ctx, tx, att); err != nil {
		return index, imported, false, err
	}
	if err := insertRecord(ctx, tx, r); err != nil {
		return index, imported, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_runtime_memory_collections SET records_count=records_count+1,content_bytes=content_bytes+$5,record_identities=record_identities+1 WHERE namespace=$1 AND project_id=$2 AND subject_id=$3 AND collection_id=$4`, entry.Scope.Namespace, entry.Scope.ProjectID, entry.Scope.SubjectID, entry.Scope.CollectionID, len(legacy.Memory)); err != nil {
		return index, imported, false, wrapStoreErr(err)
	}
	audit := memory.AuditEvent{OperationID: legacyMigrationOperationID(manifest, entry), Scope: entry.Scope, Subject: entry.Scope.Subject(), Action: memory.ActionPropose, Actor: "legacy-migration", Issuer: "postgres-manifest", MemoryID: entry.LegacyMemoryID, Revision: 1, Generation: state.Generation, OccurredAt: now}
	s.mu.RLock()
	auditFailure := s.auditFailure
	s.mu.RUnlock()
	if auditFailure != nil {
		if err := auditFailure(audit); err != nil {
			return index, imported, false, fmt.Errorf("%w: %v", memory.ErrAudit, err)
		}
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return index, imported, false, err
	}
	index++
	imported++
	completed = index == len(manifest.Entries)
	if _, err := tx.ExecContext(ctx, `UPDATE agent_runtime_memory_legacy_migrations SET next_index=$2,imported_count=$3,completed=$4,updated_at=$5 WHERE source_id=$1`, manifest.SourceID, index, imported, completed, now); err != nil {
		return index, imported, completed, wrapStoreErr(err)
	}
	if err := tx.Commit(); err != nil {
		return index, imported, completed, fmt.Errorf("%w: %v", memory.ErrCommitUnknown, err)
	}
	return index, imported, completed, nil
}

func reserveLegacyAssignment(ctx context.Context, tx *sql.Tx, manifest LegacyMemoryManifest, entry LegacyMemoryManifestEntry, now time.Time) error {
	const sourceTable = "agent_runtime_memories"
	_, err := tx.ExecContext(ctx, `INSERT INTO agent_runtime_memory_legacy_assignments(source_table,legacy_memory_id,source_id,manifest_hash,namespace,project_id,subject_id,collection_id,assigned_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`, sourceTable, entry.LegacyMemoryID, manifest.SourceID, manifest.ManifestHash, entry.Scope.Namespace, entry.Scope.ProjectID, entry.Scope.SubjectID, entry.Scope.CollectionID, now)
	if err != nil {
		return wrapStoreErr(err)
	}
	var sourceID, hash, namespace, project, subject, collection string
	if err := tx.QueryRowContext(ctx, `SELECT source_id,manifest_hash,namespace,project_id,subject_id,collection_id FROM agent_runtime_memory_legacy_assignments WHERE source_table=$1 AND legacy_memory_id=$2 FOR UPDATE`, sourceTable, entry.LegacyMemoryID).Scan(&sourceID, &hash, &namespace, &project, &subject, &collection); err != nil {
		return wrapStoreErr(err)
	}
	if sourceID != manifest.SourceID || hash != manifest.ManifestHash || namespace != entry.Scope.Namespace || project != entry.Scope.ProjectID || subject != entry.Scope.SubjectID || collection != entry.Scope.CollectionID {
		return fmt.Errorf("%w: legacy record already assigned", memory.ErrConflict)
	}
	return nil
}

func legacyMigrationOperationID(manifest LegacyMemoryManifest, entry LegacyMemoryManifestEntry) string {
	digest := sha256.Sum256([]byte(entry.LegacyMemoryID))
	return "legacy-" + manifest.ManifestHash[:16] + "-" + hex.EncodeToString(digest[:8])
}

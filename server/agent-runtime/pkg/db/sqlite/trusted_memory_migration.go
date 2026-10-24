package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

const (
	LegacyMemorySource             = "memories"
	MaxLegacyMemoryManifestEntries = 10_000
)

// LegacyMemoryMapping is an administrator-supplied authority assignment for
// one row in the legacy SQLite memories table. No field is inferred from the
// legacy user_id, agent_id, content, or namespace columns.
type LegacyMemoryMapping struct {
	SourceMemoryID string       `json:"source_memory_id"`
	Scope          memory.Scope `json:"scope"`
}

type LegacyMemoryManifest struct {
	SourceName         string                `json:"source_name"`
	CandidateRetention time.Duration         `json:"candidate_retention"`
	Mappings           []LegacyMemoryMapping `json:"mappings"`
}

type LegacyMigrationReport struct {
	ManifestHash string
	Total        int
	Importable   int
	Missing      int
	Conflicts    int
	Invalid      int
	OverQuota    int
	Imported     int
	NextEntry    int
	Complete     bool
}

type canonicalLegacyManifest struct {
	SourceName    string                `json:"source_name"`
	RetentionNano int64                 `json:"candidate_retention_ns"`
	Mappings      []LegacyMemoryMapping `json:"mappings"`
}

func normalizeLegacyManifest(manifest LegacyMemoryManifest, limits memory.Limits) (canonicalLegacyManifest, string, []byte, error) {
	// Reject the untrusted collection size before copying, sorting, or JSON
	// encoding it. Batch size bounds transaction work; this bound separately
	// caps manifest admission CPU and memory.
	if manifest.SourceName != LegacyMemorySource || manifest.CandidateRetention <= 0 || manifest.CandidateRetention > limits.MaxRetention || len(manifest.Mappings) == 0 || len(manifest.Mappings) > MaxLegacyMemoryManifestEntries {
		return canonicalLegacyManifest{}, "", nil, fmt.Errorf("%w: finite explicit legacy manifest required", memory.ErrInvalid)
	}
	mappings := append([]LegacyMemoryMapping(nil), manifest.Mappings...)
	sort.Slice(mappings, func(i, j int) bool { return mappings[i].SourceMemoryID < mappings[j].SourceMemoryID })
	for i, mapping := range mappings {
		if mapping.SourceMemoryID == "" || len(mapping.SourceMemoryID) > 256 || !utf8.ValidString(mapping.SourceMemoryID) || strings.IndexFunc(mapping.SourceMemoryID, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			return canonicalLegacyManifest{}, "", nil, fmt.Errorf("%w: invalid legacy source id", memory.ErrInvalid)
		}
		if err := mapping.Scope.Validate(); err != nil {
			return canonicalLegacyManifest{}, "", nil, err
		}
		if i > 0 && mappings[i-1].SourceMemoryID == mapping.SourceMemoryID {
			return canonicalLegacyManifest{}, "", nil, fmt.Errorf("%w: duplicate legacy source id", memory.ErrInvalid)
		}
	}
	canonical := canonicalLegacyManifest{SourceName: manifest.SourceName, RetentionNano: int64(manifest.CandidateRetention), Mappings: mappings}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return canonicalLegacyManifest{}, "", nil, err
	}
	digest := sha256.Sum256(raw)
	return canonical, hex.EncodeToString(digest[:]), raw, nil
}

func importedLegacyID(manifestHash, sourceID string) string {
	digest := sha256.Sum256([]byte(manifestHash + "\x00" + sourceID))
	return "legacy-" + hex.EncodeToString(digest[:16])
}

func legacySourceRef(manifestHash, sourceID string) string {
	digest := sha256.Sum256([]byte(sourceID))
	return "legacy:" + manifestHash[:16] + ":" + hex.EncodeToString(digest[:8])
}

type legacyRow struct {
	Content string
	Topics  []string
}

func loadLegacyRow(ctx context.Context, q sqliteQueryer, id string) (legacyRow, bool, error) {
	var content string
	var topics sql.NullString
	err := q.QueryRowContext(ctx, `SELECT memory,topics FROM memories WHERE memory_id=?`, id).Scan(&content, &topics)
	if errors.Is(err, sql.ErrNoRows) {
		return legacyRow{}, false, nil
	}
	if err != nil {
		return legacyRow{}, false, mapSQLiteError(err)
	}
	var out legacyRow
	out.Content = content
	if topics.Valid && topics.String != "" {
		if err := json.Unmarshal([]byte(topics.String), &out.Topics); err != nil {
			return legacyRow{}, true, fmt.Errorf("%w: invalid legacy topics", memory.ErrInvalid)
		}
	}
	return out, true, nil
}

// DryRunLegacyMemoryManifest is read-only and reports counts only. It neither
// persists the manifest nor exposes legacy content.
func (s *MemoryStore) DryRunLegacyMemoryManifest(ctx context.Context, manifest LegacyMemoryManifest) (LegacyMigrationReport, error) {
	canonical, hash, _, err := normalizeLegacyManifest(manifest, s.currentLimits())
	if err != nil {
		return LegacyMigrationReport{}, err
	}
	report := LegacyMigrationReport{ManifestHash: hash, Total: len(canonical.Mappings)}
	tx, err := s.db.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return report, mapSQLiteError(err)
	}
	defer tx.Rollback()
	type projection struct {
		collection sqlCollectionState
		sources    int
		subject    sqlSubjectState
	}
	projected := make(map[memory.Scope]projection)
	now := s.storeNow()
	for _, mapping := range canonical.Mappings {
		row, found, err := loadLegacyRow(ctx, tx, mapping.SourceMemoryID)
		if err != nil {
			report.Invalid++
			continue
		}
		if !found {
			report.Missing++
			continue
		}
		policy, err := s.policyFor(ctx, tx, mapping.Scope.Namespace, mapping.Scope.ProjectID)
		if err != nil || validateStoredContent(row.Content, row.Topics, time.Duration(canonical.RetentionNano), policy.Limits) != nil {
			report.Invalid++
			continue
		}
		var one int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM agent_runtime_memory_migration_items WHERE source_name=? AND source_memory_id=?`, canonical.SourceName, mapping.SourceMemoryID).Scan(&one)
		if err == nil {
			report.Conflicts++
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return report, mapSQLiteError(err)
		}
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM agent_runtime_memory_records WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND memory_id=?`, mapping.Scope.Namespace, mapping.Scope.ProjectID, mapping.Scope.SubjectID, mapping.Scope.CollectionID, importedLegacyID(hash, mapping.SourceMemoryID)).Scan(&one)
		if err == nil {
			report.Conflicts++
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return report, mapSQLiteError(err)
		}
		state, ok := projected[mapping.Scope]
		if !ok {
			state.subject, err = subjectFor(ctx, tx, mapping.Scope.Subject())
			if err != nil {
				return report, err
			}
			if state.subject.Disabled {
				report.Conflicts++
				continue
			}
			state.collection, err = collectionFor(ctx, tx, mapping.Scope)
			if err != nil {
				return report, err
			}
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_runtime_memory_sources WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=?`, mapping.Scope.Namespace, mapping.Scope.ProjectID, mapping.Scope.SubjectID, mapping.Scope.CollectionID).Scan(&state.sources); err != nil {
				return report, mapSQLiteError(err)
			}
		}
		candidate := &memory.Record{Scope: mapping.Scope, MemoryID: importedLegacyID(hash, mapping.SourceMemoryID), Revision: 1, SubjectGeneration: state.subject.Generation, PolicyGeneration: policy.Generation, CollectionProposalEpoch: state.collection.ProposalEpoch, Kind: memory.KindUserStatement, Content: row.Content, Topics: row.Topics, Status: memory.StatusCandidate, Provenance: memory.Provenance{SourceRef: legacySourceRef(hash, mapping.SourceMemoryID), SourceVersion: hash, SourceEpoch: 1, Producer: "legacy-manifest", PolicyVersion: fmt.Sprint(policy.Generation)}, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Duration(canonical.RetentionNano))}
		if validateRecordJSON(candidate, policy.Limits.MaxRecordBytes) != nil {
			report.Invalid++
			continue
		}
		if state.collection.Records+1 > policy.Limits.MaxRecordsPerScope || state.collection.Bytes+len(row.Content) > policy.Limits.MaxContentPerScope || state.sources+state.collection.RecordIdentities+state.collection.CandidateBatches+2 > policy.Limits.MaxControlsPerScope {
			report.OverQuota++
			continue
		}
		state.collection.Records++
		state.collection.Bytes += len(row.Content)
		state.collection.RecordIdentities++
		state.sources++
		projected[mapping.Scope] = state
		report.Importable++
	}
	return report, nil
}

// ImportLegacyMemoryManifest imports at most batchSize candidate rows in one
// SQLite transaction. Repeated calls resume from durable progress. A
// validation/conflict error rolls back the whole batch and leaves the legacy
// table untouched.
func (s *MemoryStore) ImportLegacyMemoryManifest(ctx context.Context, manifest LegacyMemoryManifest, batchSize int) (LegacyMigrationReport, error) {
	canonical, hash, rawManifest, err := normalizeLegacyManifest(manifest, s.currentLimits())
	if err != nil {
		return LegacyMigrationReport{}, err
	}
	report := LegacyMigrationReport{ManifestHash: hash, Total: len(canonical.Mappings)}
	if batchSize <= 0 || batchSize > 100 {
		return report, fmt.Errorf("%w: migration batch must be 1..100", memory.ErrInvalid)
	}
	conn, err := s.db.db.Conn(ctx)
	if err != nil {
		return report, mapSQLiteError(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=25`); err != nil {
		return report, mapSQLiteError(err)
	}
	if err := beginImmediate(ctx, conn); err != nil {
		return report, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	now := s.storeNow()
	var storedRaw string
	var next, imported int
	var complete bool
	err = conn.QueryRowContext(ctx, `SELECT manifest_json,next_entry,imported_count,completed FROM agent_runtime_memory_migration_manifests WHERE manifest_hash=?`, hash).Scan(&storedRaw, &next, &imported, &complete)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = conn.ExecContext(ctx, `INSERT INTO agent_runtime_memory_migration_manifests(manifest_hash,source_name,manifest_json,next_entry,imported_count,completed,created_at_ns,updated_at_ns) VALUES(?,?,?,?,?,?,?,?)`, hash, canonical.SourceName, string(rawManifest), 0, 0, false, toNS(now), toNS(now))
		if err != nil {
			return report, mapSQLiteError(err)
		}
		next, imported, complete = 0, 0, false
	} else if err != nil {
		return report, mapSQLiteError(err)
	} else if storedRaw != string(rawManifest) {
		return report, fmt.Errorf("%w: manifest hash collision", memory.ErrConflict)
	}
	if complete {
		report.Imported, report.NextEntry, report.Complete = imported, next, true
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return report, fmt.Errorf("%w: %v", memory.ErrCommitUnknown, err)
		}
		committed = true
		return report, nil
	}
	end := next + batchSize
	if end > len(canonical.Mappings) {
		end = len(canonical.Mappings)
	}
	for index := next; index < end; index++ {
		mapping := canonical.Mappings[index]
		row, found, err := loadLegacyRow(ctx, conn, mapping.SourceMemoryID)
		if err != nil {
			return report, err
		}
		if !found {
			return report, fmt.Errorf("%w: manifest row missing", memory.ErrNotFound)
		}
		if err := s.importLegacyCandidate(ctx, conn, canonical, hash, mapping, row, now); err != nil {
			return report, err
		}
		imported++
	}
	next = end
	complete = next == len(canonical.Mappings)
	_, err = conn.ExecContext(ctx, `UPDATE agent_runtime_memory_migration_manifests SET next_entry=?,imported_count=?,completed=?,updated_at_ns=? WHERE manifest_hash=?`, next, imported, complete, toNS(now), hash)
	if err != nil {
		return report, mapSQLiteError(err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return report, fmt.Errorf("%w: %v", memory.ErrCommitUnknown, err)
	}
	committed = true
	report.Imported, report.NextEntry, report.Complete = imported, next, complete
	return report, nil
}

func (s *MemoryStore) importLegacyCandidate(ctx context.Context, conn *sql.Conn, manifest canonicalLegacyManifest, hash string, mapping LegacyMemoryMapping, row legacyRow, now time.Time) error {
	policy, err := s.policyFor(ctx, conn, mapping.Scope.Namespace, mapping.Scope.ProjectID)
	if err != nil {
		return err
	}
	tx := &memoryWriteTx{store: s, conn: conn, now: now, cmd: memory.Mutation{Scope: mapping.Scope, Subject: mapping.Scope.Subject()}, policy: policy}
	if err := tx.ensurePolicy(ctx); err != nil {
		return err
	}
	policy = tx.policy
	retention := time.Duration(manifest.RetentionNano)
	if err := validateStoredContent(row.Content, row.Topics, retention, policy.Limits); err != nil {
		return err
	}
	subject, err := subjectFor(ctx, conn, mapping.Scope.Subject())
	if err != nil {
		return err
	}
	if subject.Disabled {
		return fmt.Errorf("%w: subject disabled", memory.ErrConflict)
	}
	collection, err := collectionFor(ctx, conn, mapping.Scope)
	if err != nil {
		return err
	}
	var sourceCount int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_runtime_memory_sources WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=?`, mapping.Scope.Namespace, mapping.Scope.ProjectID, mapping.Scope.SubjectID, mapping.Scope.CollectionID).Scan(&sourceCount); err != nil {
		return mapSQLiteError(err)
	}
	if collection.Records+1 > policy.Limits.MaxRecordsPerScope || collection.Bytes+len(row.Content) > policy.Limits.MaxContentPerScope || sourceCount+collection.RecordIdentities+collection.CandidateBatches+2 > policy.Limits.MaxControlsPerScope {
		return memory.ErrQuota
	}
	memoryID := importedLegacyID(hash, mapping.SourceMemoryID)
	ref := legacySourceRef(hash, mapping.SourceMemoryID)
	var existing int
	err = conn.QueryRowContext(ctx, `SELECT 1 FROM agent_runtime_memory_records WHERE namespace=? AND project_id=? AND subject_id=? AND collection_id=? AND memory_id=?`, mapping.Scope.Namespace, mapping.Scope.ProjectID, mapping.Scope.SubjectID, mapping.Scope.CollectionID, memoryID).Scan(&existing)
	if err == nil {
		return fmt.Errorf("%w: imported memory id already exists", memory.ErrConflict)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return mapSQLiteError(err)
	}
	record := &memory.Record{Scope: mapping.Scope, MemoryID: memoryID, Revision: 1, SubjectGeneration: subject.Generation, PolicyGeneration: policy.Generation, CollectionProposalEpoch: collection.ProposalEpoch, Kind: memory.KindUserStatement, Content: row.Content, Topics: append([]string(nil), row.Topics...), Status: memory.StatusCandidate, Provenance: memory.Provenance{SourceRef: ref, SourceVersion: hash, SourceEpoch: 1, Producer: "legacy-manifest", PolicyVersion: fmt.Sprint(policy.Generation)}, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(retention)}
	if err := validateRecordJSON(record, policy.Limits.MaxRecordBytes); err != nil {
		return err
	}
	tx.subject, tx.collection = subject, collection
	if err := tx.writeSubject(ctx, subject); err != nil {
		return err
	}
	if err := tx.writeSource(ctx, ref, sqlSourceState{Version: hash, Digest: "manifest:" + hash, Epoch: 1, ExpiresAt: record.ExpiresAt, Verifier: "legacy-manifest"}); err != nil {
		return err
	}
	if err := tx.writeRecord(ctx, record); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return memory.ErrConflict
		}
		return err
	}
	collection.Records++
	collection.Bytes += len(row.Content)
	collection.RecordIdentities++
	tx.collection = collection
	if err := tx.writeCollection(ctx, collection); err != nil {
		return err
	}
	operationID := "migration:" + hash[:16] + ":" + memoryID[len("legacy-"):]
	audit := memory.AuditEvent{OperationID: operationID, Scope: mapping.Scope, Subject: mapping.Scope.Subject(), Action: memory.ActionPropose, Actor: "legacy-manifest", Issuer: manifest.SourceName, MemoryID: memoryID, Revision: 1, Generation: subject.Generation, OccurredAt: now}
	if err := tx.insertAudit(ctx, audit); err != nil {
		return fmt.Errorf("%w: %v", memory.ErrAudit, err)
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO agent_runtime_memory_migration_items(manifest_hash,source_name,source_memory_id,namespace,project_id,subject_id,collection_id,imported_memory_id,outcome) VALUES(?,?,?,?,?,?,?,?,?)`, hash, manifest.SourceName, mapping.SourceMemoryID, mapping.Scope.Namespace, mapping.Scope.ProjectID, mapping.Scope.SubjectID, mapping.Scope.CollectionID, memoryID, "candidate")
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return fmt.Errorf("%w: legacy row already assigned by a manifest", memory.ErrConflict)
		}
		return mapSQLiteError(err)
	}
	return nil
}

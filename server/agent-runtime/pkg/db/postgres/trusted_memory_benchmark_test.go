package postgres

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

// This benchmark requires an explicitly supplied disposable test database.
// It measures the real ListRecords SQL, row decoding, and candidate budgets.
func BenchmarkMemoryPostgresListRecords(b *testing.B) {
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		b.Skip("POSTGRES_TEST_DSN is not set")
	}
	ctx := context.Background()
	db, err := New(ctx, dsn, Options{AutoMigrate: true, MaxOpenConns: 4, MaxIdleConns: 4})
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	scope := memory.Scope{Namespace: "mem010-benchmark-" + time.Now().UTC().Format("150405.000000000"), ProjectID: "project", SubjectID: "subject", CollectionID: "profile"}
	limits, err := json.Marshal(memory.DefaultLimits())
	if err != nil {
		b.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `INSERT INTO agent_runtime_memory_policies(namespace,project_id,generation,limits) VALUES($1,$2,1,$3)`, scope.Namespace, scope.ProjectID, string(limits)); err != nil {
		b.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `INSERT INTO agent_runtime_memory_subjects(namespace,project_id,subject_id,generation,disabled) VALUES($1,$2,$3,1,false)`, scope.Namespace, scope.ProjectID, scope.SubjectID); err != nil {
		b.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `INSERT INTO agent_runtime_memory_collections(namespace,project_id,subject_id,collection_id) VALUES($1,$2,$3,$4)`, scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID); err != nil {
		b.Fatal(err)
	}
	defer func() {
		if _, err := db.db.ExecContext(ctx, `DELETE FROM agent_runtime_memory_records WHERE namespace=$1`, scope.Namespace); err != nil {
			b.Error(err)
		}
		if _, err := db.db.ExecContext(ctx, `DELETE FROM agent_runtime_memory_collections WHERE namespace=$1`, scope.Namespace); err != nil {
			b.Error(err)
		}
		if _, err := db.db.ExecContext(ctx, `DELETE FROM agent_runtime_memory_subjects WHERE namespace=$1`, scope.Namespace); err != nil {
			b.Error(err)
		}
		if _, err := db.db.ExecContext(ctx, `DELETE FROM agent_runtime_memory_policies WHERE namespace=$1`, scope.Namespace); err != nil {
			b.Error(err)
		}
	}()
	if _, err := db.db.ExecContext(ctx, `INSERT INTO agent_runtime_memory_records(namespace,project_id,subject_id,collection_id,memory_id,revision,subject_generation,policy_generation,kind,content,topics,status,producer,policy_version,created_at,updated_at,expires_at)
		SELECT $1,$2,$3,$4,'record-'||lpad(i::text,4,'0'),1,1,1,'preference',repeat('coffee tea preference ',24),'["drink","preference"]'::jsonb,'active','benchmark','v1',now(),now()+i*interval '1 second',now()+interval '1 day' FROM generate_series(1,1000) i`, scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID); err != nil {
		b.Fatal(err)
	}
	store := NewMemoryStore(db)
	req := memory.ListRequest{Scope: scope, Limit: 200, CandidateLimit: 200, CandidateBytes: memory.DefaultLimits().MaxCandidateBytes}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		result, err := store.ListRecords(ctx, req)
		if err != nil || len(result.Records) != 200 {
			b.Fatalf("ListRecords candidates=%d: %v", len(result.Records), err)
		}
	}
	b.StopTimer()
}

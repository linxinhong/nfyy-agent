package postgres

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

type memoryListQueryCapture struct {
	query string
	args  []any
}

func (c *memoryListQueryCapture) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "SELECT r.memory_id,r.revision,") {
		c.query, c.args = data.SQL, append([]any(nil), data.Args...)
	}
	return ctx
}
func (*memoryListQueryCapture) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestMemoryPostgresCandidateBudgetAndActualPlan(t *testing.T) {
	db := testDB(t)
	truncateTrustedMemory(t, db)
	config, err := pgx.ParseConfig(os.Getenv("POSTGRES_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	capture := &memoryListQueryCapture{}
	config.Tracer = capture
	tracedDB := &DB{db: stdlib.OpenDB(*config)}
	defer tracedDB.Close()
	store := NewMemoryStore(tracedDB)
	scope := memory.Scope{Namespace: "candidate-budget", ProjectID: "project", SubjectID: "subject", CollectionID: "profile"}
	rawCreate(t, store, scope, "record", "<>&\"\n\u2028\u2029中文")
	rawCreate(t, store, scope, "record2", "second body")
	req := memory.ListRequest{Scope: scope, Limit: 200, CandidateLimit: 200, CandidateBytes: memory.DefaultLimits().MaxCandidateBytes}
	listed, err := store.ListRecords(context.Background(), req)
	if err != nil || len(listed.Records) != 2 {
		t.Fatalf("initial candidates=%d err=%v", len(listed.Records), err)
	}
	encoded, err := json.Marshal(listed.Records)
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []int{-1, 0, 1} {
		req.CandidateBytes = len(encoded) + delta
		got, err := store.ListRecords(context.Background(), req)
		want := 2
		if delta < 0 {
			want = 1
		}
		if err != nil || len(got.Records) != want || got.Truncated != (delta < 0) {
			t.Fatalf("budget delta=%d candidates=%d truncated=%v err=%v", delta, len(got.Records), got.Truncated, err)
		}
	}
	// Seed only this disposable test scope. EXPLAIN the SQL and parameters
	// captured from the real adapter call, including its source-validity guard.
	if _, err := db.db.Exec(`INSERT INTO agent_runtime_memory_records(namespace,project_id,subject_id,collection_id,memory_id,revision,subject_generation,policy_generation,kind,content,topics,status,producer,policy_version,created_at,updated_at,expires_at)
		SELECT $1,$2,$3,$4,'plan-'||lpad(i::text,4,'0'),1,1,1,'preference',repeat('coffee tea preference ',24),'["drink","preference"]'::jsonb,'active','benchmark','v1',now(),now()+i*interval '1 second',now()+interval '1 day' FROM generate_series(1,998) i`, scope.Namespace, scope.ProjectID, scope.SubjectID, scope.CollectionID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`ANALYZE agent_runtime_memory_records`); err != nil {
		t.Fatal(err)
	}
	foreign := scope
	foreign.Namespace = "foreign-candidate-budget"
	rawCreate(t, store, foreign, "foreign-record", "foreign body")
	for _, sourceScope := range []memory.Scope{scope, foreign} {
		count := 1
		if sourceScope == foreign {
			count = 100
		}
		if _, err := db.db.Exec(`INSERT INTO agent_runtime_memory_sources(namespace,project_id,subject_id,collection_id,source_ref,source_version,source_digest,source_epoch,expires_at,verifier,revoked)
			SELECT $1,$2,$3,$4,'source-'||i,'v1','digest',1,now()+interval '1 day','host',false FROM generate_series(1,$5::integer) i`, sourceScope.Namespace, sourceScope.ProjectID, sourceScope.SubjectID, sourceScope.CollectionID, count); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.db.Exec(`UPDATE agent_runtime_memory_records SET source_ref='source-1',source_version='v1',source_epoch=1 WHERE namespace=$1 AND memory_id='plan-0998'`, scope.Namespace); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`ANALYZE agent_runtime_memory_sources`); err != nil {
		t.Fatal(err)
	}
	req.CandidateBytes = memory.DefaultLimits().MaxCandidateBytes
	listed, err = store.ListRecords(context.Background(), req)
	if err != nil || len(listed.Records) != 200 {
		t.Fatalf("plan candidates=%d err=%v", len(listed.Records), err)
	}
	if capture.query == "" {
		t.Fatal("actual ListRecords SQL not captured")
	}
	var plan []byte
	if err := db.db.QueryRow(`EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+capture.query, capture.args...).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	t.Logf("actual ListRecords SQL:\n%s\nplan:\n%s", capture.query, plan)
	var plans []struct{ Plan map[string]any }
	if err := json.Unmarshal(plan, &plans); err != nil {
		t.Fatal(err)
	}
	var checkSources func(map[string]any)
	checkSources = func(node map[string]any) {
		if node["Relation Name"] == "agent_runtime_memory_sources" && node["Actual Loops"].(float64) > 0 && node["Actual Rows"].(float64) > 1 {
			t.Fatalf("source subplan materialized foreign-scope sources: %s", plan)
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				checkSources(child.(map[string]any))
			}
		}
	}
	checkSources(plans[0].Plan)
	// A matching ref/version/epoch in a foreign scope must not make the
	// newest record eligible when the local source does not exist.
	if _, err := db.db.Exec(`UPDATE agent_runtime_memory_records SET source_ref='source-2' WHERE namespace=$1 AND memory_id='plan-0998'`, scope.Namespace); err != nil {
		t.Fatal(err)
	}
	listed, err = store.ListRecords(context.Background(), req)
	if err != nil || len(listed.Records) != 200 || listed.Records[0].MemoryID != "plan-0997" {
		t.Fatalf("foreign source list=%+v err=%v", listed, err)
	}
}

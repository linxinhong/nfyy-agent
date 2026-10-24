package run

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/linxinhong/xingding-agent-runtime/internal/metrictest"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

func TestRunMetrics(t *testing.T) {
	metrictest.Install()
	v := func(name string, kv ...attribute.KeyValue) float64 { return metrictest.Value(t, name, kv...) }

	beforeAdmit := v("run.admission.wait", attribute.String("layer", "cluster"), attribute.String("outcome", "admitted"))
	beforeTimeout := v("run.admission.wait", attribute.String("layer", "cluster"), attribute.String("outcome", "timeout"))
	cluster := NewSharedAdmission(1, 0)
	c, _ := NewAdmissionController(AdmissionConfig{Total: 5, PerTenant: 5, Queue: 2, WaitTimeout: 30 * time.Millisecond, Cluster: cluster})
	_, permit, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Acquire(context.Background()); err != ErrAdmissionTimeout {
		t.Fatalf("err = %v", err)
	}
	permit.Release()
	if got := v("run.admission.wait", attribute.String("layer", "cluster"), attribute.String("outcome", "admitted")) - beforeAdmit; got != 1 {
		t.Fatalf("cluster admitted delta = %v", got)
	}
	if got := v("run.admission.wait", attribute.String("layer", "cluster"), attribute.String("outcome", "timeout")) - beforeTimeout; got != 1 {
		t.Fatalf("cluster timeout delta = %v", got)
	}

	store := in_memory.New()
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)
	_ = store.CreateSession(ctx, &session.AgentSession{SessionID: "s", AgentID: "a", UserID: "u", CreatedAt: old, UpdatedAt: old})
	_ = store.CreateSession(ctx, &session.AgentSession{SessionID: "idle", AgentID: "a", UserID: "u", CreatedAt: old, UpdatedAt: old})
	_ = store.AppendRun(ctx, &session.RunOutput{RunID: "r", SessionID: "s", AgentID: "a", UserID: "u", Status: session.RunStatusRunning, LeaseTimeout: time.Minute, CreatedAt: old, UpdatedAt: old})
	beforeStale, beforeDeleted := v("run.stale_runs.expired"), v("run.sessions.deleted")
	if _, err := ExpireStaleRuns(ctx, store, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := DeleteIdleSessions(ctx, store, 24*time.Hour, 10); err != nil {
		t.Fatal(err)
	}
	if v("run.stale_runs.expired")-beforeStale != 1 || v("run.sessions.deleted")-beforeDeleted < 1 {
		t.Fatalf("stale %v deleted %v", v("run.stale_runs.expired")-beforeStale, v("run.sessions.deleted")-beforeDeleted)
	}
}

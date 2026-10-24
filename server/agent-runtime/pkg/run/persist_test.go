package run

import (
	"context"
	"errors"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

type transientRunStore struct {
	db.RunStore
	failures int
}

func (s *transientRunStore) UpdateRun(ctx context.Context, output *session.RunOutput, expected session.RunStatus) error {
	if s.failures > 0 {
		s.failures--
		return errors.New("temporary write failure")
	}
	return s.RunStore.UpdateRun(ctx, output, expected)
}

func TestFinalizeRetriesTransientWriteFailures(t *testing.T) {
	store := in_memory.New()
	if err := store.CreateSession(context.Background(), &session.AgentSession{SessionID: "s1", AgentID: "a1"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	output := &session.RunOutput{RunID: "r1", SessionID: "s1", AgentID: "a1", Status: session.RunStatusRunning}
	if err := store.AppendRun(context.Background(), output); err != nil {
		t.Fatalf("append run: %v", err)
	}
	output.Status = session.RunStatusCompleted
	flaky := &transientRunStore{RunStore: store, failures: 2}
	if err := Finalize(context.Background(), flaky, output, session.RunStatusRunning); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	durable, _ := store.GetRun(context.Background(), db.RunQuery{RunID: "r1"})
	if durable == nil || durable.Status != session.RunStatusCompleted {
		t.Fatalf("durable run = %+v", durable)
	}
}

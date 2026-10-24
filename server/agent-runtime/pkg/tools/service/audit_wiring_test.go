package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/catalog"
)

// failingJournal wraps the memory journal and fails the requested write,
// simulating a durable-store outage (side effect must not run).
type failingJournal struct {
	*run.MemoryExecutionJournal
}

func (f *failingJournal) BeginAttemptAndAppendRequested(context.Context, execution.NamespaceScope, *run.Attempt, *run.AuditRequestedRecord, *run.OutboxEvent) error {
	return errors.New("journal unavailable")
}

func TestServiceAuditRequestedBeforeOutcome(t *testing.T) {
	s, tool := buildService(t, &scriptedAuthorizer{
		discovery: func(context.Context, execution.Principal, catalog.ToolDescriptor) (AuthorizationDecision, error) {
			return allow(false), nil
		},
		execute: func(context.Context, AuthorizeRequest) (AuthorizationDecision, error) {
			return allow(false), nil
		},
	}, "")
	journal := run.NewMemoryExecutionJournal()
	exporter := run.NewMemoryAuditExporter()
	s.SetJournal(journal, exporter, "worker-1")

	ctx := ctxWithPrincipal("ns")
	prepared, err := s.Prepare(ctx, PrepareRequest{ToolName: "t", Arguments: json.RawMessage(`{"x":1}`)})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := s.Execute(ctx, prepared.Ref); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if tool.calls.Load() != 1 {
		t.Fatalf("tool calls = %d, want 1", tool.calls.Load())
	}

	delivered := exporter.Delivered()
	if len(delivered) != 2 {
		t.Fatalf("delivered events = %d, want 2 (requested + outcome)", len(delivered))
	}
	// Both events are keyed by their stable event IDs and carry no content
	// fields (the AuditEvent type is a typed allowlist: no arguments/result).
	for id, ev := range delivered {
		if ev.EventID != id {
			t.Fatalf("delivered key %q != event id %q", id, ev.EventID)
		}
		if strings.Contains(ev.RunID, "{") {
			t.Fatalf("event run id looks like raw content: %q", ev.RunID)
		}
	}
}

func TestServiceAuditJournalFailureFailsClosed(t *testing.T) {
	s, tool := buildService(t, &scriptedAuthorizer{
		discovery: func(context.Context, execution.Principal, catalog.ToolDescriptor) (AuthorizationDecision, error) {
			return allow(false), nil
		},
		execute: func(context.Context, AuthorizeRequest) (AuthorizationDecision, error) {
			return allow(false), nil
		},
	}, "")
	s.SetJournal(&failingJournal{MemoryExecutionJournal: run.NewMemoryExecutionJournal()}, nil, "worker-1")

	ctx := ctxWithPrincipal("ns")
	prepared, err := s.Prepare(ctx, PrepareRequest{ToolName: "t"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := s.Execute(ctx, prepared.Ref); err == nil || !strings.Contains(err.Error(), "audit requested") {
		t.Fatalf("execute err = %v, want audit-requested failure", err)
	}
	if tool.calls.Load() != 0 {
		t.Fatalf("tool must not run when the audit journal is unavailable")
	}
}

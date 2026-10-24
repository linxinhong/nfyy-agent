package audit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/identity"
)

func sampleEvent() audit.Event {
	e := audit.NewEvent(audit.EventAuthKeyCreated, audit.OutcomeAllowed)
	e.Actor = &identity.Principal{UserID: "user_123"}
	e.WorkspaceID = "secops"
	e.TraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	e.RequestID = "req-42"
	e.Resource = "api-key/key_1"
	e.Detail = map[string]any{"key_id": "key_1", "prefix": "agk_live_abc"}
	return e
}

// Reliability contract: RequireWrite returns an observable error when the
// audit write fails; callers deny the operation in that case.
func TestRequireWriteFailsClosedOnWriterError(t *testing.T) {
	w := &audit.MemoryWriter{Err: errors.New("audit storage unavailable")}
	err := audit.RequireWrite(context.Background(), w, sampleEvent())
	if err == nil {
		t.Fatalf("undeliverable security audit event must surface an error")
	}
	if len(w.Events()) != 0 {
		t.Fatalf("failed write must not record the event")
	}
}

func TestRequireWriteNilWriter(t *testing.T) {
	if err := audit.RequireWrite(context.Background(), nil, sampleEvent()); err == nil {
		t.Fatalf("nil writer must fail closed, not drop the event")
	}
}

func TestRequireWriteHappyPath(t *testing.T) {
	w := &audit.MemoryWriter{}
	e := sampleEvent()
	if err := audit.RequireWrite(context.Background(), w, e); err != nil {
		t.Fatalf("deliverable event must write cleanly: %v", err)
	}
	got := w.Events()
	if len(got) != 1 {
		t.Fatalf("expected one event, got %d", len(got))
	}
	if got[0].Type != audit.EventAuthKeyCreated || got[0].Outcome != audit.OutcomeAllowed {
		t.Fatalf("event fields lost: %+v", got[0])
	}
	if got[0].Time.Location() != time.UTC {
		t.Fatalf("audit timestamps must be UTC")
	}
}

func TestValidateNoSecrets(t *testing.T) {
	cases := []struct {
		name    string
		detail  map[string]any
		wantBad bool
	}{
		{"clean", map[string]any{"key_id": "k", "count": 2.0}, false},
		{"direct api_key", map[string]any{"api_key": "agk_live_x"}, true},
		{"nested secret", map[string]any{"request": map[string]any{"authorization": "Bearer z"}}, true},
		{"token claim", map[string]any{"access_token": "z"}, true},
		{"tokens_count allowed", map[string]any{"total_tokens": 42.0}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := sampleEvent()
			e.Detail = tc.detail
			err := audit.ValidateNoSecrets(e)
			if tc.wantBad && err == nil {
				t.Fatalf("secret-shaped detail must be rejected")
			}
			if !tc.wantBad && err != nil {
				t.Fatalf("clean detail rejected: %v", err)
			}
		})
	}
}

func TestMemoryWriterConcurrent(t *testing.T) {
	w := &audit.MemoryWriter{}
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				_ = w.Write(context.Background(), sampleEvent())
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if got := len(w.Events()); got != 400 {
		t.Fatalf("expected 400 events, got %d", got)
	}
}

func TestEventIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := audit.NewEvent(audit.EventLLMRequest, audit.OutcomeAllowed).ID
		if seen[id] {
			t.Fatalf("duplicate event id: %s", id)
		}
		seen[id] = true
	}
}

// TASK-038: RequireWriteBatch delivers the set in one call when the writer
// batches, falls back to per-event writes otherwise, and fails closed on
// any validation or delivery error.
func TestRequireWriteBatch(t *testing.T) {
	events := func() []audit.Event {
		e1 := audit.NewEvent("policy.decision", audit.OutcomeAllowed)
		e1.Detail = map[string]any{"effect": "allow"}
		e2 := audit.NewEvent("llm.request", audit.OutcomeAllowed)
		return []audit.Event{e1, e2}
	}

	t.Run("batched", func(t *testing.T) {
		bw := &stubBatchWriter{}
		if err := audit.RequireWriteBatch(context.Background(), bw, events()); err != nil {
			t.Fatal(err)
		}
		if bw.batches != 1 || len(bw.got) != 2 {
			t.Fatalf("expected one batched call with 2 events: %d %d", bw.batches, len(bw.got))
		}
	})
	t.Run("fallback loop", func(t *testing.T) {
		w := &stubWriter{}
		if err := audit.RequireWriteBatch(context.Background(), w, events()); err != nil {
			t.Fatal(err)
		}
		if w.writes != 2 {
			t.Fatalf("fallback must write each event: %d", w.writes)
		}
	})
	t.Run("delivery failure fails closed", func(t *testing.T) {
		bw := &stubBatchWriter{err: errors.New("db down")}
		if err := audit.RequireWriteBatch(context.Background(), bw, events()); err == nil {
			t.Fatal("delivery failure must return an error")
		}
	})
	t.Run("secret validation fails closed", func(t *testing.T) {
		bad := audit.NewEvent("policy.decision", audit.OutcomeAllowed)
		bad.Detail = map[string]any{"api_key": "agk_should_not_be_here"}
		if err := audit.RequireWriteBatch(context.Background(), &stubBatchWriter{}, []audit.Event{bad}); err == nil {
			t.Fatal("secret-shaped detail must fail closed")
		}
	})
}

type stubBatchWriter struct {
	batches int
	got     []audit.Event
	err     error
}

func (s *stubBatchWriter) Write(ctx context.Context, e audit.Event) error { return s.err }
func (s *stubBatchWriter) WriteBatch(_ context.Context, events []audit.Event) error {
	s.batches++
	s.got = append(s.got, events...)
	return s.err
}

type stubWriter struct {
	writes int
}

func (s *stubWriter) Write(ctx context.Context, e audit.Event) error { s.writes++; return nil }

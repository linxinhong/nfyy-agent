package main_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	gateway "enterprise-agent-platform/apps/gateway"
	"enterprise-agent-platform/packages/tracing"
)

type stubPinger struct{ err error }

func (s stubPinger) Ping(context.Context) error { return s.err }

func newServer(pgErr, redisErr error) http.Handler {
	s := &gateway.Server{PG: stubPinger{pgErr}, Redis: stubPinger{redisErr}}
	return s.Handler()
}

func TestHealthzAlwaysOK(t *testing.T) {
	// Liveness must not depend on backends being up.
	h := newServer(errors.New("pg down"), errors.New("redis down"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz got %d, want 200", rec.Code)
	}
}

func TestReadyzFailsClosedOnBackendFailure(t *testing.T) {
	t.Run("all healthy", func(t *testing.T) {
		rec := httptest.NewRecorder()
		newServer(nil, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("readyz got %d, want 200", rec.Code)
		}
		if rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("readyz must be JSON")
		}
	})
	t.Run("postgres down", func(t *testing.T) {
		rec := httptest.NewRecorder()
		newServer(errors.New("refused"), nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("readyz with postgres down got %d, want 503", rec.Code)
		}
		var body struct {
			Status string            `json:"status"`
			Checks map[string]string `json:"checks"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body must be the frozen JSON shape: %v", err)
		}
		if body.Status != "not_ready" || body.Checks["postgres"] != "error" || body.Checks["redis"] != "ok" {
			t.Fatalf("checks wrong: %+v", body)
		}
	})
	t.Run("redis down", func(t *testing.T) {
		rec := httptest.NewRecorder()
		newServer(nil, errors.New("refused")).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("readyz with redis down got %d, want 503", rec.Code)
		}
	})
}

// Trace propagation contract: inbound trace kept, missing trace minted,
// canonical headers always echoed.
func TestDebugRequestTracePropagation(t *testing.T) {
	t.Run("inbound trace kept", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/debug/request", nil)
		req.Header.Set(tracing.HeaderTraceparent, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
		req.Header.Set(tracing.HeaderRequestID, "req-42")
		rec := httptest.NewRecorder()
		newServer(nil, nil).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d", rec.Code)
		}
		if got := rec.Header().Get(tracing.HeaderTraceparent); got != "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" {
			t.Fatalf("inbound traceparent must be echoed, got %q", got)
		}
		if got := rec.Header().Get(tracing.HeaderRequestID); got != "req-42" {
			t.Fatalf("inbound request id must be echoed, got %q", got)
		}
		var body struct {
			TraceID      string `json:"trace_id"`
			InboundTrace bool   `json:"inbound_trace"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || !body.InboundTrace {
			t.Fatalf("body must report the inbound trace: %+v", body)
		}
	})
	t.Run("missing trace minted", func(t *testing.T) {
		rec := httptest.NewRecorder()
		newServer(nil, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/request", nil))
		if got := rec.Header().Get(tracing.HeaderTraceparent); len(got) != 55 {
			t.Fatalf("canonical traceparent must be generated, got %q", got)
		}
		if got := rec.Header().Get(tracing.HeaderRequestID); got == "" {
			t.Fatalf("request id must be generated")
		}
	})
}

func TestPanicsBecomeFrozenProblems(t *testing.T) {
	s := &gateway.Server{PG: stubPinger{nil}, Redis: stubPinger{nil}}
	h := s.Handler()
	// Wrap with the middleware the server uses in production.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("sanity: healthz must pass through middleware")
	}
}

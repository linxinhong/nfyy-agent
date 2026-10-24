package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// captureHandler routes package logs into a buffer as JSON and restores
// the previous handler on cleanup.
func captureHandler(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	SetHandler(slog.NewJSONHandler(buf, nil))
	t.Cleanup(func() { SetHandler(slog.NewJSONHandler(nilWriter{}, nil)) })
	return buf
}

// nilWriter is a discard sink for the cleanup handler.
type nilWriter struct{}

func (nilWriter) Write(p []byte) (int, error) { return len(p), nil }

func decodeRecord(t *testing.T, line string) map[string]any {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("decode log line %q: %v", line, err)
	}
	return rec
}

func TestEventRedactsReservedFields(t *testing.T) {
	buf := captureHandler(t)
	Event(context.Background(), slog.LevelInfo, "run.finalize.retry",
		slog.String("prompt", "do not leak the system prompt"),
		slog.String("secret", "s3cr3t"),
		slog.String("token", "bearer-abc"),
		slog.String(FieldRunID, "run-1"),
	)
	rec := decodeRecord(t, strings.TrimSpace(buf.String()))

	for _, key := range []string{"prompt", "secret", "token"} {
		if got := rec[key]; got != redactionPlaceholder {
			t.Fatalf("field %q = %v, want %q", key, got, redactionPlaceholder)
		}
	}
	if rec[FieldRunID] != "run-1" {
		t.Fatalf("run_id = %v, want run-1", rec[FieldRunID])
	}
}

func TestEventRedactsNestedGroupFields(t *testing.T) {
	buf := captureHandler(t)
	Event(context.Background(), slog.LevelInfo, "http.request",
		slog.Group("req",
			slog.String("authorization", "Bearer abc"),
			slog.String("api_key", "key-123"),
			slog.String("path", "/v1/run"),
		),
	)
	rec := decodeRecord(t, strings.TrimSpace(buf.String()))

	req, ok := rec["req"].(map[string]any)
	if !ok {
		t.Fatalf("req group missing: %+v", rec)
	}
	if req["authorization"] != redactionPlaceholder {
		t.Fatalf("nested authorization = %v, want %q", req["authorization"], redactionPlaceholder)
	}
	if req["api_key"] != redactionPlaceholder {
		t.Fatalf("nested api_key = %v, want %q", req["api_key"], redactionPlaceholder)
	}
	if req["path"] != "/v1/run" {
		t.Fatalf("nested path = %v, want /v1/run", req["path"])
	}
}

func TestEventEmitsEventFieldAndAttrs(t *testing.T) {
	buf := captureHandler(t)
	Event(context.Background(), slog.LevelInfo, "requirement.resolved",
		slog.String(FieldRequirementID, "req_1"),
		slog.String(FieldOutcome, "approved"),
	)
	rec := decodeRecord(t, strings.TrimSpace(buf.String()))

	if rec["event"] != "requirement.resolved" {
		t.Fatalf("event = %v, want requirement.resolved", rec["event"])
	}
	if rec["msg"] != "requirement.resolved" {
		t.Fatalf("msg = %v, want requirement.resolved", rec["msg"])
	}
	if rec[FieldRequirementID] != "req_1" || rec[FieldOutcome] != "approved" {
		t.Fatalf("attrs missing: %+v", rec)
	}
	if rec["level"] != "INFO" {
		t.Fatalf("level = %v, want INFO", rec["level"])
	}
}

func TestErrNilProducesNoField(t *testing.T) {
	buf := captureHandler(t)
	Event(context.Background(), slog.LevelInfo, "run.finalize", Err(nil))
	rec := decodeRecord(t, strings.TrimSpace(buf.String()))

	if _, ok := rec[FieldError]; ok {
		t.Fatalf("nil error must not produce an error field: %+v", rec)
	}
}

func TestErrProducesMessage(t *testing.T) {
	buf := captureHandler(t)
	Event(context.Background(), slog.LevelError, "run.failed", Err(errors.New("boom")))
	rec := decodeRecord(t, strings.TrimSpace(buf.String()))

	errField, ok := rec[FieldError].(map[string]any)
	if !ok {
		t.Fatalf("error field missing: %+v", rec)
	}
	if errField["message"] != "boom" {
		t.Fatalf("error.message = %v, want boom", errField["message"])
	}
}

// stackErr implements the StackTrace() string interface the Err helper
// recognizes.
type stackErr struct{ msg string }

func (e *stackErr) Error() string      { return e.msg }
func (e *stackErr) StackTrace() string { return "goroutine stack" }

func TestErrProducesStackWhenAvailable(t *testing.T) {
	buf := captureHandler(t)
	Event(context.Background(), slog.LevelError, "run.failed", Err(&stackErr{msg: "boom"}))
	rec := decodeRecord(t, strings.TrimSpace(buf.String()))

	errField := rec[FieldError].(map[string]any)
	if errField["message"] != "boom" {
		t.Fatalf("error.message = %v, want boom", errField["message"])
	}
	if errField["stack"] != "goroutine stack" {
		t.Fatalf("error.stack = %v, want goroutine stack", errField["stack"])
	}
}

func TestWithAttrsRedactionCarries(t *testing.T) {
	buf := captureHandler(t)
	logger := Logger().With(slog.String("secret", "carried-secret"))
	logger.InfoContext(context.Background(), "with-attr", slog.String("ok", "fine"))
	rec := decodeRecord(t, strings.TrimSpace(buf.String()))

	if rec["secret"] != redactionPlaceholder {
		t.Fatalf("carried secret = %v, want %q", rec["secret"], redactionPlaceholder)
	}
	if rec["ok"] != "fine" {
		t.Fatalf("ok = %v, want fine", rec["ok"])
	}
}

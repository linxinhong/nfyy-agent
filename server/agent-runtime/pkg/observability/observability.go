// Package observability provides the project-wide structured logging
// conventions for xingding-agent-runtime.
//
// Use the package-level Logger and the Event helper instead of calling
// log/slog APIs directly: the package owns a redacting handler that
// guarantees reserved fields (prompt/secret/token/headers/body/...)
// never reach the output, and it keeps every record on the closed field
// vocabulary defined here. See doc.go for the full conventions.
package observability

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// Field names in the closed vocabulary. Use these constants (or the Err
// helper) when logging agent-run telemetry so downstream consumers can
// rely on stable keys.
const (
	FieldRunID         = "run_id"
	FieldSessionID     = "session_id"
	FieldAgentID       = "agent_id"
	FieldToolName      = "tool_name"
	FieldModelRound    = "model_round"
	FieldRequirementID = "requirement_id"
	FieldDurationMs    = "duration_ms"
	FieldOutcome       = "outcome"
	FieldError         = "error"
)

// redactedFields are reserved keys whose values are replaced with
// [redacted] at write time, regardless of who logged them. The list is
// intentionally conservative: content and credentials must never appear
// in logs.
var redactedFields = map[string]struct{}{
	"prompt":        {},
	"secret":        {},
	"token":         {},
	"headers":       {},
	"body":          {},
	"authorization": {},
	"api_key":       {},
	// Tool/model content (RC-10 step 6 / global constraint #7): never in
	// logs or telemetry; only hashes, sizes, revisions, status and reason
	// codes are allowed.
	"arguments": {},
	"result":    {},
	"results":   {},
	"messages":  {},
}

// redactionPlaceholder replaces blacklisted values in output.
const redactionPlaceholder = "[redacted]"

var (
	mu      sync.RWMutex
	handler slog.Handler = newRedactingHandler(slog.NewJSONHandler(os.Stderr, nil))
)

// Logger returns the package-level logger. Handlers derived from it keep
// the redaction guarantee.
func Logger() *slog.Logger {
	mu.RLock()
	defer mu.RUnlock()
	return slog.New(handler)
}

// SetHandler replaces the package-level handler. It is intended for tests
// and for deployments that need to route logs elsewhere (files, remote
// sinks); the redaction wrapper is applied on top of whatever handler is
// passed, so the reserved-field guarantee survives replacement.
func SetHandler(h slog.Handler) {
	if h == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	handler = newRedactingHandler(h)
}

// Event emits one structured log record under a dotted event name (for
// example "run.finalize.retry" or "requirement.resolved"). The event name
// is both the record message and the value of the "event" field, so
// filtering by event works at the log level and in JSON.
func Event(ctx context.Context, level slog.Level, event string, attrs ...slog.Attr) {
	if event == "" {
		return
	}
	all := make([]slog.Attr, 0, len(attrs)+1)
	all = append(all, slog.String("event", event))
	all = append(all, attrs...)
	Logger().LogAttrs(ctx, level, event, all...)
}

// Err normalizes an error into the "error" attribute group: message plus
// stack when the error exposes one via the StackTrace() string interface
// (for example errors produced by pkg/errors). A nil error yields an empty
// attribute, which slog omits from output.
func Err(err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}
	attrs := []slog.Attr{slog.String("message", err.Error())}
	if st, ok := err.(interface{ StackTrace() string }); ok && st.StackTrace() != "" {
		attrs = append(attrs, slog.String("stack", st.StackTrace()))
	}
	return slog.Group(FieldError, anyAttrs(attrs)...)
}

// anyAttrs converts a slice of Attr to a slice of any for variadic
// slog constructors.
func anyAttrs(attrs []slog.Attr) []any {
	out := make([]any, len(attrs))
	for i, a := range attrs {
		out[i] = a
	}
	return out
}

// redactingHandler wraps a slog.Handler and replaces the values of
// blacklisted keys with [redacted] before delegating to the inner handler.
type redactingHandler struct {
	inner slog.Handler
}

func newRedactingHandler(inner slog.Handler) slog.Handler {
	return &redactingHandler{inner: inner}
}

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	redactRecord(&r)
	return h.inner.Handle(ctx, r)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &redactingHandler{inner: h.inner.WithAttrs(redactAttrs(attrs))}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{inner: h.inner.WithGroup(name)}
}

func redactRecord(r *slog.Record) {
	var attrs []slog.Attr
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, redactAttr(a))
		return true
	})
	rebuilt := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	rebuilt.AddAttrs(attrs...)
	*r = rebuilt
}

func redactAttrs(attrs []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, redactAttr(a))
	}
	return out
}

func redactAttr(a slog.Attr) slog.Attr {
	if isRedactedKey(a.Key) {
		return slog.String(a.Key, redactionPlaceholder)
	}
	if a.Value.Kind() == slog.KindGroup {
		children := a.Value.Group()
		redacted := make([]slog.Attr, 0, len(children))
		for _, c := range children {
			redacted = append(redacted, redactAttr(c))
		}
		return slog.Group(a.Key, anyAttrs(redacted)...)
	}
	return a
}

func isRedactedKey(key string) bool {
	_, ok := redactedFields[strings.ToLower(key)]
	return ok
}

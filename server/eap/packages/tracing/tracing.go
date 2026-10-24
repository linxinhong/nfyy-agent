// Package tracing defines the EAP trace propagation contract: W3C Trace
// Context ("traceparent") plus "X-Request-ID". The Agent Gateway accepts an
// inbound traceparent or creates one; every response carries a canonical
// traceparent and X-Request-ID; internal calls propagate the context. This is
// the wire-format foundation that the OpenTelemetry SDK wiring will adopt in a
// later slice, so headers and ids stay stable across that migration.
package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// Canonical HTTP headers owned by this contract.
const (
	HeaderTraceparent = "Traceparent"
	HeaderRequestID   = "X-Request-ID"
)

// SpanContext identifies one request position in the trace tree.
type SpanContext struct {
	TraceID    string // 32 hex chars
	SpanID     string // 16 hex chars
	TraceFlags byte
}

// Traceparent renders the W3C traceparent header value.
func (c SpanContext) Traceparent() string {
	return fmt.Sprintf("00-%s-%s-%02x", c.TraceID, c.SpanID, c.TraceFlags)
}

// ParseTraceparent parses a W3C traceparent value strictly. The all-zero ids
// and the 0xff version are invalid per spec.
func ParseTraceparent(v string) (SpanContext, error) {
	parts := strings.Split(strings.TrimSpace(v), "-")
	if len(parts) != 4 {
		return SpanContext{}, fmt.Errorf("traceparent needs 4 fields, got %d", len(parts))
	}
	if parts[0] != "00" {
		return SpanContext{}, fmt.Errorf("unsupported traceparent version %q", parts[0])
	}
	if len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return SpanContext{}, fmt.Errorf("traceparent field lengths invalid")
	}
	if !isHex(parts[1]) || !isHex(parts[2]) || !isHex(parts[3]) {
		return SpanContext{}, fmt.Errorf("traceparent fields are not hex")
	}
	if allZero(parts[1]) || allZero(parts[2]) {
		return SpanContext{}, fmt.Errorf("traceparent ids must not be all zero")
	}
	flags, err := hex.DecodeString(parts[3])
	if err != nil {
		return SpanContext{}, fmt.Errorf("traceparent flags invalid: %w", err)
	}
	return SpanContext{TraceID: parts[1], SpanID: parts[2], TraceFlags: flags[0]}, nil
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return len(s) > 0
}

func allZero(s string) bool { return strings.Trim(s, "0") == "" }

// NewSpanContext mints a random sampled root context.
func NewSpanContext() SpanContext {
	var t [16]byte
	var s [8]byte
	if _, err := rand.Read(t[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	if _, err := rand.Read(s[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return SpanContext{TraceID: hex.EncodeToString(t[:]), SpanID: hex.EncodeToString(s[:]), TraceFlags: 0x01}
}

// NewRequestID mints a UUIDv4-shaped request id.
func NewRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// RequestMeta is the per-request observability identity.
type RequestMeta struct {
	Trace        SpanContext
	RequestID    string
	InboundTrace bool // true when the caller supplied a valid traceparent
}

// Extract reads the propagation headers, creating values when absent or
// invalid. It never fails: observability identity always exists.
func Extract(h http.Header) RequestMeta {
	meta := RequestMeta{}
	if sc, err := ParseTraceparent(h.Get(HeaderTraceparent)); err == nil {
		meta.Trace = sc
		meta.InboundTrace = true
	} else {
		meta.Trace = NewSpanContext()
	}
	if rid := strings.TrimSpace(h.Get(HeaderRequestID)); rid != "" && len(rid) <= 128 {
		meta.RequestID = rid
	} else {
		meta.RequestID = NewRequestID()
	}
	return meta
}

// EnsureHeaders writes the canonical propagation headers back onto h so
// upstream callers and logs observe the exact ids this service uses.
func EnsureHeaders(h http.Header) RequestMeta {
	meta := Extract(h)
	h.Set(HeaderTraceparent, meta.Trace.Traceparent())
	h.Set(HeaderRequestID, meta.RequestID)
	return meta
}

type ctxKey struct{}

// WithRequest stores the request observability identity in ctx.
func WithRequest(ctx context.Context, meta RequestMeta) context.Context {
	return context.WithValue(ctx, ctxKey{}, meta)
}

// FromContext returns the identity stored by WithRequest, if any.
func FromContext(ctx context.Context) (RequestMeta, bool) {
	meta, ok := ctx.Value(ctxKey{}).(RequestMeta)
	return meta, ok
}

// TraceID is a convenience accessor returning the current trace id or "".
func TraceID(ctx context.Context) string {
	if meta, ok := FromContext(ctx); ok {
		return meta.Trace.TraceID
	}
	return ""
}

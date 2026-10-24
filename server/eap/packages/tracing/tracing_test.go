package tracing_test

import (
	"context"
	"net/http"
	"testing"

	"enterprise-agent-platform/packages/tracing"
)

func TestParseTraceparent(t *testing.T) {
	valid := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	sc, err := tracing.ParseTraceparent(valid)
	if err != nil {
		t.Fatalf("valid traceparent rejected: %v", err)
	}
	if sc.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || sc.SpanID != "00f067aa0ba902b7" || sc.TraceFlags != 0x01 {
		t.Fatalf("parsed fields wrong: %+v", sc)
	}
	if sc.Traceparent() != valid {
		t.Fatalf("re-render mismatch: %s", sc.Traceparent())
	}

	for _, bad := range []string{
		"",
		"zz-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", // version
		"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", // forbidden version
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01", // zero trace id
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01", // zero span id
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7",    // missing flags
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-0g", // bad hex
	} {
		if _, err := tracing.ParseTraceparent(bad); err == nil {
			t.Errorf("invalid traceparent %q accepted", bad)
		}
	}
}

func TestExtractKeepsValidInboundTrace(t *testing.T) {
	h := http.Header{}
	h.Set(tracing.HeaderTraceparent, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	h.Set(tracing.HeaderRequestID, "req-42")
	meta := tracing.Extract(h)
	if !meta.InboundTrace || meta.Trace.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("inbound trace must be preserved: %+v", meta)
	}
	if meta.RequestID != "req-42" {
		t.Fatalf("inbound request id must be preserved")
	}
}

func TestExtractGeneratesWhenMissingOrBroken(t *testing.T) {
	for name, h := range map[string]http.Header{
		"absent": {},
		"broken": {"Traceparent": {"garbage"}},
	} {
		meta := tracing.Extract(h)
		if meta.InboundTrace {
			t.Errorf("%s: inbound flag must be false", name)
		}
		if len(meta.Trace.TraceID) != 32 || len(meta.Trace.SpanID) != 16 {
			t.Errorf("%s: generated ids have wrong length: %+v", name, meta)
		}
		if meta.RequestID == "" {
			t.Errorf("%s: request id must always exist", name)
		}
	}
}

func TestEnsureHeadersWritesCanonicalHeaders(t *testing.T) {
	h := http.Header{}
	meta := tracing.EnsureHeaders(h)
	if got := h.Get(tracing.HeaderTraceparent); got != meta.Trace.Traceparent() {
		t.Fatalf("traceparent header mismatch: %s", got)
	}
	if got := h.Get(tracing.HeaderRequestID); got != meta.RequestID {
		t.Fatalf("request id header mismatch: %s", got)
	}
}

func TestContextRoundTrip(t *testing.T) {
	meta := tracing.Extract(http.Header{})
	ctx := tracing.WithRequest(context.Background(), meta)
	got, ok := tracing.FromContext(ctx)
	if !ok || got.Trace.TraceID == "" {
		t.Fatalf("context round trip failed: %+v", got)
	}
	if tracing.TraceID(context.Background()) != "" {
		t.Fatalf("empty context must have no trace id")
	}
}

func TestGeneratedIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := tracing.NewSpanContext().TraceID
		if seen[id] {
			t.Fatalf("duplicate trace id generated: %s", id)
		}
		seen[id] = true
	}
}

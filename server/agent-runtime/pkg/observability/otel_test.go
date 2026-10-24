package observability

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

func TestSafeAttrsDropsReservedFields(t *testing.T) {
	out := SafeAttrs(
		attribute.String("tool_name", "lookup"),
		attribute.String("arguments", `{"secret":"x"}`),
		attribute.String("prompt", "system prompt"),
		attribute.String("authorization", "Bearer abc"),
		attribute.String("body", "raw body"),
		attribute.String("arguments_hash", "abc123"),
		attribute.String("namespace", "ns"),
	)
	got := make(map[string]struct{}, len(out))
	for _, kv := range out {
		got[string(kv.Key)] = struct{}{}
	}
	for _, key := range []string{"arguments", "prompt", "authorization", "body"} {
		if _, ok := got[key]; ok {
			t.Fatalf("reserved key %q was not dropped", key)
		}
	}
	for _, key := range []string{"tool_name", "arguments_hash", "namespace"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("safe key %q was dropped", key)
		}
	}
}

// TestTracerMeterNoopSafe verifies the runtime's default no-op provider path:
// instrumentation is always safe (no panic, non-nil handles) even when the
// host has not installed an SDK TracerProvider/MeterProvider.
func TestTracerMeterNoopSafe(t *testing.T) {
	ctx := context.Background()
	ctx, span := Tracer("xingding.tools").Start(ctx, "tool.execute")
	span.SetAttributes(SafeAttrs(
		attribute.String("tool_name", "lookup"),
		attribute.String("arguments", `{"x":1}`),
		attribute.String("arguments_hash", "h"),
	)...)
	span.End()

	if Tracer("xingding.tools") == nil {
		t.Fatal("Tracer returned nil")
	}
	if Meter("xingding.tools") == nil {
		t.Fatal("Meter returned nil")
	}
}

package observability

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Tracer returns the OpenTelemetry tracer for the named instrumentation
// scope, using the globally registered TracerProvider. Hosts install an SDK
// provider via otel.SetTracerProvider at startup; the default is a no-op
// provider, so instrumentation is always safe and cheap when no SDK is
// present.
func Tracer(name string) trace.Tracer { return otel.Tracer(name) }

// Meter returns the OpenTelemetry meter for the named instrumentation scope.
func Meter(name string) metric.Meter { return otel.Meter(name) }

// SafeAttrs filters attributes down to the typed allowlist: reserved
// content and credential keys (prompt/secret/token/headers/body/
// authorization/api_key) are dropped. Every span and metric attribute must
// pass through SafeAttrs so no prompt, arguments, result, or credential
// value ever reaches telemetry (RC-10 step 6).
func SafeAttrs(attrs ...attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, kv := range attrs {
		if isRedactedKey(string(kv.Key)) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

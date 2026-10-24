// Package metrictest installs an in-memory OpenTelemetry MeterProvider for
// tests and reads recorded values back.
package metrictest

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var (
	once   sync.Once
	reader *sdkmetric.ManualReader
)

// Install sets the global MeterProvider once per test binary; instruments
// created earlier through the global meter delegate to it.
func Install() {
	once.Do(func() {
		reader = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	})
}

func matches(set attribute.Set, want []attribute.KeyValue) bool {
	for _, kv := range want {
		if v, ok := set.Value(kv.Key); !ok || v != kv.Value {
			return false
		}
	}
	return true
}

// Value sums the named metric over data points carrying all of attrs: the
// sum for counters and gauges, the count for histograms.
func Value(t *testing.T, name string, attrs ...attribute.KeyValue) float64 {
	t.Helper()
	Install()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	total := 0.0
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range data.DataPoints {
					if matches(p.Attributes, attrs) {
						total += float64(p.Value)
					}
				}
			case metricdata.Sum[float64]:
				for _, p := range data.DataPoints {
					if matches(p.Attributes, attrs) {
						total += p.Value
					}
				}
			case metricdata.Gauge[int64]:
				for _, p := range data.DataPoints {
					if matches(p.Attributes, attrs) {
						total += float64(p.Value)
					}
				}
			case metricdata.Histogram[float64]:
				for _, p := range data.DataPoints {
					if matches(p.Attributes, attrs) {
						total += float64(p.Count)
					}
				}
			}
		}
	}
	return total
}

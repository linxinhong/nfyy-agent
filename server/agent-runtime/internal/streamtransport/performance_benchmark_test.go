//go:build go1.24

package streamtransport

import (
	"context"
	"testing"
	"time"

	streamcfg "github.com/linxinhong/xingding-agent-runtime/pkg/stream"
)

const perfMeasureLimit = 1 << 20

var perfMeasuredBytes int

type perfCompositePayload struct {
	Name   string
	Values []int64
}

// BenchmarkPerfLogicalPayloadBytes freezes the reflective accounting cases
// used by agent and workflow terminal/composite events. In particular, a
// typed nil stored in an interface is charged its pointer width.
func BenchmarkPerfLogicalPayloadBytes(b *testing.B) {
	var typedNil *perfCompositePayload
	cases := []struct {
		name  string
		value any
	}{
		{name: "typed_nil_pointer", value: typedNil},
		{name: "composite_struct", value: perfCompositePayload{
			Name:   "fixed-composite-payload",
			Values: []int64{1, 2, 3, 5, 8, 13, 21, 34},
		}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			want := LogicalPayloadBytes(tc.value, perfMeasureLimit)
			if want < 0 || want > perfMeasureLimit {
				b.Fatalf("invalid fixed payload size %d", want)
			}
			b.ReportAllocs()
			for b.Loop() {
				perfMeasuredBytes = LogicalPayloadBytes(tc.value, perfMeasureLimit)
			}
			b.ReportMetric(float64(want), "logical-bytes/event")
		})
	}
}

// BenchmarkPerfStreamWriter separates available-buffer delivery from the two
// bounded full-buffer exits. The timeout is deliberately short so the
// benchmark remains finite while still exercising the polling path.
func BenchmarkPerfStreamWriter(b *testing.B) {
	b.Run("available_buffer", func(b *testing.B) {
		ctx, cancel := context.WithCancel(context.Background())
		writer, output := New(ctx, cancel, streamcfg.Config{
			BufferItems:     2,
			MaxOutputBytes:  1 << 30,
			DeliveryTimeout: time.Second,
		}, func(value string, _ int) int { return len(value) }, func(error) string { return "failure" })
		b.ReportAllocs()
		for b.Loop() {
			if !writer.Emit("x", Data) {
				b.Fatal("available-buffer emit failed")
			}
			<-output
		}
		b.StopTimer()
		b.ReportMetric(1, "events/op")
		writer.Finish(nil)
		for range output {
		}
	})

	b.Run("full_queue_timeout_50us", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			ctx, cancel := context.WithCancel(context.Background())
			writer, output := New(ctx, cancel, streamcfg.Config{
				BufferItems:     2,
				MaxOutputBytes:  512,
				DeliveryTimeout: 50 * time.Microsecond,
			}, func(value string, _ int) int { return len(value) }, func(error) string { return "failure" })
			if !writer.Emit("x", Data) {
				b.Fatal("initial emit failed")
			}
			if writer.Emit("y", Data) {
				b.Fatal("full-buffer emit unexpectedly succeeded")
			}
			writer.Finish(nil)
			for range output {
			}
		}
		b.ReportMetric(2, "events/op")
	})

	b.Run("full_queue_cancelled", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			ctx, cancel := context.WithCancel(context.Background())
			writer, output := New(ctx, cancel, streamcfg.Config{
				BufferItems:     2,
				MaxOutputBytes:  512,
				DeliveryTimeout: time.Second,
			}, func(value string, _ int) int { return len(value) }, func(error) string { return "failure" })
			if !writer.Emit("x", Data) {
				b.Fatal("initial emit failed")
			}
			cancel()
			if writer.Emit("y", Data) {
				b.Fatal("cancelled full-buffer emit unexpectedly succeeded")
			}
			writer.Finish(nil)
			for range output {
			}
		}
		b.ReportMetric(2, "events/op")
	})
}

package agent_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

const baselineSeed = 20260906

// baselineModel is intentionally local and deterministic: it performs no I/O
// and has no mutable response queue that could serialize concurrent callers.
// Tool mode emits one tool call until it sees the corresponding tool result.
type baselineModel struct {
	streamEvents int
	toolCall     bool
	barrier      *cyclicBarrier
}

func (m *baselineModel) Response(ctx context.Context, messages []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
	if m.barrier != nil {
		if err := m.barrier.wait(ctx); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.toolCall {
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == models.RoleTool {
				return &models.ModelResponse{Content: "done"}, nil
			}
		}
		return &models.ModelResponse{ToolCalls: []models.ToolCall{{
			ID: "baseline-call", Type: "function",
			Function: models.FunctionCall{Name: "baseline_tool", Arguments: `{}`},
		}}}, nil
	}
	return &models.ModelResponse{Content: "done"}, nil
}

func (m *baselineModel) ResponseStream(ctx context.Context, _ []models.Message, _ models.ModelOptions) (<-chan models.StreamEvent, error) {
	events := m.streamEvents
	if events <= 0 {
		events = 8
	}
	ch := make(chan models.StreamEvent)
	go func() {
		defer close(ch)
		for i := 0; i < events; i++ {
			select {
			case ch <- models.StreamEvent{Event: "content_delta", Content: "x"}:
			case <-ctx.Done():
				return
			}
		}
		select {
		case ch <- models.StreamEvent{Event: "done", Done: true}:
		case <-ctx.Done():
		}
	}()
	return ch, nil
}

func (*baselineModel) ID() string       { return "baseline-fake" }
func (*baselineModel) Provider() string { return "test" }

type cyclicBarrier struct {
	mu      sync.Mutex
	width   int
	arrived int
	gate    chan struct{}
}

func newCyclicBarrier(width int) *cyclicBarrier {
	return &cyclicBarrier{width: width, gate: make(chan struct{})}
}

func (b *cyclicBarrier) wait(ctx context.Context) error {
	b.mu.Lock()
	gate := b.gate
	b.arrived++
	if b.arrived == b.width {
		close(gate)
		b.arrived = 0
		b.gate = make(chan struct{})
	}
	b.mu.Unlock()
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type baselineTool struct {
	calls     atomic.Uint64
	tailEvery uint64
	tailDelay time.Duration
}

func (*baselineTool) Name() string        { return "baseline_tool" }
func (*baselineTool) Description() string { return "deterministic benchmark tool" }
func (*baselineTool) Schema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":       "baseline_tool",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}
func (t *baselineTool) Call(ctx context.Context, _ map[string]any) (any, error) {
	call := t.calls.Add(1)
	if t.tailEvery == 0 || call%t.tailEvery != 0 || t.tailDelay <= 0 {
		return "ok", nil
	}
	timer := time.NewTimer(t.tailDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return "ok", nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

var _ tools.Tool = (*baselineTool)(nil)

type holdingModel struct {
	started chan struct{}
	release chan struct{}
}

func (m *holdingModel) Response(ctx context.Context, _ []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
	m.started <- struct{}{}
	select {
	case <-m.release:
		return &models.ModelResponse{Content: "done"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (*holdingModel) ResponseStream(context.Context, []models.Message, models.ModelOptions) (<-chan models.StreamEvent, error) {
	return nil, errors.New("streaming is not used by the admission benchmark")
}
func (*holdingModel) ID() string       { return "baseline-holding-fake" }
func (*holdingModel) Provider() string { return "test" }

func newBaselineAgent(tb testing.TB, model models.Model, tool tools.Tool) *agent.Agent {
	tb.Helper()
	cfg := agent.AgentConfig{ID: "performance-baseline", Model: model}
	if tool != nil {
		cfg.Tools = []tools.Tool{tool}
		cfg.MaxToolCalls = 1
	}
	value, err := agent.New(cfg)
	if err != nil {
		tb.Fatalf("agent.New: %v", err)
	}
	return value
}

func percentile(values []time.Duration, p int) time.Duration {
	if len(values) == 0 {
		return 0
	}
	copyOfValues := append([]time.Duration(nil), values...)
	sort.Slice(copyOfValues, func(i, j int) bool { return copyOfValues[i] < copyOfValues[j] })
	index := (len(copyOfValues)*p + 99) / 100
	if index < 1 {
		index = 1
	}
	return copyOfValues[index-1]
}

func benchmarkConcurrentAgentRuns(b *testing.B, submitters int, tool *baselineTool) {
	model := &baselineModel{toolCall: tool != nil, barrier: newCyclicBarrier(submitters)}
	value := newBaselineAgent(b, model, tool)
	samples := make([]time.Duration, 0, 16384)
	var failed atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	started := time.Now()
	for iteration := 0; iteration < b.N; iteration++ {
		gate := make(chan struct{})
		durations := make([]time.Duration, submitters)
		var workers sync.WaitGroup
		workers.Add(submitters)
		for index := 0; index < submitters; index++ {
			go func(index int) {
				defer workers.Done()
				<-gate
				requestStarted := time.Now()
				_, err := value.Run(context.Background(), "fixed-input")
				durations[index] = time.Since(requestStarted)
				if err != nil {
					failed.Add(1)
				}
			}(index)
		}
		close(gate)
		workers.Wait()
		remaining := cap(samples) - len(samples)
		if remaining > len(durations) {
			remaining = len(durations)
		}
		samples = append(samples, durations[:remaining]...)
	}
	elapsed := time.Since(started)
	b.StopTimer()
	if got := failed.Load(); got != 0 {
		b.Fatalf("failed requests = %d", got)
	}
	runs := float64(b.N * submitters)
	b.ReportMetric(runs/elapsed.Seconds(), "runs/s")
	b.ReportMetric(float64(percentile(samples, 50).Nanoseconds()), "p50-ns/run")
	b.ReportMetric(float64(percentile(samples, 95).Nanoseconds()), "p95-ns/run")
}

// BenchmarkAgentConcurrentRuns is the cross-Run baseline. Each benchmark
// operation releases an exact, fixed-width batch from one barrier.
func BenchmarkAgentConcurrentRuns(b *testing.B) {
	for _, submitters := range []int{1, 32, 256} {
		b.Run(fmt.Sprintf("submitters_%03d", submitters), func(b *testing.B) {
			benchmarkConcurrentAgentRuns(b, submitters, nil)
		})
	}
}

// BenchmarkAgentAdmissionOverload records the current bounded-agent overload
// behavior separately from successful throughput. One run owns the only slot;
// every barrier-released submitter must be rejected rather than queued.
func BenchmarkAgentAdmissionOverload(b *testing.B) {
	for _, submitters := range []int{32, 256} {
		b.Run(fmt.Sprintf("submitters_%03d", submitters), func(b *testing.B) {
			var rejected atomic.Int64
			b.ReportAllocs()
			b.ResetTimer()
			started := time.Now()
			for iteration := 0; iteration < b.N; iteration++ {
				model := &holdingModel{started: make(chan struct{}, 1), release: make(chan struct{})}
				value, err := agent.New(agent.AgentConfig{ID: "overload-baseline", Model: model, MaxConcurrentRuns: 1})
				if err != nil {
					b.Fatal(err)
				}
				activeDone := make(chan error, 1)
				go func() {
					_, runErr := value.Run(context.Background(), "hold")
					activeDone <- runErr
				}()
				<-model.started

				gate := make(chan struct{})
				var workers sync.WaitGroup
				workers.Add(submitters)
				for index := 0; index < submitters; index++ {
					go func() {
						defer workers.Done()
						<-gate
						if _, runErr := value.Run(context.Background(), "overload"); errors.Is(runErr, agent.ErrBusy) {
							rejected.Add(1)
						}
					}()
				}
				close(gate)
				workers.Wait()
				close(model.release)
				if err := <-activeDone; err != nil {
					b.Fatal(err)
				}
			}
			elapsed := time.Since(started)
			b.StopTimer()
			want := int64(b.N * submitters)
			if got := rejected.Load(); got != want {
				b.Fatalf("rejected requests = %d, want %d", got, want)
			}
			b.ReportMetric(float64(rejected.Load())/elapsed.Seconds(), "rejections/s")
			b.ReportMetric(float64(rejected.Load()), "rejected-total")
		})
	}
}

// BenchmarkAgentLongTailTool separates framework-only tool overhead from a
// deterministic simulated external wait: exactly every 32nd call waits 1ms.
func BenchmarkAgentLongTailTool(b *testing.B) {
	for _, scenario := range []struct {
		name      string
		tailEvery uint64
		tailDelay time.Duration
	}{
		{name: "framework_only"},
		{name: "tail_every_32", tailEvery: 32, tailDelay: time.Millisecond},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			tool := &baselineTool{tailEvery: scenario.tailEvery, tailDelay: scenario.tailDelay}
			benchmarkConcurrentAgentRuns(b, 32, tool)
			b.ReportMetric(float64(scenario.tailDelay.Nanoseconds()), "simulated-tail-ns")
		})
	}
}

func consumeAgentStream(ctx context.Context, value *agent.Agent, delay time.Duration, cancelAfterStarted bool) (int, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := value.RunStream(streamCtx, "fixed-input")
	if err != nil {
		return 0, err
	}
	events := 0
	for event := range stream {
		events++
		if cancelAfterStarted && event.Type == agent.EventTypeRunStarted {
			cancel()
		}
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return events, ctx.Err()
			}
		}
	}
	return events, nil
}

// BenchmarkAgentStreamConsumer records the normal, deliberately slow, and
// cancelled stream paths separately. The delay is the declared simulated
// consumer wait, not framework work.
func BenchmarkAgentStreamConsumer(b *testing.B) {
	for _, scenario := range []struct {
		name               string
		delay              time.Duration
		cancelAfterStarted bool
	}{
		{name: "normal"},
		{name: "slow_100us_per_event", delay: 100 * time.Microsecond},
		{name: "cancel_after_started", cancelAfterStarted: true},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			value := newBaselineAgent(b, &baselineModel{streamEvents: 8}, nil)
			var events int64
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				count, err := consumeAgentStream(context.Background(), value, scenario.delay, scenario.cancelAfterStarted)
				if err != nil {
					b.Fatal(err)
				}
				events += int64(count)
			}
			b.StopTimer()
			b.ReportMetric(float64(events)/b.Elapsed().Seconds(), "events/s")
			b.ReportMetric(float64(scenario.delay.Nanoseconds()), "simulated-consumer-ns/event")
		})
	}
}

func TestPerformanceBaselineConcurrentRuns(t *testing.T) {
	t.Logf("environment go=%s os=%s arch=%s gomaxprocs=%d seed=%d", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0), baselineSeed)
	for _, submitters := range []int{1, 32, 256} {
		t.Run(fmt.Sprintf("submitters_%03d", submitters), func(t *testing.T) {
			value := newBaselineAgent(t, &baselineModel{}, nil)
			before := runtime.NumGoroutine()
			var active atomic.Int64
			var peak atomic.Int64
			var failed atomic.Int64
			latencies := make([]time.Duration, submitters)
			gate := make(chan struct{})
			var workers sync.WaitGroup
			workers.Add(submitters)
			started := time.Now()
			for index := 0; index < submitters; index++ {
				go func(index int) {
					defer workers.Done()
					<-gate
					current := active.Add(1)
					for observed := peak.Load(); current > observed && !peak.CompareAndSwap(observed, current); observed = peak.Load() {
					}
					requestStarted := time.Now()
					_, err := value.Run(context.Background(), "fixed-input")
					latencies[index] = time.Since(requestStarted)
					active.Add(-1)
					if err != nil {
						failed.Add(1)
					}
				}(index)
			}
			goroutinePeakLowerBound := runtime.NumGoroutine()
			close(gate)
			workers.Wait()
			elapsed := time.Since(started)
			if failed.Load() != 0 {
				t.Fatalf("failed requests = %d", failed.Load())
			}
			t.Logf("runs=%d elapsed=%s throughput=%.2f_runs/s p50=%s p95=%s active_peak=%d goroutines_before=%d goroutines_peak_lower_bound=%d goroutines_after=%d",
				submitters, elapsed, float64(submitters)/elapsed.Seconds(), percentile(latencies, 50), percentile(latencies, 95), peak.Load(), before, goroutinePeakLowerBound, runtime.NumGoroutine())
		})
	}
}

func TestPerformanceBaselineSlowConsumerAndCancellation(t *testing.T) {
	value := newBaselineAgent(t, &baselineModel{streamEvents: 8}, nil)
	for _, scenario := range []struct {
		name               string
		delay              time.Duration
		cancelAfterStarted bool
	}{
		{name: "normal"},
		{name: "slow_consumer", delay: 100 * time.Microsecond},
		{name: "cancel", cancelAfterStarted: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			events, err := consumeAgentStream(ctx, value, scenario.delay, scenario.cancelAfterStarted)
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if events == 0 {
				t.Fatal("stream emitted no events")
			}
			t.Logf("events=%d simulated_consumer_delay=%s cancelled=%t", events, scenario.delay, scenario.cancelAfterStarted)
		})
	}
}

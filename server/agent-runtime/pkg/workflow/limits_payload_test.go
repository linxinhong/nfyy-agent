package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	workflowschema "github.com/linxinhong/xingding-agent-runtime/pkg/workflow/schema"
)

type countingMarshaler struct{ calls *atomic.Int32 }

func (m countingMarshaler) MarshalJSON() ([]byte, error) {
	m.calls.Add(1)
	return []byte(`"called"`), nil
}

type legacyPayload struct {
	Message string `json:"message"`
}

func TestWorkflowLimitsPayloadJSONExactBytes(t *testing.T) {
	// json.Marshal emits the UTF-8 rune, plus its two quotes: five bytes.
	if err := validatePayloadJSON("input", "€", 5); err != nil {
		t.Fatalf("exact UTF-8 boundary: %v", err)
	}
	err := validatePayloadJSON("input", "€", 4)
	var limitErr *PayloadLimitError
	if !errors.As(err, &limitErr) || limitErr.Actual != 5 {
		t.Fatalf("UTF-8 overflow = %#v, want actual 5", err)
	}
	if err := validatePayloadJSON("input", "\n", 4); err != nil {
		t.Fatalf("escaped newline exact bytes: %v", err)
	}
}

func TestWorkflowLimitsPayloadRejectsCycle(t *testing.T) {
	value := map[string]any{}
	value["self"] = value
	err := validatePayloadJSON("input", value, 1024)
	if err == nil || errors.Is(err, ErrPayloadLimitExceeded) {
		t.Fatalf("cycle error = %v, want serialization error", err)
	}
}

func TestWorkflowLimitsPayloadAllowsNonCyclicSliceAlias(t *testing.T) {
	value := make([]any, 2)
	value[0] = "x"
	value[1] = value[:1]
	want, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	got, err := measureDynamicJSON(value, len(want))
	if err != nil {
		t.Fatalf("measure alias: %v", err)
	}
	if got != len(want) {
		t.Fatalf("alias bytes = %d, want %d (%s)", got, len(want), want)
	}

	cycle := make([]any, 1)
	cycle[0] = cycle
	if err := validatePayloadJSON("input", cycle, 1024); err == nil || errors.Is(err, ErrPayloadLimitExceeded) {
		t.Fatalf("slice cycle error = %v, want value-domain rejection", err)
	}
}

func TestWorkflowLimitsRejectsEnvelopeTooSmallForTerminalError(t *testing.T) {
	registry := NewRegistry()
	if err := registry.RegisterExecutor("ok", ExecutorFunc(func(_ context.Context, _ NodeInput) (NodeOutput, error) { return NodeOutput{}, nil })); err != nil {
		t.Fatal(err)
	}
	_, err := New(Config{Definition: Definition{ID: "tiny", Version: "v1", Root: ExecutorNode("node", "ok")}, Registry: registry, Limits: &Limits{MaxRunEnvelopeBytes: 1}})
	if !errors.Is(err, ErrPayloadLimitExceeded) {
		t.Fatalf("New tiny envelope error = %v", err)
	}
}

func TestWorkflowLimitsPayloadMatchesStandardJSON(t *testing.T) {
	values := []any{
		nil, true, false, "<&>\n\u2028€", string([]byte{'x', 0xff}),
		int64(math.MinInt64), uint64(math.MaxUint64), float64(1e-9), float64(1e20), float32(1.25),
		json.Number("-123.450e+6"), []any(nil), []any{}, map[string]any(nil), map[string]any{},
		[]any{"x", true, nil, map[string]any{"escaped<&": float64(1e-7)}},
	}
	for _, value := range values {
		want, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("json.Marshal(%#v): %v", value, err)
		}
		got, err := measureDynamicJSON(value, len(want))
		if err != nil {
			t.Fatalf("measureDynamicJSON(%#v): %v", value, err)
		}
		if got != len(want) {
			t.Fatalf("measureDynamicJSON(%#v) = %d, want %d (%s)", value, got, len(want), want)
		}
		if err := validatePayloadJSON("fixture", value, len(want)); err != nil {
			t.Fatalf("exact boundary for %#v: %v", value, err)
		}
		if len(want) > 0 && !errors.Is(validatePayloadJSON("fixture", value, len(want)-1), ErrPayloadLimitExceeded) {
			t.Fatalf("one-byte-small boundary for %#v was accepted", value)
		}
	}
}

func TestWorkflowLimitsPayloadDepthBoundary(t *testing.T) {
	value := any(nil)
	for range 63 {
		value = []any{value}
	}
	if err := validatePayloadJSON("input", value, 1024); err != nil {
		t.Fatalf("depth 64 rejected: %v", err)
	}
	value = []any{value}
	if err := validatePayloadJSON("input", value, 1024); err == nil || errors.Is(err, ErrPayloadLimitExceeded) {
		t.Fatalf("depth 65 error = %v, want value-domain rejection", err)
	}
}

func TestWorkflowLimitsPayloadRejectsCustomMarshalerWithoutCallingIt(t *testing.T) {
	var calls atomic.Int32
	value := countingMarshaler{calls: &calls}
	if err := validatePayloadJSON("input", value, 1024); err == nil {
		t.Fatal("limited payload accepted a custom Marshaler")
	}
	if calls.Load() != 0 {
		t.Fatalf("MarshalJSON calls = %d, want 0", calls.Load())
	}

	envelope := &session.RunOutput{
		RunID: "custom", Status: session.RunStatusRunning, Parsed: value,
		CreatedAt: time.Unix(0, 0).UTC(), UpdatedAt: time.Unix(0, 0).UTC(),
	}
	if err := validateRunEnvelope(envelope, 4096); err == nil {
		t.Fatal("limited envelope accepted a custom Marshaler in a dynamic field")
	}
	if calls.Load() != 0 {
		t.Fatalf("envelope MarshalJSON calls = %d, want 0", calls.Load())
	}

	registry := NewRegistry()
	var executorCalls atomic.Int32
	if err := registry.RegisterExecutor("never", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		executorCalls.Add(1)
		return NodeOutput{}, nil
	})); err != nil {
		t.Fatal(err)
	}
	wf, err := New(Config{
		Definition: Definition{ID: "custom-input", Version: "v1", Root: ExecutorNode("node", "never")},
		Registry:   registry, Limits: &Limits{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wf.Run(context.Background(), RunRequest{Input: value}); err == nil {
		t.Fatal("limited Run accepted custom input")
	}
	if calls.Load() != 0 || executorCalls.Load() != 0 {
		t.Fatalf("limited Run calls: MarshalJSON=%d executor=%d, want 0/0", calls.Load(), executorCalls.Load())
	}
}

func TestWorkflowLimitsPayloadHugeValuesStopAtBudget(t *testing.T) {
	if err := validatePayloadJSON("input", strings.Repeat("x", 8<<20), 32); !errors.Is(err, ErrPayloadLimitExceeded) {
		t.Fatalf("huge string error = %v, want payload limit", err)
	}
	var calls atomic.Int32
	values := make([]any, 1<<20)
	values[len(values)-1] = countingMarshaler{calls: &calls}
	if err := validatePayloadJSON("input", values, 8); !errors.Is(err, ErrPayloadLimitExceeded) {
		t.Fatalf("huge container error = %v, want early payload limit", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("tail Marshaler calls = %d, want 0", calls.Load())
	}
}

func TestWorkflowLimitsInternalEnvelopeMatchesStandardJSON(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 30, 0, 123456789, time.FixedZone("test", 8*60*60))
	definition := Definition{ID: "bounded", Name: "界<&", Version: "v1", Root: ExecutorNode("node", "noop")}
	output := &session.RunOutput{
		RunID: "run", WorkflowID: definition.ID, Metadata: map[string]string{"key": "value<&"},
		Status: session.RunStatusPaused, Error: "wait\n", CreatedAt: now, UpdatedAt: now,
		Workflow: &workflowschema.RunData{
			SchemaVersion: 1, DefinitionVersion: "v1", Definition: &definition,
			Input: map[string]any{"n": json.Number("1.20e3")}, State: map[string]any{"ok": true},
			NodeResults: map[string]NodeResult{"node": {NodeID: "node", NodeType: NodeTypeStep, Status: workflowschema.NodeStatusCompleted, Output: []any{"done", nil}, StartedAt: now}},
			Checkpoint:  &workflowschema.Checkpoint{Version: 1, PausedNodeID: "node"},
		},
	}
	want, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	got, err := measureInternalJSON(output, len(want))
	if err != nil {
		t.Fatalf("measureInternalJSON: %v", err)
	}
	if got != len(want) {
		t.Fatalf("internal envelope bytes = %d, want %d\n%s", got, len(want), want)
	}
	if err := validateRunEnvelope(output, len(want)); err != nil {
		t.Fatalf("exact envelope boundary: %v", err)
	}
	if !errors.Is(validateRunEnvelope(output, len(want)-1), ErrPayloadLimitExceeded) {
		t.Fatal("one-byte-small envelope boundary was accepted")
	}
}

func TestWorkflowLimitsInternalTimeMeasurementMatchesStandardJSON(t *testing.T) {
	values := []time.Time{
		time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 8, 12, 30, 0, 120000000, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.FixedZone("edge", -23*60*60-59*60)),
	}
	for _, value := range values {
		want, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		got, err := timeJSONSize(value)
		if err != nil {
			t.Fatalf("timeJSONSize(%s): %v", value, err)
		}
		if got != len(want) {
			t.Fatalf("timeJSONSize(%s) = %d, want %d (%s)", value, got, len(want), want)
		}
	}
	invalid := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, marshalErr := json.Marshal(invalid); marshalErr == nil {
		t.Fatal("standard JSON accepted invalid time year")
	}
	if _, measureErr := timeJSONSize(invalid); measureErr == nil {
		t.Fatal("limited measurement accepted invalid time year")
	}
}

func TestWorkflowLimitsNilModeKeepsBusinessStructCompatibility(t *testing.T) {
	registry := NewRegistry()
	if err := registry.RegisterExecutor("legacy", ExecutorFunc(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{Value: legacyPayload{Message: "compatible"}}, nil
	})); err != nil {
		t.Fatal(err)
	}
	wf, err := New(Config{Definition: Definition{ID: "legacy", Version: "v1", Root: ExecutorNode("node", "legacy")}, Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	out, err := wf.Run(context.Background(), RunRequest{Input: legacyPayload{Message: "input"}})
	if err != nil {
		t.Fatalf("legacy Run: %v", err)
	}
	if out.Status != session.RunStatusCompleted {
		t.Fatalf("legacy status = %s", out.Status)
	}
}

package agent

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

type runPreparationTool struct{}

func (runPreparationTool) Name() string        { return "run-preparation-tool" }
func (runPreparationTool) Description() string { return "test tool" }
func (runPreparationTool) Schema() map[string]any {
	return map[string]any{
		"function": map[string]any{
			"name":        "run-preparation-tool",
			"description": "test tool",
			"parameters":  map[string]any{"type": "object"},
		},
	}
}
func (runPreparationTool) Call(context.Context, map[string]any) (any, error) {
	return "ok", nil
}

func TestResolveToolsEmptyAndConfigured(t *testing.T) {
	empty := &Agent{}
	toolMap, modelTools, err := empty.resolveTools()
	if err != nil {
		t.Fatal(err)
	}
	if toolMap != nil || modelTools != nil {
		t.Fatalf("empty tools = %#v, %#v; want nil, nil", toolMap, modelTools)
	}

	configured := &Agent{cfg: AgentConfig{Tools: []tools.Tool{runPreparationTool{}}}}
	toolMap, modelTools, err = configured.resolveTools()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := toolMap["run-preparation-tool"]; !ok {
		t.Fatalf("configured tool map = %#v", toolMap)
	}
	if len(modelTools) != 1 || modelTools[0].Function.Name != "run-preparation-tool" {
		t.Fatalf("configured model tools = %#v", modelTools)
	}
}

func TestMergeSessionStateEmptyPrecedenceAndIsolation(t *testing.T) {
	if got := mergeSessionState(nil, nil); got != nil {
		t.Fatalf("empty merged state = %#v, want nil", got)
	}
	configured := map[string]any{"configured": "value", "shared": "configured"}
	sessionState := map[string]any{"session": "value", "shared": "session"}
	merged := mergeSessionState(configured, sessionState)
	if merged["configured"] != "value" || merged["session"] != "value" || merged["shared"] != "session" {
		t.Fatalf("merged state = %#v", merged)
	}
	merged["configured"] = "changed"
	if configured["configured"] != "value" {
		t.Fatal("merged state aliases configured state")
	}
}

type runPreparationErrorModel struct{ err error }

func (m runPreparationErrorModel) Response(context.Context, []models.Message, models.ModelOptions) (*models.ModelResponse, error) {
	return nil, m.err
}
func (runPreparationErrorModel) ResponseStream(context.Context, []models.Message, models.ModelOptions) (<-chan models.StreamEvent, error) {
	return nil, errors.New("stream not used")
}
func (runPreparationErrorModel) ID() string       { return "run-preparation-error" }
func (runPreparationErrorModel) Provider() string { return "test" }

func TestCallModelStillUsesFallbackAfterPrimaryFailure(t *testing.T) {
	fallback := mock.New()
	fallback.ResponseData = &models.ModelResponse{Content: "fallback"}
	value := &Agent{cfg: AgentConfig{
		Model:          runPreparationErrorModel{err: errors.New("primary failed")},
		FallbackModels: []models.Model{fallback},
	}}
	response, err := value.callModel(context.Background(), nil, models.ModelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if response.Content != "fallback" {
		t.Fatalf("content = %q, want fallback", response.Content)
	}
}

type runPreparationRecordingModel struct {
	name     string
	calls    *[]string
	response *models.ModelResponse
	err      error
}

func (m runPreparationRecordingModel) Response(context.Context, []models.Message, models.ModelOptions) (*models.ModelResponse, error) {
	*m.calls = append(*m.calls, m.name)
	return m.response, m.err
}
func (runPreparationRecordingModel) ResponseStream(context.Context, []models.Message, models.ModelOptions) (<-chan models.StreamEvent, error) {
	return nil, errors.New("stream not used")
}
func (m runPreparationRecordingModel) ID() string     { return m.name }
func (runPreparationRecordingModel) Provider() string { return "test" }

func TestCallModelPreservesFallbackOrderAndLastError(t *testing.T) {
	var calls []string
	primaryErr := errors.New("primary failed")
	firstErr := errors.New("first fallback failed")
	lastErr := errors.New("last fallback failed")
	value := &Agent{cfg: AgentConfig{
		Model: runPreparationRecordingModel{name: "primary", calls: &calls, err: primaryErr},
		FallbackModels: []models.Model{
			runPreparationRecordingModel{name: "fallback-1", calls: &calls, err: firstErr},
			runPreparationRecordingModel{name: "fallback-2", calls: &calls, err: lastErr},
		},
	}}
	_, err := value.callModel(context.Background(), nil, models.ModelOptions{})
	if !errors.Is(err, lastErr) {
		t.Fatalf("error = %v, want last fallback error %v", err, lastErr)
	}
	want := []string{"primary", "fallback-1", "fallback-2"}
	if !slices.Equal(calls, want) {
		t.Fatalf("call order = %v, want %v", calls, want)
	}
}

type runPreparationCancellingModel struct {
	cancel context.CancelFunc
	calls  int
}

func (m *runPreparationCancellingModel) Response(context.Context, []models.Message, models.ModelOptions) (*models.ModelResponse, error) {
	m.calls++
	m.cancel()
	return nil, errors.New("server returned 500")
}
func (*runPreparationCancellingModel) ResponseStream(context.Context, []models.Message, models.ModelOptions) (<-chan models.StreamEvent, error) {
	return nil, errors.New("stream not used")
}
func (*runPreparationCancellingModel) ID() string       { return "run-preparation-cancel" }
func (*runPreparationCancellingModel) Provider() string { return "test" }

func TestCallModelCancellationStopsRetryLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	model := &runPreparationCancellingModel{cancel: cancel}
	value := &Agent{cfg: AgentConfig{
		Model:       model,
		RetryConfig: RetryConfig{MaxRetries: 3, RetryDelay: time.Hour},
	}}
	_, err := value.callModel(ctx, nil, models.ModelOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if model.calls != 1 {
		t.Fatalf("model calls = %d, want 1", model.calls)
	}
}

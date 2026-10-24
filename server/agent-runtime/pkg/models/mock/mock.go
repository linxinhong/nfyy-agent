// Package mock provides a fake models.Model implementation for testing.
package mock

import (
	"context"
	"sync"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

// MockModel is a configurable fake model for tests.
type MockModel struct {
	ResponseData        *models.ModelResponse
	ResponseErrors      []error
	ToolCallRounds      int
	StreamEvents        []models.StreamEvent
	StreamEventsPerCall [][]models.StreamEvent

	mu    sync.Mutex
	calls int
}

// New creates a new MockModel with no configured behavior.
func New() *MockModel {
	return &MockModel{}
}

// Response returns the configured response or a default placeholder.
// When ToolCallRounds is greater than zero, the first ToolCallRounds calls
// return a synthetic tool-call response instead of ResponseData.
// When ResponseErrors is set, each call consumes the next error until empty.
func (m *MockModel) Response(ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.calls < len(m.ResponseErrors) {
		defer func() { m.calls++ }()
		return nil, m.ResponseErrors[m.calls]
	}

	if m.ToolCallRounds > 0 && m.calls < len(m.ResponseErrors)+m.ToolCallRounds {
		m.calls++
		return &models.ModelResponse{
			ToolCalls: []models.ToolCall{
				{
					ID:   "mock_call_1",
					Type: "function",
					Function: models.FunctionCall{
						Name:      "mock_tool",
						Arguments: `{"query":"test"}`,
					},
				},
			},
		}, nil
	}

	m.calls++
	if m.ResponseData != nil {
		return m.ResponseData, nil
	}
	return &models.ModelResponse{Content: "mock response"}, nil
}

// ResponseStream emits the configured stream events followed by a done event.
// If StreamEventsPerCall is set, each call consumes the next slice in order.
// If ResponseErrors is set, each call consumes the next error until empty.
func (m *MockModel) ResponseStream(ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error) {
	m.mu.Lock()
	if m.calls < len(m.ResponseErrors) {
		defer func() { m.calls++ }()
		err := m.ResponseErrors[m.calls]
		m.mu.Unlock()
		return nil, err
	}

	var events []models.StreamEvent
	if m.calls < len(m.ResponseErrors)+len(m.StreamEventsPerCall) {
		events = m.StreamEventsPerCall[m.calls-len(m.ResponseErrors)]
	} else {
		events = m.StreamEvents
	}
	m.calls++
	m.mu.Unlock()

	ch := make(chan models.StreamEvent)
	go func() {
		defer close(ch)
		for _, ev := range events {
			ch <- ev
		}
		ch <- models.StreamEvent{Event: "done", Done: true}
	}()
	return ch, nil
}

// ID returns the model identifier.
func (m *MockModel) ID() string {
	return "mock"
}

// Provider returns the provider name.
func (m *MockModel) Provider() string {
	return "mock"
}

// SupportsNativeJSONSchema always returns true so agent tests exercise the
// json_schema code path.
func (m *MockModel) SupportsNativeJSONSchema() bool {
	return true
}

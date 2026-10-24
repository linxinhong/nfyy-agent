// Package tests provides a CI-runnable end-to-end harness for AgentOS.
//
// The harness boots the full AgentOS HTTP stack on a real httptest server
// backed by a SQLite temp file (never in-memory, so CAS and persistence
// paths are exercised), with the LLM provider as the only stub. Tests are
// isolated by fresh IDs (NewID) and must not rely on global state; use
// WaitFor for polling instead of sleeping.
package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/sqlite"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

// NewID returns a fresh unique identifier for test isolation. Every test
// must use its own agent/session/run IDs; never reuse global constants.
func NewID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// WaitFor polls cond until it returns true or timeout elapses. This is the
// only allowed polling mechanism for asynchronous state (background runs,
// SSE completion); do not sleep directly.
func WaitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

// FakeChat is a programmable models.Model implementation: it returns queued
// responses in order (falling back to an empty content response) and records
// every received message batch for assertions. It is the only allowed stub
// in end-to-end tests, mirroring cloudflare-os's network-interceptor.
type FakeChat struct {
	mu       sync.Mutex
	queue    []*models.ModelResponse
	received [][]models.Message
}

// NewFakeChat creates a FakeChat with the given responses, consumed in order.
func NewFakeChat(responses ...*models.ModelResponse) *FakeChat {
	return &FakeChat{queue: responses}
}

// Response implements models.Model.
func (f *FakeChat) Response(ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.received = append(f.received, cloneMessages(messages))
	if len(f.queue) == 0 {
		return &models.ModelResponse{}, nil
	}
	resp := f.queue[0]
	f.queue = f.queue[1:]
	return resp, nil
}

// ResponseStream implements models.Model with a minimal single-done stream.
func (f *FakeChat) ResponseStream(ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error) {
	ch := make(chan models.StreamEvent, 1)
	ch <- models.StreamEvent{Event: "done", Done: true}
	close(ch)
	return ch, nil
}

// ID implements models.Model.
func (f *FakeChat) ID() string { return "fake-chat" }

// Provider implements models.Model.
func (f *FakeChat) Provider() string { return "fake" }

// Calls returns the number of model calls made so far.
func (f *FakeChat) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.received)
}

// Received returns the message batches received on each call.
func (f *FakeChat) Received() [][]models.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]models.Message, len(f.received))
	for i, batch := range f.received {
		out[i] = cloneMessages(batch)
	}
	return out
}

// LastMessages returns the message batch of the most recent call.
func (f *FakeChat) LastMessages() []models.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.received) == 0 {
		return nil
	}
	return cloneMessages(f.received[len(f.received)-1])
}

func cloneMessages(in []models.Message) []models.Message {
	out := make([]models.Message, len(in))
	copy(out, in)
	return out
}

// funcTool adapts a plain function to tools.Tool.
type funcTool struct {
	name string
	desc string
	fn   func(ctx context.Context, args map[string]any) (any, error)
}

func (t *funcTool) Name() string { return t.name }
func (t *funcTool) Description() string {
	return t.desc
}
func (t *funcTool) Schema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        t.name,
			"description": t.desc,
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}
func (t *funcTool) Call(ctx context.Context, args map[string]any) (any, error) {
	return t.fn(ctx, args)
}

// NewFuncTool builds a tools.Tool from a name, description, and call func.
func NewFuncTool(name, desc string, fn func(ctx context.Context, args map[string]any) (any, error)) tools.Tool {
	return &funcTool{name: name, desc: desc, fn: fn}
}

// Server is the running AgentOS harness.
type Server struct {
	t       *testing.T
	ts      *httptest.Server
	store   *sqlite.DB
	agentID string
	// Fake is the default model attached to the harness agent. Tests can
	// preset responses and assert received messages on it.
	Fake *FakeChat
}

type options struct {
	agentID          string
	fake             *FakeChat
	tools            []tools.Tool
	contextProviders []agent.ContextProvider
}

// Option configures the harness.
type Option func(*options)

// WithAgentID overrides the default harness agent ID.
func WithAgentID(id string) Option {
	return func(o *options) { o.agentID = id }
}

// WithFakeChat replaces the default FakeChat model.
func WithFakeChat(f *FakeChat) Option {
	return func(o *options) { o.fake = f }
}

// WithTools registers extra tools on the harness agent.
func WithTools(extra []tools.Tool) Option {
	return func(o *options) { o.tools = append(o.tools, extra...) }
}

// WithContextProviders registers context providers on the harness agent.
func WithContextProviders(providers []agent.ContextProvider) Option {
	return func(o *options) { o.contextProviders = append(o.contextProviders, providers...) }
}

// NewServer boots a full AgentOS backed by a SQLite temp file and returns a
// harness handle. The server and store are closed via t.Cleanup.
func NewServer(t *testing.T, opts ...Option) *Server {
	t.Helper()
	cfg := options{agentID: "harness-agent", fake: NewFakeChat()}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}

	store, err := sqlite.New(filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	harnessAgent, err := agent.New(agent.AgentConfig{
		ID:               cfg.agentID,
		Name:             "Harness Agent",
		Instructions:     "You are a harness test agent.",
		Model:            cfg.fake,
		DB:               store,
		Tools:            cfg.tools,
		ContextProviders: cfg.contextProviders,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	agentOS, err := os.New(os.AgentOSConfig{
		Agents: map[string]*agent.Agent{cfg.agentID: harnessAgent},
		DB:     store,
	})
	if err != nil {
		t.Fatalf("os.New: %v", err)
	}

	ts := httptest.NewServer(agentOS.Handler())
	t.Cleanup(ts.Close)

	return &Server{
		t:       t,
		ts:      ts,
		store:   store,
		agentID: cfg.agentID,
		Fake:    cfg.fake,
	}
}

// URL returns the base URL of the harness server.
func (s *Server) URL() string { return s.ts.URL }

// AgentID returns the registered harness agent ID.
func (s *Server) AgentID() string { return s.agentID }

// PostJSON sends a JSON POST to path and decodes the response into out.
// It returns the HTTP status code.
func (s *Server) PostJSON(path string, body any, out any) (int, error) {
	s.t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		return 0, err
	}
	resp, err := http.Post(s.ts.URL+path, "application/json", &buf)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return s.decode(resp, out)
}

// GetJSON sends a GET to path and decodes the response into out.
func (s *Server) GetJSON(path string, out any) (int, error) {
	s.t.Helper()
	resp, err := http.Get(s.ts.URL + path)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return s.decode(resp, out)
}

func (s *Server) decode(resp *http.Response, out any) (int, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, err
	}
	if out == nil {
		return resp.StatusCode, nil
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return resp.StatusCode, fmt.Errorf("empty response body (status %d)", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return resp.StatusCode, fmt.Errorf("decode %q: %w", string(body), err)
	}
	return resp.StatusCode, nil
}

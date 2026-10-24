// Package anthropicmock tests the Anthropic dialect's prompt caching
// against an emulator of the messages API cache semantics: a request
// reads the longest prefix (tools, system, messages up to a cache_control
// block) that an earlier request wrote at one of its breakpoints, and
// writes the rest up to its last breakpoint.
package anthropicmock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/providers"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/builtin"
)

type block map[string]any

type request struct {
	System   []block `json:"system"`
	Tools    []block `json:"tools"`
	Messages []struct {
		Role    string  `json:"role"`
		Content []block `json:"content"`
	} `json:"messages"`
}

type emulator struct {
	*httptest.Server
	mu       sync.Mutex
	cache    map[string]bool
	requests []request
	errors   []string
}

func tokens(v any) int {
	b, _ := json.Marshal(v)
	return (len(b) + 3) / 4
}

func newEmulator(t *testing.T) *emulator {
	e := &emulator{cache: map[string]bool{}}
	e.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if msg := validate(req); msg != "" {
			e.mu.Lock()
			e.errors = append(e.errors, msg)
			e.mu.Unlock()
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": msg}})
			return
		}
		read, written, total := e.account(req)
		answer := "Here is a thorough answer to your question."
		content := []any{map[string]any{"type": "text", "text": answer}}
		stop := "end_turn"
		if len(req.Tools) > 0 && !hasToolResultInLastTurn(req) {
			content = []any{map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_%d", len(e.requests)), "name": req.Tools[0]["name"], "input": map[string]any{"expression": "2+2"}}}
			stop = "tool_use"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg", "type": "message", "role": "assistant", "content": content, "stop_reason": stop,
			"usage": map[string]any{"input_tokens": total - read - written, "output_tokens": 20,
				"cache_read_input_tokens": read, "cache_creation_input_tokens": written},
		})
	}))
	t.Cleanup(e.Close)
	return e
}

func hasToolResultInLastTurn(req request) bool {
	last := req.Messages[len(req.Messages)-1]
	for _, b := range last.Content {
		if b["type"] == "tool_result" {
			return true
		}
	}
	return false
}

func validate(req request) string {
	breakpoints := 0
	check := func(where string, b block) string {
		if b["cache_control"] != nil {
			breakpoints++
			if b["type"] == "text" && b["text"] == "" {
				return where + ": cache_control cannot be set for empty text blocks"
			}
		}
		return ""
	}
	for _, b := range req.Tools {
		if msg := check("tools", b); msg != "" {
			return msg
		}
	}
	for _, b := range req.System {
		if msg := check("system", b); msg != "" {
			return msg
		}
	}
	for i, m := range req.Messages {
		if i > 0 && req.Messages[i-1].Role == m.Role {
			return "messages: roles must alternate between \"user\" and \"assistant\""
		}
		for _, b := range m.Content {
			if msg := check("messages", b); msg != "" {
				return msg
			}
		}
	}
	if breakpoints > 4 {
		return fmt.Sprintf("A maximum of 4 blocks with cache_control may be provided. Found %d.", breakpoints)
	}
	return ""
}

// account walks the prefix in API order, hashing it; at each breakpoint it
// reads from the cache when an earlier request wrote that prefix, then
// records the prefix as written.
func (e *emulator) account(req request) (read, written, total int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.requests = append(e.requests, req)
	h := sha256.New()
	pos, lastBreakpoint := 0, 0
	visit := func(b block) {
		stripped := block{}
		for k, v := range b {
			if k != "cache_control" {
				stripped[k] = v
			}
		}
		raw, _ := json.Marshal(stripped)
		h.Write(raw)
		pos += tokens(stripped)
		if b["cache_control"] == nil {
			return
		}
		key := hex.EncodeToString(h.Sum(nil))
		if e.cache[key] {
			read = pos
		}
		e.cache[key] = true
		lastBreakpoint = pos
	}
	for _, b := range req.Tools {
		visit(b)
	}
	for _, b := range req.System {
		visit(b)
	}
	for _, m := range req.Messages {
		h.Write([]byte(m.Role))
		for _, b := range m.Content {
			visit(b)
		}
	}
	if lastBreakpoint > read {
		written = lastBreakpoint - read
	}
	return read, written, pos
}

type usageLog struct {
	mu      sync.Mutex
	records []agent.UsageRecord
}

func (u *usageLog) Admit(context.Context, agent.UsageScope) error { return nil }
func (u *usageLog) Record(_ context.Context, _ agent.UsageScope, r agent.UsageRecord) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.records = append(u.records, r)
}

type changingMemory struct {
	mu sync.Mutex
	n  int
}

func (m *changingMemory) CreateMemory(context.Context, string, string, string) (*memory.UserMemory, error) {
	return nil, nil
}
func (m *changingMemory) GetMemories(context.Context, string, string, int) ([]*memory.UserMemory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	return []*memory.UserMemory{{Memory: fmt.Sprintf("fact #%d", m.n)}}, nil
}

func runTurns(t *testing.T, disableCache bool) (*emulator, []agent.UsageRecord) {
	t.Helper()
	e := newEmulator(t)
	p := providers.Anthropic()
	p.BaseURL = e.URL
	for i := range p.Models {
		p.Models[i].DisablePromptCache = disableCache
	}
	reg := providers.NewRegistry(p)
	reg.SetAPIKey("anthropic", "sk-ant-mock")
	model, err := reg.Resolve("anthropic:claude-sonnet-4-20250514")
	if err != nil {
		t.Fatal(err)
	}
	calc, _ := builtin.NewCalculatorTool()
	log := &usageLog{}
	ag, err := agent.New(agent.AgentConfig{
		Model: model, Instructions: strings.Repeat("You are a careful financial assistant. ", 60),
		Tools: []tools.Tool{calc}, DB: in_memory.New(), UserID: "u", Memory: &changingMemory{}, UsageGuard: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := ag.Run(context.Background(), "turn 1")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"turn 2", "turn 3"} {
		if _, err := ag.RunWith(context.Background(), session.RunRequest{Input: session.RunInput{Message: q}, SessionID: first.SessionID}); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.errors) > 0 {
		t.Fatalf("API rejected requests: %v", e.errors)
	}
	return e, log.records
}

func TestAnthropicPromptCacheAcrossTurnsAndToolRounds(t *testing.T) {
	e, cached := runTurns(t, false)
	_, uncached := runTurns(t, true)
	if len(cached) != 6 || len(uncached) != 6 { // 3 turns x (tool round + answer)
		t.Fatalf("calls = %d / %d", len(cached), len(uncached))
	}
	for _, req := range e.requests {
		for _, b := range req.System {
			if strings.Contains(fmt.Sprint(b["text"]), "fact #") {
				t.Fatal("per-request context leaked into the system prompt")
			}
		}
	}
	var cachedCost, uncachedCost float64
	for i := range cached {
		cachedCost += cached[i].CostUSD
		uncachedCost += uncached[i].CostUSD
	}
	// The tool round of turn 1 reuses the first request; every later turn
	// reuses the previous turn's history.
	if cached[1].CachedInputTokens == 0 || cached[4].CachedInputTokens < cached[4].InputTokens/2 {
		t.Fatalf("cache reads: tool round %d, turn 3 %d of %d", cached[1].CachedInputTokens, cached[4].CachedInputTokens, cached[4].InputTokens)
	}
	if uncached[4].CachedInputTokens != 0 {
		t.Fatalf("DisablePromptCache still cached: %+v", uncached[4])
	}
	if cachedCost >= uncachedCost {
		t.Fatalf("caching must lower cost: $%.6f vs $%.6f", cachedCost, uncachedCost)
	}
	t.Logf("turn 3: %d/%d input tokens read from cache; 3-turn cost $%.6f cached vs $%.6f uncached",
		cached[4].CachedInputTokens, cached[4].InputTokens, cachedCost, uncachedCost)
}

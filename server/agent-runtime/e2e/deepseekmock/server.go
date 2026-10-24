// Package deepseekmock emulates the DeepSeek chat completions API closely
// enough to exercise the runtime end to end without network access or an
// API key. It enforces the request rules that real deployments reject with
// HTTP 400, so a client bug surfaces as a failed run rather than passing a
// lenient fake:
//
//   - V3.2 (official deepseek-chat / deepseek-reasoner): in thinking mode,
//     every assistant tool-call message after the last user message must
//     carry its reasoning_content; earlier reasoning is ignored, as the API
//     drops it from the context.
//   - R1 (original DeepSeek-R1 deployments): any reasoning_content in the
//     request is rejected; reasoning may be returned inside content as
//     <think> blocks, optionally with the opening tag pre-filled.
//   - Both: max_tokens range per model, tool_call/tool message pairing,
//     logprobs rejected in thinking mode, Bearer authentication, the
//     {"error":{...}} envelope, keep-alive comments while queued, usage
//     with prompt_cache_hit_tokens / reasoning_tokens, and prefix-cache hit
//     accounting in 64-token units.
//
// Behavior is scripted per test with a Brain, and faults (HTTP errors,
// insufficient_system_resource, dropped streams) are queued with Fail.
package deepseekmock

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mode selects the API generation being emulated.
type Mode int

const (
	// ModeV32 emulates the official API serving DeepSeek-V3.2.
	ModeV32 Mode = iota
	// ModeR1 emulates an original DeepSeek-R1 deployment.
	ModeR1
)

// Config configures a Server.
type Config struct {
	Mode Mode
	// ThinkTags returns reasoning inside content as <think>…</think>
	// instead of reasoning_content (self-hosted R1 without a reasoning
	// parser). PrefilledThink omits the opening tag.
	ThinkTags      bool
	PrefilledThink bool
	// R1Tools allows tools in ModeR1 (R1-0528 supports function calling;
	// the original R1 rejects them).
	R1Tools bool
	// KeepAlives is the number of ": keep-alive" comments sent before the
	// first stream chunk, as DeepSeek does while a request is queued.
	KeepAlives int
	// Brain decides each reply. Nil answers "ok".
	Brain func(*Request) Reply
}

// Message is a request message as received on the wire.
type Message struct {
	Role             string     `json:"role"`
	Content          *string    `json:"content"`
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
	Name             string     `json:"name,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// Text returns the message content or "".
func (m Message) Text() string {
	if m.Content == nil {
		return ""
	}
	return *m.Content
}

// ToolCall is an OpenAI-format tool call.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Tool is a declared tool.
type Tool struct {
	Type     string `json:"type"`
	Function struct {
		Name       string         `json:"name"`
		Parameters map[string]any `json:"parameters"`
	} `json:"function"`
}

// Request is a decoded chat completions request.
type Request struct {
	Model         string    `json:"model"`
	Messages      []Message `json:"messages"`
	Stream        bool      `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	MaxTokens   *int                   `json:"max_tokens"`
	Temperature *float64               `json:"temperature"`
	Tools       []Tool                 `json:"tools"`
	Logprobs    *bool                  `json:"logprobs"`
	Thinking    *struct{ Type string } `json:"thinking"`
	// Raw is the request body as received.
	Raw json.RawMessage `json:"-"`
}

// Thinking reports whether the request runs in thinking mode.
func (r *Request) ThinkingMode() bool {
	return r.Model == "deepseek-reasoner" || strings.Contains(strings.ToLower(r.Model), "r1") ||
		(r.Thinking != nil && r.Thinking.Type == "enabled")
}

// LastUser returns the index of the last user message, or -1.
func (r *Request) LastUser() int {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role == "user" {
			return i
		}
	}
	return -1
}

// ToolResults returns the tool messages after the last user message.
func (r *Request) ToolResults() []Message {
	var out []Message
	for _, m := range r.Messages[r.LastUser()+1:] {
		if m.Role == "tool" {
			out = append(out, m)
		}
	}
	return out
}

// Reply is what the emulated model produces.
type Reply struct {
	Reasoning string
	Content   string
	// ToolCalls name a tool and its JSON arguments.
	ToolCalls []Call
	// FinishReason overrides the natural one ("stop" / "tool_calls").
	FinishReason string
}

// Call is one tool call in a Reply.
type Call struct {
	Name      string
	Arguments string
}

// Fault is injected instead of (or into) the next response.
type Fault struct {
	// Status, when non-zero, answers with this HTTP status and error.
	Status     int
	Message    string
	RetryAfter string
	// FinishReason returns a normal-looking response with this finish
	// reason, e.g. "insufficient_system_resource" or "length".
	FinishReason string
	// DropAfter closes a stream after this many data chunks without a
	// finish reason or [DONE]; -1 drops before any chunk.
	DropAfter int
}

// Exchange is one recorded request and the outcome.
type Exchange struct {
	Request Request
	Status  int
	Error   string
	Usage   Usage
}

// Usage mirrors DeepSeek's usage object.
type Usage struct {
	PromptTokens          int `json:"prompt_tokens"`
	CompletionTokens      int `json:"completion_tokens"`
	TotalTokens           int `json:"total_tokens"`
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
	CompletionDetails     struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// Server is a running emulator.
type Server struct {
	*httptest.Server
	cfg Config

	mu        sync.Mutex
	faults    []Fault
	exchanges []Exchange
	cache     map[string]bool // prefix hashes seen before
	nextID    int
}

// New starts an emulator. Close it with Server.Close.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg, cache: make(map[string]bool)}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// BaseURL is the OpenAI-compatible base URL (with /v1).
func (s *Server) BaseURL() string { return s.URL + "/v1" }

// Fail queues faults applied to the next requests, in order.
func (s *Server) Fail(faults ...Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, faults...)
}

// Exchanges returns the recorded exchanges.
func (s *Server) Exchanges() []Exchange {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Exchange(nil), s.exchanges...)
}

// Accepted returns the requests that passed validation.
func (s *Server) Accepted() []Request {
	var out []Request
	for _, e := range s.Exchanges() {
		if e.Status == http.StatusOK {
			out = append(out, e.Request)
		}
	}
	return out
}

func (s *Server) record(e Exchange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exchanges = append(s.exchanges, e)
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": typ, "param": nil, "code": "invalid_request_error"}})
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeError(w, 400, "invalid_request_error", "Failed to deserialize the JSON body into the target type")
		return
	}
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, 422, "invalid_request_error", "Failed to deserialize the JSON body into the target type: "+err.Error())
		return
	}
	req.Raw = raw
	if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, "Bearer ") || len(auth) <= len("Bearer ") {
		s.record(Exchange{Request: req, Status: 401, Error: "Authentication Fails"})
		writeError(w, 401, "authentication_error", "Authentication Fails, Your api key is invalid")
		return
	}
	if msg := s.validate(&req); msg != "" {
		s.record(Exchange{Request: req, Status: 400, Error: msg})
		writeError(w, 400, "invalid_request_error", msg)
		return
	}

	s.mu.Lock()
	var fault *Fault
	if len(s.faults) > 0 {
		f := s.faults[0]
		s.faults = s.faults[1:]
		fault = &f
	}
	s.nextID++
	id := fmt.Sprintf("chatcmpl-mock-%d", s.nextID)
	s.mu.Unlock()

	if fault != nil && fault.Status != 0 {
		if fault.RetryAfter != "" {
			w.Header().Set("Retry-After", fault.RetryAfter)
		}
		msg := fault.Message
		if msg == "" {
			msg = http.StatusText(fault.Status)
		}
		s.record(Exchange{Request: req, Status: fault.Status, Error: msg})
		writeError(w, fault.Status, "api_error", msg)
		return
	}

	reply := Reply{Content: "ok"}
	if s.cfg.Brain != nil {
		reply = s.cfg.Brain(&req)
	}
	if !req.ThinkingMode() {
		reply.Reasoning = ""
	}
	finish := "stop"
	if len(reply.ToolCalls) > 0 {
		finish = "tool_calls"
	}
	if reply.FinishReason != "" {
		finish = reply.FinishReason
	}
	if fault != nil && fault.FinishReason != "" {
		finish = fault.FinishReason
		if finish == "insufficient_system_resource" {
			reply = Reply{}
		}
	}
	usage := s.usage(&req, reply)
	s.record(Exchange{Request: req, Status: 200, Usage: usage})

	content, reasoning := reply.Content, reply.Reasoning
	if s.cfg.ThinkTags && reasoning != "" {
		open := "<think>\n"
		if s.cfg.PrefilledThink {
			open = ""
		}
		content = open + reasoning + "\n</think>\n\n" + content
		reasoning = ""
	}
	calls := make([]map[string]any, len(reply.ToolCalls))
	for i, c := range reply.ToolCalls {
		calls[i] = map[string]any{"index": i, "id": fmt.Sprintf("call_%s_%d", id, i), "type": "function",
			"function": map[string]any{"name": c.Name, "arguments": c.Arguments}}
	}
	if !req.Stream {
		message := map[string]any{"role": "assistant", "content": content}
		if reasoning != "" {
			message["reasoning_content"] = reasoning
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
			"usage":   usage,
		})
		return
	}
	s.stream(w, id, req, content, reasoning, calls, finish, usage, fault)
}

// validate applies the request rules of the emulated deployment and
// returns the API's error message, or "".
func (s *Server) validate(req *Request) string {
	if len(req.Messages) == 0 {
		return "Invalid request: messages must not be empty"
	}
	maxOut := 65536
	if s.cfg.Mode == ModeV32 {
		switch req.Model {
		case "deepseek-chat":
			maxOut = 8192
		case "deepseek-reasoner":
		default:
			return "Model Not Exist"
		}
	}
	if req.MaxTokens != nil && (*req.MaxTokens < 1 || *req.MaxTokens > maxOut) {
		return fmt.Sprintf("Invalid max_tokens value, the valid range of max_tokens is [1, %d]", maxOut)
	}
	if req.ThinkingMode() && req.Logprobs != nil && *req.Logprobs {
		return "logprobs is not supported in thinking mode"
	}
	if s.cfg.Mode == ModeR1 && len(req.Tools) > 0 && !s.cfg.R1Tools {
		return "deepseek-reasoner does not support Function Calling"
	}
	// Tool message pairing, as OpenAI-compatible APIs enforce it.
	pending := map[string]bool{}
	for i, m := range req.Messages {
		switch m.Role {
		case "system", "user", "assistant", "tool":
		default:
			return fmt.Sprintf("Invalid role %q at messages[%d]", m.Role, i)
		}
		if m.Role != "tool" && len(pending) > 0 {
			return "An assistant message with 'tool_calls' must be followed by tool messages responding to each 'tool_call_id'"
		}
		switch m.Role {
		case "assistant":
			for _, tc := range m.ToolCalls {
				pending[tc.ID] = true
			}
		case "tool":
			if !pending[m.ToolCallID] {
				return "Messages with role 'tool' must be a response to a preceding message with 'tool_calls'"
			}
			delete(pending, m.ToolCallID)
		}
	}
	if len(pending) > 0 {
		return "An assistant message with 'tool_calls' must be followed by tool messages responding to each 'tool_call_id'"
	}
	if last := req.Messages[len(req.Messages)-1]; last.Role == "assistant" {
		return "The last message must be a user or tool message"
	}
	// Reasoning rules.
	lastUser := req.LastUser()
	for i, m := range req.Messages {
		hasReasoning := m.ReasoningContent != nil && *m.ReasoningContent != ""
		if s.cfg.Mode == ModeR1 && m.ReasoningContent != nil {
			return fmt.Sprintf("Invalid request: reasoning_content is not allowed in input messages (messages[%d])", i)
		}
		if s.cfg.Mode == ModeV32 && req.ThinkingMode() && m.Role == "assistant" && len(m.ToolCalls) > 0 && i > lastUser && !hasReasoning {
			return fmt.Sprintf("Missing `reasoning_content` field in the assistant message at message index %d. In thinking mode, reasoning_content of tool-call turns must be passed back to the API", i)
		}
	}
	return ""
}

// tokens estimates tokens like the runtime's approximate counter.
func tokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// usage computes usage and prefix-cache hits. The context the server sees
// drops reasoning from turns before the last user message, as the API
// does, so those never break the prefix.
func (s *Server) usage(req *Request, reply Reply) Usage {
	lastUser := req.LastUser()
	h := sha256.New()
	prompt, hit := 0, 0
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range req.Messages {
		reasoning := ""
		if m.ReasoningContent != nil && i > lastUser {
			reasoning = *m.ReasoningContent
		}
		calls, _ := json.Marshal(m.ToolCalls)
		part := m.Role + "\x00" + m.Text() + "\x00" + reasoning + "\x00" + m.ToolCallID + "\x00" + string(calls)
		h.Write([]byte(part))
		prompt += tokens(part)
		key := hex.EncodeToString(h.Sum(nil))
		if s.cache[key] && prompt > hit {
			hit = prompt
		}
		s.cache[key] = true
	}
	hit = hit / 64 * 64 // cache granularity
	completion := tokens(reply.Content) + tokens(reply.Reasoning)
	for _, c := range reply.ToolCalls {
		completion += tokens(c.Name + c.Arguments)
	}
	u := Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion,
		PromptCacheHitTokens: hit, PromptCacheMissTokens: prompt - hit}
	u.CompletionDetails.ReasoningTokens = tokens(reply.Reasoning)
	return u
}

// chunks splits s into pieces of at most n bytes, at rune boundaries.
func chunks(s string, n int) []string {
	var out []string
	for len(s) > 0 {
		cut := n
		if cut >= len(s) {
			out = append(out, s)
			break
		}
		for cut > 0 && !utf8Start(s[cut]) {
			cut--
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	return out
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

func (s *Server) stream(w http.ResponseWriter, id string, req Request, content, reasoning string, calls []map[string]any, finish string, usage Usage, fault *Fault) {
	flusher := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	for i := 0; i < s.cfg.KeepAlives; i++ {
		fmt.Fprint(w, ": keep-alive\n\n")
		flusher.Flush()
	}
	drop := -2
	if fault != nil && fault.DropAfter != 0 {
		drop = fault.DropAfter
	}
	if drop == -1 {
		return
	}
	sent := 0
	send := func(delta map[string]any, finishReason any) bool {
		if drop >= 0 && sent >= drop {
			return false
		}
		chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}}}
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
		sent++
		return true
	}
	if !send(map[string]any{"role": "assistant", "content": ""}, nil) {
		return
	}
	for _, part := range chunks(reasoning, 7) {
		if !send(map[string]any{"reasoning_content": part, "content": nil}, nil) {
			return
		}
	}
	// Small chunks split <think> tags across deltas, as real streams do.
	for _, part := range chunks(content, 5) {
		if !send(map[string]any{"content": part}, nil) {
			return
		}
	}
	for i, c := range calls {
		fn := c["function"].(map[string]any)
		head := map[string]any{"index": i, "id": c["id"], "type": "function", "function": map[string]any{"name": fn["name"], "arguments": ""}}
		if !send(map[string]any{"tool_calls": []any{head}}, nil) {
			return
		}
		for _, part := range chunks(fn["arguments"].(string), 4) {
			if !send(map[string]any{"tool_calls": []any{map[string]any{"index": i, "function": map[string]any{"arguments": part}}}}, nil) {
				return
			}
		}
	}
	if !send(map[string]any{}, finish) {
		return
	}
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		b, _ := json.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "model": req.Model, "choices": []any{}, "usage": usage})
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// Arg extracts a string argument from a tool call's JSON arguments.
func Arg(arguments, key string) string {
	var m map[string]any
	_ = json.Unmarshal([]byte(arguments), &m)
	switch v := m[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

// FirstParam returns the first declared parameter name of a tool.
func FirstParam(t Tool) string {
	props, _ := t.Function.Parameters["properties"].(map[string]any)
	for name := range props {
		return name
	}
	return ""
}

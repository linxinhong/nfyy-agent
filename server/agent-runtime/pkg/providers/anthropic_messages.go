package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/internal/httpx"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

const anthropicBaseURL = "https://api.anthropic.com"

const anthropicVersion = "2023-06-01"

// defaultAnthropicMaxTokens is used because the messages API requires
// max_tokens and neither the call options nor the model set one.
const defaultAnthropicMaxTokens = 4096

// anthropicMessagesChat is the anthropic-messages dialect implementation of
// models.Model.
type anthropicMessagesChat struct {
	model      Model
	apiKey     string
	timeout    time.Duration
	httpClient *http.Client
	// unaryClient and streamClient serve calls when httpClient is nil.
	unaryClient  *http.Client
	streamClient *http.Client
}

func newAnthropicMessagesChat(m Model, opts ChatOptions) *anthropicMessagesChat {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultChatTimeout
	}
	return &anthropicMessagesChat{
		model:      m,
		apiKey:     opts.APIKey,
		timeout:    timeout,
		httpClient: opts.HTTPClient,
		// Built once on the shared, pooled transport.
		unaryClient:  httpx.Client(timeout),
		streamClient: httpx.Client(0),
	}
}

// streamHTTPClient has no total timeout; doStream enforces an idle
// timeout instead so long reasoning streams are not cut off.
func (c *anthropicMessagesChat) streamHTTPClient() *http.Client {
	if c.httpClient != nil {
		return c.httpClient
	}
	return c.streamClient
}

func (c *anthropicMessagesChat) getHTTPClient() *http.Client {
	if c.httpClient != nil {
		return c.httpClient
	}
	return c.unaryClient
}

func (c *anthropicMessagesChat) endpoint() string {
	base := c.model.BaseURL
	if base == "" {
		base = anthropicBaseURL
	}
	return strings.TrimRight(base, "/") + "/v1/messages"
}

// ID returns the model identifier.
func (c *anthropicMessagesChat) ID() string {
	return c.model.ID
}

// Provider returns the provider identifier.
func (c *anthropicMessagesChat) Provider() string {
	return c.model.Provider
}

// Response returns a complete model response.
func (c *anthropicMessagesChat) Response(ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
	reqBody := c.buildRequest(messages, opts, false)
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)

	resp, err := c.getHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, newHTTPError(string(APIAnthropicMessages), resp)
	}

	var out anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("anthropic-messages: decode response: %w", err)
	}
	return parseAnthropicResponse(&out, opts), nil
}

// ResponseStream returns a channel of incremental stream events.
func (c *anthropicMessagesChat) ResponseStream(ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error) {
	reqBody := c.buildRequest(messages, opts, true)
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	resp, err := doStream(ctx, c.streamHTTPClient(), c.timeout, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		c.setHeaders(req)
		req.Header.Set("Accept", "text/event-stream")
		return req, nil
	})
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, newHTTPError(string(APIAnthropicMessages), resp)
	}

	ch := make(chan models.StreamEvent)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		streamAnthropicEvents(ctx, resp.Body, ch)
	}()

	return ch, nil
}

func (c *anthropicMessagesChat) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", anthropicVersion)
	if c.apiKey != "" {
		req.Header.Set("x-api-key", c.apiKey)
	}
	for k, v := range c.model.Headers {
		req.Header.Set(k, v)
	}
}

func (c *anthropicMessagesChat) buildRequest(messages []models.Message, opts models.ModelOptions, stream bool) anthropicRequest {
	system, msgs, historyEnd := toAnthropicMessages(messages)
	maxTokens := c.model.MaxOutputTokens
	if opts.MaxTokens != nil {
		maxTokens = *opts.MaxTokens
	}
	if maxTokens <= 0 {
		maxTokens = defaultAnthropicMaxTokens
	}

	req := anthropicRequest{
		Model:     c.model.ID,
		Messages:  msgs,
		MaxTokens: maxTokens,
		Stream:    stream,
	}
	req.System = system
	req.Temperature = opts.Temperature
	req.TopP = opts.TopP
	if len(opts.Stop) > 0 {
		req.StopSequences = opts.Stop
	}
	if len(opts.Tools) > 0 {
		req.Tools = toAnthropicTools(opts.Tools)
	}
	if opts.ToolChoice != nil {
		req.ToolChoice = toAnthropicToolChoice(opts.ToolChoice)
	}
	if !c.model.DisablePromptCache {
		markCacheBreakpoints(&req, historyEnd)
	}
	// ResponseFormat has no Anthropic equivalent; JSON output is driven by
	// the system-prompt instructions the agent adds for structured output.
	return req
}

// toAnthropicMessages converts the unified message list. Leading system
// messages become the top-level system blocks. Per-request context
// (models.IsEphemeralContext) and any system message after the
// conversation started become user text blocks instead: folding them into
// system would change the cached prefix on every request. Tool results become
// tool_result blocks, and consecutive same-role messages merge because the
// API requires strict role alternation. historyEnd locates the last block
// before the first such late system message (message index, block index),
// or -1 when there is none.
func toAnthropicMessages(messages []models.Message) ([]anthropicBlock, []anthropicMessage, [2]int) {
	var system []anthropicBlock
	var out []anthropicMessage
	historyEnd := [2]int{-1, -1}
	started := false

	appendMsg := func(role string, blocks []anthropicBlock) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, anthropicMessage{Role: role, Content: blocks})
	}

	for _, m := range messages {
		switch m.Role {
		case models.RoleSystem:
			if m.Content == "" {
				continue
			}
			if !started && !models.IsEphemeralContext(m) {
				system = append(system, anthropicBlock{Type: "text", Text: m.Content})
				continue
			}
			if historyEnd[0] < 0 && len(out) > 0 {
				last := len(out) - 1
				historyEnd = [2]int{last, len(out[last].Content) - 1}
			}
			appendMsg("user", []anthropicBlock{{Type: "text", Text: m.Content}})
		case models.RoleUser:
			started = true
			appendMsg("user", []anthropicBlock{{Type: "text", Text: m.Content}})
		case models.RoleAssistant:
			started = true
			// ReasoningContent is deliberately not replayed: Anthropic only
			// accepts thinking blocks carrying their original signature, and
			// reasoning from another provider (e.g. a DeepSeek-R1 primary
			// before fallback) would be rejected with HTTP 400.
			var blocks []anthropicBlock
			if m.Content != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				var input map[string]any
				if tc.Function.Arguments != "" {
					// Arguments is a JSON string; fall back to wrapping it
					// raw if it does not parse as an object.
					if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
						input = map[string]any{"__raw": tc.Function.Arguments}
					}
				}
				blocks = append(blocks, anthropicBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: input,
				})
			}
			appendMsg("assistant", blocks)
		case models.RoleTool:
			started = true
			// Tool results arrive as user messages carrying tool_result
			// blocks; consecutive ones merge via appendMsg.
			appendMsg("user", []anthropicBlock{{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   m.Content,
			}})
		}
	}
	return system, out, historyEnd
}

// markCacheBreakpoints places Anthropic's at most four cache breakpoints on
// the stable prefix: the last tool definition, the last system block, the
// end of the history (so the next turn reuses it), and the last block of
// the request (so later rounds of a tool loop reuse this one). Prefixes
// shorter than the model's minimum are simply not cached by the API.
func markCacheBreakpoints(req *anthropicRequest, historyEnd [2]int) {
	if n := len(req.Tools); n > 0 {
		req.Tools[n-1].CacheControl = ephemeralCache
	}
	if n := len(req.System); n > 0 {
		req.System[n-1].CacheControl = ephemeralCache
	}
	mark := func(msg, block int) {
		if msg < 0 || msg >= len(req.Messages) || block < 0 || block >= len(req.Messages[msg].Content) {
			return
		}
		b := &req.Messages[msg].Content[block]
		if b.Type == "text" && b.Text == "" {
			return // the API rejects cache_control on empty text
		}
		b.CacheControl = ephemeralCache
	}
	mark(historyEnd[0], historyEnd[1])
	if n := len(req.Messages); n > 0 {
		mark(n-1, len(req.Messages[n-1].Content)-1)
	}
}

func toAnthropicTools(tools []models.Tool) []anthropicTool {
	out := make([]anthropicTool, len(tools))
	for i, t := range tools {
		out[i] = anthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		}
	}
	return out
}

func toAnthropicToolChoice(tc *models.ToolChoice) *anthropicToolChoice {
	switch tc.Mode {
	case models.ToolChoiceNone:
		return &anthropicToolChoice{Type: "none"}
	case models.ToolChoiceRequired:
		return &anthropicToolChoice{Type: "any"}
	default:
		if tc.Name != "" {
			return &anthropicToolChoice{Type: "tool", Name: tc.Name}
		}
		return &anthropicToolChoice{Type: "auto"}
	}
}

func parseAnthropicResponse(resp *anthropicResponse, opts models.ModelOptions) *models.ModelResponse {
	mr := &models.ModelResponse{FinishReason: models.NormalizeFinishReason(resp.StopReason)}
	var order int
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			mr.Content += b.Text
		case "thinking":
			mr.ReasoningContent += b.Thinking
		case "tool_use":
			var args string
			if b.Input != nil {
				if raw, err := json.Marshal(b.Input); err == nil {
					args = string(raw)
				}
			}
			mr.ToolCalls = append(mr.ToolCalls, models.ToolCall{
				Index: order,
				ID:    b.ID,
				Type:  "function",
				Function: models.FunctionCall{
					Name:      b.Name,
					Arguments: args,
				},
			})
			order++
		}
	}
	if resp.Usage != nil {
		input := resp.Usage.InputTokens + resp.Usage.CacheReadInputTokens + resp.Usage.CacheCreationInputTokens
		mr.Usage = &models.MessageMetrics{
			InputTokens:       input,
			OutputTokens:      resp.Usage.OutputTokens,
			TotalTokens:       input + resp.Usage.OutputTokens,
			CachedInputTokens: resp.Usage.CacheReadInputTokens,
			CacheWriteTokens:  resp.Usage.CacheCreationInputTokens,
		}
	}
	if opts.ResponseFormat != nil && opts.ResponseFormat.Type != models.ResponseFormatDefault && mr.Content != "" {
		var parsed any
		if err := json.Unmarshal([]byte(mr.Content), &parsed); err == nil {
			mr.Parsed = parsed
		}
	}
	return mr
}

// streamAnthropicEvents decodes the messages API SSE stream and forwards
// unified stream events. Anthropic block indices are remapped to dense tool
// call indices so consumers see the same shape as the completions dialect.
// The stream is complete only at message_stop; EOF before it is
// ErrStreamInterrupted.
func streamAnthropicEvents(ctx context.Context, body io.Reader, ch chan<- models.StreamEvent) {
	out := streamSender{ctx: ctx, ch: ch}
	scanner := newSSEScanner(body)
	blockToolIndex := map[int]int{}
	nextToolIndex := 0
	inputTokens := 0
	cachedTokens := 0
	cacheWriteTokens := 0
	finish := models.FinishReasonUnknown

	for scanner.Scan() {
		data, ok := sseData(scanner.Text())
		if !ok || data == "" {
			continue
		}

		var ev anthropicStreamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			out.send(models.StreamEvent{Event: "error", Err: fmt.Errorf("anthropic-messages: decode stream event: %w", err)})
			return
		}

		sent := true
		switch ev.Type {
		case "message_start":
			if ev.Message != nil && ev.Message.Usage != nil {
				inputTokens = ev.Message.Usage.InputTokens + ev.Message.Usage.CacheReadInputTokens + ev.Message.Usage.CacheCreationInputTokens
				cachedTokens = ev.Message.Usage.CacheReadInputTokens
				cacheWriteTokens = ev.Message.Usage.CacheCreationInputTokens
			}
		case "content_block_start":
			if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
				toolIdx := nextToolIndex
				nextToolIndex++
				blockToolIndex[ev.Index] = toolIdx
				sent = out.send(models.StreamEvent{
					Event: "tool_call_delta",
					ToolCall: &models.ToolCall{
						Index: toolIdx,
						ID:    ev.ContentBlock.ID,
						Type:  "function",
						Function: models.FunctionCall{
							Name: ev.ContentBlock.Name,
						},
					},
				})
			}
		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				sent = out.send(models.StreamEvent{Event: "content_delta", Content: ev.Delta.Text})
			case "thinking_delta":
				sent = out.send(models.StreamEvent{Event: "reasoning_delta", ReasoningContent: ev.Delta.Thinking})
			case "input_json_delta":
				if toolIdx, ok := blockToolIndex[ev.Index]; ok {
					sent = out.send(models.StreamEvent{
						Event: "tool_call_delta",
						ToolCall: &models.ToolCall{
							Index:    toolIdx,
							Function: models.FunctionCall{Arguments: ev.Delta.PartialJSON},
						},
					})
				}
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				finish = models.NormalizeFinishReason(ev.Delta.StopReason)
			}
			if ev.Usage != nil {
				sent = out.send(models.StreamEvent{
					Event: "usage",
					Usage: &models.MessageMetrics{
						InputTokens:       inputTokens,
						OutputTokens:      ev.Usage.OutputTokens,
						TotalTokens:       inputTokens + ev.Usage.OutputTokens,
						CachedInputTokens: cachedTokens,
						CacheWriteTokens:  cacheWriteTokens,
					},
				})
			}
		case "message_stop":
			if finish != models.FinishReasonUnknown && !out.send(models.StreamEvent{Event: "finish", FinishReason: finish}) {
				return
			}
			out.send(models.StreamEvent{Event: "done", Done: true})
			return
		case "error":
			perr := &models.ProviderError{Provider: string(APIAnthropicMessages), Message: "stream error"}
			if ev.Error != nil {
				perr.Code = ev.Error.Type
				perr.Message = truncateRunes(ev.Error.Message, maxErrorMessageRunes)
				if ev.Error.Type == "overloaded_error" {
					perr.StatusCode = 529
				}
			}
			out.send(models.StreamEvent{Event: "error", Err: perr})
			return
		}
		if !sent {
			return
		}
	}

	if ctx.Err() != nil {
		return
	}
	if err := scanner.Err(); err != nil {
		out.send(models.StreamEvent{Event: "error", Err: fmt.Errorf("anthropic-messages: %w: %v", models.ErrStreamInterrupted, err)})
		return
	}
	out.send(models.StreamEvent{Event: "error", Err: fmt.Errorf("anthropic-messages: %w", models.ErrStreamInterrupted)})
}

// Request/response types mirroring the Anthropic messages API.

type anthropicRequest struct {
	Model         string               `json:"model"`
	Messages      []anthropicMessage   `json:"messages"`
	MaxTokens     int                  `json:"max_tokens"`
	System        []anthropicBlock     `json:"system,omitempty"`
	Temperature   *float32             `json:"temperature,omitempty"`
	TopP          *float32             `json:"top_p,omitempty"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Tools         []anthropicTool      `json:"tools,omitempty"`
	ToolChoice    *anthropicToolChoice `json:"tool_choice,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	Thinking  string         `json:"thinking,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   string         `json:"content,omitempty"`
	// CacheControl marks a prompt-cache breakpoint after this block.
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicCacheControl struct {
	Type string `json:"type"`
}

var ephemeralCache = &anthropicCacheControl{Type: "ephemeral"}

type anthropicTool struct {
	Name         string                 `json:"name"`
	Description  string                 `json:"description,omitempty"`
	InputSchema  map[string]any         `json:"input_schema"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type anthropicResponse struct {
	ID         string           `json:"id,omitempty"`
	Type       string           `json:"type,omitempty"`
	Role       string           `json:"role,omitempty"`
	Content    []anthropicBlock `json:"content,omitempty"`
	StopReason string           `json:"stop_reason,omitempty"`
	Usage      *anthropicUsage  `json:"usage,omitempty"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens,omitempty"`
	OutputTokens             int `json:"output_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

type anthropicStreamEvent struct {
	Type         string             `json:"type"`
	Index        int                `json:"index,omitempty"`
	Message      *anthropicResponse `json:"message,omitempty"`
	ContentBlock *anthropicBlock    `json:"content_block,omitempty"`
	Delta        *anthropicDelta    `json:"delta,omitempty"`
	Usage        *anthropicUsage    `json:"usage,omitempty"`
	Error        *anthropicError    `json:"error,omitempty"`
}

type anthropicDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}

type anthropicError struct {
	Type    string `json:"type,omitempty"`
	Message string `json:"message,omitempty"`
}

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

const openAIBaseURL = "https://api.openai.com/v1"

const defaultChatTimeout = 120 * time.Second

// OpenAICompletionsCompat overrides compatibility detection for
// OpenAI-compatible endpoints. Nil fields keep the auto-detected behavior.
type OpenAICompletionsCompat struct {
	// SupportsJSONSchema overrides native strict json_schema detection.
	// Auto behavior: true only for the official OpenAI endpoint.
	SupportsJSONSchema *bool
	// ReasoningReplay selects which assistant reasoning_content is sent
	// back. Auto: none for DeepSeek-R1 family models on third-party hosts
	// (original R1 rejects it), all otherwise (the official DeepSeek API
	// requires it on thinking-mode tool-call turns).
	ReasoningReplay ReasoningReplay
	// ParseThinkTags splits <think>…</think> blocks emitted inside content
	// into reasoning. Auto: on for the DeepSeek-R1 family on any host.
	ParseThinkTags *bool
	// PrefilledThink declares that the deployment's chat template
	// pre-fills "<think>" (common for self-hosted R1 on vLLM or Ollama), so
	// streamed content is reasoning until "</think>" and is emitted live.
	// Without it, untagged content is held back until "</think>" proves it
	// was reasoning, the stream ends, or 32 KiB accumulate; content from
	// endpoints that send reasoning_content separately is never held.
	PrefilledThink bool
}

// openAICompletionsChat is the openai-completions dialect implementation of
// models.Model, driven by a Model value object.
type openAICompletionsChat struct {
	model      Model
	apiKey     string
	timeout    time.Duration
	httpClient *http.Client
	// unaryClient and streamClient serve calls when httpClient is nil.
	unaryClient  *http.Client
	streamClient *http.Client
}

func newOpenAICompletionsChat(m Model, opts ChatOptions) *openAICompletionsChat {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultChatTimeout
	}
	return &openAICompletionsChat{
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
func (c *openAICompletionsChat) streamHTTPClient() *http.Client {
	if c.httpClient != nil {
		return c.httpClient
	}
	return c.streamClient
}

func (c *openAICompletionsChat) getHTTPClient() *http.Client {
	if c.httpClient != nil {
		return c.httpClient
	}
	return c.unaryClient
}

func (c *openAICompletionsChat) base() string {
	if c.model.BaseURL != "" {
		return strings.TrimRight(c.model.BaseURL, "/")
	}
	return openAIBaseURL
}

// SupportsNativeJSONSchema reports whether the endpoint supports strict
// json_schema response formats. Auto behavior: only the official OpenAI
// endpoint; OpenAI-compatible endpoints are assumed to support json_object
// at best. Override via Model.Compat.
func (c *openAICompletionsChat) SupportsNativeJSONSchema() bool {
	if c.model.Compat != nil && c.model.Compat.SupportsJSONSchema != nil {
		return *c.model.Compat.SupportsJSONSchema
	}
	return c.base() == openAIBaseURL
}

// ID returns the model identifier.
func (c *openAICompletionsChat) ID() string {
	return c.model.ID
}

// Provider returns the provider identifier.
func (c *openAICompletionsChat) Provider() string {
	return c.model.Provider
}

func (c *openAICompletionsChat) errorLabel() string {
	if c.model.Provider != "" {
		return c.model.Provider
	}
	return string(APIOpenAICompletions)
}

// Response returns a complete model response.
func (c *openAICompletionsChat) Response(ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
	reqBody := c.buildRequest(messages, opts, false)
	c.applyReasoningEffort(ctx, &reqBody)
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+"/chat/completions", bytes.NewReader(body))
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
		return nil, newHTTPError(c.errorLabel(), resp)
	}

	var out chatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%s: decode response: %w", c.errorLabel(), err)
	}
	mr := parseChatCompletionResponse(&out, opts, c.parseThinkTags())
	if mr.FinishReason == models.FinishReasonResourceExhausted {
		return nil, fmt.Errorf("%s: %w", c.errorLabel(), models.ErrResourceExhausted)
	}
	return mr, nil
}

// ResponseStream returns a channel of incremental stream events.
func (c *openAICompletionsChat) ResponseStream(ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error) {
	reqBody := c.buildRequest(messages, opts, true)
	c.applyReasoningEffort(ctx, &reqBody)
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	resp, err := doStream(ctx, c.streamHTTPClient(), c.timeout, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+"/chat/completions", bytes.NewReader(body))
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
		return nil, newHTTPError(c.errorLabel(), resp)
	}

	ch := make(chan models.StreamEvent)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		c.streamEvents(ctx, resp.Body, ch)
	}()

	return ch, nil
}

// streamEvents decodes the chat-completions SSE stream. A stream is complete
// only after "[DONE]" or a reported finish_reason; EOF before either is
// ErrStreamInterrupted, so a dropped connection never yields a partial
// answer that looks finished.
func (c *openAICompletionsChat) streamEvents(ctx context.Context, body io.Reader, ch chan<- models.StreamEvent) {
	out := streamSender{ctx: ctx, ch: ch}
	var think *thinkStreamParser
	if c.parseThinkTags() {
		think = newThinkStreamParser(c.model.Compat != nil && c.model.Compat.PrefilledThink)
	}
	finish := models.FinishReasonUnknown
	sawFinish := false

	emitContent := func(content string) bool {
		if think == nil {
			return content == "" || out.send(models.StreamEvent{Event: "content_delta", Content: content})
		}
		reasoning, answer := think.feed(content)
		if reasoning != "" && !out.send(models.StreamEvent{Event: "reasoning_delta", ReasoningContent: reasoning}) {
			return false
		}
		return answer == "" || out.send(models.StreamEvent{Event: "content_delta", Content: answer})
	}
	complete := func() {
		if think != nil {
			reasoning, answer := think.flush()
			if reasoning != "" && !out.send(models.StreamEvent{Event: "reasoning_delta", ReasoningContent: reasoning}) {
				return
			}
			if answer != "" && !out.send(models.StreamEvent{Event: "content_delta", Content: answer}) {
				return
			}
		}
		if finish == models.FinishReasonResourceExhausted {
			out.send(models.StreamEvent{Event: "error", Err: fmt.Errorf("%s: %w", c.errorLabel(), models.ErrResourceExhausted)})
			return
		}
		if sawFinish && !out.send(models.StreamEvent{Event: "finish", FinishReason: finish}) {
			return
		}
		out.send(models.StreamEvent{Event: "done", Done: true})
	}

	scanner := newSSEScanner(body)
	for scanner.Scan() {
		data, ok := sseData(scanner.Text())
		if !ok || data == "" {
			continue
		}
		if data == "[DONE]" {
			complete()
			return
		}

		var chunk chatCompletionStreamResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			out.send(models.StreamEvent{Event: "error", Err: fmt.Errorf("%s: decode stream chunk: %w", c.errorLabel(), err)})
			return
		}
		if chunk.Error != nil {
			out.send(models.StreamEvent{Event: "error", Err: &models.ProviderError{
				Provider: c.errorLabel(), Code: chunk.Error.Type, Message: truncateRunes(chunk.Error.Message, maxErrorMessageRunes),
			}})
			return
		}
		for _, ev := range parseStreamResponse(&chunk) {
			var sent bool
			switch ev.Event {
			case "content_delta":
				sent = emitContent(ev.Content)
			case "reasoning_delta":
				if think != nil {
					think.sawReasoningField()
				}
				sent = out.send(ev)
			case "finish":
				finish, sawFinish = ev.FinishReason, true
				sent = true
			default:
				sent = out.send(ev)
			}
			if !sent {
				return
			}
		}
	}

	if err := scanner.Err(); err != nil {
		if ctx.Err() == nil {
			out.send(models.StreamEvent{Event: "error", Err: fmt.Errorf("%s: %w: %v", c.errorLabel(), models.ErrStreamInterrupted, err)})
		}
		return
	}
	if !sawFinish {
		out.send(models.StreamEvent{Event: "error", Err: fmt.Errorf("%s: %w", c.errorLabel(), models.ErrStreamInterrupted)})
		return
	}
	complete()
}

// setHeaders applies authentication, content type, and any per-model extra
// headers (e.g. OpenRouter attribution headers).
func (c *openAICompletionsChat) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	for k, v := range c.model.Headers {
		req.Header.Set(k, v)
	}
}

func (c *openAICompletionsChat) buildRequest(messages []models.Message, opts models.ModelOptions, stream bool) chatCompletionRequest {
	req := chatCompletionRequest{
		Model:    c.model.ID,
		Messages: toOpenAIMessages(messages),
		Stream:   stream,
	}
	applyReasoningReplay(req.Messages, c.reasoningReplay())
	if stream {
		req.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	req.Temperature = opts.Temperature
	req.TopP = opts.TopP
	if opts.MaxTokens != nil {
		maxTokens := *opts.MaxTokens
		// Catalog limits are authoritative: DeepSeek rejects max_tokens above
		// the model ceiling (8K for deepseek-chat) with HTTP 400.
		if c.model.MaxOutputTokens > 0 && maxTokens > c.model.MaxOutputTokens {
			maxTokens = c.model.MaxOutputTokens
		}
		req.MaxTokens = maxTokens
	}
	if len(opts.Stop) > 0 {
		req.Stop = opts.Stop
	}
	if opts.ResponseFormat != nil {
		req.ResponseFormat = toOpenAIResponseFormat(opts.ResponseFormat)
	}
	if len(opts.Tools) > 0 {
		req.Tools = toOpenAITools(opts.Tools)
	}
	if opts.ToolChoice != nil {
		req.ToolChoice = toOpenAIToolChoice(opts.ToolChoice)
	}
	return req
}

func toOpenAIToolChoice(tc *models.ToolChoice) any {
	switch tc.Mode {
	case models.ToolChoiceNone, models.ToolChoiceAuto, models.ToolChoiceRequired:
		return string(tc.Mode)
	default:
		if tc.Name != "" {
			return map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": tc.Name,
				},
			}
		}
		return string(models.ToolChoiceAuto)
	}
}

func toOpenAIMessages(messages []models.Message) []chatCompletionMessage {
	out := make([]chatCompletionMessage, len(messages))
	for i, m := range messages {
		msg := chatCompletionMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
			// DeepSeek 思考模式：带 tools 的多轮请求必须回传历史 reasoning_content，
			// 否则 API 返回 400（Invalid assistant message）。
			ReasoningContent: m.ReasoningContent,
		}
		if len(m.ToolCalls) > 0 {
			msg.ToolCalls = make([]toolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				msg.ToolCalls[j] = toolCall{
					ID:   tc.ID,
					Type: tc.Type,
					Function: struct {
						Name      string `json:"name,omitempty"`
						Arguments string `json:"arguments,omitempty"`
					}{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				}
			}
		}
		out[i] = msg
	}
	return out
}

func toOpenAITools(tools []models.Tool) []toolDefinition {
	out := make([]toolDefinition, len(tools))
	for i, t := range tools {
		out[i] = toolDefinition{
			Type: t.Type,
			Function: functionDefinition{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			},
		}
	}
	return out
}

func toOpenAIResponseFormat(rf *models.ResponseFormat) *responseFormat {
	switch rf.Type {
	case models.ResponseFormatJSON:
		return &responseFormat{Type: "json_object"}
	case models.ResponseFormatJSONSchema:
		name := "response"
		schema := rf.JSONSchema
		if schema != nil {
			if n, ok := schema["name"].(string); ok && n != "" {
				name = n
			}
			if s, ok := schema["schema"].(map[string]any); ok {
				schema = s
			}
		}
		return &responseFormat{
			Type: "json_schema",
			JSONSchema: &jsonSchema{
				Name:   name,
				Schema: schema,
				Strict: true,
			},
		}
	default:
		return nil
	}
}

func parseChatCompletionResponse(resp *chatCompletionResponse, opts models.ModelOptions, thinkTags bool) *models.ModelResponse {
	mr := &models.ModelResponse{}
	if resp.Usage != nil {
		mr.Usage = usageToMetrics(resp.Usage)
	}
	if len(resp.Choices) == 0 {
		return mr
	}

	msg := resp.Choices[0].Message
	mr.FinishReason = models.NormalizeFinishReason(resp.Choices[0].FinishReason)
	mr.Content = msg.Content
	mr.ReasoningContent = msg.reasoning()
	if thinkTags {
		if reasoning, answer := splitThinkTags(mr.Content); reasoning != "" || answer != mr.Content {
			mr.Content = answer
			if mr.ReasoningContent == "" {
				mr.ReasoningContent = reasoning
			}
		}
	}
	if len(msg.ToolCalls) > 0 {
		mr.ToolCalls = make([]models.ToolCall, len(msg.ToolCalls))
		for i, tc := range msg.ToolCalls {
			mr.ToolCalls[i] = models.ToolCall{
				ID:   tc.ID,
				Type: tc.Type,
				Function: models.FunctionCall{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			}
		}
	}
	// Fill Parsed when a JSON response format was requested so the agent can
	// skip re-parsing.
	if opts.ResponseFormat != nil && opts.ResponseFormat.Type != models.ResponseFormatDefault && mr.Content != "" {
		var parsed any
		if err := json.Unmarshal([]byte(mr.Content), &parsed); err == nil {
			mr.Parsed = parsed
		}
	}
	return mr
}

func parseStreamResponse(resp *chatCompletionStreamResponse) []models.StreamEvent {
	var events []models.StreamEvent

	if len(resp.Choices) == 0 {
		if resp.Usage != nil {
			events = append(events, models.StreamEvent{
				Event: "usage",
				Usage: usageToMetrics(resp.Usage),
			})
		}
		return events
	}

	delta := resp.Choices[0].Delta
	if r := delta.reasoning(); r != "" {
		events = append(events, models.StreamEvent{
			Event:            "reasoning_delta",
			ReasoningContent: r,
		})
	}
	if delta.Content != "" {
		events = append(events, models.StreamEvent{
			Event:   "content_delta",
			Content: delta.Content,
		})
	}

	for _, tc := range delta.ToolCalls {
		toolCall := &models.ToolCall{
			Index: tc.Index,
			ID:    tc.ID,
			Type:  tc.Type,
		}
		toolCall.Function = models.FunctionCall{
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		}
		events = append(events, models.StreamEvent{
			Event:    "tool_call_delta",
			ToolCall: toolCall,
		})
	}

	if resp.Usage != nil {
		events = append(events, models.StreamEvent{
			Event: "usage",
			Usage: usageToMetrics(resp.Usage),
		})
	}
	if raw := resp.Choices[0].FinishReason; raw != "" {
		events = append(events, models.StreamEvent{
			Event:        "finish",
			FinishReason: models.NormalizeFinishReason(raw),
		})
	}

	return events
}

func usageToMetrics(u *usage) *models.MessageMetrics {
	if u == nil {
		return nil
	}
	m := &models.MessageMetrics{
		InputTokens:       u.PromptTokens,
		OutputTokens:      u.CompletionTokens,
		TotalTokens:       u.TotalTokens,
		CachedInputTokens: u.PromptCacheHitTokens,
	}
	if m.CachedInputTokens == 0 && u.PromptTokensDetails != nil {
		m.CachedInputTokens = u.PromptTokensDetails.CachedTokens
	}
	if u.CompletionTokensDetails != nil {
		m.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}
	return m
}

// Request/response types mirroring the OpenAI chat completions API.

type chatCompletionRequest struct {
	Model          string                  `json:"model"`
	Messages       []chatCompletionMessage `json:"messages"`
	Temperature    *float32                `json:"temperature,omitempty"`
	MaxTokens      int                     `json:"max_tokens,omitempty"`
	TopP           *float32                `json:"top_p,omitempty"`
	Stop           []string                `json:"stop,omitempty"`
	ResponseFormat *responseFormat         `json:"response_format,omitempty"`
	Tools          []toolDefinition        `json:"tools,omitempty"`
	ToolChoice     any                     `json:"tool_choice,omitempty"`
	Stream         bool                    `json:"stream,omitempty"`
	StreamOptions  *streamOptions          `json:"stream_options,omitempty"`
	// ReasoningEffort 与 Thinking 实现 DeepSeek 思考模式契约：
	// reasoning_effort 控制思考强度，thinking.type=enabled 打开思考模式。
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Thinking        *thinkingSwitch `json:"thinking,omitempty"`
}

type thinkingSwitch struct {
	Type string `json:"type"`
}

// applyReasoningEffort 从 context 读取思考强度并写入请求体；ctx 未注入时不动请求。
func (c *openAICompletionsChat) applyReasoningEffort(ctx context.Context, req *chatCompletionRequest) {
	if effort := models.ReasoningEffortFrom(ctx); effort != "" {
		req.ReasoningEffort = effort
		req.Thinking = &thinkingSwitch{Type: "enabled"}
	}
}

type chatCompletionMessage struct {
	Role       string     `json:"role,omitempty"`
	Content    string     `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	// ReasoningContent carries reasoning in DeepSeek's format.
	ReasoningContent string `json:"reasoning_content,omitempty"`
	// Reasoning carries reasoning in OpenRouter's format.
	Reasoning string `json:"reasoning,omitempty"`
}

// reasoning returns the reasoning content in whichever known format the
// endpoint uses (DeepSeek's reasoning_content or OpenRouter's reasoning).
func (m chatCompletionMessage) reasoning() string {
	if m.ReasoningContent != "" {
		return m.ReasoningContent
	}
	return m.Reasoning
}

type toolDefinition struct {
	Type     string             `json:"type"`
	Function functionDefinition `json:"function"`
}

type functionDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type toolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function,omitempty"`
	Index int `json:"index,omitempty"`
}

type responseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *jsonSchema `json:"json_schema,omitempty"`
}

type jsonSchema struct {
	Name   string         `json:"name"`
	Schema map[string]any `json:"schema"`
	Strict bool           `json:"strict"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatCompletionResponse struct {
	ID      string   `json:"id,omitempty"`
	Object  string   `json:"object,omitempty"`
	Created int64    `json:"created,omitempty"`
	Model   string   `json:"model,omitempty"`
	Choices []choice `json:"choices,omitempty"`
	Usage   *usage   `json:"usage,omitempty"`
}

type choice struct {
	Index        int                    `json:"index,omitempty"`
	Message      chatCompletionMessage  `json:"message,omitempty"`
	Delta        *chatCompletionMessage `json:"delta,omitempty"`
	FinishReason string                 `json:"finish_reason,omitempty"`
}

type chatCompletionStreamResponse struct {
	ID      string         `json:"id,omitempty"`
	Object  string         `json:"object,omitempty"`
	Created int64          `json:"created,omitempty"`
	Model   string         `json:"model,omitempty"`
	Choices []streamChoice `json:"choices,omitempty"`
	Usage   *usage         `json:"usage,omitempty"`
	// Error is an in-band stream error some compatible gateways emit.
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

type streamChoice struct {
	Index        int                   `json:"index,omitempty"`
	Delta        chatCompletionMessage `json:"delta,omitempty"`
	FinishReason string                `json:"finish_reason,omitempty"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
	// PromptCacheHitTokens is DeepSeek's context-cache hit count.
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens,omitempty"`
	PromptTokensDetails  *struct {
		CachedTokens int `json:"cached_tokens,omitempty"`
	} `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens,omitempty"`
	} `json:"completion_tokens_details,omitempty"`
}

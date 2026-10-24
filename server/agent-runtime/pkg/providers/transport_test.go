package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

func sseServer(t *testing.T, lines ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, l := range lines {
			_, _ = w.Write([]byte(l + "\n\n"))
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func openAIChat(t *testing.T, m Model) models.Model {
	t.Helper()
	if m.API == "" {
		m.API = APIOpenAICompletions
	}
	chat, err := m.Chat(ChatOptions{APIKey: "key"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	return chat
}

type collected struct {
	content, reasoning string
	finish             models.FinishReason
	usage              *models.MessageMetrics
	err                error
	done               bool
}

func collect(t *testing.T, ch <-chan models.StreamEvent) collected {
	t.Helper()
	var c collected
	for ev := range ch {
		switch ev.Event {
		case "content_delta":
			c.content += ev.Content
		case "reasoning_delta":
			c.reasoning += ev.ReasoningContent
		case "finish":
			c.finish = ev.FinishReason
		case "usage":
			c.usage = ev.Usage
		case "error":
			c.err = ev.Err
		case "done":
			c.done = true
		}
	}
	return c
}

func TestProviderErrorIsTypedBoundedAndCarriesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"Rate limit reached","type":"rate_limit_error","code":"rate_limit_exceeded"}}`))
	}))
	defer srv.Close()

	chat := openAIChat(t, Model{ID: "deepseek-chat", Provider: "deepseek", BaseURL: srv.URL})
	_, err := chat.Response(context.Background(), []models.Message{{Role: models.RoleUser, Content: "hi"}}, models.ModelOptions{})
	var perr *models.ProviderError
	if !errors.As(err, &perr) {
		t.Fatalf("err = %T %v, want *ProviderError", err, err)
	}
	if perr.StatusCode != 429 || perr.Code != "rate_limit_exceeded" || perr.Message != "Rate limit reached" {
		t.Fatalf("unexpected error fields: %+v", perr)
	}
	if perr.RetryAfter != 7*time.Second {
		t.Fatalf("RetryAfter = %v, want 7s", perr.RetryAfter)
	}
	if !models.IsRetryable(err) || models.RetryAfter(err) != 7*time.Second {
		t.Fatalf("429 must be retryable with advised delay")
	}
}

func TestProviderErrorDropsUnrecognizedBodies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>upstream secret-token-123 " + strings.Repeat("x", 200<<10) + "</html>"))
	}))
	defer srv.Close()

	chat := openAIChat(t, Model{ID: "m", Provider: "openai", BaseURL: srv.URL})
	_, err := chat.Response(context.Background(), []models.Message{{Role: models.RoleUser, Content: "hi"}}, models.ModelOptions{})
	if err == nil || strings.Contains(err.Error(), "secret-token") || len(err.Error()) > 200 {
		t.Fatalf("error must not echo raw body: %v", err)
	}
	if !models.IsRetryable(err) {
		t.Fatal("502 must be retryable")
	}
}

func TestProviderErrorNonRetryableStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"Insufficient Balance","type":"unknown_error"}}`))
	}))
	defer srv.Close()
	chat := openAIChat(t, Model{ID: "deepseek-chat", Provider: "deepseek", BaseURL: srv.URL})
	_, err := chat.Response(context.Background(), []models.Message{{Role: models.RoleUser, Content: "hi"}}, models.ModelOptions{})
	if err == nil || models.IsRetryable(err) {
		t.Fatalf("402 must not be retryable: %v", err)
	}
}

func TestParseRetryAfterVariants(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := http.Header{}
	h.Set("retry-after-ms", "1500")
	if got := parseRetryAfter(h, now); got != 1500*time.Millisecond {
		t.Fatalf("ms = %v", got)
	}
	h = http.Header{}
	h.Set("Retry-After", now.Add(30*time.Second).Format(http.TimeFormat))
	if got := parseRetryAfter(h, now); got != 30*time.Second {
		t.Fatalf("date = %v", got)
	}
	h.Set("Retry-After", "100000")
	if got := parseRetryAfter(h, now); got != 5*time.Minute {
		t.Fatalf("cap = %v", got)
	}
}

func TestOpenAIStreamEOFWithoutCompletionIsInterrupted(t *testing.T) {
	srv := sseServer(t, `data: {"choices":[{"index":0,"delta":{"content":"partial ans"}}]}`)
	chat := openAIChat(t, Model{ID: "m", Provider: "openai", BaseURL: srv.URL})
	ch, err := chat.ResponseStream(context.Background(), []models.Message{{Role: models.RoleUser, Content: "hi"}}, models.ModelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, ch)
	if !errors.Is(got.err, models.ErrStreamInterrupted) || got.done {
		t.Fatalf("want interrupted error and no done, got err=%v done=%v", got.err, got.done)
	}
	if !models.IsRetryable(got.err) {
		t.Fatal("interrupted stream must be retryable")
	}
}

func TestOpenAIStreamFinishWithoutDoneCompletes(t *testing.T) {
	srv := sseServer(t,
		`data: {"choices":[{"index":0,"delta":{"content":"ok"}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
	)
	chat := openAIChat(t, Model{ID: "m", Provider: "openai", BaseURL: srv.URL})
	ch, _ := chat.ResponseStream(context.Background(), []models.Message{{Role: models.RoleUser, Content: "hi"}}, models.ModelOptions{})
	got := collect(t, ch)
	if got.err != nil || !got.done || got.finish != models.FinishReasonLength {
		t.Fatalf("got %+v", got)
	}
}

func TestOpenAIStreamHandlesLinesBeyondScannerDefault(t *testing.T) {
	big := strings.Repeat("a", 200<<10)
	srv := sseServer(t,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":"`+big+`"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	)
	chat := openAIChat(t, Model{ID: "m", Provider: "openai", BaseURL: srv.URL})
	ch, _ := chat.ResponseStream(context.Background(), []models.Message{{Role: models.RoleUser, Content: "hi"}}, models.ModelOptions{})
	var args int
	var streamErr error
	for ev := range ch {
		if ev.ToolCall != nil {
			args += len(ev.ToolCall.Function.Arguments)
		}
		if ev.Err != nil {
			streamErr = ev.Err
		}
	}
	if streamErr != nil || args != len(big) {
		t.Fatalf("err=%v args=%d", streamErr, args)
	}
}

func TestOpenAIStreamProducerExitsWhenConsumerStops(t *testing.T) {
	lines := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		lines = append(lines, `data: {"choices":[{"index":0,"delta":{"content":"x"}}]}`)
	}
	srv := sseServer(t, lines...)
	chat := openAIChat(t, Model{ID: "m", Provider: "openai", BaseURL: srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := chat.ResponseStream(ctx, []models.Message{{Role: models.RoleUser, Content: "hi"}}, models.ModelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-ch // read one event, then abandon the stream
	cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("producer goroutine did not exit after cancellation")
		}
	}
}

func TestOpenAIRequestKeepsZeroTemperatureAndClampsMaxTokens(t *testing.T) {
	c := newOpenAICompletionsChat(Model{ID: "deepseek-chat", Provider: "deepseek", MaxOutputTokens: 8192}, ChatOptions{})
	zero := float32(0)
	huge := 100_000
	req := c.buildRequest(nil, models.ModelOptions{Temperature: &zero, MaxTokens: &huge}, false)
	if req.Temperature == nil || *req.Temperature != 0 {
		t.Fatal("explicit temperature 0 must be sent")
	}
	if req.MaxTokens != 8192 {
		t.Fatalf("max_tokens = %d, want clamp to 8192", req.MaxTokens)
	}
}

func TestAnthropicStreamWithoutMessageStopIsInterrupted(t *testing.T) {
	srv := sseServer(t,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half"}}`,
	)
	chat, _ := Model{ID: "claude", API: APIAnthropicMessages, Provider: "anthropic", BaseURL: srv.URL}.Chat(ChatOptions{APIKey: "k"})
	ch, err := chat.ResponseStream(context.Background(), []models.Message{{Role: models.RoleUser, Content: "hi"}}, models.ModelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, ch)
	if !errors.Is(got.err, models.ErrStreamInterrupted) {
		t.Fatalf("err = %v", got.err)
	}
}

func TestAnthropicStreamReportsStopReason(t *testing.T) {
	srv := sseServer(t,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"cut"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":9}}`,
		`data: {"type":"message_stop"}`,
	)
	chat, _ := Model{ID: "claude", API: APIAnthropicMessages, Provider: "anthropic", BaseURL: srv.URL}.Chat(ChatOptions{APIKey: "k"})
	ch, _ := chat.ResponseStream(context.Background(), []models.Message{{Role: models.RoleUser, Content: "hi"}}, models.ModelOptions{})
	got := collect(t, ch)
	if got.err != nil || !got.done || got.finish != models.FinishReasonLength {
		t.Fatalf("got %+v", got)
	}
}

func TestAnthropicDoesNotReplayForeignReasoning(t *testing.T) {
	_, msgs, _ := toAnthropicMessages([]models.Message{
		{Role: models.RoleUser, Content: "q"},
		{Role: models.RoleAssistant, Content: "a", ReasoningContent: "deepseek thoughts"},
	})
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == "thinking" {
				t.Fatal("unsigned thinking block must not be sent")
			}
		}
	}
}

func TestStreamUsesIdleTimeoutNotTotalTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		stall := r.URL.Query().Get("stall") != ""
		for i := 0; i < 6; i++ {
			_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"reasoning_content":"step "}}]}` + "\n\n"))
			flusher.Flush()
			if stall && i == 1 {
				select {
				case <-r.Context().Done():
				case <-time.After(2 * time.Second):
				}
				return
			}
			time.Sleep(80 * time.Millisecond)
		}
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	run := func(base string) collected {
		chat, _ := Model{ID: "deepseek-reasoner", API: APIOpenAICompletions, Provider: "deepseek", BaseURL: base}.Chat(ChatOptions{APIKey: "k", Timeout: 200 * time.Millisecond})
		ch, err := chat.ResponseStream(context.Background(), []models.Message{{Role: models.RoleUser, Content: "q"}}, models.ModelOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return collect(t, ch)
	}
	// ~500ms total with chunks every 80ms: longer than the 200ms timeout.
	if got := run(srv.URL); got.err != nil || got.content != "ok" {
		t.Fatalf("steady long stream was cut off: %+v", got)
	}
	got := run(srv.URL + "/?stall=1")
	if !errors.Is(got.err, models.ErrStreamInterrupted) {
		t.Fatalf("stalled stream: %+v", got)
	}
}

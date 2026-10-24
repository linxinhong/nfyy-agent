package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

func TestDetectFamily(t *testing.T) {
	cases := []struct {
		m    Model
		want modelFamily
	}{
		{Model{ID: "deepseek-chat", Provider: "deepseek"}, familyDeepSeekV3},
		{Model{ID: "deepseek-reasoner", Provider: "deepseek"}, familyDeepSeekR1},
		{Model{ID: "deepseek-ai/DeepSeek-V3.2", Provider: "siliconflow"}, familyDeepSeekV3},
		{Model{ID: "Pro/deepseek-ai/DeepSeek-R1", Provider: "siliconflow"}, familyDeepSeekR1},
		{Model{ID: "deepseek-r1-250528", Provider: "volcengine"}, familyDeepSeekR1},
		{Model{ID: "deepseek-v3-1-terminus", Provider: "volcengine"}, familyDeepSeekV3},
		{Model{ID: "deepseek-r1:14b", Provider: "ollama"}, familyDeepSeekR1},
		{Model{ID: "DeepSeek-R1-Distill-Qwen-32B", Provider: "local"}, familyDeepSeekR1},
		{Model{ID: "gpt-4o", Provider: "openai"}, familyGeneric},
		{Model{ID: "qwen-max", Provider: "qwen"}, familyGeneric},
	}
	for _, tc := range cases {
		if got := detectFamily(tc.m); got != tc.want {
			t.Errorf("detectFamily(%s/%s) = %v, want %v", tc.m.Provider, tc.m.ID, got, tc.want)
		}
	}
}

func TestRegistrySynthesizesR1AsReasoning(t *testing.T) {
	r := NewFromEnv()
	m, err := r.Model("siliconflow", "deepseek-ai/DeepSeek-R1")
	if err != nil || !m.Reasoning {
		t.Fatalf("synthesized R1 should be a reasoning model: %+v %v", m, err)
	}
	m, _ = r.Model("volcengine", "deepseek-v3-250324")
	if m.Reasoning {
		t.Fatal("V3 must not be flagged as reasoning")
	}
	ds, _ := r.Model("deepseek", "deepseek-reasoner")
	if !ds.Reasoning || ds.ContextWindow != 128_000 || ds.MaxOutputTokens != 65_536 {
		t.Fatalf("deepseek-reasoner catalog entry: %+v", ds)
	}
}

func TestSplitThinkTags(t *testing.T) {
	cases := []struct{ in, reasoning, answer string }{
		{"<think>\nplan\n</think>\n\nAnswer", "plan", "Answer"},
		{"  <think>only thinking", "only thinking", ""},
		{"prefilled reasoning\n</think>\n\nAnswer", "prefilled reasoning", "Answer"},
		{"plain answer", "", "plain answer"},
		{"code uses <think> literally", "", "code uses <think> literally"},
	}
	for _, tc := range cases {
		r, a := splitThinkTags(tc.in)
		if r != tc.reasoning || a != tc.answer {
			t.Errorf("splitThinkTags(%q) = (%q,%q), want (%q,%q)", tc.in, r, a, tc.reasoning, tc.answer)
		}
	}
}

func TestThinkStreamParserHandlesSplitTags(t *testing.T) {
	chunks := []string{"\n<th", "ink>step ", "one</thi", "nk>\n\nFin", "al"}
	var p thinkStreamParser
	var reasoning, content string
	for _, c := range chunks {
		r, a := p.feed(c)
		reasoning += r
		content += a
	}
	r, a := p.flush()
	reasoning += r
	content += a
	if reasoning != "step one" || content != "Final" {
		t.Fatalf("reasoning=%q content=%q", reasoning, content)
	}

	var plain thinkStreamParser
	r1, a1 := plain.feed("Hello")
	r2, a2 := plain.flush()
	if r1+r2 != "" || a1+a2 != "Hello" {
		t.Fatalf("plain stream must pass through, got %q/%q", r1+r2, a1+a2)
	}
}

func TestR1StreamSplitsThinkTagsIntoReasoning(t *testing.T) {
	srv := sseServer(t,
		`data: {"choices":[{"index":0,"delta":{"content":"<think>"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"let me think"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"</think>\n\n42"}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12,"completion_tokens_details":{"reasoning_tokens":4}}}`,
		`data: [DONE]`,
	)
	chat := openAIChat(t, Model{ID: "deepseek-ai/DeepSeek-R1", Provider: "siliconflow", BaseURL: srv.URL})
	ch, err := chat.ResponseStream(context.Background(), []models.Message{{Role: models.RoleUser, Content: "q"}}, models.ModelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, ch)
	if got.err != nil || got.reasoning != "let me think" || got.content != "42" || got.finish != models.FinishReasonStop {
		t.Fatalf("got %+v", got)
	}
	if got.usage == nil || got.usage.ReasoningTokens != 4 {
		t.Fatalf("usage = %+v", got.usage)
	}
}

func TestDeepSeekReasonerResponseParsesReasoningAndCacheUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"42","reasoning_content":"think"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_cache_hit_tokens":64,"prompt_cache_miss_tokens":36,"completion_tokens_details":{"reasoning_tokens":15}}}`))
	}))
	defer srv.Close()
	chat := openAIChat(t, Model{ID: "deepseek-reasoner", Provider: "deepseek", BaseURL: srv.URL})
	resp, err := chat.Response(context.Background(), []models.Message{{Role: models.RoleUser, Content: "q"}}, models.ModelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "42" || resp.ReasoningContent != "think" || resp.FinishReason != models.FinishReasonStop {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Usage.CachedInputTokens != 64 || resp.Usage.ReasoningTokens != 15 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestR1NonStreamWithPrefilledThinkTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"reasoning here\n</think>\n\nThe answer"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	chat := openAIChat(t, Model{ID: "deepseek-r1-250528", Provider: "volcengine", BaseURL: srv.URL})
	resp, err := chat.Response(context.Background(), []models.Message{{Role: models.RoleUser, Content: "q"}}, models.ModelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "The answer" || resp.ReasoningContent != "reasoning here" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestDeepSeekInsufficientResourceIsRetryableError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"par"},"finish_reason":"insufficient_system_resource"}]}`))
	}))
	defer srv.Close()
	chat := openAIChat(t, Model{ID: "deepseek-chat", Provider: "deepseek", BaseURL: srv.URL})
	_, err := chat.Response(context.Background(), []models.Message{{Role: models.RoleUser, Content: "q"}}, models.ModelOptions{})
	if !errors.Is(err, models.ErrResourceExhausted) || !models.IsRetryable(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestReasoningReplayPolicies(t *testing.T) {
	history := []models.Message{
		{Role: models.RoleUser, Content: "q1"},
		{Role: models.RoleAssistant, Content: "a1", ReasoningContent: "r-old"},
		{Role: models.RoleUser, Content: "q2"},
		{Role: models.RoleAssistant, ToolCalls: []models.ToolCall{{ID: "c1", Type: "function", Function: models.FunctionCall{Name: "f"}}}, ReasoningContent: "r-now"},
		{Role: models.RoleTool, ToolCallID: "c1", Content: "ok"},
	}
	capture := func(m Model) []chatCompletionMessage {
		var got chatCompletionRequest
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &got)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
		}))
		defer srv.Close()
		m.BaseURL = srv.URL
		if _, err := openAIChat(t, m).Response(context.Background(), history, models.ModelOptions{}); err != nil {
			t.Fatal(err)
		}
		return got.Messages
	}

	official := capture(Model{ID: "deepseek-reasoner", Provider: "deepseek"})
	if official[1].ReasoningContent != "r-old" || official[3].ReasoningContent != "r-now" {
		t.Fatalf("official DeepSeek must replay reasoning: %+v", official)
	}
	hosted := capture(Model{ID: "deepseek-ai/DeepSeek-R1", Provider: "siliconflow"})
	if hosted[1].ReasoningContent != "" || hosted[3].ReasoningContent != "" {
		t.Fatalf("third-party R1 must not receive reasoning_content: %+v", hosted)
	}
	current := capture(Model{ID: "deepseek-ai/DeepSeek-V3.2", Provider: "siliconflow",
		Compat: &OpenAICompletionsCompat{ReasoningReplay: ReasoningReplayCurrentTurn}})
	if current[1].ReasoningContent != "" || current[3].ReasoningContent != "r-now" {
		t.Fatalf("current_turn must keep only the in-flight turn: %+v", current)
	}
}

func feedAll(p *thinkStreamParser, chunks ...string) (reasoning, content string, live []string) {
	for _, c := range chunks {
		r, a := p.feed(c)
		reasoning += r
		content += a
		live = append(live, r+"|"+a)
	}
	r, a := p.flush()
	return reasoning + r, content + a, live
}

func TestThinkStreamParserPrefilledOpeningTag(t *testing.T) {
	reasoning, content, _ := feedAll(newThinkStreamParser(false), "step ", "one\n</th", "ink>\n\nFin", "al")
	if reasoning != "step one" || content != "Final" {
		t.Fatalf("reasoning=%q content=%q", reasoning, content)
	}
}

func TestThinkStreamParserPrefilledCompatStreamsReasoningLive(t *testing.T) {
	_, content, live := feedAll(newThinkStreamParser(true), "step ", "one", "</think>Final")
	if live[0] != "step |" || content != "Final" {
		t.Fatalf("reasoning must stream before </think>: %q, content=%q", live, content)
	}
}

func TestThinkStreamParserDoesNotHoldContentWithSeparateReasoning(t *testing.T) {
	p := newThinkStreamParser(false)
	p.sawReasoningField()
	if r, a := p.feed("Hello"); r != "" || a != "Hello" {
		t.Fatalf("content held although reasoning arrives separately: %q/%q", r, a)
	}
}

func TestThinkStreamParserReleasesUntaggedContentAfterHoldLimit(t *testing.T) {
	p := newThinkStreamParser(false)
	big := strings.Repeat("x", maxPrefilledHold+1)
	if _, a := p.feed(big); a != big {
		t.Fatalf("held %d bytes past the limit", len(big)-len(a))
	}
	if r, a := p.feed("</think>more"); r != "" || a != "</think>more" {
		t.Fatalf("after the limit content passes through unchanged: %q/%q", r, a)
	}
}

func TestThinkStreamParserPrefilledTagSplitIntoBytes(t *testing.T) {
	text := "reasoning goes here\n</think>\n\nanswer"
	chunks := make([]string, len(text))
	for i := range text {
		chunks[i] = text[i : i+1]
	}
	reasoning, content, _ := feedAll(newThinkStreamParser(false), chunks...)
	if reasoning != "reasoning goes here" || content != "answer" {
		t.Fatalf("reasoning=%q content=%q", reasoning, content)
	}
}

// BenchmarkThinkStreamParserPrefilled feeds ~16 KB of pre-filled reasoning
// in 4-byte deltas, the worst case for holding content until "</think>".
func BenchmarkThinkStreamParserPrefilled(b *testing.B) {
	deltas := make([]string, 0, 4100)
	for i := 0; i < 4096; i++ {
		deltas = append(deltas, "abc ")
	}
	deltas = append(deltas, "</think>", "answer")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p := newThinkStreamParser(false)
		for _, d := range deltas {
			p.feed(d)
		}
		p.flush()
	}
}

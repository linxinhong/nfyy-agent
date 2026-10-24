//go:build integration

// TASK-039 acceptance: through the real streaming handler, a dictionary
// term longer than the historical 80-byte window — split across two
// upstream SSE chunks — is redacted exactly as an in-chunk occurrence, and
// none of its bytes reach the client early.
package main_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/policy"
)

func TestSSEHoldbackDerivedForLongDictionaryTerm(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store

	term := strings.Repeat("玄铁", 60)     // 240-byte project-codename-style term
	head, tail := term[:108], term[108:] // rune-aligned: real SSE JSON cannot carry a dangling byte

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(delta string, done bool) {
			payload := map[string]any{"id": "s9", "object": "chat.completion.chunk", "model": "gate-fast",
				"choices": []map[string]any{{"index": 0, "delta": map[string]string{"content": delta}}}}
			if done {
				payload["choices"] = []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}
				payload["usage"] = map[string]int{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3}
			}
			b, _ := json.Marshal(payload)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			flusher.Flush()
		}
		chunk("代号是 "+head, false) // term head crosses the old window edge
		chunk(tail+"，别外传。", false)
		chunk("", true)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("SSE_GATE_KEY", "sse-key")
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO providers (name, base_url, credential_ref) VALUES ('sse-gate', $1, 'env:SSE_GATE_KEY')
		 ON CONFLICT (name) DO UPDATE SET base_url = EXCLUDED.base_url`, upstream.URL); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO model_aliases (alias) VALUES ('gate-fast') ON CONFLICT DO NOTHING`,
		`INSERT INTO model_routes (alias, provider, model, priority) VALUES ('gate-fast','sse-gate','gate-fast',100) ON CONFLICT DO NOTHING`,
		`INSERT INTO model_pricing (alias, provider, input_per_mt, output_per_mt) VALUES ('gate-fast','sse-gate',1.0,2.0) ON CONFLICT (alias, provider) DO NOTHING`,
	} {
		if _, err := env.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed control data: %v", err)
		}
	}
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO dlp_rules (name, pattern, action, replacement) VALUES
			('codename', 'PLACEHOLDER', 'redact', '[REDACTED:codename]')
		ON CONFLICT (name) DO UPDATE SET pattern = EXCLUDED.pattern, action = EXCLUDED.action`); err != nil {
		t.Fatalf("seed dictionary rule: %v", err)
	}
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO dlp_dictionary_terms (rule_name, term) VALUES ('codename', $1)
		 ON CONFLICT DO NOTHING`, term); err != nil {
		t.Fatalf("seed dictionary term: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(ctx, `DELETE FROM dlp_dictionary_terms WHERE rule_name='codename';
			DELETE FROM dlp_rules WHERE name='codename';`)
	})

	scanner := dlp.NewTableScanner(env.store.LoadDLPRules)
	scanner.CacheTTL = 0
	env.srv.DLP = scanner
	env.srv.Policy = &policy.TablePolicy{LoadRules: env.store.LoadPolicyRules}
	env.srv.LLMRouter = &llmkernel.Router{
		ResolvePlan:   env.store.RoutePlanFor,
		ResolveTarget: env.store.ResolveProviderTarget,
		Credential:    env.srv.Credential,
		Client:        &llmkernel.OpenAICompatClient{},
	}
	env.handler = env.srv.Handler()

	if dbgRules, dbgErr := scanner.AllRules(ctx); dbgErr == nil {
		for _, rr := range dbgRules {
			t.Logf("loaded rule %q pattern=%dB terms=%d", rr.Name, len(rr.Pattern), len(rr.Terms))
		}
	} else {
		t.Logf("rules load failed: %v", dbgErr)
	}

	mcpLogin(t, env, "holdback-user@example.com", "secops")
	keyValue := seedKey(t, env, mcpUserID(t, env, "holdback-user@example.com"))

	ts := httptest.NewServer(env.handler)
	t.Cleanup(ts.Close)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gate-fast","stream":true,"messages":[{"role":"user","content":"list"}]}`))
	req.Header.Set("Authorization", "Bearer "+keyValue)
	req.Header.Set("X-Agent-Workspace", "secops")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("stream status: %d %s", resp.StatusCode, string(body))
	}

	var assembled strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := line[len("data: "):]
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			assembled.WriteString(chunk.Choices[0].Delta.Content)
		}
	}

	out := assembled.String()
	if strings.Contains(out, head) || strings.Contains(out, "玄铁") {
		t.Fatalf("STREAM LEAK: cross-boundary term bytes reached the client: %q", out)
	}
	if !strings.Contains(out, "[REDACTED:codename]") {
		t.Fatalf("term must be redacted like an in-chunk occurrence: %q", out)
	}
	if !strings.Contains(out, "别外传") {
		t.Fatalf("surrounding content must still stream: %q", out)
	}
}

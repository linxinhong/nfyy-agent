//go:build integration

// TASK-030 coverage: a secret split across two SSE chunks is caught by the
// sliding window (deny aborts, redact rewrites); checksum-validated CN
// identity rules match well-formed numbers and reject permutations.

package main_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/policy"
)

func TestSSECrossChunkDLP(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store

	// Streaming upstream that splits the phone across two chunks.
	var hits atomic.Int64 // guarded via pointer-free use below
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(delta string, done bool) {
			payload := map[string]any{"id": "s1", "object": "chat.completion.chunk", "model": "gate-fast",
				"choices": []map[string]any{{"index": 0, "delta": map[string]string{"content": delta}}}}
			if done {
				payload["choices"] = []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}
				payload["usage"] = map[string]int{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3}
			}
			b, _ := json.Marshal(payload)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			flusher.Flush()
		}
		chunk("客户电话 138", false)
		chunk("12345009 说完了。", false)
		chunk("", true)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	// Streaming-capable client seam: reuse router with a streamer? The
	// gateway's router streams via its own client; simplest is a stub
	// provider whose /chat/completions returns SSE — but the kernel's
	// ChatStream expects the OpenAI wire format. Wire the streamer through
	// llmkernel by pointing the provider at the stub and using stream=true.
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO providers (name, base_url, credential_ref) VALUES ('sse-gate', $1, 'env:SSE_GATE_KEY')
		 ON CONFLICT (name) DO UPDATE SET base_url = EXCLUDED.base_url`, upstream.URL); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	t.Setenv("SSE_GATE_KEY", "sse-key")
	for _, stmt := range []string{
		`INSERT INTO model_aliases (alias) VALUES ('gate-fast') ON CONFLICT (alias) DO NOTHING`,
		`INSERT INTO model_routes (alias, provider, model, priority, weight) VALUES ('gate-fast','sse-gate','gate-fast',100,1) ON CONFLICT DO NOTHING`,
		`INSERT INTO model_pricing (alias, provider, input_per_mt, output_per_mt) VALUES ('gate-fast','sse-gate',1.0,2.0) ON CONFLICT (alias, provider) DO NOTHING`,
		`UPDATE dlp_rules SET action='redact' WHERE name='internal_phone'`,
	} {
		if _, err := env.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed control data: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(ctx, `DELETE FROM model_routes WHERE alias='gate-fast'; DELETE FROM model_pricing WHERE alias='gate-fast';
			DELETE FROM model_aliases WHERE alias='gate-fast'; DELETE FROM providers WHERE name='sse-gate';`)
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

	mcpLogin(t, env, "sse-user@example.com", "secops")
	keyValue := seedKey(t, env, mcpUserID(t, env, "sse-user@example.com"))

	// Stream request through the real handler.
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
		body := make([]byte, 4096)
		n, _ := resp.Body.Read(body)
		t.Fatalf("stream status: %d %s", resp.StatusCode, string(body[:n]))
	}

	var assembled strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
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
		for _, c := range chunk.Choices {
			assembled.WriteString(c.Delta.Content)
		}
	}
	out := assembled.String()
	t.Logf("upstream hits: %d assembled=%q", hits.Load(), out)
	if strings.Contains(out, "13812345009") {
		t.Fatalf("CROSS-CHUNK LEAK: assembled=%q", out)
	}
	if !strings.Contains(out, "[REDACTED:phone]") {
		t.Fatalf("expected redaction marker in assembled output: %q", out)
	}
	if !strings.Contains(out, "客户电话") || !strings.Contains(out, "说完了。") {
		t.Fatalf("benign text lost: %q", out)
	}
	_ = fmt.Sprint
}

func TestCNIdentityRules(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	scanner := dlp.NewTableScanner(env.store.LoadDLPRules)
	scanner.CacheTTL = 0
	st := env.store

	if _, err := env.pool.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatal(err)
	}
	scan := func(text string) (llmkernel.DLPResult, error) {
		return scanner.Scan(context.Background(), text)
	}

	// Valid specimens are redacted; permutations are not.
	res, err := scan("身份证 11010519491231002X 卡号 6222600260001072444 信用码 91350100M000100Y43")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"[REDACTED:resident_id]", "[REDACTED:bank_card]", "[REDACTED:uscc]"} {
		if !strings.Contains(res.Redacted, marker) {
			t.Fatalf("valid specimen not redacted (%s): %s", marker, res.Redacted)
		}
	}

	res, _ = scan("假身份证 110105194912310021 假卡 6222600260001072445")
	if strings.Contains(res.Redacted, "[REDACTED:resident_id]") || strings.Contains(res.Redacted, "[REDACTED:bank_card]") {
		t.Fatalf("checksum-invalid candidates must not match: %s", res.Redacted)
	}
	_ = st
}

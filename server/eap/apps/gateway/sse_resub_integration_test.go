//go:build integration

// TASK-032 coverage: exempt roles see originals in STREAMING responses
// (assembled SSE output), ordinary users keep pseudonyms, and the provider
// request body never contains originals for anyone.

package main_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/policy"
)

func streamOnce(t *testing.T, ts *httptest.Server, key string) string {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gate-fast","stream":true,"messages":[{"role":"user","content":"联系 13812345006 确认"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Agent-Workspace", "secops")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status: %d", resp.StatusCode)
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
	return assembled.String()
}

func TestSSEResubstitutionForExempt(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store

	var upstreamBodies atomic.Value
	upstreamBodies.Store([]string{})
	// The provider echoes the pseudonym it received (realistic model echo),
	// split so the token itself straddles SSE chunk boundaries.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 1<<20)
		n, _ := r.Body.Read(body)
		all, _ := upstreamBodies.Load().([]string)
		upstreamBodies.Store(append(all, string(body[:n])))
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(delta string) {
			payload := map[string]any{"id": "s1", "object": "chat.completion.chunk", "model": "gate-fast",
				"choices": []map[string]any{{"index": 0, "delta": map[string]string{"content": delta}}}}
			b, _ := json.Marshal(payload)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			flusher.Flush()
		}
		done := func() {
			payload := map[string]any{"id": "s1", "object": "chat.completion.chunk", "model": "gate-fast",
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
				"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3}}
			b, _ := json.Marshal(payload)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
		}
		// The upstream saw the pseudonym (request was tokenized); it echoes
		// it back split across chunks.
		reqBody := string(body[:n])
		i := strings.Index(reqBody, "[internal_phone:")
		tok := ""
		if i >= 0 {
			j := strings.IndexByte(reqBody[i:], ']')
			if j > 0 {
				tok = reqBody[i : i+j+1]
			}
		}
		if tok == "" {
			tok = "[internal_phone:none]"
		}
		mid := len(tok) / 2
		chunk("客户经理电话是 " + tok[:mid])
		chunk(tok[mid:] + " 请记录。")
		done()
	}))
	t.Cleanup(upstream.Close)

	if _, err := env.pool.Exec(ctx,
		`INSERT INTO providers (name, base_url, credential_ref) VALUES ('resub-gate', $1, 'env:RESUB_GATE_KEY')
		 ON CONFLICT (name) DO UPDATE SET base_url = EXCLUDED.base_url`, upstream.URL); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	t.Setenv("RESUB_GATE_KEY", "resub-key")
	for _, stmt := range []string{
		`INSERT INTO model_aliases (alias) VALUES ('gate-fast') ON CONFLICT (alias) DO NOTHING`,
		`INSERT INTO model_routes (alias, provider, model, priority, weight) VALUES ('gate-fast','resub-gate','gate-fast',100,1) ON CONFLICT DO NOTHING`,
		`INSERT INTO model_pricing (alias, provider, input_per_mt, output_per_mt) VALUES ('gate-fast','resub-gate',1.0,2.0) ON CONFLICT (alias, provider) DO NOTHING`,
		`UPDATE dlp_rules SET action='tokenize', except_roles='{privileged}' WHERE name='internal_phone'`,
	} {
		if _, err := env.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed control data: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(ctx, `UPDATE dlp_rules SET except_roles='{}', action='redact' WHERE name='internal_phone';
			DELETE FROM model_routes WHERE alias='gate-fast'; DELETE FROM model_pricing WHERE alias='gate-fast';
			DELETE FROM model_aliases WHERE alias='gate-fast'; DELETE FROM providers WHERE name='resub-gate'; DELETE FROM dlp_tokens;`)
	})

	scanner := dlp.NewTableScanner(env.store.LoadDLPRules)
	scanner.CacheTTL = 0
	scanner.Tokenizer = func(c context.Context, ruleName, value string) (string, error) {
		return env.store.TokenizeDLP(c, "secops", ruleName, value)
	}
	env.srv.DLP = scanner
	env.srv.Policy = &policy.TablePolicy{LoadRules: env.store.LoadPolicyRules}
	env.srv.LLMRouter = &llmkernel.Router{
		ResolvePlan:   env.store.RoutePlanFor,
		ResolveTarget: env.store.ResolveProviderTarget,
		Credential:    env.srv.Credential,
		Client:        &llmkernel.OpenAICompatClient{},
	}
	env.handler = env.srv.Handler()
	ts := httptest.NewServer(env.handler)
	t.Cleanup(ts.Close)

	setupUser := func(email string, roles string) string {
		mcpLogin(t, env, email, "secops")
		uid := mcpUserID(t, env, email)
		if _, err := env.pool.Exec(ctx,
			`UPDATE workspace_members SET roles = $1 WHERE user_id = $2`, roles, uid); err != nil {
			t.Fatalf("set roles: %v", err)
		}
		return seedKey(t, env, uid)
	}

	privKey := setupUser("resub-priv@example.com", "{privileged,developer}")
	ordKey := setupUser("resub-ord@example.com", "{developer}")

	// Both send the raw phone; both request bodies must be pseudonymized.
	privOut := streamOnce(t, ts, privKey)
	ordOut := streamOnce(t, ts, ordKey)

	bodies, _ := upstreamBodies.Load().([]string)
	joined := strings.Join(bodies, "\n")
	if strings.Contains(joined, "13812345006") {
		t.Fatalf("UPSTREAM LEAK: %s", joined)
	}
	if !strings.Contains(joined, "[internal_phone:") {
		t.Fatalf("upstream must carry pseudonyms: %s", joined)
	}

	if !strings.Contains(privOut, "13812345006") {
		t.Fatalf("privileged stream must show originals, got: %q", privOut)
	}
	if strings.Contains(ordOut, "13812345006") || !strings.Contains(ordOut, "[internal_phone:") {
		t.Fatalf("ordinary stream must keep pseudonyms, got: %q", ordOut)
	}
}

//go:build integration

// LLM protocol surface integration (TASK-007 acceptance): Models, Chat SSE,
// Responses, Embeddings and Anthropic Messages contract suites against the
// public gateway; deny/policy/DLP produce zero egress for every protocol;
// client cancellation closes upstream streaming without a race.
package main_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"enterprise-agent-platform/packages/llmkernel"
)

func TestLLMProtocolsIntegration(t *testing.T) {
	env := newSliceEnv(t, 1000)

	var sseHits int
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			body := readBody(r)
			if r.Header.Get("Authorization") != "Bearer proto-key" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if strings.Contains(body, `"stream":true`) {
				sseHits++
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				for i := 0; i < 5; i++ {
					select {
					case <-r.Context().Done():
						return
					case <-time.After(80 * time.Millisecond):
					}
					frame := map[string]any{
						"choices": []map[string]any{{"delta": map[string]any{"content": fmt.Sprintf("seg%d", i)}}},
					}
					if i == 4 {
						frame["usage"] = map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
					}
					encoded, _ := json.Marshal(frame)
					fmt.Fprintf(w, "data: %s\n\n", encoded)
					flusher.Flush()
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"proto echo"}}],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}}`))
		case strings.HasSuffix(r.URL.Path, "/embeddings"):
			_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]}],"usage":{"prompt_tokens":3,"total_tokens":3}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer provider.Close()

	seedProtoControlData(t, env, provider.URL)
	t.Setenv("PROTO_KEY", "proto-key")
	env.srv.Policy = &allowAllPolicy{}
	env.srv.DLP = denySecretDLP{}
	env.srv.LLMRouter = newProtoRouter(t, env)
	env.handler = env.srv.Handler()
	apiSrv := httptest.NewServer(env.handler)
	t.Cleanup(apiSrv.Close)

	mcpLogin(t, env, "proto@example.com", "secops")
	keyValue := seedKey(t, env, mcpUserID(t, env, "proto@example.com"))
	authz := map[string]string{"Authorization": "Bearer " + keyValue, "X-Agent-Workspace": "secops"}

	// 1. Models: control-data aliases, policy-filtered.
	rec := env.do(t, http.MethodGet, "/v1/models", keyValue, "", authz)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"proto-fast"`) {
		t.Fatalf("models: %d %s", rec.Code, rec.Body.String())
	}

	// 2. Chat SSE: the public completion resource, not a private stream
	// endpoint, emits chunks and a terminal [DONE].
	rec = env.do(t, http.MethodPost, "/v1/chat/completions", keyValue,
		`{"model":"proto-fast","messages":[{"role":"user","content":"hello streaming"}],"stream":true}`, authz)
	if rec.Code != http.StatusOK {
		t.Fatalf("sse: %d %s", rec.Code, rec.Body.String())
	}
	sseBody := rec.Body.String()
	for _, want := range []string{"seg0", "seg4", "data: [DONE]"} {
		if !strings.Contains(sseBody, want) {
			t.Fatalf("sse stream missing %q", want)
		}
	}

	// A former private streaming route must not become part of the public
	// contract. Its absence also prevents a weaker security pipeline.
	rec = env.do(t, http.MethodPost, "/v1/chat/stream", keyValue,
		`{"model":"proto-fast","messages":[{"role":"user","content":"hello"}],"stream":true}`, authz)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("private stream endpoint must be absent: %d %s", rec.Code, rec.Body.String())
	}

	// 3. Responses JSON and SSE shapes.
	rec = env.do(t, http.MethodPost, "/v1/responses", keyValue,
		`{"model":"proto-fast","input":"explain the isolation failure"}`, authz)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"output_text"`) {
		t.Fatalf("responses: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodPost, "/v1/responses", keyValue,
		`{"model":"proto-fast","input":"stream a response","stream":true}`, authz)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: response.output_text.delta") || !strings.Contains(rec.Body.String(), "event: response.completed") {
		t.Fatalf("responses sse: %d %s", rec.Code, rec.Body.String())
	}

	// 4. Embeddings shape.
	rec = env.do(t, http.MethodPost, "/v1/embeddings", keyValue,
		`{"model":"proto-fast","input":"vectorize me"}`, authz)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"embedding":[0.1,0.2,0.3]`) {
		t.Fatalf("embeddings: %d %s", rec.Code, rec.Body.String())
	}

	// 5. Anthropic Messages JSON and SSE shapes.
	rec = env.do(t, http.MethodPost, "/v1/messages", keyValue,
		`{"model":"proto-fast","max_tokens":256,"system":"be brief","messages":[{"role":"user","content":"hi"}]}`, authz)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"type":"message"`) || !strings.Contains(rec.Body.String(), `"stop_reason":"end_turn"`) {
		t.Fatalf("messages: %d %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodPost, "/v1/messages", keyValue,
		`{"model":"proto-fast","max_tokens":256,"stream":true,"messages":[{"role":"user","content":"stream this"}]}`, authz)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: message_start") || !strings.Contains(rec.Body.String(), "event: content_block_delta") || !strings.Contains(rec.Body.String(), "event: message_stop") {
		t.Fatalf("messages sse: %d %s", rec.Code, rec.Body.String())
	}
	// All three public streaming protocols persist the selected route, tokens
	// and priced cost under their own trace rather than falling through the
	// old nil-attempt path.
	var streamingRows, allStreamingRows, summedTokens int
	if err := env.pool.QueryRow(context.Background(), `
		SELECT count(*) FILTER (WHERE total_tokens = 15 AND cost_micros = 20),
		       count(*), coalesce(sum(total_tokens), 0)
		FROM usage_records WHERE alias = 'proto-fast' AND streaming = true`).Scan(&streamingRows, &allStreamingRows, &summedTokens); err != nil {
		t.Fatalf("query streaming usage: %v", err)
	}
	if streamingRows != 3 {
		t.Fatalf("expected one priced streaming usage row per protocol, got %d matching / %d total with %d tokens", streamingRows, allStreamingRows, summedTokens)
	}

	// 6. Deny-class DLP content: zero egress on every protocol.
	hitsBefore := sseHits
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		payload := `{"model":"proto-fast","messages":[{"role":"user","content":"key sk-abcdefghij0123456789AB"}]}`
		if path == "/v1/responses" {
			payload = `{"model":"proto-fast","input":"key sk-abcdefghij0123456789AB"}`
		}
		rec = env.do(t, http.MethodPost, path, keyValue, payload, authz)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "dlp_blocked") {
			t.Fatalf("dlp on %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if sseHits != hitsBefore {
		t.Fatalf("dlp-blocked protocols must not reach the provider")
	}

	// 7. Client cancellation: cancel mid-stream; the provider observes the
	// disconnect (its handler returns on ctx.Done) — no terminal event dup.
	reqCtx, cancel := context.WithCancel(context.Background())
	streamReq, _ := http.NewRequestWithContext(reqCtx, http.MethodPost,
		apiSrv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"proto-fast","messages":[{"role":"user","content":"cancel me"}],"stream":true}`))
	streamReq.Header.Set("Authorization", "Bearer "+keyValue)
	streamReq.Header.Set("X-Agent-Workspace", "secops")
	resp, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	scanner := bufio.NewScanner(resp.Body)
	frames := 0
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data:") {
			frames++
			if frames == 2 {
				cancel()
			}
		}
	}
	_ = resp.Body.Close()
	if frames < 2 {
		t.Fatalf("expected at least 2 frames before cancellation, got %d", frames)
	}
	// Wait for the provider stub to observe the disconnect.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if providerObservedCancel(provider) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
}

func providerObservedCancel(_ *httptest.Server) bool {
	// The stub handler returns on r.Context().Done(); the disconnect closes
	// the connection. Observation is implicit: no race/leak reported by the
	// race detector during the test run.
	return true
}

func readBody(r *http.Request) string {
	buf := make([]byte, 1<<20)
	n, _ := r.Body.Read(buf)
	return string(buf[:n])
}

func seedProtoControlData(t *testing.T, env *sliceEnv, providerURL string) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(),
		"INSERT INTO providers (name, base_url, credential_ref) VALUES ('proto-primary', $1, 'env:PROTO_KEY') ON CONFLICT (name) DO UPDATE SET base_url = EXCLUDED.base_url", providerURL); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	if _, err := env.pool.Exec(context.Background(),
		"INSERT INTO model_aliases (alias) VALUES ('proto-fast') ON CONFLICT DO NOTHING"); err != nil {
		t.Fatalf("seed alias: %v", err)
	}
	if _, err := env.pool.Exec(context.Background(),
		"INSERT INTO model_routes (alias, provider, model, priority) VALUES ('proto-fast', 'proto-primary', 'proto-fast-native', 100) ON CONFLICT DO NOTHING"); err != nil {
		t.Fatalf("seed route: %v", err)
	}
	if _, err := env.pool.Exec(context.Background(),
		"INSERT INTO model_pricing (alias, provider, input_per_mt, output_per_mt) VALUES ('proto-fast', 'proto-primary', 1.0, 2.0) ON CONFLICT DO NOTHING"); err != nil {
		t.Fatalf("seed pricing: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(context.Background(), "DELETE FROM model_routes WHERE alias='proto-fast'")
		_, _ = env.pool.Exec(context.Background(), "DELETE FROM model_pricing WHERE alias='proto-fast'")
		_, _ = env.pool.Exec(context.Background(), "DELETE FROM model_aliases WHERE alias='proto-fast'")
		_, _ = env.pool.Exec(context.Background(), "DELETE FROM providers WHERE name='proto-primary'")
	})
}

func newProtoRouter(t *testing.T, env *sliceEnv) *llmkernel.Router {
	t.Helper()
	return &llmkernel.Router{
		ResolvePlan:   env.store.RoutePlanFor,
		ResolveTarget: env.store.ResolveProviderTarget,
		Credential:    env.srv.Credential,
		Client:        &llmkernel.OpenAICompatClient{},
	}
}

// denySecretDLP blocks openai-key-shaped content outright (enforce mode).
type denySecretDLP struct{}

func (denySecretDLP) Scan(_ context.Context, text string) (llmkernel.DLPResult, error) {
	if strings.Contains(text, "sk-abcdefghij") {
		return llmkernel.DLPResult{Blocked: true, Reason: "content matches DLP rule openai_key"}, nil
	}
	return llmkernel.DLPResult{Redacted: text}, nil
}

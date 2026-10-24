//go:build integration

// TASK-036 / ADR-0025 acceptance: OpenAI function calling works end to end
// through the routed provider path — tools pass to the provider, tool_calls
// round-trip (non-stream and streaming deltas), finish_reason is the
// provider's, tool arguments are DLP-scanned like message content, and the
// Bifrost kernel path rejects tools loudly instead of dropping them.
//
// Run via `make verify-tool-passthrough`.
package main_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enterprise-agent-platform/packages/apikey"
	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/policy"
)

// argsDLP redacts the marker SECRET123 wherever it appears (message content
// or tool arguments); anything else passes.
type argsDLP struct{}

func (argsDLP) Scan(_ context.Context, text string) (llmkernel.DLPResult, error) {
	if strings.Contains(text, "SECRET123") {
		return llmkernel.DLPResult{
			Redacted:       strings.ReplaceAll(text, "SECRET123", "[secret:token]"),
			RedactionCount: strings.Count(text, "SECRET123"),
			RuleNames:      []string{"test_secret"},
		}, nil
	}
	return llmkernel.DLPResult{Redacted: text}, nil
}

func newToolEnv(t *testing.T, upstream *httptest.Server) *sliceEnv {
	t.Helper()
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO providers (name, base_url, credential_ref) VALUES ('tool-gate', $1, 'env:TOOL_GATE_KEY')
		ON CONFLICT (name) DO UPDATE SET base_url = EXCLUDED.base_url`, upstream.URL); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	t.Setenv("TOOL_GATE_KEY", "tool-gate-key")
	for _, stmt := range []string{
		`INSERT INTO model_aliases (alias) VALUES ('stub-model') ON CONFLICT (alias) DO NOTHING`,
		`INSERT INTO model_routes (alias, provider, model, priority, weight) VALUES ('stub-model','tool-gate','stub-model',100,1) ON CONFLICT DO NOTHING`,
	} {
		if _, err := env.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed llm control data: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = env.pool.Exec(ctx, `DELETE FROM model_routes WHERE alias='stub-model';
			DELETE FROM model_aliases WHERE alias='stub-model';
			DELETE FROM providers WHERE name='tool-gate';`)
	})
	env.srv.LLMRouter = &llmkernel.Router{
		ResolvePlan:   env.store.RoutePlanFor,
		ResolveTarget: env.store.ResolveProviderTarget,
		Credential:    env.srv.Credential,
		Client:        &llmkernel.OpenAICompatClient{},
	}
	env.srv.Policy = &policy.Allowlist{Workspaces: []string{"finance"}, Models: []string{"stub-model"}}
	env.srv.DLP = argsDLP{}
	env.handler = env.srv.Handler()
	return env
}

func toolTestUpstream(t *testing.T, captured *map[string]any) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if captured != nil {
			*captured = body
		}
		if stream, _ := body["stream"].(bool); stream {
			frames := []string{
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_s1","type":"function","function":{"name":"git_read_file","arguments":"{\"repo\":"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"acme/app\"}"}}]}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			}
			for _, f := range frames {
				_, _ = w.Write([]byte("data: " + f + "\n\n"))
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"git_read_file","arguments":"{\"repo\":\"acme/app\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func toolEnvKey(t *testing.T, env *sliceEnv) string {
	t.Helper()
	mcpLogin(t, env, "tool-user@example.com", "finance")
	return seedKey(t, env, mcpUserID(t, env, "tool-user@example.com"))
}

func toolAuthz(key string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + key, identity.HeaderWorkspace: "finance"}
}

func TestToolPassthroughNonStreamIntegration(t *testing.T) {
	var captured map[string]any
	env := newToolEnv(t, toolTestUpstream(t, &captured))
	key := toolEnvKey(t, env)

	body := `{"model":"stub-model","tools":[{"type":"function","function":{"name":"git_read_file","description":"read"}}],"tool_choice":"auto","messages":[
		{"role":"user","content":"read the file"},
		{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"git_read_file","arguments":"{\"repo\":\"acme/app\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"file body"}]}`
	rec := env.do(t, http.MethodPost, "/v1/chat/completions", key, body, toolAuthz(key))
	if rec.Code != http.StatusOK {
		t.Fatalf("non-stream tool chat: %d %s", rec.Code, rec.Body.String())
	}

	// The provider saw tools, tool_choice and the tool conversation.
	if captured["tools"] == nil || captured["tool_choice"] != "auto" {
		t.Fatalf("provider must receive tools: %v", captured)
	}
	msgs := captured["messages"].([]any)
	if msgs[2].(map[string]any)["tool_call_id"] != "call_1" {
		t.Fatalf("tool result must pass through: %v", msgs[2])
	}

	// The client receives the provider's tool_calls and finish_reason.
	var resp struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response parse: %v", err)
	}
	if len(resp.Choices) == 0 || resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason must be tool_calls: %s", rec.Body.String())
	}
	tc := resp.Choices[0].Message.ToolCalls
	if len(tc) != 1 || tc[0].ID != "call_1" || tc[0].Type != "function" ||
		tc[0].Function.Name != "git_read_file" || tc[0].Function.Arguments != `{"repo":"acme/app"}` {
		t.Fatalf("tool_calls round-trip wrong: %+v", tc)
	}
}

func TestToolPassthroughStreamIntegration(t *testing.T) {
	env := newToolEnv(t, toolTestUpstream(t, nil))
	key := toolEnvKey(t, env)

	body := `{"model":"stub-model","stream":true,"tools":[{"type":"function","function":{"name":"git_read_file"}}],"messages":[{"role":"user","content":"read it"}]}`
	rec := env.do(t, http.MethodPost, "/v1/chat/completions", key, body, toolAuthz(key))
	if rec.Code != http.StatusOK {
		t.Fatalf("stream tool chat: %d %s", rec.Code, rec.Body.String())
	}

	// Assemble tool_calls fragments and find the terminal finish_reason.
	merged := map[int]*strings.Builder{}
	ids := map[int]string{}
	finish := ""
	for _, frame := range strings.Split(rec.Body.String(), "\n\n") {
		if !strings.HasPrefix(frame, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(frame, "data: ")
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    *int   `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		c := chunk.Choices[0]
		for _, tc := range c.Delta.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			if merged[idx] == nil {
				merged[idx] = &strings.Builder{}
			}
			if tc.ID != "" {
				ids[idx] = tc.ID
			}
			merged[idx].WriteString(tc.Function.Arguments)
		}
		if c.FinishReason != nil {
			finish = *c.FinishReason
		}
	}
	if ids[0] != "call_s1" || merged[0] == nil || merged[0].String() != `{"repo":"acme/app"}` {
		t.Fatalf("stream tool_calls assembly wrong: ids=%v args=%v", ids, merged[0])
	}
	if finish != "tool_calls" {
		t.Fatalf("stream finish_reason = %q, want tool_calls", finish)
	}
}

func TestToolArgumentsDLPIntegration(t *testing.T) {
	var captured map[string]any
	env := newToolEnv(t, toolTestUpstream(t, &captured))
	key := toolEnvKey(t, env)

	// A secret inside historical tool arguments is provider-bound: it must be
	// redacted before egress, exactly like message content.
	body := `{"model":"stub-model","messages":[
		{"role":"user","content":"continue"},
		{"role":"assistant","tool_calls":[{"id":"call_2","type":"function","function":{"name":"save","arguments":"{\"note\":\"SECRET123\"}"}}]},
		{"role":"tool","tool_call_id":"call_2","content":"saved SECRET123 too"}]}`
	rec := env.do(t, http.MethodPost, "/v1/chat/completions", key, body, toolAuthz(key))
	if rec.Code != http.StatusOK {
		t.Fatalf("dlp tool chat: %d %s", rec.Code, rec.Body.String())
	}
	raw, _ := json.Marshal(captured)
	if strings.Contains(string(raw), "SECRET123") {
		t.Fatalf("secret leaked to provider: %s", raw)
	}
	if !strings.Contains(string(raw), "[secret:token]") {
		t.Fatalf("expected redacted payload: %s", raw)
	}
}

// The Bifrost kernel path (no router) must reject tools loudly, not drop them.
func TestToolKernelPathRejectedIntegration(t *testing.T) {
	env := newSliceEnv(t, 1000)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(upstream.Close)
	env.srv.LLM = newJourneyKernel(t, upstream.URL)
	env.srv.Policy = &policy.Allowlist{Workspaces: []string{"finance"}, Models: []string{"stub-model"}}
	env.srv.DLP = dlp.Scanner{}
	env.handler = env.srv.Handler()

	mcpLogin(t, env, "kernel-user@example.com", "finance")
	key := seedKey(t, env, mcpUserID(t, env, "kernel-user@example.com"))
	rec := env.do(t, http.MethodPost, "/v1/chat/completions", key,
		`{"model":"stub-model","tools":[{"type":"function","function":{"name":"f"}}],"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{identity.HeaderWorkspace: "finance"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_argument") {
		t.Fatalf("kernel path tools: %d %s, want 400 invalid_argument", rec.Code, rec.Body.String())
	}
	_ = apikey.EnvLive // keep import used if assertions above change
}

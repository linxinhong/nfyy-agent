//go:build integration

// TASK-042 acceptance: the /v1 surface speaks the OpenAI protocol envelope
// and forwards sampling parameters; assistant tool_calls serialize with
// content null; the /mcp surfaces honor Streamable-HTTP transport
// negotiation (Accept SSE, Mcp-Session-Id); /enterprise keeps the Problem
// shape. References: docs/04 §5, docs/protocol-conformance.md.
//
// Run via `make verify-protocol-conformance`.
package main_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"enterprise-agent-platform/packages/identity"
)

func TestProtocolErrorEnvelopeIntegration(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(upstream.Close)
	env := newToolEnv(t, upstream)

	mcpLogin(t, env, "conf@example.com", "finance")
	key := seedKey(t, env, mcpUserID(t, env, "conf@example.com"))

	// A workspace-authorized 403 must carry the OpenAI envelope with the
	// taxonomy type, the frozen EAP code, and an observable trace id.
	rec := env.do(t, http.MethodPost, "/v1/chat/completions", key,
		`{"model":"stub-model","messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unbound key without workspace: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Message string  `json:"message"`
			Type    string  `json:"type"`
			Code    string  `json:"code"`
			Param   *string `json:"param"`
			TraceID string  `json:"trace_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body must be the OpenAI envelope: %v (%s)", err, rec.Body.String())
	}
	if body.Error.Type != "permission_error" || body.Error.Code != "workspace_forbidden" || body.Error.TraceID == "" {
		t.Fatalf("error envelope wrong: %+v", body.Error)
	}

	// The enterprise surface keeps the frozen Problem shape.
	rec = env.do(t, http.MethodGet, "/enterprise/v1/me", "agk_live_bogus", "", nil)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"code":"unauthenticated"`) {
		t.Fatalf("enterprise problem shape changed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestProtocolSamplingParamsPassthroughIntegration(t *testing.T) {
	var captured map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	env := newToolEnv(t, upstream)
	key := toolEnvKey(t, env)

	rec := env.do(t, http.MethodPost, "/v1/chat/completions", key, `{
		"model":"stub-model",
		"temperature":0.3,"top_p":0.9,"max_tokens":128,"stop":["END"],"parallel_tool_calls":false,
		"stream_options":{"include_usage":true},
		"messages":[{"role":"user","content":"hi"}]}`, toolAuthz(key))
	if rec.Code != http.StatusOK {
		t.Fatalf("sampling chat: %d %s", rec.Code, rec.Body.String())
	}
	wants := map[string]any{
		"temperature": 0.3, "top_p": 0.9, "max_tokens": float64(128),
		"stop": []any{"END"}, "parallel_tool_calls": false,
	}
	for field, want := range wants {
		if got := captured[field]; !reflect.DeepEqual(got, want) {
			t.Fatalf("provider payload %s = %v, want %v", field, got, want)
		}
	}
	if captured["stream_options"].(map[string]any)["include_usage"] != true {
		t.Fatalf("stream_options must pass through: %v", captured["stream_options"])
	}
}

// Assistant tool_calls responses serialize content as null (strict clients).
func TestProtocolToolCallsContentNullIntegration(t *testing.T) {
	env := newToolEnv(t, toolTestUpstream(t, nil))
	key := toolEnvKey(t, env)

	rec := env.do(t, http.MethodPost, "/v1/chat/completions", key,
		`{"model":"stub-model","messages":[{"role":"user","content":"read it"}]}`, toolAuthz(key))
	if rec.Code != http.StatusOK {
		t.Fatalf("tool chat: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":null`) {
		t.Fatalf("assistant tool_calls message must serialize content as null: %s", rec.Body.String())
	}
}

// TASK-051 / ADR-0028 decision 1: ungoverned request fields reach the
// provider payload byte-faithfully while governed behavior (DLP scan of
// message content) is unchanged.
func TestProtocolOpaqueFieldsPassthroughIntegration(t *testing.T) {
	var rawPayload []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawPayload, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	env := newToolEnv(t, upstream)
	key := toolEnvKey(t, env)

	// Values are written without added whitespace so byte-equality against
	// the captured provider payload is meaningful.
	rec := env.do(t, http.MethodPost, "/v1/chat/completions", key, `{
		"model":"stub-model",
		"messages":[{"role":"user","content":"note SECRET123 please"}],
		"presence_penalty":0.7,
		"frequency_penalty":0.2,
		"response_format":{"type":"json_object"},
		"seed":42,
		"user":"usr_123",
		"metadata":{"team":"finance"},
		"n":2,
		"x-custom":{"k":[1,2,3]}}`, toolAuthz(key))
	if rec.Code != http.StatusOK {
		t.Fatalf("opaque chat: %d %s", rec.Code, rec.Body.String())
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rawPayload, &payload); err != nil {
		t.Fatalf("provider payload not JSON: %v", err)
	}
	wants := map[string]string{
		"presence_penalty":  `0.7`,
		"frequency_penalty": `0.2`,
		"response_format":   `{"type":"json_object"}`,
		"seed":              `42`,
		"user":              `"usr_123"`,
		"metadata":          `{"team":"finance"}`,
		"n":                 `2`,
		"x-custom":          `{"k":[1,2,3]}`,
	}
	for field, want := range wants {
		if got, ok := payload[field]; !ok {
			t.Fatalf("provider payload missing opaque field %s (payload: %s)", field, rawPayload)
		} else if strings.TrimSpace(string(got)) != want {
			t.Fatalf("opaque field %s = %s, want byte-identical %s", field, got, want)
		}
	}

	// Governance is unchanged: the DLP scanner still rewrote the message
	// content before it reached the provider, and opaque fields never carry
	// redaction semantics.
	if !strings.Contains(string(rawPayload), "[secret:token]") || strings.Contains(string(rawPayload), "SECRET123") {
		t.Fatalf("DLP must still redact governed message content: %s", rawPayload)
	}
}

// The /mcp surfaces honor Streamable-HTTP transport negotiation.
func TestProtocolMCPTransportIntegration(t *testing.T) {
	env := newToolEnv(t, toolTestUpstream(t, nil))
	key := toolEnvKey(t, env)

	// Accept: text/event-stream selects the SSE response form.
	rec := env.do(t, http.MethodPost, "/mcp/agent", key,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		map[string]string{"Accept": "application/json, text/event-stream", identity.HeaderWorkspace: "finance"})
	if rec.Code != http.StatusOK {
		t.Fatalf("sse tools/list: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q, want text/event-stream", ct)
	}
	if !strings.HasPrefix(rec.Body.String(), "event: message\ndata: ") {
		t.Fatalf("sse frame shape wrong: %q", rec.Body.String()[:60])
	}

	// Default JSON acceptance keeps the plain JSON form.
	rec = env.do(t, http.MethodPost, "/mcp/agent", key,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		map[string]string{"Accept": "application/json", identity.HeaderWorkspace: "finance"})
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("json content type = %q", ct)
	}

	// initialize issues a stateless session handle.
	rec = env.do(t, http.MethodPost, "/mcp/agent", key,
		`{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		map[string]string{identity.HeaderWorkspace: "finance"})
	if rec.Code != http.StatusOK || rec.Header().Get("Mcp-Session-Id") == "" {
		t.Fatalf("initialize must issue Mcp-Session-Id: %d %s", rec.Code, rec.Header().Get("Mcp-Session-Id"))
	}
}

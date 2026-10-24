package llmkernel_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/token"
)

// sessionFor builds the session shape a verified token would carry.
func sessionFor(workspace string) identity.Session {
	s := identity.Session{Principal: identity.Principal{UserID: "user_1"}}
	if workspace != "" {
		s.Workspace = identity.WorkspaceContext{WorkspaceID: workspace, Source: identity.WorkspaceSourceMembership}
	}
	return s
}

// providerStub is an OpenAI-compatible provider stand-in that counts egress.
type providerStub struct {
	mu       sync.Mutex
	hits     int
	auths    []string
	bodies   []string
	status   int // when non-zero, fail with this status instead of serving
	server   *httptest.Server
	streamWG sync.WaitGroup
}

func newProviderStub(t *testing.T) *providerStub {
	t.Helper()
	s := &providerStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.hits++
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		s.bodies = append(s.bodies, string(body))
		status := s.status
		s.mu.Unlock()

		if strings.Contains(string(body), `"broken-model"`) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"model exploded","type":"stub_error"}}`))
			return
		}
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"stub failure","type":"stub_error"}}`))
			return
		}

		var reqBody struct {
			Stream bool `json:"stream"`
		}
		isStream := json.Unmarshal(body, &reqBody) == nil && reqBody.Stream
		if isStream {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			// TASK-053: emit an immediate SSE comment so a slow provider does
			// not look like a non-SSE JSON body to upstream SSE prefix sniffing.
			fmt.Fprint(w, ": stream-open\n\n")
			flusher.Flush()
			for i := 0; i < 5; i++ {
				select {
				case <-r.Context().Done():
					return // client cancelled mid-stream
				case <-time.After(100 * time.Millisecond):
				}
				chunk := map[string]any{
					"id": "chatcmpl-stub", "object": "chat.completion.chunk", "model": "stub-model",
					"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": fmt.Sprintf("chunk%d", i)}}},
				}
				b, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", b)
				flusher.Flush()
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-stub", "object": "chat.completion", "model": "stub-model",
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "stub says hi"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18},
		})
	})
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

func (s *providerStub) hitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

func (s *providerStub) setFailStatus(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = code
}

// --- fixed engines used by the gate tests ---

type allowPolicy struct{}

func (allowPolicy) Decide(context.Context, llmkernel.PolicyInput) (llmkernel.Decision, error) {
	return llmkernel.Decision{Allowed: true}, nil
}

type denyPolicy struct{ err error }

func (d denyPolicy) Decide(context.Context, llmkernel.PolicyInput) (llmkernel.Decision, error) {
	if d.err != nil {
		return llmkernel.Decision{}, d.err
	}
	return llmkernel.Decision{Allowed: false, Reason: "test denial"}, nil
}

type cleanDLP struct{}

func (cleanDLP) Scan(_ context.Context, text string) (llmkernel.DLPResult, error) {
	return llmkernel.DLPResult{Redacted: text}, nil
}

type blockingDLP struct{ err error }

func (b blockingDLP) Scan(context.Context, string) (llmkernel.DLPResult, error) {
	if b.err != nil {
		return llmkernel.DLPResult{}, b.err
	}
	return llmkernel.DLPResult{Blocked: true, Reason: "credential pattern detected"}, nil
}

type usageRecorder struct {
	mu      sync.Mutex
	records []llmkernel.UsageRecord
}

func (u *usageRecorder) Record(_ context.Context, r llmkernel.UsageRecord) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.records = append(u.records, r)
}

// --- helpers ---

func newKernel(t *testing.T, stub *providerStub) *llmkernel.Kernel {
	t.Helper()
	k, err := llmkernel.New(context.Background(), llmkernel.Config{
		ProviderBaseURL: stub.server.URL,
		ProviderKey:     "sk-provider-secret",
		Model:           "stub-model",
	})
	if err != nil {
		t.Fatalf("init kernel: %v", err)
	}
	return k
}

// tokenFixture mints and verifies claims through the real token contract so
// the kernel receives exactly what a gateway would hand it.
type tokenFixture struct {
	signer   token.Signer
	verifier token.Verifier
}

func newTokenFixture(t *testing.T) *tokenFixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := token.NewEd25519Signer("k1", "https://agent.company.com/", priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	verifier, err := token.NewEd25519Verifier(token.VerificationKey{KeyID: "k1", Public: pub})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	return &tokenFixture{signer: signer, verifier: verifier}
}

func (f *tokenFixture) claims(t *testing.T, audience token.Audience, workspace string) *token.Claims {
	t.Helper()
	raw, err := f.signer.Sign(token.IssueInput{Session: sessionFor(workspace), Audience: audience, TTL: token.DefaultTTL})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	claims, err := f.verifier.Verify(raw, audience)
	if err != nil {
		t.Fatalf("verify own token: %v", err)
	}
	return claims
}

// --- acceptance security proofs ---

// Acceptance: invalid audience, missing Workspace and DLP failure cannot reach
// the provider stub. Every denial is generated before egress.
func chatRequest() llmkernel.ChatRequest {
	return llmkernel.ChatRequest{Messages: []llmkernel.ChatMessage{{Role: "user", Content: "hello"}}}
}

func assertDenied(t *testing.T, err error, wantErrsCode string, hitsBefore int, stub *providerStub) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected denial, got success")
	}
	problem, ok := errs.From(err)
	if !ok || string(problem.Code) != wantErrsCode {
		t.Fatalf("got %v, want code %s", err, wantErrsCode)
	}
	if stub.hitCount() != hitsBefore {
		t.Fatalf("denied request must not reach the provider: hits %d -> %d", hitsBefore, stub.hitCount())
	}
}

func TestSecurityGatesBlockProviderEgress(t *testing.T) {
	stub := newProviderStub(t)
	k := newKernel(t, stub)
	tf := newTokenFixture(t)

	t.Run("invalid audience never reaches provider", func(t *testing.T) {
		claims := tf.claims(t, token.AudienceMCPGateway, "secops")
		_, err := k.GenerateChat(context.Background(), claims, allowPolicy{}, cleanDLP{}, nil, chatRequest())
		assertDenied(t, err, "permission_denied", 0, stub)
	})

	t.Run("missing workspace never reaches provider", func(t *testing.T) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		signer, err := token.NewEd25519Signer("k1", "https://agent.company.com/", priv)
		if err != nil {
			t.Fatalf("signer: %v", err)
		}
		sess := sessionFor("")
		if _, err := signer.Sign(token.IssueInput{Session: sess, Audience: token.AudienceLLMGateway}); err == nil {
			t.Fatalf("capability token without workspace must be refused at issuance")
		}
		if stub.hitCount() != 0 {
			t.Fatalf("no egress expected")
		}
	})

	t.Run("DLP block never reaches provider", func(t *testing.T) {
		claims := tf.claims(t, token.AudienceLLMGateway, "secops")
		_, err := k.GenerateChat(context.Background(), claims, allowPolicy{}, blockingDLP{}, nil, chatRequest())
		assertDenied(t, err, "dlp_blocked", 0, stub)
	})

	t.Run("DLP engine failure fails closed", func(t *testing.T) {
		claims := tf.claims(t, token.AudienceLLMGateway, "secops")
		_, err := k.GenerateChat(context.Background(), claims, allowPolicy{}, blockingDLP{err: fmt.Errorf("scanner down")}, nil, chatRequest())
		assertDenied(t, err, "dlp_blocked", 0, stub)
	})

	t.Run("policy denial never reaches provider", func(t *testing.T) {
		claims := tf.claims(t, token.AudienceLLMGateway, "secops")
		_, err := k.GenerateChat(context.Background(), claims, denyPolicy{}, cleanDLP{}, nil, chatRequest())
		assertDenied(t, err, "policy_denied", 0, stub)
	})

	t.Run("policy engine failure fails closed", func(t *testing.T) {
		claims := tf.claims(t, token.AudienceLLMGateway, "secops")
		_, err := k.GenerateChat(context.Background(), claims, denyPolicy{err: fmt.Errorf("policy store down")}, cleanDLP{}, nil, chatRequest())
		assertDenied(t, err, "policy_denied", 0, stub)
	})

	t.Run("nil claims rejected", func(t *testing.T) {
		_, err := k.GenerateChat(context.Background(), nil, allowPolicy{}, cleanDLP{}, nil, chatRequest())
		assertDenied(t, err, "unauthenticated", 0, stub)
	})
}

// Happy path: exactly one provider egress, provider credential used (never an
// employee key), usage recorded through the hook.
func TestGenerateChatHappyPath(t *testing.T) {
	stub := newProviderStub(t)
	k := newKernel(t, stub)
	tf := newTokenFixture(t)
	recorder := &usageRecorder{}
	claims := tf.claims(t, token.AudienceLLMGateway, "secops")

	result, err := k.GenerateChat(context.Background(), claims, allowPolicy{}, cleanDLP{}, recorder, chatRequest())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if result.Content != "stub says hi" {
		t.Fatalf("unexpected content: %q", result.Content)
	}
	if result.Usage.TotalTokens != 18 {
		t.Fatalf("usage not normalized: %+v", result.Usage)
	}
	if stub.hitCount() != 1 {
		t.Fatalf("expected exactly one provider call, got %d", stub.hitCount())
	}
	stub.mu.Lock()
	auth := strings.Join(stub.auths, ",")
	body := strings.Join(stub.bodies, ",")
	stub.mu.Unlock()
	if auth != "Bearer sk-provider-secret" {
		t.Fatalf("provider credential must be the server-side key, got %q", auth)
	}
	if strings.Contains(body, "agk_") || strings.Contains(body, "eyJhbGci") {
		t.Fatalf("employee credentials must never reach the provider payload: %s", body)
	}
	if len(recorder.records) != 1 || recorder.records[0].WorkspaceID != "secops" || recorder.records[0].TotalTokens != 18 {
		t.Fatalf("usage hook not invoked correctly: %+v", recorder.records)
	}
}

// Error normalization: provider statuses map onto the frozen EAP codes.
func TestErrorNormalization(t *testing.T) {
	tf := newTokenFixture(t)
	cases := []struct {
		stubStatus int
		wantCode   string
	}{
		{http.StatusBadRequest, "invalid_argument"},
		{http.StatusTooManyRequests, "rate_limited"},
		{http.StatusInternalServerError, "upstream_unavailable"},
		{http.StatusUnauthorized, "upstream_unavailable"}, // provider auth is our infra problem
	}
	for _, tc := range cases {
		stub := newProviderStub(t)
		stub.setFailStatus(tc.stubStatus)
		k := newKernel(t, stub)
		claims := tf.claims(t, token.AudienceLLMGateway, "secops")
		_, err := k.GenerateChat(context.Background(), claims, allowPolicy{}, cleanDLP{}, nil, chatRequest())
		problem, ok := errs.From(err)
		if !ok || string(problem.Code) != tc.wantCode {
			t.Fatalf("status %d: got %v, want %s", tc.stubStatus, err, tc.wantCode)
		}
	}
}

// Retry/fallback: a failing primary model falls back within the same call.
// Retry/fallback: a failing primary model falls back to the backup model
// within the same kernel call — the provider failure never reaches the client.
func TestFallbackOnProviderError(t *testing.T) {
	stub := newProviderStub(t)
	k, err := llmkernel.New(context.Background(), llmkernel.Config{
		ProviderBaseURL: stub.server.URL,
		ProviderKey:     "sk-provider-secret",
		Model:           "stub-model",
		Models:          []string{"broken-model", "stub-model"},
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	tf := newTokenFixture(t)
	claims := tf.claims(t, token.AudienceLLMGateway, "secops")
	req := llmkernel.ChatRequest{
		Model:      "broken-model",
		FallbackTo: "stub-model",
		Messages:   []llmkernel.ChatMessage{{Role: "user", Content: "hello"}},
	}
	result, err := k.GenerateChat(context.Background(), claims, allowPolicy{}, cleanDLP{}, nil, req)
	if err != nil {
		t.Fatalf("fallback must absorb the primary failure, got: %v", err)
	}
	if result.Content != "stub says hi" {
		t.Fatalf("fallback response wrong: %+v", result)
	}
	if stub.hitCount() < 2 {
		t.Fatalf("expected primary attempt plus fallback, got %d hits", stub.hitCount())
	}
}

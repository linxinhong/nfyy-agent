package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/mcp"
)

// fakeMCPServer is a minimal MCP-over-HTTP endpoint for transport tests.
type fakeMCPServer struct {
	mu          sync.Mutex
	requests    []string // JSON-RPC methods received
	authHeaders []string
	sessionID   string
	handler     func(w http.ResponseWriter, r *http.Request, req map[string]any)
}

func (s *fakeMCPServer) serve() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		s.mu.Lock()
		method, _ := req["method"].(string)
		s.requests = append(s.requests, method)
		s.authHeaders = append(s.authHeaders, r.Header.Get("Authorization"))
		s.mu.Unlock()

		if s.handler != nil {
			s.handler(w, r, req)
			return
		}
		w.Header().Set("MCP-Session-Id", s.sessionID)
		switch method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req["id"],
				"result": map[string]any{
					"protocolVersion": mcp.ProtocolVersion,
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "fake", "version": "1.0"},
				},
			})
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req["id"],
				"result": map[string]any{"tools": []any{map[string]any{"name": "t", "inputSchema": map[string]any{}}}},
			})
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{}})
		}
	}))
}

type countingCredentialProvider struct {
	calls atomic.Int64
	value string
}

func (p *countingCredentialProvider) Credentials(ctx context.Context) (string, error) {
	p.calls.Add(1)
	return p.value, nil
}

func TestHTTPTransportDynamicCredentialsPerCall(t *testing.T) {
	srv := &fakeMCPServer{}
	ts := srv.serve()
	defer ts.Close()

	provider := &countingCredentialProvider{value: "Bearer tok-1"}
	transport, err := NewHTTPClientTransportWithOptions(ts.URL, HTTPTransportOptions{
		CredentialProvider: provider,
	})
	if err != nil {
		t.Fatalf("NewHTTPClientTransportWithOptions: %v", err)
	}
	client := NewClient(transport)
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatalf("list tools: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	// initialize + initialized notification + tools/list = 3 requests, each
	// carrying fresh credentials.
	if len(srv.authHeaders) != 3 {
		t.Fatalf("requests = %d, want 3 with credentials", len(srv.authHeaders))
	}
	for i, h := range srv.authHeaders {
		if h != "Bearer tok-1" {
			t.Fatalf("request %d Authorization = %q", i, h)
		}
	}
	if provider.calls.Load() != 3 {
		t.Fatalf("credential provider calls = %d, want 3 (per request)", provider.calls.Load())
	}
}

func TestHTTPTransportHeaderProviderAndReservedRejection(t *testing.T) {
	ts := (&fakeMCPServer{}).serve()
	defer ts.Close()

	transport, err := NewHTTPClientTransportWithOptions(ts.URL, HTTPTransportOptions{
		HeaderProvider: headerMapProvider(map[string]string{"X-Custom": "yes", "Host": "evil.example"}),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := transport.Call(context.Background(), mcp.Request{JSONRPC: "2.0", ID: "1", Method: "ping"}); err == nil ||
		!strings.Contains(err.Error(), "must not override") {
		t.Fatalf("err = %v, want reserved-header rejection", err)
	}
}

type headerMapProvider map[string]string

func (p headerMapProvider) Headers(ctx context.Context) (map[string]string, error) {
	return p, nil
}

func TestHTTPTransportSessionIDReuseAndTerminate(t *testing.T) {
	srv := &fakeMCPServer{sessionID: "sess-abc"}
	ts := srv.serve()
	defer ts.Close()

	transport, err := NewHTTPClientTransportWithOptions(ts.URL, HTTPTransportOptions{})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	client := NewClient(transport)
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	// Second call must carry the session ID.
	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatalf("list: %v", err)
	}
	// Terminate sends DELETE with the session ID.
	if err := transport.Terminate(context.Background()); err != nil {
		t.Fatalf("terminate: %v", err)
	}

	// Verify the fake server saw the session header on requests after init.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.requests) < 2 {
		t.Fatalf("requests = %d", len(srv.requests))
	}
}

func TestHTTPTransportNotifyNoIDAnd202Empty(t *testing.T) {
	srv := &fakeMCPServer{}
	srv.handler = func(w http.ResponseWriter, r *http.Request, req map[string]any) {
		// Notifications get 202 with empty body.
		if _, hasID := req["id"]; !hasID {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{}})
	}
	ts := srv.serve()
	defer ts.Close()

	transport, err := NewHTTPClientTransportWithOptions(ts.URL, HTTPTransportOptions{})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := transport.Notify(context.Background(), mcp.Notification{JSONRPC: "2.0", Method: "notifications/initialized"}); err != nil {
		t.Fatalf("notify: %v", err)
	}
}

func TestHTTPTransportSSEMatchingAndMalformed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "matching message", body: "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"req-1\",\"result\":{}}\n\n"},
		{name: "non-data event ignored", body: "event: ping\ndata: {}\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"req-1\",\"result\":{}}\n\n"},
		{name: "malformed json", body: "data: not-json\n\n", wantErr: true},
		{name: "wrong id", body: "data: {\"jsonrpc\":\"2.0\",\"id\":\"other\",\"result\":{}}\n\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &fakeMCPServer{}
			srv.handler = func(w http.ResponseWriter, r *http.Request, req map[string]any) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(tc.body))
			}
			ts := srv.serve()
			defer ts.Close()

			transport, err := NewHTTPClientTransportWithOptions(ts.URL, HTTPTransportOptions{})
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			_, err = transport.Call(context.Background(), mcp.Request{JSONRPC: "2.0", ID: "req-1", Method: "ping"})
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestHTTPTransportTypedErrors(t *testing.T) {
	for _, status := range []int{401, 403, 409, 429, 503} {
		srv := &fakeMCPServer{}
		srv.handler = func(w http.ResponseWriter, r *http.Request, req map[string]any) {
			w.Header().Set("Retry-After", "5")
			w.Header().Set("X-Request-Id", "req-xyz")
			http.Error(w, "{\"error\":\"boom\"}", status)
		}
		ts := srv.serve()
		defer ts.Close()

		transport, err := NewHTTPClientTransportWithOptions(ts.URL, HTTPTransportOptions{})
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		_, err = transport.Call(context.Background(), mcp.Request{JSONRPC: "2.0", ID: "1", Method: "ping"})
		httpErr, ok := err.(*HTTPError)
		if !ok {
			t.Fatalf("status %d: err = %v, want *HTTPError", status, err)
		}
		if httpErr.StatusCode != status {
			t.Fatalf("status code = %d, want %d", httpErr.StatusCode, status)
		}
		if httpErr.RetryAfter != "5" || httpErr.RequestID != "req-xyz" {
			t.Fatalf("metadata = %+v", httpErr)
		}
	}
}

func TestHTTPTransportCrossOriginRedirectDropsCredentials(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{}}`))
	}))
	defer target.Close()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer ts.Close()

	transport, err := NewHTTPClientTransportWithOptions(ts.URL, HTTPTransportOptions{
		CredentialProvider: &countingCredentialProvider{value: "Bearer secret"},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	_, err = transport.Call(context.Background(), mcp.Request{JSONRPC: "2.0", ID: "1", Method: "ping"})
	if err == nil {
		t.Fatalf("expected cross-origin redirect rejection")
	}
	if leaked.Load() {
		t.Fatalf("credentials leaked across origin redirect")
	}
}

func TestHTTPTransportResponseSizeLimit(t *testing.T) {
	big := strings.Repeat("x", 5<<20) // 5 MiB > default 4 MiB
	srv := &fakeMCPServer{}
	srv.handler = func(w http.ResponseWriter, r *http.Request, req map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":` + big + `}`))
	}
	ts := srv.serve()
	defer ts.Close()

	transport, err := NewHTTPClientTransportWithOptions(ts.URL, HTTPTransportOptions{})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	_, err = transport.Call(context.Background(), mcp.Request{JSONRPC: "2.0", ID: "1", Method: "ping"})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want size-limit error", err)
	}
}

func TestHTTPTransportCancelReleases(t *testing.T) {
	release := make(chan struct{})
	srv := &fakeMCPServer{}
	srv.handler = func(w http.ResponseWriter, r *http.Request, req map[string]any) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{}}`))
	}
	ts := srv.serve()
	defer ts.Close()

	transport, err := NewHTTPClientTransportWithOptions(ts.URL, HTTPTransportOptions{})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = transport.Call(ctx, mcp.Request{JSONRPC: "2.0", ID: "1", Method: "ping"})
	if err == nil {
		t.Fatalf("expected context error")
	}
	close(release)
	// Transport must remain usable after a canceled call.
	if _, err := transport.Call(context.Background(), mcp.Request{JSONRPC: "2.0", ID: "2", Method: "ping"}); err != nil {
		t.Fatalf("call after cancel: %v", err)
	}
}

func TestHTTPTransportRejectsUserinfo(t *testing.T) {
	if _, err := NewHTTPClientTransportWithOptions("https://user:pass@example.com/mcp", HTTPTransportOptions{}); err == nil {
		t.Fatalf("expected userinfo rejection")
	}
}

func TestHTTPTransportConcurrentCalls(t *testing.T) {
	srv := &fakeMCPServer{}
	ts := srv.serve()
	defer ts.Close()

	transport, err := NewHTTPClientTransportWithOptions(ts.URL, HTTPTransportOptions{})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, err := transport.Call(context.Background(), mcp.Request{JSONRPC: "2.0", ID: fmt.Sprintf("%d", n), Method: "ping"})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent call: %v", err)
		}
	}
}

func TestHTTPTransportTLSHandshakeWithInjectedClient(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{}}`))
	}))
	defer ts.Close()

	// Reuse the test server's trusted client (its transport trusts the
	// server's self-signed CA), injected through HTTPTransportOptions.
	trusted := ts.Client()
	transport, err := NewHTTPClientTransportWithOptions(ts.URL, HTTPTransportOptions{
		HTTPClient: trusted,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := transport.Call(context.Background(), mcp.Request{JSONRPC: "2.0", ID: "1", Method: "ping"}); err != nil {
		t.Fatalf("TLS call: %v", err)
	}
}

// TestProtocolVersionOverride: hosts can pin a newer protocolVersion (e.g.
// governed gateway contract 2025-11-25); default stays mcp.ProtocolVersion.
func TestProtocolVersionOverride(t *testing.T) {
	var gotHeader, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"method":"initialize"`) {
			gotHeader = r.Header.Get("MCP-Protocol-Version")
			gotBody = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"t","version":"0"}}}`)
	}))
	defer srv.Close()
	tr, err := NewHTTPClientTransportWithOptions(srv.URL, HTTPTransportOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(tr)
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotHeader != "2025-11-25" || !strings.Contains(gotBody, `"protocolVersion":"2025-11-25"`) {
		t.Fatalf("override not applied: header=%q body=%s", gotHeader, gotBody)
	}
	// Default: no override.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"t","version":"0"}}}`)
	}))
	defer srv2.Close()
	tr2, _ := NewHTTPClientTransportWithOptions(srv2.URL, HTTPTransportOptions{})
	c2 := NewClient(tr2)
	if err := c2.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
}

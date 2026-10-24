package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// flippyServer is a streamable-HTTP MCP server that "restarts" on demand:
// after flip, requests carrying the old session id get 404 (session unknown),
// a fresh initialize gets a new session and succeeds. This reproduces the
// E2E-04 deviation #1 topology (gateway restart behind the same endpoint).
type flippyServer struct {
	mu       sync.Mutex
	session  string
	initCnt  atomic.Int64
	flipped  atomic.Bool
	oldCalls atomic.Int64
}

func (f *flippyServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sid := r.Header.Get("Mcp-Session-Id")
		if f.flipped.Load() && sid != "" {
			f.mu.Lock()
			known := sid == f.session
			f.mu.Unlock()
			if !known {
				f.oldCalls.Add(1)
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, "session not found")
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("x") == "" && r.Header.Get("Accept") == "" {
			// keep defaults sane
		}
		// initialize has no session header
		if sid == "" {
			f.initCnt.Add(1)
			f.mu.Lock()
			f.session = fmt.Sprintf("sess-%d", f.initCnt.Load())
			f.mu.Unlock()
			w.Header().Set("Mcp-Session-Id", f.session)
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"flip","version":"0"}}}`)
			return
		}
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"ok\":true}"}]}}`)
	}
}

func TestSessionRecoveryAfterServerRestart(t *testing.T) {
	f := &flippyServer{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	tr, err := NewHTTPClientTransportWithOptions(srv.URL, HTTPTransportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(tr)
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CallTool(context.Background(), "tool", nil); err != nil {
		t.Fatal(err)
	}

	// Server "restarts": forgets the current session (old ids now 404).
	f.mu.Lock()
	f.session = "forgotten"
	f.mu.Unlock()
	f.flipped.Store(true)
	out, err := c.CallTool(context.Background(), "tool", nil)
	if err != nil {
		t.Fatalf("CallTool must transparently recover after gateway restart: %v", err)
	}
	if s, _ := out.(string); s == "" {
		t.Fatalf("unexpected result %#v", out)
	}
	if got := f.initCnt.Load(); got != 2 {
		t.Fatalf("expected exactly one re-initialize, got %d", got)
	}
	// Subsequent calls keep working on the new session.
	if _, err := c.CallTool(context.Background(), "tool", nil); err != nil {
		t.Fatalf("post-recovery call failed: %v", err)
	}
	if got := f.oldCalls.Load(); got < 1 {
		t.Fatal("test setup error: expected at least one 404")
	}
}

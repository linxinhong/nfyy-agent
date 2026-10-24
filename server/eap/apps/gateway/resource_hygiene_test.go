package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"enterprise-agent-platform/packages/llmkernel"
)

// TASK-040: concurrent provider calls must share ONE transport with tuned
// idle-connection limits, instead of each call riding DefaultTransport
// (MaxIdleConnsPerHost=2 → TCP+TLS re-handshake churn). Each fresh TCP
// connection shows up as a distinct client ephemeral port in RemoteAddr.
func TestProviderTransportSharedAndTuned(t *testing.T) {
	var conns atomic.Int64
	connsSeen := sync.Map{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, loaded := connsSeen.LoadOrStore(r.RemoteAddr, struct{}{}); !loaded {
			conns.Add(1)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer backend.Close()

	client := llmkernel.NewProviderHTTPClient()
	if client.Transport == http.DefaultTransport {
		t.Fatal("provider client must not ride DefaultTransport")
	}

	target := llmkernel.ProviderTarget{Name: "t", BaseURL: backend.URL}
	burst := func() {
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c := &llmkernel.OpenAICompatClient{Client: client}
				if _, err := c.Chat(context.Background(), target, "", llmkernel.ChatRequest{
					Model: "m", Messages: []llmkernel.ChatMessage{{Role: "user", Content: "hi"}},
				}); err != nil {
					t.Errorf("chat: %v", err)
				}
			}()
		}
		wg.Wait()
	}

	before := conns.Load()
	burst()
	// The first burst legitimately opens up to 16 connections (one per
	// in-flight request). The tuned pool keeps them idle afterwards.
	if idle := conns.Load() - before; idle == 0 {
		t.Fatalf("first burst opened no connections: %d", idle)
	}
	// The second burst must reuse the idle pool, not re-handshake. With the
	// old DefaultTransport wiring, MaxIdleConnsPerHost=2 would discard 14 of
	// the 16 connections and force fresh TCP setups here — on every attempt,
	// however long it waits. Connections return to the pool asynchronously,
	// so a loaded machine needs longer to settle: the settle time grows per
	// attempt instead of being a single fixed sleep (a fixed 50ms flaked
	// under parallel `go test ./...`).
	var fresh int64
	for _, settle := range []time.Duration{50 * time.Millisecond, 250 * time.Millisecond, time.Second} {
		time.Sleep(settle)
		before = conns.Load()
		burst()
		if fresh = conns.Load() - before; fresh <= 2 {
			return
		}
	}
	t.Fatalf("second burst opened %d fresh connections; idle pool must absorb the burst", fresh)
}

package providers

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

// Concurrent calls to one provider reuse pooled connections instead of
// dialing per request (Go's default keeps two idle connections per host).
func TestConcurrentCallsReuseConnections(t *testing.T) {
	var dials atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(5 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			dials.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	chat, err := Model{ID: "m", API: APIOpenAICompletions, BaseURL: srv.URL}.Chat(ChatOptions{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	const workers, waves = 32, 3
	for wave := 0; wave < waves; wave++ {
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := chat.Response(context.Background(), []models.Message{{Role: models.RoleUser, Content: "hi"}}, models.ModelOptions{}); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
	}
	if got := dials.Load(); got > workers+8 {
		t.Fatalf("%d connections for %d requests in %d waves; later waves must reuse the pool", got, workers*waves, waves)
	}
}

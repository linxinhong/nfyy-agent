package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/linxinhong/xingding-agent-runtime/internal/metrictest"
)

func serveAll(t *testing.T, h *StreamHub, runID string, owner StreamOwner, after uint64) (string, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	err := h.Serve(rec, req, runID, owner, after)
	return rec.Body.String(), err
}

func TestStreamHubReplaysAfterCursorAndIsolatesOwners(t *testing.T) {
	h := NewStreamHub(StreamHubConfig{})
	alice := StreamOwner{Subject: "alice", Isolated: true}
	pub := h.Start("r1", alice, func() {})
	pub.Publish("message", map[string]string{"content": "a"})
	pub.Publish("message", map[string]string{"content": "b"})
	pub.Publish("run_completed", nil)
	pub.Close()

	body, err := serveAll(t, h, "r1", alice, 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, `"a"`) || !strings.Contains(body, "id: 2\nevent: message\ndata: {\"content\":\"b\"}") || !strings.Contains(body, "id: 3\nevent: run_completed") {
		t.Fatalf("body = %q", body)
	}
	if _, err := serveAll(t, h, "r1", StreamOwner{Subject: "mallory", Isolated: true}, 0); !errors.Is(err, ErrStreamNotFound) {
		t.Fatalf("other owner err = %v", err)
	}
}

func TestStreamHubReportsTruncatedReplay(t *testing.T) {
	h := NewStreamHub(StreamHubConfig{MaxFramesPerRun: 2})
	pub := h.Start("r", StreamOwner{}, func() {})
	for i := 0; i < 5; i++ {
		pub.Publish("message", i)
	}
	pub.Close()
	body, _ := serveAll(t, h, "r", StreamOwner{}, 1)
	if !strings.Contains(body, "event: replay_truncated\ndata: {\"first_available\":4}") || !strings.Contains(body, "id: 5") {
		t.Fatalf("body = %q", body)
	}
}

func TestStreamHubContinuesSequenceForResumedRun(t *testing.T) {
	h := NewStreamHub(StreamHubConfig{})
	pub := h.Start("r", StreamOwner{}, func() {})
	pub.Publish("run_paused", nil)
	pub.Close()
	pub = h.Start("r", StreamOwner{}, func() {})
	pub.Publish("run_resumed", nil)
	pub.Close()
	body, _ := serveAll(t, h, "r", StreamOwner{}, 1)
	if !strings.Contains(body, "id: 2\nevent: run_resumed") {
		t.Fatalf("body = %q", body)
	}
}

func TestStreamHubReconnectStopsGraceCancel(t *testing.T) {
	h := NewStreamHub(StreamHubConfig{DisconnectGrace: 30 * time.Millisecond})
	var cancelled atomic.Bool
	pub := h.Start("r", StreamOwner{}, func() { cancelled.Store(true) })

	// First subscriber connects and leaves: the grace timer starts.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx, leave := context.WithCancel(req.Context())
	leave()
	_ = h.Serve(httptest.NewRecorder(), req.WithContext(ctx), "r", StreamOwner{}, 0)

	// A second subscriber attaches within the grace window and stays.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.Serve(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), "r", StreamOwner{}, 0)
	}()
	time.Sleep(80 * time.Millisecond)
	if cancelled.Load() {
		t.Fatal("run cancelled although a subscriber reattached")
	}
	pub.Close()
	<-done
}

func TestStreamMetrics(t *testing.T) {
	metrictest.Install()
	v := func(name string, kv ...attribute.KeyValue) float64 { return metrictest.Value(t, name, kv...) }
	beforeActive, beforeFrames := v("stream.active"), v("stream.frames")
	beforeReconnects := v("stream.reconnects", attribute.String("source", "local"))
	h := NewStreamHub(StreamHubConfig{})
	pub := h.Start("metric-run", StreamOwner{}, func() {})
	if got := v("stream.active") - beforeActive; got != 1 {
		t.Fatalf("active during run = %v", got)
	}
	pub.Publish("message", "a")
	pub.Publish("message", "b")
	pub.Close()
	if _, err := serveAll(t, h, "metric-run", StreamOwner{}, 1); err != nil {
		t.Fatal(err)
	}
	if v("stream.active") != beforeActive || v("stream.frames")-beforeFrames != 2 ||
		v("stream.reconnects", attribute.String("source", "local"))-beforeReconnects != 1 {
		t.Fatalf("active %v frames %v reconnects %v", v("stream.active")-beforeActive, v("stream.frames")-beforeFrames,
			v("stream.reconnects", attribute.String("source", "local"))-beforeReconnects)
	}
}

func TestStreamHubEvictsOldestFinishedPastByteBudget(t *testing.T) {
	h := NewStreamHub(StreamHubConfig{MaxBytes: 2500})
	payload := strings.Repeat("x", 1000)
	for _, id := range []string{"old", "mid", "new"} {
		pub := h.Start(id, StreamOwner{}, func() {})
		pub.Publish("message", payload)
		pub.Close()
		time.Sleep(time.Millisecond) // distinct finish times
	}
	live := h.Start("live", StreamOwner{}, func() {})
	live.Publish("message", payload)
	if _, err := serveAll(t, h, "old", StreamOwner{}, 0); !errors.Is(err, ErrStreamNotFound) {
		t.Fatalf("oldest journal kept past the byte budget: %v", err)
	}
	if _, err := serveAll(t, h, "new", StreamOwner{}, 0); err != nil {
		t.Fatalf("newest finished journal evicted: %v", err)
	}
	if got := h.bytes.Load(); got > 2500+1100 {
		t.Fatalf("retained %d bytes", got)
	}
	live.Close()
}

func TestStreamHubDeleteExpiredReleasesLocalJournals(t *testing.T) {
	h := NewStreamHub(StreamHubConfig{Retention: time.Millisecond})
	pub := h.Start("r", StreamOwner{}, func() {})
	pub.Publish("message", "hello")
	pub.Close()
	time.Sleep(5 * time.Millisecond)
	if _, err := h.DeleteExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := serveAll(t, h, "r", StreamOwner{}, 0); !errors.Is(err, ErrStreamNotFound) {
		t.Fatalf("expired journal still served: %v", err)
	}
	if got := h.bytes.Load(); got != 0 {
		t.Fatalf("bytes = %d after eviction", got)
	}
}

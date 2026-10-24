package os

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/os/service"
	"github.com/linxinhong/xingding-agent-runtime/pkg/stream"
)

// pausingStreamModel streams "part1", waits for release, then "part2".
type pausingStreamModel struct {
	release   chan struct{}
	cancelled chan struct{}
}

func (m *pausingStreamModel) Response(context.Context, []models.Message, models.ModelOptions) (*models.ModelResponse, error) {
	return nil, errors.New("stream only")
}
func (m *pausingStreamModel) ResponseStream(ctx context.Context, _ []models.Message, _ models.ModelOptions) (<-chan models.StreamEvent, error) {
	ch := make(chan models.StreamEvent)
	go func() {
		defer close(ch)
		ch <- models.StreamEvent{Event: "content_delta", Content: "part1"}
		select {
		case <-m.release:
		case <-ctx.Done():
			close(m.cancelled)
			ch <- models.StreamEvent{Event: "error", Err: ctx.Err()}
			return
		}
		ch <- models.StreamEvent{Event: "content_delta", Content: "part2"}
		ch <- models.StreamEvent{Event: "finish", FinishReason: models.FinishReasonStop}
	}()
	return ch, nil
}
func (*pausingStreamModel) ID() string       { return "pausing" }
func (*pausingStreamModel) Provider() string { return "test" }

type sseFrame struct{ id, event, data string }

// readFrames reads SSE frames until stop returns true or the body ends.
func readFrames(t *testing.T, r *bufio.Reader, stop func(sseFrame) bool) []sseFrame {
	t.Helper()
	var frames []sseFrame
	var cur sseFrame
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return frames
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			if cur.event != "" {
				frames = append(frames, cur)
				if stop != nil && stop(cur) {
					return frames
				}
			}
			cur = sseFrame{}
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func newStreamingOS(t *testing.T, m models.Model, cfg service.StreamHubConfig) *httptest.Server {
	t.Helper()
	ag, err := agent.New(agent.AgentConfig{ID: "a", Model: m})
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(AgentOSConfig{Agents: map[string]*agent.Agent{"a": ag}, Streams: cfg})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestStreamSurvivesDisconnectAndResumesFromLastEventID(t *testing.T) {
	m := &pausingStreamModel{release: make(chan struct{}), cancelled: make(chan struct{})}
	srv := newStreamingOS(t, m, service.StreamHubConfig{DisconnectGrace: 5 * time.Second})

	ctx, disconnect := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/agents/a/runs?stream=true", strings.NewReader(`{"message":"q"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	runID := resp.Header.Get("X-Run-ID")
	first := readFrames(t, bufio.NewReader(resp.Body), func(f sseFrame) bool { return f.event == "message" })
	disconnect()
	resp.Body.Close()
	if runID == "" || len(first) == 0 || first[0].event != "run_started" || !strings.Contains(first[0].data, runID) {
		t.Fatalf("run id %q, first frames %+v", runID, first)
	}
	lastID := first[len(first)-1].id

	time.Sleep(50 * time.Millisecond) // the run must keep going without a client
	close(m.release)

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/runs/"+runID+"/events", nil)
	req.Header.Set("Last-Event-ID", lastID)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rest := readFrames(t, bufio.NewReader(resp.Body), nil)
	var messages []string
	for _, f := range rest {
		if f.event == "message" {
			messages = append(messages, f.data)
		}
	}
	if len(messages) != 1 || !strings.Contains(messages[0], "part2") || len(rest) == 0 || rest[len(rest)-1].event != "run_completed" {
		t.Fatalf("resumed frames = %+v", rest)
	}
	select {
	case <-m.cancelled:
		t.Fatal("run was cancelled although the client reconnected within the grace window")
	default:
	}
}

func TestAbandonedStreamIsCancelledAfterGrace(t *testing.T) {
	m := &pausingStreamModel{release: make(chan struct{}), cancelled: make(chan struct{})}
	srv := newStreamingOS(t, m, service.StreamHubConfig{DisconnectGrace: 20 * time.Millisecond})
	ctx, disconnect := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/agents/a/runs?stream=true", strings.NewReader(`{"message":"q"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	readFrames(t, bufio.NewReader(resp.Body), func(f sseFrame) bool { return f.event == "message" })
	disconnect()
	resp.Body.Close()
	select {
	case <-m.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("abandoned run was not cancelled")
	}
}

func TestStreamEventsUnknownRunIsNotFound(t *testing.T) {
	srv := newStreamingOS(t, &pausingStreamModel{}, service.StreamHubConfig{})
	resp, err := http.Get(srv.URL + "/runs/nope/events")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestStreamReconnectsToAnotherReplicaThroughSharedJournal(t *testing.T) {
	journal := stream.NewMemoryJournal()
	m := &pausingStreamModel{release: make(chan struct{}), cancelled: make(chan struct{})}
	cfg := service.StreamHubConfig{Journal: journal, DisconnectGrace: 100 * time.Millisecond,
		FlushInterval: 10 * time.Millisecond, PollInterval: 10 * time.Millisecond}
	replicaA := newStreamingOS(t, m, cfg)
	replicaB := newStreamingOS(t, &pausingStreamModel{}, cfg) // never runs this stream

	ctx, disconnect := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, replicaA.URL+"/agents/a/runs?stream=true", strings.NewReader(`{"message":"q"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	runID := resp.Header.Get("X-Run-ID")
	first := readFrames(t, bufio.NewReader(resp.Body), func(f sseFrame) bool { return f.event == "message" })
	disconnect()
	resp.Body.Close()
	lastID := first[len(first)-1].id
	time.Sleep(30 * time.Millisecond) // let replica A flush

	req, _ = http.NewRequest(http.MethodGet, replicaB.URL+"/runs/"+runID+"/events", nil)
	req.Header.Set("Last-Event-ID", lastID)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replica B status = %d", resp.StatusCode)
	}
	// Outlast replica A's grace while attached to replica B only.
	time.Sleep(300 * time.Millisecond)
	select {
	case <-m.cancelled:
		t.Fatal("replica A cancelled the run although the client reattached on replica B")
	default:
	}
	close(m.release)
	rest := readFrames(t, bufio.NewReader(resp.Body), nil)
	var messages []string
	for _, f := range rest {
		if f.event == "message" {
			messages = append(messages, f.data)
		}
	}
	if len(messages) != 1 || !strings.Contains(messages[0], "part2") || rest[len(rest)-1].event != "run_completed" {
		t.Fatalf("frames from replica B = %+v", rest)
	}
}

func TestSharedJournalIsOwnerScoped(t *testing.T) {
	journal := stream.NewMemoryJournal()
	if err := journal.Append(context.Background(), "r", stream.Owner{Subject: "alice", Isolated: true}, []stream.Frame{{Seq: 1, Event: "message"}}, true); err != nil {
		t.Fatal(err)
	}
	hub := service.NewStreamHub(service.StreamHubConfig{Journal: journal})
	rec := httptest.NewRecorder()
	err := hub.Serve(rec, httptest.NewRequest(http.MethodGet, "/", nil), "r", service.StreamOwner{Subject: "mallory", Isolated: true}, 0)
	if !errors.Is(err, service.ErrStreamNotFound) || rec.Body.Len() != 0 {
		t.Fatalf("err = %v body = %q", err, rec.Body.String())
	}
}

func TestResumedRunContinuesSequenceOnAnotherReplica(t *testing.T) {
	journal := stream.NewMemoryJournal()
	a := service.NewStreamHub(service.StreamHubConfig{Journal: journal})
	pub := a.Start("r", service.StreamOwner{}, func() {})
	pub.Publish("run_paused", nil)
	pub.Close()
	b := service.NewStreamHub(service.StreamHubConfig{Journal: journal})
	pub = b.Start("r", service.StreamOwner{}, func() {})
	pub.Publish("run_resumed", nil)
	pub.Close()
	frames, done, err := journal.Read(context.Background(), "r", service.StreamOwner{}, 0, 0)
	if err != nil || !done || len(frames) != 2 || frames[1].Seq != 2 || frames[1].Event != "run_resumed" {
		t.Fatalf("frames = %+v done = %v err = %v", frames, done, err)
	}
}

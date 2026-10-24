package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/linxinhong/xingding-agent-runtime/pkg/os/routers/authz"
	"github.com/linxinhong/xingding-agent-runtime/pkg/stream"
	toolservice "github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

// ErrStreamNotFound reports that no journal holds the run: it never
// streamed, belongs to someone else, or its retention window passed.
var ErrStreamNotFound = errors.New("stream not found")

// StreamHubConfig bounds the stream journal. Zero fields select defaults.
type StreamHubConfig struct {
	// DisconnectGrace is how long a run keeps executing after its last
	// subscriber disconnected, waiting for a reconnect. Default 30s;
	// negative cancels the run as soon as the last subscriber leaves.
	DisconnectGrace time.Duration
	// Retention keeps a finished run's journal for late reconnects.
	// Default 5 minutes.
	Retention time.Duration
	// MaxFramesPerRun and MaxBytesPerRun bound one run's in-memory
	// journal; older frames are dropped first. Defaults 10000 frames and
	// 8 MiB.
	MaxFramesPerRun int
	MaxBytesPerRun  int
	// MaxRuns bounds the number of in-memory journals and MaxBytes their
	// combined size; the oldest finished journals are evicted first.
	// Defaults 10000 and 256 MiB. Journals of running runs are never
	// evicted, so MaxBytes is a target rather than a hard cap.
	MaxRuns  int
	MaxBytes int
	// Journal, when set, also stores frames where every replica can read
	// them, so a client may reconnect to any replica. Frames are written in
	// batches every FlushInterval (default 100ms); readers on other
	// replicas poll every PollInterval (default 200ms).
	Journal       stream.Journal
	FlushInterval time.Duration
	PollInterval  time.Duration
}

// Frame is one journaled server-sent event.
type Frame = stream.Frame

// StreamOwner identifies who may attach to a run's stream.
type StreamOwner = stream.Owner

// OwnerFromRequest derives the stream owner from verified request identity
// only; request bodies never contribute.
func OwnerFromRequest(req *http.Request) StreamOwner {
	owner := StreamOwner{}
	owner.Subject, owner.Isolated = authz.UserID(req)
	if principal, err := toolservice.PrincipalFromContext(req.Context()); err == nil {
		owner.Subject, owner.Isolated = principal.Subject, true
	}
	owner.Namespace, owner.Tenant = authz.Namespace(req)
	return owner
}

// StreamHub journals SSE frames per run so a client can reconnect with
// Last-Event-ID and resume where it left off. Runs are decoupled from the
// HTTP request: when the last subscriber disconnects the run keeps going for
// DisconnectGrace and is cancelled only if nobody reattaches, on this or
// (with a shared Journal) any other replica. Without a Journal, reconnects
// need sticky routing by run ID.
type StreamHub struct {
	cfg StreamHubConfig

	// mu guards only the runs map; each run has its own lock so
	// concurrent streams never contend. Lock order: hub.mu, then run.mu.
	mu   sync.Mutex
	runs map[string]*hubRun
	// lastSweep throttles the full eviction scan; guarded by mu.
	lastSweep time.Time
	// bytes is the combined size of every journal in runs.
	bytes atomic.Int64
}

type hubRun struct {
	hub    *StreamHub
	id     string
	owner  StreamOwner
	cancel context.CancelFunc

	mu sync.Mutex
	// guarded by mu
	frames      []Frame
	bytes       int
	nextSeq     uint64
	done        bool
	finishedAt  time.Time
	subscribers int
	// evicted is set once the run left the hub's map and its bytes the
	// hub's total.
	evicted    bool
	graceTimer *time.Timer
	notify     chan struct{}
	// unflushed holds frames not yet written to the shared journal.
	unflushed []Frame
	flushDone chan struct{}
	// flushMu serializes journal writes so batches land in order and the
	// final done marker is written last.
	flushMu sync.Mutex
}

// NewStreamHub creates a hub.
func NewStreamHub(cfg StreamHubConfig) *StreamHub {
	if cfg.DisconnectGrace == 0 {
		cfg.DisconnectGrace = 30 * time.Second
	}
	if cfg.Retention <= 0 {
		cfg.Retention = 5 * time.Minute
	}
	if cfg.MaxFramesPerRun <= 0 {
		cfg.MaxFramesPerRun = 10000
	}
	if cfg.MaxBytesPerRun <= 0 {
		cfg.MaxBytesPerRun = 8 << 20
	}
	if cfg.MaxRuns <= 0 {
		cfg.MaxRuns = 10000
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 256 << 20
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 100 * time.Millisecond
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 200 * time.Millisecond
	}
	return &StreamHub{cfg: cfg, runs: make(map[string]*hubRun)}
}

// Publisher appends frames to one run's journal.
type Publisher struct{ run *hubRun }

// Start registers a run. cancel stops the run when no subscriber returns
// within the disconnect grace. An existing journal for runID (e.g. the
// original stream of a resumed run, here or in the shared Journal) is
// continued so sequence numbers keep increasing.
func (h *StreamHub) Start(runID string, owner StreamOwner, cancel context.CancelFunc) *Publisher {
	var sharedSeq uint64
	if h.cfg.Journal != nil {
		ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
		sharedSeq, _ = h.cfg.Journal.LastSeq(ctx, runID)
		done()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.maybeEvictLocked(time.Now())
	run := h.runs[runID]
	if run == nil || run.owner != owner {
		if run != nil {
			h.dropLocked(run)
		}
		run = &hubRun{hub: h, id: runID, owner: owner, nextSeq: 1}
		h.runs[runID] = run
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if sharedSeq >= run.nextSeq {
		run.nextSeq = sharedSeq + 1
	}
	run.cancel = cancel
	if run.done || run.notify == nil {
		streamsActive.Add(context.Background(), 1)
	}
	run.done = false
	run.notify = make(chan struct{})
	if h.cfg.Journal != nil {
		// A run restarted before its previous publisher closed keeps one
		// flush loop.
		if run.flushDone != nil {
			close(run.flushDone)
		}
		run.flushDone = make(chan struct{})
		go h.flushLoop(run, run.flushDone)
	}
	return &Publisher{run: run}
}

// Publish appends one frame and wakes subscribers.
func (p *Publisher) Publish(event string, data any) {
	raw := json.RawMessage("null")
	if data != nil {
		if b, err := json.Marshal(data); err == nil {
			raw = b
		}
	}
	run := p.run
	h := run.hub
	run.mu.Lock()
	defer run.mu.Unlock()
	frame := Frame{Seq: run.nextSeq, Event: event, Data: raw}
	run.frames = append(run.frames, frame)
	if h.cfg.Journal != nil {
		run.unflushed = append(run.unflushed, frame)
	}
	run.nextSeq++
	framesPublished.Add(context.Background(), 1)
	before := run.bytes
	run.bytes += len(raw) + len(event)
	for len(run.frames) > 1 && (len(run.frames) > h.cfg.MaxFramesPerRun || run.bytes > h.cfg.MaxBytesPerRun) {
		run.bytes -= len(run.frames[0].Data) + len(run.frames[0].Event)
		run.frames[0] = Frame{}
		run.frames = run.frames[1:]
	}
	if !run.evicted {
		h.bytes.Add(int64(run.bytes - before))
	}
	run.wakeLocked()
}

// Close marks the run finished; subscribers drain and disconnect. With a
// shared Journal the remaining frames are written before Close returns.
func (p *Publisher) Close() {
	run := p.run
	h := run.hub
	run.mu.Lock()
	if !run.done {
		streamsActive.Add(context.Background(), -1)
	}
	run.done = true
	run.finishedAt = time.Now()
	if run.graceTimer != nil {
		run.graceTimer.Stop()
		run.graceTimer = nil
	}
	flushDone := run.flushDone
	run.flushDone = nil
	run.wakeLocked()
	run.mu.Unlock()
	if flushDone != nil {
		close(flushDone)
		h.flush(run, true)
	}
}

// flushLoop writes batches to the shared journal until stop closes.
func (h *StreamHub) flushLoop(run *hubRun, stop chan struct{}) {
	ticker := time.NewTicker(h.cfg.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			h.flush(run, false)
		}
	}
}

// flush writes unflushed frames. Failures keep the frames for the next
// attempt; the local journal still serves this replica's subscribers.
func (h *StreamHub) flush(run *hubRun, done bool) {
	run.flushMu.Lock()
	defer run.flushMu.Unlock()
	run.mu.Lock()
	batch := run.unflushed
	run.unflushed = nil
	run.mu.Unlock()
	if len(batch) == 0 && !done {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.cfg.Journal.Append(ctx, run.id, run.owner, batch, done); err != nil {
		journalWriteErrors.Add(context.Background(), 1)
		run.mu.Lock()
		run.unflushed = append(batch, run.unflushed...)
		run.mu.Unlock()
	}
}

func (r *hubRun) wakeLocked() {
	close(r.notify)
	r.notify = make(chan struct{})
}

// sweepInterval throttles the full eviction scan done on Start.
const sweepInterval = time.Second

// maybeEvictLocked runs evictLocked at most once per sweepInterval unless a
// limit is exceeded, keeping Start cheap with many retained journals.
func (h *StreamHub) maybeEvictLocked(now time.Time) {
	if now.Sub(h.lastSweep) < sweepInterval && h.withinLimitsLocked() {
		return
	}
	h.evictLocked(now)
}

// evictLocked drops journals past retention, then the oldest finished ones
// while above MaxRuns or MaxBytes.
func (h *StreamHub) evictLocked(now time.Time) {
	h.lastSweep = now
	for _, run := range h.runs {
		if idle, finishedAt := run.idle(); idle && now.Sub(finishedAt) > h.cfg.Retention {
			h.dropLocked(run)
		}
	}
	if h.withinLimitsLocked() {
		return
	}
	type idleRun struct {
		run        *hubRun
		finishedAt time.Time
	}
	var candidates []idleRun
	for _, run := range h.runs {
		if idle, finishedAt := run.idle(); idle {
			candidates = append(candidates, idleRun{run, finishedAt})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].finishedAt.Before(candidates[j].finishedAt) })
	for _, c := range candidates {
		if h.withinLimitsLocked() {
			return
		}
		h.dropLocked(c.run)
	}
}

func (h *StreamHub) withinLimitsLocked() bool {
	return len(h.runs) < h.cfg.MaxRuns && h.bytes.Load() <= int64(h.cfg.MaxBytes)
}

// idle reports whether the run finished with nobody attached, and when.
func (r *hubRun) idle() (bool, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done && r.subscribers == 0, r.finishedAt
}

// dropLocked removes run from the map and its bytes from the total.
func (h *StreamHub) dropLocked(run *hubRun) {
	if h.runs[run.id] == run {
		delete(h.runs, run.id)
	}
	run.mu.Lock()
	if !run.evicted {
		run.evicted = true
		h.bytes.Add(-int64(run.bytes))
	}
	run.mu.Unlock()
}

// LastEventID parses the resume cursor from the Last-Event-ID header or the
// last_event_id query parameter (EventSource cannot set headers on first
// connect).
func LastEventID(req *http.Request) uint64 {
	value := req.Header.Get("Last-Event-ID")
	if value == "" {
		value = req.URL.Query().Get("last_event_id")
	}
	seq, _ := strconv.ParseUint(value, 10, 64)
	return seq
}

func writeStreamHeaders(w http.ResponseWriter, runID string) (http.Flusher, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Run-ID", runID)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return flusher, true
}

func writeFrame(w http.ResponseWriter, frame Frame) {
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", frame.Seq, frame.Event, frame.Data)
}

// Serve streams frames after afterSeq to w until the run finishes or the
// client disconnects. Runs published on this replica are served from
// memory; others from the shared Journal. It returns ErrStreamNotFound
// before writing anything when the run is unknown or owned by someone else.
func (h *StreamHub) Serve(w http.ResponseWriter, req *http.Request, runID string, owner StreamOwner, afterSeq uint64) error {
	h.mu.Lock()
	run := h.runs[runID]
	h.mu.Unlock()
	if run == nil || run.owner != owner {
		if h.cfg.Journal != nil {
			return h.serveShared(w, req, runID, owner, afterSeq)
		}
		return ErrStreamNotFound
	}
	if afterSeq > 0 {
		streamReconnects.Add(req.Context(), 1, metric.WithAttributes(attribute.String("source", "local")))
	}
	run.mu.Lock()
	run.subscribers++
	if run.graceTimer != nil {
		run.graceTimer.Stop()
		run.graceTimer = nil
	}
	run.mu.Unlock()
	defer h.unsubscribe(run)

	flusher, ok := writeStreamHeaders(w, runID)
	if !ok {
		return nil
	}
	cursor := afterSeq
	for {
		run.mu.Lock()
		var pending []Frame
		truncated := false
		if len(run.frames) > 0 && run.frames[0].Seq > cursor+1 && cursor > 0 {
			truncated = true
		}
		for _, frame := range run.frames {
			if frame.Seq > cursor {
				pending = append(pending, frame)
			}
		}
		done, notify := run.done, run.notify
		run.mu.Unlock()

		if truncated {
			// The client missed frames that were dropped from the bounded
			// journal; tell it so it can refetch the run state.
			fmt.Fprintf(w, "event: replay_truncated\ndata: {\"first_available\":%d}\n\n", pending[0].Seq)
		}
		for _, frame := range pending {
			writeFrame(w, frame)
			cursor = frame.Seq
		}
		if len(pending) > 0 || truncated {
			flusher.Flush()
		}
		if done && len(pending) == 0 {
			return nil
		}
		if done {
			continue
		}
		select {
		case <-notify:
		case <-req.Context().Done():
			return nil
		}
	}
}

// serveShared follows a run published on another replica by polling the
// shared journal, touching the subscriber marker so the publishing replica
// does not cancel the run as abandoned.
func (h *StreamHub) serveShared(w http.ResponseWriter, req *http.Request, runID string, owner StreamOwner, afterSeq uint64) error {
	ctx := req.Context()
	frames, done, err := h.cfg.Journal.Read(ctx, runID, owner, afterSeq, 500)
	if errors.Is(err, stream.ErrJournalNotFound) {
		return ErrStreamNotFound
	}
	if err != nil {
		return err
	}
	streamReconnects.Add(ctx, 1, metric.WithAttributes(attribute.String("source", "shared")))
	flusher, ok := writeStreamHeaders(w, runID)
	if !ok {
		return nil
	}
	cursor := afterSeq
	for {
		_ = h.cfg.Journal.TouchSubscriber(ctx, runID)
		for _, frame := range frames {
			writeFrame(w, frame)
			cursor = frame.Seq
		}
		if len(frames) > 0 {
			flusher.Flush()
		}
		if done && len(frames) == 0 {
			return nil
		}
		if len(frames) == 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(h.cfg.PollInterval):
			}
		}
		frames, done, err = h.cfg.Journal.Read(ctx, runID, owner, cursor, 500)
		if err != nil {
			// The headers are out; end the stream and let the client
			// reconnect from its last id.
			return nil
		}
	}
}

func (h *StreamHub) unsubscribe(run *hubRun) {
	run.mu.Lock()
	defer run.mu.Unlock()
	run.subscribers--
	if run.subscribers > 0 || run.done || run.cancel == nil {
		return
	}
	if h.cfg.DisconnectGrace < 0 {
		run.cancel()
		return
	}
	h.armGraceLocked(run)
}

// armGraceLocked cancels the run after the grace unless a subscriber
// attached here or, through the shared journal, on another replica. The
// caller holds run.mu.
func (h *StreamHub) armGraceLocked(run *hubRun) {
	cancel := run.cancel
	run.graceTimer = time.AfterFunc(h.cfg.DisconnectGrace, func() {
		if h.cfg.Journal != nil {
			ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
			seen, err := h.cfg.Journal.SubscriberSeen(ctx, run.id)
			done()
			if err == nil && time.Since(seen) < h.cfg.DisconnectGrace {
				run.mu.Lock()
				if run.subscribers == 0 && !run.done {
					h.armGraceLocked(run)
				}
				run.mu.Unlock()
				return
			}
		}
		run.mu.Lock()
		abandoned := run.subscribers == 0 && !run.done
		run.mu.Unlock()
		if abandoned {
			cancel()
		}
	})
}

// Active returns the number of runs still producing frames.
func (h *StreamHub) Active() int {
	n := 0
	for _, run := range h.snapshot() {
		run.mu.Lock()
		if !run.done {
			n++
		}
		run.mu.Unlock()
	}
	return n
}

func (h *StreamHub) snapshot() []*hubRun {
	h.mu.Lock()
	defer h.mu.Unlock()
	runs := make([]*hubRun, 0, len(h.runs))
	for _, run := range h.runs {
		runs = append(runs, run)
	}
	return runs
}

// Shutdown waits for streamed runs to finish until ctx is done, then
// cancels the rest; their final frames still reach attached subscribers.
func (h *StreamHub) Shutdown(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for h.Active() > 0 {
		select {
		case <-ctx.Done():
			for _, run := range h.snapshot() {
				run.mu.Lock()
				cancel := run.cancel
				if run.done {
					cancel = nil
				}
				run.mu.Unlock()
				if cancel != nil {
					cancel()
				}
			}
			return
		case <-ticker.C:
		}
	}
}

// DeleteExpired drops in-memory journals past Retention and removes shared
// journals finished longer than Retention ago; AgentOS maintenance calls it
// periodically so memory is released even when no new run starts.
func (h *StreamHub) DeleteExpired(ctx context.Context) (int, error) {
	h.mu.Lock()
	h.evictLocked(time.Now())
	h.mu.Unlock()
	if h.cfg.Journal == nil {
		return 0, nil
	}
	return h.cfg.Journal.DeleteFinishedBefore(ctx, time.Now().Add(-h.cfg.Retention))
}

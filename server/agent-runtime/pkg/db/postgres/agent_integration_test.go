package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// blockingModel blocks until the run context ends.
type blockingModel struct{ entered chan struct{} }

func (b *blockingModel) Response(ctx context.Context, _ []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
	close(b.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}
func (b *blockingModel) ResponseStream(context.Context, []models.Message, models.ModelOptions) (<-chan models.StreamEvent, error) {
	return nil, errors.New("not streamed")
}
func (*blockingModel) ID() string       { return "blocking" }
func (*blockingModel) Provider() string { return "test" }

// TestAgentLeaseLifecycleOnPostgres runs an Agent against PostgreSQL: the
// run records a lease, heartbeats while a model call is in flight, and stops
// when another process marks it cancelled.
func TestAgentLeaseLifecycleOnPostgres(t *testing.T) {
	store := testDB(t)
	m := &blockingModel{entered: make(chan struct{})}
	ag, err := agent.New(agent.AgentConfig{ID: "lease-agent", Model: m, DB: store, UserID: "alice", RunLeaseTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	runID := uuid.NewString()
	go func() {
		<-m.entered
		time.Sleep(150 * time.Millisecond) // let a few heartbeats land
		run, err := store.GetRun(context.Background(), db.RunQuery{RunID: runID})
		if err != nil || run == nil {
			t.Errorf("GetRun: %v", err)
			return
		}
		if time.Since(run.UpdatedAt) > 120*time.Millisecond {
			t.Errorf("heartbeat did not refresh updated_at: %s old", time.Since(run.UpdatedAt))
		}
		run.Status = session.RunStatusCancelled
		if err := store.UpdateRun(context.Background(), run, session.RunStatusRunning); err != nil {
			t.Errorf("cancel elsewhere: %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := ag.RunWith(ctx, session.RunRequest{RunID: runID, Input: session.RunInput{Message: "q"}})
	if !errors.Is(err, agent.ErrRunCancelledExternally) || out.Status != session.RunStatusCancelled {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
	stored, _ := store.GetRun(context.Background(), db.RunQuery{RunID: runID})
	if stored.Status != session.RunStatusCancelled || stored.LeaseTimeout != 200*time.Millisecond {
		t.Fatalf("stored = %s lease %s", stored.Status, stored.LeaseTimeout)
	}
	// A cancelled run is never expired by the reaper.
	if ids, err := store.ExpireStaleRuns(context.Background(), time.Now().Add(time.Hour), "x", 1000); err != nil {
		t.Fatal(err)
	} else {
		for _, id := range ids {
			if id == runID {
				t.Fatal("reaper expired a cancelled run")
			}
		}
	}
}

// TestAgentHistoryAndCheckpointsOnPostgres covers recent-run history reads
// and per-round checkpoints through the real store.
func TestAgentHistoryAndCheckpointsOnPostgres(t *testing.T) {
	store := testDB(t)
	var seen [][]models.Message
	m := &recordingModel{seen: &seen}
	ag, err := agent.New(agent.AgentConfig{ID: "history-agent", Model: m, DB: store, UserID: "alice", MaxHistoryRuns: 1})
	if err != nil {
		t.Fatal(err)
	}
	first, err := ag.Run(context.Background(), "q1")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"q2", "q3"} {
		if _, err := ag.RunWith(context.Background(), session.RunRequest{Input: session.RunInput{Message: q}, SessionID: first.SessionID}); err != nil {
			t.Fatal(err)
		}
	}
	var users []string
	for _, msg := range seen[2] {
		if msg.Role == models.RoleUser {
			users = append(users, msg.Content)
		}
	}
	if len(users) != 2 || users[0] != "q2" || users[1] != "q3" {
		t.Fatalf("history users = %v, want only the newest run plus the input", users)
	}
}

type recordingModel struct{ seen *[][]models.Message }

func (r *recordingModel) Response(_ context.Context, messages []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
	*r.seen = append(*r.seen, append([]models.Message(nil), messages...))
	return &models.ModelResponse{Content: "ok", FinishReason: models.FinishReasonStop}, nil
}
func (r *recordingModel) ResponseStream(context.Context, []models.Message, models.ModelOptions) (<-chan models.StreamEvent, error) {
	return nil, errors.New("not streamed")
}
func (*recordingModel) ID() string       { return "recording" }
func (*recordingModel) Provider() string { return "test" }

// TestSessionLockerSerializesAcrossReplicas runs two Agents, standing in for
// two replicas, each with its own locker over the same database.
func TestSessionLockerSerializesAcrossReplicas(t *testing.T) {
	store := testDB(t)
	gate := &gatedModel{entered: make(chan struct{}), release: make(chan struct{})}
	newReplica := func() *agent.Agent {
		ag, err := agent.New(agent.AgentConfig{ID: "locked-agent", Model: gate, DB: store, UserID: "alice",
			SessionLocker: store.SessionLocker(SessionLockConfig{LeaseTTL: 2 * time.Second, PollInterval: 20 * time.Millisecond})})
		if err != nil {
			t.Fatal(err)
		}
		return ag
	}
	a, b := newReplica(), newReplica()
	seed, err := a.Run(context.Background(), "seed")
	if err != nil {
		t.Fatal(err)
	}
	req := func(q string) session.RunRequest {
		return session.RunRequest{Input: session.RunInput{Message: q}, SessionID: seed.SessionID}
	}
	firstDone := make(chan error, 1)
	go func() { _, err := a.RunWith(context.Background(), req("first")); firstDone <- err }()
	select {
	case <-gate.entered:
	case err := <-firstDone:
		t.Fatalf("first run ended before reaching the model: %v", err)
	}
	secondDone := make(chan error, 1)
	go func() { _, err := b.RunWith(context.Background(), req("second")); secondDone <- err }()
	time.Sleep(150 * time.Millisecond)
	if calls := gate.callCount(); calls != 2 { // seed + first
		t.Fatalf("second replica reached the model while the session was locked: calls=%d", calls)
	}
	close(gate.release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if !gate.lastSaw("first") {
		t.Fatal("second run did not see the first run's transcript")
	}
}

func TestSessionLockExpiresWhenHolderDisappears(t *testing.T) {
	store := testDB(t)
	crashed := store.SessionLocker(SessionLockConfig{LeaseTTL: 300 * time.Millisecond})
	key := "lock-" + uuid.NewString()
	// Take the lock and "crash": never unlock and stop renewing by
	// simulating the row a dead replica leaves behind.
	if ok, err := crashed.tryLock(context.Background(), key, "dead-replica"); err != nil || !ok {
		t.Fatalf("tryLock = %v, %v", ok, err)
	}
	other := store.SessionLocker(SessionLockConfig{LeaseTTL: time.Second, PollInterval: 20 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	unlock, err := other.LockSession(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if waited := time.Since(started); waited < 200*time.Millisecond {
		t.Fatalf("took over a live lease after %s", waited)
	}
	quick, cancelQuick := context.WithTimeout(context.Background(), time.Second)
	defer cancelQuick()
	unlock, err = other.LockSession(quick, key)
	if err != nil {
		t.Fatalf("unlock did not free the session: %v", err)
	}
	unlock()
}

// gatedModel blocks the call whose input is "first" until release closes.
type gatedModel struct {
	mu       sync.Mutex
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	received [][]models.Message
}

func (g *gatedModel) Response(ctx context.Context, messages []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
	g.mu.Lock()
	g.received = append(g.received, append([]models.Message(nil), messages...))
	g.mu.Unlock()
	if messages[len(messages)-1].Content == "first" {
		g.once.Do(func() { close(g.entered) })
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &models.ModelResponse{Content: "ok", FinishReason: models.FinishReasonStop}, nil
}
func (g *gatedModel) ResponseStream(context.Context, []models.Message, models.ModelOptions) (<-chan models.StreamEvent, error) {
	return nil, errors.New("not streamed")
}
func (*gatedModel) ID() string       { return "gated" }
func (*gatedModel) Provider() string { return "test" }
func (g *gatedModel) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.received)
}
func (g *gatedModel) lastSaw(content string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, m := range g.received[len(g.received)-1] {
		if m.Content == content {
			return true
		}
	}
	return false
}

func TestAgentRecoverOnPostgres(t *testing.T) {
	store := testDB(t)
	ctx := context.Background()
	sid, runID := uuid.NewString(), uuid.NewString()
	if err := store.CreateSession(ctx, &session.AgentSession{SessionID: sid, AgentID: "recover-agent", UserID: "alice"}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := store.AppendRun(ctx, &session.RunOutput{RunID: runID, AgentID: "recover-agent", SessionID: sid, UserID: "alice",
		Input: session.RunInput{Message: "hello"}, Status: session.RunStatusRunning, LeaseTimeout: time.Minute, CreatedAt: old, UpdatedAt: old,
		Messages: []models.Message{{Role: models.RoleUser, Content: "hello"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExpireStaleRuns(ctx, time.Now(), runmanager.StaleRunReason, 1000); err != nil {
		t.Fatal(err)
	}
	var seen [][]models.Message
	ag, err := agent.New(agent.AgentConfig{ID: "recover-agent", Model: &recordingModel{seen: &seen}, DB: store, UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ag.Recover(ctx, agent.RecoverRequest{RunID: runID})
	if err != nil || out.Status != session.RunStatusCompleted {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
	orig, _ := store.GetRun(ctx, db.RunQuery{RunID: runID})
	if orig.RecoveredBy != out.RunID || orig.Status != session.RunStatusError {
		t.Fatalf("original = %s recovered_by=%q", orig.Status, orig.RecoveredBy)
	}
	if _, err := ag.Recover(ctx, agent.RecoverRequest{RunID: runID}); !errors.Is(err, agent.ErrRecoverConflict) {
		t.Fatalf("second recover = %v", err)
	}
}

// TestRunStopsWhenItsSessionLockIsTaken simulates a replica whose renewals
// failed long enough for another holder to take the session: the run must
// stop instead of writing the session alongside the new holder.
func TestRunStopsWhenItsSessionLockIsTaken(t *testing.T) {
	store := testDB(t)
	locker := store.SessionLocker(SessionLockConfig{LeaseTTL: 300 * time.Millisecond})
	prior, err := agent.New(agent.AgentConfig{Model: &gatedModel{entered: make(chan struct{}), release: make(chan struct{})}, DB: store, UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := prior.Run(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	m := &blockingModel{entered: make(chan struct{})}
	ag, err := agent.New(agent.AgentConfig{Model: m, DB: store, UserID: "u", SessionLocker: locker, RunLeaseTimeout: -1})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		<-m.entered
		if _, err := store.db.Exec(`UPDATE agent_runtime_session_locks SET holder = 'other-replica'`); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = ag.RunWith(ctx, session.RunRequest{SessionID: first.SessionID, Input: session.RunInput{Message: "q"}})
	if !errors.Is(err, agent.ErrSessionLockLost) {
		t.Fatalf("err = %v, want ErrSessionLockLost", err)
	}
}

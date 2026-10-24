package stream

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// ErrJournalNotFound reports that a journal holds no run with that ID for
// the given owner.
var ErrJournalNotFound = errors.New("stream journal not found")

// Frame is one journaled server-sent event. Seq increases by one per frame
// of a run, across every replica that published to it.
type Frame struct {
	Seq   uint64
	Event string
	Data  json.RawMessage
}

// Owner identifies who may read a run's frames. It is derived from
// verified request identity only.
type Owner struct {
	Subject   string
	Isolated  bool
	Namespace string
	Tenant    bool
}

// Journal is a store of SSE frames shared by every replica, so a client
// can reconnect to any of them. The publishing replica writes frames in
// batches; readers on other replicas poll. Implementations must be safe
// for concurrent use.
type Journal interface {
	// Append stores frames (already numbered) for runID, creating the run
	// for owner on first use; done marks the run finished.
	Append(ctx context.Context, runID string, owner Owner, frames []Frame, done bool) error
	// Read returns up to limit frames with Seq > after and whether the run
	// is finished. It returns ErrJournalNotFound for an unknown run or a
	// different owner.
	Read(ctx context.Context, runID string, owner Owner, after uint64, limit int) ([]Frame, bool, error)
	// LastSeq returns the highest stored Seq of runID (0 when unknown), so
	// a run resumed on another replica continues the numbering.
	LastSeq(ctx context.Context, runID string) (uint64, error)
	// TouchSubscriber records that a client is attached to runID.
	TouchSubscriber(ctx context.Context, runID string) error
	// SubscriberSeen returns when a client last touched runID.
	SubscriberSeen(ctx context.Context, runID string) (time.Time, error)
	// DeleteFinishedBefore removes runs finished before cutoff.
	DeleteFinishedBefore(ctx context.Context, cutoff time.Time) (int, error)
}

// NewMemoryJournal returns an in-process Journal, useful for tests and for
// several stream hubs in one process.
func NewMemoryJournal() Journal {
	return &memoryJournal{runs: make(map[string]*memoryRun)}
}

type memoryJournal struct {
	mu   sync.Mutex
	runs map[string]*memoryRun
}

type memoryRun struct {
	owner      Owner
	frames     []Frame
	done       bool
	finishedAt time.Time
	seen       time.Time
}

func (j *memoryJournal) Append(_ context.Context, runID string, owner Owner, frames []Frame, done bool) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	run := j.runs[runID]
	if run == nil {
		run = &memoryRun{owner: owner}
		j.runs[runID] = run
	}
	if run.owner != owner {
		return ErrJournalNotFound
	}
	run.frames = append(run.frames, frames...)
	run.done = done
	if done {
		run.finishedAt = time.Now()
	}
	return nil
}

func (j *memoryJournal) Read(_ context.Context, runID string, owner Owner, after uint64, limit int) ([]Frame, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	run := j.runs[runID]
	if run == nil || run.owner != owner {
		return nil, false, ErrJournalNotFound
	}
	var out []Frame
	for _, f := range run.frames {
		if f.Seq > after && (limit <= 0 || len(out) < limit) {
			out = append(out, f)
		}
	}
	return out, run.done, nil
}

func (j *memoryJournal) LastSeq(_ context.Context, runID string) (uint64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	run := j.runs[runID]
	if run == nil || len(run.frames) == 0 {
		return 0, nil
	}
	return run.frames[len(run.frames)-1].Seq, nil
}

func (j *memoryJournal) TouchSubscriber(_ context.Context, runID string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if run := j.runs[runID]; run != nil {
		run.seen = time.Now()
	}
	return nil
}

func (j *memoryJournal) SubscriberSeen(_ context.Context, runID string) (time.Time, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if run := j.runs[runID]; run != nil {
		return run.seen, nil
	}
	return time.Time{}, nil
}

func (j *memoryJournal) DeleteFinishedBefore(_ context.Context, cutoff time.Time) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	n := 0
	for id, run := range j.runs {
		if run.done && run.finishedAt.Before(cutoff) {
			delete(j.runs, id)
			n++
		}
	}
	return n, nil
}

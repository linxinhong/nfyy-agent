package memory

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type recallStore struct {
	result ListResult
	err    error
	calls  atomic.Int64
	req    ListRequest
}

type deadlineIgnoringRecallStore struct{ recallStore }

type recallTelemetryCapture struct{ traces chan RecallTelemetry }

func (c recallTelemetryCapture) ObserveRecall(_ context.Context, trace RecallTelemetry) {
	c.traces <- trace
}

func (s *deadlineIgnoringRecallStore) ListRecords(ctx context.Context, _ ListRequest) (ListResult, error) {
	<-ctx.Done()
	return s.result, nil
}

func (s *recallStore) SnapshotSubject(context.Context, SubjectScope) (ControlSnapshot, error) {
	return ControlSnapshot{}, errors.New("unused")
}
func (s *recallStore) Snapshot(context.Context, Scope, string) (ControlSnapshot, error) {
	return ControlSnapshot{}, errors.New("unused")
}
func (s *recallStore) ReadRecord(context.Context, Scope, string) (*Record, ControlSnapshot, error) {
	return nil, ControlSnapshot{}, errors.New("unused")
}
func (s *recallStore) CommitMutation(context.Context, Mutation) (MutationResult, error) {
	return MutationResult{}, errors.New("unused")
}
func (s *recallStore) ListRecords(ctx context.Context, req ListRequest) (ListResult, error) {
	s.calls.Add(1)
	s.req = req
	if s.err != nil {
		return ListResult{}, s.err
	}
	return s.result, ctx.Err()
}

func newRecallModule(t *testing.T, store AtomicStore, limits Limits, authorize AuthorizerFunc) *Module {
	t.Helper()
	if authorize == nil {
		authorize = func(context.Context, AuthorizeRequest) error { return nil }
	}
	m, err := NewModule(ModuleConfig{Store: store, Authorizer: authorize, Keys: map[string][]byte{"test": []byte("0123456789abcdef0123456789abcdef")}, CurrentKey: "test", Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRecallBoundedRankingAndMetadata(t *testing.T) {
	now := time.Date(2026, 9, 8, 1, 2, 3, 0, time.UTC)
	scope := Scope{Namespace: "n", ProjectID: "p", SubjectID: "s", CollectionID: "c"}
	store := &recallStore{result: ListResult{StoreNow: now, Generation: 7, Truncated: true, Records: []Record{
		{Scope: scope, MemoryID: "new-unrelated", Revision: 1, SubjectGeneration: 7, Kind: KindPreference, Content: "tea", Status: StatusActive, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)},
		{Scope: scope, MemoryID: "older-match", Revision: 3, SubjectGeneration: 7, Kind: KindPreference, Content: "prefers dark coffee", Topics: []string{"drink"}, Status: StatusActive, Provenance: Provenance{SourceRef: "run", SourceVersion: "v2", SourceEpoch: 4}, UpdatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)},
	}}}
	m := newRecallModule(t, store, DefaultLimits(), nil)
	got, err := m.Recall(context.Background(), Principal{Subject: "s", Namespace: "n", Issuer: "host"}, scope, RecallRequest{Query: "coffee", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 2 || got.Items[0].MemoryID != "older-match" || got.Items[0].Revision != 3 || got.Items[0].SourceEpoch != 4 {
		t.Fatalf("ranked items = %#v", got.Items)
	}
	if got.CandidateCount != 2 || !got.CandidateTruncated || store.req.Limit != 200 || store.req.CandidateLimit != 200 || store.req.CandidateBytes != 1600<<10 || store.calls.Load() != 1 {
		t.Fatalf("bounds/result = %#v request=%#v calls=%d", got, store.req, store.calls.Load())
	}
}

func TestRecallRejectsIneligibleAdapterResult(t *testing.T) {
	now := time.Now().UTC()
	scope := Scope{Namespace: "n", ProjectID: "p", SubjectID: "s", CollectionID: "c"}
	wrong := Scope{Namespace: "other", ProjectID: "p", SubjectID: "s", CollectionID: "c"}
	store := &recallStore{result: ListResult{StoreNow: now, Generation: 1, Records: []Record{{Scope: wrong, MemoryID: "leak", SubjectGeneration: 1, Status: StatusActive, Content: "secret", ExpiresAt: now.Add(time.Hour)}}}}
	_, err := newRecallModule(t, store, DefaultLimits(), nil).Recall(context.Background(), Principal{Subject: "s", Namespace: "n", Issuer: "host"}, scope, RecallRequest{})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v", err)
	}
}

func TestRecallTotalDeadlineRejectsLateSuccess(t *testing.T) {
	limits := DefaultLimits()
	limits.RecallTimeout = time.Millisecond
	store := &deadlineIgnoringRecallStore{}
	scope := Scope{Namespace: "n", ProjectID: "p", SubjectID: "s", CollectionID: "c"}
	_, err := newRecallModule(t, store, limits, nil).Recall(context.Background(), Principal{Subject: "s", Namespace: "n", Issuer: "host"}, scope, RecallRequest{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
}

func TestRecallTelemetryReportsListDeadlineWithoutPayload(t *testing.T) {
	limits := DefaultLimits()
	limits.RecallTimeout = time.Millisecond
	store := &deadlineIgnoringRecallStore{}
	traces := make(chan RecallTelemetry, 1)
	m, err := NewModule(ModuleConfig{
		Store: store, Authorizer: AuthorizerFunc(func(context.Context, AuthorizeRequest) error { return nil }),
		Keys: map[string][]byte{"test": []byte("0123456789abcdef0123456789abcdef")}, CurrentKey: "test", Limits: limits,
		RecallObserver: recallTelemetryCapture{traces: traces},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Namespace: "n", ProjectID: "p", SubjectID: "s", CollectionID: "c"}
	_, err = m.Recall(context.Background(), Principal{Subject: "s", Namespace: "n", Issuer: "host"}, scope, RecallRequest{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
	select {
	case trace := <-traces:
		if trace.ListRecords <= 0 || trace.Total < trace.ListRecords || trace.DeadlineAt != "after_list_records" || trace.Ranking != 0 {
			t.Fatalf("trace = %+v", trace)
		}
	case <-time.After(time.Second):
		t.Fatal("missing recall telemetry")
	}
}

func TestRecallAuthorizationAndInvalidQueryPrecedeStore(t *testing.T) {
	scope := Scope{Namespace: "n", ProjectID: "p", SubjectID: "s", CollectionID: "c"}
	store := &recallStore{}
	m := newRecallModule(t, store, DefaultLimits(), func(context.Context, AuthorizeRequest) error { return errors.New("denied") })
	if _, err := m.Recall(context.Background(), Principal{Subject: "s", Namespace: "n", Issuer: "host"}, scope, RecallRequest{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("authorization = %v", err)
	}
	if store.calls.Load() != 0 {
		t.Fatalf("store calls = %d", store.calls.Load())
	}
}

func TestRecallOutputByteExactEdge(t *testing.T) {
	now := time.Now().UTC()
	scope := Scope{Namespace: "n", ProjectID: "p", SubjectID: "s", CollectionID: "c"}
	record := Record{Scope: scope, MemoryID: "m", Revision: 1, SubjectGeneration: 1, Kind: KindPreference, Content: "coffee", Status: StatusActive, ExpiresAt: now.Add(time.Hour)}
	item := RecallItem{MemoryID: "m", Revision: 1, SubjectGeneration: 1, Kind: KindPreference, Content: "coffee", ExpiresAt: record.ExpiresAt}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name         string
		budget, want int
	}{{"exact", len(encoded), 1}, {"under", len(encoded) - 1, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			limits := DefaultLimits()
			limits.MaxRecallBytes = tc.budget
			store := &recallStore{result: ListResult{StoreNow: now, Generation: 1, Records: []Record{record}}}
			got, err := newRecallModule(t, store, limits, nil).Recall(context.Background(), Principal{Subject: "s", Namespace: "n", Issuer: "host"}, scope, RecallRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Items) != tc.want || got.OutputTruncated != (tc.want == 0) {
				t.Fatalf("result = %#v", got)
			}
		})
	}
}

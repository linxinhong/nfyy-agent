// Package memorytest provides the shared behavioral contract required of
// every strict memory.AtomicStore adapter.
package memorytest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

type Harness struct {
	Store               memory.AtomicStore
	Advance             func(time.Duration)
	ContentReads        func() uint64
	LifecycleExamined   func() uint64
	LifecycleAudits     func() []memory.AuditEvent
	ReceiptContentBytes func() int
	FailAudit           func(bool)
}

type Factory func(*testing.T, memory.Limits) Harness

type countingAtomicStore struct {
	memory.AtomicStore
	calls atomic.Int64
}

func (s *countingAtomicStore) SnapshotSubject(ctx context.Context, subject memory.SubjectScope) (memory.ControlSnapshot, error) {
	s.calls.Add(1)
	return s.AtomicStore.SnapshotSubject(ctx, subject)
}

func (s *countingAtomicStore) Snapshot(ctx context.Context, scope memory.Scope, sourceRef string) (memory.ControlSnapshot, error) {
	s.calls.Add(1)
	return s.AtomicStore.Snapshot(ctx, scope, sourceRef)
}

func (s *countingAtomicStore) ReadRecord(ctx context.Context, scope memory.Scope, id string) (*memory.Record, memory.ControlSnapshot, error) {
	s.calls.Add(1)
	return s.AtomicStore.ReadRecord(ctx, scope, id)
}

func (s *countingAtomicStore) ListRecords(ctx context.Context, req memory.ListRequest) (memory.ListResult, error) {
	s.calls.Add(1)
	return s.AtomicStore.ListRecords(ctx, req)
}

func (s *countingAtomicStore) CommitMutation(ctx context.Context, cmd memory.Mutation) (memory.MutationResult, error) {
	s.calls.Add(1)
	return s.AtomicStore.CommitMutation(ctx, cmd)
}

var operationSequence atomic.Uint64

var (
	testKeyOld = []byte("0123456789abcdef0123456789abcdef")
	testKeyNew = []byte("fedcba9876543210fedcba9876543210")
)

func Contract(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("scope_isolation_and_unambiguous_keys", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		scopes := []memory.Scope{
			{Namespace: "n", ProjectID: "p", SubjectID: "s", CollectionID: "c"},
			{Namespace: "n2", ProjectID: "p", SubjectID: "s", CollectionID: "c"},
			{Namespace: "n", ProjectID: "p2", SubjectID: "s", CollectionID: "c"},
			{Namespace: "n", ProjectID: "p", SubjectID: "s2", CollectionID: "c"},
			{Namespace: "n", ProjectID: "p", SubjectID: "s", CollectionID: "c2"},
			{Namespace: "a:b", ProjectID: "c", SubjectID: "d", CollectionID: "e"},
			{Namespace: "a", ProjectID: "b:c", SubjectID: "d", CollectionID: "e"},
		}
		for i, scope := range scopes {
			directCreate(t, h.Store, scope, "same-id", fmt.Sprintf("body-%d", i))
		}
		for i, scope := range scopes {
			r, _, err := h.Store.ReadRecord(context.Background(), scope, "same-id")
			if err != nil || r == nil || r.Content != fmt.Sprintf("body-%d", i) {
				t.Fatalf("scope %d read = %#v, %v", i, r, err)
			}
		}
	})

	t.Run("authorization_precedes_content_read", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		scope := baseScope()
		directCreate(t, h.Store, scope, "secret", "classified")
		m := newModule(t, h.Store, memory.DefaultLimits(), map[string][]byte{"old": testKeyOld}, "old")
		before := h.ContentReads()
		if _, err := m.Read(context.Background(), memory.Principal{}, scope, "secret"); !errors.Is(err, memory.ErrUnauthorized) {
			t.Fatalf("missing principal: %v", err)
		}
		spoof := principal(scope)
		spoof.Namespace = "other"
		if _, err := m.Read(context.Background(), spoof, scope, "secret"); !errors.Is(err, memory.ErrUnauthorized) {
			t.Fatalf("spoofed namespace: %v", err)
		}
		if got := h.ContentReads(); got != before {
			t.Fatalf("content reads = %d, want %d", got, before)
		}
	})

	t.Run("sensitive_content_policy_precedes_store", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		scope := baseScope()
		p := principal(scope)
		counting := &countingAtomicStore{AtomicStore: h.Store}
		m, err := memory.NewModule(memory.ModuleConfig{
			Store:      counting,
			Authorizer: memory.AuthorizerFunc(func(context.Context, memory.AuthorizeRequest) error { return nil }),
			ContentPolicy: memory.ContentPolicyFunc(func(_ context.Context, req memory.ContentPolicyRequest) error {
				if req.Content == "host-classified" {
					return errors.New("rejected")
				}
				return nil
			}),
			Keys: map[string][]byte{"old": testKeyOld}, CurrentKey: "old", Limits: memory.DefaultLimits(),
		})
		if err != nil {
			t.Fatal(err)
		}
		token := issue(t, m, p, scope, memory.ActionCreate, time.Hour)
		for name, content := range map[string]string{
			"pkcs8":       "-----BEGIN PRIVATE KEY-----\nnot-logged\n-----END PRIVATE KEY-----",
			"bearer":      "Authorization: Bearer abcdefghijklmnop",
			"host_policy": "host-classified",
		} {
			counting.calls.Store(0)
			if _, err := m.Create(context.Background(), p, scope, memory.CreateRequest{Content: content, Kind: memory.KindPreference, OperationToken: token}); !errors.Is(err, memory.ErrInvalid) {
				t.Errorf("%s content policy = %v", name, err)
			}
			if calls := counting.calls.Load(); calls != 0 {
				t.Errorf("%s caused %d store calls", name, calls)
			}
		}

		batch := buildCandidateBatchCommand(t, h.Store, scope, memory.CandidateDraft{MemoryID: "sensitive-candidate", Kind: memory.KindPreference, Content: "-----BEGIN OPENSSH PRIVATE KEY-----\nnot-logged", Retention: time.Hour})
		if _, err := h.Store.CommitMutation(context.Background(), batch); !errors.Is(err, memory.ErrInvalid) {
			t.Fatalf("candidate credential = %v", err)
		}
		if record, _, err := h.Store.ReadRecord(context.Background(), scope, "sensitive-candidate"); err != nil || record != nil {
			t.Fatalf("candidate credential committed = %#v, %v", record, err)
		}
	})

	t.Run("oversized_operation_token_is_constant_cost_before_store", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		scope := baseScope()
		counting := &countingAtomicStore{AtomicStore: h.Store}
		m := newModule(t, counting, memory.DefaultLimits(), map[string][]byte{"old": testKeyOld}, "old")
		oversized := strings.Repeat("A", 8<<20) + "." + strings.Repeat("B", 43)
		req := memory.CreateRequest{Content: "small", Kind: memory.KindPreference, OperationToken: oversized}
		if _, err := m.Create(context.Background(), principal(scope), scope, req); !errors.Is(err, memory.ErrUnauthorized) {
			t.Fatalf("oversized token = %v", err)
		}
		if calls := counting.calls.Load(); calls != 0 {
			t.Fatalf("oversized token caused %d store calls", calls)
		}
		bench := testing.Benchmark(func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				_, _ = m.Create(context.Background(), principal(scope), scope, req)
			}
		})
		if bytes := bench.AllocedBytesPerOp(); bytes > 64<<10 {
			t.Fatalf("oversized token allocated %d bytes/op", bytes)
		}
		if elapsed := time.Duration(bench.NsPerOp()); elapsed > 5*time.Millisecond {
			t.Fatalf("oversized token took %s/op", elapsed)
		}
		if calls := counting.calls.Load(); calls != 0 {
			t.Fatalf("oversized token benchmark caused %d store calls", calls)
		}
	})

	t.Run("store_replay_binding", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		scope := baseScope()
		cmd := directCreate(t, h.Store, scope, "bound-id", "body")
		other := scope
		other.CollectionID = "other"
		forgedScope := cmd
		forgedScope.Scope = other
		forgedScope.Subject = other.Subject()
		forgedScope.Audit.Scope = other
		forgedScope.Audit.Subject = other.Subject()
		if _, err := h.Store.CommitMutation(context.Background(), forgedScope); !errors.Is(err, memory.ErrInvalid) {
			t.Fatalf("cross-scope replay = %v", err)
		}
		forgedAction := cmd
		forgedAction.Type = memory.MutationRetract
		if _, err := h.Store.CommitMutation(context.Background(), forgedAction); !errors.Is(err, memory.ErrInvalid) {
			t.Fatalf("cross-action replay = %v", err)
		}
	})

	t.Run("receipt_quota_is_scope_wide_across_actions", func(t *testing.T) {
		limits := memory.DefaultLimits()
		limits.MaxReceiptsPerScope = 1
		h := factory(t, limits)
		scope := baseScope()
		p := principal(scope)
		m := newModule(t, h.Store, limits, map[string][]byte{"old": testKeyOld}, "old")
		created := create(t, m, p, scope, "body", nil)
		token := issue(t, m, p, scope, memory.ActionRetract, time.Hour)
		if _, err := m.RetractRecord(context.Background(), p, scope, memory.RetractRequest{MemoryID: created.MemoryID, ExpectedRevision: created.Revision, OperationToken: token}); !errors.Is(err, memory.ErrQuota) {
			t.Fatalf("cross-action receipt quota = %v", err)
		}
	})

	t.Run("bounded_keyset_materialization", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		scope := baseScope()
		for i := 0; i < 300; i++ {
			directCreate(t, h.Store, scope, fmt.Sprintf("item-%03d", i), "0123456789")
		}
		before := h.ContentReads()
		page, err := h.Store.ListRecords(context.Background(), memory.ListRequest{Scope: scope, Limit: 100, CandidateLimit: 20, CandidateBytes: 1600 << 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Records) != 20 || !page.Truncated {
			t.Fatalf("first page = %d truncated=%v", len(page.Records), page.Truncated)
		}
		if got := h.ContentReads() - before; got != 20 {
			t.Fatalf("materialized bodies = %d, want 20", got)
		}
		last := page.Records[len(page.Records)-1]
		second, err := h.Store.ListRecords(context.Background(), memory.ListRequest{Scope: scope, Limit: 100, CandidateLimit: 20, CandidateBytes: 1600 << 10, AfterScope: scope, AfterUpdatedAt: last.UpdatedAt, AfterMemoryID: last.MemoryID})
		if err != nil || len(second.Records) != 20 {
			t.Fatalf("second page = %d, %v", len(second.Records), err)
		}
		if second.Records[0].MemoryID >= last.MemoryID {
			t.Fatalf("keyset did not advance: %q >= %q", second.Records[0].MemoryID, last.MemoryID)
		}
		reads := h.ContentReads()
		wrong := scope
		wrong.ProjectID = "wrong"
		if _, err := h.Store.ListRecords(context.Background(), memory.ListRequest{Scope: scope, Limit: 10, CandidateLimit: 20, CandidateBytes: 1600 << 10, AfterScope: wrong, AfterUpdatedAt: last.UpdatedAt, AfterMemoryID: last.MemoryID}); !errors.Is(err, memory.ErrInvalid) {
			t.Fatalf("wrong-scope cursor = %v", err)
		}
		if h.ContentReads() != reads {
			t.Fatal("invalid cursor caused body reads")
		}
	})

	t.Run("encoded_json_budgets_are_exact_and_precede_clone", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		scope := baseScope()
		for i := 0; i < 2; i++ {
			directCreate(t, h.Store, scope, fmt.Sprintf("byte-%03d", i), strings.Repeat("\"", 10))
		}
		all, err := h.Store.ListRecords(context.Background(), memory.ListRequest{Scope: scope, Limit: 2, CandidateLimit: 2, CandidateBytes: memory.DefaultLimits().MaxCandidateBytes})
		if err != nil || len(all.Records) != 2 {
			t.Fatalf("budget fixture = %d, %v", len(all.Records), err)
		}
		oneEncoded, err := json.Marshal([]memory.Record{all.Records[0]})
		if err != nil {
			t.Fatal(err)
		}
		before := h.ContentReads()
		page, err := h.Store.ListRecords(context.Background(), memory.ListRequest{Scope: scope, Limit: 2, CandidateLimit: 2, CandidateBytes: len(oneEncoded)})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Records) != 1 || !page.Truncated {
			t.Fatalf("byte page = %d truncated=%v", len(page.Records), page.Truncated)
		}
		if got := h.ContentReads() - before; got != 1 {
			t.Fatalf("byte-limited bodies = %d, want 1", got)
		}
		before = h.ContentReads()
		under, err := h.Store.ListRecords(context.Background(), memory.ListRequest{Scope: scope, Limit: 2, CandidateLimit: 2, CandidateBytes: len(oneEncoded) - 1})
		if err != nil || len(under.Records) != 0 || !under.Truncated {
			t.Fatalf("one-byte-under candidate budget = %d truncated=%v, %v", len(under.Records), under.Truncated, err)
		}
		if got := h.ContentReads() - before; got != 0 {
			t.Fatalf("over-budget body clones = %d", got)
		}

		response := memory.ListResult{Records: []memory.Record{all.Records[0]}, StoreNow: all.StoreNow, Generation: all.Generation, Truncated: true}
		responseJSON, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		limits := memory.DefaultLimits()
		limits.MaxListBytes = len(responseJSON)
		m := newModule(t, h.Store, limits, map[string][]byte{"old": testKeyOld}, "old")
		exact, err := m.List(context.Background(), principal(scope), scope, 1)
		if err != nil || len(exact.Records) != 1 {
			t.Fatalf("exact response budget = %d, %v", len(exact.Records), err)
		}
		limits.MaxListBytes--
		m = newModule(t, h.Store, limits, map[string][]byte{"old": testKeyOld}, "old")
		underResponse, err := m.List(context.Background(), principal(scope), scope, 1)
		if err != nil || len(underResponse.Records) != 0 || !underResponse.Truncated {
			t.Fatalf("one-byte-under response budget = %d truncated=%v, %v", len(underResponse.Records), underResponse.Truncated, err)
		}
	})

	t.Run("encoded_record_json_limit_is_exact", func(t *testing.T) {
		scope := baseScope()
		now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
		content := strings.Repeat("\"", 40)
		record := memory.Record{Scope: scope, MemoryID: "json-boundary", Revision: 1, SubjectGeneration: 1, PolicyGeneration: 1, Kind: memory.KindPreference, Content: content, Status: memory.StatusActive, Provenance: memory.Provenance{Producer: "contract", PolicyVersion: "1"}, Decision: memory.Decision{Actor: scope.SubjectID, Basis: string(memory.ActionCreate), DecidedAt: now}, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		limits := memory.DefaultLimits()
		limits.MaxContentBytes = len(content)
		limits.MaxRecordBytes = len(encoded)
		exact := factory(t, limits)
		directCreate(t, exact.Store, scope, record.MemoryID, content)
		limits.MaxRecordBytes--
		under := factory(t, limits)
		if _, err := tryDirectCreate(t, under.Store, scope, record.MemoryID, content); !errors.Is(err, memory.ErrInvalid) {
			t.Fatalf("one-byte-under record budget = %v", err)
		}
	})

	t.Run("cas_has_one_winner", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		scope := baseScope()
		p := principal(scope)
		m := newModule(t, h.Store, memory.DefaultLimits(), map[string][]byte{"old": testKeyOld}, "old")
		created := create(t, m, p, scope, "initial", nil)
		tokens := make([]string, 64)
		for i := range tokens {
			tokens[i] = issue(t, m, p, scope, memory.ActionRevise, time.Hour)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var wins atomic.Int32
		for i := range tokens {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, err := m.Revise(context.Background(), p, scope, memory.ReviseRequest{MemoryID: created.MemoryID, ExpectedRevision: created.Revision, Content: fmt.Sprintf("v-%d", i), OperationToken: tokens[i]})
				if err == nil {
					wins.Add(1)
					return
				}
				if !errors.Is(err, memory.ErrConflict) && !errors.Is(err, memory.ErrOverloaded) {
					t.Errorf("revise %d: %v", i, err)
				}
			}(i)
		}
		close(start)
		wg.Wait()
		if got := wins.Load(); got != 1 {
			t.Fatalf("CAS winners = %d, want 1", got)
		}
	})

	t.Run("signed_token_and_idempotency", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		scope := baseScope()
		other := scope
		other.ProjectID = "other"
		p := principal(scope)
		m := newModule(t, h.Store, memory.DefaultLimits(), map[string][]byte{"old": testKeyOld}, "old")
		if _, err := m.IssueOperationToken(context.Background(), p, scope, memory.ActionCreate, 7*24*time.Hour); err != nil {
			t.Fatalf("maximum token lifetime: %v", err)
		}
		if _, err := m.IssueOperationToken(context.Background(), p, scope, memory.ActionCreate, 7*24*time.Hour+time.Nanosecond); !errors.Is(err, memory.ErrInvalid) {
			t.Fatalf("overlong token lifetime = %v", err)
		}
		token := issue(t, m, p, scope, memory.ActionCreate, time.Hour)
		request := memory.CreateRequest{Content: "same", Kind: memory.KindPreference, OperationToken: token}
		first, err := m.Create(context.Background(), p, scope, request)
		if err != nil {
			t.Fatal(err)
		}
		replay, err := m.Create(context.Background(), p, scope, request)
		if err != nil || replay.MemoryID != first.MemoryID || replay.Revision != first.Revision {
			t.Fatalf("replay = %#v, %v", replay, err)
		}
		if replay.Content != "" || len(replay.Topics) != 0 || h.ReceiptContentBytes() != 0 {
			t.Fatal("idempotency receipt retained or replayed content")
		}
		request.Content = "different"
		if _, err := m.Create(context.Background(), p, scope, request); !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("different command = %v", err)
		}
		pOther := principal(other)
		if _, err := m.Create(context.Background(), pOther, other, memory.CreateRequest{Content: "x", Kind: memory.KindPreference, OperationToken: token}); !errors.Is(err, memory.ErrUnauthorized) {
			t.Fatalf("wrong scope = %v", err)
		}
		tamperedFirst := byte('A')
		if token[0] == tamperedFirst {
			tamperedFirst = 'B'
		}
		tampered := string(tamperedFirst) + token[1:]
		if _, err := m.Create(context.Background(), p, scope, memory.CreateRequest{Content: "x", Kind: memory.KindPreference, OperationToken: tampered}); !errors.Is(err, memory.ErrUnauthorized) {
			t.Fatalf("tamper = %v", err)
		}

		oldToken := issue(t, m, p, scope, memory.ActionCreate, time.Hour)
		rotated := newModule(t, h.Store, memory.DefaultLimits(), map[string][]byte{"old": testKeyOld, "new": testKeyNew}, "new")
		if _, err := rotated.Create(context.Background(), p, scope, memory.CreateRequest{Content: "old-key-valid", Kind: memory.KindPreference, OperationToken: oldToken}); err != nil {
			t.Fatalf("retained old key: %v", err)
		}
		removed := newModule(t, h.Store, memory.DefaultLimits(), map[string][]byte{"new": testKeyNew}, "new")
		unusedOld := issue(t, m, p, scope, memory.ActionCreate, time.Hour)
		if _, err := removed.Create(context.Background(), p, scope, memory.CreateRequest{Content: "old-key-rejected", Kind: memory.KindPreference, OperationToken: unusedOld}); !errors.Is(err, memory.ErrUnauthorized) {
			t.Fatalf("removed key = %v", err)
		}

		expiring := issue(t, rotated, p, scope, memory.ActionCreate, time.Minute)
		expiringReq := memory.CreateRequest{Content: "expires", Kind: memory.KindPreference, OperationToken: expiring}
		if _, err := rotated.Create(context.Background(), p, scope, expiringReq); err != nil {
			t.Fatal(err)
		}
		h.Advance(2 * time.Minute)
		if _, err := rotated.Create(context.Background(), p, scope, expiringReq); !errors.Is(err, memory.ErrExpired) {
			t.Fatalf("expired after receipt cleanup = %v", err)
		}

		retractToken := issue(t, rotated, p, scope, memory.ActionRetract, time.Hour)
		retractedFirst, err := rotated.RetractRecord(context.Background(), p, scope, memory.RetractRequest{MemoryID: first.MemoryID, ExpectedRevision: first.Revision, OperationToken: retractToken})
		if err != nil {
			t.Fatal(err)
		}
		afterRetract, err := rotated.Create(context.Background(), p, scope, memory.CreateRequest{Content: "same", Kind: memory.KindPreference, OperationToken: token})
		if err != nil || afterRetract.Content != "" || len(afterRetract.Topics) != 0 {
			t.Fatalf("create replay after retract = %#v, %v", afterRetract, err)
		}
		directPurge(t, h.Store, scope, retractedFirst)
		afterPurge, err := rotated.Create(context.Background(), p, scope, memory.CreateRequest{Content: "same", Kind: memory.KindPreference, OperationToken: token})
		if err != nil || afterPurge.Content != "" || h.ReceiptContentBytes() != 0 {
			t.Fatalf("create replay after purge = %#v, %v", afterPurge, err)
		}
	})

	t.Run("first_write_forget_generation_fence", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		scope := baseScope()
		p := principal(scope)
		m := newModule(t, h.Store, memory.DefaultLimits(), map[string][]byte{"old": testKeyOld}, "old")
		createToken := issue(t, m, p, scope, memory.ActionCreate, time.Hour)
		forgetToken, err := m.IssueForgetToken(context.Background(), p, scope.Subject(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		createdCh := make(chan *memory.Record, 1)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			r, _ := m.Create(context.Background(), p, scope, memory.CreateRequest{Content: "racy", Kind: memory.KindPreference, OperationToken: createToken})
			createdCh <- r
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _ = m.ForgetSubject(context.Background(), p, scope.Subject(), forgetToken)
		}()
		close(start)
		wg.Wait()
		if created := <-createdCh; created != nil {
			if _, err := m.Read(context.Background(), p, scope, created.MemoryID); !errors.Is(err, memory.ErrNotFound) {
				t.Fatalf("record resurrected: %v", err)
			}
		}
		if _, err := m.Create(context.Background(), p, scope, memory.CreateRequest{Content: "racy", Kind: memory.KindPreference, OperationToken: createToken}); !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("pre-forget operation replay = %v", err)
		}
		freshOld := issueForDisabled(t, m, p, scope)
		if freshOld != "" {
			t.Fatalf("disabled subject issued token")
		}
	})

	t.Run("quota_lifecycle_and_audit_atomicity", func(t *testing.T) {
		limits := memory.DefaultLimits()
		limits.MaxRecordsPerScope = 2
		limits.MaxContentPerScope = 12
		h := factory(t, limits)
		scope := baseScope()
		p := principal(scope)
		m := newModule(t, h.Store, limits, map[string][]byte{"old": testKeyOld}, "old")
		one := create(t, m, p, scope, "12345", nil)
		two := create(t, m, p, scope, "67890", nil)
		tok := issue(t, m, p, scope, memory.ActionCreate, time.Hour)
		if _, err := m.Create(context.Background(), p, scope, memory.CreateRequest{Content: "z", Kind: memory.KindPreference, OperationToken: tok}); !errors.Is(err, memory.ErrQuota) {
			t.Fatalf("create over quota = %v", err)
		}
		retract := issue(t, m, p, scope, memory.ActionRetract, time.Hour)
		retractedOne, err := m.RetractRecord(context.Background(), p, scope, memory.RetractRequest{MemoryID: one.MemoryID, ExpectedRevision: one.Revision, OperationToken: retract})
		if err != nil {
			t.Fatal(err)
		}
		tok = issue(t, m, p, scope, memory.ActionCreate, time.Hour)
		if _, err := m.Create(context.Background(), p, scope, memory.CreateRequest{Content: "z", Kind: memory.KindPreference, OperationToken: tok}); !errors.Is(err, memory.ErrQuota) {
			t.Fatalf("retract released physical quota: %v", err)
		}

		h.FailAudit(true)
		auditToken := issue(t, m, p, scope, memory.ActionRevise, time.Hour)
		_, err = m.Revise(context.Background(), p, scope, memory.ReviseRequest{MemoryID: two.MemoryID, ExpectedRevision: two.Revision, Content: "nope", OperationToken: auditToken})
		if !errors.Is(err, memory.ErrAudit) {
			t.Fatalf("audit failure = %v", err)
		}
		h.FailAudit(false)
		// The same command can commit after the injected failure: neither its
		// record change nor its receipt was partially persisted.
		updated, err := m.Revise(context.Background(), p, scope, memory.ReviseRequest{MemoryID: two.MemoryID, ExpectedRevision: two.Revision, Content: "nope", OperationToken: auditToken})
		if err != nil || updated.Content != "nope" {
			t.Fatalf("retry after audit failure = %#v, %v", updated, err)
		}

		reads := h.ContentReads()
		h.Advance(31 * 24 * time.Hour)
		if _, err := m.Read(context.Background(), p, scope, two.MemoryID); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("expired read = %v", err)
		}
		if h.ContentReads() != reads {
			t.Fatal("expired record body was read")
		}
		tok = issue(t, m, p, scope, memory.ActionCreate, time.Hour)
		if _, err := m.Create(context.Background(), p, scope, memory.CreateRequest{Content: "z", Kind: memory.KindPreference, OperationToken: tok}); !errors.Is(err, memory.ErrQuota) {
			t.Fatalf("expiry released physical quota: %v", err)
		}
		directPurge(t, h.Store, scope, retractedOne)
		tok = issue(t, m, p, scope, memory.ActionCreate, time.Hour)
		if _, err := m.Create(context.Background(), p, scope, memory.CreateRequest{Content: "z", Kind: memory.KindPreference, OperationToken: tok}); err != nil {
			t.Fatalf("create after physical purge: %v", err)
		}
	})

	t.Run("trusted_source_and_detached_values", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		scope := baseScope()
		p := principal(scope)
		p.Issuer = "trusted-source"
		m, err := memory.NewModule(memory.ModuleConfig{Store: h.Store, Authorizer: memory.AuthorizerFunc(func(_ context.Context, req memory.AuthorizeRequest) error {
			if req.Principal.Subject != req.Subject.SubjectID {
				return errors.New("actor does not own subject")
			}
			if req.Action == memory.ActionCreateVerifiedFact && req.Principal.Issuer != "trusted-source" {
				return errors.New("verified capability required")
			}
			return nil
		}), Keys: map[string][]byte{"old": testKeyOld}, CurrentKey: "old", Limits: memory.DefaultLimits()})
		if err != nil {
			t.Fatal(err)
		}
		ordinary := issue(t, m, p, scope, memory.ActionCreate, time.Hour)
		if _, err := m.Create(context.Background(), p, scope, memory.CreateRequest{Content: "spoof", Kind: memory.KindVerifiedFact, OperationToken: ordinary}); !errors.Is(err, memory.ErrInvalid) {
			t.Fatalf("verified spoof = %v", err)
		}
		verified := issue(t, m, p, scope, memory.ActionCreateVerifiedFact, time.Hour)
		topics := []string{"one"}
		snapshot, err := h.Store.Snapshot(context.Background(), scope, "")
		if err != nil {
			t.Fatal(err)
		}
		att := memory.SourceAttestation{Scope: scope, SourceRef: "invoice:1", Version: "v1", Digest: "digest", SourceEpoch: 1, ExpiresAt: snapshot.StoreNow.Add(time.Hour), Verifier: "billing"}
		record, err := m.CreateVerifiedFact(context.Background(), p, scope, memory.CreateRequest{Content: "paid", Topics: topics, Kind: memory.KindVerifiedFact, OperationToken: verified}, att)
		if err != nil {
			t.Fatal(err)
		}
		topics[0] = "mutated"
		record.Content = "mutated"
		record.Topics[0] = "mutated"
		stored, err := m.Read(context.Background(), p, scope, record.MemoryID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Content != "paid" || stored.Topics[0] != "one" || stored.Provenance.Producer != "billing" {
			t.Fatalf("stored alias/provenance = %#v", stored)
		}
		for name, mutate := range map[string]func(*memory.SourceAttestation){
			"digest":   func(a *memory.SourceAttestation) { a.Digest = "changed" },
			"verifier": func(a *memory.SourceAttestation) { a.Verifier = "changed" },
			"expiry":   func(a *memory.SourceAttestation) { a.ExpiresAt = a.ExpiresAt.Add(time.Minute) },
		} {
			changed := att
			mutate(&changed)
			if _, err := m.CreateVerifiedFact(context.Background(), p, scope, memory.CreateRequest{Content: "paid", Topics: []string{"one"}, Kind: memory.KindVerifiedFact, OperationToken: verified}, changed); !errors.Is(err, memory.ErrConflict) {
				t.Errorf("same token changed %s = %v", name, err)
			}
		}
		browser := p
		browser.Issuer = "browser-session"
		reviseToken := issue(t, m, browser, scope, memory.ActionRevise, time.Hour)
		if _, err := m.Revise(context.Background(), browser, scope, memory.ReviseRequest{MemoryID: stored.MemoryID, ExpectedRevision: stored.Revision, Content: "forged paid", OperationToken: reviseToken}); !errors.Is(err, memory.ErrUnauthorized) {
			t.Fatalf("ordinary verified revision = %v", err)
		}
		conflictToken := issue(t, m, p, scope, memory.ActionCreateVerifiedFact, time.Hour)
		att.Version = "forged-v2"
		if _, err := m.CreateVerifiedFact(context.Background(), p, scope, memory.CreateRequest{Content: "x", Kind: memory.KindVerifiedFact, OperationToken: conflictToken}, att); !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("source same-epoch change = %v", err)
		}
	})

	t.Run("candidate_confirm_and_authoritative_fences", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		scope := baseScope()
		directCreate(t, h.Store, scope, "active", "old")
		candidate, batchCommand := directCandidateBatchCommand(t, h.Store, scope, memory.CandidateDraft{MemoryID: "candidate", Kind: memory.KindPreference, Content: "new", Retention: time.Hour})
		deduplicated := batchCommand
		deduplicated.Operation.ID = fmt.Sprintf("candidate:%d", operationSequence.Add(1))
		deduplicated.Audit.OperationID = deduplicated.Operation.ID
		replayed, err := h.Store.CommitMutation(context.Background(), deduplicated)
		if err != nil || !replayed.Replayed || len(replayed.Records) != 1 || replayed.Records[0].MemoryID != candidate.Records[0].MemoryID || replayed.Records[0].Content != "" {
			t.Fatalf("same-source candidate dedup = %#v, %v", replayed, err)
		}
		changed := deduplicated
		changed.Operation.ID = fmt.Sprintf("candidate:%d", operationSequence.Add(1))
		changed.Operation.CommandDigest = []byte(changed.Operation.ID)
		changed.Audit.OperationID = changed.Operation.ID
		if _, err := h.Store.CommitMutation(context.Background(), changed); !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("same-source changed candidate batch = %v", err)
		}
		confirmed, err := directConfirm(t, h.Store, scope, candidate.Records[0], "active", 1, 1)
		if err != nil || confirmed.Record == nil || confirmed.Record.Status != memory.StatusActive {
			t.Fatalf("confirm = %#v, %v", confirmed, err)
		}

		staleScope := scope
		staleScope.CollectionID = "proposal-fence"
		active := directCreate(t, h.Store, staleScope, "active", "old")
		stale := directCandidateBatch(t, h.Store, staleScope, memory.CandidateDraft{MemoryID: "candidate", Kind: memory.KindPreference, Content: "new", Retention: time.Hour})
		retractDirect(t, h.Store, staleScope, active.MemoryID, 1)
		if _, err := directConfirm(t, h.Store, staleScope, stale.Records[0], "", 0, 1); !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("proposal epoch fence = %v", err)
		}

		policyScope := scope
		policyScope.ProjectID = "policy-fence"
		stalePolicy := directCandidateBatch(t, h.Store, policyScope, memory.CandidateDraft{MemoryID: "candidate", Kind: memory.KindPreference, Content: "new", Retention: time.Hour})
		updatePolicy(t, h.Store, policyScope)
		if _, err := directConfirm(t, h.Store, policyScope, stalePolicy.Records[0], "", 0, 1); !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("policy fence = %v", err)
		}

		sourceScope := scope
		sourceScope.ProjectID = "source-fence"
		sourced := directCandidateBatch(t, h.Store, sourceScope, memory.CandidateDraft{MemoryID: "candidate", Kind: memory.KindPreference, Content: "new", Retention: time.Hour})
		retractSource(t, h.Store, sourceScope, sourced.Records[0].Provenance.SourceRef, 1)
		if _, err := directConfirm(t, h.Store, sourceScope, sourced.Records[0], "", 0, 1); !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("source fence = %v", err)
		}
	})

	t.Run("revoked_source_cannot_be_revised_visible", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		scope := baseScope()
		candidate := directCandidateBatch(t, h.Store, scope, memory.CandidateDraft{MemoryID: "candidate", Kind: memory.KindPreference, Content: "source value", Retention: time.Hour})
		active, err := directConfirm(t, h.Store, scope, candidate.Records[0], "", 0, 1)
		if err != nil {
			t.Fatal(err)
		}
		p := principal(scope)
		m := newModule(t, h.Store, memory.DefaultLimits(), map[string][]byte{"old": testKeyOld}, "old")
		revise := issue(t, m, p, scope, memory.ActionRevise, time.Hour)
		retractSource(t, h.Store, scope, active.Record.Provenance.SourceRef, 1)
		if _, err := m.Read(context.Background(), p, scope, active.Record.MemoryID); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("revoked source read = %v", err)
		}
		if _, err := m.Revise(context.Background(), p, scope, memory.ReviseRequest{MemoryID: active.Record.MemoryID, ExpectedRevision: active.Record.Revision, Content: "source value", OperationToken: revise}); !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("revoked source revision = %v", err)
		}
		if _, err := m.Read(context.Background(), p, scope, active.Record.MemoryID); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("revoked source became visible = %v", err)
		}
	})

	t.Run("forget_purge_lease_and_new_generation", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		scope := baseScope()
		p := principal(scope)
		m := newModule(t, h.Store, memory.DefaultLimits(), map[string][]byte{"old": testKeyOld}, "old")
		old := create(t, m, p, scope, "old-generation", nil)
		forget, err := m.IssueForgetToken(context.Background(), p, scope.Subject(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		generation, err := m.ForgetSubject(context.Background(), p, scope.Subject(), forget)
		if err != nil || generation != 2 {
			t.Fatalf("forget = %d, %v", generation, err)
		}
		lease := claimPurge(t, h.Store, scope, 1)
		if _, err := tryClaimPurge(t, h.Store, scope, 2); !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("active purge generation cutoff drift = %v", err)
		}
		if _, err := m.IssueOperationToken(context.Background(), p, scope, memory.ActionClaimPurge, time.Hour); err != nil {
			t.Fatalf("disabled subject could not authorize internal cleanup: %v", err)
		}
		enableSubject(t, h.Store, scope)
		newRecord := create(t, m, p, scope, "new-generation", nil)
		purged, err := purgeWithLease(t, h.Store, scope, lease, []memory.PurgeTarget{{MemoryID: old.MemoryID, ExpectedRevision: old.Revision, SelectedBefore: lease.SelectionCutoff}})
		if err != nil || purged.PurgedRecords != 1 {
			t.Fatalf("old generation purge = %#v, %v", purged, err)
		}
		if _, err := m.Read(context.Background(), p, scope, newRecord.MemoryID); err != nil {
			t.Fatalf("new generation was deleted: %v", err)
		}
		wrong := lease
		wrong.OwnerToken = "stale-owner"
		if _, err := purgeWithLease(t, h.Store, scope, wrong, []memory.PurgeTarget{{MemoryID: newRecord.MemoryID, ExpectedRevision: newRecord.Revision, SelectedBefore: lease.SelectionCutoff}}); !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("stale purge owner = %v", err)
		}
		duplicate := memory.PurgeTarget{MemoryID: newRecord.MemoryID, ExpectedRevision: newRecord.Revision, SelectedBefore: purged.PurgeLease.SelectionCutoff}
		if _, err := purgeWithLease(t, h.Store, scope, purged.PurgeLease, []memory.PurgeTarget{duplicate, duplicate}); !errors.Is(err, memory.ErrInvalid) {
			t.Fatalf("duplicate purge target = %v", err)
		}
	})

	t.Run("purged_tombstone_cannot_be_refilled", func(t *testing.T) {
		limits := memory.DefaultLimits()
		limits.MaxRecordsPerScope = 1
		h := factory(t, limits)
		scope := baseScope()
		p := principal(scope)
		m := newModule(t, h.Store, limits, map[string][]byte{"old": testKeyOld}, "old")
		expired := create(t, m, p, scope, "old", nil)
		h.Advance(31 * 24 * time.Hour)
		directPurge(t, h.Store, scope, expired)
		revise := issue(t, m, p, scope, memory.ActionRevise, time.Hour)
		if _, err := m.Revise(context.Background(), p, scope, memory.ReviseRequest{MemoryID: expired.MemoryID, ExpectedRevision: expired.Revision, Content: "refill", OperationToken: revise}); !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("purged tombstone refill = %v", err)
		}
		create(t, m, p, scope, "replacement", nil)
		token := issue(t, m, p, scope, memory.ActionCreate, time.Hour)
		if _, err := m.Create(context.Background(), p, scope, memory.CreateRequest{Content: "over quota", Kind: memory.KindPreference, OperationToken: token}); !errors.Is(err, memory.ErrQuota) {
			t.Fatalf("post-purge quota accounting = %v", err)
		}
	})

	t.Run("expired_purge_takeover_preserves_task_cutoff", func(t *testing.T) {
		h := factory(t, memory.DefaultLimits())
		scope := baseScope()
		p := principal(scope)
		m := newModule(t, h.Store, memory.DefaultLimits(), map[string][]byte{"old": testKeyOld}, "old")
		forget, err := m.IssueForgetToken(context.Background(), p, scope.Subject(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.ForgetSubject(context.Background(), p, scope.Subject(), forget); err != nil {
			t.Fatal(err)
		}
		if _, err := tryClaimPurgeAs(t, h.Store, scope, 1, "task-a", "worker-a"); err != nil {
			t.Fatal(err)
		}
		h.Advance(2 * time.Hour)
		if _, err := tryClaimPurgeAs(t, h.Store, scope, 2, "task-a", "worker-b"); !errors.Is(err, memory.ErrConflict) {
			t.Fatalf("expired same-task cutoff drift = %v", err)
		}
		lease, err := tryClaimPurgeAs(t, h.Store, scope, 2, "task-b", "worker-b")
		if err != nil || lease.PurgeBeforeGeneration != 2 || lease.LeaseEpoch != 1 {
			t.Fatalf("expired new-task takeover = %#v, %v", lease, err)
		}
	})

	t.Run("tombstone_identity_budget_is_not_released_by_purge", func(t *testing.T) {
		limits := memory.DefaultLimits()
		limits.MaxRecordsPerScope = 1
		limits.MaxControlsPerScope = 3
		h := factory(t, limits)
		scope := baseScope()
		p := principal(scope)
		m := newModule(t, h.Store, limits, map[string][]byte{"old": testKeyOld}, "old")
		for i := 0; i < 3; i++ {
			r := create(t, m, p, scope, fmt.Sprintf("value-%d", i), nil)
			token := issue(t, m, p, scope, memory.ActionRetract, time.Hour)
			r, err := m.RetractRecord(context.Background(), p, scope, memory.RetractRequest{MemoryID: r.MemoryID, ExpectedRevision: r.Revision, OperationToken: token})
			if err != nil {
				t.Fatal(err)
			}
			directPurge(t, h.Store, scope, r)
		}
		token := issue(t, m, p, scope, memory.ActionCreate, time.Hour)
		if _, err := m.Create(context.Background(), p, scope, memory.CreateRequest{Content: "bounded", Kind: memory.KindPreference, OperationToken: token}); !errors.Is(err, memory.ErrQuota) {
			t.Fatalf("identity budget = %v", err)
		}
	})
}

func baseScope() memory.Scope {
	return memory.Scope{Namespace: "tenant", ProjectID: "project", SubjectID: "user", CollectionID: "private"}
}
func principal(scope memory.Scope) memory.Principal {
	return memory.Principal{Subject: scope.SubjectID, Namespace: scope.Namespace, Issuer: "test-host", AuthzEpoch: 1}
}

func newModule(t *testing.T, store memory.AtomicStore, limits memory.Limits, keys map[string][]byte, current string) *memory.Module {
	t.Helper()
	m, err := memory.NewModule(memory.ModuleConfig{Store: store, Authorizer: memory.AuthorizerFunc(func(_ context.Context, req memory.AuthorizeRequest) error {
		if req.Principal.Subject != req.Subject.SubjectID {
			return errors.New("actor does not own subject")
		}
		return nil
	}), Keys: keys, CurrentKey: current, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func issue(t *testing.T, m *memory.Module, p memory.Principal, scope memory.Scope, action memory.Action, ttl time.Duration) string {
	t.Helper()
	token, err := m.IssueOperationToken(context.Background(), p, scope, action, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func create(t *testing.T, m *memory.Module, p memory.Principal, scope memory.Scope, content string, topics []string) *memory.Record {
	t.Helper()
	token := issue(t, m, p, scope, memory.ActionCreate, time.Hour)
	r, err := m.Create(context.Background(), p, scope, memory.CreateRequest{Content: content, Topics: topics, Kind: memory.KindPreference, OperationToken: token})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func directCreate(t *testing.T, store memory.AtomicStore, scope memory.Scope, id, content string) memory.Mutation {
	t.Helper()
	cmd, err := tryDirectCreate(t, store, scope, id, content)
	if err != nil {
		t.Fatal(err)
	}
	return cmd
}

func tryDirectCreate(t *testing.T, store memory.AtomicStore, scope memory.Scope, id, content string) (memory.Mutation, error) {
	t.Helper()
	snap, err := store.Snapshot(context.Background(), scope, "")
	if err != nil {
		return memory.Mutation{}, err
	}
	n := operationSequence.Add(1)
	opID := fmt.Sprintf("direct:%d", n)
	cmd := memory.Mutation{Type: memory.MutationCreate, Scope: scope, Subject: scope.Subject(), MemoryID: id, Kind: memory.KindPreference, Content: content, Retention: time.Hour,
		Operation:     memory.Operation{ID: opID, KeyID: "test", Action: memory.ActionCreate, Scope: scope, Subject: scope.Subject(), SubjectGeneration: snap.SubjectGeneration, IssuedAt: snap.StoreNow, ExpiresAt: snap.StoreNow.Add(time.Hour), CommandDigest: []byte(opID)},
		Preconditions: memory.Preconditions{SubjectGeneration: snap.SubjectGeneration, PolicyGeneration: snap.PolicyGeneration, CollectionProposalEpoch: snap.CollectionProposalEpoch}, Audit: memory.AuditEvent{OperationID: opID, Scope: scope, Subject: scope.Subject(), Action: memory.ActionCreate, Actor: scope.SubjectID, Issuer: "contract", MemoryID: id}}
	_, err = store.CommitMutation(context.Background(), cmd)
	return cmd, err
}

func directCandidateBatch(t *testing.T, store memory.AtomicStore, scope memory.Scope, drafts ...memory.CandidateDraft) memory.MutationResult {
	t.Helper()
	result, _ := directCandidateBatchCommand(t, store, scope, drafts...)
	return result
}

func directCandidateBatchCommand(t *testing.T, store memory.AtomicStore, scope memory.Scope, drafts ...memory.CandidateDraft) (memory.MutationResult, memory.Mutation) {
	t.Helper()
	cmd := buildCandidateBatchCommand(t, store, scope, drafts...)
	result, err := store.CommitMutation(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	return result, cmd
}

func buildCandidateBatchCommand(t *testing.T, store memory.AtomicStore, scope memory.Scope, drafts ...memory.CandidateDraft) memory.Mutation {
	t.Helper()
	snap, err := store.Snapshot(context.Background(), scope, "")
	if err != nil {
		t.Fatal(err)
	}
	opID := fmt.Sprintf("candidate:%d", operationSequence.Add(1))
	batchSource := "source"
	batchVersion := "v1"
	if len(drafts) > 0 {
		batchSource = "source:" + drafts[0].MemoryID
	}
	att := &memory.SourceAttestation{Scope: scope, SourceRef: batchSource, Version: batchVersion, Digest: "contract-source", SourceEpoch: 1, ExpiresAt: snap.StoreNow.Add(time.Hour), Verifier: "contract-host"}
	cmd := memory.Mutation{Type: memory.MutationCreateCandidateBatch, Scope: scope, Subject: scope.Subject(), Candidates: drafts, CandidateBatch: &memory.CandidateBatchIdentity{SourceRef: batchSource, SourceVersion: batchVersion, Producer: "extractor", ModelConfigVersion: "test-v1"}, Attestation: att, Operation: memory.Operation{ID: opID, KeyID: "test", Action: memory.ActionPropose, Scope: scope, Subject: scope.Subject(), SubjectGeneration: snap.SubjectGeneration, IssuedAt: snap.StoreNow, ExpiresAt: snap.StoreNow.Add(time.Hour), CommandDigest: []byte(opID)}, Preconditions: memory.Preconditions{SubjectGeneration: snap.SubjectGeneration, PolicyGeneration: snap.PolicyGeneration, CollectionProposalEpoch: snap.CollectionProposalEpoch, SourceEpoch: 1}, Audit: memory.AuditEvent{OperationID: opID, Scope: scope, Subject: scope.Subject(), Action: memory.ActionPropose, Actor: scope.SubjectID, Issuer: "extractor"}}
	return cmd
}

func directConfirm(t *testing.T, store memory.AtomicStore, scope memory.Scope, candidate memory.Record, replacedID string, replacedRevision, sourceEpoch uint64) (memory.MutationResult, error) {
	t.Helper()
	snap, err := store.Snapshot(context.Background(), scope, candidate.Provenance.SourceRef)
	if err != nil {
		t.Fatal(err)
	}
	opID := fmt.Sprintf("confirm:%d", operationSequence.Add(1))
	cmd := memory.Mutation{Type: memory.MutationConfirmCandidate, Scope: scope, Subject: scope.Subject(), MemoryID: candidate.MemoryID, ReplacedMemoryID: replacedID, Operation: memory.Operation{ID: opID, KeyID: "test", Action: memory.ActionConfirm, Scope: scope, Subject: scope.Subject(), SubjectGeneration: candidate.SubjectGeneration, IssuedAt: snap.StoreNow, ExpiresAt: snap.StoreNow.Add(time.Hour), CommandDigest: []byte(opID)}, Preconditions: memory.Preconditions{SubjectGeneration: candidate.SubjectGeneration, ExpectedRevision: candidate.Revision, ExpectedReplacedRevision: replacedRevision, PolicyGeneration: candidate.PolicyGeneration, SourceEpoch: sourceEpoch, CollectionProposalEpoch: candidate.CollectionProposalEpoch}, Audit: memory.AuditEvent{OperationID: opID, Scope: scope, Subject: scope.Subject(), Action: memory.ActionConfirm, Actor: scope.SubjectID, Issuer: "reviewer", MemoryID: candidate.MemoryID}}
	return store.CommitMutation(context.Background(), cmd)
}

func retractDirect(t *testing.T, store memory.AtomicStore, scope memory.Scope, id string, revision uint64) memory.MutationResult {
	t.Helper()
	snap, err := store.Snapshot(context.Background(), scope, "")
	if err != nil {
		t.Fatal(err)
	}
	opID := fmt.Sprintf("retract:%d", operationSequence.Add(1))
	cmd := memory.Mutation{Type: memory.MutationRetract, Scope: scope, Subject: scope.Subject(), MemoryID: id, Operation: memory.Operation{ID: opID, KeyID: "test", Action: memory.ActionRetract, Scope: scope, Subject: scope.Subject(), SubjectGeneration: snap.SubjectGeneration, IssuedAt: snap.StoreNow, ExpiresAt: snap.StoreNow.Add(time.Hour), CommandDigest: []byte(opID)}, Preconditions: memory.Preconditions{SubjectGeneration: snap.SubjectGeneration, ExpectedRevision: revision, PolicyGeneration: snap.PolicyGeneration}, Audit: memory.AuditEvent{OperationID: opID, Scope: scope, Subject: scope.Subject(), Action: memory.ActionRetract, Actor: scope.SubjectID, Issuer: "contract", MemoryID: id}}
	result, err := store.CommitMutation(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func retractSource(t *testing.T, store memory.AtomicStore, scope memory.Scope, sourceRef string, epoch uint64) {
	t.Helper()
	snap, err := store.Snapshot(context.Background(), scope, sourceRef)
	if err != nil {
		t.Fatal(err)
	}
	opID := fmt.Sprintf("source-retract:%d", operationSequence.Add(1))
	cmd := memory.Mutation{Type: memory.MutationRetractSource, Scope: scope, Subject: scope.Subject(), SourceRef: sourceRef, Operation: memory.Operation{ID: opID, KeyID: "test", Action: memory.ActionRetractSource, Scope: scope, Subject: scope.Subject(), SubjectGeneration: snap.SubjectGeneration, IssuedAt: snap.StoreNow, ExpiresAt: snap.StoreNow.Add(time.Hour), CommandDigest: []byte(opID)}, Preconditions: memory.Preconditions{SubjectGeneration: snap.SubjectGeneration, PolicyGeneration: snap.PolicyGeneration, SourceEpoch: epoch}, Audit: memory.AuditEvent{OperationID: opID, Scope: scope, Subject: scope.Subject(), Action: memory.ActionRetractSource, Actor: scope.SubjectID, Issuer: "host"}}
	if _, err := store.CommitMutation(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
}

func updatePolicy(t *testing.T, store memory.AtomicStore, scope memory.Scope) memory.MutationResult {
	t.Helper()
	snap, err := store.Snapshot(context.Background(), scope, "")
	if err != nil {
		t.Fatal(err)
	}
	next := snap.Policy
	next.MaxRecordsPerScope--
	opID := fmt.Sprintf("policy:%d", operationSequence.Add(1))
	cmd := memory.Mutation{Type: memory.MutationUpdatePolicy, Scope: scope, Subject: scope.Subject(), PolicyUpdate: &memory.PolicyUpdate{ExpectedGeneration: snap.PolicyGeneration, Limits: next}, Operation: memory.Operation{ID: opID, KeyID: "test", Action: memory.ActionUpdatePolicy, Scope: scope, Subject: scope.Subject(), SubjectGeneration: snap.SubjectGeneration, IssuedAt: snap.StoreNow, ExpiresAt: snap.StoreNow.Add(time.Hour), CommandDigest: []byte(opID)}, Preconditions: memory.Preconditions{SubjectGeneration: snap.SubjectGeneration, PolicyGeneration: snap.PolicyGeneration}, Audit: memory.AuditEvent{OperationID: opID, Scope: scope, Subject: scope.Subject(), Action: memory.ActionUpdatePolicy, Actor: scope.SubjectID, Issuer: "host"}}
	result, err := store.CommitMutation(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func enableSubject(t *testing.T, store memory.AtomicStore, scope memory.Scope) {
	t.Helper()
	snap, err := store.Snapshot(context.Background(), scope, "")
	if err != nil {
		t.Fatal(err)
	}
	opID := fmt.Sprintf("enable:%d", operationSequence.Add(1))
	cmd := memory.Mutation{Type: memory.MutationEnableSubject, Subject: scope.Subject(), Operation: memory.Operation{ID: opID, KeyID: "test", Action: memory.ActionEnableSubject, Subject: scope.Subject(), SubjectGeneration: snap.SubjectGeneration, IssuedAt: snap.StoreNow, ExpiresAt: snap.StoreNow.Add(time.Hour), CommandDigest: []byte(opID)}, Preconditions: memory.Preconditions{SubjectGeneration: snap.SubjectGeneration, PolicyGeneration: snap.PolicyGeneration}, Audit: memory.AuditEvent{OperationID: opID, Subject: scope.Subject(), Action: memory.ActionEnableSubject, Actor: scope.SubjectID, Issuer: "host"}}
	if _, err := store.CommitMutation(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
}

func claimPurge(t *testing.T, store memory.AtomicStore, scope memory.Scope, cutoff uint64) memory.PurgeLease {
	t.Helper()
	lease, err := tryClaimPurge(t, store, scope, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func tryClaimPurge(t *testing.T, store memory.AtomicStore, scope memory.Scope, cutoff uint64) (memory.PurgeLease, error) {
	t.Helper()
	return tryClaimPurgeAs(t, store, scope, cutoff, "contract-purge", "worker")
}

func tryClaimPurgeAs(t *testing.T, store memory.AtomicStore, scope memory.Scope, cutoff uint64, taskID, owner string) (memory.PurgeLease, error) {
	t.Helper()
	snap, err := store.Snapshot(context.Background(), scope, "")
	if err != nil {
		return memory.PurgeLease{}, err
	}
	opID := fmt.Sprintf("claim:%d", operationSequence.Add(1))
	cmd := memory.Mutation{Type: memory.MutationClaimPurgeLease, Scope: scope, Subject: scope.Subject(), Operation: memory.Operation{ID: opID, KeyID: "test", Action: memory.ActionClaimPurge, Scope: scope, Subject: scope.Subject(), SubjectGeneration: snap.SubjectGeneration, IssuedAt: snap.StoreNow, ExpiresAt: snap.StoreNow.Add(time.Hour), CommandDigest: []byte(opID)}, Preconditions: memory.Preconditions{SubjectGeneration: snap.SubjectGeneration, PolicyGeneration: snap.PolicyGeneration}, PurgeLease: memory.PurgeLease{TaskID: taskID, OwnerToken: owner, PurgeBeforeGeneration: cutoff}, LeaseDuration: time.Hour, Audit: memory.AuditEvent{OperationID: opID, Scope: scope, Subject: scope.Subject(), Action: memory.ActionClaimPurge, Actor: owner, Issuer: "contract"}}
	result, err := store.CommitMutation(context.Background(), cmd)
	if err != nil {
		return memory.PurgeLease{}, err
	}
	return result.PurgeLease, nil
}

func purgeWithLease(t *testing.T, store memory.AtomicStore, scope memory.Scope, lease memory.PurgeLease, targets []memory.PurgeTarget) (memory.MutationResult, error) {
	t.Helper()
	snap, err := store.Snapshot(context.Background(), scope, "")
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].MemoryID < targets[j].MemoryID })
	opID := fmt.Sprintf("purge:%d", operationSequence.Add(1))
	cmd := memory.Mutation{Type: memory.MutationPurge, Scope: scope, Subject: scope.Subject(), PurgeTargets: targets, PurgeCursor: opID, PurgeLease: lease, Operation: memory.Operation{ID: opID, KeyID: "test", Action: memory.ActionPurge, Scope: scope, Subject: scope.Subject(), SubjectGeneration: snap.SubjectGeneration, IssuedAt: snap.StoreNow, ExpiresAt: snap.StoreNow.Add(time.Hour), CommandDigest: []byte(opID)}, Preconditions: memory.Preconditions{SubjectGeneration: snap.SubjectGeneration, PolicyGeneration: snap.PolicyGeneration}, Audit: memory.AuditEvent{OperationID: opID, Scope: scope, Subject: scope.Subject(), Action: memory.ActionPurge, Actor: "worker", Issuer: "contract"}}
	return store.CommitMutation(context.Background(), cmd)
}

func directPurge(t *testing.T, store memory.AtomicStore, scope memory.Scope, records ...*memory.Record) {
	t.Helper()
	snap, err := store.Snapshot(context.Background(), scope, "")
	if err != nil {
		t.Fatal(err)
	}
	claimID := fmt.Sprintf("claim:%d", operationSequence.Add(1))
	claim := memory.Mutation{Type: memory.MutationClaimPurgeLease, Scope: scope, Subject: scope.Subject(), Operation: memory.Operation{ID: claimID, KeyID: "test", Action: memory.ActionClaimPurge, Scope: scope, Subject: scope.Subject(), SubjectGeneration: snap.SubjectGeneration, IssuedAt: snap.StoreNow, ExpiresAt: snap.StoreNow.Add(time.Hour), CommandDigest: []byte(claimID)}, Preconditions: memory.Preconditions{SubjectGeneration: snap.SubjectGeneration, PolicyGeneration: snap.PolicyGeneration}, PurgeLease: memory.PurgeLease{TaskID: "contract-purge", OwnerToken: "worker", PurgeBeforeGeneration: snap.SubjectGeneration}, LeaseDuration: time.Hour, Audit: memory.AuditEvent{OperationID: claimID, Scope: scope, Subject: scope.Subject(), Action: memory.ActionClaimPurge, Actor: "worker", Issuer: "contract"}}
	claimed, err := store.CommitMutation(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	targets := make([]memory.PurgeTarget, 0, len(records))
	for _, record := range records {
		targets = append(targets, memory.PurgeTarget{MemoryID: record.MemoryID, ExpectedRevision: record.Revision, SelectedBefore: claimed.PurgeLease.SelectionCutoff})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].MemoryID < targets[j].MemoryID })
	opID := fmt.Sprintf("purge:%d", operationSequence.Add(1))
	cmd := memory.Mutation{Type: memory.MutationPurge, Scope: scope, Subject: scope.Subject(), PurgeTargets: targets, PurgeCursor: "done", PurgeLease: claimed.PurgeLease, Operation: memory.Operation{ID: opID, KeyID: "test", Action: memory.ActionPurge, Scope: scope, Subject: scope.Subject(), SubjectGeneration: snap.SubjectGeneration, IssuedAt: snap.StoreNow, ExpiresAt: snap.StoreNow.Add(time.Hour), CommandDigest: []byte(opID)}, Preconditions: memory.Preconditions{SubjectGeneration: snap.SubjectGeneration, PolicyGeneration: snap.PolicyGeneration}, Audit: memory.AuditEvent{OperationID: opID, Scope: scope, Subject: scope.Subject(), Action: memory.ActionPurge, Actor: "worker", Issuer: "contract"}}
	first, err := store.CommitMutation(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CommitMutation(context.Background(), cmd)
	if err != nil || !second.Replayed || first.SubjectGeneration != second.SubjectGeneration {
		t.Fatalf("purge replay = %#v, %v", second, err)
	}
}

func issueForDisabled(t *testing.T, m *memory.Module, p memory.Principal, scope memory.Scope) string {
	t.Helper()
	token, err := m.IssueOperationToken(context.Background(), p, scope, memory.ActionCreate, time.Hour)
	if !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("disabled issue = %v", err)
	}
	return token
}

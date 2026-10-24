package memory_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

var (
	proposalScope = memory.Scope{Namespace: "tenant", ProjectID: "project", SubjectID: "subject", CollectionID: "profile"}
	proposalUser  = memory.Principal{Subject: "reviewer", Namespace: "tenant", Issuer: "host-auth"}
	proposalKey   = []byte("0123456789abcdef0123456789abcdef")
)

type proposalModel struct {
	calls atomic.Int64
	mu    sync.Mutex
	opts  models.ModelOptions
	fn    func(context.Context, []models.Message, models.ModelOptions) (*models.ModelResponse, error)
}

type proposalBlockingReadStore struct {
	memory.AtomicStore
	started chan struct{}
	release chan struct{}
}

func (s *proposalBlockingReadStore) ReadRecord(ctx context.Context, scope memory.Scope, memoryID string) (*memory.Record, memory.ControlSnapshot, error) {
	s.started <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, memory.ControlSnapshot{}, ctx.Err()
	}
	return s.AtomicStore.ReadRecord(ctx, scope, memoryID)
}

func (m *proposalModel) Response(ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
	m.calls.Add(1)
	m.mu.Lock()
	m.opts = opts
	m.mu.Unlock()
	if m.fn != nil {
		return m.fn(ctx, messages, opts)
	}
	return &models.ModelResponse{Content: `{"candidates":[{"kind":"preference","content":"likes tea"}]}`}, nil
}

func (*proposalModel) ResponseStream(context.Context, []models.Message, models.ModelOptions) (<-chan models.StreamEvent, error) {
	return nil, errors.New("unexpected streaming call")
}
func (*proposalModel) ID() string                     { return "proposal-test" }
func (*proposalModel) Provider() string               { return "test" }
func (*proposalModel) SupportsNativeJSONSchema() bool { return true }

func newProposalModule(t *testing.T, store memory.AtomicStore, model *proposalModel, limits memory.Limits, authorize memory.AuthorizerFunc, contentPolicy memory.ContentPolicy, outbound memory.ProposalOutboundPolicy) *memory.Module {
	t.Helper()
	if authorize == nil {
		authorize = func(context.Context, memory.AuthorizeRequest) error { return nil }
	}
	if outbound == nil {
		outbound = memory.ProposalOutboundPolicyFunc(func(context.Context, memory.ProposalOutboundRequest) error { return nil })
	}
	extractor, err := memory.NewModelProposalExtractor(model, 256)
	if err != nil {
		t.Fatal(err)
	}
	m, err := memory.NewModule(memory.ModuleConfig{
		Store: store, Authorizer: authorize, ContentPolicy: contentPolicy,
		Proposal: &memory.ProposalConfig{Extractor: extractor, OutboundPolicy: outbound, Producer: "test-extractor", ModelConfigVersion: "prompt-v1"},
		Keys:     map[string][]byte{"test": proposalKey}, CurrentKey: "test", Limits: limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func proposalRequest(t *testing.T, m *memory.Module, content string) memory.ProposalRequest {
	t.Helper()
	token, err := m.IssueOperationToken(context.Background(), proposalUser, proposalScope, memory.ActionPropose, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return memory.ProposalRequest{
		Fragments: []memory.ProposalFragment{{Content: content}}, OperationToken: token,
		Source: memory.SourceAttestation{Scope: proposalScope, SourceRef: "conversation", Version: "turn-1", Digest: "sha256-value", SourceEpoch: 1, ExpiresAt: time.Now().Add(time.Hour), Verifier: "trusted-host"},
	}
}

func TestMemoryProposalDefaultDisabledAndPreModelGuards(t *testing.T) {
	store := in_memory.NewMemoryStore()
	model := &proposalModel{}
	m, err := memory.NewModule(memory.ModuleConfig{Store: store, Authorizer: memory.AuthorizerFunc(func(context.Context, memory.AuthorizeRequest) error { return nil }), Keys: map[string][]byte{"test": proposalKey}, CurrentKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Propose(context.Background(), proposalUser, proposalScope, memory.ProposalRequest{}); !errors.Is(err, memory.ErrUnavailable) {
		t.Fatalf("disabled Propose error = %v", err)
	}
	if model.calls.Load() != 0 || len(store.Audits()) != 0 {
		t.Fatalf("disabled calls=%d audits=%d", model.calls.Load(), len(store.Audits()))
	}

	t.Run("authorization", func(t *testing.T) {
		deniedModel := &proposalModel{}
		denied := newProposalModule(t, in_memory.NewMemoryStore(), deniedModel, memory.DefaultLimits(), func(context.Context, memory.AuthorizeRequest) error { return errors.New("denied") }, nil, nil)
		_, err := denied.Propose(context.Background(), proposalUser, proposalScope, memory.ProposalRequest{})
		if !errors.Is(err, memory.ErrUnauthorized) || deniedModel.calls.Load() != 0 {
			t.Fatalf("error=%v calls=%d", err, deniedModel.calls.Load())
		}
	})

	t.Run("source", func(t *testing.T) {
		sourceModel := &proposalModel{}
		var outboundCalls atomic.Int64
		module := newProposalModule(t, in_memory.NewMemoryStore(), sourceModel, memory.DefaultLimits(), nil, nil, memory.ProposalOutboundPolicyFunc(func(context.Context, memory.ProposalOutboundRequest) error {
			outboundCalls.Add(1)
			return nil
		}))
		req := proposalRequest(t, module, "safe")
		req.Source.Scope.ProjectID = "other"
		_, err := module.Propose(context.Background(), proposalUser, proposalScope, req)
		if !errors.Is(err, memory.ErrInvalid) || sourceModel.calls.Load() != 0 || outboundCalls.Load() != 0 {
			t.Fatalf("error=%v model calls=%d outbound calls=%d", err, sourceModel.calls.Load(), outboundCalls.Load())
		}
	})

	t.Run("credential", func(t *testing.T) {
		sensitiveModel := &proposalModel{}
		sensitiveStore := in_memory.NewMemoryStore()
		var outboundCalls atomic.Int64
		module := newProposalModule(t, sensitiveStore, sensitiveModel, memory.DefaultLimits(), nil, nil, memory.ProposalOutboundPolicyFunc(func(context.Context, memory.ProposalOutboundRequest) error {
			outboundCalls.Add(1)
			return nil
		}))
		req := proposalRequest(t, module, "Authorization: Bearer secret-value")
		_, err := module.Propose(context.Background(), proposalUser, proposalScope, req)
		if !errors.Is(err, memory.ErrInvalid) || sensitiveModel.calls.Load() != 0 || outboundCalls.Load() != 0 || len(sensitiveStore.Audits()) != 0 {
			t.Fatalf("error=%v model calls=%d outbound calls=%d audits=%d", err, sensitiveModel.calls.Load(), outboundCalls.Load(), len(sensitiveStore.Audits()))
		}
	})

	t.Run("host content and outbound policy", func(t *testing.T) {
		policyModel := &proposalModel{}
		module := newProposalModule(t, in_memory.NewMemoryStore(), policyModel, memory.DefaultLimits(), nil, memory.ContentPolicyFunc(func(context.Context, memory.ContentPolicyRequest) error { return errors.New("sensitive") }), nil)
		_, err := module.Propose(context.Background(), proposalUser, proposalScope, proposalRequest(t, module, "safe"))
		if !errors.Is(err, memory.ErrInvalid) || policyModel.calls.Load() != 0 {
			t.Fatalf("content policy error=%v calls=%d", err, policyModel.calls.Load())
		}

		outboundModel := &proposalModel{}
		module = newProposalModule(t, in_memory.NewMemoryStore(), outboundModel, memory.DefaultLimits(), nil, nil, memory.ProposalOutboundPolicyFunc(func(context.Context, memory.ProposalOutboundRequest) error { return errors.New("no egress") }))
		_, err = module.Propose(context.Background(), proposalUser, proposalScope, proposalRequest(t, module, "safe"))
		if !errors.Is(err, memory.ErrUnauthorized) || outboundModel.calls.Load() != 0 {
			t.Fatalf("outbound error=%v calls=%d", err, outboundModel.calls.Load())
		}
	})
}

func TestProposeResolvedGatesTrustedResolverBeforeHostRead(t *testing.T) {
	limits := memory.DefaultLimits()
	limits.MaxProposeConcurrent = 1
	store := in_memory.NewMemoryStore()
	model := &proposalModel{}
	module := newProposalModule(t, store, model, limits, nil, nil, nil)
	var resolverCalls atomic.Int64
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	resolver := memory.ProposalSourceResolverFunc(func(ctx context.Context, _ memory.Principal, scope memory.Scope, ref string) (memory.SourceAttestation, []memory.ProposalFragment, error) {
		resolverCalls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return memory.SourceAttestation{}, nil, ctx.Err()
		}
		return memory.SourceAttestation{Scope: scope, SourceRef: ref, Version: "v1", Digest: "digest", SourceEpoch: 1, ExpiresAt: time.Now().Add(time.Hour), Verifier: "host"}, []memory.ProposalFragment{{Content: "safe"}}, nil
	})
	issue := func() string {
		token, err := module.IssueOperationToken(context.Background(), proposalUser, proposalScope, memory.ActionPropose, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	firstToken := issue()
	firstDone := make(chan error, 1)
	go func() {
		_, err := module.ProposeResolved(context.Background(), proposalUser, proposalScope, "run-1", firstToken, resolver)
		firstDone <- err
	}()
	<-started
	_, err := module.ProposeResolved(context.Background(), proposalUser, proposalScope, "run-2", issue(), resolver)
	if !errors.Is(err, memory.ErrOverloaded) || resolverCalls.Load() != 1 || model.calls.Load() != 0 || store.ContentReadCount() != 0 {
		t.Fatalf("overload error=%v resolver=%d model=%d reads=%d", err, resolverCalls.Load(), model.calls.Load(), store.ContentReadCount())
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}

	before := resolverCalls.Load()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = module.ProposeResolved(cancelled, proposalUser, proposalScope, "run-3", issue(), resolver)
	if !errors.Is(err, context.Canceled) || resolverCalls.Load() != before {
		t.Fatalf("cancelled error=%v resolver=%d want=%d", err, resolverCalls.Load(), before)
	}

	deniedModel := &proposalModel{}
	denied := newProposalModule(t, in_memory.NewMemoryStore(), deniedModel, memory.DefaultLimits(), func(_ context.Context, req memory.AuthorizeRequest) error {
		if req.Action == memory.ActionPropose {
			return errors.New("denied")
		}
		return nil
	}, nil, nil)
	deniedToken, err := module.IssueOperationToken(context.Background(), proposalUser, proposalScope, memory.ActionPropose, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, err = denied.ProposeResolved(context.Background(), proposalUser, proposalScope, "run-4", deniedToken, resolver)
	if !errors.Is(err, memory.ErrUnauthorized) || resolverCalls.Load() != before || deniedModel.calls.Load() != 0 {
		t.Fatalf("denied error=%v resolver=%d model=%d", err, resolverCalls.Load(), deniedModel.calls.Load())
	}

	timeoutPolicy := memory.DefaultLimits()
	timeoutPolicy.ProposeTimeout = 5 * time.Millisecond
	timeoutStore := in_memory.NewMemoryStore().SetLimits(timeoutPolicy)
	timeoutModel := &proposalModel{}
	var timeoutContentCalls atomic.Int64
	timeoutModule := newProposalModule(t, timeoutStore, timeoutModel, memory.DefaultLimits(), nil, memory.ContentPolicyFunc(func(context.Context, memory.ContentPolicyRequest) error {
		timeoutContentCalls.Add(1)
		return nil
	}), nil)
	timeoutToken, err := timeoutModule.IssueOperationToken(context.Background(), proposalUser, proposalScope, memory.ActionPropose, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var timeoutResolverCalls atomic.Int64
	timeoutResolver := memory.ProposalSourceResolverFunc(func(_ context.Context, _ memory.Principal, scope memory.Scope, ref string) (memory.SourceAttestation, []memory.ProposalFragment, error) {
		timeoutResolverCalls.Add(1)
		time.Sleep(20 * time.Millisecond) // deliberately ignores cancellation and returns success late
		return memory.SourceAttestation{Scope: scope, SourceRef: ref, Version: "v1", Digest: "digest", SourceEpoch: 1, ExpiresAt: time.Now().Add(time.Hour), Verifier: "host"}, []memory.ProposalFragment{{Content: "safe"}}, nil
	})
	_, err = timeoutModule.ProposeResolved(context.Background(), proposalUser, proposalScope, "run-timeout", timeoutToken, timeoutResolver)
	if !errors.Is(err, context.DeadlineExceeded) || timeoutResolverCalls.Load() != 1 || timeoutContentCalls.Load() != 0 || timeoutModel.calls.Load() != 0 || len(timeoutStore.Audits()) != 0 {
		t.Fatalf("timeout error=%v resolver=%d content=%d model=%d audits=%d", err, timeoutResolverCalls.Load(), timeoutContentCalls.Load(), timeoutModel.calls.Load(), len(timeoutStore.Audits()))
	}
}

func TestProposeResolvedReservesGlobalCapacityAcrossResolverAndCommit(t *testing.T) {
	newResolver := func(started, release chan struct{}, calls *atomic.Int64) memory.ProposalSourceResolver {
		return memory.ProposalSourceResolverFunc(func(ctx context.Context, _ memory.Principal, scope memory.Scope, ref string) (memory.SourceAttestation, []memory.ProposalFragment, error) {
			calls.Add(1)
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return memory.SourceAttestation{}, nil, ctx.Err()
			}
			return memory.SourceAttestation{Scope: scope, SourceRef: ref, Version: "v1", Digest: "digest", SourceEpoch: 1, ExpiresAt: time.Now().Add(time.Hour), Verifier: "host"}, []memory.ProposalFragment{{Content: "safe"}}, nil
		})
	}

	t.Run("full before proposal performs no host or model work", func(t *testing.T) {
		inner := in_memory.NewMemoryStore()
		blocking := &proposalBlockingReadStore{AtomicStore: inner, started: make(chan struct{}, 1), release: make(chan struct{})}
		limits := memory.DefaultLimits()
		limits.MaxConcurrentOps = 1
		model := &proposalModel{}
		module := newProposalModule(t, blocking, model, limits, nil, nil, nil)
		token, err := module.IssueOperationToken(context.Background(), proposalUser, proposalScope, memory.ActionPropose, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			_, _ = module.Read(context.Background(), proposalUser, proposalScope, "occupied")
		}()
		<-blocking.started
		var resolverCalls atomic.Int64
		_, err = module.ProposeResolved(context.Background(), proposalUser, proposalScope, "run", token, memory.ProposalSourceResolverFunc(func(context.Context, memory.Principal, memory.Scope, string) (memory.SourceAttestation, []memory.ProposalFragment, error) {
			resolverCalls.Add(1)
			return memory.SourceAttestation{}, nil, nil
		}))
		if !errors.Is(err, memory.ErrOverloaded) || resolverCalls.Load() != 0 || model.calls.Load() != 0 {
			t.Fatalf("error=%v resolver=%d model=%d", err, resolverCalls.Load(), model.calls.Load())
		}
		close(blocking.release)
		<-readDone
	})

	t.Run("reserved proposal commits while remaining capacity is occupied", func(t *testing.T) {
		inner := in_memory.NewMemoryStore()
		blocking := &proposalBlockingReadStore{AtomicStore: inner, started: make(chan struct{}, 1), release: make(chan struct{})}
		limits := memory.DefaultLimits()
		limits.MaxConcurrentOps = 2
		model := &proposalModel{}
		module := newProposalModule(t, blocking, model, limits, nil, nil, nil)
		token, err := module.IssueOperationToken(context.Background(), proposalUser, proposalScope, memory.ActionPropose, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		resolverStarted, resolverRelease := make(chan struct{}, 1), make(chan struct{})
		var resolverCalls atomic.Int64
		proposalDone := make(chan error, 1)
		go func() {
			_, proposeErr := module.ProposeResolved(context.Background(), proposalUser, proposalScope, "run", token, newResolver(resolverStarted, resolverRelease, &resolverCalls))
			proposalDone <- proposeErr
		}()
		<-resolverStarted
		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			_, _ = module.Read(context.Background(), proposalUser, proposalScope, "occupied")
		}()
		<-blocking.started
		close(resolverRelease)
		select {
		case err := <-proposalDone:
			if err != nil || resolverCalls.Load() != 1 || model.calls.Load() != 1 {
				t.Fatalf("error=%v resolver=%d model=%d", err, resolverCalls.Load(), model.calls.Load())
			}
		case <-time.After(time.Second):
			t.Fatal("proposal attempted to reacquire occupied global capacity")
		}
		close(blocking.release)
		<-readDone
	})
}

func TestMemoryProposalHardCeilingsCannotBeConfiguredAway(t *testing.T) {
	for _, mutate := range []func(*memory.Limits){
		func(v *memory.Limits) { v.MaxProposeConcurrent = 5 },
		func(v *memory.Limits) { v.MaxProposeInputBytes = (32 << 10) + 1 },
		func(v *memory.Limits) { v.MaxProposeCandidates = 9 },
		func(v *memory.Limits) { v.ProposeTimeout = 10*time.Second + 1 },
	} {
		limits := memory.DefaultLimits()
		mutate(&limits)
		_, err := memory.NewModule(memory.ModuleConfig{Store: in_memory.NewMemoryStore(), Authorizer: memory.AuthorizerFunc(func(context.Context, memory.AuthorizeRequest) error { return nil }), Keys: map[string][]byte{"test": proposalKey}, CurrentKey: "test", Limits: limits})
		if !errors.Is(err, memory.ErrInvalid) {
			t.Fatalf("loosened limits=%#v error=%v", limits, err)
		}
	}
}

func TestMemoryProposalUsesTighterPersistedPreModelPolicy(t *testing.T) {
	t.Run("input", func(t *testing.T) {
		policy := memory.DefaultLimits()
		policy.MaxProposeInputBytes = 4
		store := in_memory.NewMemoryStore().SetLimits(policy)
		model := &proposalModel{}
		module := newProposalModule(t, store, model, memory.DefaultLimits(), nil, nil, nil)
		_, err := module.Propose(context.Background(), proposalUser, proposalScope, proposalRequest(t, module, "12345"))
		if !errors.Is(err, memory.ErrInvalid) || model.calls.Load() != 0 {
			t.Fatalf("persisted input policy error=%v calls=%d", err, model.calls.Load())
		}
	})

	t.Run("deadline", func(t *testing.T) {
		policy := memory.DefaultLimits()
		policy.ProposeTimeout = 5 * time.Millisecond
		store := in_memory.NewMemoryStore().SetLimits(policy)
		model := &proposalModel{fn: func(ctx context.Context, _ []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		module := newProposalModule(t, store, model, memory.DefaultLimits(), nil, nil, nil)
		started := time.Now()
		_, err := module.Propose(context.Background(), proposalUser, proposalScope, proposalRequest(t, module, "safe"))
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 250*time.Millisecond {
			t.Fatalf("persisted deadline error=%v elapsed=%s", err, time.Since(started))
		}
	})
}

func TestMemoryProposalExistingSourceMustMatchBeforeModel(t *testing.T) {
	store := in_memory.NewMemoryStore()
	model := &proposalModel{}
	m := newProposalModule(t, store, model, memory.DefaultLimits(), nil, nil, nil)
	att := memory.SourceAttestation{Scope: proposalScope, SourceRef: "controlled-source", Version: "v1", Digest: "digest-v1", SourceEpoch: 1, ExpiresAt: time.Now().UTC().Truncate(time.Second).Add(time.Hour), Verifier: "trusted-host"}
	createToken, err := m.IssueOperationToken(context.Background(), proposalUser, proposalScope, memory.ActionCreateVerifiedFact, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateVerifiedFact(context.Background(), proposalUser, proposalScope, memory.CreateRequest{Content: "source anchor", Kind: memory.KindVerifiedFact, OperationToken: createToken}, att); err != nil {
		t.Fatal(err)
	}

	for name, mutate := range map[string]func(*memory.SourceAttestation){
		"version":  func(a *memory.SourceAttestation) { a.Version = "v2" },
		"digest":   func(a *memory.SourceAttestation) { a.Digest = "other-digest" },
		"verifier": func(a *memory.SourceAttestation) { a.Verifier = "other-verifier" },
		"expiry":   func(a *memory.SourceAttestation) { a.ExpiresAt = a.ExpiresAt.Add(time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := att
			mutate(&candidate)
			token, err := m.IssueOperationToken(context.Background(), proposalUser, proposalScope, memory.ActionPropose, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			_, err = m.Propose(context.Background(), proposalUser, proposalScope, memory.ProposalRequest{Fragments: []memory.ProposalFragment{{Content: "must not leave process"}}, Source: candidate, OperationToken: token})
			if !errors.Is(err, memory.ErrConflict) || model.calls.Load() != 0 {
				t.Fatalf("error=%v model calls=%d", err, model.calls.Load())
			}
		})
	}

	snapshot, err := store.Snapshot(context.Background(), proposalScope, att.SourceRef)
	if err != nil {
		t.Fatal(err)
	}
	revoke := memory.Mutation{
		Type: memory.MutationRetractSource, Scope: proposalScope, Subject: proposalScope.Subject(), SourceRef: att.SourceRef,
		Operation:     memory.Operation{ID: "revoke-source", KeyID: "test", Action: memory.ActionRetractSource, Scope: proposalScope, Subject: proposalScope.Subject(), SubjectGeneration: snapshot.SubjectGeneration, IssuedAt: snapshot.StoreNow, ExpiresAt: snapshot.StoreNow.Add(time.Hour), CommandDigest: []byte("revoke-source")},
		Preconditions: memory.Preconditions{SubjectGeneration: snapshot.SubjectGeneration, PolicyGeneration: snapshot.PolicyGeneration, SourceEpoch: att.SourceEpoch},
		Audit:         memory.AuditEvent{OperationID: "revoke-source", Scope: proposalScope, Subject: proposalScope.Subject(), Action: memory.ActionRetractSource, Actor: proposalUser.Subject, Issuer: proposalUser.Issuer},
	}
	if _, err := store.CommitMutation(context.Background(), revoke); err != nil {
		t.Fatal(err)
	}
	revoked := att
	revoked.SourceEpoch++
	token, err := m.IssueOperationToken(context.Background(), proposalUser, proposalScope, memory.ActionPropose, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Propose(context.Background(), proposalUser, proposalScope, memory.ProposalRequest{Fragments: []memory.ProposalFragment{{Content: "still must not leave"}}, Source: revoked, OperationToken: token})
	if !errors.Is(err, memory.ErrConflict) || model.calls.Load() != 0 {
		t.Fatalf("revoked error=%v model calls=%d", err, model.calls.Load())
	}
}

func TestMemoryProposalBoundsSchemaAtomicityAndDedup(t *testing.T) {
	store := in_memory.NewMemoryStore()
	model := &proposalModel{}
	m := newProposalModule(t, store, model, memory.DefaultLimits(), nil, nil, nil)

	exact := strings.Repeat("x", memory.DefaultLimits().MaxProposeInputBytes)
	result, err := m.Propose(context.Background(), proposalUser, proposalScope, proposalRequest(t, m, exact))
	if err != nil || len(result.Candidates) != 1 || result.Candidates[0].Status != memory.StatusCandidate {
		t.Fatalf("exact input result=%#v error=%v", result, err)
	}
	if model.calls.Load() != 1 {
		t.Fatalf("exact input model calls=%d", model.calls.Load())
	}
	tooLarge := proposalRequest(t, m, exact+"x")
	if _, err := m.Propose(context.Background(), proposalUser, proposalScope, tooLarge); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("oversize error=%v", err)
	}
	if model.calls.Load() != 1 {
		t.Fatalf("oversize reached model, calls=%d", model.calls.Load())
	}

	eight := make([]string, 8)
	for i := range eight {
		eight[i] = fmt.Sprintf(`{"kind":"preference","content":"value-%d"}`, i)
	}
	model.fn = func(context.Context, []models.Message, models.ModelOptions) (*models.ModelResponse, error) {
		return &models.ModelResponse{Content: `{"candidates":[` + strings.Join(eight, ",") + `]}`}, nil
	}
	req := proposalRequest(t, m, "another source excerpt")
	req.Source.SourceRef, req.Source.Version = "conversation-2", "turn-2"
	batch, err := m.Propose(context.Background(), proposalUser, proposalScope, req)
	if err != nil || len(batch.Candidates) != 8 {
		t.Fatalf("eight candidates=%d error=%v", len(batch.Candidates), err)
	}

	nine := append(append([]string(nil), eight...), `{"kind":"preference","content":"value-9"}`)
	model.fn = func(context.Context, []models.Message, models.ModelOptions) (*models.ModelResponse, error) {
		return &models.ModelResponse{Content: `{"candidates":[` + strings.Join(nine, ",") + `]}`}, nil
	}
	req = proposalRequest(t, m, "third source excerpt")
	req.Source.SourceRef, req.Source.Version = "conversation-3", "turn-3"
	if _, err := m.Propose(context.Background(), proposalUser, proposalScope, req); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("nine candidates error=%v", err)
	}
	model.fn = func(context.Context, []models.Message, models.ModelOptions) (*models.ModelResponse, error) {
		return &models.ModelResponse{Content: `{"candidates":[],"padding":"` + strings.Repeat("x", 128<<10) + `"}`}, nil
	}
	req = proposalRequest(t, m, "oversize output")
	req.Source.SourceRef, req.Source.Version = "conversation-output", "turn-output"
	if _, err := m.Propose(context.Background(), proposalUser, proposalScope, req); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("oversize output error=%v", err)
	}

	beforeAudits := len(store.Audits())
	model.fn = func(context.Context, []models.Message, models.ModelOptions) (*models.ModelResponse, error) {
		return &models.ModelResponse{Content: `{"candidates":[{"kind":"preference","content":"valid"},{"kind":"preference","content":"API_KEY=do-not-store"}]}`}, nil
	}
	req = proposalRequest(t, m, "fourth source excerpt")
	req.Source.SourceRef, req.Source.Version = "conversation-4", "turn-4"
	if _, err := m.Propose(context.Background(), proposalUser, proposalScope, req); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("partly invalid error=%v", err)
	}
	if len(store.Audits()) != beforeAudits {
		t.Fatalf("partly invalid batch appended audit: before=%d after=%d", beforeAudits, len(store.Audits()))
	}

	model.fn = nil
	dedupReq := proposalRequest(t, m, "deduplicate this")
	dedupReq.Source.SourceRef, dedupReq.Source.Version = "conversation-dedup", "turn-dedup"
	first, err := m.Propose(context.Background(), proposalUser, proposalScope, dedupReq)
	if err != nil {
		t.Fatal(err)
	}
	dedupReq.OperationToken, err = m.IssueOperationToken(context.Background(), proposalUser, proposalScope, memory.ActionPropose, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Propose(context.Background(), proposalUser, proposalScope, dedupReq)
	if err != nil || !second.Replayed || len(first.Candidates) != 1 || first.Candidates[0].MemoryID != second.Candidates[0].MemoryID {
		t.Fatalf("dedup first=%#v second=%#v error=%v", first, second, err)
	}

	model.mu.Lock()
	opts := model.opts
	model.mu.Unlock()
	if len(opts.Tools) != 0 || opts.ToolChoice == nil || opts.ToolChoice.Mode != models.ToolChoiceNone || opts.MaxTokens == nil || *opts.MaxTokens != 256 || opts.ResponseFormat == nil || opts.ResponseFormat.Type != models.ResponseFormatJSONSchema {
		t.Fatalf("unsafe/unbounded model options=%#v", opts)
	}
}

func TestMemoryProposalConcurrencyDeadlineAndCancellationReclaim(t *testing.T) {
	limits := memory.DefaultLimits()
	limits.ProposeTimeout = 200 * time.Millisecond
	started := make(chan struct{}, 8)
	model := &proposalModel{fn: func(ctx context.Context, _ []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	m := newProposalModule(t, in_memory.NewMemoryStore(), model, limits, nil, nil, nil)

	contexts := make([]context.CancelFunc, 4)
	results := make(chan error, 4)
	for i := 0; i < 4; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		contexts[i] = cancel
		req := proposalRequest(t, m, fmt.Sprintf("input-%d", i))
		req.Source.Version = fmt.Sprintf("turn-%d", i)
		go func() {
			_, err := m.Propose(ctx, proposalUser, proposalScope, req)
			results <- err
		}()
	}
	for i := 0; i < 4; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("four model calls did not start")
		}
	}
	overload := proposalRequest(t, m, "overload")
	overload.Source.Version = "turn-overload"
	if _, err := m.Propose(context.Background(), proposalUser, proposalScope, overload); !errors.Is(err, memory.ErrOverloaded) {
		t.Fatalf("fifth proposal error=%v", err)
	}
	contexts[0]()
	if err := <-results; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled proposal error=%v", err)
	}
	reclaimedCtx, reclaimedCancel := context.WithCancel(context.Background())
	reclaimed := proposalRequest(t, m, "reclaimed")
	reclaimed.Source.Version = "turn-reclaimed"
	go func() {
		_, err := m.Propose(reclaimedCtx, proposalUser, proposalScope, reclaimed)
		results <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("canceled slot was not reclaimed")
	}
	reclaimedCancel()
	for i := 1; i < len(contexts); i++ {
		contexts[i]()
	}
	for i := 0; i < 4; i++ {
		<-results
	}

	deadlineModel := &proposalModel{fn: func(ctx context.Context, _ []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	short := memory.DefaultLimits()
	short.ProposeTimeout = time.Millisecond
	deadlineModule := newProposalModule(t, in_memory.NewMemoryStore(), deadlineModel, short, nil, nil, nil)
	if _, err := deadlineModule.Propose(context.Background(), proposalUser, proposalScope, proposalRequest(t, deadlineModule, "deadline")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline error=%v", err)
	}
}

func TestMemoryProposalForgetWhileModelRunsCannotWrite(t *testing.T) {
	store := in_memory.NewMemoryStore()
	started := make(chan struct{})
	release := make(chan struct{})
	model := &proposalModel{fn: func(ctx context.Context, _ []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
		close(started)
		select {
		case <-release:
			return &models.ModelResponse{Content: `{"candidates":[{"kind":"preference","content":"stale candidate"}]}`}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	module := newProposalModule(t, store, model, memory.DefaultLimits(), nil, nil, nil)
	req := proposalRequest(t, module, "in flight")
	result := make(chan error, 1)
	go func() {
		_, err := module.Propose(context.Background(), proposalUser, proposalScope, req)
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("model did not start")
	}
	forgetToken, err := module.IssueForgetToken(context.Background(), proposalUser, proposalScope.Subject(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := module.ForgetSubject(context.Background(), proposalUser, proposalScope.Subject(), forgetToken); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("proposal after ForgetSubject error=%v", err)
	}
	for _, audit := range store.Audits() {
		if audit.Action == memory.ActionPropose {
			t.Fatalf("stale proposal appended audit=%#v", audit)
		}
	}
}

func TestMemoryProposalStrictDTOAndConfirmAuthorization(t *testing.T) {
	store := in_memory.NewMemoryStore()
	model := &proposalModel{fn: func(context.Context, []models.Message, models.ModelOptions) (*models.ModelResponse, error) {
		return &models.ModelResponse{Content: `{"candidates":[{"kind":"preference","content":"value","scope":{"namespace":"other"}}]}`}, nil
	}}
	m := newProposalModule(t, store, model, memory.DefaultLimits(), nil, nil, nil)
	if _, err := m.Propose(context.Background(), proposalUser, proposalScope, proposalRequest(t, m, "strict")); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("cross-scope model DTO error=%v", err)
	}
	if len(store.Audits()) != 0 {
		t.Fatalf("invalid DTO wrote audits=%d", len(store.Audits()))
	}

	model.fn = nil
	createToken, err := m.IssueOperationToken(context.Background(), proposalUser, proposalScope, memory.ActionCreate, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	target, err := m.Create(context.Background(), proposalUser, proposalScope, memory.CreateRequest{Content: "old active value", Kind: memory.KindPreference, OperationToken: createToken})
	if err != nil {
		t.Fatal(err)
	}
	proposed, err := m.Propose(context.Background(), proposalUser, proposalScope, proposalRequest(t, m, "confirm"))
	if err != nil {
		t.Fatal(err)
	}
	candidate := proposed.Candidates[0]
	confirmToken, err := m.IssueOperationToken(context.Background(), proposalUser, proposalScope, memory.ActionConfirm, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	denied := newProposalModule(t, store, model, memory.DefaultLimits(), func(_ context.Context, req memory.AuthorizeRequest) error {
		if req.Action == memory.ActionConfirm {
			return errors.New("not a reviewer")
		}
		return nil
	}, nil, nil)
	_, err = denied.Confirm(context.Background(), proposalUser, proposalScope, memory.ConfirmRequest{MemoryID: candidate.MemoryID, ExpectedRevision: candidate.Revision, SourceRef: candidate.Provenance.SourceRef, SourceEpoch: candidate.Provenance.SourceEpoch, OperationToken: confirmToken})
	if !errors.Is(err, memory.ErrUnauthorized) {
		t.Fatalf("independent confirm authorization error=%v", err)
	}
	wrongTarget := memory.ConfirmRequest{MemoryID: candidate.MemoryID, ExpectedRevision: candidate.Revision, ReplacedMemoryID: target.MemoryID, ExpectedReplacedRevision: target.Revision + 1, SourceRef: candidate.Provenance.SourceRef, SourceEpoch: candidate.Provenance.SourceEpoch, OperationToken: confirmToken}
	if _, err := m.Confirm(context.Background(), proposalUser, proposalScope, wrongTarget); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("stale replacement revision error=%v", err)
	}
	correct := wrongTarget
	correct.ExpectedReplacedRevision = target.Revision
	active, err := m.Confirm(context.Background(), proposalUser, proposalScope, correct)
	if err != nil || active.Status != memory.StatusActive || active.Revision != candidate.Revision+1 {
		t.Fatalf("confirmed record=%#v error=%v", active, err)
	}
	if _, err := m.Read(context.Background(), proposalUser, proposalScope, target.MemoryID); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("replaced record remained active: %v", err)
	}
}

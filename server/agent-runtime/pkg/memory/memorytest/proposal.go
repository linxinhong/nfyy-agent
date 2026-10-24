package memorytest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

// ProposalFenceState is the durable state which a rejected proposal must not
// partially change. Adapter tests populate it directly from their SQL tables.
type ProposalFenceState struct {
	CandidateRecords int
	ProposalAudits   int
	ProposalReceipts int
	ConfirmAudits    int
	ConfirmReceipts  int
	PhysicalRecords  int
	PhysicalBytes    int
	CandidateBatches int
}

type ProposalFenceHarness struct {
	Primary    memory.AtomicStore
	Concurrent memory.AtomicStore
	State      func(context.Context, memory.Scope) (ProposalFenceState, error)
}

type proposalCommitBarrier struct {
	memory.AtomicStore
	Reached chan struct{}
	Release chan struct{}
}

func (s *proposalCommitBarrier) CommitMutation(ctx context.Context, cmd memory.Mutation) (memory.MutationResult, error) {
	if cmd.Type == memory.MutationCreateCandidateBatch || cmd.Type == memory.MutationConfirmCandidate {
		close(s.Reached)
		select {
		case <-s.Release:
		case <-ctx.Done():
			return memory.MutationResult{}, ctx.Err()
		}
	}
	return s.AtomicStore.CommitMutation(ctx, cmd)
}

type fixedProposalExtractor struct{}

func (fixedProposalExtractor) Extract(context.Context, memory.ProposalExtractionRequest) (memory.ProposalExtractionResult, error) {
	return memory.ProposalExtractionResult{Candidates: []memory.CandidateDTO{{Kind: memory.KindPreference, Content: "candidate-after-precheck"}}}, nil
}

// ProposalFenceContract proves with two independent adapter connections that
// a control change after Module's final snapshot and before SQL commit is
// rejected by the transaction without candidate/audit/receipt/quota residue.
func ProposalFenceContract(t *testing.T, factory func(*testing.T) ProposalFenceHarness) {
	t.Helper()
	for _, fence := range []string{"source-revoke", "policy-tighten", "record-retract"} {
		t.Run("propose/"+fence, func(t *testing.T) {
			h := factory(t)
			if h.Primary == nil || h.Concurrent == nil || h.State == nil {
				t.Fatal("incomplete proposal fence harness")
			}
			barrier := &proposalCommitBarrier{AtomicStore: h.Primary, Reached: make(chan struct{}), Release: make(chan struct{})}
			module := proposalContractModule(t, barrier)
			scope := memory.Scope{Namespace: "proposal", ProjectID: "project-" + fence, SubjectID: "subject", CollectionID: "private"}
			principal := memory.Principal{Subject: "reviewer", Namespace: scope.Namespace, Issuer: "proposal-contract"}
			initial, err := h.Primary.Snapshot(context.Background(), scope, "source")
			if err != nil {
				t.Fatal(err)
			}
			att := memory.SourceAttestation{Scope: scope, SourceRef: "source", Version: "v1", Digest: "source-digest", SourceEpoch: 1, ExpiresAt: initial.StoreNow.Add(time.Hour), Verifier: "trusted-host"}
			createToken := proposalContractToken(t, module, principal, scope, memory.ActionCreateVerifiedFact)
			anchor, err := module.CreateVerifiedFact(context.Background(), principal, scope, memory.CreateRequest{Content: "active anchor", Kind: memory.KindVerifiedFact, OperationToken: createToken}, att)
			if err != nil {
				t.Fatal(err)
			}
			proposeToken := proposalContractToken(t, module, principal, scope, memory.ActionPropose)
			result := make(chan error, 1)
			go func() {
				_, err := module.Propose(context.Background(), principal, scope, memory.ProposalRequest{Fragments: []memory.ProposalFragment{{Content: "bounded input"}}, Source: att, OperationToken: proposeToken})
				result <- err
			}()
			select {
			case <-barrier.Reached:
			case <-time.After(5 * time.Second):
				t.Fatal("proposal did not reach the commit barrier")
			}

			switch fence {
			case "source-revoke":
				retractSource(t, h.Concurrent, scope, att.SourceRef, att.SourceEpoch)
			case "policy-tighten":
				updatePolicy(t, h.Concurrent, scope)
			case "record-retract":
				retractDirect(t, h.Concurrent, scope, anchor.MemoryID, anchor.Revision)
			}
			before, err := h.State(context.Background(), scope)
			if err != nil {
				t.Fatal(err)
			}
			close(barrier.Release)
			if err := <-result; !errors.Is(err, memory.ErrConflict) {
				t.Fatalf("proposal after %s error = %v", fence, err)
			}
			after, err := h.State(context.Background(), scope)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("proposal left partial state after %s: before=%+v after=%+v", fence, before, after)
			}
		})
		t.Run("confirm/"+fence, func(t *testing.T) {
			h := factory(t)
			if h.Primary == nil || h.Concurrent == nil || h.State == nil {
				t.Fatal("incomplete proposal fence harness")
			}
			scope := memory.Scope{Namespace: "confirm", ProjectID: "project-" + fence, SubjectID: "subject", CollectionID: "private"}
			principal := memory.Principal{Subject: "reviewer", Namespace: scope.Namespace, Issuer: "proposal-contract"}
			module := proposalContractModule(t, h.Primary)
			initial, err := h.Primary.Snapshot(context.Background(), scope, "source")
			if err != nil {
				t.Fatal(err)
			}
			att := memory.SourceAttestation{Scope: scope, SourceRef: "source", Version: "v1", Digest: "source-digest", SourceEpoch: 1, ExpiresAt: initial.StoreNow.Add(time.Hour), Verifier: "trusted-host"}
			createToken := proposalContractToken(t, module, principal, scope, memory.ActionCreateVerifiedFact)
			anchor, err := module.CreateVerifiedFact(context.Background(), principal, scope, memory.CreateRequest{Content: "active anchor", Kind: memory.KindVerifiedFact, OperationToken: createToken}, att)
			if err != nil {
				t.Fatal(err)
			}
			proposeToken := proposalContractToken(t, module, principal, scope, memory.ActionPropose)
			proposed, err := module.Propose(context.Background(), principal, scope, memory.ProposalRequest{Fragments: []memory.ProposalFragment{{Content: "bounded input"}}, Source: att, OperationToken: proposeToken})
			if err != nil || len(proposed.Candidates) != 1 {
				t.Fatalf("proposal setup=%#v error=%v", proposed, err)
			}
			candidate := proposed.Candidates[0]
			barrier := &proposalCommitBarrier{AtomicStore: h.Primary, Reached: make(chan struct{}), Release: make(chan struct{})}
			confirmModule := proposalContractModule(t, barrier)
			confirmToken := proposalContractToken(t, confirmModule, principal, scope, memory.ActionConfirm)
			result := make(chan error, 1)
			go func() {
				_, err := confirmModule.Confirm(context.Background(), principal, scope, memory.ConfirmRequest{MemoryID: candidate.MemoryID, ExpectedRevision: candidate.Revision, SourceRef: candidate.Provenance.SourceRef, SourceEpoch: candidate.Provenance.SourceEpoch, OperationToken: confirmToken})
				result <- err
			}()
			select {
			case <-barrier.Reached:
			case <-time.After(5 * time.Second):
				t.Fatal("confirm did not reach the commit barrier")
			}
			switch fence {
			case "source-revoke":
				retractSource(t, h.Concurrent, scope, att.SourceRef, att.SourceEpoch)
			case "policy-tighten":
				updatePolicy(t, h.Concurrent, scope)
			case "record-retract":
				retractDirect(t, h.Concurrent, scope, anchor.MemoryID, anchor.Revision)
			}
			before, err := h.State(context.Background(), scope)
			if err != nil {
				t.Fatal(err)
			}
			close(barrier.Release)
			if err := <-result; !errors.Is(err, memory.ErrConflict) {
				t.Fatalf("confirm after %s error = %v", fence, err)
			}
			after, err := h.State(context.Background(), scope)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("confirm left partial state after %s: before=%+v after=%+v", fence, before, after)
			}
		})
	}
}

func proposalContractModule(t *testing.T, store memory.AtomicStore) *memory.Module {
	t.Helper()
	m, err := memory.NewModule(memory.ModuleConfig{
		Store:      store,
		Authorizer: memory.AuthorizerFunc(func(context.Context, memory.AuthorizeRequest) error { return nil }),
		Proposal: &memory.ProposalConfig{
			Extractor:      fixedProposalExtractor{},
			OutboundPolicy: memory.ProposalOutboundPolicyFunc(func(context.Context, memory.ProposalOutboundRequest) error { return nil }),
			Producer:       "contract-extractor", ModelConfigVersion: "v1",
		},
		Keys: map[string][]byte{"test": []byte("0123456789abcdef0123456789abcdef")}, CurrentKey: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func proposalContractToken(t *testing.T, module *memory.Module, principal memory.Principal, scope memory.Scope, action memory.Action) string {
	t.Helper()
	token, err := module.IssueOperationToken(context.Background(), principal, scope, action, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

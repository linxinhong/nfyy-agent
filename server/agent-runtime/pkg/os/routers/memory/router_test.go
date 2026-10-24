package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	mem "github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

var testMemoryKey = []byte("0123456789abcdef0123456789abcdef")

func newHTTPMemory(t *testing.T) (*in_memory.MemoryStore, http.Handler) {
	t.Helper()
	store := in_memory.NewMemoryStore()
	module, err := mem.NewModule(mem.ModuleConfig{
		Store: store,
		Authorizer: mem.AuthorizerFunc(func(_ context.Context, req mem.AuthorizeRequest) error {
			if req.Scope != (mem.Scope{}) && req.Principal.Namespace != req.Scope.Namespace {
				return mem.ErrUnauthorized
			}
			return nil
		}),
		Keys: map[string][]byte{"current": testMemoryKey}, CurrentKey: "current",
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, NewRouter(Config{Module: module, ProjectID: "project", CollectionID: "profile"})
}

func memoryHTTPRequest(handler http.Handler, principal *execution.Principal, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if principal != nil {
		req = req.WithContext(service.ContextWithPrincipal(req.Context(), *principal))
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func issueHTTPToken(t *testing.T, handler http.Handler, principal execution.Principal, action mem.Action) string {
	t.Helper()
	response := memoryHTTPRequest(handler, &principal, http.MethodPost, "/operations", `{"action":"`+string(action)+`"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("issue %s: status=%d body=%s", action, response.Code, response.Body.String())
	}
	var body struct {
		Token string `json:"operation_token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Token == "" {
		t.Fatalf("decode token: %v body=%s", err, response.Body.String())
	}
	return body.Token
}

func createHTTPMemory(t *testing.T, handler http.Handler, principal execution.Principal, token, content string) mem.Record {
	t.Helper()
	encoded, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"content":`+string(encoded)+`,"kind":"preference","topics":["profile"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", token)
	req = req.WithContext(service.ContextWithPrincipal(req.Context(), principal))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var record mem.Record
	if err := json.Unmarshal(recorder.Body.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestMemoryHTTP(t *testing.T) {
	store, handler := newHTTPMemory(t)
	alice := execution.Principal{Subject: "alice", Namespace: "tenant", Issuer: "issuer"}
	bob := execution.Principal{Subject: "bob", Namespace: "tenant", Issuer: "issuer"}

	t.Run("disabled and missing authority never fall back or read content", func(t *testing.T) {
		disabled := memoryHTTPRequest(NewRouter(Config{}), nil, http.MethodGet, "/legacy-id", "")
		if disabled.Code != http.StatusServiceUnavailable {
			t.Fatalf("disabled status=%d body=%s", disabled.Code, disabled.Body.String())
		}
		before := store.ContentReadCount()
		denied := memoryHTTPRequest(handler, nil, http.MethodGet, "/secret", "")
		if denied.Code != http.StatusUnauthorized || store.ContentReadCount() != before {
			t.Fatalf("denied status=%d reads=%d", denied.Code, store.ContentReadCount()-before)
		}
	})

	t.Run("strict DTO rejects browser authority and provenance", func(t *testing.T) {
		for _, body := range []string{
			`{"content":"x","kind":"preference","user_id":"bob"}`,
			`{"content":"x","kind":"preference","project_id":"other"}`,
			`{"content":"x","kind":"preference","collection_id":"other"}`,
			`{"content":"x","kind":"verified_fact","verified":true}`,
			`{"content":"x","kind":"preference","producer":"browser"}`,
			`{"content":"x","kind":"preference","decision":{"actor":"browser"}}`,
			`{"content":"x","kind":"preference","source":{"source_ref":"browser"}}`,
			`{"content":"x","kind":"preference","policy":{"max_records":999999}}`,
		} {
			response := memoryHTTPRequest(handler, &alice, http.MethodPost, "/", body)
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("body=%s status=%d response=%s", body, response.Code, response.Body.String())
			}
		}
	})

	createToken := issueHTTPToken(t, handler, alice, mem.ActionCreate)
	record := createHTTPMemory(t, handler, alice, createToken, "alice-private")
	if record.Scope.SubjectID != alice.Subject || record.Scope.ProjectID != "project" || record.Scope.CollectionID != "profile" {
		t.Fatalf("server scope=%+v", record.Scope)
	}

	t.Run("same id is not visible across subject", func(t *testing.T) {
		response := memoryHTTPRequest(handler, &bob, http.MethodGet, "/"+record.MemoryID, "")
		if response.Code != http.StatusNotFound {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("idempotency and action binding conflict explicitly", func(t *testing.T) {
		replay := createHTTPMemory(t, handler, alice, createToken, "alice-private")
		if replay.MemoryID != record.MemoryID {
			t.Fatalf("replay id=%s want=%s", replay.MemoryID, record.MemoryID)
		}
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"content":"different","kind":"preference"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", createToken)
		req = req.WithContext(service.ContextWithPrincipal(req.Context(), alice))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusConflict {
			t.Fatalf("changed replay status=%d body=%s", response.Code, response.Body.String())
		}
		wrongAction := httptest.NewRequest(http.MethodPatch, "/"+record.MemoryID, strings.NewReader(`{"expected_revision":1,"content":"changed"}`))
		wrongAction.Header.Set("Content-Type", "application/json")
		wrongAction.Header.Set("Idempotency-Key", createToken)
		wrongAction = wrongAction.WithContext(service.ContextWithPrincipal(wrongAction.Context(), alice))
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, wrongAction)
		if response.Code != http.StatusForbidden {
			t.Fatalf("action mismatch status=%d body=%s", response.Code, response.Body.String())
		}
	})

	_ = createHTTPMemory(t, handler, alice, issueHTTPToken(t, handler, alice, mem.ActionCreate), "alice-second")
	var aliceCursor string
	t.Run("cursor is scope bound", func(t *testing.T) {
		response := memoryHTTPRequest(handler, &alice, http.MethodGet, "/?limit=1", "")
		if response.Code != http.StatusOK {
			t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
		}
		var page struct {
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || page.NextCursor == "" {
			t.Fatalf("cursor=%q err=%v body=%s", page.NextCursor, err, response.Body.String())
		}
		aliceCursor = page.NextCursor
		wrong := memoryHTTPRequest(handler, &bob, http.MethodGet, "/?limit=1&cursor="+page.NextCursor, "")
		if wrong.Code != http.StatusNotFound {
			t.Fatalf("wrong cursor status=%d body=%s", wrong.Code, wrong.Body.String())
		}
		tampered := memoryHTTPRequest(handler, &alice, http.MethodGet, "/?limit=1&cursor="+page.NextCursor+"x", "")
		if tampered.Code != http.StatusForbidden {
			t.Fatalf("tampered cursor status=%d body=%s", tampered.Code, tampered.Body.String())
		}
	})

	t.Run("revision preconditions and forget", func(t *testing.T) {
		revise := httptest.NewRequest(http.MethodPatch, "/"+record.MemoryID, strings.NewReader(`{"expected_revision":1,"content":"revised","topics":["profile"]}`))
		revise.Header.Set("Content-Type", "application/json")
		revise.Header.Set("Idempotency-Key", issueHTTPToken(t, handler, alice, mem.ActionRevise))
		revise = revise.WithContext(service.ContextWithPrincipal(revise.Context(), alice))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, revise)
		if response.Code != http.StatusOK || response.Header().Get("ETag") != `"2"` {
			t.Fatalf("revise status=%d etag=%q body=%s", response.Code, response.Header().Get("ETag"), response.Body.String())
		}
		stale := httptest.NewRequest(http.MethodDelete, "/"+record.MemoryID, nil)
		stale.Header.Set("If-Match", `"1"`)
		stale.Header.Set("Idempotency-Key", issueHTTPToken(t, handler, alice, mem.ActionRetract))
		stale = stale.WithContext(service.ContextWithPrincipal(stale.Context(), alice))
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, stale)
		if response.Code != http.StatusConflict {
			t.Fatalf("stale delete status=%d body=%s", response.Code, response.Body.String())
		}
		forget := httptest.NewRequest(http.MethodPost, "/forget", nil)
		forget.Header.Set("Idempotency-Key", issueHTTPToken(t, handler, alice, mem.ActionForgetSubject))
		forget = forget.WithContext(service.ContextWithPrincipal(forget.Context(), alice))
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, forget)
		if response.Code != http.StatusOK {
			t.Fatalf("forget status=%d body=%s", response.Code, response.Body.String())
		}
		staleCursor := memoryHTTPRequest(handler, &alice, http.MethodGet, "/?limit=1&cursor="+aliceCursor, "")
		if staleCursor.Code != http.StatusConflict {
			t.Fatalf("old generation cursor status=%d body=%s", staleCursor.Code, staleCursor.Body.String())
		}
		read := memoryHTTPRequest(handler, &alice, http.MethodGet, "/"+record.MemoryID, "")
		if read.Code != http.StatusNotFound {
			t.Fatalf("post-forget read status=%d", read.Code)
		}
	})
}

type httpProposalExtractor struct{ calls atomic.Int64 }

func (e *httpProposalExtractor) Extract(context.Context, mem.ProposalExtractionRequest) (mem.ProposalExtractionResult, error) {
	e.calls.Add(1)
	return mem.ProposalExtractionResult{Candidates: []mem.CandidateDTO{{Kind: mem.KindPreference, Content: "likes tea", Topics: []string{"profile"}}}}, nil
}

type httpSourceResolver struct {
	calls atomic.Int64
	now   func() time.Time
}

func (r *httpSourceResolver) ResolveProposalSource(_ context.Context, _ mem.Principal, scope mem.Scope, sourceRef string) (mem.SourceAttestation, []mem.ProposalFragment, error) {
	r.calls.Add(1)
	now := time.Now().UTC()
	if r.now != nil {
		now = r.now()
	}
	return mem.SourceAttestation{
		Scope: scope, SourceRef: sourceRef, Version: "turn-1", Digest: "digest-1",
		SourceEpoch: 1, ExpiresAt: now.Add(time.Hour), Verifier: "trusted-host",
	}, []mem.ProposalFragment{{Content: "user said they like tea"}}, nil
}

func TestMemoryHTTPProposalAndConfirmUseTrustedNarrowInputs(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	extractor := &httpProposalExtractor{}
	resolver := &httpSourceResolver{now: func() time.Time { return now }}
	store := in_memory.NewMemoryStore().SetClock(func() time.Time { return now })
	var denyConfirm atomic.Bool
	module, err := mem.NewModule(mem.ModuleConfig{
		Store: store,
		Authorizer: mem.AuthorizerFunc(func(_ context.Context, req mem.AuthorizeRequest) error {
			if req.Action == mem.ActionConfirm && denyConfirm.Load() {
				return errors.New("confirm denied")
			}
			return nil
		}),
		Proposal: &mem.ProposalConfig{
			Extractor:      extractor,
			OutboundPolicy: mem.ProposalOutboundPolicyFunc(func(context.Context, mem.ProposalOutboundRequest) error { return nil }),
			Producer:       "http-test", ModelConfigVersion: "v1",
		},
		Keys: map[string][]byte{"current": testMemoryKey}, CurrentKey: "current",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewRouter(Config{Module: module, ProjectID: "project", CollectionID: "profile", ProposeEnabled: true, SourceResolver: resolver})
	alice := execution.Principal{Subject: "alice", Namespace: "tenant", Issuer: "issuer"}

	spoofed := memoryHTTPRequest(handler, &alice, http.MethodPost, "/propose", `{"source_ref":"run-1","source_epoch":99,"fragments":[{"content":"browser"}]}`)
	if spoofed.Code != http.StatusUnprocessableEntity || resolver.calls.Load() != 0 || extractor.calls.Load() != 0 {
		t.Fatalf("spoof status=%d resolver=%d extractor=%d body=%s", spoofed.Code, resolver.calls.Load(), extractor.calls.Load(), spoofed.Body.String())
	}
	missing := memoryHTTPRequest(handler, nil, http.MethodPost, "/propose", `{"source_ref":"run-1"}`)
	if missing.Code != http.StatusUnauthorized || resolver.calls.Load() != 0 || extractor.calls.Load() != 0 {
		t.Fatalf("missing authority status=%d resolver=%d extractor=%d", missing.Code, resolver.calls.Load(), extractor.calls.Load())
	}
	noToken := memoryHTTPRequest(handler, &alice, http.MethodPost, "/propose", `{"source_ref":"run-1"}`)
	if noToken.Code != http.StatusForbidden || resolver.calls.Load() != 0 || extractor.calls.Load() != 0 || store.ContentReadCount() != 0 || len(store.Audits()) != 0 {
		t.Fatalf("missing token status=%d resolver=%d extractor=%d reads=%d audits=%d", noToken.Code, resolver.calls.Load(), extractor.calls.Load(), store.ContentReadCount(), len(store.Audits()))
	}
	wrongToken := httptest.NewRequest(http.MethodPost, "/propose", strings.NewReader(`{"source_ref":"run-1"}`))
	wrongToken.Header.Set("Content-Type", "application/json")
	wrongToken.Header.Set("Idempotency-Key", issueHTTPToken(t, handler, alice, mem.ActionCreate))
	wrongToken = wrongToken.WithContext(service.ContextWithPrincipal(wrongToken.Context(), alice))
	wrongTokenResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongTokenResponse, wrongToken)
	if wrongTokenResponse.Code != http.StatusForbidden || resolver.calls.Load() != 0 || extractor.calls.Load() != 0 || store.ContentReadCount() != 0 || len(store.Audits()) != 0 {
		t.Fatalf("wrong token status=%d resolver=%d extractor=%d reads=%d audits=%d", wrongTokenResponse.Code, resolver.calls.Load(), extractor.calls.Load(), store.ContentReadCount(), len(store.Audits()))
	}

	propose := httptest.NewRequest(http.MethodPost, "/propose", strings.NewReader(`{"source_ref":"run-1"}`))
	propose.Header.Set("Content-Type", "application/json")
	propose.Header.Set("Idempotency-Key", issueHTTPToken(t, handler, alice, mem.ActionPropose))
	propose = propose.WithContext(service.ContextWithPrincipal(propose.Context(), alice))
	proposed := httptest.NewRecorder()
	handler.ServeHTTP(proposed, propose)
	if proposed.Code != http.StatusCreated || resolver.calls.Load() != 1 || extractor.calls.Load() != 1 {
		t.Fatalf("propose status=%d resolver=%d extractor=%d body=%s", proposed.Code, resolver.calls.Load(), extractor.calls.Load(), proposed.Body.String())
	}
	var result mem.ProposalResult
	if err := json.Unmarshal(proposed.Body.Bytes(), &result); err != nil || len(result.Candidates) != 1 {
		t.Fatalf("proposal=%+v err=%v", result, err)
	}
	candidate := result.Candidates[0]
	confirmToken := issueHTTPToken(t, handler, alice, mem.ActionConfirm)
	denyConfirm.Store(true)
	deniedConfirm := httptest.NewRequest(http.MethodPost, "/"+candidate.MemoryID+"/confirm", strings.NewReader(`{"expected_revision":1,"source_ref":"run-1","source_epoch":1}`))
	deniedConfirm.Header.Set("Content-Type", "application/json")
	deniedConfirm.Header.Set("Idempotency-Key", confirmToken)
	deniedConfirm = deniedConfirm.WithContext(service.ContextWithPrincipal(deniedConfirm.Context(), alice))
	deniedConfirmResponse := httptest.NewRecorder()
	handler.ServeHTTP(deniedConfirmResponse, deniedConfirm)
	if deniedConfirmResponse.Code != http.StatusForbidden || extractor.calls.Load() != 1 {
		t.Fatalf("denied confirm status=%d extractor=%d body=%s", deniedConfirmResponse.Code, extractor.calls.Load(), deniedConfirmResponse.Body.String())
	}
	denyConfirm.Store(false)
	confirm := httptest.NewRequest(http.MethodPost, "/"+candidate.MemoryID+"/confirm", strings.NewReader(`{"expected_revision":1,"source_ref":"run-1","source_epoch":1}`))
	confirm.Header.Set("Content-Type", "application/json")
	confirm.Header.Set("Idempotency-Key", confirmToken)
	confirm = confirm.WithContext(service.ContextWithPrincipal(confirm.Context(), alice))
	confirmed := httptest.NewRecorder()
	handler.ServeHTTP(confirmed, confirm)
	if confirmed.Code != http.StatusOK || confirmed.Header().Get("ETag") != `"2"` {
		t.Fatalf("confirm status=%d etag=%q body=%s", confirmed.Code, confirmed.Header().Get("ETag"), confirmed.Body.String())
	}
	staleConfirm := httptest.NewRequest(http.MethodPost, "/"+candidate.MemoryID+"/confirm", strings.NewReader(`{"expected_revision":1,"source_ref":"run-1","source_epoch":1}`))
	staleConfirm.Header.Set("Content-Type", "application/json")
	staleConfirm.Header.Set("Idempotency-Key", issueHTTPToken(t, handler, alice, mem.ActionConfirm))
	staleConfirm = staleConfirm.WithContext(service.ContextWithPrincipal(staleConfirm.Context(), alice))
	staleConfirmResponse := httptest.NewRecorder()
	handler.ServeHTTP(staleConfirmResponse, staleConfirm)
	if staleConfirmResponse.Code != http.StatusConflict {
		t.Fatalf("stale confirm status=%d body=%s", staleConfirmResponse.Code, staleConfirmResponse.Body.String())
	}
	expiringToken := issueHTTPToken(t, handler, alice, mem.ActionPropose)
	auditsBeforeExpiry := len(store.Audits())
	now = now.Add(2 * time.Hour)
	expired := httptest.NewRequest(http.MethodPost, "/propose", strings.NewReader(`{"source_ref":"run-1"}`))
	expired.Header.Set("Content-Type", "application/json")
	expired.Header.Set("Idempotency-Key", expiringToken)
	expired = expired.WithContext(service.ContextWithPrincipal(expired.Context(), alice))
	expiredResponse := httptest.NewRecorder()
	handler.ServeHTTP(expiredResponse, expired)
	if expiredResponse.Code != http.StatusGone || resolver.calls.Load() != 1 || extractor.calls.Load() != 1 || len(store.Audits()) != auditsBeforeExpiry {
		t.Fatalf("expired status=%d resolver=%d extractor=%d audits=%d body=%s", expiredResponse.Code, resolver.calls.Load(), extractor.calls.Load(), len(store.Audits()), expiredResponse.Body.String())
	}
}

func TestMemoryHTTPBoundaryMappings(t *testing.T) {
	maxMilliseconds := int64(math.MaxInt64 / int64(time.Millisecond))
	if got, err := httpRetention(0); err != nil || got != 0 {
		t.Fatalf("zero retention=%s err=%v", got, err)
	}
	if got, err := httpRetention(maxMilliseconds); err != nil || got != time.Duration(maxMilliseconds)*time.Millisecond {
		t.Fatalf("max retention=%s err=%v", got, err)
	}
	for _, value := range []int64{-1, math.MinInt64, maxMilliseconds + 1, math.MaxInt64} {
		if _, err := httpRetention(value); !errors.Is(err, mem.ErrInvalid) {
			t.Fatalf("retention %d error=%v", value, err)
		}
	}

	store := in_memory.NewMemoryStore()
	module, err := mem.NewModule(mem.ModuleConfig{
		Store: store, Authorizer: mem.AuthorizerFunc(func(context.Context, mem.AuthorizeRequest) error { return nil }),
		Keys: map[string][]byte{"current": testMemoryKey}, CurrentKey: "current",
	})
	if err != nil {
		t.Fatal(err)
	}
	alice := execution.Principal{Subject: "alice", Namespace: "tenant", Issuer: "issuer"}
	badScope := memoryHTTPRequest(NewRouter(Config{Module: module, CollectionID: "profile"}), &alice, http.MethodGet, "/record", "")
	if badScope.Code != http.StatusServiceUnavailable {
		t.Fatalf("invalid server scope status=%d body=%s", badScope.Code, badScope.Body.String())
	}

	response := httptest.NewRecorder()
	respondMemoryError(response, mem.ErrCommitUnknown)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"commit_unknown"`) {
		t.Fatalf("commit unknown status=%d body=%s", response.Code, response.Body.String())
	}
	typedNil := NewRouter(Config{Module: module, ProjectID: "project", CollectionID: "profile", ProposeEnabled: true, SourceResolver: mem.ProposalSourceResolverFunc(nil)})
	proposal := memoryHTTPRequest(typedNil, &alice, http.MethodPost, "/propose", `{"source_ref":"run-1"}`)
	if proposal.Code != http.StatusServiceUnavailable {
		t.Fatalf("typed nil resolver status=%d body=%s", proposal.Code, proposal.Body.String())
	}
}

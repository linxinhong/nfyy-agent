package main_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gateway "enterprise-agent-platform/apps/gateway"
	"enterprise-agent-platform/packages/apikey"
	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/governance"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/skillreg"
	"enterprise-agent-platform/packages/store"

	"enterprise-agent-platform/packages/token"
	"github.com/jackc/pgx/v5"
)

// fakeStore implements gateway.ControlStore in memory with an injectable
// audit writer so tests can assert audit evidence and its failure path.
type groupBinding struct {
	idpGroup    string
	WorkspaceID string
	Roles       []string
}

type fakeStore struct {
	audit.Writer

	users         map[string]store.User
	keys          map[string]*store.APIKey // by hash
	byID          map[string]*store.APIKey
	members       map[string][]store.Membership
	groupBindings []groupBinding // mapped group -> workspace roles
	failWrite     error          // audit write failure injection
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		Writer:  &audit.MemoryWriter{},
		users:   map[string]store.User{},
		keys:    map[string]*store.APIKey{},
		byID:    map[string]*store.APIKey{},
		members: map[string][]store.Membership{},
	}
}

func (f *fakeStore) seedUser(id string) store.User {
	u := store.User{ID: id, Email: id + "@e.io", DisplayName: "Dev", OrganizationID: "org_default", DepartmentID: "dept_platform"}
	f.users[id] = u
	return u
}

func (f *fakeStore) seedKey(id, userID string) string {
	g, err := apikey.Generate(apikey.EnvLive)
	if err != nil {
		panic(err)
	}
	k := &store.APIKey{KeyID: id, UserID: userID, Prefix: g.Prefix, Name: "test", CreatedAt: time.Now()}
	f.keys[apikey.Hash(g.Full)] = k
	f.byID[id] = k
	return g.Full
}

func (f *fakeStore) UpsertUser(_ context.Context, c store.IDClaims) (store.User, error) {
	u := store.User{ID: "usr_" + c.Subject, Email: c.Email, DisplayName: c.DisplayName, OrganizationID: c.OrganizationID, DepartmentID: c.DepartmentID}
	f.users[u.ID] = u
	return u, nil
}

func (f *fakeStore) GetUser(_ context.Context, id string) (store.User, error) {
	u, ok := f.users[id]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return u, nil
}

func (f *fakeStore) IsMember(_ context.Context, userID, ws string) (identity.Membership, error) {
	for _, m := range f.members[userID] {
		if m.WorkspaceID == ws {
			return identity.Membership{Member: true, Roles: m.Roles}, nil
		}
	}
	return identity.Membership{}, nil
}

func (f *fakeStore) ListMemberships(_ context.Context, userID string) ([]store.Membership, error) {
	return f.members[userID], nil
}

func (f *fakeStore) SyncMemberships(_ context.Context, userID string, wsIDs []string, roles []string) error {
	out := make([]store.Membership, 0, len(wsIDs))
	for _, ws := range wsIDs {
		out = append(out, store.Membership{WorkspaceID: ws, Roles: roles})
	}
	f.members[userID] = out
	return nil
}

func (f *fakeStore) ListDLPRulesAll(_ context.Context) ([]store.AdminDLPRule, error) {
	return nil, nil
}

func (f *fakeStore) UpdateDLPRuleAction(_ context.Context, _, _ string, _ bool, _ []string) (store.AdminDLPRule, error) {
	return store.AdminDLPRule{}, nil
}

func (f *fakeStore) TokenizeDLP(_ context.Context, _, _, _ string) (string, error) {
	return "tok-test", nil
}

func (f *fakeStore) RevealDLP(_ context.Context, _, _ string) (string, string, error) {
	return "", "", pgx.ErrNoRows
}

func (f *fakeStore) GetLocalCredential(_ context.Context, _ string) (string, string, error) {
	return "", "", pgx.ErrNoRows
}

func (f *fakeStore) UpsertLocalCredential(_ context.Context, _, _, _ string) error {
	return nil
}

func (f *fakeStore) RequireLocalPasswordChange(_ context.Context, _ string) error { return nil }

func (f *fakeStore) LocalPasswordChangeRequired(_ context.Context, _ string) (bool, error) {
	return false, nil
}

func (f *fakeStore) ResolveGroupBindings(_ context.Context, groups []string) ([]store.GroupBinding, error) {
	bindings := map[string]*store.GroupBinding{}
	for _, g := range groups {
		for _, b := range f.groupBindings {
			if b.idpGroup == g {
				existing, ok := bindings[b.WorkspaceID]
				if !ok {
					existing = &store.GroupBinding{WorkspaceID: b.WorkspaceID, Roles: []string{}}
					bindings[b.WorkspaceID] = existing
				}
				seen := map[string]bool{}
				for _, r := range existing.Roles {
					seen[r] = true
				}
				for _, r := range b.Roles {
					if !seen[r] {
						existing.Roles = append(existing.Roles, r)
						seen[r] = true
					}
				}
			}
		}
	}
	out := make([]store.GroupBinding, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, *b)
	}
	return out, nil
}

func (f *fakeStore) SyncGroupBindings(_ context.Context, userID string, bindings []store.GroupBinding) error {
	keep := map[string]bool{}
	out := make([]store.Membership, 0, len(bindings))
	for _, b := range bindings {
		keep[b.WorkspaceID] = true
		out = append(out, store.Membership{WorkspaceID: b.WorkspaceID, Roles: b.Roles})
	}
	for uid, members := range f.members {
		filtered := members[:0]
		for _, m := range members {
			if keep[m.WorkspaceID] {
				filtered = append(filtered, m)
			}
		}
		f.members[uid] = filtered
	}
	f.members[userID] = append(f.members[userID], out...)
	return nil
}

func (f *fakeStore) CreateAPIKey(_ context.Context, userID, keyHash, prefix, name, workspaceID string, expiresAt *time.Time, _ audit.Event) (store.APIKey, error) {
	k := &store.APIKey{KeyID: "key_" + keyHash[:8], UserID: userID, Prefix: prefix, Name: name, CreatedAt: time.Now(), ExpiresAt: expiresAt}
	f.keys[keyHash] = k
	f.byID[k.KeyID] = k
	return *k, nil
}

func (f *fakeStore) GetAPIKeyByHash(_ context.Context, keyHash string) (store.APIKey, error) {
	k, ok := f.keys[keyHash]
	if !ok {
		return store.APIKey{}, store.ErrNotFound
	}
	return *k, nil
}

func (f *fakeStore) ListAPIKeys(_ context.Context, userID string) ([]store.APIKey, error) {
	var out []store.APIKey
	for _, k := range f.keys {
		if k.UserID == userID {
			out = append(out, *k)
		}
	}
	return out, nil
}

func (f *fakeStore) RevokeAPIKey(_ context.Context, userID, keyID string, _ audit.Event) (bool, error) {
	k, ok := f.byID[keyID]
	if !ok || k.UserID != userID || k.RevokedAt != nil {
		return false, nil
	}
	now := time.Now()
	k.RevokedAt = &now
	return true, nil
}

func (f *fakeStore) TouchAPIKey(_ context.Context, keyID, lastIP, clientID string) {}

func (f *fakeStore) RoutePlanFor(context.Context, string) (llmkernel.RoutePlanView, error) {
	return llmkernel.RoutePlanView{}, errors.New("no routes in fake")
}

func (f *fakeStore) ResolveProviderTarget(_ context.Context, provider string) (llmkernel.ProviderTarget, error) {
	return llmkernel.ProviderTarget{}, errors.New("no providers in fake")
}

func (f *fakeStore) PricingKnown(context.Context, string, string) (governance.Pricing, bool, int, error) {
	return governance.Pricing{Currency: "USD"}, false, 0, nil
}

func (f *fakeStore) InsertUsage(context.Context, governance.UsageRecord) error { return nil }

func (f *fakeStore) ListEnabledAliases(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}

func (f *fakeStore) ListPackageKeys(context.Context) ([]skillreg.PkgKey, error) {
	return nil, nil
}

func (f *fakeStore) ActivePackageKeyID(context.Context) (string, error) {
	return "", errors.New("no package keys in fake")
}

// Write routes security audit events to the injected writer so tests can make
// the reliable path fail.
func (f *fakeStore) Write(ctx context.Context, e audit.Event) error {
	if f.failWrite != nil {
		return f.failWrite
	}
	return f.Writer.Write(ctx, e)
}

func auditEventsOf(t *testing.T, fs *fakeStore) []audit.Event {
	t.Helper()
	mw, ok := fs.Writer.(*audit.MemoryWriter)
	if !ok {
		t.Fatalf("test writer expected")
	}
	return mw.Events()
}

// newSignerVerifier builds a matched Ed25519 signer/verifier pair.
func newSignerVerifier(t *testing.T) (token.Signer, token.Verifier) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := token.NewEd25519Signer("k1", "https://agent.company.com/", priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	verifier, err := token.NewEd25519Verifier(token.VerificationKey{KeyID: "k1", Public: pub})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	return signer, verifier
}

func doJSON(t *testing.T, h http.Handler, method, path, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload string
	if body != nil {
		b, _ := json.Marshal(body)
		payload = string(b)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(payload))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func problemCode(t *testing.T, rec *httptest.ResponseRecorder) (string, int) {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("problem body must be JSON: %v", err)
	}
	return body.Code, rec.Code
}

// Security regression: every API-key denial returns 401 AND records
// gateway.auth.failed audit evidence through the reliable writer.
func TestAPIKeyDenialsFailClosedWithAudit(t *testing.T) {
	signer, verifier := newSignerVerifier(t)
	fs := newFakeStore()
	s := &gateway.Server{PG: stubPinger{}, Redis: stubPinger{}, Store: fs, Signer: signer, Verifier: verifier, DevMode: true}
	h := s.Handler()

	t.Run("missing credential", func(t *testing.T) {
		rec := doJSON(t, h, http.MethodGet, "/enterprise/v1/me", "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("got %d, want 401", rec.Code)
		}
	})

	t.Run("malformed key", func(t *testing.T) {
		rec := doJSON(t, h, http.MethodGet, "/enterprise/v1/me", "agk_live_short", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("got %d, want 401", rec.Code)
		}
		events := auditEventsOf(t, fs)
		if len(events) != 1 || events[0].Type != audit.EventAuthFailed {
			t.Fatalf("expected one auth failure audit event, got %+v", events)
		}
	})

	t.Run("unknown key", func(t *testing.T) {
		rec := doJSON(t, h, http.MethodGet, "/enterprise/v1/me", "agk_live_0000000000000000000000000000000000", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("got %d, want 401", rec.Code)
		}
		if got := len(auditEventsOf(t, fs)); got != 2 {
			t.Fatalf("second denial must add audit evidence, total %d", got)
		}
	})
}

func TestRevokedAndExpiredKeysDenied(t *testing.T) {
	signer, verifier := newSignerVerifier(t)
	fs := newFakeStore()
	s := &gateway.Server{PG: stubPinger{}, Redis: stubPinger{}, Store: fs, Signer: signer, Verifier: verifier, DevMode: true}
	h := s.Handler()

	fs.seedUser("usr_1")
	fs.members["usr_1"] = []store.Membership{{WorkspaceID: "secops", Roles: []string{"developer"}}}
	liveValue := fs.seedKey("k_live", "usr_1")

	// expired key
	expValue := fs.seedKey("k_exp", "usr_1")
	past := time.Now().Add(-time.Hour)
	fs.byID["k_exp"].ExpiresAt = &past

	if rec := doJSON(t, h, http.MethodGet, "/enterprise/v1/me", expValue, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired key: got %d, want 401", rec.Code)
	}
	events := auditEventsOf(t, fs)
	if len(events) != 1 || events[0].Detail["reason"] != "expired" {
		t.Fatalf("expired denial must emit audit evidence, got %+v", events)
	}

	// revoke the live key, then the same value must stop working immediately.
	now := time.Now()
	fs.byID["k_live"].RevokedAt = &now
	if rec := doJSON(t, h, http.MethodGet, "/enterprise/v1/me", liveValue, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key: got %d, want 401", rec.Code)
	}
	events = auditEventsOf(t, fs)
	if len(events) != 2 || events[1].Detail["reason"] != "revoked" {
		t.Fatalf("revocation denial must emit audit evidence, got %+v", events)
	}
}

// Profile is the session-authenticated surface: an API key must NOT work, a
// session must.
func TestProfileAuthSurface(t *testing.T) {
	signer, verifier := newSignerVerifier(t)
	fs := newFakeStore()
	s := &gateway.Server{PG: stubPinger{}, Redis: stubPinger{}, Store: fs, Signer: signer, Verifier: verifier, DevMode: true}
	h := s.Handler()

	fs.seedUser("usr_1")
	fs.members["usr_1"] = []store.Membership{{WorkspaceID: "secops", Roles: []string{"developer"}}}
	keyValue := fs.seedKey("k1", "usr_1")

	sessionRec := doJSON(t, h, http.MethodPost, "/enterprise/v1/auth/dev-login", "", map[string]any{
		"email": "usr_1@e.io", "workspaces": []string{"secops"},
	})
	if sessionRec.Code != http.StatusOK {
		t.Fatalf("dev-login: %d", sessionRec.Code)
	}
	var session struct {
		SessionToken string `json:"session_token"`
	}
	_ = json.Unmarshal(sessionRec.Body.Bytes(), &session)

	// Session works on profile...
	rec := doJSON(t, h, http.MethodGet, "/enterprise/v1/profile", session.SessionToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("profile with session: %d %s", rec.Code, rec.Body.String())
	}
	var me struct {
		User struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if me.User.Email == "" {
		t.Fatalf("profile payload wrong: %s", rec.Body.String())
	}

	// ...but an API key must not.
	if rec := doJSON(t, h, http.MethodGet, "/enterprise/v1/profile", keyValue, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("profile with api key: got %d, want 401", rec.Code)
	}

	// me (key surface) must reject a session token symmetrically.
	if rec := doJSON(t, h, http.MethodGet, "/enterprise/v1/me", session.SessionToken, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("me with session: got %d, want 401", rec.Code)
	}
}

func TestExchangeRejectsGatewayAudienceAndUnregistered(t *testing.T) {
	signer, verifier := newSignerVerifier(t)
	fs := newFakeStore()
	s := &gateway.Server{PG: stubPinger{}, Redis: stubPinger{}, Store: fs, Signer: signer, Verifier: verifier, DevMode: true}
	h := s.Handler()

	fs.seedUser("usr_1")
	fs.members["usr_1"] = []store.Membership{{WorkspaceID: "secops", Roles: []string{"developer"}}}
	keyValue := fs.seedKey("k1", "usr_1")

	rec := doJSON(t, h, http.MethodPost, "/enterprise/v1/token/exchange", keyValue, map[string]any{"audience": "agent-gateway"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("agent-gateway audience: got %d, want 403", rec.Code)
	}
	rec = doJSON(t, h, http.MethodPost, "/enterprise/v1/token/exchange", keyValue, map[string]any{"audience": "attacker-service"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unregistered audience: got %d, want 400", rec.Code)
	}
}

func TestExchangeCrossWorkspaceDeniedWithAudit(t *testing.T) {
	signer, verifier := newSignerVerifier(t)
	fs := newFakeStore()
	s := &gateway.Server{PG: stubPinger{}, Redis: stubPinger{}, Store: fs, Signer: signer, Verifier: verifier, DevMode: true}
	h := s.Handler()

	fs.seedUser("usr_1")
	fs.members["usr_1"] = []store.Membership{{WorkspaceID: "secops", Roles: []string{"developer"}}}
	keyValue := fs.seedKey("k1", "usr_1")

	req := httptest.NewRequest(http.MethodPost, "/enterprise/v1/token/exchange", strings.NewReader(`{"audience":"mcp-gateway"}`))
	req.Header.Set("Authorization", "Bearer "+keyValue)
	req.Header.Set(identity.HeaderWorkspace, "finance")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	code, status := problemCode(t, rec)
	if status != http.StatusForbidden || code != "workspace_forbidden" {
		t.Fatalf("cross-workspace: got %d %s, want 403 workspace_forbidden", status, code)
	}
	events := auditEventsOf(t, fs)
	if len(events) != 1 || events[0].Type != audit.EventAuthFailed || events[0].Detail["reason"] != "workspace_denied" {
		t.Fatalf("cross-workspace denial must emit audit evidence, got %+v", events)
	}
}

// Audit delivery failure must be observable: the exchange must refuse to hand
// out a token without its audit evidence.
func TestExchangeFailsClosedWhenAuditUnavailable(t *testing.T) {
	signer, verifier := newSignerVerifier(t)
	fs := newFakeStore()
	fs.failWrite = errors.New("audit storage down")
	s := &gateway.Server{PG: stubPinger{}, Redis: stubPinger{}, Store: fs, Signer: signer, Verifier: verifier, DevMode: true}
	h := s.Handler()

	fs.seedUser("usr_1")
	fs.members["usr_1"] = []store.Membership{{WorkspaceID: "secops", Roles: []string{"developer"}}}
	keyValue := fs.seedKey("k1", "usr_1")

	req := httptest.NewRequest(http.MethodPost, "/enterprise/v1/token/exchange", strings.NewReader(`{"audience":"mcp-gateway"}`))
	req.Header.Set("Authorization", "Bearer "+keyValue)
	req.Header.Set(identity.HeaderWorkspace, "secops")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("exchange without audit evidence: got %d, want 500", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["access_token"] != nil {
		t.Fatalf("no token may be returned when audit delivery fails")
	}
}

func TestDevLoginDisabledWithoutDevMode(t *testing.T) {
	signer, verifier := newSignerVerifier(t)
	fs := newFakeStore()
	s := &gateway.Server{PG: stubPinger{}, Redis: stubPinger{}, Store: fs, Signer: signer, Verifier: verifier, DevMode: false}
	rec := doJSON(t, s.Handler(), http.MethodPost, "/enterprise/v1/auth/dev-login", "", map[string]any{"email": "x@e.io"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("dev login without dev mode: got %d, want 404", rec.Code)
	}
}

func TestRateLimitBlocksExcess(t *testing.T) {
	signer, verifier := newSignerVerifier(t)
	fs := newFakeStore()
	counter := &countingCounter{}
	s := &gateway.Server{
		PG: stubPinger{}, Redis: stubPinger{}, Store: fs, Signer: signer, Verifier: verifier, DevMode: true,
		Rate: &gateway.FixedWindow{Counter: counter, Limit: 2, Window: time.Minute},
	}
	h := s.Handler()

	fs.seedUser("usr_1")
	keyValue := fs.seedKey("k1", "usr_1")

	for i := 0; i < 2; i++ {
		if rec := doJSON(t, h, http.MethodGet, "/enterprise/v1/me", keyValue, nil); rec.Code != http.StatusOK {
			t.Fatalf("request %d: got %d, want 200", i+1, rec.Code)
		}
	}
	rec := doJSON(t, h, http.MethodGet, "/enterprise/v1/me", keyValue, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third request: got %d, want 429", rec.Code)
	}
}

type countingCounter struct{ n int64 }

func (c *countingCounter) Incr(context.Context, string) (int64, error) {
	c.n++
	return c.n, nil
}

func TestRateLimitFailsOpenOnCounterError(t *testing.T) {
	s := &gateway.Server{Rate: &gateway.FixedWindow{Counter: errCounter{}, Limit: 1, Window: time.Minute}}
	allowed, err := s.Rate.Allow(context.Background(), "subject")
	if !allowed {
		t.Fatalf("counter failure must fail open")
	}
	if err == nil {
		t.Fatalf("counter failure must be surfaced for logging")
	}
}

type errCounter struct{}

func (errCounter) Incr(context.Context, string) (int64, error) { return 0, errors.New("redis down") }

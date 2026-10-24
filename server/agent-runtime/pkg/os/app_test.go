package os

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/sqlite"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

type blockingModel struct{}

func (blockingModel) Response(ctx context.Context, _ []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (blockingModel) ResponseStream(ctx context.Context, _ []models.Message, _ models.ModelOptions) (<-chan models.StreamEvent, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (blockingModel) ID() string       { return "blocking" }
func (blockingModel) Provider() string { return "test" }

type releasableModel struct{ release <-chan struct{} }

func (m releasableModel) Response(ctx context.Context, _ []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
	select {
	case <-m.release:
		return &models.ModelResponse{Content: "done"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m releasableModel) ResponseStream(ctx context.Context, _ []models.Message, _ models.ModelOptions) (<-chan models.StreamEvent, error) {
	select {
	case <-m.release:
		ch := make(chan models.StreamEvent)
		close(ch)
		return ch, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (releasableModel) ID() string       { return "releasable" }
func (releasableModel) Provider() string { return "test" }

func TestInfoRoute(t *testing.T) {
	os, err := New(AgentOSConfig{})
	if err != nil {
		t.Fatalf("create os: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/info", nil)
	rec := httptest.NewRecorder()
	os.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["name"] != "xingding-agent-runtime AgentOS" {
		t.Fatalf("unexpected response: %v", resp)
	}
	if _, exists := resp["project"]; exists {
		t.Fatalf("direct AgentOS unexpectedly exposed project metadata: %v", resp)
	}
}

func TestProjectInfoIsSnapshotted(t *testing.T) {
	project := &ProjectInfo{ID: "original", Name: "Original", Version: "v1", SchemaVersion: 1, SpecHash: "abc"}
	os, err := New(AgentOSConfig{Project: project})
	if err != nil {
		t.Fatalf("create os: %v", err)
	}
	project.ID = "mutated"
	rec := httptest.NewRecorder()
	os.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/info", nil))
	var response struct {
		Project ProjectInfo `json:"project"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode info: %v", err)
	}
	if response.Project.ID != "original" {
		t.Fatalf("project info changed after New: %+v", response.Project)
	}
}

func TestAgentOSRejectsStrictModuleWithLegacyAgentMemory(t *testing.T) {
	legacyDB := in_memory.New()
	value, err := agent.New(agent.AgentConfig{ID: "legacy", Model: mock.New(), Memory: memory.NewManager(legacyDB)})
	if err != nil {
		t.Fatal(err)
	}
	module, err := memory.NewModule(memory.ModuleConfig{
		Store:      in_memory.NewMemoryStore(),
		Authorizer: memory.AuthorizerFunc(func(context.Context, memory.AuthorizeRequest) error { return nil }),
		Keys:       map[string][]byte{"current": []byte("0123456789abcdef0123456789abcdef")}, CurrentKey: "current",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(AgentOSConfig{Agents: map[string]*agent.Agent{"legacy": value}, Memory: &MemoryConfig{Module: module, ProjectID: "project", CollectionID: "profile"}})
	if !errors.Is(err, agent.ErrConflictingMemoryConfiguration) {
		t.Fatalf("error=%v", err)
	}
}

func TestAgentRouteIntegration(t *testing.T) {
	os, err := New(AgentOSConfig{
		Agents: map[string]*agent.Agent{
			"a1": newTestAgent(t, "a1"),
		},
	})
	if err != nil {
		t.Fatalf("create os: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/agents/a1/runs", nil)
	rec := httptest.NewRecorder()
	os.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBackgroundAgentRunCanBeQueriedAndCancelled(t *testing.T) {
	store := in_memory.New()
	a, err := agent.New(agent.AgentConfig{ID: "a1", Model: blockingModel{}, DB: store})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	runtime, err := New(AgentOSConfig{Agents: map[string]*agent.Agent{"a1": a}, DB: store})
	if err != nil {
		t.Fatalf("create os: %v", err)
	}

	rec := httptest.NewRecorder()
	runtime.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/agents/a1/runs?background=true", bytes.NewBufferString(`{"message":"wait"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("background start = %d: %s", rec.Code, rec.Body.String())
	}
	var started session.RunOutput
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode start: %v", err)
	}
	if started.RunID == "" || started.Status != session.RunStatusRunning {
		t.Fatalf("start response = %+v", started)
	}

	rec = httptest.NewRecorder()
	runtime.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/"+started.RunID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("immediate get = %d: %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	runtime.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/runs/"+started.RunID+"/cancel", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel = %d: %s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(time.Second)
	for {
		rec = httptest.NewRecorder()
		runtime.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/"+started.RunID, nil))
		var output session.RunOutput
		_ = json.Unmarshal(rec.Body.Bytes(), &output)
		if output.Status == session.RunStatusCancelled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not become cancelled: %s", rec.Body.String())
		}
		time.Sleep(5 * time.Millisecond)
	}

	rec = httptest.NewRecorder()
	runtime.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/runs/"+started.RunID+"/cancel", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("repeated cancel = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBackgroundRunRequiresDurableStore(t *testing.T) {
	runtime, err := New(AgentOSConfig{Agents: map[string]*agent.Agent{"a1": newTestAgent(t, "a1")}})
	if err != nil {
		t.Fatalf("create os: %v", err)
	}
	rec := httptest.NewRecorder()
	runtime.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/agents/a1/runs?background=true", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("background without store = %d: %s", rec.Code, rec.Body.String())
	}
}

func isolationToken(t *testing.T, key, userID string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": userID,
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	encoded, err := token.SignedString([]byte(key))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return encoded
}

func tenantToken(t *testing.T, key, userID, namespace string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": userID, "namespace": namespace, "exp": time.Now().Add(time.Hour).Unix(),
	})
	encoded, err := token.SignedString([]byte(key))
	if err != nil {
		t.Fatalf("sign tenant token: %v", err)
	}
	return encoded
}

func tokenWithMemoryClaims(t *testing.T, key string, claims jwt.MapClaims) string {
	t.Helper()
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	encoded, err := token.SignedString([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestAgentOSPrivateMemoryRequiresCompleteJWTPrincipal(t *testing.T) {
	const key = "memory-auth-key"
	const securityKey = "machine-key"
	store := in_memory.NewMemoryStore()
	module, err := memory.NewModule(memory.ModuleConfig{
		Store:      store,
		Authorizer: memory.AuthorizerFunc(func(context.Context, memory.AuthorizeRequest) error { return nil }),
		Keys:       map[string][]byte{"current": []byte("0123456789abcdef0123456789abcdef")}, CurrentKey: "current",
	})
	if err != nil {
		t.Fatal(err)
	}
	resolverCalls := 0
	resolver := memory.ProposalSourceResolverFunc(func(context.Context, memory.Principal, memory.Scope, string) (memory.SourceAttestation, []memory.ProposalFragment, error) {
		resolverCalls++
		return memory.SourceAttestation{}, nil, nil
	})
	runtime, err := New(AgentOSConfig{
		Authorization: AuthorizationConfig{Enabled: true, JWTVerificationKey: key}, SecurityKey: securityKey,
		Memory: &MemoryConfig{Module: module, ProjectID: "project", CollectionID: "profile", ProposeEnabled: true, SourceResolver: resolver},
	})
	if err != nil {
		t.Fatal(err)
	}
	tokens := []string{
		securityKey,
		tokenWithMemoryClaims(t, key, jwt.MapClaims{"namespace": "tenant", "iss": "issuer"}),
		tokenWithMemoryClaims(t, key, jwt.MapClaims{"sub": "alice", "iss": "issuer"}),
		tokenWithMemoryClaims(t, key, jwt.MapClaims{"sub": "alice", "namespace": "tenant"}),
	}
	for _, token := range tokens {
		before := store.ContentReadCount()
		response := httptest.NewRecorder()
		runtime.Handler().ServeHTTP(response, authorizedRequest(http.MethodGet, "/memories/secret", token, nil))
		if response.Code != http.StatusUnauthorized || store.ContentReadCount() != before {
			t.Fatalf("GET token=%q status=%d reads=%d", token, response.Code, store.ContentReadCount()-before)
		}
		response = httptest.NewRecorder()
		runtime.Handler().ServeHTTP(response, authorizedRequest(http.MethodPost, "/memories/propose", token, []byte(`{"source_ref":"run-1"}`)))
		if response.Code != http.StatusUnauthorized || resolverCalls != 0 {
			t.Fatalf("propose token=%q status=%d resolver=%d", token, response.Code, resolverCalls)
		}
	}
}

func TestAgentOSTenantSessionIsolationUsesSQLite(t *testing.T) {
	const key = "tenant-key"
	store, err := sqlite.New("")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer store.Close()
	runtime, err := New(AgentOSConfig{DB: store, Authorization: AuthorizationConfig{Enabled: true, JWTVerificationKey: key}})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	create := func(namespace string) session.AgentSession {
		rec := httptest.NewRecorder()
		runtime.Handler().ServeHTTP(rec, authorizedRequest(http.MethodPost, "/sessions", tenantToken(t, key, "same-user", namespace), []byte(`{"agent_id":"a"}`)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s = %d: %s", namespace, rec.Code, rec.Body.String())
		}
		var out session.AgentSession
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode create: %v", err)
		}
		return out
	}
	a := create("tenant-a")
	b := create("tenant-b")
	for _, tc := range []struct{ token, path string }{
		{tenantToken(t, key, "same-user", "tenant-a"), "/sessions/" + b.SessionID},
		{tenantToken(t, key, "same-user", "tenant-b"), "/sessions/" + a.SessionID},
	} {
		rec := httptest.NewRecorder()
		runtime.Handler().ServeHTTP(rec, authorizedRequest(http.MethodGet, tc.path, tc.token, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("cross tenant get = %d: %s", rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	runtime.Handler().ServeHTTP(rec, authorizedRequest(http.MethodGet, "/sessions", tenantToken(t, key, "same-user", "tenant-a"), nil))
	var listed struct {
		Sessions []session.AgentSession `json:"sessions"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &listed) != nil || len(listed.Sessions) != 1 || listed.Sessions[0].SessionID != a.SessionID {
		t.Fatalf("tenant A list = %d: %s", rec.Code, rec.Body.String())
	}
	patch := []byte(`{"revision":1,"summary":"forged"}`)
	rec = httptest.NewRecorder()
	runtime.Handler().ServeHTTP(rec, authorizedRequest(http.MethodPatch, "/sessions/"+b.SessionID, tenantToken(t, key, "same-user", "tenant-a"), patch))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross tenant update = %d: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	runtime.Handler().ServeHTTP(rec, authorizedRequest(http.MethodDelete, "/sessions/"+b.SessionID, tenantToken(t, key, "same-user", "tenant-a"), nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross tenant delete = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentOSRunPersistsTrustedTenantNamespace(t *testing.T) {
	const key = "tenant-run-key"
	store, err := sqlite.New("")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer store.Close()
	model := mock.New()
	model.ResponseData = &models.ModelResponse{Content: "ok"}
	a, err := agent.New(agent.AgentConfig{ID: "tenant-agent", Model: model, DB: store})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	runtime, err := New(AgentOSConfig{DB: store, Agents: map[string]*agent.Agent{"tenant-agent": a}, Authorization: AuthorizationConfig{Enabled: true, JWTVerificationKey: key}})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	rec := httptest.NewRecorder()
	runtime.Handler().ServeHTTP(rec, authorizedRequest(http.MethodPost, "/agents/tenant-agent/runs", tenantToken(t, key, "same-user", "tenant-run"), []byte(`{"message":"go","namespace":"forged"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("run = %d: %s", rec.Code, rec.Body.String())
	}
	var output session.RunOutput
	if err := json.Unmarshal(rec.Body.Bytes(), &output); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if output.Namespace != "tenant-run" {
		t.Fatalf("run namespace = %q", output.Namespace)
	}
	stored, err := store.GetRunScoped(context.Background(), db.RunQuery{RunID: output.RunID, Namespace: "tenant-run"})
	if err != nil || stored == nil || stored.Namespace != "tenant-run" {
		t.Fatalf("stored tenant run = %+v, %v", stored, err)
	}
	if leaked, err := store.GetRunScoped(context.Background(), db.RunQuery{RunID: output.RunID, Namespace: "other"}); err != nil || leaked != nil {
		t.Fatalf("cross tenant run = %+v, %v", leaked, err)
	}

	// Legacy AgentOS allows the older subject+namespace JWT without an issuer.
	// Its background route must preserve that namespace instead of applying the
	// strict-memory detached-context behavior and falling into a legacy scope.
	backgroundStore, err := sqlite.New(filepath.Join(t.TempDir(), "legacy-background.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer backgroundStore.Close()
	releaseBackground := make(chan struct{})
	backgroundAgent, err := agent.New(agent.AgentConfig{ID: "background-agent", Model: releasableModel{release: releaseBackground}, DB: backgroundStore})
	if err != nil {
		t.Fatal(err)
	}
	backgroundRuntime, err := New(AgentOSConfig{DB: backgroundStore, Agents: map[string]*agent.Agent{"background-agent": backgroundAgent}, Authorization: AuthorizationConfig{Enabled: true, JWTVerificationKey: key}})
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	backgroundRuntime.Handler().ServeHTTP(rec, authorizedRequest(http.MethodPost, "/agents/background-agent/runs?background=true", tenantToken(t, key, "same-user", "tenant-run"), []byte(`{"message":"background","user_id":"same-user"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("background run = %d: %s", rec.Code, rec.Body.String())
	}
	var started session.RunOutput
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode background run: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		stored, err = backgroundStore.GetRunScoped(context.Background(), db.RunQuery{RunID: started.RunID, Namespace: "tenant-run"})
		if err == nil && stored != nil {
			break
		}
		if err != nil || time.Now().After(deadline) {
			t.Fatalf("stored background tenant run = %+v, %v", stored, err)
		}
		time.Sleep(time.Millisecond)
	}
	if stored.Namespace != "tenant-run" {
		t.Fatalf("background namespace = %q", stored.Namespace)
	}
	legacyContext := execution.ContextWithNamespace(context.Background(), execution.ReservedNamespace)
	if leaked, err := backgroundStore.GetRun(legacyContext, db.RunQuery{RunID: started.RunID}); err != nil || leaked != nil {
		t.Fatalf("background run leaked to legacy namespace = %+v, %v", leaked, err)
	}
	close(releaseBackground)
	deadline = time.Now().Add(2 * time.Second)
	for {
		stored, err = backgroundStore.GetRunScoped(context.Background(), db.RunQuery{RunID: started.RunID, Namespace: "tenant-run"})
		if err == nil && stored != nil && stored.Status == session.RunStatusCompleted {
			break
		}
		if err != nil || time.Now().After(deadline) {
			t.Fatalf("completed background tenant run = %+v, %v", stored, err)
		}
		time.Sleep(time.Millisecond)
	}
}

func authorizedRequest(method, path, token string, body []byte) *http.Request {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func TestAgentOSUserIsolation(t *testing.T) {
	const key = "test-key"
	store := in_memory.New()
	now := time.Now().UTC()
	if err := store.CreateSession(context.Background(), &session.AgentSession{
		SessionID: "alice-session", AgentID: "a1", UserID: "alice", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create alice session: %v", err)
	}
	if err := store.CreateMemory(context.Background(), &memory.UserMemory{
		MemoryID: "alice-memory", Memory: "private", UserID: "alice", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create alice memory: %v", err)
	}
	model := mock.New()
	model.ResponseData = &models.ModelResponse{Content: "hello"}
	a, err := agent.New(agent.AgentConfig{ID: "a1", Model: model, DB: store})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	os, err := New(AgentOSConfig{
		Agents: map[string]*agent.Agent{"a1": a}, DB: store,
		Authorization: AuthorizationConfig{Enabled: true, UserIsolation: true, JWTVerificationKey: key},
	})
	if err != nil {
		t.Fatalf("create os: %v", err)
	}

	// Every base route is protected when authentication is enabled.
	rec := httptest.NewRecorder()
	os.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/info", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated base route = %d, want 401", rec.Code)
	}

	bobToken := isolationToken(t, key, "bob")
	aliceToken := isolationToken(t, key, "alice")
	rec = httptest.NewRecorder()
	os.Handler().ServeHTTP(rec, authorizedRequest(http.MethodGet, "/sessions/alice-session", bobToken, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bob GET session = %d, want 404", rec.Code)
	}
	// A legacy DB does not implicitly enable the strict memory route. This is
	// unavailable rather than a raw-DB lookup (even though that record exists).
	rec = httptest.NewRecorder()
	os.Handler().ServeHTTP(rec, authorizedRequest(http.MethodGet, "/memories/alice-memory", bobToken, nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("bob GET disabled memory = %d, want 503", rec.Code)
	}
	legacy, err := store.ReadMemory(context.Background(), "alice-memory")
	if err != nil || legacy == nil || legacy.Memory != "private" {
		t.Fatalf("legacy record changed by disabled route: %+v, %v", legacy, err)
	}

	// The authenticated subject overrides client-supplied user_id.
	rec = httptest.NewRecorder()
	os.Handler().ServeHTTP(rec, authorizedRequest(http.MethodPost, "/agents/a1/runs", bobToken, []byte(`{"message":"hi","user_id":"alice"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("run status = %d: %s", rec.Code, rec.Body.String())
	}
	var output session.RunOutput
	if err := json.Unmarshal(rec.Body.Bytes(), &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if output.UserID != "bob" {
		t.Fatalf("run user_id = %q, want bob", output.UserID)
	}

	// The owner can still access their resource.
	rec = httptest.NewRecorder()
	os.Handler().ServeHTTP(rec, authorizedRequest(http.MethodGet, "/sessions/alice-session", aliceToken, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("alice GET session = %d", rec.Code)
	}
}

// TestAgentOSRejectsOversizedBody verifies the P1-5 DoS guard: a request
// body larger than maxRequestBodyBytes fails before routing.
func TestAgentOSRejectsOversizedBody(t *testing.T) {
	os, err := New(AgentOSConfig{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	big := bytes.Repeat([]byte("x"), maxRequestBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/agents/a/runs", bytes.NewReader(big))
	rec := httptest.NewRecorder()
	os.Handler().ServeHTTP(rec, req)

	// The oversized body must be rejected (413 via MaxBytesReader surfacing
	// as an error, or a downstream 4xx) and never reach a handler that would
	// read the whole body.
	if rec.Code >= 500 {
		t.Fatalf("oversized body must not reach a 5xx: status=%d", rec.Code)
	}
}

// TestAgentOSAcceptsNormalBody ensures the guard does not block normal
// requests.
func TestAgentOSAcceptsNormalBody(t *testing.T) {
	os, err := New(AgentOSConfig{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/agents/missing/runs", bytes.NewReader([]byte(`{"message":"hi"}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	os.Handler().ServeHTTP(rec, req)
	// "agent not found" is 404: the body passed the guard and reached routing.
	if rec.Code == 413 || rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("normal body was rejected: %d", rec.Code)
	}
}

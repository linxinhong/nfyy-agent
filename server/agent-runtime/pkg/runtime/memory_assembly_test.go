package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	agentos "github.com/linxinhong/xingding-agent-runtime/pkg/os"
	agentruntime "github.com/linxinhong/xingding-agent-runtime/pkg/runtime"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
	"github.com/linxinhong/xingding-agent-runtime/pkg/workflow"
)

type memoryExecutionObservation struct {
	subject  string
	messages string
	private  any
	canceled bool
	err      error
}

type requestPrivateKey struct{}

type memoryExecutionModel struct {
	observations chan memoryExecutionObservation
}

type namespaceInMemoryDB struct{ *in_memory.DB }

func (*namespaceInMemoryDB) SupportsNamespaceScopes() {}

func (d *namespaceInMemoryDB) CreateSession(ctx context.Context, value *session.AgentSession) error {
	namespace, err := db.NamespaceFor(ctx, value.Namespace)
	if err != nil {
		return err
	}
	value.Namespace = namespace
	return d.DB.CreateSession(ctx, value)
}

func (d *namespaceInMemoryDB) AppendRun(ctx context.Context, value *session.RunOutput) error {
	namespace, err := db.NamespaceFor(ctx, value.Namespace)
	if err != nil {
		return err
	}
	value.Namespace = namespace
	return d.DB.AppendRun(ctx, value)
}

func (d *namespaceInMemoryDB) UpdateRun(ctx context.Context, value *session.RunOutput, expected session.RunStatus) error {
	namespace, err := db.NamespaceFor(ctx, value.Namespace)
	if err != nil {
		return err
	}
	value.Namespace = namespace
	return d.DB.UpdateRun(ctx, value, expected)
}

func (d *namespaceInMemoryDB) CreateWorkflowSession(ctx context.Context, value *session.WorkflowSession) error {
	namespace, err := db.NamespaceFor(ctx, value.Namespace)
	if err != nil {
		return err
	}
	value.Namespace = namespace
	return d.DB.CreateWorkflowSession(ctx, value)
}

func (m *memoryExecutionModel) observe(ctx context.Context, messages []models.Message) {
	p, err := service.PrincipalFromContext(ctx)
	var contents []string
	for _, message := range messages {
		contents = append(contents, message.Content)
	}
	m.observations <- memoryExecutionObservation{
		subject: p.Subject, messages: strings.Join(contents, "\n"),
		private: ctx.Value(requestPrivateKey{}), canceled: ctx.Err() != nil, err: err,
	}
}

func (m *memoryExecutionModel) Response(ctx context.Context, messages []models.Message, _ models.ModelOptions) (*models.ModelResponse, error) {
	m.observe(ctx, messages)
	return &models.ModelResponse{Content: "done"}, nil
}

func (m *memoryExecutionModel) ResponseStream(ctx context.Context, messages []models.Message, _ models.ModelOptions) (<-chan models.StreamEvent, error) {
	m.observe(ctx, messages)
	ch := make(chan models.StreamEvent, 2)
	ch <- models.StreamEvent{Event: "content_delta", Content: "done"}
	ch <- models.StreamEvent{Event: "done", Done: true}
	close(ch)
	return ch, nil
}

func (*memoryExecutionModel) ID() string       { return "memory-execution" }
func (*memoryExecutionModel) Provider() string { return "test" }

type runtimeProposalExtractor struct{}

func (runtimeProposalExtractor) Extract(context.Context, memory.ProposalExtractionRequest) (memory.ProposalExtractionResult, error) {
	return memory.ProposalExtractionResult{}, nil
}

func TestMemoryAssembly(t *testing.T) {
	store := in_memory.NewMemoryStore()
	module, err := memory.NewModule(memory.ModuleConfig{
		Store:      store,
		Authorizer: memory.AuthorizerFunc(func(context.Context, memory.AuthorizeRequest) error { return nil }),
		Keys:       map[string][]byte{"current": []byte(strings.Repeat("k", 32))}, CurrentKey: "current",
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("v6 decode validate and build", func(t *testing.T) {
		raw := `{"schema_version":6,"id":"project","version":"v1","agents":[{"id":"assistant","model_ref":"chat"}],"memory":{"ref":"private","collection_id":"profile","recall":{"limit":1,"kinds":["preference"],"topics":["profile"],"failure_mode":"strict"}}}`
		spec, err := agentruntime.DecodeSpec(strings.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		project, err := agentruntime.Build(spec, agentruntime.Dependencies{
			Models:   map[string]models.Model{"chat": mock.New()},
			Memories: map[string]agentruntime.MemoryDependency{"private": {Module: module}},
		})
		if err != nil {
			t.Fatal(err)
		}
		got, ok := project.Memory()
		if !ok || got != module || project.SchemaVersion() != agentruntime.SchemaVersion6 {
			t.Fatalf("memory=%p ok=%v schema=%d", got, ok, project.SchemaVersion())
		}
		hash := project.SpecHash()
		spec.Memory.CollectionID = "forged"
		spec.Memory.Recall.Limit = 50
		spec.Memory.Recall.Topics[0] = "forged"
		if project.SpecHash() != hash {
			t.Fatal("built project changed after caller mutated policy")
		}
	})

	t.Run("v1 through v5 remain disabled", func(t *testing.T) {
		for version := 1; version <= 5; version++ {
			plain, err := agentruntime.DecodeSpec(strings.NewReader(fmt.Sprintf(`{"schema_version":%d,"id":"old","version":"v1"}`, version)))
			if err != nil {
				t.Fatalf("v%d plain: %v", version, err)
			}
			project, err := agentruntime.Build(plain, agentruntime.Dependencies{})
			if err != nil {
				t.Fatalf("v%d build: %v", version, err)
			}
			if _, ok := project.Memory(); ok {
				t.Fatalf("v%d unexpectedly enabled memory", version)
			}
			_, err = agentruntime.DecodeSpec(strings.NewReader(fmt.Sprintf(`{"schema_version":%d,"id":"old","version":"v1","memory":null}`, version)))
			if err == nil {
				t.Fatalf("v%d accepted memory field", version)
			}
		}
	})

	t.Run("complete dependency and field diagnostics", func(t *testing.T) {
		_, err := agentruntime.Build(agentruntime.ProjectSpec{
			SchemaVersion: agentruntime.SchemaVersion6, ID: "project", Version: "v1",
			Memory: &agentruntime.MemorySpec{Ref: "missing", CollectionID: "", Recall: &agentruntime.MemoryRecallSpec{Limit: 51, FailureMode: "open"}, ProposeEnabled: true},
		}, agentruntime.Dependencies{})
		var buildErr *agentruntime.BuildError
		if !errors.As(err, &buildErr) {
			t.Fatalf("error=%v", err)
		}
		paths := map[string]bool{}
		for _, diagnostic := range buildErr.Diagnostics {
			paths[diagnostic.Path] = true
		}
		for _, path := range []string{"memory.ref", "memory.collection_id", "memory.recall.limit", "memory.recall.failure_mode"} {
			if !paths[path] {
				t.Fatalf("missing diagnostic %s in %+v", path, buildErr.Diagnostics)
			}
		}
	})

	t.Run("recall bounds and proposal capability are build time gates", func(t *testing.T) {
		base := agentruntime.ProjectSpec{
			SchemaVersion: agentruntime.SchemaVersion6, ID: "project", Version: "v1",
			Agents: []agentruntime.AgentSpec{{ID: "assistant", ModelRef: "chat"}},
			Memory: &agentruntime.MemorySpec{Ref: "private", CollectionID: "profile", ProposeEnabled: true,
				Recall: &agentruntime.MemoryRecallSpec{
					Kinds:  []memory.Kind{memory.KindPreference, memory.KindPreference, memory.KindPreference, memory.KindPreference},
					Topics: append(make([]string, memory.MaxRecallTopics), strings.Repeat("t", memory.MaxRecallTopicBytes+1)),
				}},
		}
		_, err := agentruntime.Build(base, agentruntime.Dependencies{
			Models: map[string]models.Model{"chat": mock.New()},
			Memories: map[string]agentruntime.MemoryDependency{"private": {
				Module: module,
				SourceResolver: memory.ProposalSourceResolverFunc(func(context.Context, memory.Principal, memory.Scope, string) (memory.SourceAttestation, []memory.ProposalFragment, error) {
					return memory.SourceAttestation{}, nil, nil
				}),
			}},
		})
		var buildErr *agentruntime.BuildError
		if !errors.As(err, &buildErr) {
			t.Fatalf("error=%v", err)
		}
		paths := map[string]bool{}
		for _, diagnostic := range buildErr.Diagnostics {
			paths[diagnostic.Path] = true
		}
		for _, path := range []string{"memory.propose_enabled", "memory.recall.kinds", "memory.recall.topics", "memory.recall.topics[32]"} {
			if !paths[path] {
				t.Fatalf("missing diagnostic %s in %+v", path, buildErr.Diagnostics)
			}
		}
	})

	t.Run("scope components and typed nil source dependency fail at build", func(t *testing.T) {
		_, err := agentruntime.Build(agentruntime.ProjectSpec{
			SchemaVersion: agentruntime.SchemaVersion6, ID: "project\tforged", Version: "v1",
			Agents: []agentruntime.AgentSpec{{ID: "assistant", ModelRef: "chat"}},
			Memory: &agentruntime.MemorySpec{Ref: "private", CollectionID: "profile\x7f", ProposeEnabled: true},
		}, agentruntime.Dependencies{
			Models: map[string]models.Model{"chat": mock.New()},
			Memories: map[string]agentruntime.MemoryDependency{"private": {
				Module: module, SourceResolver: memory.ProposalSourceResolverFunc(nil),
			}},
		})
		var buildErr *agentruntime.BuildError
		if !errors.As(err, &buildErr) {
			t.Fatalf("error=%v", err)
		}
		paths := map[string]bool{}
		for _, diagnostic := range buildErr.Diagnostics {
			paths[diagnostic.Path] = true
		}
		for _, path := range []string{"id", "memory.collection_id", "memory.propose_enabled"} {
			if !paths[path] {
				t.Fatalf("missing diagnostic %s in %+v", path, buildErr.Diagnostics)
			}
		}
	})

	t.Run("v6 proposal requires and accepts complete trusted dependencies", func(t *testing.T) {
		proposalModule, err := memory.NewModule(memory.ModuleConfig{
			Store:      in_memory.NewMemoryStore(),
			Authorizer: memory.AuthorizerFunc(func(context.Context, memory.AuthorizeRequest) error { return nil }),
			Proposal: &memory.ProposalConfig{
				Extractor:      runtimeProposalExtractor{},
				OutboundPolicy: memory.ProposalOutboundPolicyFunc(func(context.Context, memory.ProposalOutboundRequest) error { return nil }),
				Producer:       "runtime", ModelConfigVersion: "v1",
			},
			Keys: map[string][]byte{"current": []byte(strings.Repeat("p", 32))}, CurrentKey: "current",
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = agentruntime.Build(agentruntime.ProjectSpec{
			SchemaVersion: agentruntime.SchemaVersion6, ID: "project", Version: "v1",
			Agents: []agentruntime.AgentSpec{{ID: "assistant", ModelRef: "chat"}},
			Memory: &agentruntime.MemorySpec{Ref: "private", CollectionID: "profile", ProposeEnabled: true},
		}, agentruntime.Dependencies{
			Models: map[string]models.Model{"chat": mock.New()},
			Memories: map[string]agentruntime.MemoryDependency{"private": {
				Module: proposalModule,
				SourceResolver: memory.ProposalSourceResolverFunc(func(context.Context, memory.Principal, memory.Scope, string) (memory.SourceAttestation, []memory.ProposalFragment, error) {
					return memory.SourceAttestation{}, nil, nil
				}),
			}},
		})
		if err != nil {
			t.Fatalf("complete v6 proposal build: %v", err)
		}
	})
}

func TestBuiltAgentUsesFrozenPerRequestAuthorityForSyncStreamAndBackground(t *testing.T) {
	const jwtKey = "memory-runtime-key"
	store := in_memory.NewMemoryStore()
	module, err := memory.NewModule(memory.ModuleConfig{
		Store: store,
		Authorizer: memory.AuthorizerFunc(func(_ context.Context, req memory.AuthorizeRequest) error {
			if req.Scope != (memory.Scope{}) && (req.Principal.Namespace != req.Scope.Namespace || req.Principal.Subject != req.Scope.SubjectID) {
				return memory.ErrUnauthorized
			}
			return nil
		}),
		Keys: map[string][]byte{"current": []byte("0123456789abcdef0123456789abcdef")}, CurrentKey: "current",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, subject := range []string{"alice", "bob"} {
		principal := memory.Principal{Subject: subject, Namespace: "tenant", Issuer: "issuer"}
		scope := memory.Scope{Namespace: "tenant", ProjectID: "project", SubjectID: subject, CollectionID: "profile"}
		token, issueErr := module.IssueOperationToken(context.Background(), principal, scope, memory.ActionCreate, time.Hour)
		if issueErr != nil {
			t.Fatal(issueErr)
		}
		if _, createErr := module.Create(context.Background(), principal, scope, memory.CreateRequest{Content: subject + "-private", Topics: []string{"profile"}, Kind: memory.KindPreference, OperationToken: token}); createErr != nil {
			t.Fatal(createErr)
		}
	}
	sessionStore := &namespaceInMemoryDB{DB: in_memory.New()}
	model := &memoryExecutionModel{observations: make(chan memoryExecutionObservation, 16)}
	spec := agentruntime.ProjectSpec{
		SchemaVersion: agentruntime.SchemaVersion6, ID: "project", Version: "v1",
		Agents: []agentruntime.AgentSpec{{ID: "assistant", ModelRef: "chat"}},
		Workflows: []agentruntime.WorkflowSpec{{Definition: workflow.Definition{
			ID: "remember", Version: "v1", Root: workflow.ExecutorNode("agent", agentruntime.AgentExecutorRef("assistant")),
		}}},
		Memory: &agentruntime.MemorySpec{Ref: "private", CollectionID: "profile", Recall: &agentruntime.MemoryRecallSpec{Limit: 1, Kinds: []memory.Kind{memory.KindPreference}, Topics: []string{"profile"}}},
	}
	project, err := agentruntime.Build(spec, agentruntime.Dependencies{
		DB: sessionStore, Models: map[string]models.Model{"chat": model},
		Memories:      map[string]agentruntime.MemoryDependency{"private": {Module: module}},
		Authorization: agentos.AuthorizationConfig{Enabled: true, JWTVerificationKey: jwtKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Caller mutation after Build cannot alter the Agent's frozen recall policy.
	spec.Memory.CollectionID = "forged"
	spec.Memory.Recall.Topics[0] = "forged"

	tokenFor := func(subject string) string {
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"sub": subject, "namespace": "tenant", "iss": "issuer", "exp": time.Now().Add(time.Hour).Unix(),
		})
		signed, signErr := token.SignedString([]byte(jwtKey))
		if signErr != nil {
			t.Fatal(signErr)
		}
		return signed
	}
	type requestCase struct{ subject, suffix string }
	for _, suffix := range []string{"", "?stream=true", "?background=true"} {
		cases := []requestCase{{subject: "alice", suffix: suffix}, {subject: "bob", suffix: suffix}}
		var wg sync.WaitGroup
		errs := make(chan string, len(cases))
		for _, subject := range []string{"alice", "bob"} {
			tc := requestCase{subject: subject, suffix: suffix}
			wg.Add(1)
			go func() {
				defer wg.Done()
				forged := "alice"
				if tc.subject == "alice" {
					forged = "bob"
				}
				req := httptest.NewRequest(http.MethodPost, "/agents/assistant/runs"+tc.suffix, strings.NewReader(`{"message":"profile","user_id":"`+forged+`"}`))
				req.Header.Set("Authorization", "Bearer "+tokenFor(tc.subject))
				req.Header.Set("Content-Type", "application/json")
				recorder := httptest.NewRecorder()
				project.AgentOS().Handler().ServeHTTP(recorder, req)
				want := http.StatusOK
				if tc.suffix == "?background=true" {
					want = http.StatusAccepted
				}
				if recorder.Code != want {
					errs <- fmt.Sprintf("%s%s status=%d body=%s", tc.subject, tc.suffix, recorder.Code, recorder.Body.String())
				}
			}()
		}
		wg.Wait()
		close(errs)
		for failure := range errs {
			t.Error(failure)
		}
		for range cases {
			select {
			case observation := <-model.observations:
				if observation.err != nil || !strings.Contains(observation.messages, observation.subject+"-private") {
					t.Fatalf("observation=%+v", observation)
				}
				other := "alice-private"
				if observation.subject == "alice" {
					other = "bob-private"
				}
				if strings.Contains(observation.messages, other) {
					t.Fatalf("cross-subject memory in %+v", observation)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("timed out waiting for model execution mode=%q", suffix)
			}
		}
	}

	// Nested Agent execution receives the verified identity in a fresh background
	// context. The request's cancellation and unrelated private values are not
	// retained by the run manager after the HTTP handler returns.
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), requestPrivateKey{}, "http-private"))
	req := httptest.NewRequest(http.MethodPost, "/workflows/remember/runs?background=true", strings.NewReader(`{"message":"profile","user_id":"bob"}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+tokenFor("alice"))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	project.AgentOS().Handler().ServeHTTP(recorder, req)
	cancel()
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("workflow background status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	select {
	case observation := <-model.observations:
		if observation.err != nil || observation.subject != "alice" || observation.private != nil || observation.canceled || !strings.Contains(observation.messages, "alice-private") || strings.Contains(observation.messages, "bob-private") {
			t.Fatalf("workflow background observation=%+v", observation)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for nested workflow Agent")
	}
}

func ExampleBuild_trustedMemory() {
	module, err := memory.NewModule(memory.ModuleConfig{
		Store: in_memory.NewMemoryStore(),
		Authorizer: memory.AuthorizerFunc(func(_ context.Context, req memory.AuthorizeRequest) error {
			if req.Scope != (memory.Scope{}) && (req.Principal.Namespace != req.Scope.Namespace || req.Principal.Subject != req.Scope.SubjectID) {
				return memory.ErrUnauthorized
			}
			return nil
		}),
		Keys: map[string][]byte{"current": []byte("0123456789abcdef0123456789abcdef")}, CurrentKey: "current",
	})
	if err != nil {
		panic(err)
	}
	project, err := agentruntime.Build(agentruntime.ProjectSpec{
		SchemaVersion: agentruntime.SchemaVersion6, ID: "project", Version: "v1",
		Agents: []agentruntime.AgentSpec{{ID: "assistant", ModelRef: "chat"}},
		Memory: &agentruntime.MemorySpec{
			Ref: "private", CollectionID: "profile",
			Recall: &agentruntime.MemoryRecallSpec{Limit: 3, Kinds: []memory.Kind{memory.KindPreference}},
		},
	}, agentruntime.Dependencies{
		Models:        map[string]models.Model{"chat": mock.New()},
		Memories:      map[string]agentruntime.MemoryDependency{"private": {Module: module}},
		Authorization: agentos.AuthorizationConfig{Enabled: true, JWTVerificationKey: "host-jwt-verification-key"},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(project.SchemaVersion(), project.AgentOS() != nil)
	// Output: 6 true
}

package mcpgateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/errs"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/mcpgateway"
)

type fakeRegistry struct {
	tools map[string]map[string]mcpgateway.RegisteredTool // server -> name -> tool
}

func (f *fakeRegistry) add(tool mcpgateway.RegisteredTool) {
	if f.tools == nil {
		f.tools = map[string]map[string]mcpgateway.RegisteredTool{}
	}
	if f.tools[tool.Server] == nil {
		f.tools[tool.Server] = map[string]mcpgateway.RegisteredTool{}
	}
	f.tools[tool.Server][tool.Name] = tool
}

func (f *fakeRegistry) ListToolsForWorkspace(_ context.Context, server, workspace string) ([]mcpgateway.RegisteredTool, error) {
	var out []mcpgateway.RegisteredTool
	for _, tool := range f.tools[server] {
		for _, ws := range tool.Workspaces {
			if ws == workspace {
				out = append(out, tool)
				break
			}
		}
	}
	return out, nil
}

func (f *fakeRegistry) GetToolForWorkspace(_ context.Context, name, server, workspace string) (mcpgateway.RegisteredTool, error) {
	tool, ok := f.tools[server][name]
	if !ok {
		return mcpgateway.RegisteredTool{}, storeNotFound
	}
	for _, ws := range tool.Workspaces {
		if ws == workspace {
			return tool, nil
		}
	}
	return mcpgateway.RegisteredTool{}, storeNotFound
}

var storeNotFound = errors.New("not found")

type staticResolver struct{ value string }

func (s staticResolver) Resolve(context.Context, string) (string, error) {
	if s.value == "" {
		return "", fmt.Errorf("credential not configured")
	}
	return s.value, nil
}

type recordingExecutor struct {
	gotCredential string
	gotArgs       map[string]any
	response      json.RawMessage
}

func (e *recordingExecutor) Execute(_ context.Context, _ mcpgateway.RegisteredTool, args map[string]any, credential string) (json.RawMessage, error) {
	e.gotCredential = credential
	e.gotArgs = args
	return e.response, nil
}

func session(workspace string) identity.Session {
	return identity.Session{
		Principal: identity.Principal{UserID: "user_1"},
		Workspace: identity.WorkspaceContext{WorkspaceID: workspace, Source: identity.WorkspaceSourceMembership},
	}
}

func newService(registry *fakeRegistry, executor mcpgateway.Executor) *mcpgateway.Service {
	writer := &audit.MemoryWriter{}
	return &mcpgateway.Service{Registry: registry, Resolver: staticResolver{value: "git-secret-token"}, Executor: executor, Audit: writer}
}

func gitTool() mcpgateway.RegisteredTool {
	return mcpgateway.RegisteredTool{
		Name: "git_read_file", Server: "gitlab", Description: "read", RiskLevel: mcpgateway.RiskR1,
		Workspaces: []string{"secops"}, EndpointURL: "http://upstream.invalid/repos/{repo}/contents/{path}",
		EndpointMethod: "GET", CredentialRef: "env:EAP_MCP_GIT_TOKEN",
	}
}

// Discovery only exposes tools the principal may execute.
func TestListToolsFiltersByWorkspace(t *testing.T) {
	reg := &fakeRegistry{}
	reg.add(gitTool())
	reg.add(mcpgateway.RegisteredTool{Name: "finance_only", Server: "gitlab", RiskLevel: mcpgateway.RiskR1, Workspaces: []string{"finance"}})
	svc := newService(reg, &recordingExecutor{})

	tools, err := svc.ListTools(context.Background(), session("secops"), "gitlab")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "git_read_file" {
		t.Fatalf("discovery leaked foreign tools: %+v", tools)
	}
	for _, tool := range tools {
		if tool.Description == "" {
			t.Fatalf("metadata projection incomplete: %+v", tool)
		}
	}
}

// A direct call for a foreign tool is a recorded denial, indistinguishable
// from an unknown tool.
func TestCallToolForeignWorkspaceDeniedAndAudited(t *testing.T) {
	reg := &fakeRegistry{}
	reg.add(gitTool())
	svc := newService(reg, &recordingExecutor{})

	_, err := svc.CallTool(context.Background(), session("finance"), mcpgateway.ToolCall{
		Server: "gitlab", Name: "git_read_file", Arguments: map[string]any{"repo": "x", "path": "y"},
	})
	problem, ok := errs.From(err)
	if !ok || problem.Code != errs.NotFound {
		t.Fatalf("foreign tool must look unknown, got %v", err)
	}
	mw := svc.Audit.(*audit.MemoryWriter)
	events := mw.Events()
	if len(events) != 1 || events[0].Outcome != audit.OutcomeDenied || events[0].Detail["reason"] != "workspace_not_allowed" {
		t.Fatalf("cross-workspace call must be recorded as denial, got %+v", events)
	}
}

// Happy path: credential resolved server-side and handed to the executor only;
// the returned result carries just the upstream payload.
func TestCallToolHappyPathKeepsCredentialServerSide(t *testing.T) {
	reg := &fakeRegistry{}
	reg.add(gitTool())
	exec := &recordingExecutor{response: json.RawMessage(`{"content":"file body"}`)}
	svc := newService(reg, exec)

	result, err := svc.CallTool(context.Background(), session("secops"), mcpgateway.ToolCall{
		Server: "gitlab", Name: "git_read_file", Arguments: map[string]any{"repo": "acme/app", "path": "README.md"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if exec.gotCredential != "git-secret-token" {
		t.Fatalf("executor must receive the resolved credential")
	}
	if !strings.Contains(string(result.Result), "file body") || strings.Contains(string(result.Result), "git-secret-token") {
		t.Fatalf("response must carry upstream payload without the credential: %s", result.Result)
	}
	mw := svc.Audit.(*audit.MemoryWriter)
	events := mw.Events()
	if len(events) != 1 || events[0].Outcome != audit.OutcomeAllowed || events[0].Type != audit.EventMCPToolExecuted {
		t.Fatalf("execution must be audited as allowed, got %+v", events)
	}
}

// A missing credential fails the call closed instead of degrading to an
// unauthenticated upstream request.
func TestCallToolFailsClosedOnCredentialError(t *testing.T) {
	reg := &fakeRegistry{}
	reg.add(gitTool())
	svc := newService(reg, &recordingExecutor{})
	svc.Resolver = staticResolver{value: ""}

	_, err := svc.CallTool(context.Background(), session("secops"), mcpgateway.ToolCall{
		Server: "gitlab", Name: "git_read_file", Arguments: map[string]any{"repo": "x", "path": "y"},
	})
	problem, ok := errs.From(err)
	if !ok || problem.Code != errs.Internal {
		t.Fatalf("credential failure must fail closed, got %v", err)
	}
}

// HTTPExecutor injects the credential as bearer and never returns it.
func TestHTTPExecutorInjectsCredentialOnlyInHeader(t *testing.T) {
	var gotAuth, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	exec := &mcpgateway.HTTPExecutor{}
	tool := gitTool()
	tool.EndpointURL = upstream.URL + "/repos/{repo}/contents/{path}"
	result, err := exec.Execute(context.Background(), tool,
		map[string]any{"repo": "acme/app", "path": "README.md"}, "git-secret-token")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if gotAuth != "Bearer git-secret-token" {
		t.Fatalf("credential must be the upstream bearer: %q", gotAuth)
	}
	if strings.Contains(string(result), "git-secret-token") || strings.Contains(gotBody, "git-secret-token") {
		t.Fatalf("credential leaked: %s %s", gotBody, result)
	}
}

package cli_test

// TASK-017 CLI acceptance coverage: config precedence + redaction (unit),
// device-login/whoami/workspace/model/MCP/doctor against the integration
// gateway (in-process Run), stable exit codes, and credential-free output.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"enterprise-agent-platform/packages/cli"
)

func TestConfigPrecedenceAndRedaction(t *testing.T) {
	dir := t.TempDir()
	// user config sets server A; project config sets server B; env sets C;
	// flags set D -> D wins. Project workspace selection persists.
	userDir := filepath.Join(dir, "home", ".config", "eap")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "config.yaml"),
		[]byte("server: http://user-config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	projDir := filepath.Join(dir, "proj")
	if err := os.MkdirAll(filepath.Join(projDir, ".eap"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, ".eap", "workspace.yaml"),
		[]byte("server: http://project-config\nworkspace: secops\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	o := cli.Options{
		Workdir: projDir,
		Stdout:  &strings.Builder{},
		Stderr:  &strings.Builder{},
		Keyring: cli.NewMemKeyring(),
	}
	t.Setenv("EAP_SERVER", "http://env-config")
	code := cli.Run(context.Background(), o)
	_ = code

	// A second Run with a --server flag must win over env/user/project.
	o2 := cli.Options{
		Workdir: projDir,
		Stdout:  &strings.Builder{},
		Stderr:  &strings.Builder{},
		Keyring: cli.NewMemKeyring(),
		Args:    []string{"config", "show", "--server", "http://flag-config", "--json"},
	}
	if code := cli.Run(context.Background(), o2); code != cli.ExitOK {
		t.Fatalf("config show exit %d", code)
	}
	out := o2.Stdout.(*strings.Builder).String()
	if !strings.Contains(out, "http://flag-config") {
		t.Fatalf("flag layer must win: %s", out)
	}
	if strings.Contains(out, "EAP_API_KEY\":") && !strings.Contains(out, "false") {
		t.Fatalf("credential state must not leak values: %s", out)
	}

	// Redaction: errors never echo secrets.
	o3 := cli.Options{
		Workdir: projDir,
		Stdout:  &strings.Builder{},
		Stderr:  &strings.Builder{},
		Keyring: cli.NewMemKeyring(),
		Args:    []string{"whoami", "--server", "http://flag-config"},
	}
	t.Setenv("EAP_API_KEY", "agk_live_supersecretvalue123")
	_ = os.Setenv("EAP_API_KEY", "agk_live_supersecretvalue123")
	code = cli.Run(context.Background(), o3)
	if code != cli.ExitError {
		t.Fatalf("whoami without gateway: %d", code)
	}
	errOut := o3.Stderr.(*strings.Builder).String()
	if strings.Contains(errOut, "agk_live_supersecretvalue123") {
		t.Fatalf("credential leaked in error output: %s", errOut)
	}
}

func TestCLIAgainstGateway(t *testing.T) {
	gw := newCLIGatewayStub(t)
	dir := t.TempDir()

	// 共享 keyring：登录写入的凭据对后续命令可见（与 OS Keychain 行为一致）。
	sharedKeyring := cli.NewMemKeyring()
	run := func(args ...string) (int, *strings.Builder, *strings.Builder) {
		full := append([]string{"--server", gw.URL}, args...)
		o := cli.Options{
			Server:  gw.URL,
			Workdir: dir,
			Stdout:  &strings.Builder{},
			Stderr:  &strings.Builder{},
			Keyring: sharedKeyring,
			Args:    full,
		}
		code := cli.Run(context.Background(), o)
		return code, o.Stdout.(*strings.Builder), o.Stderr.(*strings.Builder)
	}

	// 1. Device login end-to-end (approver auto-approves).
	code, out, errOut := run("login", "--json")
	if code != cli.ExitOK {
		t.Fatalf("login: %d %s %s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "登录成功") {
		t.Fatalf("login output: %s", out.String())
	}

	// 2. whoami via keychain credential.
	code, out, _ = run("whoami", "--json")
	if code != cli.ExitOK || !strings.Contains(out.String(), `"email":"device@example.com"`) {
		t.Fatalf("whoami: %d %s", code, out.String())
	}

	// 3. Workspace list + use + status.
	code, out, _ = run("workspace", "list", "--json")
	if code != cli.ExitOK || !strings.Contains(out.String(), `"id":"secops"`) {
		t.Fatalf("workspace list: %d %s", code, out.String())
	}
	code, _, _ = run("workspace", "use", "secops")
	if code != cli.ExitOK {
		t.Fatalf("workspace use: %d", code)
	}
	code, out, _ = run("workspace", "status", "--json")
	if code != cli.ExitOK || !strings.Contains(out.String(), "secops") {
		t.Fatalf("workspace status: %d %s", code, out.String())
	}

	// 4. Model list.
	code, out, _ = run("model", "list", "--json")
	if code != cli.ExitOK || !strings.Contains(out.String(), `"id":"cli-fast"`) {
		t.Fatalf("model list: %d %s", code, out.String())
	}

	// 5. MCP status via token exchange.
	code, _, _ = run("mcp", "list", "--json")
	if code != cli.ExitOK {
		t.Fatalf("mcp list: %d", code)
	}

	// 6. Doctor: all checks pass against the stub.
	code, out, _ = run("doctor", "--json")
	if code != cli.ExitOK || !strings.Contains(out.String(), `"ok":true`) {
		t.Fatalf("doctor: %d %s", code, out.String())
	}

	// 7. Logout revokes: subsequent whoami fails with non-zero exit.
	code, _, _ = run("logout")
	if code != cli.ExitOK {
		t.Fatalf("logout: %d", code)
	}
	code, _, errOut = run("whoami")
	if code != cli.ExitError {
		t.Fatalf("post-logout whoami: %d %s", code, errOut.String())
	}
	if strings.Contains(errOut.String(), "refresh_token") {
		t.Fatalf("credential material in error output: %s", errOut.String())
	}
}

// newCLIGatewayStub is a minimal gateway standing in for the integration
// environment: device flow, me, models, token exchange.
func newCLIGatewayStub(t *testing.T) *httptest.Server {
	t.Helper()
	var approvals = map[string]bool{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /enterprise/v1/auth/device/code", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"device_code":"dc1","user_code":"abcd2345","verification_uri":"/portal/device","expires_in":600,"interval":1}`))
		approvals["dc1"] = true // auto-approve for the test
	})
	mux.HandleFunc("POST /enterprise/v1/auth/device/token", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			GrantType    string `json:"grant_type"`
			DeviceCode   string `json:"device_code"`
			RefreshToken string `json:"refresh_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body.GrantType {
		case "urn:ietf:params:oauth:grant-type:device_code":
			if !approvals[body.DeviceCode] {
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"stub-access","refresh_token":"stub-refresh-1","expires_in":28800}`))
		case "refresh_token":
			if body.RefreshToken == "" {
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"stub-access","refresh_token":"stub-refresh-2","expires_in":28800}`))
		}
	})
	mux.HandleFunc("POST /enterprise/v1/auth/device/logout", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /enterprise/v1/me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer stub-access" && !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer agk_") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"user":{"id":"usr_1","email":"device@example.com"},"workspaces":[{"id":"secops","name":"Sec"}],"default_workspace":"secops"}`))
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Agent-Workspace") != "secops" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"cli-fast","object":"model"}]}`))
	})
	mux.HandleFunc("POST /enterprise/v1/token/exchange", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"stub-mcp","token_type":"Bearer","expires_in":600}`))
	})
	mux.HandleFunc("GET /enterprise/v1/cli/bootstrap", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"manifest_version":1,"user":{"id":"usr_1"},"workspace":"secops","workspaces":[],"agents":[],"endpoints":{}}`))
	})
	return httptest.NewServer(mux)
}

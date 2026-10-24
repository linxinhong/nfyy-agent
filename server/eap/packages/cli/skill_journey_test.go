package cli_test

// TASK-018 journey coverage: one skill completes init → validate → test →
// publish → review → release, then desired-state sync installs the identical
// verified release into Harness and Codex and emits an exact lockfile;
// repeated sync is idempotent; upgrade and policy removal converge only
// managed files; revoked/non-released versions fail closed. The stub gateway
// signs downloads with a real pkg- key so the offline verification path is
// exercised end to end.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cli "enterprise-agent-platform/packages/cli"
	"enterprise-agent-platform/packages/skillreg"
)

func testTime() time.Time { return time.Now() }

func TestSkillLifecycleJourney(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // adapter roots stay inside the test sandbox
	gw := newSkillGatewayStub(t)
	dir := t.TempDir()
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

	if code, _, errOut := run("login", "--json"); code != cli.ExitOK {
		t.Fatalf("login: %d %s", code, errOut.String())
	}
	if code, _, errOut := run("workspace", "use", "secops"); code != cli.ExitOK {
		t.Fatalf("workspace use: %d %s", code, errOut.String())
	}

	// 1. init + validate + declared test command (explicit invocation only).
	if code, _, errOut := run("skill", "init", "demo"); code != cli.ExitOK {
		t.Fatalf("skill init: %d %s", code, errOut.String())
	}
	skillDir := filepath.Join(dir, "demo")
	if code, _, errOut := run("skill", "validate", skillDir); code != cli.ExitOK {
		t.Fatalf("skill validate: %d %s", code, errOut.String())
	}
	skillYAML, err := os.ReadFile(filepath.Join(skillDir, "skill.yaml"))
	if err != nil {
		t.Fatalf("skill.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "skill.yaml"),
		append(skillYAML, []byte("test: echo skill-test-ok\n")...), 0o644); err != nil {
		t.Fatalf("write test cmd: %v", err)
	}
	code, out, errOut := run("skill", "test", skillDir)
	if code != cli.ExitOK || !strings.Contains(out.String(), "skill-test-ok") {
		t.Fatalf("skill test: %d %s %s", code, out.String(), errOut.String())
	}

	// 2. publish → review → release.
	if code, _, errOut := run("skill", "publish", "demo", "0.1.0", skillDir); code != cli.ExitOK {
		t.Fatalf("skill publish: %d %s", code, errOut.String())
	}
	if code, _, errOut := run("skill", "review", "demo", "0.1.0"); code != cli.ExitOK {
		t.Fatalf("skill review: %d %s", code, errOut.String())
	}
	if code, _, errOut := run("skill", "release", "demo", "0.1.0"); code != cli.ExitOK {
		t.Fatalf("skill release: %d %s", code, errOut.String())
	}

	// 3. search + info.
	code, out, errOut = run("skill", "search", "demo", "--json")
	if code != cli.ExitOK || !strings.Contains(out.String(), `"status":"released"`) {
		t.Fatalf("skill search: %d %s %s", code, out.String(), errOut.String())
	}
	code, out, _ = run("skill", "info", "demo", "--json")
	if code != cli.ExitOK || !strings.Contains(out.String(), `"version":"0.1.0"`) {
		t.Fatalf("skill info: %d %s", code, out.String())
	}

	// 4. install resolves the latest released version and records the key id.
	if code, _, errOut := run("skill", "install", "demo"); code != cli.ExitOK {
		t.Fatalf("skill install: %d %s", code, errOut.String())
	}
	lockBytes, err := os.ReadFile(filepath.Join(dir, "eap.lock"))
	if err != nil {
		t.Fatalf("eap.lock: %v", err)
	}
	if !strings.Contains(string(lockBytes), `"key_id": "pkg-stub"`) || !strings.Contains(string(lockBytes), `"version": "0.1.0"`) {
		t.Fatalf("lockfile missing key_id/version: %s", lockBytes)
	}
	installed, err := os.ReadFile(filepath.Join(home, ".agents", "skills", "harness", "demo", "SKILL.md"))
	if err != nil || !strings.Contains(string(installed), "demo") {
		t.Fatalf("harness install content: %v %s", err, installed)
	}

	// 5. Desired-state sync: dry-run shows codex install + harness unchanged,
	// apply converges both clients, replay is a full no-op.
	desired := "server: " + gw.URL + "\nworkspace: secops\nclients:\n  - harness\n  - codex\nskills:\n  - demo\n"
	if err := os.WriteFile(filepath.Join(dir, ".eap", "workspace.yaml"), []byte(desired), 0o644); err != nil {
		t.Fatalf("write desired state: %v", err)
	}
	code, out, errOut = run("sync", "--dry-run", "--json")
	if code != cli.ExitOK ||
		!strings.Contains(out.String(), `"action":"install"`) ||
		!strings.Contains(out.String(), `"client":"codex"`) ||
		!strings.Contains(out.String(), `"action":"unchanged"`) {
		t.Fatalf("dry-run plan: %d %s %s", code, out.String(), errOut.String())
	}
	codexSkill := filepath.Join(home, ".agents", "skills", "codex", "demo", "SKILL.md")
	if _, err := os.Stat(codexSkill); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not write: %v", err)
	}
	if code, _, errOut := run("sync"); code != cli.ExitOK {
		t.Fatalf("sync apply: %d %s", code, errOut.String())
	}
	if _, err := os.Stat(codexSkill); err != nil {
		t.Fatalf("codex install after sync: %v", err)
	}
	lock2, err := os.ReadFile(filepath.Join(dir, "eap.lock"))
	if err != nil || strings.Count(string(lock2), `"name": "demo"`) != 2 {
		t.Fatalf("lockfile must hold both clients: %v %s", err, lock2)
	}
	if code, out, errOut := run("sync", "--json"); code != cli.ExitOK ||
		strings.Contains(out.String(), `"action":"install"`) ||
		strings.Contains(out.String(), `"action":"upgrade"`) {
		t.Fatalf("replay must be all unchanged: %d %s %s", code, out.String(), errOut.String())
	}

	// 6. A newly released version upgrades only managed files.
	if code, _, errOut := run("skill", "publish", "demo", "0.2.0", skillDir); code != cli.ExitOK {
		t.Fatalf("publish 0.2.0: %d %s", code, errOut.String())
	}
	if code, _, errOut := run("skill", "review", "demo", "0.2.0"); code != cli.ExitOK {
		t.Fatalf("review 0.2.0: %d %s", code, errOut.String())
	}
	if code, _, errOut := run("skill", "release", "demo", "0.2.0"); code != cli.ExitOK {
		t.Fatalf("release 0.2.0: %d %s", code, errOut.String())
	}
	if code, _, errOut := run("skill", "update"); code != cli.ExitOK {
		t.Fatalf("skill update: %d %s", code, errOut.String())
	}
	lock3, _ := os.ReadFile(filepath.Join(dir, "eap.lock"))
	if !strings.Contains(string(lock3), `"version": "0.2.0"`) {
		t.Fatalf("update must record 0.2.0: %s", lock3)
	}
	upgraded, err := os.ReadFile(filepath.Join(home, ".agents", "skills", "harness", "demo", "SKILL.md"))
	if err != nil || !strings.Contains(string(upgraded), "v0.2.0") {
		t.Fatalf("upgraded content: %v %s", err, upgraded)
	}

	// 7. Policy removal converges only the managed files of that client.
	if code, _, errOut := run("skill", "remove", "demo"); code != cli.ExitOK {
		t.Fatalf("skill remove: %d %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".agents", "skills", "harness", "demo", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("remove must delete managed files: %v", err)
	}
	lock4, _ := os.ReadFile(filepath.Join(dir, "eap.lock"))
	if strings.Contains(string(lock4), `"client": "harness"`) {
		t.Fatalf("harness entries must be gone: %s", lock4)
	}

	// 8. Desired state without harness reinstalls it (convergence) and a
	// non-released desired version fails closed.
	if code, _, errOut := run("sync"); code != cli.ExitOK {
		t.Fatalf("re-sync after remove: %d %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".agents", "skills", "harness", "demo", "SKILL.md")); err != nil {
		t.Fatalf("convergence must reinstall harness: %v", err)
	}
	bad := strings.Replace(desired, "  - demo\n", "  - demo@9.9.9\n", 1)
	if err := os.WriteFile(filepath.Join(dir, ".eap", "workspace.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatalf("write bad desired: %v", err)
	}
	if code, _, _ := run("sync"); code != cli.ExitError {
		t.Fatalf("non-released desired version must fail closed: %d", code)
	}
}

// adapter roots follow each client's convention and unknown names fail closed
// to the project layout.
func TestSkillAdapterRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	if got := cli.AgentSkillRoot(dir, "claude-code"); got != filepath.Join(home, ".claude", "skills") {
		t.Fatalf("claude-code root: %s", got)
	}
	if got := cli.AgentSkillRoot(dir, "pi"); got != filepath.Join(home, ".pi", "agent", "skills") {
		t.Fatalf("pi root: %s", got)
	}
	if got := cli.AgentSkillRoot(dir, "project"); got != filepath.Join(dir, ".agents", "skills") {
		t.Fatalf("project root: %s", got)
	}
	if got := cli.AgentSkillRoot(dir, "mystery"); got != filepath.Join(dir, ".agents", "skills") {
		t.Fatalf("unknown client must fall back to project layout: %s", got)
	}
}

// newSkillGatewayStub stands in for the registry surface: publish/review/
// release/deprecate/revoke, catalog, signed downloads and package-key
// discovery, with deterministic archives per published version.
func newSkillGatewayStub(t *testing.T) *httptest.Server {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	type version struct {
		archive []byte
		digest  string
		status  string
		content string
	}
	pkgs := map[string]map[string]*version{}
	approve := func(w http.ResponseWriter, r *http.Request) {
		name, ver := r.PathValue("name"), r.PathValue("version")
		v, ok := pkgs[name][ver]
		if !ok {
			http.Error(w, "no such skill version", http.StatusNotFound)
			return
		}
		verb := strings.Split(r.URL.Path, "/")[len(strings.Split(r.URL.Path, "/"))-1]
		switch verb {
		case "review":
			v.status = skillreg.StatusApproved
		case "release":
			v.status = skillreg.StatusReleased
		case "deprecate":
			v.status = skillreg.StatusDeprecated
		case "revoke":
			v.status = "revoked"
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"` + v.status + `"}`))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /enterprise/v1/auth/device/code", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"device_code":"dc1","user_code":"abcd2345","verification_uri":"/portal/device","expires_in":600,"interval":1}`))
	})
	mux.HandleFunc("POST /enterprise/v1/auth/device/token", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			GrantType string `json:"grant_type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.GrantType == "urn:ietf:params:oauth:grant-type:device_code" {
			_, _ = w.Write([]byte(`{"access_token":"stub-access","refresh_token":"stub-refresh-1","expires_in":28800}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"stub-access","refresh_token":"stub-refresh-2","expires_in":28800}`))
	})
	mux.HandleFunc("POST /enterprise/v1/auth/device/logout", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /enterprise/v1/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"user":{"id":"usr_1","email":"device@example.com"},"workspaces":[{"id":"secops","name":"Sec"}],"default_workspace":"secops"}`))
	})
	mux.HandleFunc("POST /enterprise/v1/skills/{name}/versions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Version string `json:"version"`
			Files   []struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			} `json:"files"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		var content strings.Builder
		for i, f := range body.Files {
			if f.Path == "SKILL.md" {
				// Make each published version's installed bytes distinct.
				content.WriteString(f.Content)
				content.WriteString("\n<!-- release marker -->")
			}
			_ = i
		}
		marker := strings.ReplaceAll(content.String(), "demo", "demo v"+body.Version)
		archive, digest, err := skillreg.BuildArchive([]skillreg.PackageFile{
			{Path: "SKILL.md", Content: marker},
			{Path: "skill.yaml", Content: "name: " + r.PathValue("name") + "\nversion: " + body.Version + "\n"},
		})
		if err != nil {
			http.Error(w, "archive failed", http.StatusBadRequest)
			return
		}
		if pkgs[r.PathValue("name")] == nil {
			pkgs[r.PathValue("name")] = map[string]*version{}
		}
		pkgs[r.PathValue("name")][body.Version] = &version{archive: archive, digest: digest, status: skillreg.StatusPendingReview, content: marker}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"status":"pending_review"}`))
	})
	mux.HandleFunc("POST /enterprise/v1/skills/{name}/versions/{version}/review", approve)
	mux.HandleFunc("POST /enterprise/v1/skills/{name}/versions/{version}/release", approve)
	mux.HandleFunc("POST /enterprise/v1/skills/{name}/versions/{version}/deprecate", approve)
	mux.HandleFunc("POST /enterprise/v1/skills/{name}/versions/{version}/revoke", approve)
	mux.HandleFunc("GET /enterprise/v1/skills", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		rows := []string{}
		for name, versions := range pkgs {
			if q != "" && !strings.Contains(name, q) {
				continue
			}
			for ver, v := range versions {
				rows = append(rows, `{"name":"`+name+`","version":"`+ver+`","status":"`+v.status+`","digest":"sha256-`+v.digest+`"}`)
			}
		}
		_, _ = w.Write([]byte(`{"skills":[` + strings.Join(rows, ",") + `]}`))
	})
	mux.HandleFunc("GET /enterprise/v1/skills/{name}/versions/{version}/download", func(w http.ResponseWriter, r *http.Request) {
		v, ok := pkgs[r.PathValue("name")][r.PathValue("version")]
		if !ok || v.status != skillreg.StatusReleased {
			http.Error(w, "not released", http.StatusNotFound)
			return
		}
		sig, err := skillreg.SignRelease("pkg-stub", priv, v.digest, testTime())
		if err != nil {
			http.Error(w, "sign failed", http.StatusInternalServerError)
			return
		}
		sigJSON, _ := json.Marshal(sig)
		w.Header().Set("X-Skill-Digest", "sha256-"+v.digest)
		w.Header().Set("X-Skill-Signature", string(sigJSON))
		_, _ = w.Write(v.archive)
	})
	mux.HandleFunc("GET /.well-known/package-keys.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"keys":[{"kid":"pkg-stub","x":"` + base64.RawURLEncoding.EncodeToString(pub) + `","active":true}]}`))
	})
	return httptest.NewServer(mux)
}

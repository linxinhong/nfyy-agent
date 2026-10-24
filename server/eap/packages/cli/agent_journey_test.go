package cli_test

// TASK-019 journey coverage: `eap agent` lists the platform-matched release,
// installs it after ticket redemption with digest+signature verification,
// records the adapter-owned VERSION marker, updates to a newer release,
// refuses a minimum-version denial, and doctor surfaces the policy.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	cli "enterprise-agent-platform/packages/cli"
	"enterprise-agent-platform/packages/skillreg"
)

func TestAgentCLIJourney(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // DefaultInstallRoot lands in the sandbox
	// The journey is platform-agnostic: the CLI declares its own runtime
	// platform and the stub matches it (CI runs linux/amd64).
	platform := runtime.GOOS + "-" + runtime.GOARCH
	gw := newAgentGatewayStub(t)
	dir := t.TempDir()
	sharedKeyring := cli.NewMemKeyring()
	agentRoot := filepath.Join(dir, "agents")
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
	agentRun := func(args ...string) (int, *strings.Builder, *strings.Builder) {
		return run(append([]string{"agent", "--root", agentRoot}, args...)...)
	}
	binary := func(client string) string {
		name := client
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		return filepath.Join(dir, "agents", platform, client, name)
	}

	if code, _, errOut := run("login", "--json"); code != cli.ExitOK {
		t.Fatalf("login: %d %s", code, errOut.String())
	}
	if code, _, errOut := run("workspace", "use", "secops"); code != cli.ExitOK {
		t.Fatalf("workspace use: %d %s", code, errOut.String())
	}

	// 1. List shows the released version for the declared platform.
	code, out, errOut := agentRun("list", "harness", "--json")
	if code != cli.ExitOK || !strings.Contains(out.String(), `"version":"2.0.0"`) {
		t.Fatalf("agent list: %d %s %s", code, out.String(), errOut.String())
	}

	// 2. Install an explicit version, redeem the ticket, verify and activate.
	code, out, errOut = agentRun("install", "harness", "2.0.0")
	if code != cli.ExitOK || !strings.Contains(out.String(), "2.0.0") {
		t.Fatalf("agent install: %d %s %s", code, out.String(), errOut.String())
	}
	data, err := os.ReadFile(binary("harness"))
	if err != nil || string(data) != "harness-2.0.0-bytes" {
		t.Fatalf("installed bytes: %v %s", err, data)
	}
	marker, err := os.ReadFile(filepath.Join(filepath.Dir(binary("harness")), "VERSION"))
	if err != nil || strings.TrimSpace(string(marker)) != "2.0.0" {
		t.Fatalf("VERSION marker: %v %s", err, marker)
	}

	// 3. Doctor reports the installed agent as meeting policy.
	code, out, _ = run("doctor", "--json")
	if code != cli.ExitOK || !strings.Contains(out.String(), `"name":"agents"`) {
		t.Fatalf("doctor agents check: %d %s", code, out.String())
	}

	// 4. Update moves the installation to the newer release.
	code, out, errOut = agentRun("update", "harness")
	if code != cli.ExitOK || !strings.Contains(out.String(), "2.1.0") {
		t.Fatalf("agent update: %d %s %s", code, out.String(), errOut.String())
	}
	data, _ = os.ReadFile(binary("harness"))
	if string(data) != "harness-2.1.0-bytes" {
		t.Fatalf("updated bytes: %s", data)
	}

	// 5. Minimum-version denial surfaces as a CLI failure.
	code, _, errOut = agentRun("install", "harness", "1.0.0")
	if code == cli.ExitOK || !strings.Contains(errOut.String(), "minimum") {
		t.Fatalf("minimum denial must fail closed: %d %s", code, errOut.String())
	}
	if _, err := os.Stat(binary("harness")); err != nil {
		t.Fatalf("denial must preserve the working installation: %v", err)
	}

	// 6. Unknown client and platform-mismatched installs are refused.
	code, _, errOut = agentRun("install", "windows-only")
	if code == cli.ExitOK {
		t.Fatalf("unknown client must fail: %s", errOut.String())
	}
}

func newAgentGatewayStub(t *testing.T) *httptest.Server {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	type release struct {
		version, minimum, payload string
	}
	releases := map[string][]*release{
		"harness": {
			{version: "2.1.0", minimum: "1.5.0", payload: "harness-2.1.0-bytes"},
			{version: "2.0.0", minimum: "1.0.0", payload: "harness-2.0.0-bytes"},
			{version: "1.0.0", minimum: "1.5.0", payload: "harness-1.0.0-bytes"},
		},
	}
	sign := func(payload string) (string, string) {
		sum := sha256.Sum256([]byte(payload))
		digest := hex.EncodeToString(sum[:])
		sig, err := skillreg.SignRelease("pkg-agent", priv, digest, time.Now())
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		env, _ := json.Marshal(sig)
		return digest, string(env)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /enterprise/v1/auth/device/code", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"device_code":"dc1","user_code":"abcd2345","verification_uri":"/portal/device","expires_in":600,"interval":1}`))
	})
	mux.HandleFunc("POST /enterprise/v1/auth/device/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"stub-access","refresh_token":"stub-refresh-1","expires_in":28800}`))
	})
	mux.HandleFunc("GET /enterprise/v1/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"user":{"id":"usr_1","email":"device@example.com"},"workspaces":[{"id":"secops","name":"Sec"}],"default_workspace":"secops"}`))
	})
	mux.HandleFunc("GET /enterprise/v1/cli/bootstrap", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"manifest_version":1,"agents":[{"client":"harness","minimum_version":"1.5.0"}],"endpoints":{}}`))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
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
	mux.HandleFunc("GET /.well-known/package-keys.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"keys":[{"kid":"pkg-agent","x":"` + base64.RawURLEncoding.EncodeToString(pub) + `","active":true}]}`))
	})
	mux.HandleFunc("GET /enterprise/v1/agents/", func(w http.ResponseWriter, r *http.Request) {
		client := strings.TrimPrefix(r.URL.Path, "/enterprise/v1/agents/")
		rels := releases[client]
		if rels == nil || r.URL.Query().Get("os") != runtime.GOOS || r.URL.Query().Get("arch") != runtime.GOARCH {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		rows := []string{}
		for _, rel := range rels {
			rows = append(rows, fmt.Sprintf(`{"client":%q,"version":%q,"os":%q,"arch":%q,"minimum_version":%q,"digest":"sha256-x"}`, client, rel.version, runtime.GOOS, runtime.GOARCH, rel.minimum))
		}
		_, _ = w.Write([]byte(`{"releases":[` + strings.Join(rows, ",") + `]}`))
	})
	mux.HandleFunc("POST /enterprise/v1/agents/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/enterprise/v1/agents/"), "/")
		client, action := parts[0], parts[len(parts)-1]
		var body struct {
			Version string `json:"version"`
			OS      string `json:"os"`
			Arch    string `json:"arch"`
			Ticket  string `json:"ticket"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if action == "ticket" {
			var found *release
			for _, rel := range releases[client] {
				if rel.version == body.Version && body.OS == runtime.GOOS && body.Arch == runtime.GOARCH {
					found = rel
				}
			}
			if found == nil {
				http.Error(w, "no release", http.StatusNotFound)
				return
			}
			if !versionMeetStub(found.version, found.minimum) {
				http.Error(w, fmt.Sprintf("version %s is below the workspace minimum %s — upgrade required", body.Version, found.minimum), http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(fmt.Sprintf(`{"ticket":%q,"expires_in":120}`, "tk-"+client+"-"+body.Version)))
			return
		}
		// download: the ticket embeds the version for this stub
		version := strings.TrimPrefix(body.Ticket, "tk-"+client+"-")
		for _, rel := range releases[client] {
			if rel.version == version {
				digest, sigEnv := sign(rel.payload)
				w.Header().Set("X-Agent-Digest", "sha256-"+digest)
				w.Header().Set("X-Agent-Signature", sigEnv)
				_, _ = w.Write([]byte(rel.payload))
				return
			}
		}
		http.Error(w, "unknown ticket", http.StatusForbidden)
	})
	return httptest.NewServer(mux)
}

func versionMeetStub(have, minimum string) bool {
	var hv, mv [3]int
	fmt.Sscanf(have, "%d.%d.%d", &hv[0], &hv[1], &hv[2])
	fmt.Sscanf(minimum, "%d.%d.%d", &mv[0], &mv[1], &mv[2])
	for i := 0; i < 3; i++ {
		if hv[i] != mv[i] {
			return hv[i] > mv[i]
		}
	}
	return true
}

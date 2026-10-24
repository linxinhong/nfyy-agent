//go:build integration

// Security adversarial suite (TASK-047). Three adversarial surfaces plus the
// rate-limit effectiveness probe, all against the real store/Redis stack:
//
//  1. TestAuthzMatrix — every route registered in apps/gateway/server.go x
//     every credential class, every cell asserted. The route inventory is
//     parsed from the HandleFunc registrations via go/ast, so a new surface
//     without a matrix row fails the sweep instead of shipping unverified.
//  2. TestCredentialLeakage — a full key lifecycle (create/use/revoke) plus
//     the device flow runs while the gateway's REAL log stream is captured
//     in-process; every response body and the generated bootstrap config are
//     scanned for key material. The two documented one-time displays (key
//     creation response, device refresh token) are the only allowed places.
//  3. TestDLPBypassCorpus — table-driven evasive payloads derived from the
//     seeded rule set: canonical matches, case variations for (?i) rules,
//     separator runs inside character classes, multiline private-key
//     headers, checksum valid/invalid identities, and chunk-boundary splits
//     through the live SSE sanitizer. Zero leak is asserted on the
//     provider-bound payload, the released response, and the raw SSE bytes.
//  4. TestRateLimitEffectiveness — at the configured limit a burst trips 429
//     with the frozen Problem shape, the limit is per-subject, and the
//     limiter recovers on the next window.
//
// Payloads the current rules genuinely miss are recorded as FINDING
// subtests (self-verifying, skipped while unfixed) — they become fix cards;
// this suite never weakens an assertion to pass. TASK-054 (migration
// 000021) closed the three fixable TASK-047 findings, so their evasive
// shapes are asserted as POSITIVE coverage here and in TestDLPRuleFindings;
// the one remaining gap (db_conn_string password segment with spaces) is a
// documented accepted limitation whose self-verifying subtest flips
// positive automatically if the rule ever learns the shape.
package main_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	gateway "enterprise-agent-platform/apps/gateway"
	"enterprise-agent-platform/packages/apikey"
	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/blob"
	"enterprise-agent-platform/packages/capability"
	"enterprise-agent-platform/packages/catalog"
	"enterprise-agent-platform/packages/content"
	"enterprise-agent-platform/packages/contribution"
	"enterprise-agent-platform/packages/curation"
	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/oidc"
	"enterprise-agent-platform/packages/policy"
	"enterprise-agent-platform/packages/skillreg"
	"enterprise-agent-platform/packages/skillshare"
	"enterprise-agent-platform/packages/wiki"
)

// ---------------------------------------------------------------------------
// shared adversarial environment
// ---------------------------------------------------------------------------

// advEnv wires EVERY conditional gateway surface (OIDC, local auth, CLI
// dist, Portal static, capability registry, LLM plane, knowledge) so the
// matrix sweeps the full registered route table of a production-shaped
// server, not a reduced fixture.
type advEnv struct {
	*sliceEnv
	userSession  string // control-plane session, developer role
	adminSession string // control-plane session, admin role
	maintSession string // control-plane session, maintainer role
	userID       string

	keyValue      string // valid agk key, unbound (used with X-Agent-Workspace)
	boundKeyValue string // valid agk key bound to secops (ADR-0024, no header)
	revokedValue  string // revoked key
	expiredValue  string // expired key

	idp *idpStub

	// echoText makes the LLM upstream answer with exactly this content
	// (response-side DLP corpus). streamParts, when set, is streamed
	// verbatim as consecutive upstream SSE deltas (chunk-boundary corpus).
	// upstreamBodies records everything the provider ever received (egress
	// truth for the zero-leak assertions).
	echoMu         sync.Mutex
	echoText       string
	streamMu       sync.Mutex
	streamParts    []string
	upstreamMu     sync.Mutex
	upstreamBodies []string
}

func newAdversarialEnv(t *testing.T) *advEnv {
	t.Helper()
	env := &advEnv{sliceEnv: newSliceEnv(t, 1000)}
	ctx := context.Background()
	env.srv.OIDCStore = env.store

	// Principals and roles.
	env.userSession = mcpLogin(t, env.sliceEnv, "adv-emp@example.com", "secops")
	env.adminSession = mcpLogin(t, env.sliceEnv, "adv-admin@example.com", "secops")
	env.maintSession = mcpLogin(t, env.sliceEnv, "adv-maint@example.com", "secops")
	if _, err := env.pool.Exec(ctx,
		`UPDATE workspace_members SET roles = '{admin}' WHERE user_id = (SELECT id FROM users WHERE email='adv-admin@example.com')`); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	if _, err := env.pool.Exec(ctx,
		`UPDATE workspace_members SET roles = '{maintainer}' WHERE user_id = (SELECT id FROM users WHERE email='adv-maint@example.com')`); err != nil {
		t.Fatalf("grant maintainer: %v", err)
	}
	env.userID = mcpUserID(t, env.sliceEnv, "adv-emp@example.com")

	// Credential classes: all four key states are real api_keys rows.
	env.keyValue = seedKey(t, env.sliceEnv, env.userID)
	env.boundKeyValue = advPlantKey(t, env, func(k *advKeyShape) { k.bound = true })
	env.revokedValue = advPlantKey(t, env, func(k *advKeyShape) { k.bound, k.revoked = true, true })
	env.expiredValue = advPlantKey(t, env, func(k *advKeyShape) { k.bound, k.expired = true, true })

	// LLM routed plane: one local provider answering chat + embeddings.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)
		env.upstreamMu.Lock()
		env.upstreamBodies = append(env.upstreamBodies, bodyStr)
		env.upstreamMu.Unlock()
		env.echoMu.Lock()
		echo := env.echoText
		env.echoMu.Unlock()
		env.streamMu.Lock()
		parts := env.streamParts
		env.streamMu.Unlock()
		flusher, _ := w.(http.Flusher)
		if strings.Contains(bodyStr, `"stream":true`) || strings.Contains(bodyStr, `"stream": true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			chunk := func(delta string, done bool) {
				payload := map[string]any{"id": "adv", "object": "chat.completion.chunk", "model": "adv-fast",
					"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": delta}}}}
				if done {
					payload["choices"] = []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}
					payload["usage"] = map[string]int{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3}
				}
				b, _ := json.Marshal(payload)
				_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
				flusher.Flush()
			}
			if len(parts) > 0 {
				for _, p := range parts {
					chunk(p, false)
				}
			} else {
				chunk(echo, false)
			}
			chunk("", true)
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			return
		}
		if strings.HasSuffix(r.URL.Path, "/embeddings") {
			_, _ = w.Write([]byte(`{"data":[{"embedding":[0.4,0.2,0.1]}]}`))
			return
		}
		content := "adv benign echo"
		if strings.Contains(bodyStr, "ADV-ECHO-REQUEST") && echo != "" {
			content = echo
		}
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"adv-fast","choices":[{"index":0,"message":{"role":"assistant","content":` +
			advJSONQuote(content) + `}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("ADV_UPSTREAM_KEY", "adv-upstream-key")
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO providers (name, base_url, credential_ref) VALUES ('adv-primary', $1, 'env:ADV_UPSTREAM_KEY')
		 ON CONFLICT (name) DO UPDATE SET base_url = EXCLUDED.base_url`, upstream.URL); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO model_aliases (alias) VALUES ('adv-fast') ON CONFLICT DO NOTHING`,
		`INSERT INTO model_routes (alias, provider, model, priority) VALUES ('adv-fast','adv-primary','adv-fast',100) ON CONFLICT DO NOTHING`,
		`INSERT INTO model_pricing (alias, provider, input_per_mt, output_per_mt) VALUES ('adv-fast','adv-primary',1.0,2.0) ON CONFLICT (alias, provider) DO NOTHING`,
	} {
		if _, err := env.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed llm control data: %v", err)
		}
	}

	// Governance engines and the remaining conditional surfaces.
	env.srv.Policy = &policy.TablePolicy{LoadRules: env.store.LoadPolicyRules}
	scanner := dlp.NewTableScanner(env.store.LoadDLPRules)
	scanner.CacheTTL = 50 * time.Millisecond // near-live rule propagation for the corpus
	// Production tokenize wiring (main.go): vault-backed pseudonyms scoped to
	// the request's workspace. The workspace reaches this callback through
	// the scan context (withDLPSubject binds it for exactly this purpose).
	scanner.Tokenizer = func(ctx context.Context, ruleName, value string) (string, error) {
		return env.store.TokenizeDLP(ctx, dlp.WorkspaceFrom(ctx), ruleName, value)
	}
	scanner.TokenizeBatch = func(ctx context.Context, ruleName string, values []string) (map[string]string, error) {
		return env.store.BatchTokenizeDLP(ctx, dlp.WorkspaceFrom(ctx), ruleName, values)
	}
	env.srv.DLP = scanner
	env.srv.LLMRouter = &llmkernel.Router{
		ResolvePlan:   env.store.RoutePlanFor,
		ResolveTarget: env.store.ResolveProviderTarget,
		Credential:    env.srv.Credential,
		Client:        &llmkernel.OpenAICompatClient{},
	}
	env.srv.Knowledge = newKnowledgeService(t, env.sliceEnv)
	env.srv.Capabilities = &capability.Registry{}
	env.srv.LocalAuth = true
	env.srv.Members = env.store
	env.srv.Content = content.New(env.pool, env.store)
	env.srv.Contributions = contribution.New(env.pool, env.store)
	env.srv.Wiki = wiki.NewStore(env.pool, env.store)
	env.srv.Blobs = blob.New(env.pool, env.store, t.TempDir())
	env.srv.Curation = curation.New(env.pool, env.store, env.srv.Content)
	env.srv.Skills = skillshare.New(env.pool, env.store)
	env.srv.Catalog = advCatalog(t)

	// CLI dist: one fake binary is enough (the handler allowlists the name).
	dist := t.TempDir()
	if err := os.WriteFile(filepath.Join(dist, "eap-darwin-arm64"), []byte("adv-fake-eap-binary"), 0o644); err != nil {
		t.Fatalf("seed cli dist: %v", err)
	}
	env.srv.CLIDist = dist

	// Portal static: minimal dist so GET /{$} and the SPA catch-all exist.
	webDist := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDist, "index.html"), []byte("<html>adv portal</html>"), 0o644); err != nil {
		t.Fatalf("seed web dist: %v", err)
	}
	site, err := gateway.NewStaticSite(webDist)
	if err != nil {
		t.Fatalf("static site: %v", err)
	}
	env.srv.Web = site

	// Skill release signing key (pkg- namespace, purpose-separated).
	advSeedPackageKey(t, env)

	// Production OIDC boundary against the shared IdP stub.
	env.idp = newIDPStub(t)
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO idp_group_workspaces (idp_group, workspace_id) VALUES ('secops-team', 'secops')
		 ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed group mapping: %v", err)
	}
	env.srv.OIDC = &oidc.Config{
		Issuer: env.idp.issuer, ClientID: "eap-portal", ClientSecret: "eap-secret",
		RedirectURL: "http://127.0.0.1:18081/enterprise/v1/auth/oidc/callback",
		Scopes:      []string{"openid", "email", "profile", "groups"},
	}
	env.srv.JWKSFetch = jwksFrom

	env.handler = env.srv.Handler()

	// Local account for the local-auth rows (created through the governed
	// admin surface, exercising that endpoint during setup).
	rec := env.do(t, http.MethodPost, "/enterprise/v1/admin/local-accounts", env.adminSession,
		`{"username":"advlocal","password":"adv-local-matrix-pass","email":"advlocal@example.com","workspace":"secops"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed local account: %d %s", rec.Code, rec.Body.String())
	}
	// The matrix logs in with it directly; the one-time-password flow is covered elsewhere.
	if _, err := env.pool.Exec(ctx, `UPDATE local_credentials SET must_change_password = false WHERE username = 'advlocal'`); err != nil {
		t.Fatalf("seed local account: %v", err)
	}

	// Portal cookie session for the cookie-scoped Portal rows.
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO portal_sessions (session_id, user_id, email, display_name, idp_subject, workspaces, expires_at)
		VALUES ('adv-portal-session', $1, 'adv-emp@example.com', 'Adv Emp', 'adv-idp-subject', '{secops}', now() + interval '15 minutes')
		ON CONFLICT (session_id) DO NOTHING`, env.userID); err != nil {
		t.Fatalf("seed portal session: %v", err)
	}
	return env
}

var advLocalSeq atomic.Int64

// advKeyShape tweaks the planted key row: ADR-0024 binding, revocation, expiry.
type advKeyShape struct {
	bound   bool
	revoked bool
	expired bool
}

// advPlantKey plants one agk key row for the adversarial principal in the
// requested state and returns the full one-time value.
func advPlantKey(t *testing.T, env *advEnv, tweak func(*advKeyShape)) string {
	t.Helper()
	shape := &advKeyShape{}
	if tweak != nil {
		tweak(shape)
	}
	g, err := apikey.Generate(apikey.EnvLive)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	var expires *time.Time
	if shape.expired {
		past := time.Now().Add(-time.Minute)
		expires = &past
	}
	key, err := env.store.CreateAPIKey(context.Background(), env.userID,
		apikey.Hash(g.Full), g.Prefix, "adv-adversarial", map[bool]string{true: "secops", false: ""}[shape.bound],
		expires, audit.NewEvent(audit.EventAuthKeyCreated, audit.OutcomeAllowed))
	if err != nil {
		t.Fatalf("plant key: %v", err)
	}
	if shape.revoked {
		if _, err := env.store.RevokeAPIKey(context.Background(), env.userID, key.KeyID,
			audit.NewEvent(audit.EventAuthKeyRevoked, audit.OutcomeAllowed)); err != nil {
			t.Fatalf("revoke planted key: %v", err)
		}
	}
	return g.Full
}

func advSeedPackageKey(t *testing.T, env *advEnv) {
	t.Helper()
	keyPub, keyPriv, err := ed25519Generate()
	if err != nil {
		t.Fatalf("generate package key: %v", err)
	}
	if _, err := env.pool.Exec(context.Background(),
		`INSERT INTO skill_package_keys (kid, public_key, active) VALUES ('pkg-adv', $1, true)
		 ON CONFLICT (kid) DO UPDATE SET public_key = EXCLUDED.public_key, active = true, retired_at = NULL`,
		base64Encode(keyPub)); err != nil {
		t.Fatalf("seed package key: %v", err)
	}
	env.srv.PackageKeys = map[string]ed25519.PrivateKey{"pkg-adv": keyPriv}
}

func (e *advEnv) upstreamCount() int {
	e.upstreamMu.Lock()
	defer e.upstreamMu.Unlock()
	return len(e.upstreamBodies)
}

func (e *advEnv) upstreamAll() []string {
	e.upstreamMu.Lock()
	defer e.upstreamMu.Unlock()
	out := make([]string, len(e.upstreamBodies))
	copy(out, e.upstreamBodies)
	return out
}

func advJSONQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func advTruncate(s string) string {
	if len(s) > 220 {
		return s[:220] + "..."
	}
	return s
}

// ---------------------------------------------------------------------------
// 1. authorization matrix sweep
// ---------------------------------------------------------------------------

// credential classes (matrix columns).
const (
	clsNone    = "no_auth"
	clsKey     = "agk_key+ws_header"
	clsBound   = "agk_key_bound"
	clsRevoked = "revoked_key"
	clsExpired = "expired_key"
	clsSession = "session_jwt"
	clsForeign = "key+foreign_ws"
	clsOwner   = "authorized_actor"
)

var matrixClasses = []string{clsNone, clsKey, clsBound, clsRevoked, clsExpired, clsSession, clsForeign, clsOwner}

// probe is one concrete HTTP request for a matrix row/class.
type probe struct {
	path    string
	body    string
	headers map[string]string
}

// matrixRow is one endpoint (matched 1:1 against the AST route inventory).
type matrixRow struct {
	method  string
	pattern string // must equal the HandleFunc pattern in server.go
	kind    string // public | session | apikey | catalog | admin | maintainer | manual
	success int    // status for authorized cells (default 200)
	owner   string // admin | maint | portal | sess | oidc ("" = skip owner cell)
	want    map[string]int
	// wsAgnostic marks endpoints that never establish workspace authority
	// (e.g. /me lists the caller's own memberships) — the foreign-workspace
	// success invariant does not apply to them.
	wsAgnostic bool
	probe      func(t *testing.T, e *advEnv, cls string) probe
}

// expectedStatus merges kind defaults with the row's explicit overrides.
// A negative result means "no cell for this class" (manual rows only).
func (r matrixRow) expectedStatus(cls string) int {
	if v, ok := r.want[cls]; ok {
		return v
	}
	success := r.success
	if success == 0 {
		success = http.StatusOK
	}
	switch r.kind {
	case "public":
		// Anonymous by contract: every bearer class gets the same answer
		// because credentials are not consulted at all.
		return success
	case "session":
		if cls == clsSession || cls == clsOwner {
			return success
		}
		return http.StatusUnauthorized
	case "apikey":
		switch cls {
		case clsKey, clsBound, clsOwner:
			return success
		case clsForeign:
			return http.StatusForbidden // membership revalidated: explicit denial
		default:
			return http.StatusUnauthorized
		}
	case "catalog":
		switch cls {
		case clsKey, clsBound, clsSession, clsOwner:
			return success
		case clsForeign:
			return http.StatusForbidden
		default:
			return http.StatusUnauthorized
		}
	case "admin", "maintainer":
		switch cls {
		case clsOwner:
			return success
		case clsSession:
			return http.StatusForbidden // right credential class, missing role
		case clsForeign:
			return http.StatusUnauthorized // keys are the wrong credential class here
		default:
			return http.StatusUnauthorized
		}
	case "manual":
		return -1
	default:
		panic("unknown matrix kind " + r.kind)
	}
}

// credential maps a class onto (bearer, workspace header). The session class
// carries the workspace header the Portal really sends; the bound-key class
// deliberately carries none — that is the ADR-0024 contract under test.
func (e *advEnv) credential(t *testing.T, row matrixRow, cls string) (string, string) {
	t.Helper()
	switch cls {
	case clsNone:
		return "", ""
	case clsKey:
		return e.keyValue, "secops"
	case clsBound:
		return e.boundKeyValue, ""
	case clsRevoked:
		return e.revokedValue, "secops"
	case clsExpired:
		return e.expiredValue, "secops"
	case clsSession:
		return e.userSession, "secops"
	case clsForeign:
		return e.keyValue, "finance"
	case clsOwner:
		switch row.owner {
		case "admin":
			return e.adminSession, "secops"
		case "maint":
			return e.maintSession, "secops"
		case "portal":
			return "", "" // authority is the cookie carried by the probe
		default:
			return e.userSession, "secops"
		}
	default:
		t.Fatalf("unknown class %q", cls)
		return "", ""
	}
}

var advCounter atomic.Int64

func advUnique(format string) string {
	return fmt.Sprintf(format, advCounter.Add(1))
}

// advThrowawayKeyID plants a fresh key row so DELETE probes never interfere.
func advThrowawayKeyID(t *testing.T, e *advEnv) string {
	t.Helper()
	g, err := apikey.Generate(apikey.EnvLive)
	if err != nil {
		t.Fatalf("generate throwaway: %v", err)
	}
	key, err := e.store.CreateAPIKey(context.Background(), e.userID,
		apikey.Hash(g.Full), g.Prefix, "adv-throwaway", "", nil,
		audit.NewEvent(audit.EventAuthKeyCreated, audit.OutcomeAllowed))
	if err != nil {
		t.Fatalf("plant throwaway: %v", err)
	}
	return key.KeyID
}

// advDeviceFlowRefresh drives the full device flow and returns the raw
// refresh credential (documented one-time display).
func advDeviceFlowRefresh(t *testing.T, e *advEnv) string {
	t.Helper()
	rec := e.do(t, http.MethodPost, "/enterprise/v1/auth/device/code", "", "", nil)
	var code struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &code)
	rec = e.do(t, http.MethodPost, "/enterprise/v1/auth/device/approve", e.userSession,
		`{"user_code":"`+code.UserCode+`"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("device approve: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, http.MethodPost, "/enterprise/v1/auth/device/token", "",
		`{"grant_type":"urn:ietf:params:oauth:grant-type:device_code","device_code":"`+code.DeviceCode+`"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("device token: %d %s", rec.Code, rec.Body.String())
	}
	var tok struct {
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	return tok.RefreshToken
}

// advOIDCCallbackProbe completes a real IdP round trip for the owner cell.
func advOIDCCallbackProbe(t *testing.T, e *advEnv) probe {
	t.Helper()
	start := e.do(t, http.MethodGet, "/enterprise/v1/auth/oidc/login", "", "", nil)
	if start.Code != http.StatusFound {
		t.Fatalf("oidc login: %d", start.Code)
	}
	var state, nonce, verifier string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT state, nonce, code_verifier FROM oidc_flow_states ORDER BY created_at DESC LIMIT 1`).
		Scan(&state, &nonce, &verifier); err != nil {
		t.Fatalf("flow state: %v", err)
	}
	code := e.idp.authorize(verifier, nonce)
	return probe{path: "/enterprise/v1/auth/oidc/callback?state=" + url.QueryEscape(state) + "&code=" + url.QueryEscape(code)}
}

// advSeedSkillPackage inserts a skill package row in the requested status so
// the review/release/deprecate/revoke/download probes hit real state.
func advSeedSkillPackage(t *testing.T, e *advEnv, name, version, status string) {
	t.Helper()
	files := []map[string]any{{"path": "SKILL.md", "content": "# adv benign skill"}}
	filesJSON, _ := json.Marshal(files)
	var archive []byte
	var digest string
	// BuildArchive takes typed files; reuse the JSON round trip shape used by
	// the publish handler's request binding.
	var typed []skillreg.PackageFile
	_ = json.Unmarshal(filesJSON, &typed)
	archive, digest, err := skillreg.BuildArchive(typed)
	if err != nil {
		t.Fatalf("build archive: %v", err)
	}
	if _, err := e.pool.Exec(context.Background(), `
		INSERT INTO skill_packages (name, version, workspace_id, digest, payload, status, published_by)
		VALUES ($1, $2, 'secops', $3, $4, $5, $6)
		ON CONFLICT (name, version) DO UPDATE SET status = EXCLUDED.status`,
		name, version, digest, archive, status, e.userID); err != nil {
		t.Fatalf("seed skill package %s@%s: %v", name, version, err)
	}
}

const advCatalogArchive = "YuanpuEchoMcp-linux-x64.tar.gz"

// advCatalog publishes a fixture manifest and archive in a temp artifact root.
func advCatalog(t *testing.T) *catalog.Directory {
	t.Helper()
	root := t.TempDir()
	manifest := `{"id":"builtin.python.echo","version":"9.8.7","artifacts":[{"url":"artifacts/` + advCatalogArchive + `"}]}`
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("catalog manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, advCatalogArchive), []byte("signed-fixture"), 0o600); err != nil {
		t.Fatalf("catalog archive: %v", err)
	}
	d, err := catalog.New(root)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	return d
}

// advSeedFlagProposal plants an auto-applied flag proposal on a fresh page.
func advSeedFlagProposal(t *testing.T, e *advEnv) string {
	t.Helper()
	ctx := context.Background()
	pageID, _ := advSeedWikiPage(t, e)
	var flagID int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO content_flags (resource_id, kind) VALUES ($1, 'conflict') RETURNING id`, pageID).Scan(&flagID); err != nil {
		t.Fatalf("seed flag: %v", err)
	}
	id := advUnique("prp_adv%06d")
	if _, err := e.pool.Exec(ctx, `INSERT INTO curation_proposals (id, workspace_id, kind, risk, status, target_resource_id, payload, sources, rationale, revert)
		VALUES ($1, 'secops', 'flag_conflict', 'auto', 'auto_applied', $2, '{}', '[]', 'adv', jsonb_build_object('clear_flag', $3::bigint))`,
		id, pageID, flagID); err != nil {
		t.Fatalf("seed proposal: %v", err)
	}
	return id
}

// advSeedShelf plants an empty shelf in secops.
func advSeedShelf(t *testing.T, e *advEnv) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO wiki_nodes (workspace_id, level, slug, title)
		VALUES ('secops', 'shelf', $1, 'adv') RETURNING id`, advUnique("adv-node-%06d")).Scan(&id); err != nil {
		t.Fatalf("seed shelf: %v", err)
	}
	return id
}

// advSeedWikiPage plants a published organization wiki page under a fresh
// shelf/book/chapter and returns its id and path.
func advSeedWikiPage(t *testing.T, e *advEnv) (string, string) {
	t.Helper()
	ctx := context.Background()
	parent := ""
	path := ""
	for _, level := range []string{"shelf", "book", "chapter"} {
		slug := advUnique("adv-" + level + "-%06d")
		var p *string
		if parent != "" {
			p = &parent
		}
		if err := e.pool.QueryRow(ctx, `INSERT INTO wiki_nodes (workspace_id, parent_id, level, slug, title)
			VALUES ('secops', $1, $2, $3, 'adv') RETURNING id`, p, level, slug).Scan(&parent); err != nil {
			t.Fatalf("seed %s: %v", level, err)
		}
		path += slug + "/"
	}
	id := advUnique("res_advwiki%06d")
	if _, err := e.pool.Exec(ctx, `INSERT INTO content_resources (id, workspace_id, kind, owner_id, visibility, node_id, slug)
		VALUES ($1, 'secops', 'wiki', $2, 'organization', $3, 'page')`, id, e.userID, parent); err != nil {
		t.Fatalf("seed page: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO content_revisions (resource_id, revision, title, summary, body, status, created_by)
		VALUES ($1, 1, 'adv page', '', 'benign', 'approved', $2)`, id, e.userID); err != nil {
		t.Fatalf("seed page revision: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE content_resources SET published_revision = 1 WHERE id = $1`, id); err != nil {
		t.Fatalf("publish page: %v", err)
	}
	return id, path + "page"
}

// advSeedContribution plants a received contribution of the developer principal.
func advSeedContribution(t *testing.T, e *advEnv) string {
	t.Helper()
	ctx := context.Background()
	id := advUnique("ctb_adv%06d")
	if _, err := e.pool.Exec(ctx, `INSERT INTO contributions (id, workspace_id, author_id, local_id, current_version, status, scope)
		VALUES ($1, 'secops', $2, $1, 1, 'received', 'organization')`, id, e.userID); err != nil {
		t.Fatalf("seed contribution: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO contribution_versions (contribution_id, version, content_hash, title, summary, markdown,
		details, links, source_refs, attachments, scope, upload_mode, client)
		VALUES ($1, 1, 'sha256:adv', 'adv', '', 'benign', '{}', '[]', '[]', '[]', 'organization', 'manual', '{}')`, id); err != nil {
		t.Fatalf("seed contribution version: %v", err)
	}
	return id
}

// advSeedWikiAttachment plants a published page citing a contribution with
// one stored text attachment; it returns the page id and the attachment hash.
func advSeedWikiAttachment(t *testing.T, e *advEnv) (string, string) {
	t.Helper()
	ctx := context.Background()
	page, _ := advSeedWikiPage(t, e)
	body := advUnique("adv page attachment %06d")
	digest := sha256.Sum256([]byte(body))
	sum := hex.EncodeToString(digest[:])
	if _, _, err := e.srv.Blobs.Put(ctx, "secops", e.userID, sum, "text/plain", strings.NewReader(body),
		audit.NewEvent("blob", audit.OutcomeAllowed)); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	contribution := advSeedContribution(t, e)
	attachments := fmt.Sprintf(`[{"sha256":%q,"name":"adv.txt","media_type":"text/plain","size":%d}]`, sum, len(body))
	if _, err := e.pool.Exec(ctx, `UPDATE contribution_versions SET attachments = $2 WHERE contribution_id = $1`, contribution, attachments); err != nil {
		t.Fatalf("seed attachment: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO content_revision_sources (resource_id, revision, contribution_id, contribution_version, anchor)
		VALUES ($1, 1, $2, 1, '')`, page, contribution); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	return page, sum
}

// advSeedResource plants an organization news item owned by the developer
// principal in secops, at the given revision status ("approved" publishes it).
func advSeedResource(t *testing.T, e *advEnv, status string) string {
	t.Helper()
	ctx := context.Background()
	id := advUnique("res_adv%06d")
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO content_resources (id, workspace_id, kind, owner_id, visibility)
		VALUES ($1, 'secops', 'news', $2, 'organization')`, id, e.userID); err != nil {
		t.Fatalf("seed resource: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO content_revisions (resource_id, revision, title, summary, body, status, created_by)
		VALUES ($1, 1, 'adv seeded', '', 'benign', $2, $3)`, id, status, e.userID); err != nil {
		t.Fatalf("seed revision: %v", err)
	}
	if status == "approved" {
		if _, err := e.pool.Exec(ctx, `UPDATE content_resources SET published_revision = 1 WHERE id = $1`, id); err != nil {
			t.Fatalf("publish seed: %v", err)
		}
	}
	return id
}

// advSeedMember plants a throwaway developer member of secops per class so a
// DELETE cell never removes a principal another row depends on.
func advSeedMember(t *testing.T, e *advEnv, cls string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO users (idp_subject, email, display_name, organization_id, department_id)
		VALUES ($1, $2, 'adv throwaway', 'org_default', 'dept_platform')
		ON CONFLICT (idp_subject) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id`, "adv-throwaway:"+cls, "adv-throwaway-"+cls+"@example.com").Scan(&id); err != nil {
		t.Fatalf("seed member %s: %v", cls, err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO workspace_members (workspace_id, user_id, roles) VALUES ('secops', $1, '{developer}')
		ON CONFLICT DO NOTHING`, id); err != nil {
		t.Fatalf("seed membership %s: %v", cls, err)
	}
	return id
}

// advSeedGroup plants a secops group with a per-call unique name.
func advSeedGroup(t *testing.T, e *advEnv, name string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `
		INSERT INTO workspace_groups (workspace_id, name) VALUES ('secops', $1)
		ON CONFLICT (workspace_id, name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id`, "adv "+name).Scan(&id); err != nil {
		t.Fatalf("seed group %s: %v", name, err)
	}
	return id
}

// advSeedAgentRelease plants a released harness release for darwin/arm64.
func advSeedAgentRelease(t *testing.T, e *advEnv) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `
		INSERT INTO agent_releases (client, version, os, arch, minimum_version, digest, payload, signature, signed_by, workspace_id)
		VALUES ('harness', '2.0.0', 'darwin', 'arm64', '1.0.0', 'advdeadbeef', 'adv-payload-bytes', 'adv-sig', 'pkg-adv', 'secops')
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed agent release: %v", err)
	}
}

func matrixRows(t *testing.T) []matrixRow {
	t.Helper()
	return []matrixRow{
		// --- public surfaces -------------------------------------------------
		{method: http.MethodGet, pattern: "GET /healthz", kind: "public",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/healthz"} }},
		{method: http.MethodGet, pattern: "GET /readyz", kind: "public",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/readyz"} }},
		{method: http.MethodGet, pattern: "GET /debug/request", kind: "public",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/debug/request"} }},
		{method: http.MethodGet, pattern: "GET /.well-known/jwks.json", kind: "public",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/.well-known/jwks.json"} }},
		{method: http.MethodGet, pattern: "GET /.well-known/package-keys.json", kind: "public",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/.well-known/package-keys.json"} }},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/auth/methods", kind: "public",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/auth/methods"} }},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/auth/dev-login", kind: "public",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/auth/dev-login",
					body: `{"email":"adv-devlogin@example.com","workspaces":["secops"]}`}
			}},
		{method: http.MethodGet, pattern: "GET /{$}", kind: "public", success: http.StatusFound,
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/"} }},
		{method: http.MethodGet, pattern: "GET /cli/install.sh", kind: "public",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/cli/install.sh"} }},
		{method: http.MethodGet, pattern: "GET /cli/install/sha256sums", kind: "public",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/cli/install/sha256sums"} }},
		{method: http.MethodGet, pattern: "GET /cli/install/manifest.json", kind: "public",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/cli/install/manifest.json"} }},
		{method: http.MethodGet, pattern: "GET /cli/install/{platform}", kind: "public",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/cli/install/darwin-arm64"} }},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/auth/device/code", kind: "public",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/auth/device/code"} }},
		// Device approve/deny: a control-plane session authorizes a decision
		// on a FRESH pending code per class; every bearer key class fails the
		// session check before the code is consulted.
		{method: http.MethodPost, pattern: "POST /enterprise/v1/auth/device/approve", kind: "session",
			probe: func(t *testing.T, e *advEnv, cls string) probe {
				rec := e.do(t, http.MethodPost, "/enterprise/v1/auth/device/code", "", "", nil)
				var code struct {
					UserCode string `json:"user_code"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &code)
				return probe{path: "/enterprise/v1/auth/device/approve",
					body: `{"user_code":"` + code.UserCode + `"}`}
			}},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/auth/device/deny", kind: "session",
			probe: func(t *testing.T, e *advEnv, cls string) probe {
				rec := e.do(t, http.MethodPost, "/enterprise/v1/auth/device/code", "", "", nil)
				var code struct {
					UserCode string `json:"user_code"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &code)
				return probe{path: "/enterprise/v1/auth/device/deny",
					body: `{"user_code":"` + code.UserCode + `"}`}
			}},
		// Local login: the password IS the credential — no bearer is consulted,
		// so every class gets the identical (documented) answer.
		{method: http.MethodPost, pattern: "POST /enterprise/v1/auth/local/login", kind: "public",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/auth/local/login",
					body: `{"username":"advlocal","password":"adv-local-matrix-pass"}`}
			}},
		// Device token polling: the owner completes a pre-approved flow (200);
		// every other class only holds a fresh UNKNOWN code (expired_token 400).
		{method: http.MethodPost, pattern: "POST /enterprise/v1/auth/device/token", kind: "manual", owner: "sess",
			want: map[string]int{
				clsOwner: http.StatusOK,
				clsNone:  http.StatusBadRequest, clsKey: http.StatusBadRequest, clsBound: http.StatusBadRequest,
				clsRevoked: http.StatusBadRequest, clsExpired: http.StatusBadRequest,
				clsSession: http.StatusBadRequest, clsForeign: http.StatusBadRequest,
			},
			probe: func(t *testing.T, e *advEnv, cls string) probe {
				if cls == clsOwner {
					rec := e.do(t, http.MethodPost, "/enterprise/v1/auth/device/code", "", "", nil)
					var code struct {
						DeviceCode string `json:"device_code"`
						UserCode   string `json:"user_code"`
					}
					_ = json.Unmarshal(rec.Body.Bytes(), &code)
					if rec := e.do(t, http.MethodPost, "/enterprise/v1/auth/device/approve", e.userSession,
						`{"user_code":"`+code.UserCode+`"}`, nil); rec.Code != http.StatusOK {
						t.Fatalf("device approve: %d %s", rec.Code, rec.Body.String())
					}
					return probe{path: "/enterprise/v1/auth/device/token",
						body: `{"grant_type":"urn:ietf:params:oauth:grant-type:device_code","device_code":"` + code.DeviceCode + `"}`}
				}
				return probe{path: "/enterprise/v1/auth/device/token",
					body: `{"grant_type":"urn:ietf:params:oauth:grant-type:device_code","device_code":"` + advUnique("adv-unknown-%d") + `"}`}
			}},
		// Device logout: possession of a refresh token is the authority
		// (RFC-7009-style). Every class presenting a FRESH valid refresh token
		// succeeds — the sweep proves no bearer escalation is required.
		{method: http.MethodPost, pattern: "POST /enterprise/v1/auth/device/logout", kind: "public", success: http.StatusNoContent,
			probe: func(t *testing.T, e *advEnv, cls string) probe {
				return probe{path: "/enterprise/v1/auth/device/logout",
					body: `{"refresh_token":"` + advDeviceFlowRefresh(t, e) + `"}`}
			}},
		// OIDC: login redirects everyone; callback completes only with a real
		// flow state (owner); logout is cookie-scoped and answers 204 to all;
		// profile is cookie-scoped (portal owner, every bearer class fails).
		{method: http.MethodGet, pattern: "GET /enterprise/v1/auth/oidc/login", kind: "public", success: http.StatusFound,
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/auth/oidc/login"} }},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/auth/oidc/callback", kind: "manual", owner: "oidc",
			want: map[string]int{
				clsOwner: http.StatusFound,
				clsNone:  http.StatusUnauthorized, clsKey: http.StatusUnauthorized, clsBound: http.StatusUnauthorized,
				clsRevoked: http.StatusUnauthorized, clsExpired: http.StatusUnauthorized,
				clsSession: http.StatusUnauthorized, clsForeign: http.StatusUnauthorized,
			},
			probe: func(t *testing.T, e *advEnv, cls string) probe {
				if cls == clsOwner {
					return advOIDCCallbackProbe(t, e)
				}
				return probe{path: "/enterprise/v1/auth/oidc/callback?state=adv-bogus&code=adv-bogus"}
			}},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/auth/oidc/logout", kind: "public", success: http.StatusNoContent,
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/auth/oidc/logout"} }},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/auth/oidc/profile", kind: "manual", owner: "portal",
			want: map[string]int{
				clsOwner: http.StatusOK,
				clsNone:  http.StatusUnauthorized, clsKey: http.StatusUnauthorized, clsBound: http.StatusUnauthorized,
				clsRevoked: http.StatusUnauthorized, clsExpired: http.StatusUnauthorized,
				clsSession: http.StatusUnauthorized, clsForeign: http.StatusUnauthorized,
			},
			probe: func(t *testing.T, e *advEnv, cls string) probe {
				if cls == clsOwner {
					// Authority is the HttpOnly cookie; no bearer is consulted.
					return probe{path: "/enterprise/v1/auth/oidc/profile",
						headers: map[string]string{"Cookie": oidc.SessionCookieName + "=adv-portal-session"}}
				}
				return probe{path: "/enterprise/v1/auth/oidc/profile"}
			}},

		// --- control-plane (session) surfaces --------------------------------
		{method: http.MethodPost, pattern: "POST /enterprise/v1/keys", kind: "session", success: http.StatusCreated,
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/keys", body: `{"name":"adv-matrix"}`}
			}},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/keys", kind: "session",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/keys"} }},
		{method: http.MethodDelete, pattern: "DELETE /enterprise/v1/keys/{keyID}", kind: "session", success: http.StatusNoContent,
			probe: func(t *testing.T, e *advEnv, cls string) probe {
				return probe{path: "/enterprise/v1/keys/" + advThrowawayKeyID(t, e)}
			}},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/profile", kind: "session",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/profile"} }},

		// --- agent surfaces (employee API key) -------------------------------
		// /me is identity-informational: it lists the CALLER's own
		// memberships and never establishes workspace authority, so the
		// foreign header is simply ignored (contract: 200, own data only).
		{method: http.MethodGet, pattern: "GET /enterprise/v1/me", kind: "apikey", wsAgnostic: true,
			want:  map[string]int{clsForeign: http.StatusOK},
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/me"} }},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/token/exchange", kind: "apikey",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/token/exchange",
					body: `{"audience":"mcp-gateway","ttl_seconds":300}`}
			}},
		{method: http.MethodPost, pattern: "POST /v1/chat/completions", kind: "apikey",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/v1/chat/completions",
					body: `{"model":"adv-fast","messages":[{"role":"user","content":"adv matrix benign"}]}`}
			}},
		{method: http.MethodGet, pattern: "GET /v1/models", kind: "apikey",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/v1/models"} }},
		{method: http.MethodPost, pattern: "POST /v1/responses", kind: "apikey",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/v1/responses", body: `{"model":"adv-fast","input":"adv matrix benign"}`}
			}},
		{method: http.MethodPost, pattern: "POST /v1/embeddings", kind: "apikey",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/v1/embeddings", body: `{"model":"adv-fast","input":"adv matrix benign"}`}
			}},
		{method: http.MethodPost, pattern: "POST /v1/messages", kind: "apikey",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/v1/messages",
					body: `{"model":"adv-fast","max_tokens":16,"messages":[{"role":"user","content":"adv matrix benign"}]}`}
			}},
		{method: http.MethodPost, pattern: "POST /mcp/{server}", kind: "apikey",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/mcp/git",
					body: `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`}
			}},
		{method: http.MethodPost, pattern: "POST /mcp/agent", kind: "apikey",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/mcp/agent",
					body: `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`}
			}},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/cli/bootstrap", kind: "catalog",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/cli/bootstrap"} }},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/dlp/reveal", kind: "catalog",
			probe: func(t *testing.T, e *advEnv, cls string) probe {
				tok, err := e.store.TokenizeDLP(context.Background(), "secops", "internal_phone", "13800138000")
				if err != nil {
					t.Fatalf("tokenize for reveal probe: %v", err)
				}
				return probe{path: "/enterprise/v1/dlp/reveal", body: `{"tokens":["` + tok + `"]}`}
			}},

		// --- catalog surfaces (key or session) --------------------------------
		{method: http.MethodGet, pattern: "GET /enterprise/v1/skills", kind: "catalog",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/skills"} }},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/agents/{client}", kind: "catalog",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/agents/harness?os=darwin&arch=arm64"}
			}},

		// --- skill sharing (key or session; scope decided per skill) ----------
		{method: http.MethodPost, pattern: "POST /enterprise/v1/skills/{name}/versions", kind: "catalog", success: http.StatusCreated,
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/skills/adv-skill-pub/versions",
					body: `{"version":"` + advUnique("1.%d.0") + `","files":[{"path":"SKILL.md","content":"benign"}]}`}
			}},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/me/skills", kind: "catalog",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/me/skills"} }},
		{method: http.MethodPut, pattern: "PUT /enterprise/v1/me/skills/{name}", kind: "catalog", success: http.StatusNoContent,
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/me/skills/adv-skill-dl"} }},
		{method: http.MethodDelete, pattern: "DELETE /enterprise/v1/me/skills/{name}", kind: "catalog", success: http.StatusNoContent,
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/me/skills/adv-skill-dl"} }},
		// Requiring needs skill.require: members authenticate but are refused.
		{method: http.MethodPut, pattern: "PUT /enterprise/v1/skills/{name}/requirement", kind: "catalog", owner: "admin",
			success: http.StatusNoContent, want: map[string]int{clsKey: http.StatusForbidden, clsBound: http.StatusForbidden, clsSession: http.StatusForbidden},
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/skills/adv-skill-dl/requirement"}
			}},
		{method: http.MethodDelete, pattern: "DELETE /enterprise/v1/skills/{name}/requirement", kind: "catalog", owner: "admin",
			success: http.StatusNoContent, want: map[string]int{clsKey: http.StatusForbidden, clsBound: http.StatusForbidden, clsSession: http.StatusForbidden},
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/skills/adv-skill-dl/requirement"}
			}},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/skill-reviews", kind: "catalog",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/skill-reviews"} }},
		// The developer principal authored the seeded skills: it may read them.
		{method: http.MethodGet, pattern: "GET /enterprise/v1/skills/{name}/versions/{version}/content", kind: "catalog",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/skills/adv-skill-dl/versions/9.9.1/content"}
			}},
		// Rejecting needs skill.review and never the author: the maintainer decides.
		{method: http.MethodPost, pattern: "POST /enterprise/v1/skills/{name}/versions/{version}/reject", kind: "catalog", owner: "maint",
			want: map[string]int{clsKey: http.StatusForbidden, clsBound: http.StatusForbidden, clsSession: http.StatusForbidden},
			probe: func(t *testing.T, e *advEnv, cls string) probe {
				version := advUnique("8.%d.0")
				advSeedSkillPackage(t, e, "adv-skill-rej", version, "pending_review")
				return probe{path: "/enterprise/v1/skills/adv-skill-rej/versions/" + version + "/reject", body: `{"reason":"needs sources"}`}
			}},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/skills/{name}/versions/{version}/download", kind: "apikey",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/skills/adv-skill-dl/versions/9.9.1/download"}
			}},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/knowledge/documents", kind: "apikey", success: http.StatusCreated,
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/knowledge/documents",
					body: `{"title":"adv-doc-` + advUnique("%d") + `","content":"adv benign knowledge body"}`}
			}},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/knowledge/search", kind: "apikey",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/knowledge/search", body: `{"query":"benign"}`}
			}},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/agents/{client}/ticket", kind: "apikey",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/agents/harness/ticket",
					body: `{"version":"2.0.0","os":"darwin","arch":"arm64"}`}
			}},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/agents/{client}/download", kind: "apikey",
			probe: func(t *testing.T, e *advEnv, cls string) probe {
				// Fresh single-use ticket minted through the API per probe.
				rec := e.do(t, http.MethodPost, "/enterprise/v1/agents/harness/ticket", e.keyValue,
					`{"version":"2.0.0","os":"darwin","arch":"arm64"}`,
					map[string]string{identity.HeaderWorkspace: "secops"})
				var tk struct {
					Ticket string `json:"ticket"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &tk)
				return probe{path: "/enterprise/v1/agents/harness/download",
					body: `{"ticket":"` + tk.Ticket + `"}`}
			}},

		// --- skill governance (maintainer control-plane) ----------------------
		{method: http.MethodPost, pattern: "POST /enterprise/v1/skills/{name}/versions/{version}/review", kind: "maintainer", owner: "maint",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/skills/adv-skill-rev/versions/9.9.2/review"}
			}},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/skills/{name}/versions/{version}/release", kind: "maintainer", owner: "maint",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/skills/adv-skill-rel/versions/9.9.3/release"}
			}},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/skills/{name}/versions/{version}/deprecate", kind: "maintainer", owner: "maint",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/skills/adv-skill-dep/versions/9.9.4/deprecate"}
			}},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/skills/{name}/versions/{version}/revoke", kind: "maintainer", owner: "maint",
			success: http.StatusNoContent,
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/skills/adv-skill-rev2/versions/9.9.5/revoke"}
			}},

		// --- admin inspection --------------------------------------------------
		{method: http.MethodGet, pattern: "GET /enterprise/v1/admin/users", kind: "admin", owner: "admin",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/admin/users"} }},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/admin/agents", kind: "admin", owner: "admin",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/admin/agents"} }},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/admin/models", kind: "admin", owner: "admin",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/admin/models"} }},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/admin/usage", kind: "admin", owner: "admin",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/admin/usage"} }},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/admin/mcp", kind: "admin", owner: "admin",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/admin/mcp"} }},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/admin/knowledge", kind: "admin", owner: "admin",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/admin/knowledge"} }},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/admin/dlp", kind: "admin", owner: "admin",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/admin/dlp"} }},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/admin/audit", kind: "admin", owner: "admin",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/admin/audit"} }},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/admin/dlp/rules", kind: "admin", owner: "admin",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/admin/dlp/rules"} }},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/admin/dlp/test", kind: "admin", owner: "admin",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/admin/dlp/test", body: `{"content":"adv benign probe text"}`}
			}},
		{method: http.MethodPatch, pattern: "PATCH /enterprise/v1/admin/dlp/rules/{name}", kind: "admin", owner: "admin",
			probe: func(*testing.T, *advEnv, string) probe {
				// Idempotent no-op: redact -> redact on password_field.
				return probe{path: "/enterprise/v1/admin/dlp/rules/password_field", body: `{"action":"redact"}`}
			}},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/admin/local-accounts", kind: "admin", owner: "admin",
			success: http.StatusCreated,
			probe: func(*testing.T, *advEnv, string) probe {
				// Existing usernames are refused, so every probe creates a new one.
				n := advLocalSeq.Add(1)
				return probe{path: "/enterprise/v1/admin/local-accounts",
					body: fmt.Sprintf(`{"username":"advlocal-%d","password":"adv-local-matrix-pass","email":"advlocal-%d@example.com","workspace":"secops"}`, n, n)}
			}},

		// --- skill/capability directory merged with the Skill Registry ---------
		{method: http.MethodGet, pattern: "GET /enterprise/v1/catalog/search", kind: "catalog",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/catalog/search?q=python"} }},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/catalog/items/{id}", kind: "catalog",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/catalog/items/builtin.python.echo"}
			}},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/capability-packages/{id}/manifest", kind: "catalog",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/capability-packages/builtin.python.echo/manifest"}
			}},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/capability-packages/{id}/artifacts/{filename}", kind: "catalog",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/capability-packages/builtin.python.echo/artifacts/" + advCatalogArchive}
			}},

		// --- shared content (session or key; resource rules in packages/content) --
		{method: http.MethodGet, pattern: "GET /enterprise/v1/resources", kind: "catalog",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/resources"} }},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/resources", kind: "catalog", success: http.StatusCreated,
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/resources",
					body: `{"kind":"news","visibility":"organization","title":"adv matrix","summary":"","body":"benign"}`}
			}},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/resources/{id}", kind: "catalog",
			probe: func(t *testing.T, e *advEnv, _ string) probe {
				return probe{path: "/enterprise/v1/resources/" + advSeedResource(t, e, "approved")}
			}},
		{method: http.MethodPut, pattern: "PUT /enterprise/v1/resources/{id}", kind: "catalog",
			probe: func(t *testing.T, e *advEnv, _ string) probe {
				return probe{path: "/enterprise/v1/resources/" + advSeedResource(t, e, "draft"),
					body: `{"kind":"news","visibility":"organization","title":"adv edited","summary":"","body":"benign"}`}
			}},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/resources/{id}/submit", kind: "catalog",
			probe: func(t *testing.T, e *advEnv, _ string) probe {
				return probe{path: "/enterprise/v1/resources/" + advSeedResource(t, e, "draft") + "/submit"}
			}},
		// Review actions: the developer principal owns the resource, so its
		// credential classes are authenticated but refused; the admin reviews.
		{method: http.MethodPost, pattern: "POST /enterprise/v1/resources/{id}/approve", kind: "catalog", owner: "admin",
			want: map[string]int{clsKey: http.StatusForbidden, clsBound: http.StatusForbidden, clsSession: http.StatusForbidden},
			probe: func(t *testing.T, e *advEnv, _ string) probe {
				return probe{path: "/enterprise/v1/resources/" + advSeedResource(t, e, "submitted") + "/approve"}
			}},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/resources/{id}/reject", kind: "catalog", owner: "admin",
			want: map[string]int{clsKey: http.StatusForbidden, clsBound: http.StatusForbidden, clsSession: http.StatusForbidden},
			probe: func(t *testing.T, e *advEnv, _ string) probe {
				return probe{path: "/enterprise/v1/resources/" + advSeedResource(t, e, "submitted") + "/reject"}
			}},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/resources/{id}/withdraw", kind: "catalog",
			probe: func(t *testing.T, e *advEnv, _ string) probe {
				return probe{path: "/enterprise/v1/resources/" + advSeedResource(t, e, "approved") + "/withdraw"}
			}},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/review/queue", kind: "catalog", owner: "admin",
			want:  map[string]int{clsKey: http.StatusForbidden, clsBound: http.StatusForbidden, clsSession: http.StatusForbidden},
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/review/queue"} }},

		// --- wiki: tree, pages, search (members); node management (admin) -------
		{method: http.MethodGet, pattern: "GET /enterprise/v1/wiki/tree", kind: "catalog",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/wiki/tree"} }},
		{method: http.MethodPost, pattern: "POST /mcp/wiki", kind: "catalog",
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/mcp/wiki",
					body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_wiki","arguments":{"query":"adv"}}}`}
			}},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/wiki/pages/{id}/attachments/{sha256}", kind: "catalog",
			probe: func(t *testing.T, e *advEnv, _ string) probe {
				page, sum := advSeedWikiAttachment(t, e)
				return probe{path: "/enterprise/v1/wiki/pages/" + page + "/attachments/" + sum}
			}},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/wiki/search", kind: "catalog",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/wiki/search?q=adv"} }},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/wiki/pages/{id}", kind: "catalog",
			probe: func(t *testing.T, e *advEnv, _ string) probe {
				id, _ := advSeedWikiPage(t, e)
				return probe{path: "/enterprise/v1/wiki/pages/" + id}
			}},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/wiki/by-path/{path...}", kind: "catalog",
			probe: func(t *testing.T, e *advEnv, _ string) probe {
				_, path := advSeedWikiPage(t, e)
				return probe{path: "/enterprise/v1/wiki/by-path/" + path}
			}},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/wiki/nodes", kind: "admin", owner: "admin", success: http.StatusCreated,
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/wiki/nodes", body: `{"level":"shelf","slug":"` + advUnique("adv-shelf-%06d") + `","title":"adv"}`}
			}},
		{method: http.MethodPatch, pattern: "PATCH /enterprise/v1/wiki/nodes/{id}", kind: "admin", owner: "admin",
			probe: func(t *testing.T, e *advEnv, _ string) probe {
				return probe{path: "/enterprise/v1/wiki/nodes/" + advSeedShelf(t, e), body: `{"slug":"` + advUnique("adv-renamed-%06d") + `","title":"adv"}`}
			}},
		{method: http.MethodDelete, pattern: "DELETE /enterprise/v1/wiki/nodes/{id}", kind: "admin", owner: "admin", success: http.StatusNoContent,
			probe: func(t *testing.T, e *advEnv, _ string) probe {
				return probe{path: "/enterprise/v1/wiki/nodes/" + advSeedShelf(t, e)}
			}},

		{method: http.MethodPut, pattern: "PUT /enterprise/v1/blobs/{sha256}", kind: "catalog", success: http.StatusCreated,
			probe: func(*testing.T, *advEnv, string) probe {
				body := advUnique("adv attachment %06d")
				sum := sha256.Sum256([]byte(body))
				return probe{path: "/enterprise/v1/blobs/" + hex.EncodeToString(sum[:]), body: body,
					headers: map[string]string{"Content-Type": "text/plain"}}
			}},

		// --- curation proposals (reviewers; members are refused) ----------------
		{method: http.MethodGet, pattern: "GET /enterprise/v1/curation/proposals", kind: "catalog", owner: "admin",
			want:  map[string]int{clsKey: http.StatusForbidden, clsBound: http.StatusForbidden, clsSession: http.StatusForbidden},
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/curation/proposals"} }},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/curation/proposals/{id}/decision", kind: "catalog", owner: "admin",
			want: map[string]int{clsKey: http.StatusNotFound, clsBound: http.StatusNotFound, clsSession: http.StatusNotFound},
			probe: func(t *testing.T, e *advEnv, _ string) probe {
				return probe{path: "/enterprise/v1/curation/proposals/" + advSeedFlagProposal(t, e) + "/decision", body: `{"decision":"revert"}`}
			}},

		{method: http.MethodPost, pattern: "POST /enterprise/v1/curation/sweeps", kind: "admin", owner: "admin",
			success: http.StatusAccepted,
			probe:   func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/curation/sweeps"} }},

		// --- contribution inbox (author-only; session or key) -------------------
		{method: http.MethodPost, pattern: "POST /enterprise/v1/contributions", kind: "catalog", success: http.StatusCreated,
			probe: func(*testing.T, *advEnv, string) probe {
				local := advUnique("adv/page-%06d")
				return probe{path: "/enterprise/v1/contributions",
					headers: map[string]string{"Idempotency-Key": local},
					body: `{"local_id":"` + local + `","title":"adv","summary":"","markdown":"benign","requested_scope":"organization",` +
						`"redaction_declared":true,"upload_mode":"manual","client":{"name":"adv","version":"1","schema_version":1}}`}
			}},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/contributions", kind: "catalog",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/contributions"} }},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/contributions/{id}", kind: "catalog",
			probe: func(t *testing.T, e *advEnv, _ string) probe {
				return probe{path: "/enterprise/v1/contributions/" + advSeedContribution(t, e)}
			}},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/contributions/{id}/withdraw", kind: "catalog",
			probe: func(t *testing.T, e *advEnv, _ string) probe {
				return probe{path: "/enterprise/v1/contributions/" + advSeedContribution(t, e) + "/withdraw"}
			}},

		// --- member and group administration (admin session's Workspace) -------
		{method: http.MethodGet, pattern: "GET /enterprise/v1/admin/members", kind: "admin", owner: "admin",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/admin/members"} }},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/admin/members", kind: "admin", owner: "admin",
			success: http.StatusCreated,
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/admin/members",
					body: `{"username":"advmember","email":"advmember@example.com","display_name":"adv member"}`}
			}},
		{method: http.MethodPatch, pattern: "PATCH /enterprise/v1/admin/members/{userID}", kind: "admin", owner: "admin",
			probe: func(t *testing.T, e *advEnv, cls string) probe {
				// Idempotent no-op: the developer principal has no group.
				return probe{path: "/enterprise/v1/admin/members/" + e.userID, body: `{"group_id":""}`}
			}},
		{method: http.MethodDelete, pattern: "DELETE /enterprise/v1/admin/members/{userID}", kind: "admin", owner: "admin",
			success: http.StatusNoContent,
			probe: func(t *testing.T, e *advEnv, cls string) probe {
				return probe{path: "/enterprise/v1/admin/members/" + advSeedMember(t, e, cls)}
			}},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/admin/groups", kind: "admin", owner: "admin",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/admin/groups"} }},
		{method: http.MethodPost, pattern: "POST /enterprise/v1/admin/groups", kind: "admin", owner: "admin",
			success: http.StatusCreated,
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/admin/groups", body: `{"name":"adv matrix group"}`}
			}},
		{method: http.MethodPatch, pattern: "PATCH /enterprise/v1/admin/groups/{groupID}", kind: "admin", owner: "admin",
			probe: func(t *testing.T, e *advEnv, cls string) probe {
				return probe{path: "/enterprise/v1/admin/groups/" + advSeedGroup(t, e, "rename-"+cls),
					body: `{"name":"adv renamed ` + cls + `"}`}
			}},
		{method: http.MethodDelete, pattern: "DELETE /enterprise/v1/admin/groups/{groupID}", kind: "admin", owner: "admin",
			success: http.StatusNoContent,
			probe: func(t *testing.T, e *advEnv, cls string) probe {
				return probe{path: "/enterprise/v1/admin/groups/" + advSeedGroup(t, e, "delete-"+cls)}
			}},
		// --- role administration -----------------------------------------------
		{method: http.MethodGet, pattern: "GET /enterprise/v1/admin/roles", kind: "admin", owner: "admin",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/admin/roles"} }},
		{method: http.MethodPut, pattern: "PUT /enterprise/v1/admin/roles/{key}", kind: "admin", owner: "admin",
			probe: func(t *testing.T, e *advEnv, cls string) probe {
				return probe{path: "/enterprise/v1/admin/roles/adv-" + cls,
					body: `{"name":"adv role ` + cls + `","scope":"workspace","permissions":[]}`}
			}},
		// Resetting a built-in role without edits is an idempotent no-op.
		{method: http.MethodDelete, pattern: "DELETE /enterprise/v1/admin/roles/{key}", kind: "admin", owner: "admin",
			success: http.StatusNoContent,
			probe:   func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/admin/roles/reviewer"} }},
		{method: http.MethodGet, pattern: "GET /enterprise/v1/me/permissions", kind: "session",
			probe: func(*testing.T, *advEnv, string) probe { return probe{path: "/enterprise/v1/me/permissions"} }},
		// Password change: the current password IS the credential — no bearer
		// is consulted, so every class gets the identical answer for an
		// unknown username.
		{method: http.MethodPost, pattern: "POST /enterprise/v1/auth/local/password", kind: "public",
			success: http.StatusUnauthorized,
			probe: func(*testing.T, *advEnv, string) probe {
				return probe{path: "/enterprise/v1/auth/local/password",
					body: `{"username":"adv-nobody","current_password":"adv-old-pass-1","new_password":"adv-new-pass-1"}`}
			}},
	}
}

// registeredRoutePatterns parses the HandleFunc registrations out of
// apps/gateway/server.go via go/ast. This — not a hand-written list — is the
// inventory the sweep is judged against: a newly registered surface without
// a matrix row fails the run.
func registeredRoutePatterns(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go (run from the apps/gateway package dir): %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", src, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}
	var routes []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) < 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "HandleFunc" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		routes = append(routes, strings.Trim(lit.Value, `"`))
		return true
	})
	if len(routes) == 0 {
		t.Fatalf("route inventory is empty — the AST parse stopped matching HandleFunc registrations")
	}
	return routes
}

// TestAuthzMatrix sweeps every registered route x credential class. Every
// cell asserts its expected outcome; nothing is skipped by default.
func TestAuthzMatrix(t *testing.T) {
	env := newAdversarialEnv(t)

	// Real control data for rows whose authorized cells need it.
	advSeedAgentRelease(t, env)
	advSeedSkillPackage(t, env, "adv-skill-dl", "9.9.1", "released")
	advSeedSkillPackage(t, env, "adv-skill-rev", "9.9.2", "pending_review")
	advSeedSkillPackage(t, env, "adv-skill-rel", "9.9.3", "approved")
	advSeedSkillPackage(t, env, "adv-skill-dep", "9.9.4", "released")
	advSeedSkillPackage(t, env, "adv-skill-rev2", "9.9.5", "released")

	registered := registeredRoutePatterns(t)
	rows := matrixRows(t)
	byPattern := map[string]matrixRow{}
	for _, r := range rows {
		if _, dup := byPattern[r.pattern]; dup {
			t.Fatalf("duplicate matrix row for %s", r.pattern)
		}
		byPattern[r.pattern] = r
	}
	var missing, stale []string
	for _, p := range registered {
		if _, ok := byPattern[p]; !ok {
			missing = append(missing, p)
		}
	}
	for p := range byPattern {
		found := false
		for _, reg := range registered {
			if reg == p {
				found = true
				break
			}
		}
		if !found {
			stale = append(stale, p)
		}
	}
	if len(missing) > 0 || len(stale) > 0 {
		t.Fatalf("route inventory and matrix diverged — missing rows: %v; stale rows: %v", missing, stale)
	}

	var dump *os.File
	if path := os.Getenv("EAP_MATRIX_DUMP"); path != "" {
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("open matrix dump: %v", err)
		}
		defer f.Close()
		dump = f
		_, _ = fmt.Fprintln(f, "method,pattern,class,expected,got")
	}

	cells := 0
	for _, row := range rows {
		for _, cls := range matrixClasses {
			if cls == clsOwner && row.owner == "" {
				continue
			}
			want := row.expectedStatus(cls)
			if want < 0 {
				continue
			}
			p := row.probe(t, env, cls)
			bearer, ws := env.credential(t, row, cls)
			headers := map[string]string{}
			for k, v := range p.headers {
				headers[k] = v
			}
			if ws != "" {
				headers[identity.HeaderWorkspace] = ws
			}
			rec := env.do(t, row.method, p.path, bearer, p.body, headers)
			cells++
			got := rec.Code
			if got != want {
				t.Errorf("matrix %s %s [%s]: got %d want %d body=%s",
					row.method, row.pattern, cls, got, want, advTruncate(rec.Body.String()))
			}
			// Fail-closed invariants on governed surfaces: dead/absent
			// credentials and foreign workspaces must never see success,
			// whatever the endpoint's individual expectation says.
			if row.kind != "public" && !row.wsAgnostic && got >= 200 && got < 300 {
				switch cls {
				case clsNone, clsRevoked, clsExpired:
					t.Errorf("matrix %s %s [%s]: governed surface succeeded for a dead credential", row.method, row.pattern, cls)
				case clsForeign:
					t.Errorf("matrix %s %s [%s]: governed surface succeeded for a foreign workspace", row.method, row.pattern, cls)
				}
			}
			if dump != nil {
				_, _ = fmt.Fprintf(dump, "%s,%s,%s,%d,%d\n", row.method, row.pattern, cls, want, got)
			}
		}
	}
	t.Logf("authorization matrix: %d routes, %d cells asserted, zero skipped", len(rows), cells)

	// The SPA catch-all must answer unknown API paths with the JSON problem
	// shape, never the HTML shell (machines read this surface).
	rec := env.do(t, http.MethodGet, "/enterprise/v1/definitely-not-a-route", "", "", nil)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Errorf("unknown API path under the SPA catch-all: %d %s, want JSON 404", rec.Code, rec.Header().Get("Content-Type"))
	}
}

// ---------------------------------------------------------------------------
// 2. credential-leakage scan
// ---------------------------------------------------------------------------

// advLockedBuffer is the captured REAL gateway log stream (the std logger
// every handler writes to).
type advLockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *advLockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *advLockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// advRespRecord is one observed response.
type advRespRecord struct {
	method, path string
	status       int
	body         string
}

// advRecordingHandler wraps the live handler so EVERY response body is
// captured (the sweep scans the traffic, not curated samples).
type advRecordingHandler struct {
	inner   http.Handler
	mu      sync.Mutex
	records []advRespRecord
}

type advBodyRecorder struct {
	http.ResponseWriter
	status int
	body   strings.Builder
}

func (w *advBodyRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *advBodyRecorder) Write(p []byte) (int, error) {
	w.body.Write(p)
	return w.ResponseWriter.Write(p)
}

func (h *advRecordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &advBodyRecorder{ResponseWriter: w, status: http.StatusOK}
	h.inner.ServeHTTP(rec, r)
	h.mu.Lock()
	h.records = append(h.records, advRespRecord{method: r.Method, path: r.URL.Path, status: rec.status, body: rec.body.String()})
	h.mu.Unlock()
}

func (h *advRecordingHandler) all() []advRespRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]advRespRecord(nil), h.records...)
}

// advStripDocumentedField removes the field the product contract
// deliberately shows exactly once (key creation, device refresh). Everything
// else in the body stays in the scan corpus.
func advStripDocumentedField(t *testing.T, body, field string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return body
	}
	delete(m, field)
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return string(out)
}

func maskPrefix(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:8]
}

// ---------------------------------------------------------------------------
// 3. DLP bypass corpus
// ---------------------------------------------------------------------------

// advCorpusCase is one adversarial payload. The rule pattern MUST still match
// the secret inside payload; action is the seeded enforcement class.
type advCorpusCase struct {
	rule    string
	label   string
	secret  string
	payload string
}

func advAlnum(n int) string {
	const set = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	out := make([]byte, n)
	last := byte('a')
	for i := range out {
		last = set[(int(last)+i*7)%len(set)]
		out[i] = last
	}
	return string(out)
}

func advCorpusCases(t *testing.T) (deny, redact, negative []advCorpusCase) {
	t.Helper()
	deny = []advCorpusCase{
		{rule: "openai_key", label: "canonical-min-20", secret: "sk-abcdefghijklmnopqrst",
			payload: "call the API with sk-abcdefghijklmnopqrst please"},
		{rule: "openai_key", label: "upper-bound-200", secret: "sk-" + advAlnum(200),
			payload: "key: sk-" + advAlnum(200)},
		{rule: "openai_key", label: "two-occurrences", secret: "sk-AbCdEf0123456789ZzQq42",
			payload: "first sk-AbCdEf0123456789ZzQq42 then sk-YyXxWwVu9876543210TsQq11"},
		{rule: "openai_key", label: "json-embedded", secret: "sk-JsonEmbed1234567890Xx",
			payload: `{"config":{"openai":"sk-JsonEmbed1234567890Xx"}}`},
		{rule: "eap_api_key", label: "live-min-16", secret: "agk_live_ABCDEFGHIJKLMNOP",
			payload: "use agk_live_ABCDEFGHIJKLMNOP for the deploy"},
		{rule: "eap_api_key", label: "test-40", secret: "agk_test_" + advAlnum(40),
			payload: "CI token agk_test_" + advAlnum(40)},
		{rule: "bearer_token", label: "lower-single-space", secret: "bearer abcdefghijklmnopqrstuvwx",
			payload: "auth: bearer abcdefghijklmnopqrstuvwx"},
		{rule: "bearer_token", label: "UPPER-tab-sep", secret: "BEARER\tAbCdEf0123456789+-._~",
			payload: "Authorization BEARER\tAbCdEf0123456789+-._~"},
		{rule: "bearer_token", label: "mixed-8-space-run", secret: "BeArEr        plain29char-token.value~ok",
			payload: "hdr BeArEr        plain29char-token.value~ok tail"},
		{rule: "private_key", label: "generic", secret: "-----BEGIN PRIVATE KEY-----",
			payload: "paste:\n-----BEGIN PRIVATE KEY-----\nMIIEv"},
		{rule: "private_key", label: "rsa", secret: "-----BEGIN RSA PRIVATE KEY-----",
			payload: "-----BEGIN RSA PRIVATE KEY-----"},
		{rule: "private_key", label: "encrypted", secret: "-----BEGIN ENCRYPTED PRIVATE KEY-----",
			payload: "-----BEGIN ENCRYPTED PRIVATE KEY-----"},
		{rule: "private_key", label: "multiline-openssh", secret: "-----BEGIN OPENSSH PRIVATE KEY-----",
			payload: "file starts here\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA"},
		{rule: "db_conn_string", label: "postgres", secret: "postgres://app:s3cret@db.internal:5432/prod",
			payload: "dsn postgres://app:s3cret@db.internal:5432/prod"},
		{rule: "db_conn_string", label: "POSTGRESQL-upper", secret: "POSTGRESQL://svc:pw123@host",
			payload: "POSTGRESQL://svc:pw123@host/db"},
		{rule: "db_conn_string", label: "mysql-mixed-case", secret: "MySQL://user:secretpass@10.0.0.5:3306/db",
			payload: "MySQL://user:secretpass@10.0.0.5:3306/db"},
		{rule: "db_conn_string", label: "mongodb", secret: "mongodb://cluster:s3cret@mongo.svc",
			payload: "mongodb://cluster:s3cret@mongo.svc/?tls=true"},
		{rule: "openai_key", label: "uppercase-prefix", secret: "SK-AbCdEf0123456789ZzQq",
			payload: "KEY: SK-AbCdEf0123456789ZzQq"},
	}
	redact = []advCorpusCase{
		{rule: "password_field", label: "lower-colon", secret: "hunter2secret",
			payload: "password: hunter2secret"},
		{rule: "password_field", label: "PASSWD-equals", secret: "p@ssw0rd!123",
			payload: "PASSWD = p@ssw0rd!123"},
		{rule: "password_field", label: "unquoted-key-json", secret: "trustno1pass",
			payload: `{password: trustno1pass}`},
		// TASK-054: the JSON quoted-key form is the dominant API payload
		// shape (TASK-047 finding) — the quoted key must redact the value.
		{rule: "password_field", label: "json-quoted-key", secret: "hunter2secret9",
			payload: `{"password": "hunter2secret9"}`},
		{rule: "internal_phone", label: "embedded", secret: "13800138000",
			payload: "call 13800138000 today"},
		{rule: "internal_phone", label: "199-prefix", secret: "19912345678",
			payload: "hotline 19912345678 on call"},
		// TASK-054: canonical CN 3-4-4 visual grouping (TASK-047 finding) —
		// separators only at the two fixed group boundaries must match.
		{rule: "internal_phone", label: "separated-344", secret: "138 0013 8000",
			payload: "回拨 138 0013 8000 谢谢"},
		{rule: "internal_ipv4", label: "10-net", secret: "10.0.0.1",
			payload: "subnet gw 10.0.0.1 ok"},
		{rule: "internal_ipv4", label: "172-31", secret: "172.31.254.10",
			payload: "node 172.31.254.10 up"},
		{rule: "internal_ipv4", label: "192-168", secret: "192.168.1.100",
			payload: "printer 192.168.1.100"},
		{rule: "cn_resident_id", label: "valid-digit-check", secret: advGenResidentID(t, false),
			payload: ""},
		{rule: "cn_resident_id", label: "valid-X-check", secret: advGenResidentID(t, true),
			payload: ""},
		{rule: "cn_bank_card", label: "luhn-16", secret: advGenBankCard(t, 16),
			payload: ""},
		{rule: "cn_bank_card", label: "luhn-19", secret: advGenBankCard(t, 19),
			payload: ""},
		{rule: "cn_uscc", label: "gb32100", secret: advGenUSCC(t),
			payload: ""},
	}
	// Fill the payload for the checksum rules (value is generated).
	for i := range redact {
		if redact[i].payload == "" && redact[i].secret != "" {
			noun := "id"
			switch redact[i].rule {
			case "cn_bank_card":
				noun = "card"
			case "cn_uscc":
				noun = "uscc"
			}
			redact[i].payload = noun + " " + redact[i].secret + " filed"
		}
	}
	// Checksum negative space (documented design, TASK-030): candidates that
	// fail the national checksum stay untouched — asserting the design, not
	// conceding a leak.
	negative = []advCorpusCase{
		{rule: "cn_resident_id", label: "checksum-evading-permutation", secret: advPermutedResidentID(t), payload: ""},
		{rule: "cn_bank_card", label: "luhn-evading-permutation", secret: advPermutedBankCard(t), payload: ""},
		{rule: "cn_uscc", label: "bad-check-char", secret: advPermutedUSCC(t), payload: ""},
	}
	for i := range negative {
		negative[i].payload = "note " + negative[i].secret + " end"
	}
	return deny, redact, negative
}

// advGenResidentID builds an 18-digit GB 11643-valid ID; wantX targets an X
// check digit.
func advGenResidentID(t *testing.T, wantX bool) string {
	t.Helper()
	for seq := 0; seq <= 999; seq++ {
		for d := byte('0'); d <= '9'; d++ {
			cand := fmt.Sprintf("11010519491231%03d%c", seq, d)
			if !wantX && dlp.ResidentIDValid(cand) {
				return cand
			}
		}
		cand := fmt.Sprintf("11010519491231%03dX", seq)
		if wantX && dlp.ResidentIDValid(cand) {
			return cand
		}
	}
	t.Fatalf("no valid resident id (wantX=%v)", wantX)
	return ""
}

func advPermutedResidentID(t *testing.T) string {
	t.Helper()
	valid := advGenResidentID(t, false)
	perm := []byte(valid)
	if perm[17] == '9' {
		perm[17] = '8'
	} else {
		perm[17]++
	}
	if dlp.ResidentIDValid(string(perm)) {
		t.Fatalf("permutation unexpectedly valid")
	}
	return string(perm)
}

func advGenBankCard(t *testing.T, digits int) string {
	t.Helper()
	base := "6222020200115593188765"[:digits-1]
	for d := byte('0'); d <= '9'; d++ {
		cand := base + string(d)
		if dlp.BankCardValid(cand) {
			return cand
		}
	}
	t.Fatalf("no valid %d-digit luhn card", digits)
	return ""
}

func advPermutedBankCard(t *testing.T) string {
	t.Helper()
	valid := advGenBankCard(t, 16)
	perm := []byte(valid)
	if perm[15] == '9' {
		perm[15] = '8'
	} else {
		perm[15]++
	}
	if dlp.BankCardValid(string(perm)) {
		t.Fatalf("permutation unexpectedly valid")
	}
	return string(perm)
}

func advGenUSCC(t *testing.T) string {
	t.Helper()
	base := "91350100M000100Y1" // 17 chars
	for _, c := range "0123456789ABCDEFGHJKLMNPQRTUWXY" {
		cand := base + string(c)
		if dlp.USCCValid(cand) {
			return cand
		}
	}
	t.Fatalf("no valid uscc")
	return ""
}

func advPermutedUSCC(t *testing.T) string {
	t.Helper()
	valid := advGenUSCC(t)
	perm := []byte(valid)
	if perm[17] == 'Y' {
		perm[17] = 'W'
	} else {
		perm[17]++
	}
	if dlp.USCCValid(string(perm)) {
		t.Fatalf("uscc permutation unexpectedly valid")
	}
	return string(perm)
}

// TestDLPBypassCorpus drives the SEEDED rule set (000006 + 000017 + 000020)
// through the real chat pipeline: every evasive variant that the pattern
// still matches must be blocked (deny) or rewritten (redact) with zero leak
// toward the provider or the employee.
func TestDLPBypassCorpus(t *testing.T) {
	env := newAdversarialEnv(t)
	ws := map[string]string{identity.HeaderWorkspace: "secops"}

	// Guard against silent rule drift: the corpus is derived from the seeded
	// rules, so their names/actions/patterns must be exactly what we think.
	rules, err := env.store.LoadDLPRules(context.Background())
	if err != nil {
		t.Fatalf("load rules: %v", err)
	}
	ruleByName := map[string]dlp.Rule{}
	for _, r := range rules {
		ruleByName[r.Name] = r
	}
	wantActions := map[string]string{
		"openai_key": "deny", "eap_api_key": "deny", "bearer_token": "deny",
		"password_field": "redact", "private_key": "deny", "db_conn_string": "deny",
		"internal_phone": "redact", "internal_ipv4": "redact",
		"cn_resident_id": "redact", "cn_bank_card": "redact", "cn_uscc": "redact",
	}
	for name, action := range wantActions {
		r, ok := ruleByName[name]
		if !ok {
			t.Fatalf("seeded rule %q missing — update the corpus derivation", name)
		}
		if r.Action != action {
			t.Fatalf("seeded rule %q action %q, corpus expects %q", name, r.Action, action)
		}
	}

	sendChat := func(content string) (int, string, int) {
		before := env.upstreamCount()
		rec := env.do(t, http.MethodPost, "/v1/chat/completions", env.keyValue,
			`{"model":"adv-fast","messages":[{"role":"user","content":`+advJSONQuote(content)+`}]}`, ws)
		return rec.Code, rec.Body.String(), env.upstreamCount() - before
	}

	// leakPrefix reports whether any LONG prefix of the secret survived in
	// text (catches a sanitizer that released a partial match raw).
	leakPrefix := func(text, secret string) string {
		minLen := len(secret) / 2
		if minLen < 8 {
			minLen = 8
		}
		if minLen > len(secret) {
			minLen = len(secret)
		}
		for k := minLen; k < len(secret); k++ {
			if strings.Contains(text, secret[:k]) {
				return secret[:k]
			}
		}
		return ""
	}

	assertDeny := func(tc advCorpusCase) {
		t.Run(tc.rule+"/"+tc.label, func(t *testing.T) {
			status, body, egress := sendChat(tc.payload)
			if status != http.StatusUnprocessableEntity || !strings.Contains(body, "dlp_blocked") {
				t.Fatalf("deny-class payload not blocked: %d %s", status, advTruncate(body))
			}
			if egress != 0 {
				t.Fatalf("deny-class payload produced %d provider requests — zero egress violated", egress)
			}
		})
	}
	assertRedact := func(tc advCorpusCase, replacement string) {
		t.Run(tc.rule+"/"+tc.label, func(t *testing.T) {
			status, body, egress := sendChat(tc.payload)
			if status != http.StatusOK {
				t.Fatalf("redact-class payload blocked: %d %s", status, advTruncate(body))
			}
			if egress != 1 {
				t.Fatalf("expected exactly one provider request, got %d", egress)
			}
			upstream := env.upstreamAll()[env.upstreamCount()-1]
			if strings.Contains(upstream, tc.secret) {
				t.Fatalf("raw secret reached the provider (value %s... masked)", maskPrefix(tc.secret))
			}
			if p := leakPrefix(upstream, tc.secret); p != "" {
				t.Fatalf("secret prefix %q survived toward the provider", maskPrefix(p))
			}
			if !strings.Contains(upstream, replacement) {
				t.Fatalf("provider-bound payload lacks the replacement %s: %s", replacement, advTruncate(upstream))
			}
		})
	}

	denyCases, redactCases, negativeCases := advCorpusCases(t)
	for _, tc := range denyCases {
		assertDeny(tc)
	}
	replacements := map[string]string{
		"password_field": "[REDACTED:password]", "internal_phone": "[REDACTED:phone]",
		"internal_ipv4": "[REDACTED:internal_ip]", "cn_resident_id": "[REDACTED:resident_id]",
		"cn_bank_card": "[REDACTED:bank_card]", "cn_uscc": "[REDACTED:uscc]",
	}
	for _, tc := range redactCases {
		assertRedact(tc, replacements[tc.rule])
	}

	// Negative space: checksum-evading permutations pass through UNTOUCHED by
	// design (the validator kills false positives; the permutation is not the
	// credential).
	for _, tc := range negativeCases {
		t.Run(tc.rule+"/"+tc.label, func(t *testing.T) {
			status, body, egress := sendChat(tc.payload)
			if status != http.StatusOK {
				t.Fatalf("negative-space payload must pass: %d %s", status, advTruncate(body))
			}
			if egress != 1 {
				t.Fatalf("expected exactly one provider request, got %d", egress)
			}
			upstream := env.upstreamAll()[env.upstreamCount()-1]
			if !strings.Contains(upstream, tc.secret) {
				t.Fatalf("checksum-failing candidate must stay untouched (documented design)")
			}
		})
	}

	// Response side: the provider answers WITH the full matching shape
	// (echo mode); the pre-release scan must block deny rules and rewrite
	// redact rules before the employee sees anything. The echo carries the
	// payload, not the bare value: context-free values (a password without
	// its key= prefix) are not identifiable by any seeded rule by design.
	for _, tc := range append(append([]advCorpusCase(nil), denyCases[0:3]...), redactCases[0:3]...) {
		t.Run("response/"+tc.rule, func(t *testing.T) {
			env.echoMu.Lock()
			env.echoText = "answer: " + tc.payload + " done"
			env.echoMu.Unlock()
			defer func() {
				env.echoMu.Lock()
				env.echoText = ""
				env.echoMu.Unlock()
			}()
			status, body, _ := sendChat("ADV-ECHO-REQUEST benign question")
			if strings.Contains(body, tc.secret) {
				t.Fatalf("released response carries the raw secret (value %s... masked)", maskPrefix(tc.secret))
			}
			if p := leakPrefix(body, tc.secret); p != "" {
				t.Fatalf("released response carries secret prefix %q", maskPrefix(p))
			}
			action := ruleByName[tc.rule].Action
			if action == "deny" {
				if status != http.StatusUnprocessableEntity || !strings.Contains(body, "dlp_blocked") {
					t.Fatalf("deny-class response must be blocked: %d %s", status, advTruncate(body))
				}
			} else {
				if status != http.StatusOK {
					t.Fatalf("redact-class response must pass: %d %s", status, advTruncate(body))
				}
				if !strings.Contains(body, replacements[tc.rule]) {
					t.Fatalf("released response lacks the replacement: %s", advTruncate(body))
				}
			}
		})
	}

	// Chunk-boundary splits through the LIVE SSE sanitizer: the secret is
	// streamed split at every byte position; the sanitizer's holdback window
	// must keep every partial match inside the window until it is complete.
	streamSecret := func(tc advCorpusCase, split int) {
		t.Run("sse/"+tc.rule+"/"+tc.label+"/split"+fmt.Sprint(split), func(t *testing.T) {
			head, tail := tc.secret[:split], tc.secret[split:]
			// Padding exceeds the derived holdback window so the sanitizer
			// actually releases text BETWEEN the two upstream chunks. The
			// pad is non-word text so \b-anchored rules (cn_*) keep matching
			// — word-character padding would be negative space, not evasion
			// the pattern still matches.
			pad := strings.Repeat(". ", 650)
			env.streamMu.Lock()
			env.streamParts = []string{pad + head, tail + pad}
			env.streamMu.Unlock()
			defer func() {
				env.streamMu.Lock()
				env.streamParts = nil
				env.streamMu.Unlock()
			}()

			ts := httptest.NewServer(env.handler)
			t.Cleanup(ts.Close)
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
				ts.URL+"/v1/chat/completions",
				strings.NewReader(`{"model":"adv-fast","stream":true,"messages":[{"role":"user","content":"stream benign"}]}`))
			req.Header.Set("Authorization", "Bearer "+env.keyValue)
			req.Header.Set(identity.HeaderWorkspace, "secops")
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatalf("stream request: %v", err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)

			// Reassemble the released deltas.
			var released strings.Builder
			for _, line := range strings.Split(string(raw), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				payload := strings.TrimPrefix(line, "data: ")
				if payload == "[DONE]" {
					continue
				}
				var chunk struct {
					Choices []struct {
						Delta struct {
							Content string `json:"content"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if json.Unmarshal([]byte(payload), &chunk) == nil && len(chunk.Choices) > 0 {
					released.WriteString(chunk.Choices[0].Delta.Content)
				}
			}
			text := released.String()
			if strings.Contains(text, tc.secret) || strings.Contains(string(raw), tc.secret) {
				t.Fatalf("SSE release carries the raw secret at split %d", split)
			}
			if p := leakPrefix(text, tc.secret); p != "" {
				t.Fatalf("SSE release carries secret prefix %q at split %d", maskPrefix(p), split)
			}
			action := ruleByName[tc.rule].Action
			if action == "deny" {
				if !strings.Contains(text, "[withheld:dlp]") && !strings.Contains(string(raw), "[withheld:dlp]") {
					// A deny may abort before anything is released; both
					// shapes are safe, but SOMETHING must mark the withhold
					// or the release must be empty of the secret (asserted
					// above). No additional constraint needed here.
					t.Logf("deny aborted the stream at split %d (no withhold marker)", split)
				}
			} else if !strings.Contains(text, replacements[tc.rule]) {
				t.Fatalf("SSE release lacks the replacement at split %d: %s", split, advTruncate(text))
			}
		})
	}
	// sseTargets are picked by rule+label (not list index) so corpus growth
	// cannot silently repoint a split target at the wrong shape.
	findCase := func(list []advCorpusCase, rule, label string) advCorpusCase {
		t.Helper()
		for _, tc := range list {
			if tc.rule == rule && tc.label == label {
				return tc
			}
		}
		t.Fatalf("corpus case %s/%s missing — update sseTargets", rule, label)
		return advCorpusCase{}
	}
	sseTargets := []advCorpusCase{
		findCase(denyCases, "eap_api_key", "live-min-16"),
		findCase(denyCases, "bearer_token", "UPPER-tab-sep"),
		findCase(redactCases, "cn_resident_id", "valid-digit-check"),
		// TASK-054: the widened separator form must also survive faithful
		// chunk splits (the separators ride inside the held-back window).
		findCase(redactCases, "internal_phone", "separated-344"),
	}
	for _, tc := range sseTargets {
		for split := 1; split < len(tc.secret); split++ {
			streamSecret(tc, split)
		}
	}

	// Tokenize leg: the same corpus under the tokenize action — provider and
	// employee see stable pseudonyms; the audited reveal restores the
	// original only for the owning workspace.
	t.Run("tokenize/internal_phone", func(t *testing.T) {
		adminAuth := map[string]string{"Authorization": "Bearer " + env.adminSession}
		if rec := env.do(t, http.MethodPatch, "/enterprise/v1/admin/dlp/rules/internal_phone", "",
			`{"action":"tokenize"}`, adminAuth); rec.Code != http.StatusOK {
			t.Fatalf("toggle tokenize: %d %s", rec.Code, rec.Body.String())
		}
		time.Sleep(120 * time.Millisecond) // scanner cache TTL
		status, body, egress := sendChat("call 13800138000 today")
		if status != http.StatusOK {
			t.Fatalf("tokenized chat failed: %d %s", status, advTruncate(body))
		}
		if egress != 1 {
			t.Fatalf("expected one provider request, got %d", egress)
		}
		upstream := env.upstreamAll()[env.upstreamCount()-1]
		if strings.Contains(upstream, "13800138000") {
			t.Fatalf("raw phone reached the provider under tokenize")
		}
		if !strings.Contains(upstream, "[internal_phone:") {
			t.Fatalf("provider-bound payload lacks the pseudonym: %s", advTruncate(upstream))
		}
		// Extract the pseudonym and reveal it back through the audited API.
		start := strings.Index(upstream, "[internal_phone:") + len("[internal_phone:")
		end := strings.Index(upstream[start:], "]") + start
		pseudonym := upstream[start:end]
		rec := env.do(t, http.MethodPost, "/enterprise/v1/dlp/reveal", env.keyValue,
			`{"tokens":["`+pseudonym+`"]}`, ws)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "13800138000") {
			t.Fatalf("reveal: %d %s", rec.Code, advTruncate(rec.Body.String()))
		}
		// Restore the seeded action so later legs see the baseline.
		if rec := env.do(t, http.MethodPatch, "/enterprise/v1/admin/dlp/rules/internal_phone", "",
			`{"action":"redact"}`, adminAuth); rec.Code != http.StatusOK {
			t.Fatalf("restore redact: %d", rec.Code)
		}
		time.Sleep(120 * time.Millisecond)
	})
}

// ---------------------------------------------------------------------------
// 3b. evasive-form coverage: TASK-047 findings, closed by TASK-054 (000021)
// ---------------------------------------------------------------------------

// TestDLPRuleFindings covers the gaps the TASK-047 corpus recorded. TASK-054
// (migration 000021) fixed the three fixable findings, so their subtests are
// POSITIVE coverage now: the widened seeded rules must catch the exact
// evasive shapes end to end, every run (a regression reopens the finding by
// failing here). The remaining gap — db_conn_string's password segment with
// raw spaces — is a documented accepted limitation (false-positive analysis
// in migrations/000021): its self-verifying subtest skips with the rationale
// today and flips positive automatically if the rule ever learns the shape.
func TestDLPRuleFindings(t *testing.T) {
	env := newAdversarialEnv(t)
	ws := map[string]string{identity.HeaderWorkspace: "secops"}
	sendChat := func(content string) (int, string, string) {
		rec := env.do(t, http.MethodPost, "/v1/chat/completions", env.keyValue,
			`{"model":"adv-fast","messages":[{"role":"user","content":`+advJSONQuote(content)+`}]}`, ws)
		last := ""
		if all := env.upstreamAll(); len(all) > 0 {
			last = all[len(all)-1]
		}
		return rec.Code, rec.Body.String(), last
	}

	t.Run("phone-separators-redacted", func(t *testing.T) {
		// Internal phone with canonical CN 3-4-4 visual grouping: the widened
		// internal_phone rule must redact it end to end (redact class — the
		// request passes, the provider never sees the number).
		payload := "回拨 138 0013 8000 谢谢"
		status, _, upstream := sendChat(payload)
		if status != http.StatusOK {
			t.Fatalf("redact-class payload blocked: %d", status)
		}
		if !strings.Contains(upstream, "[REDACTED:phone]") || strings.Contains(upstream, "138 0013 8000") {
			t.Fatalf("separator phone form not redacted: %s", advTruncate(upstream))
		}
	})

	t.Run("password-json-quoted-key-redacted", func(t *testing.T) {
		// The JSON quoted-key form is the dominant API payload shape: the
		// widened password_field rule must redact the value (quoted-key
		// tolerance, value class excludes quotes so the JSON stays
		// well-formed after the rewrite).
		payload := `{"password": "hunter2secret9"}`
		status, _, upstream := sendChat(payload)
		if status != http.StatusOK {
			t.Fatalf("redact-class payload blocked: %d", status)
		}
		if !strings.Contains(upstream, `[REDACTED:password]`) || strings.Contains(upstream, "hunter2secret9") {
			t.Fatalf("JSON quoted-key password not redacted: %s", advTruncate(upstream))
		}
	})

	t.Run("openai-key-uppercase-prefix-denied", func(t *testing.T) {
		// The case-mangled SK- prefix (copy/paste through a formatter) is the
		// same key shape: deny class, zero provider egress.
		status, body, _ := sendChat("KEY: SK-AbCdEf0123456789ZzQq")
		if status != http.StatusUnprocessableEntity || !strings.Contains(body, "dlp_blocked") {
			t.Fatalf("uppercase-prefix key not blocked: %d %s", status, advTruncate(body))
		}
	})

	t.Run("db-conn-password-space-accepted-limitation", func(t *testing.T) {
		// ACCEPTED LIMITATION (TASK-054): a raw-space password segment in
		// scheme://user:pass@host escapes [^\s"] by design. Closing it needs
		// bounded "word, optional space-run" repetition that no bound
		// separates from prose after a "scheme://user:" prefix ("see
		// postgres://db:5432 then ping alice@example.com" would become a
		// deny-class hit), while the FUNCTIONAL percent-encoded form (%20) is
		// already covered by the unchanged class and a raw space is not valid
		// URI userinfo (RFC 3986). Full analysis: migrations/000021 and
		// docs/verify/security-adversarial.md. Self-verifying: a future fix
		// flips this subtest to a positive log instead of a skip.
		payload := "dsn postgres://app:my s3cret@db.internal:5432/prod"
		status, _, upstream := sendChat(payload)
		if status == http.StatusUnprocessableEntity || strings.Contains(upstream, "dlp_blocked") {
			t.Logf("db_conn_string space finding is FIXED: update migrations/000021 and docs/verify/security-adversarial.md, then assert the blocked shape positively here")
			return
		}
		t.Skip("ACCEPTED LIMITATION (TASK-054, false-positive analysis in migrations/000021): raw-space password segment in db_conn_string stays unmatched — functional %20 form is covered; space tolerance would make prose after scheme://user: a deny-class hit")
	})
}

// ---------------------------------------------------------------------------
// 4. rate-limit effectiveness
// ---------------------------------------------------------------------------

// TestRateLimitEffectiveness proves the documented abuse-protection
// behavior: at the configured limit a burst trips 429 in the frozen Problem
// shape, the limit is per-subject, and the limiter recovers next window.
func TestRateLimitEffectiveness(t *testing.T) {
	env := newSliceEnv(t, 1000)

	// Deterministic fixed window via a controllable clock (the production
	// FixedWindow semantics, real Redis counter, real TTL).
	now := time.Unix(1_800_000_000, 0)
	env.srv.Rate = &gateway.FixedWindow{
		Counter: gateway.RedisCounter{
			Client: redis.NewClient(&redis.Options{Addr: os.Getenv("EAP_TEST_REDIS_ADDR")}),
			TTL:    time.Minute,
		},
		Limit:  3,
		Window: time.Minute,
		Now:    func() time.Time { return now },
	}
	env.handler = env.srv.Handler()

	mcpLogin(t, env, "adv-emp@example.com", "secops")
	userID := mcpUserID(t, env, "adv-emp@example.com")
	keyA := seedKey(t, env, userID)
	keyB := seedKey(t, env, userID)
	me := func(key string) *httptest.ResponseRecorder {
		return env.do(t, http.MethodGet, "/enterprise/v1/me", key, "", nil)
	}

	// Burst AT the limit: exactly limit requests succeed.
	for i := 0; i < 3; i++ {
		if rec := me(keyA); rec.Code != http.StatusOK {
			t.Fatalf("request %d inside the limit: %d, want 200", i+1, rec.Code)
		}
	}
	rec := me(keyA)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("burst beyond the limit: %d, want 429", rec.Code)
	}
	// Documented shape: frozen Problem, retriable, trace-linked.
	var problem struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		TraceID   string `json:"trace_id"`
		Retriable bool   `json:"retriable"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("429 body must be the frozen JSON shape: %v", err)
	}
	if problem.Code != "rate_limited" || !problem.Retriable || problem.TraceID == "" {
		t.Fatalf("429 shape wrong: %+v", problem)
	}

	// Per-subject: another key keeps its own window.
	if rec := me(keyB); rec.Code != http.StatusOK {
		t.Fatalf("second subject inside its own window: %d, want 200", rec.Code)
	}

	// Recovery on the next window: advancing the clock rotates the bucket.
	now = now.Add(61 * time.Second)
	if rec := me(keyA); rec.Code != http.StatusOK {
		t.Fatalf("limiter must recover next window: %d, want 200", rec.Code)
	}
	// And the new window enforces again.
	for i := 0; i < 2; i++ {
		if rec := me(keyA); rec.Code != http.StatusOK {
			t.Fatalf("recovery request %d: %d, want 200", i+1, rec.Code)
		}
	}
	if rec := me(keyA); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("new window must enforce again: %d, want 429", rec.Code)
	}
}

// TestCredentialLeakage proves no credential material appears in the
// gateway's real log stream, in any response body, or in the generated
// bootstrap config while a full key lifecycle runs.
func TestCredentialLeakage(t *testing.T) {
	env := newAdversarialEnv(t)

	// Swap the gateway's real log output in-process; restore on cleanup.
	logs := &advLockedBuffer{}
	prev := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	rh := &advRecordingHandler{inner: env.handler}
	env.handler = rh

	// secrets tracks every credential minted during the lifecycle;
	// shownOnce is the one documented display path ("" = zero tolerance).
	type secretT struct {
		value     string
		shownOnce string
	}
	var secrets []secretT
	track := func(value, shownOnce string) secretT {
		s := secretT{value: value, shownOnce: shownOnce}
		secrets = append(secrets, s)
		return s
	}

	sess := mcpLogin(t, env.sliceEnv, "adv-leak@example.com", "secops")
	track(sess, "") // session JWTs are credentials too

	// 1. Create a key through the API: the ONE documented full display.
	rec := env.do(t, http.MethodPost, "/enterprise/v1/keys", sess, `{"name":"adv-leak-key"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create key: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Key   string `json:"key"`
		KeyID string `json:"key_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Key == "" {
		t.Fatalf("one-time key missing from create response")
	}
	createdKey := track(created.Key, "POST /enterprise/v1/keys")

	// 2. A planted key that is never displayed: zero tolerance everywhere.
	planted := seedKey(t, env.sliceEnv, mcpUserID(t, env.sliceEnv, "adv-leak@example.com"))
	track(planted, "")

	// 3. Use the key across planes (employee surface + generated config).
	ws := map[string]string{identity.HeaderWorkspace: "secops"}
	if rec := env.do(t, http.MethodGet, "/enterprise/v1/me", created.Key, "", ws); rec.Code != http.StatusOK {
		t.Fatalf("me: %d", rec.Code)
	}
	if rec := env.do(t, http.MethodPost, "/enterprise/v1/token/exchange", created.Key,
		`{"audience":"mcp-gateway","ttl_seconds":300}`, ws); rec.Code != http.StatusOK {
		t.Fatalf("exchange: %d", rec.Code)
	}
	if rec := env.do(t, http.MethodGet, "/enterprise/v1/cli/bootstrap", created.Key, "", ws); rec.Code != http.StatusOK {
		t.Fatalf("bootstrap config: %d", rec.Code)
	}
	if rec := env.do(t, http.MethodGet, "/enterprise/v1/keys", sess, "", nil); rec.Code != http.StatusOK {
		t.Fatalf("list keys: %d", rec.Code)
	}

	// 4. The key pasted into chat is deny-class DLP: zero provider egress.
	upBefore := env.upstreamCount()
	if rec := env.do(t, http.MethodPost, "/v1/chat/completions", created.Key,
		`{"model":"adv-fast","messages":[{"role":"user","content":"my key is `+createdKey.value+`"}]}`, ws); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("chat with a pasted agk key: %d, want 422", rec.Code)
	}
	if env.upstreamCount() != upBefore {
		t.Fatalf("deny-class key material reached the provider")
	}

	// 5. Device flow: refresh credential shown exactly once, then logout.
	refresh := advDeviceFlowRefresh(t, env)
	refreshSecret := track(refresh, "POST /enterprise/v1/auth/device/token")
	if rec := env.do(t, http.MethodPost, "/enterprise/v1/auth/device/logout", "",
		`{"refresh_token":"`+refresh+`"}`, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("device logout: %d", rec.Code)
	}

	// 6. Revoke, then prove the dead key fails closed (error bodies scanned).
	if rec := env.do(t, http.MethodDelete, "/enterprise/v1/keys/"+created.KeyID, sess, "", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := env.do(t, http.MethodGet, "/enterprise/v1/me", created.Key, "", ws); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key: %d, want 401", rec.Code)
	}
	if rec := env.do(t, http.MethodGet, "/enterprise/v1/me", "agk_live_tooshort", "", ws); rec.Code != http.StatusUnauthorized {
		t.Fatalf("malformed key: %d, want 401", rec.Code)
	}

	// --- assertions over the captured corpus --------------------------------
	keyShape := regexp.MustCompile(`agk_(live|test)_[A-Za-z0-9]{16,}`)

	// 7. The real log stream must be clean of every tracked credential.
	logStream := logs.String()
	for _, s := range secrets {
		if strings.Contains(logStream, s.value) {
			t.Errorf("credential material leaked to the gateway log stream (value %s... masked)", maskPrefix(s.value))
		}
	}
	if keyShape.MatchString(logStream) {
		t.Errorf("agk-shaped material matched in the gateway log stream")
	}

	// 8. Response bodies: strip the documented one-time fields, then the
	// whole corpus must be free of key material.
	createdShows := 0
	refreshShows := 0
	sessionShows := 0
	for _, r := range rh.all() {
		body := r.body
		if r.method == http.MethodPost && r.path == "/enterprise/v1/keys" && r.status == http.StatusCreated {
			if keyShape.MatchString(body) {
				createdShows++
			}
			body = advStripDocumentedField(t, body, "key")
		}
		if r.method == http.MethodPost && r.path == "/enterprise/v1/auth/dev-login" && r.status == http.StatusOK {
			// The login response is the documented one-time display of the
			// control-plane session credential.
			if strings.Contains(r.body, sess) {
				sessionShows++
			}
			body = advStripDocumentedField(t, body, "session_token")
		}
		if r.method == http.MethodPost && r.path == "/enterprise/v1/auth/device/token" && r.status == http.StatusOK {
			if strings.Contains(r.body, refreshSecret.value) {
				refreshShows++
			}
			body = advStripDocumentedField(t, body, "refresh_token")
		}
		for _, s := range secrets {
			if s.shownOnce != "" || s.value == "" {
				continue
			}
			if strings.Contains(body, s.value) {
				t.Errorf("zero-tolerance credential found in %s %s (value %s... masked)", r.method, r.path, maskPrefix(s.value))
			}
		}
		if keyShape.MatchString(body) {
			t.Errorf("agk-shaped material outside the documented display in %s %s: %s",
				r.method, r.path, advTruncate(body))
		}
	}
	if createdShows != 1 {
		t.Errorf("created key must be shown exactly once in responses, got %d", createdShows)
	}
	if refreshShows != 1 {
		t.Errorf("device refresh token must be shown exactly once in responses, got %d", refreshShows)
	}
	if sessionShows != 1 {
		t.Errorf("session token must be shown exactly once in responses, got %d", sessionShows)
	}

	// 9. Error bodies carry the frozen Problem shape — or the OpenAI error
	// envelope on the /v1/* inference namespace (frozen client contract) —
	// and never credential fields.
	for _, r := range rh.all() {
		if r.status < 400 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(r.body), &m); err != nil {
			t.Errorf("error body %s %s is not JSON: %s", r.method, r.path, advTruncate(r.body))
			continue
		}
		code, hasCode := m["code"].(string)
		message, hasMessage := m["message"].(string)
		if !hasCode || !hasMessage {
			// OpenAI envelope: {"error":{"code","message","type",...}}.
			var envelope struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(r.body), &envelope); err != nil || envelope.Error.Code == "" || envelope.Error.Message == "" {
				if r.path == "/enterprise/v1/auth/device/token" {
					if _, ok := m["error"]; !ok {
						t.Errorf("oauth error body lacks the error field: %s", advTruncate(r.body))
					}
					continue
				}
				t.Errorf("error body %s %s matches neither Problem nor OpenAI envelope: %s", r.method, r.path, advTruncate(r.body))
				continue
			}
			code, message = envelope.Error.Code, envelope.Error.Message
		}
		_ = code
		_ = message
		for _, forbidden := range []string{"key", "api_key", "secret", "token_hash", "refresh_token", "credential"} {
			if _, ok := m[forbidden]; ok {
				t.Errorf("error body %s %s carries credential-shaped field %q", r.method, r.path, forbidden)
			}
		}
	}

	// 10. The durable audit trail stores decisions, not credential values.
	for _, s := range secrets {
		if !strings.HasPrefix(s.value, "agk_") {
			continue
		}
		var n int
		if err := env.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM audit_events WHERE detail::text LIKE '%'||$1||'%' OR resource LIKE '%'||$1||'%'`,
			s.value).Scan(&n); err != nil {
			t.Fatalf("audit scan: %v", err)
		}
		if n != 0 {
			t.Errorf("audit_events carries credential material (%d rows, value %s... masked)", n, maskPrefix(s.value))
		}
	}

	// 11. The provider never received any key material either.
	for _, body := range env.upstreamAll() {
		for _, s := range secrets {
			if strings.HasPrefix(s.value, "agk_") && strings.Contains(body, s.value) {
				t.Errorf("provider-bound payload carries key material (value %s... masked)", maskPrefix(s.value))
			}
		}
	}

	t.Logf("leakage scan: %d responses, %d tracked credentials, log corpus clean (%d bytes)",
		len(rh.all()), len(secrets), len(logStream))
}

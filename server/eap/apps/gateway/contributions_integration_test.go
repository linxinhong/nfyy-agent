//go:build integration

// Contribution inbox contract (api/mindlink-openapi.yaml): idempotent replay,
// server-side content versions, structured rejection reasons, never-upload
// fields, DLP returns, scope rules, withdrawal and author-only access.

package main_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"enterprise-agent-platform/packages/content"
	"enterprise-agent-platform/packages/contribution"
	"enterprise-agent-platform/packages/dlp"
)

type uploadResult struct {
	ID               string `json:"id"`
	Version          int    `json:"version"`
	ContentHash      string `json:"content_hash"`
	Status           string `json:"status"`
	IdempotentReplay bool   `json:"idempotent_replay"`
	Reasons          []struct {
		Field string   `json:"field"`
		Code  string   `json:"code"`
		Rules []string `json:"rules"`
	} `json:"reasons"`
	Code string `json:"code"`
}

func uploadBody(t *testing.T, overrides map[string]any) string {
	t.Helper()
	body := map[string]any{
		"local_id": "wiki/equity-buyback", "title": "股权回购纠纷", "summary": "一句话结论",
		"markdown": "## 要点\n回购条款的效力判断。", "details": map[string]any{"type": "办案经验", "confidence": "已验证"},
		"source_refs":     []map[string]any{{"kind": "case", "ref": "(2025)沪01民终123号"}},
		"requested_scope": "organization", "redaction_declared": true, "upload_mode": "manual",
		"client": map[string]any{"name": "mindlink-desktop", "version": "0.1.0", "schema_version": 1},
	}
	for k, v := range overrides {
		if v == nil {
			delete(body, k)
		} else {
			body[k] = v
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestContributionInboxContract(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store
	env.srv.DLP = dlp.NewTableScanner(env.store.LoadDLPRules)
	env.srv.Content = content.New(env.pool, env.store)
	env.srv.Contributions = contribution.New(env.pool, env.store)
	env.handler = env.srv.Handler()

	authorSession := mcpLogin(t, env, "author@example.com", "secops")
	authorID := mcpUserID(t, env, "author@example.com")
	otherSession := mcpLogin(t, env, "other@example.com", "secops")
	var teamA, teamB string
	for name, dst := range map[string]*string{"诉讼组": &teamA, "非诉组": &teamB} {
		if err := env.pool.QueryRow(ctx, `INSERT INTO workspace_groups (workspace_id, name) VALUES ('secops', $1) RETURNING id`, name).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.pool.Exec(ctx, `INSERT INTO workspace_group_members (group_id, workspace_id, user_id) VALUES ($1, 'secops', $2)`, teamA, authorID); err != nil {
		t.Fatal(err)
	}
	ws := func(key string) map[string]string {
		h := map[string]string{"X-Agent-Workspace": "secops"}
		if key != "" {
			h["Idempotency-Key"] = key
		}
		return h
	}
	upload := func(session, key, body string) (int, uploadResult) {
		rec := env.do(t, http.MethodPost, "/enterprise/v1/contributions", session, body, ws(key))
		var out uploadResult
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	reasonCodes := func(r uploadResult) map[string]string {
		out := map[string]string{}
		for _, reason := range r.Reasons {
			out[reason.Field] = reason.Code
		}
		return out
	}

	// First upload creates version 1 with a server-computed hash.
	code, first := upload(authorSession, "k-1", uploadBody(t, nil))
	if code != http.StatusCreated || first.Version != 1 || first.Status != "received" || !strings.HasPrefix(first.ContentHash, "sha256:") {
		t.Fatalf("first upload: %d %+v", code, first)
	}
	// Same key, same body: replay of the recorded result.
	if code, replay := upload(authorSession, "k-1", uploadBody(t, nil)); code != http.StatusOK || !replay.IdempotentReplay || replay.Version != 1 || replay.ID != first.ID {
		t.Fatalf("key replay: %d %+v", code, replay)
	}
	// New key, same content (details key order differs): content replay, no new version.
	reordered := uploadBody(t, map[string]any{"details": map[string]any{"confidence": "已验证", "type": "办案经验"}})
	if code, again := upload(authorSession, "k-2", reordered); code != http.StatusOK || !again.IdempotentReplay || again.Version != 1 {
		t.Fatalf("content replay: %d %+v", code, again)
	}
	// Same key reused for a different body is refused.
	changed := uploadBody(t, map[string]any{"markdown": "## 要点\n补充了新的裁判观点。"})
	if code, _ := upload(authorSession, "k-1", changed); code != http.StatusConflict {
		t.Fatalf("key reuse with new body: %d", code)
	}
	// Changed content becomes version 2 of the same contribution.
	if code, v2 := upload(authorSession, "k-3", changed); code != http.StatusCreated || v2.Version != 2 || v2.ID != first.ID || v2.ContentHash == first.ContentHash {
		t.Fatalf("version 2: %d %+v", code, v2)
	}

	// Missing Idempotency-Key is a structured rejection.
	if code, r := upload(authorSession, "", uploadBody(t, nil)); code != http.StatusBadRequest || reasonCodes(r)["Idempotency-Key"] != contribution.ReasonRequired {
		t.Fatalf("missing key: %d %+v", code, r)
	}
	// Every problem is reported at once, with field and code; nothing is stored.
	bad := uploadBody(t, map[string]any{
		"local_id": "wiki/fresh", "title": "", "redaction_declared": false,
		"details": map[string]any{"type": "办案经验", "脱敏映射": map[string]any{"张三": "当事人A"}},
	})
	code, rejected := upload(authorSession, "k-bad", bad)
	got := reasonCodes(rejected)
	if code != http.StatusBadRequest || rejected.Code != "invalid_argument" || got["title"] != contribution.ReasonRequired ||
		got["redaction_declared"] != contribution.ReasonNotRedacted || got["details.脱敏映射"] != contribution.ReasonForbiddenField {
		t.Fatalf("multi-reason rejection: %d %+v", code, rejected)
	}
	var stored int
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM contributions WHERE local_id = 'wiki/fresh'`).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("rejected upload stored: %d %v", stored, err)
	}
	// Sensitive content goes back to the author with the rule ids, never redacted server-side.
	code, dlpHit := upload(authorSession, "k-dlp", uploadBody(t, map[string]any{"local_id": "wiki/leak", "markdown": "key sk-ABCDEFGHIJKLMNOPQRSTUVWX"}))
	if code != http.StatusBadRequest || len(dlpHit.Reasons) != 1 || dlpHit.Reasons[0].Code != contribution.ReasonSensitiveContent ||
		dlpHit.Reasons[0].Field != "markdown" || len(dlpHit.Reasons[0].Rules) == 0 {
		t.Fatalf("dlp rejection: %d %+v", code, dlpHit)
	}
	var leaked int
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM contribution_versions WHERE markdown LIKE '%sk-ABCDEF%'`).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("sensitive upload stored: %d %v", leaked, err)
	}

	// Scope rules: own team ok, another team refused, attachments need blobs first.
	if code, r := upload(authorSession, "k-team", uploadBody(t, map[string]any{"local_id": "wiki/team", "requested_scope": "team", "group_id": teamA})); code != http.StatusCreated {
		t.Fatalf("own team: %d %+v", code, r)
	}
	if code, r := upload(authorSession, "k-other-team", uploadBody(t, map[string]any{"local_id": "wiki/other", "requested_scope": "team", "group_id": teamB})); code != http.StatusBadRequest || reasonCodes(r)["group_id"] != contribution.ReasonScopeNotAllowed {
		t.Fatalf("other team: %d %+v", code, r)
	}
	attachment := []map[string]any{{"sha256": strings.Repeat("a", 64), "name": "判决书.pdf", "media_type": "application/pdf", "size": 1024}}
	if code, r := upload(authorSession, "k-att", uploadBody(t, map[string]any{"local_id": "wiki/att", "attachments": attachment})); code != http.StatusBadRequest || reasonCodes(r)["attachments[0].sha256"] != contribution.ReasonAttachmentMissing {
		t.Fatalf("attachment before blob: %d %+v", code, r)
	}

	// Listing is the author's own, incremental by updated_at; others see nothing.
	rec := env.do(t, http.MethodGet, "/enterprise/v1/contributions", authorSession, "", ws(""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), first.ID) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(t, http.MethodGet, "/enterprise/v1/contributions?limit=1", authorSession, "", ws("")); !strings.Contains(rec.Body.String(), "next_since") {
		t.Fatalf("page cursor missing: %s", rec.Body.String())
	}
	if rec := env.do(t, http.MethodGet, "/enterprise/v1/contributions/"+first.ID, otherSession, "", ws("")); rec.Code != http.StatusNotFound {
		t.Fatalf("other author read: %d", rec.Code)
	}
	if rec := env.do(t, http.MethodPost, "/enterprise/v1/contributions/"+first.ID+"/withdraw", otherSession, "", ws("")); rec.Code != http.StatusNotFound {
		t.Fatalf("other author withdraw: %d", rec.Code)
	}
	rec = env.do(t, http.MethodPost, "/enterprise/v1/contributions/"+first.ID+"/withdraw", authorSession, "", ws(""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"withdrawn"`) {
		t.Fatalf("withdraw: %d %s", rec.Code, rec.Body.String())
	}

	for _, typ := range []string{"contribution.received", "contribution.rejected", "contribution.withdrawn"} {
		var n int
		if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE type = $1`, typ).Scan(&n); err != nil || n == 0 {
			t.Fatalf("audit %s missing: %d %v", typ, n, err)
		}
	}
	var leakedAudit int
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE detail::text LIKE '%sk-ABCDEF%' OR detail::text LIKE '%当事人A%'`).Scan(&leakedAudit); err != nil || leakedAudit != 0 {
		t.Fatalf("rejected content leaked into audit: %d %v", leakedAudit, err)
	}
}

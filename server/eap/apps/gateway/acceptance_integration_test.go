//go:build integration

// Knowledge-platform acceptance (docs/knowledge-platform-architecture.md §10
// step 9): member A uploads a team contribution with an attachment → the
// curator proposes a page → a reviewer publishes it → teammate B finds and
// reads it (search, page, attachment, MCP) → member C of another team gets
// it in none of them → A withdraws → the page is paused and the attachment
// gone for B → reverting the pause republishes the page, never the withdrawn
// attachment → state survives a restart of the gateway.

package main_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"enterprise-agent-platform/packages/blob"
	"enterprise-agent-platform/packages/content"
	"enterprise-agent-platform/packages/contribution"
	"enterprise-agent-platform/packages/curation"
	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/wiki"
)

func TestKnowledgePlatformAcceptance(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	blobRoot := t.TempDir()
	wire := func() {
		env.srv.OIDCStore = env.store
		env.srv.DLP = dlp.NewTableScanner(env.store.LoadDLPRules)
		env.srv.Content = content.New(env.pool, env.store)
		env.srv.Contributions = contribution.New(env.pool, env.store)
		env.srv.Wiki = wiki.NewStore(env.pool, env.store)
		env.srv.Blobs = blob.New(env.pool, env.store, blobRoot)
		env.srv.Curation = curation.New(env.pool, env.store, env.srv.Content)
		env.handler = env.srv.Handler()
	}
	wire()
	worker := &curation.Worker{Store: env.srv.Curation, Content: env.srv.Content, Curator: curation.Baseline{},
		Owner: "acceptance", Lease: time.Minute, Idle: time.Millisecond}
	drain := func() {
		for i := 0; i < 10; i++ {
			worked, err := worker.RunOnce(ctx)
			if err != nil {
				t.Fatalf("worker: %v", err)
			}
			if !worked {
				return
			}
		}
	}

	reviewer := mcpLogin(t, env, "acc-reviewer@example.com", "secops")
	if _, err := env.pool.Exec(ctx, `UPDATE workspace_members SET roles = '{reviewer}' WHERE user_id = $1`,
		mcpUserID(t, env, "acc-reviewer@example.com")); err != nil {
		t.Fatal(err)
	}
	a := mcpLogin(t, env, "acc-a@example.com", "secops")
	b := mcpLogin(t, env, "acc-b@example.com", "secops")
	c := mcpLogin(t, env, "acc-c@example.com", "secops")
	team := map[string]string{}
	for _, name := range []string{"诉讼组", "非诉组"} {
		var id string
		if err := env.pool.QueryRow(ctx, `INSERT INTO workspace_groups (workspace_id, name) VALUES ('secops', $1) RETURNING id`, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		team[name] = id
	}
	for email, group := range map[string]string{"acc-a@example.com": "诉讼组", "acc-b@example.com": "诉讼组", "acc-c@example.com": "非诉组"} {
		if _, err := env.pool.Exec(ctx, `INSERT INTO workspace_group_members (group_id, workspace_id, user_id) VALUES ($1, 'secops', $2)`,
			team[group], mcpUserID(t, env, email)); err != nil {
			t.Fatal(err)
		}
	}
	ws := map[string]string{"X-Agent-Workspace": "secops"}
	get := func(session, path string) (int, map[string]any) {
		rec := env.do(t, http.MethodGet, path, session, "", ws)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	mcpSearch := func(session, query string) string {
		rec := env.do(t, http.MethodPost, "/mcp/wiki", session,
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_wiki","arguments":{"query":"`+query+`"}}}`, ws)
		return rec.Body.String()
	}

	// 1. A uploads an attachment and a team contribution citing it.
	attachment := "判决书摘录：回购条款在对赌失败时有效。"
	digest := sha256.Sum256([]byte(attachment))
	sum := hex.EncodeToString(digest[:])
	if rec := env.do(t, http.MethodPut, "/enterprise/v1/blobs/"+sum, a, attachment,
		map[string]string{"X-Agent-Workspace": "secops", "Content-Type": "text/plain"}); rec.Code != http.StatusCreated {
		t.Fatalf("attachment upload: %d %s", rec.Code, rec.Body.String())
	}
	upload := fmt.Sprintf(`{"local_id":"wiki/duidu-huigou","title":"对赌回购条款效力","summary":"对赌失败时回购条款有效",`+
		`"markdown":"## 要点\n对赌失败触发回购，法院支持。","requested_scope":"team","group_id":%q,"redaction_declared":true,`+
		`"upload_mode":"manual","client":{"name":"t","version":"1","schema_version":1},`+
		`"attachments":[{"sha256":%q,"name":"摘录.txt","media_type":"text/plain","size":%d}]}`, team["诉讼组"], sum, len(attachment))
	rec := env.do(t, http.MethodPost, "/enterprise/v1/contributions", a, upload,
		map[string]string{"X-Agent-Workspace": "secops", "Idempotency-Key": "acc-1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("contribution: %d %s", rec.Code, rec.Body.String())
	}
	var ctb struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ctb)

	// 2. The curator proposes a page; the reviewer publishes it.
	drain()
	_, listed := get(reviewer, "/enterprise/v1/curation/proposals")
	var proposalID, pageID string
	for _, raw := range listed["proposals"].([]any) {
		p := raw.(map[string]any)
		if p["kind"] == curation.KindCreatePage && p["status"] == "pending" {
			proposalID, pageID = p["id"].(string), p["target"].(map[string]any)["page_id"].(string)
		}
	}
	if proposalID == "" {
		t.Fatalf("no create_page proposal: %v", listed)
	}
	if rec := env.do(t, http.MethodPost, "/enterprise/v1/curation/proposals/"+proposalID+"/decision", reviewer, `{"decision":"accept"}`, ws); rec.Code != http.StatusOK {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body.String())
	}
	attachmentPath := "/enterprise/v1/wiki/pages/" + pageID + "/attachments/" + sum

	// 3. Teammate B finds and reads it everywhere; C of another team nowhere.
	if code, page := get(b, "/enterprise/v1/wiki/pages/"+pageID); code != http.StatusOK || page["visibility"] != "team" ||
		len(page["attachments"].([]any)) != 1 {
		t.Fatalf("B reads the team page with its attachment: %d %v", code, page)
	}
	if rec := env.do(t, http.MethodGet, attachmentPath, b, "", ws); rec.Code != http.StatusOK || rec.Body.String() != attachment ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("B downloads the attachment: %d %q", rec.Code, rec.Body.String())
	}
	if _, found := get(b, "/enterprise/v1/wiki/search?q=回购"); len(found["results"].([]any)) != 1 {
		t.Fatalf("B search: %v", found)
	}
	if !strings.Contains(mcpSearch(b, "回购"), pageID) {
		t.Fatal("B finds the page through the local agent's MCP tool")
	}
	if code, _ := get(c, "/enterprise/v1/wiki/pages/"+pageID); code != http.StatusNotFound {
		t.Fatalf("C reads a foreign team page: %d", code)
	}
	if rec := env.do(t, http.MethodGet, attachmentPath, c, "", ws); rec.Code != http.StatusNotFound {
		t.Fatalf("C downloads a foreign team attachment: %d", rec.Code)
	}
	if _, found := get(c, "/enterprise/v1/wiki/search?q=回购"); len(found["results"].([]any)) != 0 {
		t.Fatalf("C search: %v", found)
	}
	if strings.Contains(mcpSearch(c, "回购"), pageID) {
		t.Fatal("C finds a foreign team page through MCP")
	}
	if _, tree := get(c, "/enterprise/v1/wiki/tree"); strings.Contains(fmt.Sprint(tree["pages"]), pageID) {
		t.Fatal("C sees the page in the tree")
	}

	// 4. A withdraws: the page (its only source is gone) is paused for B.
	if rec := env.do(t, http.MethodPost, "/enterprise/v1/contributions/"+ctb.ID+"/withdraw", a, "", ws); rec.Code != http.StatusOK {
		t.Fatalf("withdraw: %d", rec.Code)
	}
	drain()
	if code, _ := get(b, "/enterprise/v1/wiki/pages/"+pageID); code != http.StatusNotFound {
		t.Fatalf("withdrawn-only page still readable: %d", code)
	}
	if rec := env.do(t, http.MethodGet, attachmentPath, b, "", ws); rec.Code != http.StatusNotFound {
		t.Fatalf("withdrawn attachment still downloadable: %d", rec.Code)
	}

	// 5. Auto actions are revertible: the pause is undone, the attachment stays gone.
	_, listed = get(reviewer, "/enterprise/v1/curation/proposals")
	for _, raw := range listed["proposals"].([]any) {
		p := raw.(map[string]any)
		if p["kind"] == curation.KindRestrictVisibility && p["status"] == "auto_applied" {
			if rec := env.do(t, http.MethodPost, "/enterprise/v1/curation/proposals/"+p["id"].(string)+"/decision", reviewer, `{"decision":"revert"}`, ws); rec.Code != http.StatusOK {
				t.Fatalf("revert pause: %d %s", rec.Code, rec.Body.String())
			}
		}
	}
	if code, page := get(b, "/enterprise/v1/wiki/pages/"+pageID); code != http.StatusOK || len(page["attachments"].([]any)) != 0 {
		t.Fatalf("reverted page without the withdrawn attachment: %d %v", code, page["attachments"])
	}

	// 6. A restarted gateway (fresh stores over the same database) serves the same state.
	wire()
	if code, _ := get(b, "/enterprise/v1/wiki/pages/"+pageID); code != http.StatusOK {
		t.Fatalf("after restart, B: %d", code)
	}
	if code, _ := get(c, "/enterprise/v1/wiki/pages/"+pageID); code != http.StatusNotFound {
		t.Fatalf("after restart, C: %d", code)
	}
}

//go:build integration

// Knowledge layer: admin-built shelf/book/chapter tree, wiki pages placed by
// slug, server-rendered sanitized HTML, [[links]] resolved only to readable
// targets, tags, the source chain to contribution versions, optimistic lock,
// keyword search limited to readable published pages, and path lookup.

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
	"enterprise-agent-platform/packages/wiki"
)

func TestWikiKnowledgeLayer(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store
	env.srv.DLP = dlp.NewTableScanner(env.store.LoadDLPRules)
	env.srv.Content = content.New(env.pool, env.store)
	env.srv.Contributions = contribution.New(env.pool, env.store)
	env.srv.Wiki = wiki.NewStore(env.pool, env.store)
	env.handler = env.srv.Handler()

	adminSession := mcpLogin(t, env, "wiki-admin@example.com", "secops")
	if _, err := env.pool.Exec(ctx, `UPDATE workspace_members SET roles = '{admin}' WHERE user_id = $1`,
		mcpUserID(t, env, "wiki-admin@example.com")); err != nil {
		t.Fatal(err)
	}
	authorSession := mcpLogin(t, env, "wiki-author@example.com", "secops")
	authorID := mcpUserID(t, env, "wiki-author@example.com")
	outsiderSession := mcpLogin(t, env, "wiki-outsider@example.com", "secops")
	var team string
	if err := env.pool.QueryRow(ctx, `INSERT INTO workspace_groups (workspace_id, name) VALUES ('secops', '诉讼组') RETURNING id`).Scan(&team); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `INSERT INTO workspace_group_members (group_id, workspace_id, user_id) VALUES ($1, 'secops', $2)`, team, authorID); err != nil {
		t.Fatal(err)
	}
	ws := map[string]string{"X-Agent-Workspace": "secops"}
	call := func(session, method, path, body string) (int, map[string]any) {
		rec := env.do(t, method, path, session, body, ws)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// Admin builds the tree; levels and sibling slugs are enforced.
	node := func(parent, level, slug string) string {
		body := `{"level":"` + level + `","slug":"` + slug + `","title":"` + slug + `","parent_id":"` + parent + `"}`
		code, out := call(adminSession, http.MethodPost, "/enterprise/v1/wiki/nodes", body)
		if code != http.StatusCreated {
			t.Fatalf("create %s %s: %d %v", level, slug, code, out)
		}
		return out["id"].(string)
	}
	shelf := node("", "shelf", "诉讼")
	book := node(shelf, "book", "股权纠纷")
	chapter := node(book, "chapter", "裁判观点")
	if code, _ := call(adminSession, http.MethodPost, "/enterprise/v1/wiki/nodes", `{"level":"chapter","slug":"x","title":"x","parent_id":"`+shelf+`"}`); code != http.StatusBadRequest {
		t.Fatalf("chapter under shelf must be refused: %d", code)
	}
	if code, _ := call(adminSession, http.MethodPost, "/enterprise/v1/wiki/nodes", `{"level":"book","slug":"股权纠纷","title":"dup","parent_id":"`+shelf+`"}`); code != http.StatusConflict {
		t.Fatalf("sibling slug clash: %d", code)
	}
	if code, _ := call(authorSession, http.MethodPost, "/enterprise/v1/wiki/nodes", `{"level":"shelf","slug":"x","title":"x"}`); code != http.StatusForbidden {
		t.Fatalf("member creating nodes: %d", code)
	}

	// A contribution the page cites.
	rec := env.do(t, http.MethodPost, "/enterprise/v1/contributions", authorSession,
		`{"local_id":"wiki/source","title":"原稿","markdown":"原始材料","requested_scope":"organization","redaction_declared":true,"upload_mode":"manual","client":{"name":"t","version":"1","schema_version":1}}`,
		map[string]string{"X-Agent-Workspace": "secops", "Idempotency-Key": "wiki-src-1"})
	var ctb struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ctb)

	// The target of a [[link]]: a team page the outsider may not read.
	page := func(slug, visibility, extra, body string) string {
		group := ""
		if visibility == "team" {
			group = `"group_id":"` + team + `",`
		}
		code, out := call(authorSession, http.MethodPost, "/enterprise/v1/resources",
			`{"kind":"wiki","visibility":"`+visibility+`",`+group+`"node_id":"`+chapter+`","slug":"`+slug+`","title":"`+slug+`","summary":"结论","body":"`+body+`"`+extra+`}`)
		if code != http.StatusCreated {
			t.Fatalf("create page %s: %d %v", slug, code, out)
		}
		id := out["id"].(string)
		call(authorSession, http.MethodPost, "/enterprise/v1/resources/"+id+"/submit", "")
		if code, out := call(adminSession, http.MethodPost, "/enterprise/v1/resources/"+id+"/approve", ""); code != http.StatusOK {
			t.Fatalf("approve %s: %d %v", slug, code, out)
		}
		return id
	}
	hidden := page("内部口径", "team", "", "团队内部")
	main := page("回购条款", "organization",
		`,"tags":["回购","股权"],"sources":[{"contribution_id":"`+ctb.ID+`","version":1,"anchor":"要点"}]`,
		`## 要点\n参见 [[诉讼/股权纠纷/裁判观点/内部口径]]。<script>alert(1)</script>`)
	if code, _ := call(authorSession, http.MethodPost, "/enterprise/v1/resources",
		`{"kind":"wiki","visibility":"organization","node_id":"`+chapter+`","slug":"回购条款","title":"dup","summary":"","body":"x"}`); code != http.StatusConflict {
		t.Fatalf("page slug clash: %d", code)
	}

	// Reading: sanitized HTML, path, tags, source chain; links filtered per reader.
	code, view := call(outsiderSession, http.MethodGet, "/enterprise/v1/wiki/pages/"+main, "")
	if code != http.StatusOK || strings.Contains(view["html"].(string), "<script") || view["path"] != "诉讼/股权纠纷/裁判观点/回购条款" {
		t.Fatalf("outsider page view: %d %v", code, view)
	}
	if links := view["links"].([]any); len(links) != 0 {
		t.Fatalf("outsider must not see the team page link: %v", links)
	}
	if sources := view["sources"].([]any); len(sources) != 1 || sources[0].(map[string]any)["anchor"] != "要点" {
		t.Fatalf("source chain: %v", view["sources"])
	}
	if tags := view["tags"].([]any); len(tags) != 2 {
		t.Fatalf("tags: %v", view["tags"])
	}
	if _, authorView := call(authorSession, http.MethodGet, "/enterprise/v1/wiki/by-path/诉讼/股权纠纷/裁判观点/回购条款", ""); len(authorView["links"].([]any)) != 1 {
		t.Fatalf("teammate sees the link target: %v", authorView["links"])
	}
	if code, _ := call(outsiderSession, http.MethodGet, "/enterprise/v1/wiki/pages/"+hidden, ""); code != http.StatusNotFound {
		t.Fatalf("outsider reading team page: %d", code)
	}

	// Tree and search only show what the reader may open.
	_, tree := call(outsiderSession, http.MethodGet, "/enterprise/v1/wiki/tree", "")
	if len(tree["nodes"].([]any)) != 3 || len(tree["pages"].([]any)) != 1 {
		t.Fatalf("outsider tree: %v", tree)
	}
	_, found := call(outsiderSession, http.MethodGet, "/enterprise/v1/wiki/search?q=团队内部", "")
	if len(found["results"].([]any)) != 0 {
		t.Fatalf("search leaked a team page: %v", found)
	}
	_, found = call(authorSession, http.MethodGet, "/enterprise/v1/wiki/search?q=参见", "")
	if results := found["results"].([]any); len(results) != 1 || !strings.Contains(results[0].(map[string]any)["snippet"].(string), "参见") {
		t.Fatalf("search snippet: %v", found)
	}

	// The knowledge MCP server answers with the same authorization.
	rpc := func(session, method, params string) map[string]any {
		rec := env.do(t, http.MethodPost, "/mcp/wiki", session,
			`{"jsonrpc":"2.0","id":7,"method":"`+method+`","params":`+params+`}`, ws)
		out := map[string]any{}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("mcp %s: %d %s", method, rec.Code, rec.Body.String())
		}
		return out
	}
	toolText := func(session, name, args string) (string, bool) {
		result := rpc(session, "tools/call", `{"name":"`+name+`","arguments":`+args+`}`)["result"].(map[string]any)
		return result["content"].([]any)[0].(map[string]any)["text"].(string), result["isError"] == true
	}
	if rec := env.do(t, http.MethodPost, "/mcp/wiki", outsiderSession, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, ws); rec.Code != http.StatusAccepted {
		t.Fatalf("mcp notification: %d", rec.Code)
	}
	if tools := rpc(outsiderSession, "tools/list", `{}`)["result"].(map[string]any)["tools"].([]any); len(tools) != 3 {
		t.Fatalf("mcp tools: %v", tools)
	}
	if text, isErr := toolText(outsiderSession, "search_wiki", `{"query":"团队内部"}`); isErr || strings.Contains(text, "内部口径") {
		t.Fatalf("mcp search leaked a team page: %s", text)
	}
	if text, isErr := toolText(outsiderSession, "read_wiki_page", `{"path":"诉讼/股权纠纷/裁判观点/回购条款"}`); isErr ||
		!strings.Contains(text, "参见") || strings.Contains(text, "<p>") {
		t.Fatalf("mcp read by path returns Markdown only: %s", text)
	}
	if text, isErr := toolText(outsiderSession, "read_wiki_page", `{"page_id":"`+hidden+`"}`); !isErr || strings.Contains(text, "团队内部") {
		t.Fatalf("mcp read of an unreadable page: %v %s", isErr, text)
	}

	// Optimistic lock: a stale base_revision is refused.
	stale := `{"kind":"wiki","visibility":"organization","node_id":"` + chapter + `","slug":"回购条款","title":"改","summary":"","body":"x","base_revision":0}`
	if code, _ := call(authorSession, http.MethodPut, "/enterprise/v1/resources/"+main, stale); code != http.StatusConflict {
		t.Fatalf("stale base_revision: %d", code)
	}
	fresh := strings.Replace(stale, `"base_revision":0`, `"base_revision":1`, 1)
	if code, out := call(authorSession, http.MethodPut, "/enterprise/v1/resources/"+main, fresh); code != http.StatusOK {
		t.Fatalf("current base_revision: %d %v", code, out)
	}

	// Renaming a chapter keeps old paths (and [[links]] to them) resolving.
	if code, out := call(adminSession, http.MethodPatch, "/enterprise/v1/wiki/nodes/"+chapter, `{"slug":"观点","title":"观点"}`); code != http.StatusOK {
		t.Fatalf("rename chapter: %d %v", code, out)
	}
	if code, moved := call(outsiderSession, http.MethodGet, "/enterprise/v1/wiki/by-path/诉讼/股权纠纷/裁判观点/回购条款", ""); code != http.StatusOK ||
		moved["id"] != main || moved["path"] != "诉讼/股权纠纷/观点/回购条款" {
		t.Fatalf("old path after rename: %d %v", code, moved)
	}
	if code, _ := call(outsiderSession, http.MethodGet, "/enterprise/v1/wiki/by-path/诉讼/股权纠纷/裁判观点/内部口径", ""); code != http.StatusNotFound {
		t.Fatalf("an alias never bypasses authorization: %d", code)
	}

	// Nodes with pages cannot be deleted.
	if code, _ := call(adminSession, http.MethodDelete, "/enterprise/v1/wiki/nodes/"+chapter, ""); code != http.StatusConflict {
		t.Fatalf("deleting a chapter with pages: %d", code)
	}
}

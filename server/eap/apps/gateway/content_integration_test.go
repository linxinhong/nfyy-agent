//go:build integration

// Shared content acceptance (docs/knowledge-platform-architecture.md §10.9,
// ported from the MindLink center): A writes, the team lead publishes to
// team A, teammate B finds and reads it through session and employee key,
// member C of team B gets nothing from list, detail or review queue, and a
// withdrawal removes it from B again. Also covers owner self-review refusal,
// published-revision stability while a new draft exists, DLP gating, group
// deletion refusal and audit evidence.

package main_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/content"
	"enterprise-agent-platform/packages/dlp"
)

type contentMember struct {
	id      string
	session string
}

func TestContentReviewAndVisibility(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store
	env.srv.DLP = dlp.NewTableScanner(env.store.LoadDLPRules)
	env.srv.Content = content.New(env.pool, env.store)
	env.handler = env.srv.Handler()

	member := func(email string) contentMember {
		session := mcpLogin(t, env, email, "secops")
		return contentMember{id: mcpUserID(t, env, email), session: session}
	}
	a, b, c, lead, reviewer := member("a@example.com"), member("b@example.com"),
		member("c@example.com"), member("lead@example.com"), member("rev@example.com")

	var teamA, teamB string
	for name, dst := range map[string]*string{"诉讼组": &teamA, "非诉组": &teamB} {
		if err := env.pool.QueryRow(ctx,
			`INSERT INTO workspace_groups (workspace_id, name) VALUES ('secops', $1) RETURNING id`, name).Scan(dst); err != nil {
			t.Fatalf("group %s: %v", name, err)
		}
	}
	for _, g := range []struct {
		user, group string
		lead        bool
	}{{a.id, teamA, false}, {b.id, teamA, false}, {lead.id, teamA, true}, {c.id, teamB, false}} {
		teamRoles := []string{}
		if g.lead {
			teamRoles = []string{"lead"}
		}
		if _, err := env.pool.Exec(ctx, `INSERT INTO workspace_group_members (group_id, workspace_id, user_id, team_roles)
			VALUES ($1, 'secops', $2, $3)`, g.group, g.user, teamRoles); err != nil {
			t.Fatalf("group member: %v", err)
		}
	}
	if _, err := env.pool.Exec(ctx, `UPDATE workspace_members SET roles = '{reviewer}' WHERE user_id = $1`, reviewer.id); err != nil {
		t.Fatalf("grant reviewer: %v", err)
	}

	ws := map[string]string{"X-Agent-Workspace": "secops"}
	call := func(m contentMember, method, path, body string) (int, string) {
		rec := env.do(t, method, path, m.session, body, ws)
		return rec.Code, rec.Body.String()
	}
	decode := func(body string) content.Resource {
		var r content.Resource
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatalf("decode resource: %v %s", err, body)
		}
		return r
	}

	// A drafts a long team Wiki entry; the old 800-character cap is gone.
	longBody := strings.Repeat("裁判观点要点。", 300)
	code, body := call(a, http.MethodPost, "/enterprise/v1/resources",
		`{"kind":"wiki","visibility":"team","group_id":"`+teamA+`","title":"股权回购纠纷","summary":"一句话结论","body":"`+longBody+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	wiki := decode(body)
	path := "/enterprise/v1/resources/" + wiki.ID

	if code, _ := call(c, http.MethodPost, "/enterprise/v1/resources",
		`{"kind":"wiki","visibility":"team","group_id":"`+teamA+`","title":"冒名","summary":"","body":"x"}`); code != http.StatusForbidden {
		t.Fatalf("outsider creating for another team: %d", code)
	}
	if code, _ := call(b, http.MethodGet, path, ""); code != http.StatusNotFound {
		t.Fatalf("teammate must not see a draft: %d", code)
	}
	if code, _ := call(a, http.MethodPost, path+"/submit", ""); code != http.StatusOK {
		t.Fatalf("submit: %d", code)
	}
	if code, _ := call(a, http.MethodPost, path+"/approve", ""); code != http.StatusForbidden {
		t.Fatalf("owner self-approval: %d", code)
	}
	if code, _ := call(c, http.MethodGet, "/enterprise/v1/review/queue", ""); code != http.StatusForbidden {
		t.Fatalf("plain member review queue: %d", code)
	}
	if code, body := call(lead, http.MethodGet, "/enterprise/v1/review/queue", ""); code != http.StatusOK || !strings.Contains(body, wiki.ID) {
		t.Fatalf("lead queue: %d %s", code, body)
	}
	if code, _ := call(c, http.MethodPost, path+"/approve", ""); code != http.StatusNotFound {
		t.Fatalf("outsider approve must not reveal the resource: %d", code)
	}
	if code, body := call(lead, http.MethodPost, path+"/approve", ""); code != http.StatusOK || decode(body).PublishedRevision == nil {
		t.Fatalf("lead approve: %d %s", code, body)
	}

	// B reads through the session and through an employee key (desktop path).
	if code, body := call(b, http.MethodGet, "/enterprise/v1/resources?kind=wiki", ""); code != http.StatusOK || !strings.Contains(body, wiki.ID) {
		t.Fatalf("teammate list: %d %s", code, body)
	}
	bKey := seedKey(t, env, b.id)
	if rec := env.do(t, http.MethodGet, path, bKey, "", ws); rec.Code != http.StatusOK {
		t.Fatalf("teammate key read: %d %s", rec.Code, rec.Body.String())
	}
	if code, body := call(reviewer, http.MethodGet, "/enterprise/v1/resources", ""); code != http.StatusOK || !strings.Contains(body, wiki.ID) {
		t.Fatalf("reviewer list: %d %s", code, body)
	}
	// C gets nothing anywhere.
	if code, body := call(c, http.MethodGet, "/enterprise/v1/resources?q=股权", ""); code != http.StatusOK || strings.Contains(body, wiki.ID) {
		t.Fatalf("other team list leak: %d %s", code, body)
	}
	if code, _ := call(c, http.MethodGet, path, ""); code != http.StatusNotFound {
		t.Fatalf("other team detail: %d", code)
	}

	// A new draft never replaces what B sees until it is approved.
	code, body = call(a, http.MethodPut, path,
		`{"kind":"wiki","visibility":"team","group_id":"`+teamA+`","title":"股权回购纠纷（修订）","summary":"一句话结论","body":"新版"}`)
	if code != http.StatusOK || decode(body).Revision != 2 {
		t.Fatalf("edit: %d %s", code, body)
	}
	if code, body := call(b, http.MethodGet, path, ""); code != http.StatusOK || decode(body).Title != "股权回购纠纷" {
		t.Fatalf("teammate must keep the published revision: %d %s", code, body)
	}

	// Organization news reaches C once a reviewer approves it.
	code, body = call(a, http.MethodPost, "/enterprise/v1/resources",
		`{"kind":"news","visibility":"organization","title":"所内公告","summary":"","body":"全所可见"}`)
	news := decode(body)
	call(a, http.MethodPost, "/enterprise/v1/resources/"+news.ID+"/submit", "")
	// A lead may not review organization content and may not see its draft.
	if code, _ := call(lead, http.MethodPost, "/enterprise/v1/resources/"+news.ID+"/approve", ""); code != http.StatusNotFound {
		t.Fatalf("lead must not review organization content: %d", code)
	}
	if code, _ := call(reviewer, http.MethodPost, "/enterprise/v1/resources/"+news.ID+"/approve", ""); code != http.StatusOK {
		t.Fatalf("reviewer approve: %d", code)
	}
	if code, _ := call(c, http.MethodGet, "/enterprise/v1/resources/"+news.ID, ""); code != http.StatusOK {
		t.Fatalf("organization news for other team: %d", code)
	}

	// Withdrawal removes the Wiki entry from B everywhere.
	if code, _ := call(a, http.MethodPost, path+"/withdraw", ""); code != http.StatusOK {
		t.Fatalf("withdraw: %d", code)
	}
	if code, _ := call(b, http.MethodGet, path, ""); code != http.StatusNotFound {
		t.Fatalf("withdrawn detail: %d", code)
	}
	if _, body := call(b, http.MethodGet, "/enterprise/v1/resources", ""); strings.Contains(body, wiki.ID) {
		t.Fatalf("withdrawn resource still listed")
	}

	// DLP: deny-class content is refused and never stored.
	rec := env.do(t, http.MethodPost, "/enterprise/v1/resources", a.session,
		`{"kind":"news","visibility":"organization","title":"泄露","summary":"","body":"key sk-ABCDEFGHIJKLMNOPQRSTUVWX"}`, ws)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "dlp_blocked") {
		t.Fatalf("dlp deny: %d %s", rec.Code, rec.Body.String())
	}
	var leaked int
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM content_revisions WHERE body LIKE '%sk-ABCDEF%'`).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("blocked content stored: %d %v", leaked, err)
	}

	// A group that still scopes content cannot be deleted.
	if err := env.store.DeleteGroup(ctx, "secops", teamA, audit.NewEvent("admin.group_deleted", audit.OutcomeAllowed)); err == nil {
		t.Fatalf("deleting a group with team content must fail")
	}

	for _, typ := range []string{"content.create", "content.submit", "content.approve", "content.edit", "content.withdraw"} {
		var n int
		if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE type = $1 AND workspace_id = 'secops'`, typ).Scan(&n); err != nil || n == 0 {
			t.Fatalf("audit %s missing: %d %v", typ, n, err)
		}
	}
}

//go:build integration

// Skill sharing: a protected team skill is reviewed by its team's reviewer
// (never by its author), its SKILL.md never appears in a download, only the
// team sees and enables it, requirements need skill.require at the audience,
// personal skills stay with their author without review, rejections carry a
// reason back to the author, Workspace-wide registry readers never list
// team/protected skills, and every change is audited.

package main_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"enterprise-agent-platform/packages/content"
	"enterprise-agent-platform/packages/rbac"
	"enterprise-agent-platform/packages/skillshare"
)

func TestSkillSharing(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store
	env.srv.Members = env.store
	env.srv.Content = content.New(env.pool, env.store)
	env.srv.Skills = skillshare.New(env.pool, env.store)
	keyPub, keyPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `INSERT INTO skill_package_keys (kid, public_key, active) VALUES ('pkg-share', $1, true)`,
		base64.RawURLEncoding.EncodeToString(keyPub)); err != nil {
		t.Fatalf("seed package key: %v", err)
	}
	env.srv.PackageKeys = map[string]ed25519.PrivateKey{"pkg-share": keyPriv}
	env.handler = env.srv.Handler()

	type member struct{ id, session, key string }
	join := func(email string) member {
		session := mcpLogin(t, env, email, "secops")
		id := mcpUserID(t, env, email)
		return member{id: id, session: session, key: seedKey(t, env, id)}
	}
	author, mate, lead, outsider, otherLead, admin := join("sk-author@example.com"), join("sk-mate@example.com"),
		join("sk-lead@example.com"), join("sk-out@example.com"), join("sk-olead@example.com"), join("sk-admin@example.com")
	if _, err := env.pool.Exec(ctx, `UPDATE workspace_members SET roles = '{admin}' WHERE user_id = $1`, admin.id); err != nil {
		t.Fatal(err)
	}
	var teamA, teamB string
	for name, id := range map[string]*string{"并购组": &teamA, "诉讼组": &teamB} {
		if err := env.pool.QueryRow(ctx, `INSERT INTO workspace_groups (workspace_id, name) VALUES ('secops', $1) RETURNING id`, name).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	for _, g := range []struct {
		user, group string
		roles       []string
	}{{author.id, teamA, nil}, {mate.id, teamA, nil}, {lead.id, teamA, []string{"lead"}}, {outsider.id, teamB, nil}, {otherLead.id, teamB, []string{"lead"}}} {
		if _, err := env.pool.Exec(ctx, `INSERT INTO workspace_group_members (group_id, workspace_id, user_id, team_roles) VALUES ($1, 'secops', $2, $3)`,
			g.group, g.user, append([]string{}, g.roles...)); err != nil {
			t.Fatal(err)
		}
	}

	ws := map[string]string{"X-Agent-Workspace": "secops"}
	call := func(method, path, bearer, body string) *httpResult {
		t.Helper()
		rec := env.do(t, method, path, bearer, body, ws)
		return &httpResult{code: rec.Code, body: rec.Body.String(), raw: rec.Body.Bytes()}
	}
	skillMD := "---\nname: equity-review\ndescription: 对赌回购条款审查要点\n---\n# 审查步骤\n1. 先看回购触发条件。\n"
	publish := func(who member, name, version, visibility string) *httpResult {
		body, _ := json.Marshal(map[string]any{"version": version, "visibility": visibility, "protected": true, "files": []map[string]string{
			{"path": "SKILL.md", "content": skillMD}, {"path": "templates/checklist.md", "content": "- 回购价格\n"}}})
		return call(http.MethodPost, "/enterprise/v1/skills/"+name+"/versions", who.key, string(body))
	}

	// Publish a protected team skill: pending review, description parsed.
	if r := publish(author, "equity-review", "1.0.0", "team"); r.code != http.StatusCreated ||
		!strings.Contains(r.body, `"status":"pending_review"`) || !strings.Contains(r.body, "对赌回购条款审查要点") || !strings.Contains(r.body, `"group_id":"`+teamA) {
		t.Fatalf("publish: %d %s", r.code, r.body)
	}
	if r := call(http.MethodGet, "/enterprise/v1/skills/equity-review/versions/1.0.0/download", mate.key, ""); r.code != http.StatusNotFound {
		t.Fatalf("download before release: %d", r.code)
	}
	if r := call(http.MethodGet, "/enterprise/v1/skills/equity-review/versions/1.0.0/content", author.key, ""); r.code != http.StatusOK ||
		!strings.Contains(r.body, "先看回购触发条件") || !strings.Contains(r.body, "templates/checklist.md") {
		t.Fatalf("author reads content: %d %s", r.code, r.body)
	}
	if r := call(http.MethodGet, "/enterprise/v1/skills/equity-review/versions/1.0.0/content", outsider.key, ""); r.code != http.StatusNotFound {
		t.Fatalf("outsider reads content: %d", r.code)
	}

	// Review: only skill.review at the team, never the author.
	if r := call(http.MethodGet, "/enterprise/v1/skill-reviews", lead.session, ""); !strings.Contains(r.body, "equity-review") {
		t.Fatalf("lead's queue: %s", r.body)
	}
	if r := call(http.MethodGet, "/enterprise/v1/skill-reviews", otherLead.session, ""); strings.Contains(r.body, "equity-review") {
		t.Fatalf("other team's lead sees it: %s", r.body)
	}
	if r := call(http.MethodPost, "/enterprise/v1/skills/equity-review/versions/1.0.0/review", otherLead.session, "{}"); r.code != http.StatusForbidden {
		t.Fatalf("other team's lead reviews: %d %s", r.code, r.body)
	}
	for _, step := range []string{"review", "release"} {
		if r := call(http.MethodPost, "/enterprise/v1/skills/equity-review/versions/1.0.0/"+step, lead.session, "{}"); r.code != http.StatusOK {
			t.Fatalf("lead %s: %d %s", step, r.code, r.body)
		}
	}
	if r := publish(lead, "lead-own", "1.0.0", "team"); r.code != http.StatusCreated {
		t.Fatalf("lead publishes: %d %s", r.code, r.body)
	}
	if r := call(http.MethodPost, "/enterprise/v1/skills/lead-own/versions/1.0.0/review", lead.session, "{}"); r.code != http.StatusForbidden {
		t.Fatalf("self review: %d %s", r.code, r.body)
	}

	// The download carries resources and the marker, never SKILL.md.
	r := call(http.MethodGet, "/enterprise/v1/skills/equity-review/versions/1.0.0/download", mate.key, "")
	if r.code != http.StatusOK {
		t.Fatalf("teammate download: %d %s", r.code, r.body)
	}
	files := untar(t, r.raw)
	if _, ok := files["SKILL.md"]; ok || files["templates/checklist.md"] == "" || files[skillshare.MarkerFile] == "" {
		t.Fatalf("protected archive: %v", keys(files))
	}
	if r := call(http.MethodGet, "/enterprise/v1/skills/equity-review/versions/1.0.0/download", outsider.key, ""); r.code != http.StatusNotFound {
		t.Fatalf("outsider download: %d", r.code)
	}

	// Shelf and enablement.
	shelf := func(who member) map[string]map[string]any {
		t.Helper()
		var out struct {
			Skills []map[string]any `json:"skills"`
		}
		r := call(http.MethodGet, "/enterprise/v1/me/skills", who.key, "")
		_ = json.Unmarshal(r.raw, &out)
		m := map[string]map[string]any{}
		for _, s := range out.Skills {
			m[s["name"].(string)+"@"+s["version"].(string)] = s
		}
		return m
	}
	if s := shelf(mate)["equity-review@1.0.0"]; s == nil || s["active"] != false || s["has_resources"] != true || s["author_id"] != author.id {
		t.Fatalf("teammate shelf: %v", s)
	}
	if _, ok := shelf(outsider)["equity-review@1.0.0"]; ok {
		t.Fatal("outsider sees a team skill")
	}
	if r := call(http.MethodPut, "/enterprise/v1/me/skills/equity-review", outsider.key, ""); r.code != http.StatusNotFound {
		t.Fatalf("outsider enables: %d", r.code)
	}
	if r := call(http.MethodPut, "/enterprise/v1/me/skills/equity-review", mate.key, ""); r.code != http.StatusNoContent {
		t.Fatalf("enable: %d %s", r.code, r.body)
	}
	if s := shelf(mate)["equity-review@1.0.0"]; s["enabled"] != true || s["active"] != true {
		t.Fatalf("enabled shelf: %v", s)
	}

	// Requirements need skill.require where they apply.
	if r := call(http.MethodPut, "/enterprise/v1/skills/equity-review/requirement?group_id="+teamA, otherLead.key, ""); r.code != http.StatusForbidden {
		t.Fatalf("other lead requires for team A: %d", r.code)
	}
	if r := call(http.MethodPut, "/enterprise/v1/skills/equity-review/requirement", lead.key, ""); r.code != http.StatusForbidden {
		t.Fatalf("team lead requires Workspace-wide: %d", r.code)
	}
	if r := call(http.MethodPut, "/enterprise/v1/skills/equity-review/requirement", admin.key, ""); r.code != http.StatusBadRequest {
		t.Fatalf("team skill required for everyone: %d %s", r.code, r.body)
	}
	if r := call(http.MethodPut, "/enterprise/v1/skills/equity-review/requirement?group_id="+teamA, lead.key, ""); r.code != http.StatusNoContent {
		t.Fatalf("lead requires for team: %d %s", r.code, r.body)
	}
	if s := shelf(author)["equity-review@1.0.0"]; s["required"] != true || s["required_scope"] != "team" || s["active"] != true {
		t.Fatalf("required shelf: %v", s)
	}

	// The gateway's injection source: active, visible, protected skills only.
	viewer := func(m member) skillshare.Viewer {
		g, err := rbac.Load(ctx, env.pool, "secops", m.id, []string{"developer"})
		if err != nil {
			t.Fatal(err)
		}
		return skillshare.Viewer{UserID: m.id, Grants: g}
	}
	loaded, skipped, err := env.srv.Skills.Bodies(ctx, "secops", viewer(mate), []string{"equity-review", "unknown"})
	if err != nil || len(loaded) != 1 || !strings.Contains(loaded[0].Body, "先看回购触发条件") || len(skipped) != 1 {
		t.Fatalf("bodies for teammate: %+v %v %v", loaded, skipped, err)
	}
	if loaded, _, _ := env.srv.Skills.Bodies(ctx, "secops", viewer(outsider), []string{"equity-review"}); len(loaded) != 0 {
		t.Fatal("outsider gets a team skill body")
	}

	// Personal skills: private to the author, no review.
	if r := publish(mate, "my-notes", "0.1.0", "personal"); r.code != http.StatusCreated || !strings.Contains(r.body, `"status":"private"`) {
		t.Fatalf("personal publish: %d %s", r.code, r.body)
	}
	if s := shelf(mate)["my-notes@0.1.0"]; s == nil || s["active"] != true {
		t.Fatalf("personal shelf: %v", s)
	}
	if _, ok := shelf(author)["my-notes@0.1.0"]; ok {
		t.Fatal("a personal skill leaks to a teammate")
	}
	if r := call(http.MethodPost, "/enterprise/v1/skills/my-notes/versions/0.1.0/review", admin.session, "{}"); r.code != http.StatusConflict {
		t.Fatalf("review personal: %d", r.code)
	}

	// Rejection reaches the author with its reason.
	if r := publish(author, "equity-review", "1.1.0", "team"); r.code != http.StatusCreated {
		t.Fatalf("publish 1.1.0: %d %s", r.code, r.body)
	}
	if r := call(http.MethodPost, "/enterprise/v1/skills/equity-review/versions/1.1.0/reject", lead.key, `{"reason":"缺少裁判依据"}`); r.code != http.StatusOK {
		t.Fatalf("reject: %d %s", r.code, r.body)
	}
	if s := shelf(author)["equity-review@1.1.0"]; s == nil || s["status"] != "rejected" || s["reject_reason"] != "缺少裁判依据" {
		t.Fatalf("author sees rejection: %v", s)
	}
	if s := shelf(mate)["equity-review@1.0.0"]; s == nil {
		t.Fatal("the released version stays in use after a rejected update")
	}

	// Workspace-wide registry readers never see team or protected skills.
	released, err := env.store.ListReleasedSkillsForWorkspace(ctx, "secops")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range released {
		if s.Name == "equity-review" {
			t.Fatalf("team skill in the Workspace registry: %+v", released)
		}
	}

	for _, typ := range []string{"skill.published", "skill.rejected", "skill.requirement", "skill.enablement", "skill.content_viewed", "skill.reviewed", "skill.released"} {
		var n int
		if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE type = $1`, typ).Scan(&n); err != nil || n == 0 {
			t.Fatalf("audit %s missing: %d %v", typ, n, err)
		}
	}
}

type httpResult struct {
	code int
	body string
	raw  []byte
}

func untar(t *testing.T, archive []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out[h.Name] = string(b)
	}
}

func keys(m map[string]string) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

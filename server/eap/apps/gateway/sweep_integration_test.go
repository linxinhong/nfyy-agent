//go:build integration

// Global re-curation (sweep): an admin queues a sweep of the Workspace (at
// most one open at a time); the baseline sweep marks a later same-title page
// as a duplicate and links pages sharing tags, never pairing pages of
// different audiences; the scheduler skips a recently swept Workspace; a
// model-style merge proposed by a sweep carries the absorbed pages' sources,
// so team material is refused in an organization page.

package main_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"enterprise-agent-platform/packages/content"
	"enterprise-agent-platform/packages/curation"
	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/wiki"
)

type sweeperFunc struct {
	curation.Baseline
	sweep func([]curation.SweepPage) []curation.Proposed
}

func (f sweeperFunc) Sweep(_ context.Context, pages []curation.SweepPage) ([]curation.Proposed, error) {
	return f.sweep(pages), nil
}

func TestCurationSweep(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store
	env.srv.DLP = dlp.NewTableScanner(env.store.LoadDLPRules)
	env.srv.Content = content.New(env.pool, env.store)
	env.srv.Wiki = wiki.NewStore(env.pool, env.store)
	env.srv.Curation = curation.New(env.pool, env.store, env.srv.Content)
	env.handler = env.srv.Handler()
	worker := &curation.Worker{Store: env.srv.Curation, Content: env.srv.Content, Curator: curation.Baseline{},
		Owner: "sweep-test", Lease: time.Minute, Idle: time.Millisecond}
	drain := func() {
		for i := 0; i < 5; i++ {
			if worked, err := worker.RunOnce(ctx); err != nil {
				t.Fatalf("worker: %v", err)
			} else if !worked {
				return
			}
		}
	}

	admin := mcpLogin(t, env, "sweep-admin@example.com", "secops")
	if _, err := env.pool.Exec(ctx, `UPDATE workspace_members SET roles = '{admin}' WHERE user_id = $1`,
		mcpUserID(t, env, "sweep-admin@example.com")); err != nil {
		t.Fatal(err)
	}
	member := mcpLogin(t, env, "sweep-member@example.com", "secops")
	reviewer := mcpLogin(t, env, "sweep-reviewer@example.com", "secops")
	if _, err := env.pool.Exec(ctx, `UPDATE workspace_members SET roles = '{reviewer}' WHERE user_id = $1`,
		mcpUserID(t, env, "sweep-reviewer@example.com")); err != nil {
		t.Fatal(err)
	}
	var team string
	if err := env.pool.QueryRow(ctx, `INSERT INTO workspace_groups (workspace_id, name) VALUES ('secops', '诉讼组') RETURNING id`).Scan(&team); err != nil {
		t.Fatal(err)
	}
	ws := map[string]string{"X-Agent-Workspace": "secops"}
	publish := func(title, visibility, tags string) string {
		group := ""
		if visibility == "team" {
			group = `"group_id":"` + team + `",`
		}
		rec := env.do(t, http.MethodPost, "/enterprise/v1/resources", admin,
			`{"kind":"wiki","visibility":"`+visibility+`",`+group+`"title":"`+title+`","summary":"s","body":"b","tags":`+tags+`}`, ws)
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", title, rec.Code, rec.Body.String())
		}
		env.do(t, http.MethodPost, "/enterprise/v1/resources/"+out.ID+"/submit", admin, "", ws)
		if rec := env.do(t, http.MethodPost, "/enterprise/v1/resources/"+out.ID+"/approve", reviewer, "", ws); rec.Code != http.StatusOK {
			t.Fatalf("publish %s: %d %s", title, rec.Code, rec.Body.String())
		}
		time.Sleep(5 * time.Millisecond) // distinct updated_at ordering
		return out.ID
	}
	first := publish("回购条款", "organization", `["公司","回购"]`)
	teamPage := publish("回购条款", "team", `["公司","回购"]`)
	second := publish("回购条款", "organization", `["公司","回购"]`)

	// Only admins queue sweeps; one open sweep per Workspace.
	if rec := env.do(t, http.MethodPost, "/enterprise/v1/curation/sweeps", member, "", ws); rec.Code != http.StatusForbidden {
		t.Fatalf("member queued a sweep: %d", rec.Code)
	}
	for i, want := range []bool{true, false} {
		rec := env.do(t, http.MethodPost, "/enterprise/v1/curation/sweeps", admin, "", ws)
		var out struct {
			Queued bool `json:"queued"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusAccepted || out.Queued != want {
			t.Fatalf("sweep request %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	drain()
	_, err := env.srv.Curation.EnqueueSweeps(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var open int
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM curation_jobs WHERE kind = 'sweep' AND status IN ('queued', 'running')`).Scan(&open); err != nil || open != 0 {
		t.Fatalf("a recently swept Workspace is not scheduled again: %d %v", open, err)
	}

	rows, err := env.pool.Query(ctx, `SELECT kind, status, target_resource_id, payload FROM curation_proposals`)
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ kind, status, target, payload string }
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.kind, &r.status, &r.target, &r.payload); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	rows.Close()
	if len(got) != 2 {
		t.Fatalf("baseline sweep proposals: %+v", got)
	}
	for _, r := range got {
		switch r.kind {
		case curation.KindMarkDuplicate:
			if r.target != second || r.status != "auto_applied" {
				t.Fatalf("later same-title org page is the duplicate: %+v", r)
			}
		case curation.KindLink:
			if r.status != "auto_applied" || (r.target != first && r.target != second) {
				t.Fatalf("link stays within the organization audience: %+v", r)
			}
		default:
			t.Fatalf("unexpected proposal: %+v", r)
		}
	}

	// A sweep merge that would pour team material into an organization page is refused.
	worker.Curator = sweeperFunc{sweep: func([]curation.SweepPage) []curation.Proposed {
		base := 1
		payload, _ := json.Marshal(curation.MergePagesPayload{Absorbed: []string{teamPage},
			Merged: curation.PagePayload{Title: "回购条款", Summary: "s", Body: "合并", BaseRevision: &base}})
		return []curation.Proposed{{Kind: curation.KindMergePages, TargetPageID: first, Payload: payload, Rationale: "合并"}}
	}}
	adminID := mcpUserID(t, env, "sweep-admin@example.com")
	cite := func(page, contribution, scope, group string) {
		if _, err := env.pool.Exec(ctx, `INSERT INTO contributions (id, workspace_id, author_id, local_id, current_version, status, scope, group_id)
			VALUES ($1, 'secops', $2, $1, 1, 'received', $3, NULLIF($4, ''))`, contribution, adminID, scope, group); err != nil {
			t.Fatal(err)
		}
		if _, err := env.pool.Exec(ctx, `INSERT INTO contribution_versions (contribution_id, version, content_hash, title, summary, markdown,
			details, links, source_refs, attachments, scope, group_id, upload_mode, client)
			VALUES ($1, 1, 'sha256:x', 't', '', 'm', '{}', '[]', '[]', '[]', $2, NULLIF($3, ''), 'manual', '{}')`, contribution, scope, group); err != nil {
			t.Fatal(err)
		}
		if _, err := env.pool.Exec(ctx, `INSERT INTO content_revision_sources (resource_id, revision, contribution_id, contribution_version, anchor)
			VALUES ($1, 1, $2, 1, '')`, page, contribution); err != nil {
			t.Fatal(err)
		}
	}
	cite(first, "ctb_sweep_org", "organization", "")
	cite(teamPage, "ctb_sweep_team", "team", team)
	if ok, err := env.srv.Curation.EnqueueSweep(ctx, "secops"); err != nil || !ok {
		t.Fatalf("queue second sweep: %v %v", ok, err)
	}
	drain()
	var merged, why string
	if err := env.pool.QueryRow(ctx, `SELECT status, rationale FROM curation_proposals WHERE kind = $1`, curation.KindMergeIntoPage).Scan(&merged, &why); err != nil ||
		merged != "rejected" || !strings.Contains(why, "wider audience") {
		t.Fatalf("team page merged into an organization page: %q %q %v", merged, why, err)
	}
}

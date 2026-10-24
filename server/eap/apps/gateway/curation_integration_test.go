//go:build integration

// Curation pipeline: an upload queues a job in the same transaction; the
// worker (baseline curator) proposes a new page as a submitted draft owned by
// the curator service member; a reviewer accepts it through the proposal API
// and it publishes; a later upload with the same title merges into it; the
// author's withdrawal flags the page automatically; a reviewer reverts the
// flag. Personal contributions are never curated, and members cannot decide.

package main_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"enterprise-agent-platform/packages/content"
	"enterprise-agent-platform/packages/contribution"
	"enterprise-agent-platform/packages/curation"
	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/wiki"
)

func TestCurationPipeline(t *testing.T) {
	env := newSliceEnv(t, 1000)
	ctx := context.Background()
	env.srv.OIDCStore = env.store
	env.srv.DLP = dlp.NewTableScanner(env.store.LoadDLPRules)
	env.srv.Content = content.New(env.pool, env.store)
	env.srv.Contributions = contribution.New(env.pool, env.store)
	env.srv.Wiki = wiki.NewStore(env.pool, env.store)
	env.srv.Curation = curation.New(env.pool, env.store, env.srv.Content)
	env.handler = env.srv.Handler()
	worker := &curation.Worker{Store: env.srv.Curation, Content: env.srv.Content, Curator: curation.Baseline{},
		Owner: "test-curator", Lease: time.Minute, Idle: time.Millisecond}

	reviewer := mcpLogin(t, env, "cur-reviewer@example.com", "secops")
	if _, err := env.pool.Exec(ctx, `UPDATE workspace_members SET roles = '{reviewer}' WHERE user_id = $1`,
		mcpUserID(t, env, "cur-reviewer@example.com")); err != nil {
		t.Fatal(err)
	}
	author := mcpLogin(t, env, "cur-author@example.com", "secops")
	ws := map[string]string{"X-Agent-Workspace": "secops"}
	upload := func(key, localID, scope, markdown string) string {
		body := `{"local_id":"` + localID + `","title":"股权回购","summary":"一句话结论","markdown":"` + markdown + `",` +
			`"details":{"type":"办案经验"},"requested_scope":"` + scope + `","redaction_declared":true,"upload_mode":"manual",` +
			`"client":{"name":"t","version":"1","schema_version":1}}`
		rec := env.do(t, http.MethodPost, "/enterprise/v1/contributions", author, body,
			map[string]string{"X-Agent-Workspace": "secops", "Idempotency-Key": key})
		var c struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &c)
		if rec.Code != http.StatusCreated {
			t.Fatalf("upload %s: %d %s", key, rec.Code, rec.Body.String())
		}
		return c.ID
	}
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
	proposals := func(session string) []map[string]any {
		rec := env.do(t, http.MethodGet, "/enterprise/v1/curation/proposals", session, "", ws)
		var out struct {
			Proposals []map[string]any `json:"proposals"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Proposals
	}

	// Personal contributions queue a job but are never curated.
	upload("cur-personal", "wiki/private", "personal", "仅本人")
	first := upload("cur-1", "wiki/buyback", "organization", "回购条款有效。")
	var queued int
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM curation_jobs WHERE status = 'queued'`).Scan(&queued); err != nil || queued != 2 {
		t.Fatalf("jobs queued with uploads: %d %v", queued, err)
	}
	drain()
	list := proposals(reviewer)
	if len(list) != 1 || list[0]["kind"] != curation.KindCreatePage || list[0]["status"] != "pending" {
		t.Fatalf("baseline create_page proposal: %v", list)
	}
	if rec := env.do(t, http.MethodGet, "/enterprise/v1/curation/proposals", author, "", ws); rec.Code != http.StatusForbidden {
		t.Fatalf("members cannot list proposals: %d", rec.Code)
	}
	created := list[0]
	pageID := created["target"].(map[string]any)["page_id"].(string)
	rec := env.do(t, http.MethodPost, "/enterprise/v1/curation/proposals/"+created["id"].(string)+"/decision", reviewer, `{"decision":"accept"}`, ws)
	if rec.Code != http.StatusOK {
		t.Fatalf("accept: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(t, http.MethodGet, "/enterprise/v1/wiki/pages/"+pageID, author, "", ws); rec.Code != http.StatusOK {
		t.Fatalf("accepted page is published: %d %s", rec.Code, rec.Body.String())
	}
	var tree struct {
		Pages []struct {
			ID     string `json:"id"`
			NodeID string `json:"node_id"`
		} `json:"pages"`
	}
	if rec := env.do(t, http.MethodGet, "/enterprise/v1/wiki/tree", author, "", ws); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &tree) != nil {
		t.Fatalf("wiki tree: %d %s", rec.Code, rec.Body.String())
	}
	if len(tree.Pages) != 1 || tree.Pages[0].ID != pageID || tree.Pages[0].NodeID != "" {
		t.Fatalf("published curator page must be visible as unplaced: %+v", tree.Pages)
	}

	// Same title again: merge into the published page plus an auto tag.
	upload("cur-2", "wiki/buyback-2", "organization", "新增裁判观点。")
	drain()
	kinds := map[string]string{}
	for _, p := range proposals(reviewer) {
		kinds[p["kind"].(string)] = p["status"].(string)
	}
	if kinds[curation.KindMergeIntoPage] != "pending" || kinds[curation.KindTag] != "auto_applied" {
		t.Fatalf("merge and tag proposals: %v", kinds)
	}

	// Withdrawal flags the page that cites the contribution and, since it was
	// the page's only published source, pauses publication; a reviewer
	// reverts both.
	if rec := env.do(t, http.MethodPost, "/enterprise/v1/contributions/"+first+"/withdraw", author, "", ws); rec.Code != http.StatusOK {
		t.Fatalf("withdraw: %d", rec.Code)
	}
	drain()
	var flagID, pauseID string
	for _, p := range proposals(reviewer) {
		switch p["kind"] {
		case curation.KindFlagSourceWithdrawn:
			flagID = p["id"].(string)
		case curation.KindRestrictVisibility:
			pauseID = p["id"].(string)
		}
	}
	if rec := env.do(t, http.MethodGet, "/enterprise/v1/wiki/pages/"+pageID, author, "", ws); pauseID == "" || rec.Code != http.StatusNotFound {
		t.Fatalf("page built only from withdrawn sources is paused: %q %d", pauseID, rec.Code)
	}
	if rec := env.do(t, http.MethodPost, "/enterprise/v1/curation/proposals/"+pauseID+"/decision", reviewer, `{"decision":"revert"}`, ws); rec.Code != http.StatusOK {
		t.Fatalf("revert pause: %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(t, http.MethodGet, "/enterprise/v1/wiki/pages/"+pageID, author, "", ws); rec.Code != http.StatusOK {
		t.Fatalf("revert republishes: %d", rec.Code)
	}
	var openFlags int
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM content_flags WHERE resource_id = $1 AND cleared_at IS NULL`, pageID).Scan(&openFlags); err != nil || flagID == "" || openFlags != 1 {
		t.Fatalf("withdrawal flag: %q %d %v", flagID, openFlags, err)
	}
	if rec := env.do(t, http.MethodPost, "/enterprise/v1/curation/proposals/"+flagID+"/decision", author, `{"decision":"revert"}`, ws); rec.Code != http.StatusNotFound {
		t.Fatalf("members cannot decide: %d", rec.Code)
	}
	if rec := env.do(t, http.MethodPost, "/enterprise/v1/curation/proposals/"+flagID+"/decision", reviewer, `{"decision":"revert"}`, ws); rec.Code != http.StatusOK {
		t.Fatalf("revert: %d %s", rec.Code, rec.Body.String())
	}
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM content_flags WHERE resource_id = $1 AND cleared_at IS NULL`, pageID).Scan(&openFlags); err != nil || openFlags != 0 {
		t.Fatalf("flag cleared by revert: %d %v", openFlags, err)
	}
	var done int
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM curation_jobs WHERE status = 'done'`).Scan(&done); err != nil || done != 4 {
		t.Fatalf("all jobs done: %d %v", done, err)
	}

	// A conclusion change goes to review; an agent can never widen visibility.
	// The pending merge is returned first: a page has one revision in review at a time.
	for _, p := range proposals(reviewer) {
		if p["kind"] == curation.KindMergeIntoPage {
			if rec := env.do(t, http.MethodPost, "/enterprise/v1/curation/proposals/"+p["id"].(string)+"/decision", reviewer, `{"decision":"reject"}`, ws); rec.Code != http.StatusOK {
				t.Fatalf("reject merge: %d %s", rec.Code, rec.Body.String())
			}
		}
	}
	worker.Curator = curatorFunc(func(_ context.Context, in curation.Input) ([]curation.Proposed, error) {
		src := []curation.Source{{ContributionID: in.Contribution.ID, Version: in.Contribution.Version}}
		base := 0
		for _, page := range in.Similar {
			if page.ID == pageID {
				base = page.Revision
			}
		}
		change, _ := json.Marshal(curation.PagePayload{Title: "股权回购", Summary: "回购条款在特定情形下无效", Body: "新结论", BaseRevision: &base})
		return []curation.Proposed{
			{Kind: curation.KindChangeConclusion, TargetPageID: pageID, Payload: change, Sources: src, Rationale: "新判例"},
			{Kind: curation.KindWidenVisibility, TargetPageID: pageID, Payload: json.RawMessage(`{}`), Sources: src, Rationale: "公开"},
		}, nil
	})
	upload("cur-3", "wiki/buyback-3", "organization", "最新判例推翻原结论。")
	drain()
	kinds = map[string]string{}
	for _, p := range proposals(reviewer) {
		kinds[p["kind"].(string)] = p["status"].(string)
	}
	if kinds[curation.KindChangeConclusion] != "pending" || kinds[curation.KindWidenVisibility] != "rejected" {
		t.Fatalf("conclusion change and widening: %v", kinds)
	}
}

type curatorFunc func(context.Context, curation.Input) ([]curation.Proposed, error)

func (f curatorFunc) Curate(ctx context.Context, in curation.Input) ([]curation.Proposed, error) {
	return f(ctx, in)
}

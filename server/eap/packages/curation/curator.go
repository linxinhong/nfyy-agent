package curation

// Curator interface, the model-free baseline curator, and the worker loop.
//
// Why: the worker owns leasing, scoping and server-side checks; a Curator
// only turns one contribution plus its similar pages into proposals. The
// model curator (packages/curatoragent, on xingding-agent-runtime) implements
// the same interface.
//
// Pitfalls:
//   - Similar pages are searched as the curator in the contribution's own
//     scope, so a curator never sees (or merges into) pages its sources may
//     not reach.
//   - Withdrawal jobs need no Curator: the server flags every page citing the
//     withdrawn contribution for re-curation, and pauses publication of a
//     page whose published sources are all withdrawn (both auto, revertible).

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/content"
	"enterprise-agent-platform/packages/identity"
)

// SimilarPage is a readable page that may already cover the contribution.
type SimilarPage struct {
	ID       string
	Path     string
	Title    string
	Summary  string
	Revision int
	Body     string
}

// Input is what a Curator sees for one job.
type Input struct {
	Contribution Contribution
	Similar      []SimilarPage
}

// Curator proposes changes; it never writes.
type Curator interface {
	Curate(ctx context.Context, in Input) ([]Proposed, error)
}

// Sweeper is a Curator that can also re-curate a whole Workspace (§5.5).
// Curators without it get the baseline sweep.
type Sweeper interface {
	Sweep(ctx context.Context, pages []SweepPage) ([]Proposed, error)
}

// SweepLimit bounds how many pages one sweep looks at.
const SweepLimit = 200

const maxSweepLinks = 20

// BaselineSweep marks later pages with the same title as duplicates of the
// earliest one and links pages sharing at least two tags. Pages are compared
// only within the same audience, so a mark or link never pairs a team page
// with another team's.
func BaselineSweep(pages []SweepPage) []Proposed {
	audience := func(p SweepPage) string { return p.Visibility + "/" + p.GroupID }
	var out []Proposed
	first := map[string]SweepPage{}
	// Oldest first, so the earliest page with a title is the one kept.
	for i := len(pages) - 1; i >= 0; i-- {
		page := pages[i]
		key := audience(page) + "/" + strings.ToLower(strings.TrimSpace(page.Title))
		kept, seen := first[key]
		if !seen {
			first[key] = page
			continue
		}
		if page.Duplicate {
			continue
		}
		payload, _ := json.Marshal(FlagPayload{Reason: "same title as page " + kept.ID})
		out = append(out, Proposed{Kind: KindMarkDuplicate, TargetPageID: page.ID, Payload: payload,
			Rationale: "全局重整：与页面 " + kept.Path + " 标题相同，可能重复，待合并或清理"})
	}
	links := 0
	for i := 0; i < len(pages) && links < maxSweepLinks; i++ {
		for k := i + 1; k < len(pages) && links < maxSweepLinks; k++ {
			a, b := pages[i], pages[k]
			if audience(a) != audience(b) || slices.Contains(a.Related, b.ID) || slices.Contains(b.Related, a.ID) {
				continue
			}
			shared := 0
			for _, tag := range a.Tags {
				if slices.Contains(b.Tags, tag) {
					shared++
				}
			}
			if shared < 2 {
				continue
			}
			payload, _ := json.Marshal(LinkPayload{RelatedPageID: b.ID})
			out = append(out, Proposed{Kind: KindLink, TargetPageID: a.ID, Payload: payload,
				Rationale: fmt.Sprintf("全局重整：与页面 %s 共有 %d 个标签，补充关联", b.Title, shared)})
			links++
		}
	}
	return out
}

// Baseline is the model-free curator: merge into a page with the same title,
// otherwise propose a new unplaced page; tag with the contribution's type.
type Baseline struct{}

func (Baseline) Curate(_ context.Context, in Input) ([]Proposed, error) {
	c := in.Contribution
	source := []Source{{ContributionID: c.ID, Version: c.Version}}
	var tags []string
	for _, key := range []string{"type", "area", "业务领域", "类型"} {
		if v, ok := c.Details[key].(string); ok && strings.TrimSpace(v) != "" && len([]rune(v)) <= 40 {
			tags = append(tags, strings.TrimSpace(v))
		}
	}
	for _, page := range in.Similar {
		if !strings.EqualFold(strings.TrimSpace(page.Title), strings.TrimSpace(c.Title)) {
			continue
		}
		base := page.Revision
		payload, _ := json.Marshal(PagePayload{
			Title: page.Title, Summary: page.Summary, BaseRevision: &base,
			Body: strings.TrimRight(page.Body, "\n") + "\n\n## 补充：" + c.Title + "\n\n" + c.Markdown,
		})
		out := []Proposed{{Kind: KindMergeIntoPage, TargetPageID: page.ID, Payload: payload, Sources: source,
			Rationale: "同名页面已存在，将投稿内容作为补充段落合并，待审核"}}
		if len(tags) > 0 {
			tagPayload, _ := json.Marshal(TagPayload{Tags: tags})
			out = append(out, Proposed{Kind: KindTag, TargetPageID: page.ID, Payload: tagPayload, Sources: source,
				Rationale: "按投稿的类型/领域字段补充标签"})
		}
		return out, nil
	}
	payload, _ := json.Marshal(PagePayload{Title: c.Title, Summary: c.Summary, Body: c.Markdown, Tags: tags})
	return []Proposed{{Kind: KindCreatePage, Payload: payload, Sources: source,
		Rationale: "没有覆盖该主题的页面，按投稿新建草稿页（未归档），待审核与归类"}}, nil
}

// Worker leases jobs and runs them.
type Worker struct {
	Store   *Store
	Content *content.Store
	Curator Curator
	Owner   string
	Lease   time.Duration
	Idle    time.Duration
}

func (w *Worker) event(j Job, curatorID string) audit.Event {
	ev := audit.NewEvent("curation", audit.OutcomeAllowed)
	ev.Actor = &identity.Principal{UserID: curatorID}
	ev.WorkspaceID = j.WorkspaceID
	return ev
}

// Run processes jobs until ctx ends.
func (w *Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		worked, err := w.RunOnce(ctx)
		if err != nil {
			log.Printf("curator: %v", err)
		}
		if !worked {
			select {
			case <-ctx.Done():
			case <-time.After(w.Idle):
			}
		}
	}
}

// RunOnce leases and processes at most one job; worked reports whether a job
// was leased. A failed job is re-queued with backoff and its error returned.
func (w *Worker) RunOnce(ctx context.Context) (worked bool, err error) {
	j, ok, err := w.Store.Lease(ctx, w.Owner, w.Lease)
	if err != nil || !ok {
		return false, err
	}
	if perr := w.process(ctx, j); perr != nil {
		if ferr := w.Store.Fail(context.WithoutCancel(ctx), j, w.Owner, perr); ferr != nil {
			return true, fmt.Errorf("job %d: %v (requeue failed: %v)", j.ID, perr, ferr)
		}
		return true, fmt.Errorf("job %d attempt %d: %w", j.ID, j.Attempts, perr)
	}
	return true, w.Store.Complete(ctx, j, w.Owner)
}

func (w *Worker) process(ctx context.Context, j Job) error {
	curatorID, err := w.Store.EnsureCurator(ctx, j.WorkspaceID)
	if err != nil {
		return err
	}
	if j.Kind == "sweep" {
		return w.sweep(ctx, j, curatorID, w.event(j, curatorID))
	}
	c, err := w.Store.LoadContribution(ctx, j)
	if err != nil {
		return err
	}
	ev := w.event(j, curatorID)
	source := []Source{{ContributionID: c.ID, Version: c.Version}}
	if j.Kind == "withdrawal" {
		pages, err := w.Store.PagesCiting(ctx, j.WorkspaceID, c.ID)
		if err != nil {
			return err
		}
		var proposals []Proposed
		for _, id := range pages {
			payload, _ := json.Marshal(FlagPayload{Reason: "cited contribution " + c.ID + " was withdrawn by its author"})
			proposals = append(proposals, Proposed{Kind: KindFlagSourceWithdrawn, TargetPageID: id, Payload: payload,
				Sources: source, Rationale: "来源投稿已被作者撤回，页面需要重新整理"})
		}
		orphaned, err := w.Store.PagesOnlyWithdrawn(ctx, j.WorkspaceID, pages)
		if err != nil {
			return err
		}
		for _, id := range orphaned {
			proposals = append(proposals, Proposed{Kind: KindRestrictVisibility, TargetPageID: id, Payload: json.RawMessage(`{}`),
				Sources: source, Rationale: "页面已发布版本的全部来源均已撤回，暂停公开，待重新整理或恢复"})
		}
		return w.Store.Submit(ctx, j, curatorID, proposals, ev)
	}
	if c.Status == "withdrawn" || c.Scope == "personal" {
		return nil // nothing to share
	}
	actor := content.Actor{UserID: curatorID, GroupID: c.GroupID}
	query := c.Title
	if len([]rune(query)) > 60 {
		query = string([]rune(query)[:60])
	}
	hits, err := w.Content.Search(ctx, j.WorkspaceID, actor, query, 5)
	if err != nil && err != content.ErrInvalid {
		return fmt.Errorf("similar pages: %w", err)
	}
	var similar []SimilarPage
	for _, hit := range hits {
		page, err := w.Content.Get(ctx, j.WorkspaceID, actor, hit.PageID)
		if err != nil {
			continue
		}
		similar = append(similar, SimilarPage{ID: page.ID, Path: page.Path, Title: page.Title, Summary: page.Summary,
			Revision: page.Revision, Body: page.Body})
	}
	proposals, err := w.Curator.Curate(ctx, Input{Contribution: c, Similar: similar})
	if err != nil {
		return fmt.Errorf("curate: %w", err)
	}
	return w.Store.Submit(ctx, j, curatorID, proposals, ev)
}

// sweep re-curates a whole Workspace with the Curator's Sweeper, or the
// baseline sweep when it has none.
func (w *Worker) sweep(ctx context.Context, j Job, curatorID string, ev audit.Event) error {
	pages, err := w.Store.SweepPages(ctx, j.WorkspaceID, SweepLimit)
	if err != nil {
		return err
	}
	if len(pages) < 2 {
		return nil
	}
	var proposals []Proposed
	if sweeper, ok := w.Curator.(Sweeper); ok {
		if proposals, err = sweeper.Sweep(ctx, pages); err != nil {
			return fmt.Errorf("sweep: %w", err)
		}
	} else {
		proposals = BaselineSweep(pages)
	}
	return w.Store.Submit(ctx, j, curatorID, proposals, ev)
}

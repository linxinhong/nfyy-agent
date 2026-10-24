// Package curation turns inbox contributions into knowledge-layer changes:
// durable jobs for the curation worker, structured proposals, server-side
// application of auto proposals, and review proposals that become submitted
// draft revisions published only through the content review queue.
//
// Why: the curation agent (LLM or not) only proposes; the server decides what
// a proposal may touch. Risk is a property of the kind, never of the agent's
// claim, and the visibility of curated content is computed from its sources.
//
// Invariants and pitfalls:
//   - Auto kinds never change published conclusions or widen visibility and
//     store enough in `revert` to undo; everything else is a review kind.
//   - Curated visibility is the intersection of the source contributions'
//     scopes: personal sources are never curated into shared content, team
//     sources from two teams cannot meet in one page, and a team source never
//     flows into an organization page.
//   - split_page and merge_pages are composites: Submit expands them into
//     kinds the server applies (a page revision, new pages, duplicate marks),
//     each its own proposal row, so every piece is reviewed or reverted on
//     its own. widen_visibility is never accepted from an agent.
//   - Review proposals are materialized before their row is written; a crash
//     in between can leave an orphan draft that a retry duplicates (rare, and
//     visible in the review queue). Proposal ids are deterministic per job.
package curation

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

// Proposal kinds (contract surface, api/mindlink-openapi.yaml CurationProposal).
const (
	KindTag                 = "tag"
	KindLink                = "link"
	KindClassifyDraft       = "classify_draft"
	KindMarkDuplicate       = "mark_duplicate"
	KindFlagSensitive       = "flag_sensitive"
	KindFlagConflict        = "flag_conflict"
	KindFlagSourceWithdrawn = "flag_source_withdrawn"
	KindRestrictVisibility  = "restrict_visibility"
	KindCreatePage          = "create_page"
	KindMergeIntoPage       = "merge_into_page"
	KindRewriteSection      = "rewrite_section"
	KindSplitPage           = "split_page"
	KindMergePages          = "merge_pages"
	KindWidenVisibility     = "widen_visibility"
	KindChangeConclusion    = "change_published_conclusion"
)

// autoKinds apply at once (audited, revertible); every other kind waits for a human.
var autoKinds = map[string]bool{
	KindTag: true, KindLink: true, KindClassifyDraft: true, KindMarkDuplicate: true,
	KindFlagSensitive: true, KindFlagConflict: true, KindFlagSourceWithdrawn: true, KindRestrictVisibility: true,
}

// supported kinds the server applies directly; composites are expanded first
// and widen_visibility is refused (agents may only tighten visibility).
var supported = map[string]bool{
	KindTag: true, KindLink: true, KindClassifyDraft: true, KindMarkDuplicate: true, KindFlagSensitive: true,
	KindFlagConflict: true, KindFlagSourceWithdrawn: true, KindRestrictVisibility: true,
	KindCreatePage: true, KindMergeIntoPage: true, KindRewriteSection: true, KindChangeConclusion: true,
}

var flagKinds = map[string]string{
	KindMarkDuplicate: "duplicate", KindFlagSensitive: "sensitive",
	KindFlagConflict: "conflict", KindFlagSourceWithdrawn: "source_withdrawn",
}

// Risk returns "auto" or "review" for a kind.
func Risk(kind string) string {
	if autoKinds[kind] {
		return "auto"
	}
	return "review"
}

// Source cites one contribution version.
type Source struct {
	ContributionID string `json:"contribution_id"`
	Version        int    `json:"version"`
	Anchor         string `json:"anchor,omitempty"`
}

// Proposed is what a curator returns; the server fills risk and status.
type Proposed struct {
	Kind         string          `json:"kind"`
	TargetPageID string          `json:"target_page_id,omitempty"`
	Payload      json.RawMessage `json:"payload"`
	Sources      []Source        `json:"sources"`
	Rationale    string          `json:"rationale"`
}

// PagePayload is the body of create_page, merge_into_page and rewrite_section.
type PagePayload struct {
	Title        string   `json:"title"`
	Summary      string   `json:"summary"`
	Body         string   `json:"body"`
	NodeID       string   `json:"node_id,omitempty"`
	Slug         string   `json:"slug,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	BaseRevision *int     `json:"base_revision,omitempty"`
}

// TagPayload adds tags to a page.
type TagPayload struct {
	Tags []string `json:"tags"`
}

// LinkPayload relates the target page to another page.
type LinkPayload struct {
	RelatedPageID string `json:"related_page_id"`
}

// ClassifyPayload places an unpublished page under a chapter.
type ClassifyPayload struct {
	NodeID string `json:"node_id"`
	Slug   string `json:"slug"`
}

// SplitPayload is the body of split_page: the target keeps Remaining and
// every part becomes a new page.
type SplitPayload struct {
	Remaining PagePayload   `json:"remaining"`
	Parts     []PagePayload `json:"parts"`
}

// MergePagesPayload is the body of merge_pages: the target receives Merged
// and every page in Absorbed is marked a duplicate of the target.
type MergePagesPayload struct {
	Merged   PagePayload `json:"merged"`
	Absorbed []string    `json:"absorbed_page_ids"`
}

const maxCompositeParts = 10

// expand turns a composite proposal into the proposals the server applies;
// other kinds pass through. A malformed composite returns an error and is
// recorded as rejected.
func expand(p Proposed) ([]Proposed, error) {
	piece := func(kind, target string, payload any, note string) Proposed {
		raw, _ := json.Marshal(payload)
		return Proposed{Kind: kind, TargetPageID: target, Payload: raw, Sources: p.Sources,
			Rationale: strings.TrimSpace(p.Rationale) + " [" + note + "]"}
	}
	switch p.Kind {
	case KindSplitPage:
		var sp SplitPayload
		if json.Unmarshal(p.Payload, &sp) != nil || p.TargetPageID == "" || len(sp.Parts) == 0 || len(sp.Parts) > maxCompositeParts {
			return nil, errors.New("split_page needs target_page_id, remaining and 1-10 parts")
		}
		out := []Proposed{piece(KindRewriteSection, p.TargetPageID, sp.Remaining, "拆分：原页面保留部分")}
		for _, part := range sp.Parts {
			part.BaseRevision = nil
			out = append(out, piece(KindCreatePage, "", part, "拆分：自页面 "+p.TargetPageID+" 拆出"))
		}
		return out, nil
	case KindMergePages:
		var mp MergePagesPayload
		if json.Unmarshal(p.Payload, &mp) != nil || p.TargetPageID == "" || len(mp.Absorbed) == 0 || len(mp.Absorbed) > maxCompositeParts {
			return nil, errors.New("merge_pages needs target_page_id, merged and 1-10 absorbed_page_ids")
		}
		out := []Proposed{piece(KindMergeIntoPage, p.TargetPageID, mp.Merged, "合并多个页面：合并后的正文")}
		for _, absorbed := range mp.Absorbed {
			if absorbed == p.TargetPageID {
				return nil, errors.New("a page cannot absorb itself")
			}
			out = append(out, piece(KindMarkDuplicate, absorbed, FlagPayload{Reason: "merged into page " + p.TargetPageID},
				"合并多个页面：已并入目标页面"))
		}
		return out, nil
	default:
		return []Proposed{p}, nil
	}
}

// FlagPayload explains a flag.
type FlagPayload struct {
	Reason string `json:"reason"`
}

func validRationale(r string) bool {
	n := utf8.RuneCountInString(strings.TrimSpace(r))
	return n >= 1 && n <= 2000
}

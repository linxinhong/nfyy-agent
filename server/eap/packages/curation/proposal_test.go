package curation

import (
	"encoding/json"
	"testing"
)

func TestExpandComposites(t *testing.T) {
	src := []Source{{ContributionID: "c1", Version: 1}}
	base := 4
	split, _ := json.Marshal(SplitPayload{
		Remaining: PagePayload{Title: "总则", Body: "保留", BaseRevision: &base},
		Parts:     []PagePayload{{Title: "一", Body: "a"}, {Title: "二", Body: "b", BaseRevision: &base}},
	})
	out, err := expand(Proposed{Kind: KindSplitPage, TargetPageID: "p1", Payload: split, Sources: src, Rationale: "太长"})
	if err != nil || len(out) != 3 || out[0].Kind != KindRewriteSection || out[0].TargetPageID != "p1" ||
		out[1].Kind != KindCreatePage || out[2].TargetPageID != "" {
		t.Fatalf("split: %+v %v", out, err)
	}
	var part PagePayload
	if err := json.Unmarshal(out[2].Payload, &part); err != nil || part.BaseRevision != nil || len(out[2].Sources) != 1 {
		t.Fatalf("new parts carry no base revision but keep sources: %s %v", out[2].Payload, err)
	}

	merge, _ := json.Marshal(MergePagesPayload{Merged: PagePayload{Title: "合并", Body: "全文"}, Absorbed: []string{"p2", "p3"}})
	out, err = expand(Proposed{Kind: KindMergePages, TargetPageID: "p1", Payload: merge, Sources: src, Rationale: "重复"})
	if err != nil || len(out) != 3 || out[0].Kind != KindMergeIntoPage || out[1].Kind != KindMarkDuplicate || out[2].TargetPageID != "p3" {
		t.Fatalf("merge: %+v %v", out, err)
	}
	if Risk(out[1].Kind) != "auto" || Risk(out[0].Kind) != "review" {
		t.Fatal("the merged text is reviewed; duplicate marks apply at once and can be reverted")
	}

	self, _ := json.Marshal(MergePagesPayload{Merged: PagePayload{Title: "x"}, Absorbed: []string{"p1"}})
	for _, bad := range []Proposed{
		{Kind: KindSplitPage, TargetPageID: "p1", Payload: json.RawMessage(`{"parts":[]}`)},
		{Kind: KindMergePages, TargetPageID: "p1", Payload: self},
		{Kind: KindMergePages, Payload: merge},
	} {
		if _, err := expand(bad); err == nil {
			t.Fatalf("malformed composite accepted: %s", bad.Payload)
		}
	}
	if out, err := expand(Proposed{Kind: KindTag}); err != nil || len(out) != 1 {
		t.Fatal("plain kinds pass through")
	}
	if Risk(KindChangeConclusion) != "review" || Risk(KindWidenVisibility) != "review" {
		t.Fatal("conclusion changes and widening are never automatic")
	}
}

func TestBaselineSweep(t *testing.T) {
	// Newest first, as SweepPages returns them.
	pages := []SweepPage{
		{ID: "p4", Title: "回购条款", Visibility: "team", GroupID: "g2"},
		{ID: "p3", Title: "回购条款 ", Visibility: "organization", Tags: []string{"公司", "回购"}},
		{ID: "p2", Title: "对赌", Visibility: "organization", Tags: []string{"回购", "公司", "对赌"}},
		{ID: "p1", Title: "回购条款", Visibility: "organization"},
	}
	out := BaselineSweep(pages)
	var dup, link []Proposed
	for _, p := range out {
		switch p.Kind {
		case KindMarkDuplicate:
			dup = append(dup, p)
		case KindLink:
			link = append(link, p)
		}
	}
	if len(dup) != 1 || dup[0].TargetPageID != "p3" {
		t.Fatalf("the later same-title page in the same audience is the duplicate: %+v", dup)
	}
	if len(link) != 1 || link[0].TargetPageID != "p3" {
		t.Fatalf("pages sharing two tags are linked: %+v", link)
	}
	pages[1].Duplicate, pages[1].Related = true, []string{"p2"}
	if again := BaselineSweep(pages); len(again) != 0 {
		t.Fatalf("an open flag or an existing relation is not proposed again: %+v", again)
	}
}

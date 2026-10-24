package curatoragent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"

	"enterprise-agent-platform/packages/curation"
)

// scripted replays tool-call rounds, then answers with text.
type scripted struct {
	mu     sync.Mutex
	rounds [][]models.ToolCall
	calls  int
	fail   error
}

func (s *scripted) Response(context.Context, []models.Message, models.ModelOptions) (*models.ModelResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return nil, s.fail
	}
	defer func() { s.calls++ }()
	if s.calls < len(s.rounds) {
		return &models.ModelResponse{ToolCalls: s.rounds[s.calls], FinishReason: models.FinishReasonToolCalls}, nil
	}
	return &models.ModelResponse{Content: "done", FinishReason: models.FinishReasonStop}, nil
}

func (s *scripted) ResponseStream(ctx context.Context, m []models.Message, o models.ModelOptions) (<-chan models.StreamEvent, error) {
	return nil, errors.New("not streamed")
}
func (s *scripted) ID() string       { return "scripted" }
func (s *scripted) Provider() string { return "test" }

func call(id, name string, args map[string]any) models.ToolCall {
	raw, _ := json.Marshal(args)
	return models.ToolCall{ID: id, Type: "function", Function: models.FunctionCall{Name: name, Arguments: string(raw)}}
}

func TestCuratorProposesThroughTools(t *testing.T) {
	model := &scripted{rounds: [][]models.ToolCall{
		{call("1", "read_page", map[string]any{"page_id": "p1"})},
		{
			call("2", "propose", map[string]any{"kind": "merge_into_page", "target_page_id": "p1", "title": "回购",
				"summary": "有效", "body": "合并后正文", "rationale": "同一主题"}),
			call("3", "propose", map[string]any{"kind": "tag", "target_page_id": "p1", "tags": []string{"公司"}, "rationale": "领域"}),
			// Rejected by the tools: foreign page, unavailable kind.
			call("4", "propose", map[string]any{"kind": "tag", "target_page_id": "elsewhere", "tags": []string{"x"}, "rationale": "r"}),
			call("5", "propose", map[string]any{"kind": "widen_visibility", "target_page_id": "p1", "rationale": "r"}),
		},
	}}
	in := curation.Input{
		Contribution: curation.Contribution{ID: "c1", Version: 3, Title: "回购", Markdown: "正文"},
		Similar:      []curation.SimilarPage{{ID: "p1", Path: "a/b/c/d", Title: "回购", Revision: 7, Body: "旧"}},
	}
	out, err := WithModel(model).Curate(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Kind != curation.KindMergeIntoPage || out[1].Kind != curation.KindTag {
		t.Fatalf("proposals: %+v", out)
	}
	var page curation.PagePayload
	if err := json.Unmarshal(out[0].Payload, &page); err != nil || page.BaseRevision == nil || *page.BaseRevision != 7 {
		t.Fatalf("merge pins the candidate revision: %s %v", out[0].Payload, err)
	}
	for _, p := range out {
		if len(p.Sources) != 1 || p.Sources[0].ContributionID != "c1" || p.Sources[0].Version != 3 {
			t.Fatalf("sources are always this contribution version: %+v", p.Sources)
		}
	}
}

func TestCuratorModelFailureIsAnError(t *testing.T) {
	in := curation.Input{Contribution: curation.Contribution{ID: "c1", Version: 1, Title: "t", Markdown: "m"}}
	if _, err := WithModel(&scripted{fail: errors.New("down")}).Curate(context.Background(), in); err == nil {
		t.Fatal("a model failure must fail the job so it retries")
	}
}

func TestSweepProposesOnlyOverExistingPages(t *testing.T) {
	model := &scripted{rounds: [][]models.ToolCall{{
		call("1", "propose", map[string]any{"kind": "merge_pages", "target_page_id": "p1", "absorbed_page_ids": []string{"p2"},
			"title": "回购", "summary": "有效", "body": "合并", "rationale": "同一主题"}),
		call("2", "propose", map[string]any{"kind": "create_page", "title": "新", "body": "x", "rationale": "r"}),
		call("3", "propose", map[string]any{"kind": "merge_pages", "target_page_id": "p1", "absorbed_page_ids": []string{"elsewhere"},
			"title": "x", "body": "x", "rationale": "r"}),
	}}}
	out, err := WithModel(model).Sweep(context.Background(), []curation.SweepPage{
		{ID: "p1", Title: "回购", Revision: 2}, {ID: "p2", Title: "回购条款", Revision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Kind != curation.KindMergePages || len(out[0].Sources) != 0 {
		t.Fatalf("one merge, sources left to the server: %+v", out)
	}
	var merge curation.MergePagesPayload
	if err := json.Unmarshal(out[0].Payload, &merge); err != nil || merge.Merged.BaseRevision == nil || *merge.Merged.BaseRevision != 2 {
		t.Fatalf("merge payload: %s %v", out[0].Payload, err)
	}
}

// recorder captures the prompt and the tools the model was offered.
type recorder struct {
	scripted
	input string
	tools []string
}

func (r *recorder) Response(ctx context.Context, m []models.Message, o models.ModelOptions) (*models.ModelResponse, error) {
	if r.input == "" {
		for _, msg := range m {
			if msg.Role == "user" {
				r.input = msg.Content
			}
		}
		for _, tool := range o.Tools {
			r.tools = append(r.tools, tool.Function.Name)
		}
	}
	return r.scripted.Response(ctx, m, o)
}

func TestLongContributionIsReadBySection(t *testing.T) {
	body := "## 一、事实\n" + strings.Repeat("原告与被告签订对赌协议。", 1200) + "\n\n## 二、理由\n回购条款有效的关键理由。\n"
	model := &recorder{scripted: scripted{rounds: [][]models.ToolCall{
		{call("1", "read_contribution", map[string]any{"section": 99})},
		{call("2", "read_contribution", map[string]any{"section": 1})},
		{call("3", "propose", map[string]any{"kind": "create_page", "title": "回购", "body": "要点", "rationale": "新主题"})},
	}}}
	in := curation.Input{Contribution: curation.Contribution{ID: "c1", Version: 1, Title: "回购", Markdown: body}}
	out, err := WithModel(model).Curate(context.Background(), in)
	if err != nil || len(out) != 1 {
		t.Fatalf("proposals: %+v %v", out, err)
	}
	if strings.Contains(model.input, "关键理由") || !strings.Contains(model.input, "] 二、理由（") {
		t.Fatalf("a long body is outlined, not inlined: %.200s", model.input)
	}
	if !slices.Contains(model.tools, "read_contribution") {
		t.Fatalf("tools: %v", model.tools)
	}
}

func TestShortContributionIsInlined(t *testing.T) {
	model := &recorder{}
	in := curation.Input{Contribution: curation.Contribution{ID: "c1", Version: 1, Title: "t", Markdown: "回购条款有效。"}}
	if _, err := WithModel(model).Curate(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(model.input, "回购条款有效。") || slices.Contains(model.tools, "read_contribution") {
		t.Fatalf("input %q tools %v", model.input, model.tools)
	}
}

func TestLongPageIsReadBySection(t *testing.T) {
	j := &job{pages: map[string]curation.SimilarPage{"p1": {ID: "p1", Title: "回购",
		Body: "## 甲\n" + strings.Repeat("长", 13_000) + "\n\n## 乙\n结论段。\n"}}}
	first, err := j.read(readArgs{PageID: "p1"})
	if err != nil || !strings.Contains(first, "分段目录") || strings.Contains(first, "结论段") {
		t.Fatalf("first read is the outline: %.300s %v", first, err)
	}
	last := strings.Count(first, "\n  [") - 1
	text, err := j.read(readArgs{PageID: "p1", Section: &last})
	if err != nil || !strings.Contains(text, "结论段") {
		t.Fatalf("section read: %.200s %v", text, err)
	}
}

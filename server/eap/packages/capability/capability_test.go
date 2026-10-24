package capability

import (
	"context"
	"testing"
)

func TestTokenizeAndScore(t *testing.T) {
	toks := TokenizeName("mcp:gitlab git_readFile GIT-TAGS")
	joined := toksString(toks)
	for _, want := range []string{"mcp", "gitlab", "git", "read", "file", "git", "tags"} {
		if !contains(joined, want) {
			t.Fatalf("tokens %v missing %q", toks, want)
		}
	}
	c := Capability{Name: "mcp:gitlab:git_read_file", Description: "Read a file from an authorized repository", Type: "mcp_tool"}
	if ScoreCapability(c, Tokenize("read git file")) < 10 {
		t.Fatalf("expected strong score, got %d", ScoreCapability(c, Tokenize("read git file")))
	}
	if ScoreCapability(c, Tokenize("calendar")) != 0 {
		t.Fatalf("irrelevant query must score 0")
	}
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}

func toksString(toks []string) string {
	out := ""
	for _, t := range toks {
		out += t + " "
	}
	return out
}

// Empty query lists everything visible to the caller (ADR-0023 refinement):
// sources already applied workspace/policy filters, so "查一下有哪些能力"
// with no keywords returns the full permission-filtered catalog.
func TestRegistryEmptyQueryListsAllVisible(t *testing.T) {
	r := &Registry{Sources: []Source{&listStubSource{caps: []Capability{
		{Name: "mcp:gitlab:git_read_file", Description: "read", Status: StatusAvailable},
		{Name: "mcp:gitlab:git_push", Description: "push", Status: StatusNeedsApproval},
	}}}}
	caps, err := r.Search(context.Background(), Subject{WorkspaceID: "finance"}, SearchQuery{Text: "  "})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(caps) != 2 || caps[0].Name != "mcp:gitlab:git_push" || caps[1].Name != "mcp:gitlab:git_read_file" {
		t.Fatalf("empty query must list all visible, ordered by name: %+v", caps)
	}
}

// listStubSource is a Source whose Search returns a fixed visible set —
// workspace/policy filtering is the source's job, the registry only merges.
type listStubSource struct{ caps []Capability }

func (s *listStubSource) Kind() string { return string(KindMCP) }
func (s *listStubSource) Search(_ context.Context, _ Subject, _ SearchQuery) ([]Capability, error) {
	return s.caps, nil
}
func (s *listStubSource) Parse(_ context.Context, _ Subject, _ string) (*Parsed, error) {
	return nil, nil
}
func (s *listStubSource) Execute(_ context.Context, _ Subject, _ *Parsed, _ ExecuteInput) (ExecuteResult, error) {
	return ExecuteResult{}, nil
}

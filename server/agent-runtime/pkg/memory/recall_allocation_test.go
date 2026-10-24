package memory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Keep the previous scorer as an independent equivalence oracle for Unicode
// boundaries, case folding, repeated words, and matches spanning topics.
func referenceRelevance(normalized string, query map[string]struct{}, r Record) int {
	if len(query) == 0 {
		return 0
	}
	haystack := strings.ToLower(r.Content + " " + strings.Join(r.Topics, " "))
	seen := tokenSet(haystack)
	score := 0
	if normalized != "" && strings.Contains(haystack, normalized) {
		score += 100
	}
	for token := range query {
		if _, ok := seen[token]; ok {
			score += 10
		}
	}
	return score
}

func FuzzRecallRelevanceEquivalent(f *testing.F) {
	for _, input := range [][3]string{
		{"coffee coffee TEA", "coffee tea", "coffee"},
		{"你好 café CAFÉ e\u0301 İ １２٣", "你好 café e\u0301", "İ"},
		{"x\xffy", "x y", "\xff"},
		{"abc123---abc", "abc123 abc", "topic"},
		{"", "!!!", ""}, {"hello", "hello world", "world"},
	} {
		f.Add(input[0], input[1], input[2])
	}
	f.Fuzz(func(t *testing.T, content, query, topic string) {
		if len(content)+len(query)+len(topic) > 8192 {
			t.Skip()
		}
		r := Record{Content: content, Topics: []string{topic, "coffee"}}
		tokens := tokenSet(query)
		normalized := strings.ToLower(strings.TrimSpace(query))
		if got, want := relevance(normalized, tokens, r), referenceRelevance(normalized, tokens, r); got != want {
			t.Fatalf("score=%d want=%d content=%q query=%q topic=%q", got, want, content, query, topic)
		}
	})
}

func TestRecallCandidateJSONBudgetExact(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 123456789, time.UTC)
	scope := Scope{Namespace: "n", ProjectID: "p", SubjectID: "s", CollectionID: "c"}
	for _, content := range []string{"plain", "<>&\"\n\u2028\u2029中文", "invalid\xffutf8"} {
		t.Run(content, func(t *testing.T) {
			r := Record{Scope: scope, MemoryID: "m", Revision: 1, SubjectGeneration: 1, Kind: KindPreference,
				Content: content, Topics: []string{"<topic>", ""}, Status: StatusActive, ExpiresAt: now.Add(time.Hour)}
			records := []Record{r, r}
			encoded, err := json.Marshal(records)
			if err != nil {
				t.Fatal(err)
			}
			for _, delta := range []int{-1, 0, 1} {
				limits := DefaultLimits()
				limits.MaxCandidateBytes = len(encoded) + delta
				store := &recallStore{result: ListResult{StoreNow: now, Generation: 1, Records: records}}
				_, err := newRecallModule(t, store, limits, nil).Recall(context.Background(), Principal{Subject: "s", Namespace: "n", Issuer: "host"}, scope, RecallRequest{})
				if delta < 0 && !errors.Is(err, ErrUnavailable) || delta >= 0 && err != nil {
					t.Fatalf("delta=%d err=%v", delta, err)
				}
			}
		})
	}
	r := Record{Scope: scope, MemoryID: "bad-time", CreatedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}
	store := &recallStore{result: ListResult{StoreNow: now, Generation: 1, Records: []Record{r}}}
	if _, err := newRecallModule(t, store, DefaultLimits(), nil).Recall(context.Background(), Principal{Subject: "s", Namespace: "n", Issuer: "host"}, scope, RecallRequest{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("invalid time err=%v", err)
	}
}

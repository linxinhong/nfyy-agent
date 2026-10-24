package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// BenchmarkMemoryRecallCandidateWindow isolates Module work from database IO.
// It keeps the full 200-candidate window and normal 10-item output.
func BenchmarkMemoryRecallCandidateWindow(b *testing.B) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	scope := Scope{Namespace: "tenant", ProjectID: "project", SubjectID: "subject", CollectionID: "profile"}
	store := &recallStore{result: ListResult{StoreNow: now, Generation: 1}}
	for i := 0; i < 200; i++ {
		store.result.Records = append(store.result.Records, Record{
			Scope: scope, MemoryID: fmt.Sprintf("record-%03d", i), Revision: 1, SubjectGeneration: 1,
			Kind: KindPreference, Content: strings.Repeat("coffee tea preference ", 24), Topics: []string{"drink", "preference"},
			Status: StatusActive, CreatedAt: now, UpdatedAt: now.Add(time.Duration(i) * time.Second), ExpiresAt: now.Add(time.Hour),
		})
	}
	m, err := NewModule(ModuleConfig{Store: store, Authorizer: AuthorizerFunc(func(context.Context, AuthorizeRequest) error { return nil }),
		Keys: map[string][]byte{"test": []byte("0123456789abcdef0123456789abcdef")}, CurrentKey: "test", Limits: DefaultLimits()})
	if err != nil {
		b.Fatal(err)
	}
	p := Principal{Namespace: scope.Namespace, Subject: scope.SubjectID, Issuer: "host"}
	for _, query := range []string{"", "coffee"} {
		b.Run(fmt.Sprintf("query=%q", query), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				result, err := m.Recall(context.Background(), p, scope, RecallRequest{Query: query, Limit: 10})
				if err != nil || len(result.Items) != 10 {
					b.Fatalf("Recall items=%d: %v", len(result.Items), err)
				}
			}
		})
	}
}

// Paired component benchmarks distinguish intrinsic work from whole-request
// timing noise; both implementations process identical records in one binary.
func BenchmarkMemoryRecallAllocationComponents(b *testing.B) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	r := Record{Scope: Scope{Namespace: "tenant", ProjectID: "project", SubjectID: "subject", CollectionID: "profile"},
		MemoryID: "record", Revision: 1, SubjectGeneration: 1, Kind: KindPreference,
		Content: strings.Repeat("coffee tea preference ", 24), Topics: []string{"drink", "preference"},
		Status: StatusActive, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
	for round := 0; round < 3; round++ {
		b.Run(fmt.Sprintf("round-%d", round), func(b *testing.B) {
			b.Run("marshal-record-value", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if _, err := json.Marshal(r); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("encode-record-pointer", func(b *testing.B) {
				var buf bytes.Buffer
				enc := json.NewEncoder(&buf)
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					buf.Reset()
					if err := enc.Encode(&r); err != nil {
						b.Fatal(err)
					}
				}
			})
			tokens := tokenSet("coffee")
			b.Run("old-relevance", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if referenceRelevance("coffee", tokens, r) != 110 {
						b.Fatal("score")
					}
				}
			})
			b.Run("new-relevance", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if relevance("coffee", tokens, r) != 110 {
						b.Fatal("score")
					}
				}
			})
		})
	}
}

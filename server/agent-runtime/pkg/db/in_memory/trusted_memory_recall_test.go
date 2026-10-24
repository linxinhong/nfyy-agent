package in_memory_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db/in_memory"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

func TestRecallFiltersBeforeCandidateLimitAndDoesNotReadOtherScopes(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	store := in_memory.NewMemoryStore().SetClock(func() time.Time { return now })
	m, err := memory.NewModule(memory.ModuleConfig{
		Store:      store,
		Authorizer: memory.AuthorizerFunc(func(context.Context, memory.AuthorizeRequest) error { return nil }),
		Keys:       map[string][]byte{"test": []byte("0123456789abcdef0123456789abcdef")}, CurrentKey: "test",
		Limits: memory.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := memory.Scope{Namespace: "n", ProjectID: "p", SubjectID: "s", CollectionID: "c"}
	p := memory.Principal{Subject: "s", Namespace: "n", Issuer: "host"}
	create := func(scope memory.Scope, p memory.Principal, kind memory.Kind, content string, retention time.Duration) {
		t.Helper()
		token, err := m.IssueOperationToken(context.Background(), p, scope, memory.ActionCreate, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.Create(context.Background(), p, scope, memory.CreateRequest{Content: content, Kind: kind, Retention: retention, OperationToken: token}); err != nil {
			t.Fatal(err)
		}
	}
	create(scope, p, memory.KindUserStatement, "old eligible coffee", time.Hour)
	now = now.Add(time.Minute)
	for i := 0; i < 201; i++ {
		create(scope, p, memory.KindPreference, fmt.Sprintf("new wrong kind %03d", i), time.Hour)
	}
	for i := 0; i < 100; i++ {
		create(scope, p, memory.KindUserStatement, fmt.Sprintf("expired %03d", i), time.Minute)
	}
	for i := 0; i < 100; i++ {
		other := memory.Scope{Namespace: "n", ProjectID: "p", SubjectID: fmt.Sprintf("other-%03d", i), CollectionID: "c"}
		create(other, memory.Principal{Subject: other.SubjectID, Namespace: "n", Issuer: "host"}, memory.KindUserStatement, "coffee from other scope", time.Hour)
	}
	now = now.Add(2 * time.Minute)
	before := store.ContentReadCount()
	got, err := m.Recall(context.Background(), p, scope, memory.RecallRequest{Query: "coffee", Limit: 10, Kinds: []memory.Kind{memory.KindUserStatement}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].Content != "old eligible coffee" || got.CandidateCount != 1 {
		t.Fatalf("recall = %#v", got)
	}
	if reads := store.ContentReadCount() - before; reads != 1 {
		t.Fatalf("decoded contents = %d, want 1 (expired and cross-scope must be filtered before materialization)", reads)
	}
}

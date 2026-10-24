package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type cursorTestStore struct{}

func (cursorTestStore) SnapshotSubject(context.Context, SubjectScope) (ControlSnapshot, error) {
	return ControlSnapshot{}, nil
}
func (cursorTestStore) Snapshot(context.Context, Scope, string) (ControlSnapshot, error) {
	return ControlSnapshot{}, nil
}
func (cursorTestStore) ReadRecord(context.Context, Scope, string) (*Record, ControlSnapshot, error) {
	return nil, ControlSnapshot{}, nil
}
func (cursorTestStore) ListRecords(context.Context, ListRequest) (ListResult, error) {
	return ListResult{}, errors.New("expired cursor reached store")
}
func (cursorTestStore) CommitMutation(context.Context, Mutation) (MutationResult, error) {
	return MutationResult{}, nil
}

func TestListPageRejectsCryptographicallyValidExpiredCursorBeforeStore(t *testing.T) {
	module, err := NewModule(ModuleConfig{
		Store: cursorTestStore{}, Authorizer: AuthorizerFunc(func(context.Context, AuthorizeRequest) error { return nil }),
		Keys: map[string][]byte{"current": []byte(strings.Repeat("k", 32))}, CurrentKey: "current",
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Namespace: "tenant", ProjectID: "project", SubjectID: "alice", CollectionID: "profile"}
	cursor, err := module.signListCursor(listCursorClaims{
		Version: 1, KeyID: "current", Scope: scope, Generation: 1,
		UpdatedAtNS: time.Now().Add(-time.Hour).UnixNano(), MemoryID: "record", ExpiresAtNS: time.Now().Add(-time.Second).UnixNano(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = module.ListPage(context.Background(), Principal{Subject: "alice", Namespace: "tenant", Issuer: "issuer"}, scope, ListPageRequest{Limit: 1, Cursor: cursor})
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("error=%v", err)
	}
}

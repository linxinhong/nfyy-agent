package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	knowledgepkg "github.com/linxinhong/xingding-agent-runtime/pkg/knowledge"
	"github.com/linxinhong/xingding-agent-runtime/pkg/vectordb"
)

type fakeKnowledge struct {
	searchDocs []vectordb.Document
	content    string
}

func (f *fakeKnowledge) Search(_ context.Context, _ string, _ int) ([]vectordb.Document, error) {
	return f.searchDocs, nil
}

func (f *fakeKnowledge) GetContent(_ context.Context, _ string) (string, error) {
	return f.content, nil
}

func TestListKnowledges(t *testing.T) {
	knowledges := map[string]knowledgepkg.Knowledge{
		"kb1": &fakeKnowledge{},
		"kb2": &fakeKnowledge{},
	}
	router := NewRouter(knowledges)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp ListKnowledgeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Knowledges) != 2 {
		t.Fatalf("expected 2 knowledges, got %d", len(resp.Knowledges))
	}
	if resp.Knowledges[0].ID != "kb1" || resp.Knowledges[1].ID != "kb2" {
		t.Fatalf("unexpected order: %+v", resp.Knowledges)
	}
}

func TestSearchKnowledge(t *testing.T) {
	knowledges := map[string]knowledgepkg.Knowledge{
		"kb1": &fakeKnowledge{
			searchDocs: []vectordb.Document{
				{ID: "doc-1", Content: "result one"},
				{ID: "doc-2", Content: "result two"},
			},
		},
	}
	router := NewRouter(knowledges)
	body, _ := json.Marshal(SearchRequest{Query: "hello", Limit: 2})
	req := httptest.NewRequest(http.MethodPost, "/kb1/search", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp SearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Documents) != 2 {
		t.Fatalf("expected 2 documents, got %d", len(resp.Documents))
	}
	if resp.Documents[0].Content != "result one" {
		t.Fatalf("unexpected document: %+v", resp.Documents[0])
	}
}

func TestGetContent(t *testing.T) {
	knowledges := map[string]knowledgepkg.Knowledge{
		"kb1": &fakeKnowledge{content: "full document content"},
	}
	router := NewRouter(knowledges)
	req := httptest.NewRequest(http.MethodGet, "/kb1/content/doc-1", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["content"] != "full document content" {
		t.Fatalf("content = %q, want full document content", resp["content"])
	}
}

func TestKnowledgeNotFound(t *testing.T) {
	router := NewRouter(map[string]knowledgepkg.Knowledge{})

	// Search on a missing knowledge base.
	body, _ := json.Marshal(SearchRequest{Query: "hello"})
	req := httptest.NewRequest(http.MethodPost, "/missing/search", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("search: expected 404, got %d", rec.Code)
	}

	// Content on a missing knowledge base.
	req = httptest.NewRequest(http.MethodGet, "/missing/content/doc-1", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("content: expected 404, got %d", rec.Code)
	}
}

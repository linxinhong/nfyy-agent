package bookstack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBookStackClientPageAndPermissions(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token service-id:secret" {
			t.Error("token was not sent to the configured BookStack host")
		}
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.Method + " " + r.URL.Path {
		case "GET /base/api/pages/42":
			_, _ = w.Write([]byte(`{"id":42,"name":"知识条目","markdown":"正文","created_by":{"id":9}}`))
		case "POST /base/api/pages":
			var input PageInput
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.ChapterID != 7 || input.Name != "待审核" {
				t.Errorf("unexpected page create input: %+v, %v", input, err)
			}
			_, _ = w.Write([]byte(`{"id":43,"name":"待审核"}`))
		case "PUT /base/api/content-permissions/page/43":
			var input Permissions
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.FallbackPermissions.View || len(input.RolePermissions) != 1 || !input.RolePermissions[0].View {
				t.Errorf("permissions were not fail-closed: %+v, %v", input, err)
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(Config{URL: server.URL + "/base", TokenID: "service-id", TokenSecret: "secret", AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	page, err := client.GetPage(context.Background(), 42)
	if err != nil || page.CreatedBy.ID != 9 || page.Markdown != "正文" {
		t.Fatalf("unexpected page: %+v, %v", page, err)
	}
	page, err = client.CreatePage(context.Background(), PageInput{ChapterID: 7, Name: "待审核", Markdown: "正文"})
	if err != nil || page.ID != 43 {
		t.Fatalf("unexpected created page: %+v, %v", page, err)
	}
	err = client.SetPermissions(context.Background(), "page", 43, Permissions{
		RolePermissions:     []RolePermission{{RoleID: 4, View: true, Update: true}},
		FallbackPermissions: FallbackPermission{Inheriting: false},
	})
	if err != nil || len(calls) != 3 {
		t.Fatalf("unexpected ACL call result: %v, %v", calls, err)
	}
}

func TestBookStackClientRejectsUnsafeConfigurationAndRedirect(t *testing.T) {
	for _, cfg := range []Config{
		{URL: "http://bookstack", TokenID: "id", TokenSecret: "secret"},
		{URL: "https://user:password@bookstack", TokenID: "id", TokenSecret: "secret"},
		{URL: "https://bookstack?x=1", TokenID: "id", TokenSecret: "secret"},
		{URL: "https://bookstack", TokenID: "id\nInjected: yes", TokenSecret: "secret"},
	} {
		if _, err := New(cfg); !errors.Is(err, ErrInvalid) {
			t.Errorf("unsafe config accepted: %+v, %v", cfg, err)
		}
	}
	var redirected bool
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", other.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	client, err := New(Config{URL: server.URL, TokenID: "id", TokenSecret: "secret", AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetPage(context.Background(), 1)
	if !errors.Is(err, ErrUnavailable) || redirected {
		t.Fatalf("redirect followed or error exposed: %v, redirected=%v", err, redirected)
	}
	if err := client.SetPermissions(context.Background(), "page", 1, Permissions{FallbackPermissions: FallbackPermission{View: true}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("open fallback accepted: %v", err)
	}
	if err := client.SetPermissions(context.Background(), "../users", 1, Permissions{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unrecognized content type accepted: %v", err)
	}
	if _, err := client.CreatePage(context.Background(), PageInput{Name: strings.Repeat("x", 2), Markdown: "body"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("parentless page accepted: %v", err)
	}
}

package httpkit

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSONEnforcesWholeBodyLimit(t *testing.T) {
	for _, body := range []string{
		`{"name":"ok"}` + strings.Repeat(" ", 100),
		`{"name":"ok"}{"extra":true}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		var decoded struct {
			Name string `json:"name"`
		}
		if err := DecodeJSON(request, &decoded, 32); err == nil {
			t.Fatalf("oversized or multiple JSON objects accepted: %q", body)
		}
	}
}

func TestBearerTokenRejectsAmbiguousHeaders(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Add("Authorization", "Bearer first")
	request.Header.Add("Authorization", "Bearer second")
	if BearerToken(request) != "" {
		t.Fatal("duplicate authorization header was accepted")
	}
	request.Header.Del("Authorization")
	request.Header.Set("Authorization", "Bearer one token")
	if BearerToken(request) != "" {
		t.Fatal("malformed bearer token was accepted")
	}
}

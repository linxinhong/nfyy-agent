package industry

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestIndustryRoutesRequireAuthAndReturnValidation(t *testing.T) {
	files := os.DirFS("../../industries")
	pack, err := Load(files, "legal")
	if err != nil {
		t.Fatal(err)
	}
	authenticate := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer test-session" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	router := chi.NewRouter()
	NewHTTP(pack, files, "legal", authenticate).Mount(router)
	call := func(method, path, auth, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", auth)
		if body != "" {
			request.Header.Set("Content-Type", "application/json")
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}
	if got := call("GET", "/v1/industry", "", "").Code; got != http.StatusUnauthorized {
		t.Fatalf("industry pack exposed without auth: %d", got)
	}
	if response := call("GET", "/v1/industry/templates/case_experience", "Bearer test-session", ""); response.Code != 200 || !strings.Contains(response.Body.String(), "办案经验") {
		t.Fatalf("template response: %d %s", response.Code, response.Body.String())
	}
	response := call("POST", "/v1/industry/validate", "Bearer test-session", `{"type":"case_experience","redacted":false}`)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"field":"source"`) || !strings.Contains(response.Body.String(), `"valid":false`) {
		t.Fatalf("validation response: %d %s", response.Code, response.Body.String())
	}
}

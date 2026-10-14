package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"yuanpu.dev/server/internal/content"
	"yuanpu.dev/server/internal/identity"
	"yuanpu.dev/server/migrations"
)

func TestIdentityHTTPWithPostgres(t *testing.T) {
	dsn := os.Getenv("YUANPU_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YUANPU_TEST_DATABASE_URL for real PostgreSQL identity test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminDB, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("test database unavailable")
	}
	defer adminDB.Close(context.Background())
	schema := fmt.Sprintf("yuanpu_identity_test_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := adminDB.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := adminDB.Exec(cleanup, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test DSN")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	sql, err := migrations.Files.ReadFile("0002_identity.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	contentSQL, err := migrations.Files.ReadFile("0003_content.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(contentSQL)); err != nil {
		t.Fatal(err)
	}
	apiKeySQL, err := migrations.Files.ReadFile("0004_api_keys.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(apiKeySQL)); err != nil {
		t.Fatal(err)
	}
	users := identity.New(pool)
	first, err := users.BootstrapAdmin(ctx, "ADMIN@EXAMPLE.COM", "Administrator", "one long initial passphrase")
	if err != nil || first.Email != "admin@example.com" {
		t.Fatalf("bootstrap failed: %v", err)
	}
	if _, err := users.BootstrapAdmin(ctx, "next@example.com", "Next", "another long passphrase"); !errors.Is(err, identity.ErrBootstrapClosed) {
		t.Fatalf("bootstrap reopened: %v", err)
	}
	accounts := identity.NewHTTP(users)
	handler := New("test", readinessFunc(func(context.Context) (int, error) { return 4, nil }), accounts, content.NewHTTP(content.New(pool), accounts.Authenticate))
	call := func(method, path, token string, input any) *httptest.ResponseRecorder {
		t.Helper()
		var body []byte
		if input != nil {
			body, _ = json.Marshal(input)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		if input != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	login := call("POST", "/v1/auth/login", "", map[string]string{"email": "admin@example.com", "password": "one long initial passphrase"})
	if login.Code != 200 {
		t.Fatalf("login failed: %d %s", login.Code, login.Body.String())
	}
	var session struct {
		User  identity.User `json:"user"`
		Token string        `json:"token"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &session); err != nil || session.Token == "" {
		t.Fatal("session token missing")
	}
	if got := call("POST", "/v1/auth/login", "", map[string]string{"email": "admin@example.com", "password": "wrong password"}); got.Code != 401 {
		t.Fatalf("wrong password: %d", got.Code)
	}
	if got := call("GET", "/v1/auth/me", session.Token, nil); got.Code != 200 {
		t.Fatalf("me failed: %d", got.Code)
	}
	if got := call("POST", "/v1/admin/users", "", map[string]string{"email": "x@example.com", "name": "X", "role": "member"}); got.Code != 401 {
		t.Fatalf("unauthenticated account creation: %d", got.Code)
	}
	team := call("POST", "/v1/admin/teams", session.Token, map[string]string{"name": "Litigation"})
	if team.Code != 201 {
		t.Fatalf("team creation failed: %d %s", team.Code, team.Body.String())
	}
	var teamBody identity.Team
	if err := json.Unmarshal(team.Body.Bytes(), &teamBody); err != nil {
		t.Fatal(err)
	}
	created := call("POST", "/v1/admin/users", session.Token, map[string]string{"email": "member@example.com", "name": "Member", "role": "member", "teamId": teamBody.ID})
	if created.Code != 201 {
		t.Fatalf("member creation failed: %d %s", created.Code, created.Body.String())
	}
	var account struct {
		User            identity.User `json:"user"`
		InitialPassword string        `json:"initialPassword"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &account); err != nil || account.InitialPassword == "" {
		t.Fatal("initial password missing")
	}
	memberLogin := call("POST", "/v1/auth/login", "", map[string]string{"email": "member@example.com", "password": account.InitialPassword})
	if memberLogin.Code != 200 {
		t.Fatalf("member login failed: %d", memberLogin.Code)
	}
	var memberSession struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(memberLogin.Body.Bytes(), &memberSession); err != nil {
		t.Fatal(err)
	}
	issued := call("POST", "/v1/admin/api-keys", session.Token, map[string]any{
		"userId": account.User.ID, "name": "Member desktop", "expiresInDays": 90,
	})
	if issued.Code != 201 {
		t.Fatalf("API key issue failed: %d %s", issued.Code, issued.Body.String())
	}
	var issuedKey struct {
		APIKey identity.APIKey `json:"apiKey"`
		Secret string          `json:"secret"`
	}
	if err := json.Unmarshal(issued.Body.Bytes(), &issuedKey); err != nil || !strings.HasPrefix(issuedKey.Secret, "ypk_") {
		t.Fatal("API key secret missing")
	}
	if got := call("POST", "/v1/admin/api-keys", memberSession.Token, map[string]any{
		"userId": account.User.ID, "name": "Forbidden", "expiresInDays": 90,
	}); got.Code != 403 {
		t.Fatalf("member issued API key: %d", got.Code)
	}
	listedKeys := call("GET", "/v1/admin/api-keys", session.Token, nil)
	if listedKeys.Code != 200 || !strings.Contains(listedKeys.Body.String(), issuedKey.APIKey.ID) || strings.Contains(listedKeys.Body.String(), issuedKey.Secret) {
		t.Fatalf("API key metadata list leaked or omitted secret: %d", listedKeys.Code)
	}
	if got := call("POST", "/v1/auth/api-key", "", map[string]string{"apiKey": "ypk_invalid"}); got.Code != 401 {
		t.Fatalf("invalid API key accepted: %d", got.Code)
	}
	keyLogin := call("POST", "/v1/auth/api-key", "", map[string]string{"apiKey": issuedKey.Secret})
	if keyLogin.Code != 200 {
		t.Fatalf("API key login failed: %d %s", keyLogin.Code, keyLogin.Body.String())
	}
	var keySession struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(keyLogin.Body.Bytes(), &keySession); err != nil || keySession.Token == "" {
		t.Fatal("API key session missing")
	}
	if got := call("GET", "/v1/auth/me", keySession.Token, nil); got.Code != 200 {
		t.Fatalf("API key session unusable: %d", got.Code)
	}
	if got := call("DELETE", "/v1/admin/api-keys/"+issuedKey.APIKey.ID, session.Token, nil); got.Code != 204 {
		t.Fatalf("API key revoke failed: %d", got.Code)
	}
	if got := call("GET", "/v1/auth/me", keySession.Token, nil); got.Code != 401 {
		t.Fatalf("API key derived session survived revoke: %d", got.Code)
	}
	if got := call("POST", "/v1/auth/api-key", "", map[string]string{"apiKey": issuedKey.Secret}); got.Code != 401 {
		t.Fatalf("revoked API key still worked: %d", got.Code)
	}
	expiring := call("POST", "/v1/admin/api-keys", session.Token, map[string]any{
		"userId": account.User.ID, "name": "Expiring desktop", "expiresInDays": 1,
	})
	if expiring.Code != 201 {
		t.Fatalf("second API key issue failed: %d", expiring.Code)
	}
	var expiringKey struct {
		APIKey identity.APIKey `json:"apiKey"`
		Secret string          `json:"secret"`
	}
	if err := json.Unmarshal(expiring.Body.Bytes(), &expiringKey); err != nil {
		t.Fatal(err)
	}
	expiringLogin := call("POST", "/v1/auth/api-key", "", map[string]string{"apiKey": expiringKey.Secret})
	if expiringLogin.Code != 200 {
		t.Fatalf("second API key login failed: %d", expiringLogin.Code)
	}
	var expiringSession struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(expiringLogin.Body.Bytes(), &expiringSession); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE yuanpu_api_keys SET expires_at=now()-interval '1 second' WHERE id=$1`, expiringKey.APIKey.ID); err != nil {
		t.Fatal(err)
	}
	if got := call("GET", "/v1/auth/me", expiringSession.Token, nil); got.Code != 401 {
		t.Fatalf("API key session survived expiry: %d", got.Code)
	}
	if got := call("POST", "/v1/auth/api-key", "", map[string]string{"apiKey": expiringKey.Secret}); got.Code != 401 {
		t.Fatalf("expired API key still worked: %d", got.Code)
	}
	if got := call("POST", "/v1/admin/teams", memberSession.Token, map[string]string{"name": "Forbidden"}); got.Code != 403 {
		t.Fatalf("member created team: %d", got.Code)
	}
	if got := call("GET", "/v1/admin/users", memberSession.Token, nil); got.Code != 403 {
		t.Fatalf("member listed accounts: %d", got.Code)
	}
	if got := call("GET", "/v1/admin/users", session.Token, nil); got.Code != 200 {
		t.Fatalf("administrator could not list accounts: %d", got.Code)
	}
	if got := call("PUT", "/v1/admin/users/"+first.ID, session.Token, map[string]any{"role": "member", "teamId": nil, "active": true}); got.Code != 409 {
		t.Fatalf("last administrator demoted: %d", got.Code)
	}
	if got := call("PUT", "/v1/admin/users/"+account.User.ID, session.Token, map[string]any{"role": "team_lead", "teamId": teamBody.ID, "active": true}); got.Code != 200 {
		t.Fatalf("team lead update failed: %d %s", got.Code, got.Body.String())
	}
	if got := call("POST", "/v1/auth/logout", memberSession.Token, nil); got.Code != 204 {
		t.Fatalf("logout failed: %d", got.Code)
	}
	if got := call("GET", "/v1/auth/me", memberSession.Token, nil); got.Code != 401 {
		t.Fatalf("revoked token still worked: %d", got.Code)
	}
	memberLogin = call("POST", "/v1/auth/login", "", map[string]string{"email": "member@example.com", "password": account.InitialPassword})
	if memberLogin.Code != 200 {
		t.Fatalf("member re-login failed: %d", memberLogin.Code)
	}
	if err := json.Unmarshal(memberLogin.Body.Bytes(), &memberSession); err != nil {
		t.Fatal(err)
	}
	if got := call("PUT", "/v1/admin/users/"+account.User.ID, session.Token, map[string]any{"role": "member", "teamId": teamBody.ID, "active": false}); got.Code != 200 {
		t.Fatalf("member deactivation failed: %d %s", got.Code, got.Body.String())
	}
	if got := call("GET", "/v1/auth/me", memberSession.Token, nil); got.Code != 401 {
		t.Fatalf("deactivated member session still worked: %d", got.Code)
	}
	if got := call("POST", "/v1/auth/login", "", map[string]string{"email": "member@example.com", "password": account.InitialPassword}); got.Code != 401 {
		t.Fatalf("deactivated member login succeeded: %d", got.Code)
	}
	otherTeam := call("POST", "/v1/admin/teams", session.Token, map[string]string{"name": "Corporate"})
	if otherTeam.Code != 201 {
		t.Fatalf("second team failed: %d", otherTeam.Code)
	}
	var otherTeamBody identity.Team
	if err := json.Unmarshal(otherTeam.Body.Bytes(), &otherTeamBody); err != nil {
		t.Fatal(err)
	}
	makeMember := func(email, role, teamID string) string {
		t.Helper()
		created := call("POST", "/v1/admin/users", session.Token, map[string]string{"email": email, "name": email, "role": role, "teamId": teamID})
		if created.Code != 201 {
			t.Fatalf("account %s: %d %s", email, created.Code, created.Body.String())
		}
		var result struct {
			InitialPassword string `json:"initialPassword"`
		}
		if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		login := call("POST", "/v1/auth/login", "", map[string]string{"email": email, "password": result.InitialPassword})
		if login.Code != 200 {
			t.Fatalf("login %s: %d", email, login.Code)
		}
		var session struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(login.Body.Bytes(), &session); err != nil {
			t.Fatal(err)
		}
		return session.Token
	}
	aToken := makeMember("author@example.com", "member", teamBody.ID)
	bToken := makeMember("reader@example.com", "member", teamBody.ID)
	cToken := makeMember("outsider@example.com", "team_lead", otherTeamBody.ID)
	reviewerToken := makeMember("reviewer@example.com", "reviewer", otherTeamBody.ID)
	for _, kind := range []string{"news", "wiki", "capability"} {
		input := map[string]any{"kind": kind, "visibility": "team", "teamId": teamBody.ID, "title": "Shared " + kind, "summary": "Team knowledge", "body": "Reviewed content", "details": map[string]any{}}
		created := call("POST", "/v1/resources", aToken, input)
		if created.Code != 201 {
			t.Fatalf("create %s: %d %s", kind, created.Code, created.Body.String())
		}
		var item content.Resource
		if err := json.Unmarshal(created.Body.Bytes(), &item); err != nil {
			t.Fatal(err)
		}
		url := "/v1/resources/" + item.ID
		if got := call("GET", url, bToken, nil); got.Code != 404 {
			t.Fatalf("draft visible to teammate: %d", got.Code)
		}
		if got := call("POST", url+"/submit", aToken, nil); got.Code != 200 {
			t.Fatalf("submit %s: %d %s", kind, got.Code, got.Body.String())
		}
		if got := call("POST", url+"/approve", aToken, nil); got.Code != 403 {
			t.Fatalf("author self-approved %s: %d", kind, got.Code)
		}
		if got := call("POST", url+"/approve", cToken, nil); got.Code != 403 {
			t.Fatalf("other team lead approved %s: %d", kind, got.Code)
		}
		if got := call("GET", "/v1/review/queue", reviewerToken, nil); got.Code != 200 || !strings.Contains(got.Body.String(), item.ID) {
			t.Fatalf("review queue missing %s: %d", kind, got.Code)
		}
		if got := call("POST", url+"/approve", reviewerToken, nil); got.Code != 200 {
			t.Fatalf("approve %s: %d %s", kind, got.Code, got.Body.String())
		}
		if got := call("GET", url, bToken, nil); got.Code != 200 {
			t.Fatalf("teammate cannot read %s: %d", kind, got.Code)
		}
		if got := call("GET", url, cToken, nil); got.Code != 404 {
			t.Fatalf("other team read %s: %d", kind, got.Code)
		}
		input["title"] = "Updated " + kind
		if got := call("PUT", url, aToken, input); got.Code != 200 {
			t.Fatalf("edit %s: %d %s", kind, got.Code, got.Body.String())
		}
		if got := call("GET", url, aToken, nil); got.Code != 200 || !strings.Contains(got.Body.String(), "Updated "+kind) {
			t.Fatalf("author cannot read pending %s: %d %s", kind, got.Code, got.Body.String())
		}
		if got := call("GET", url, bToken, nil); got.Code != 200 || strings.Contains(got.Body.String(), "Updated "+kind) {
			t.Fatalf("teammate saw pending %s: %d %s", kind, got.Code, got.Body.String())
		}
		if got := call("GET", "/v1/resources?kind="+kind, cToken, nil); got.Code != 200 || strings.Contains(got.Body.String(), item.ID) {
			t.Fatalf("other team listed %s: %d", kind, got.Code)
		}
		if got := call("POST", url+"/withdraw", aToken, nil); got.Code != 200 {
			t.Fatalf("withdraw %s: %d", kind, got.Code)
		}
		if got := call("GET", url, bToken, nil); got.Code != 404 {
			t.Fatalf("withdrawn %s remained visible: %d", kind, got.Code)
		}
	}
	longWiki := map[string]any{"kind": "wiki", "visibility": "team", "teamId": teamBody.ID, "title": "Long", "summary": "", "body": strings.Repeat("字", 801)}
	if got := call("POST", "/v1/resources", aToken, longWiki); got.Code != 400 {
		t.Fatalf("overlong wiki accepted: %d", got.Code)
	}
	longNews := map[string]any{"kind": "news", "visibility": "team", "teamId": teamBody.ID, "title": "Long news", "summary": "", "body": strings.Repeat("a", 12_000)}
	if got := call("POST", "/v1/resources", aToken, longNews); got.Code != 201 {
		t.Fatalf("valid long news body rejected: %d", got.Code)
	}
}

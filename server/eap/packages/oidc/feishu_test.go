package oidc_test

// Feishu profile coverage (TASK-013): authorize URL shape, code exchange with
// the {code,msg,data} envelope, identity binding via the server-side
// user_info call, union_id preference over open_id, and fail-closed error
// paths (non-zero envelope code, missing subject).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"enterprise-agent-platform/packages/oidc"
)

func newFeishuStub(t *testing.T, tokenBody, userInfoBody string, userInfoAuth string) (*oidc.FeishuConfig, *[]string) {
	t.Helper()
	var capturedAuth []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /open-apis/auth/v3/app_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["app_id"] != "cli_test" || body["app_secret"] != "sec" {
			_, _ = w.Write([]byte(`{"code":99991661,"msg":"invalid app credential","data":null}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","app_access_token":"app-at","expire":7200}`))
	})
	mux.HandleFunc("POST /open-apis/authen/v1/oidc/access_token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer app-at" {
			_, _ = w.Write([]byte(`{"code":20014,"msg":"invalid app access token","data":null}`))
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["code"] != "the-code" || body["grant_type"] != "authorization_code" {
			_, _ = w.Write([]byte(`{"code":20003,"msg":"invalid code","data":null}`))
			return
		}
		_, _ = w.Write([]byte(tokenBody))
	})
	mux.HandleFunc("GET /open-apis/authen/v1/user_info", func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = append(capturedAuth, r.Header.Get("Authorization"))
		if userInfoAuth != "" && r.Header.Get("Authorization") != "Bearer "+userInfoAuth {
			_, _ = w.Write([]byte(`{"code":99991661,"msg":"invalid access token","data":null}`))
			return
		}
		_, _ = w.Write([]byte(userInfoBody))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	cfg := &oidc.FeishuConfig{
		ClientID: "cli_test", ClientSecret: "sec",
		RedirectURL:   "http://127.0.0.1:18080/enterprise/v1/auth/oidc/callback",
		DefaultGroups: []string{"secops"},
		AppTokenURL:   ts.URL + "/open-apis/auth/v3/app_access_token/internal",
		TokenURL:      ts.URL + "/open-apis/authen/v1/oidc/access_token",
		UserInfoURL:   ts.URL + "/open-apis/authen/v1/user_info",
	}
	return cfg, &capturedAuth
}

func TestFeishuAuthorizeURLShape(t *testing.T) {
	cfg := &oidc.FeishuConfig{ClientID: "cli_test", RedirectURL: "http://127.0.0.1:18080/cb"}
	req, _ := oidc.NewAuthorizationRequest(time.Now())
	u, err := cfg.FeishuAuthorizeURL(req)
	if err != nil {
		t.Fatalf("authorize url: %v", err)
	}
	for _, want := range []string{
		"https://accounts.feishu.cn/open-apis/authen/v1/authorize?",
		"client_id=cli_test", "response_type=code",
		"redirect_uri=", "state=",
	} {
		if !contains(u, want) {
			t.Fatalf("authorize url missing %q: %s", want, u)
		}
	}
	if contains(u, "nonce=") || contains(u, "code_challenge=") {
		t.Fatalf("feishu authorize must not carry nonce/PKCE: %s", u)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && stringsIndex(haystack, needle) >= 0
}

func stringsIndex(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

func TestFeishuExchangeIdentityBindsViaUserInfo(t *testing.T) {
	tokenBody := `{"code":0,"msg":"ok","data":{"access_token":"u-at","expires_in":6900,"id_token":"x.y.z"}}`
	userBody := `{"code":0,"msg":"ok","data":{"union_id":"on_x","open_id":"ou_y","name":"张三","email":"zhang@company.com"}}`
	cfg, auths := newFeishuStub(t, tokenBody, userBody, "u-at")
	res, err := cfg.ExchangeIdentity(context.Background(), "the-code")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if res.Subject != "on_x" {
		t.Fatalf("subject must prefer union_id: %q", res.Subject)
	}
	if res.Email != "zhang@company.com" || res.DisplayName != "张三" {
		t.Fatalf("identity mapping wrong: %+v", res)
	}
	if len(res.Groups) != 1 || res.Groups[0] != "secops" {
		t.Fatalf("operator-declared groups must flow to the mapping: %v", res.Groups)
	}
	if len(*auths) != 1 || (*auths)[0] != "Bearer u-at" {
		t.Fatalf("user_info must be called with the exchanged access token: %v", *auths)
	}
}

func TestFeishuExchangeIdentityFallsBackToOpenID(t *testing.T) {
	cfg, _ := newFeishuStub(t,
		`{"code":0,"msg":"ok","data":{"access_token":"u-at","expires_in":3600}}`,
		`{"code":0,"msg":"ok","data":{"open_id":"ou_only","name":"李四","email":"lisi@company.com"}}`, "u-at")
	res, err := cfg.ExchangeIdentity(context.Background(), "the-code")
	if err != nil || res.Subject != "ou_only" {
		t.Fatalf("open_id fallback: %v %+v", err, res)
	}
}

func TestFeishuExchangeIdentityMissingEmailIsDiagnosable(t *testing.T) {
	cfg, _ := newFeishuStub(t,
		`{"code":0,"msg":"ok","data":{"access_token":"u-at","expires_in":3600}}`,
		`{"code":0,"msg":"ok","data":{"open_id":"ou_noemail","name":"王五"}}`, "u-at")
	res, err := cfg.ExchangeIdentity(context.Background(), "the-code")
	if err != nil {
		t.Fatalf("missing email must not fail at the adapter: %v", err)
	}
	if res.Email != "" || res.Subject != "ou_noemail" {
		t.Fatalf("identity mapping: %+v", res)
	}
	if !strings.Contains(res.Diagnostics, "fields present: name,open_id") {
		t.Fatalf("diagnostics must explain the absent email: %q", res.Diagnostics)
	}
}

func TestFeishuExchangeIdentityFailsClosed(t *testing.T) {
	// Non-zero envelope code at the token endpoint (after a valid app token).
	cfg, _ := newFeishuStub(t, `{"code":20003,"msg":"invalid code","data":null}`, ``, "")
	if _, err := cfg.ExchangeIdentity(context.Background(), "bad"); err == nil {
		t.Fatalf("rejected exchange must fail closed")
	}
	// user_info without a stable subject.
	cfg2, _ := newFeishuStub(t,
		`{"code":0,"msg":"ok","data":{"access_token":"u-at"}}`,
		`{"code":0,"msg":"ok","data":{"name":"no id"}}`, "u-at")
	if _, err := cfg2.ExchangeIdentity(context.Background(), "the-code"); err == nil {
		t.Fatalf("missing subject must fail closed")
	}
}

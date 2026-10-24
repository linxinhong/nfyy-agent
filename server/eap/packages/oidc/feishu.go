// Feishu (Lark) identity provider profile (TASK-013 real-IdP qualification).
//
// Feishu implements OAuth2 authorization-code login with its own endpoint
// shapes rather than standard OIDC discovery: the authorize endpoint accepts
// the standard client_id/response_type/redirect_uri/state parameter set, the
// token exchange is a JSON POST whose envelope is {code, msg, data}, and the
// identity anchor is the server-side user_info call made with the exchanged
// user access token (TLS + confidential client secret). If the exchange
// returns an id_token it is kept for audit but identity binding uses
// user_info — Feishu does not publish a standard JWKS discovery document.
//
// Trust rules (ADR-0009) are unchanged: the browser never supplies identity;
// the subject is the Feishu union_id (falling back to open_id), and workspace
// authority comes exclusively from the operator-declared group mapping
// (DefaultGroups, supplied out of band), never from client input.
package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Feishu endpoint constants (production defaults; overridable per instance
// for tests and future tenant regions). The identity exchange is two-legged:
// the confidential app first mints an app_access_token (cached, ~2h TTL),
// then presents it as the Bearer credential for the code exchange.
const (
	FeishuAuthorizeBase = "https://accounts.feishu.cn/open-apis/authen/v1/authorize"
	FeishuAppTokenURL   = "https://open.feishu.cn/open-apis/auth/v3/app_access_token/internal"
	FeishuTokenURL      = "https://open.feishu.cn/open-apis/authen/v1/oidc/access_token"
	FeishuUserInfoURL   = "https://open.feishu.cn/open-apis/authen/v1/user_info"
)

// FeishuConfig describes one Feishu self-built application (企业自建应用).
type FeishuConfig struct {
	ClientID     string // App ID, cli_...
	ClientSecret string // App Secret; stays server-side
	RedirectURL  string // registered in the Feishu console security settings
	// DefaultGroups are the operator-declared IdP groups every login of this
	// tenant carries (Feishu user_info has no group claim). They feed the
	// same explicit idp_group_workspaces mapping as standard OIDC.
	DefaultGroups []string
	AuthorizeBase string // default FeishuAuthorizeBase
	AppTokenURL   string // default FeishuAppTokenURL
	TokenURL      string // default FeishuTokenURL
	UserInfoURL   string // default FeishuUserInfoURL
	HTTPClient    *http.Client
	Now           func() time.Time

	appTokenMu        sync.Mutex
	appToken          string
	appTokenExpiresAt time.Time
}

// FeishuAuthorizeURL builds the Feishu consent redirect. Feishu does not
// consume nonce/PKCE here; the server-side state binding is the replay
// defense and it stays single-use exactly like the standard flow.
func (c *FeishuConfig) FeishuAuthorizeURL(req *AuthorizationRequest) (string, error) {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", c.ClientID)
	q.Set("redirect_uri", c.RedirectURL)
	q.Set("state", req.State)
	base := c.AuthorizeBase
	if base == "" {
		base = FeishuAuthorizeBase
	}
	return base + "?" + q.Encode(), nil
}

type feishuEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// appAccessToken mints and caches the app-level credential for the tenant.
func (c *FeishuConfig) appAccessToken(ctx context.Context) (string, error) {
	c.appTokenMu.Lock()
	defer c.appTokenMu.Unlock()
	now := c.Now
	if now == nil {
		now = time.Now
	}
	if c.appToken != "" && now().Before(c.appTokenExpiresAt) {
		return c.appToken, nil
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	appTokenURL := c.AppTokenURL
	if appTokenURL == "" {
		appTokenURL = FeishuAppTokenURL
	}
	payload, _ := json.Marshal(map[string]string{"app_id": c.ClientID, "app_secret": c.ClientSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, appTokenURL, strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	// The app_access_token endpoint returns a FLAT envelope (fields at the
	// top level, no data wrapper) unlike the authen endpoints.
	var app struct {
		Code           int    `json:"code"`
		Msg            string `json:"msg"`
		AppAccessToken string `json:"app_access_token"`
		Expire         int64  `json:"expire"`
	}
	if err := json.Unmarshal(body, &app); err != nil {
		return "", fmt.Errorf("feishu app token response is not valid JSON (status %d)", resp.StatusCode)
	}
	if app.Code != 0 {
		return "", fmt.Errorf("feishu app credential rejected: code=%d msg=%s", app.Code, app.Msg)
	}
	if app.AppAccessToken == "" {
		return "", fmt.Errorf("feishu app token response carries no app_access_token")
	}
	c.appToken = app.AppAccessToken
	ttl := time.Duration(app.Expire) * time.Second
	if ttl <= 0 || ttl > 2*time.Hour {
		ttl = 2 * time.Hour
	}
	c.appTokenExpiresAt = now().Add(ttl - time.Minute)
	return c.appToken, nil
}

// ExchangeIdentity performs the app-token-authenticated code ->
// user_access_token exchange and binds the identity through the server-side
// user_info call.
func (c *FeishuConfig) ExchangeIdentity(ctx context.Context, code string) (TokenResult, error) {
	var zero TokenResult
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	appToken, err := c.appAccessToken(ctx)
	if err != nil {
		return zero, err
	}
	tokenURL := c.TokenURL
	if tokenURL == "" {
		tokenURL = FeishuTokenURL
	}
	payload, _ := json.Marshal(map[string]string{
		"grant_type":   "authorization_code",
		"code":         code,
		"redirect_uri": c.RedirectURL,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(string(payload)))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+appToken)
	resp, err := client.Do(req)
	if err != nil {
		return zero, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return zero, err
	}
	var env feishuEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return zero, fmt.Errorf("feishu token response is not valid JSON (status %d)", resp.StatusCode)
	}
	if env.Code != 0 {
		return zero, fmt.Errorf("feishu token exchange rejected: code=%d msg=%s", env.Code, env.Msg)
	}
	var token struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		IDToken     string `json:"id_token"`
	}
	if err := json.Unmarshal(env.Data, &token); err != nil || token.AccessToken == "" {
		return zero, fmt.Errorf("feishu token exchange returned no access_token")
	}

	infoReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.userInfoURL(), nil)
	if err != nil {
		return zero, err
	}
	infoReq.Header.Set("Authorization", "Bearer "+token.AccessToken)
	infoResp, err := client.Do(infoReq)
	if err != nil {
		return zero, err
	}
	defer func() { _ = infoResp.Body.Close() }()
	infoBody, err := io.ReadAll(io.LimitReader(infoResp.Body, 1<<20))
	if err != nil {
		return zero, err
	}
	var infoEnv feishuEnvelope
	if err := json.Unmarshal(infoBody, &infoEnv); err != nil {
		return zero, fmt.Errorf("feishu user_info response is not valid JSON (status %d)", infoResp.StatusCode)
	}
	if infoEnv.Code != 0 {
		return zero, fmt.Errorf("feishu user_info rejected: code=%d msg=%s", infoEnv.Code, infoEnv.Msg)
	}
	var info struct {
		UnionID string `json:"union_id"`
		OpenID  string `json:"open_id"`
		Name    string `json:"name"`
		Email   string `json:"email"`
	}
	if err := json.Unmarshal(infoEnv.Data, &info); err != nil {
		return zero, fmt.Errorf("feishu user_info payload is corrupt")
	}
	subject := info.UnionID
	if subject == "" {
		subject = info.OpenID
	}
	if subject == "" {
		return zero, fmt.Errorf("feishu user_info carries no stable subject")
	}
	// A missing email is NOT an adapter error: whether an email-less
	// identity may proceed is a store-level policy (the subject must be
	// pre-bound to an employee row by an operator). Field-presence context
	// is carried on the result for the callback to log — no PII values.
	diag := ""
	if info.Email == "" {
		var present []string
		for field, value := range map[string]string{
			"open_id": info.OpenID, "union_id": info.UnionID, "name": info.Name,
		} {
			if value != "" {
				present = append(present, field)
			}
		}
		sort.Strings(present)
		diag = fmt.Sprintf("no email in user_info (fields present: %s) — check "+
			"contact:user.email:readonly, the version release, or pre-bind the subject",
			strings.Join(present, ","))
	}
	now := c.Now
	if now == nil {
		now = time.Now
	}
	expiry := now().Add(time.Duration(token.ExpiresIn) * time.Second)
	if token.ExpiresIn <= 0 {
		expiry = now().Add(time.Hour)
	}
	return TokenResult{
		Subject:     subject,
		Email:       info.Email,
		DisplayName: info.Name,
		Groups:      c.DefaultGroups,
		Expiry:      expiry,
		Diagnostics: diag,
	}, nil
}

func (c *FeishuConfig) userInfoURL() string {
	if c.UserInfoURL != "" {
		return c.UserInfoURL
	}
	return FeishuUserInfoURL
}

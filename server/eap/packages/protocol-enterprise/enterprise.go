// Package enterprise defines the EAP enterprise bootstrap contract (v1):
// the /enterprise/v1/me and /enterprise/v1/bootstrap response shapes that let
// supported clients self-configure from one authenticated call, plus the
// identity slice contracts: API key lifecycle and audience-scoped token
// exchange (ADR-0002). Shapes are frozen by golden fixtures under
// contracts/schemas/v1/enterprise/.
package enterprise

import "time"

// ContractVersion identifies the frozen contract generation.
const ContractVersion = "v1"

// Client version policy decisions (architecture §7).
const (
	ClientDecisionAllow     = "ALLOW"
	ClientDecisionWarn      = "WARN_UPGRADE"
	ClientDecisionDenyOlder = "DENY_VERSION_TOO_OLD"
)

// User is the employee identity snapshot exposed to clients.
type User struct {
	ID             string `json:"id"`
	DisplayName    string `json:"display_name"`
	Email          string `json:"email"`
	OrganizationID string `json:"organization_id,omitempty"`
	DepartmentID   string `json:"department_id,omitempty"`
}

// Workspace is one workspace the user may select.
type Workspace struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Default bool     `json:"default,omitempty"`
	Roles   []string `json:"roles,omitempty"`
}

// MCPServer is one MCP endpoint offered through the gateway. Endpoint is the
// gateway-relative path; upstream credentials are never part of this contract.
type MCPServer struct {
	Name       string   `json:"name"`
	Endpoint   string   `json:"endpoint"`
	RiskLevels []string `json:"risk_levels,omitempty"`
}

// SkillRef names an available skill.
type SkillRef struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// ClientPolicy is the client version policy for the calling client.
type ClientPolicy struct {
	MinVersion         string `json:"min_version"`
	RecommendedVersion string `json:"recommended_version,omitempty"`
	Decision           string `json:"decision"`
}

// MeResponse is the /enterprise/v1/me body: who am I and which workspaces can
// I select. Client selection via X-Agent-Workspace still requires
// server-side membership validation.
type MeResponse struct {
	User             User        `json:"user"`
	Workspaces       []Workspace `json:"workspaces"`
	DefaultWorkspace string      `json:"default_workspace"`
}

// BootstrapResponse is the /enterprise/v1/bootstrap body: everything a
// supported client needs to self-configure ("one auth, zero config").
type BootstrapResponse struct {
	User         User            `json:"user"`
	Workspaces   []Workspace     `json:"workspaces"`
	ModelAliases []string        `json:"model_aliases"`
	MCPServers   []MCPServer     `json:"mcp_servers"`
	Skills       []SkillRef      `json:"skills"`
	ClientPolicy ClientPolicy    `json:"client_policy"`
	FeatureFlags map[string]bool `json:"feature_flags"`
}

// Key environments on the wire.
const (
	KeyEnvironmentLive = "live"
	KeyEnvironmentTest = "test"
)

// TokenType is the only access token type the exchange returns.
const TokenType = "Bearer"

// TokenExchangeRequest asks the gateway to mint an audience-scoped internal
// token from the presented employee API key and server-validated workspace.
// Audience is a registered internal audience (never agent-gateway).
type TokenExchangeRequest struct {
	Audience   string `json:"audience"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// TokenExchangeResponse carries the short-lived internal token.
type TokenExchangeResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}

// APIKeyCreateRequest creates a new employee key. Environment selects the
// agk_live_/agk_test_ family (default live). ExpiresAt is optional.
// Workspace optionally binds the key to a workspace the creator belongs to
// (ADR-0024): bound keys default to that workspace when the client sends no
// X-Agent-Workspace header.
type APIKeyCreateRequest struct {
	Name        string     `json:"name"`
	Environment string     `json:"environment,omitempty"`
	Workspace   string     `json:"workspace,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

// APIKeyCreated is the create response. Key is the one-time full display
// value; it can never be retrieved again.
type APIKeyCreated struct {
	KeyID     string     `json:"key_id"`
	Key       string     `json:"key"`
	Prefix    string     `json:"prefix"`
	Name      string     `json:"name"`
	Workspace string     `json:"workspace,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// APIKeyView is a stored key as listed by its owner. Only metadata: the full
// key value and its hash are never returned.
type APIKeyView struct {
	KeyID      string     `json:"key_id"`
	Prefix     string     `json:"prefix"`
	Name       string     `json:"name"`
	Workspace  string     `json:"workspace,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	LastIP     *string    `json:"last_ip"`
	ClientID   *string    `json:"client_id"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

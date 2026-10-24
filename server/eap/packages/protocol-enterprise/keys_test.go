package enterprise_test

import (
	"testing"

	"enterprise-agent-platform/internal/golden"
	"enterprise-agent-platform/packages/protocol-enterprise"
)

const keyFixtureDir = "../../contracts/schemas/v1/enterprise"

func TestTokenExchangeRequestFixture(t *testing.T) {
	var req enterprise.TokenExchangeRequest
	golden.Unmarshal(t, keyFixtureDir+"/token-exchange.request.json", &req)
	if req.Audience != "mcp-gateway" || req.TTLSeconds != 600 {
		t.Fatalf("request wrong: %+v", req)
	}
	golden.Check(t, keyFixtureDir+"/token-exchange.request.json", req)
}

func TestTokenExchangeResponseFixture(t *testing.T) {
	var resp enterprise.TokenExchangeResponse
	golden.Unmarshal(t, keyFixtureDir+"/token-exchange.response.json", &resp)
	if resp.TokenType != enterprise.TokenType || resp.ExpiresIn != 600 {
		t.Fatalf("response wrong: %+v", resp)
	}
	golden.Check(t, keyFixtureDir+"/token-exchange.response.json", resp)
}

func TestAPIKeyCreateRequestFixture(t *testing.T) {
	var req enterprise.APIKeyCreateRequest
	golden.Unmarshal(t, keyFixtureDir+"/api-key.create.request.json", &req)
	if req.Name != "MacBook Harness" || req.Environment != enterprise.KeyEnvironmentLive {
		t.Fatalf("request wrong: %+v", req)
	}
	if req.ExpiresAt != nil {
		t.Fatalf("null expires_at must decode to nil")
	}
	golden.Check(t, keyFixtureDir+"/api-key.create.request.json", req)
}

func TestAPIKeyCreatedFixture(t *testing.T) {
	var resp enterprise.APIKeyCreated
	golden.Unmarshal(t, keyFixtureDir+"/api-key.create.response.json", &resp)
	if !goldenHasKeyShape(resp.Key) {
		t.Fatalf("fixture key must match agk_live_ shape: %s", resp.Key)
	}
	golden.Check(t, keyFixtureDir+"/api-key.create.response.json", resp)
}

func TestAPIKeyViewFixture(t *testing.T) {
	var view enterprise.APIKeyView
	golden.Unmarshal(t, keyFixtureDir+"/api-key.view.json", &view)
	if view.LastIP == nil || *view.LastIP != "10.1.2.3" {
		t.Fatalf("view wrong: %+v", view)
	}
	if view.RevokedAt != nil {
		t.Fatalf("null revoked_at must decode to nil")
	}
	golden.Check(t, keyFixtureDir+"/api-key.view.json", view)
}

func goldenHasKeyShape(key string) bool {
	return len(key) > 9 && key[:9] == "agk_live_"
}

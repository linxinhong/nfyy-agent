package enterprise_test

import (
	"testing"

	"enterprise-agent-platform/internal/golden"
	"enterprise-agent-platform/packages/protocol-enterprise"
)

const fixtureDir = "../../contracts/schemas/v1/enterprise"

func TestBootstrapResponseFixture(t *testing.T) {
	var b enterprise.BootstrapResponse
	golden.Unmarshal(t, fixtureDir+"/bootstrap.response.json", &b)
	if b.User.ID != "user_123" || len(b.Workspaces) != 1 {
		t.Fatalf("bootstrap decoded wrong: %+v", b)
	}
	if b.Workspaces[0].Default != true {
		t.Fatalf("default flag must decode")
	}
	if len(b.MCPServers) != 1 || b.MCPServers[0].Endpoint != "/mcp/gitlab" {
		t.Fatalf("mcp servers wrong: %+v", b.MCPServers)
	}
	if b.ClientPolicy.Decision != enterprise.ClientDecisionAllow {
		t.Fatalf("client policy wrong: %+v", b.ClientPolicy)
	}
	if !b.FeatureFlags["knowledge_search"] {
		t.Fatalf("feature flags wrong: %+v", b.FeatureFlags)
	}
	golden.Check(t, fixtureDir+"/bootstrap.response.json", b)
}

func TestMeResponseFixture(t *testing.T) {
	var me enterprise.MeResponse
	golden.Unmarshal(t, fixtureDir+"/me.response.json", &me)
	if me.DefaultWorkspace != "secops" {
		t.Fatalf("me decoded wrong: %+v", me)
	}
	golden.Check(t, fixtureDir+"/me.response.json", me)
}

func TestContractVersionFrozen(t *testing.T) {
	if enterprise.ContractVersion != "v1" {
		t.Fatalf("contract version changed: %s", enterprise.ContractVersion)
	}
}

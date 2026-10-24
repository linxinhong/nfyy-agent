package mcp_test

import (
	"encoding/json"
	"testing"

	"enterprise-agent-platform/internal/golden"
	"enterprise-agent-platform/packages/protocol-mcp"
)

const fixtureDir = "../../contracts/schemas/v1/mcp"

func TestInitializeRequestFixture(t *testing.T) {
	var req mcp.InitializeRequest
	golden.Unmarshal(t, fixtureDir+"/initialize.request.json", &req)
	if req.JSONRPC != "2.0" || req.Method != "initialize" {
		t.Fatalf("request envelope wrong: %+v", req)
	}
	if req.Params.ProtocolVersion != mcp.ProtocolVersion {
		t.Fatalf("fixture protocol version %q must match contract %q", req.Params.ProtocolVersion, mcp.ProtocolVersion)
	}
	golden.Check(t, fixtureDir+"/initialize.request.json", req)
}

func TestInitializeResponseFixture(t *testing.T) {
	var resp mcp.InitializeResponse
	golden.Unmarshal(t, fixtureDir+"/initialize.response.json", &resp)
	if resp.Result.ServerInfo.Name != "eap-mcp-gateway" {
		t.Fatalf("response wrong: %+v", resp)
	}
	if resp.Result.Capabilities.Tools == nil {
		t.Fatalf("tools capability must decode")
	}
	golden.Check(t, fixtureDir+"/initialize.response.json", resp)
}

func TestToolsListRequestFixture(t *testing.T) {
	var req mcp.ToolsListRequest
	golden.Unmarshal(t, fixtureDir+"/tools-list.request.json", &req)
	if req.Method != "tools/list" {
		t.Fatalf("request wrong: %+v", req)
	}
	golden.Check(t, fixtureDir+"/tools-list.request.json", req)
}

func TestToolsListResponseFixture(t *testing.T) {
	var resp mcp.ToolsListResponse
	golden.Unmarshal(t, fixtureDir+"/tools-list.response.json", &resp)
	if len(resp.Result.Tools) != 1 || resp.Result.Tools[0].Name != "git_read_file" {
		t.Fatalf("tools wrong: %+v", resp.Result)
	}
	if resp.Result.Tools[0].InputSchema.Type != "object" {
		t.Fatalf("input schema wrong: %+v", resp.Result.Tools[0].InputSchema)
	}
	golden.Check(t, fixtureDir+"/tools-list.response.json", resp)
}

func TestErrorResponseShape(t *testing.T) {
	resp := mcp.ErrorResponse{
		JSONRPC: "2.0",
		ID:      7,
		Error:   mcp.RPCError{Code: mcp.CodeInternalError, Message: "tool execution failed"},
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded mcp.ErrorResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("error response must round trip: %v", err)
	}
	if decoded.Error.Code != mcp.CodeInternalError {
		t.Fatalf("error response decoded wrong: %+v", decoded)
	}
	if id, ok := decoded.ID.(float64); !ok || id != 7 {
		t.Fatalf("numeric id must round trip, got %#v", decoded.ID)
	}
}

func TestContractVersionsFrozen(t *testing.T) {
	if mcp.ContractVersion != "v1" || mcp.ProtocolVersion != "2025-06-18" {
		t.Fatalf("contract versions changed: %s %s", mcp.ContractVersion, mcp.ProtocolVersion)
	}
}

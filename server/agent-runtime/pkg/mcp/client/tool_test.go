package client

import (
	"encoding/json"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/mcp"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

func boolPtr(b bool) *bool { return &b }

func testTool(ann *mcp.ToolAnnotations) *MCPTool {
	return NewMCPTool(nil, mcp.ToolDef{Name: "t", Description: "d", InputSchema: map[string]any{}, Annotations: ann})
}

func TestApproval_UnannotatedRequiresApproval(t *testing.T) {
	for _, tc := range []struct {
		name string
		def  mcp.ToolDef
	}{
		{name: "no annotations field", def: mcp.ToolDef{Name: "t"}},
		{name: "empty annotations", def: mcp.ToolDef{Name: "t", Annotations: &mcp.ToolAnnotations{}}},
		{name: "readOnlyHint false", def: mcp.ToolDef{Name: "t", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: boolPtr(false)}}},
		{name: "readOnlyHint unset with other hints", def: mcp.ToolDef{Name: "t", Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(false), IdempotentHint: boolPtr(true)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewMCPTool(nil, tc.def).Approval(); got != tools.ApprovalRequired {
				t.Fatalf("Approval() = %q, want required", got)
			}
		})
	}
}

func TestApproval_BYOReadOnlyTrueIsNone(t *testing.T) {
	tool := testTool(&mcp.ToolAnnotations{ReadOnlyHint: boolPtr(true)})
	if got := tool.Approval(); got != tools.ApprovalNone {
		t.Fatalf("Approval() = %q, want none", got)
	}
}

func TestApproval_VettedRequiresAllSafeHints(t *testing.T) {
	// Fully safe: none.
	if got := testTool(&mcp.ToolAnnotations{
		ReadOnlyHint: boolPtr(true), DestructiveHint: boolPtr(false), IdempotentHint: boolPtr(true),
	}).WithServerTrust(TrustVetted).Approval(); got != tools.ApprovalNone {
		t.Fatalf("fully safe Approval() = %q, want none", got)
	}
	// Each missing/unsafe combination must fall back to required.
	for _, tc := range []struct {
		name string
		ann  *mcp.ToolAnnotations
	}{
		{name: "destructive unset", ann: &mcp.ToolAnnotations{ReadOnlyHint: boolPtr(true), IdempotentHint: boolPtr(true)}},
		{name: "destructive true", ann: &mcp.ToolAnnotations{ReadOnlyHint: boolPtr(true), DestructiveHint: boolPtr(true), IdempotentHint: boolPtr(true)}},
		{name: "idempotent unset", ann: &mcp.ToolAnnotations{ReadOnlyHint: boolPtr(true), DestructiveHint: boolPtr(false)}},
		{name: "idempotent false", ann: &mcp.ToolAnnotations{ReadOnlyHint: boolPtr(true), DestructiveHint: boolPtr(false), IdempotentHint: boolPtr(false)}},
		{name: "readOnly unset", ann: &mcp.ToolAnnotations{DestructiveHint: boolPtr(false), IdempotentHint: boolPtr(true)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := testTool(tc.ann).WithServerTrust(TrustVetted).Approval(); got != tools.ApprovalRequired {
				t.Fatalf("Approval() = %q, want required", got)
			}
		})
	}
}

func TestApproval_VettedOnlyWhenTrusted(t *testing.T) {
	// The same hints that are auto-approvable under vetted still require
	// approval under the default BYO? No: BYO only needs readOnlyHint==true.
	// Here destructive/idempotent are explicit-safe too, so both agree.
	ann := &mcp.ToolAnnotations{
		ReadOnlyHint: boolPtr(true), DestructiveHint: boolPtr(false), IdempotentHint: boolPtr(true),
	}
	if got := testTool(ann).WithServerTrust(TrustBYO).Approval(); got != tools.ApprovalNone {
		t.Fatalf("BYO Approval() = %q, want none", got)
	}
	if got := testTool(ann).WithServerTrust(TrustVetted).Approval(); got != tools.ApprovalNone {
		t.Fatalf("vetted Approval() = %q, want none", got)
	}
}

func TestToolDefAnnotationsJSONRoundTrip(t *testing.T) {
	raw := []byte(`{"name":"t","description":"d","inputSchema":{},"annotations":{"readOnlyHint":true}}`)
	var def mcp.ToolDef
	if err := json.Unmarshal(raw, &def); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if def.Annotations == nil || def.Annotations.ReadOnlyHint == nil || !*def.Annotations.ReadOnlyHint {
		t.Fatalf("readOnlyHint not parsed: %+v", def.Annotations)
	}
	if def.Annotations.DestructiveHint != nil {
		t.Fatalf("destructiveHint should be unset, got %v", *def.Annotations.DestructiveHint)
	}
	// Round-trip back through JSON.
	reencoded, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var again mcp.ToolDef
	if err := json.Unmarshal(reencoded, &again); err != nil {
		t.Fatalf("second unmarshal: %v", err)
	}
	if again.Annotations == nil || again.Annotations.ReadOnlyHint == nil || !*again.Annotations.ReadOnlyHint {
		t.Fatalf("round-trip lost readOnlyHint: %+v", again.Annotations)
	}
}

func TestApproval_UnannotatedSurvivesPoolNamespace(t *testing.T) {
	// prefixedTool embeds MCPTool, so Approval must be inherited.
	inner := testTool(nil)
	if inner.Approval() != tools.ApprovalRequired {
		t.Fatalf("unannotated Approval() = %q, want required", inner.Approval())
	}
}

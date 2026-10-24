package catalog

import (
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/mcp"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

func TestFromMCPDefPreservesSchemaAndTriState(t *testing.T) {
	ro, de, id := true, false, true
	def := mcp.ToolDef{
		Name:         "t",
		Description:  "d",
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "string"},
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: &ro, DestructiveHint: &de, IdempotentHint: &id},
	}
	desc := FromMCPDef(def, Source{Kind: "mcp", Name: "srv", Version: "1"}, "catrev-1")
	if desc.ReadOnly != TriStateTrue || desc.Destructive != TriStateFalse || desc.Idempotent != TriStateTrue {
		t.Fatalf("tristates = %+v", desc)
	}
	if desc.SourceCatalogRevision != "catrev-1" || desc.OutputSchema["type"] != "string" {
		t.Fatalf("schema/provenance lost: %+v", desc)
	}
}

func TestFromMCPDefUnsetStaysUnset(t *testing.T) {
	def := mcp.ToolDef{Name: "t", InputSchema: map[string]any{}}
	desc := FromMCPDef(def, Source{Kind: "mcp", Name: "srv"}, "rev")
	if desc.ReadOnly != TriStateUnset || desc.Destructive != TriStateUnset || desc.Idempotent != TriStateUnset {
		t.Fatalf("unset hints must stay unset: %+v", desc)
	}
}

// tighteningPolicy always forces Idempotent to false (host never trusts
// remote idempotency claims).
type tighteningPolicy struct{}

func (tighteningPolicy) Resolve(in ToolDescriptor) (ToolDescriptor, error) {
	in.Idempotent = TriStateFalse
	return in, nil
}

func TestRegisterMCPBatchPolicyTightensHints(t *testing.T) {
	ro := true
	defs := []ToolDescriptor{
		FromMCPDef(mcp.ToolDef{
			Name: "t", InputSchema: map[string]any{},
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: &ro, IdempotentHint: &ro},
		}, Source{Kind: "mcp", Name: "srv"}, "rev"),
	}
	b := NewBuilder()
	if err := RegisterMCPBatch(b, defs, map[string]tools.Tool{"t": &stubTool{name: "t"}}, tighteningPolicy{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	reg, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	desc, _, ok := reg.Get("t")
	if !ok || desc.Idempotent != TriStateFalse {
		t.Fatalf("host policy must tighten idempotent claim: %+v", desc)
	}
	if desc.ReadOnly != TriStateTrue {
		t.Fatalf("host policy must not loosen readonly: %+v", desc)
	}
}

func TestRegisterMCPBatchNameMismatchFails(t *testing.T) {
	defs := []ToolDescriptor{
		FromMCPDef(mcp.ToolDef{Name: "t", InputSchema: map[string]any{}}, Source{Kind: "mcp"}, "rev"),
	}
	b := NewBuilder()
	// Executor registered under a different name.
	err := RegisterMCPBatch(b, defs, map[string]tools.Tool{"other": &stubTool{name: "other"}}, nil)
	if err == nil {
		t.Fatalf("expected name mismatch error")
	}
}

func TestRegisterMCPBatchDuplicateFails(t *testing.T) {
	defs := []ToolDescriptor{
		FromMCPDef(mcp.ToolDef{Name: "t", InputSchema: map[string]any{}}, Source{Kind: "mcp", Name: "a"}, "r1"),
		FromMCPDef(mcp.ToolDef{Name: "t", InputSchema: map[string]any{}}, Source{Kind: "mcp", Name: "b"}, "r2"),
	}
	b := NewBuilder()
	err := RegisterMCPBatch(b, defs, map[string]tools.Tool{"t": &stubTool{name: "t"}}, nil)
	if err == nil {
		t.Fatalf("expected duplicate-name failure")
	}
}

func TestPolicyRejectedStopsBatch(t *testing.T) {
	rejecting := PolicyResolverFunc(func(in ToolDescriptor) (ToolDescriptor, error) {
		return in, ErrPolicyRejected
	})
	defs := []ToolDescriptor{
		FromMCPDef(mcp.ToolDef{Name: "t", InputSchema: map[string]any{}}, Source{Kind: "mcp"}, "rev"),
	}
	b := NewBuilder()
	if err := RegisterMCPBatch(b, defs, map[string]tools.Tool{"t": &stubTool{name: "t"}}, rejecting); err == nil {
		t.Fatalf("expected policy rejection")
	}
}

// PolicyResolverFunc adapts a function to PolicyResolver.
type PolicyResolverFunc func(ToolDescriptor) (ToolDescriptor, error)

func (f PolicyResolverFunc) Resolve(in ToolDescriptor) (ToolDescriptor, error) { return f(in) }

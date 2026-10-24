package client

import (
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/mcp"
)

func TestCatalogRevisionStableForSamePolicy(t *testing.T) {
	defs := []mcp.ToolDef{
		{Name: "a", Description: "desc a", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: boolPtr(true)}},
		{Name: "b"},
	}
	first := CatalogRevision(defs)
	// Same names + hints, different descriptions: revision must not change.
	changed := []mcp.ToolDef{
		{Name: "a", Description: "completely different", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: boolPtr(true)}},
		{Name: "b", Description: "another desc"},
	}
	if second := CatalogRevision(changed); second != first {
		t.Fatalf("description change altered revision: %q != %q", second, first)
	}
}

func TestCatalogRevisionChangesOnHintChange(t *testing.T) {
	base := []mcp.ToolDef{{Name: "a", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: boolPtr(true)}}}
	rev := CatalogRevision(base)

	// Hint flipped: revision must change.
	flipped := []mcp.ToolDef{{Name: "a", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: boolPtr(false)}}}
	if got := CatalogRevision(flipped); got == rev {
		t.Fatalf("hint change did not alter revision")
	}

	// Hint removed (unset): distinct from false, so also a change.
	unset := []mcp.ToolDef{{Name: "a"}}
	if got := CatalogRevision(unset); got == rev || got == CatalogRevision(flipped) {
		t.Fatalf("unset state not distinct: %q %q %q", rev, got, CatalogRevision(flipped))
	}

	// Name change: revision must change.
	renamed := []mcp.ToolDef{{Name: "b", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: boolPtr(true)}}}
	if got := CatalogRevision(renamed); got == rev {
		t.Fatalf("name change did not alter revision")
	}
}

func TestCatalogRevisionOrderSensitive(t *testing.T) {
	a := []mcp.ToolDef{{Name: "a"}, {Name: "b"}}
	b := []mcp.ToolDef{{Name: "b"}, {Name: "a"}}
	if CatalogRevision(a) == CatalogRevision(b) {
		t.Fatalf("tool order should affect revision")
	}
}

func TestChangedSince(t *testing.T) {
	defs := []mcp.ToolDef{{Name: "a"}}
	rev := CatalogRevision(defs)
	if ChangedSince(rev, defs) {
		t.Fatalf("same revision should not be changed")
	}
	if !ChangedSince("", defs) {
		t.Fatalf("empty baseline should count as changed")
	}
	other := []mcp.ToolDef{{Name: "a"}, {Name: "b"}}
	if !ChangedSince(rev, other) {
		t.Fatalf("different tools should count as changed")
	}
}

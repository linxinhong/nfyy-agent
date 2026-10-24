package catalog

import (
	"context"
	"strings"
	"testing"
)

type stubTool struct{ name string }

func (t *stubTool) Name() string           { return t.name }
func (t *stubTool) Description() string    { return "d" }
func (t *stubTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (t *stubTool) Call(ctx context.Context, args map[string]any) (any, error) {
	return nil, nil
}

func localDesc(name string) ToolDescriptor {
	return ToolDescriptor{
		Name:        name,
		Description: "desc " + name,
		InputSchema: map[string]any{"type": "object"},
		ReadOnly:    TriStateTrue,
		Source:      Source{Kind: "local", Name: "builtin", Version: "1"},
	}
}

func TestTriStateValues(t *testing.T) {
	if TriStateUnset != "unset" || TriStateTrue != "true" || TriStateFalse != "false" {
		t.Fatalf("tristate constants drifted")
	}
}

func TestRegistryDuplicateNameFails(t *testing.T) {
	b := NewBuilder()
	if err := b.Add(localDesc("t"), &stubTool{name: "t"}); err != nil {
		t.Fatalf("first add: %v", err)
	}
	err := b.Add(localDesc("t"), &stubTool{name: "t"})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err = %v, want duplicate", err)
	}
}

func TestRegistryMismatchedNameFails(t *testing.T) {
	b := NewBuilder()
	err := b.Add(localDesc("t"), &stubTool{name: "other"})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v, want name mismatch", err)
	}
}

func TestRegistryMissingExecutorFails(t *testing.T) {
	b := NewBuilder()
	if err := b.Add(localDesc("t"), nil); err == nil {
		t.Fatalf("expected missing-executor error")
	}
}

func TestRegistryImmutableSnapshotAndRevision(t *testing.T) {
	b := NewBuilder()
	_ = b.Add(localDesc("b"), &stubTool{name: "b"})
	_ = b.Add(localDesc("a"), &stubTool{name: "a"})
	reg, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	rev := reg.Revision()
	if rev == "" {
		t.Fatalf("empty revision")
	}
	if names := reg.Names(); len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("names = %v", names)
	}
	snap := reg.Snapshot()
	if snap.Descriptors["a"].RegistryRevision != rev {
		t.Fatalf("descriptor revision not filled")
	}
	// Mutating the snapshot must not affect the registry.
	delete(snap.Descriptors, "a")
	if _, _, ok := reg.Get("a"); !ok {
		t.Fatalf("registry mutated through snapshot")
	}
}

func TestRegistryRevisionChangesOnPolicyChange(t *testing.T) {
	build := func(readOnly TriState) string {
		b := NewBuilder()
		d := localDesc("t")
		d.ReadOnly = readOnly
		_ = b.Add(d, &stubTool{name: "t"})
		reg, err := b.Build()
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		return reg.Revision()
	}
	revTrue := build(TriStateTrue)
	revFalse := build(TriStateFalse)
	revUnset := build(TriStateUnset)
	if revTrue == revFalse || revFalse == revUnset || revTrue == revUnset {
		t.Fatalf("policy changes must alter registry revision")
	}
}

func TestRegistryRevisionOrderIndependent(t *testing.T) {
	build := func(first, second string) string {
		b := NewBuilder()
		_ = b.Add(localDesc(second), &stubTool{name: second})
		_ = b.Add(localDesc(first), &stubTool{name: first})
		reg, _ := b.Build()
		return reg.Revision()
	}
	if build("a", "b") != build("b", "a") {
		t.Fatalf("registry revision must be order-independent")
	}
}

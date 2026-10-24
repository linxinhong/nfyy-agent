package catalog

import (
	"errors"

	"github.com/linxinhong/xingding-agent-runtime/pkg/mcp"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

// FromMCPDef maps a remote MCP ToolDef into a ToolDescriptor. Remote hints
// are capability hints only (D-7): unset stays unset and can only tighten a
// host policy later; an explicit false maps to TriStateFalse.
func FromMCPDef(def mcp.ToolDef, source Source, sourceCatalogRevision string) ToolDescriptor {
	desc := ToolDescriptor{
		Name:                  def.Name,
		Description:           def.Description,
		InputSchema:           def.InputSchema,
		OutputSchema:          def.OutputSchema,
		Source:                source,
		SourceCatalogRevision: sourceCatalogRevision,
		ReadOnly:              TriStateUnset,
		Destructive:           TriStateUnset,
		Idempotent:            TriStateUnset,
	}
	if ann := def.Annotations; ann != nil {
		if v := boolToTriState(ann.ReadOnlyHint); v != TriStateUnset {
			desc.ReadOnly = v
		}
		if v := boolToTriState(ann.DestructiveHint); v != TriStateUnset {
			desc.Destructive = v
		}
		if v := boolToTriState(ann.IdempotentHint); v != TriStateUnset {
			desc.Idempotent = v
		}
	}
	return desc
}

func boolToTriState(b *bool) TriState {
	if b == nil {
		return TriStateUnset
	}
	if *b {
		return TriStateTrue
	}
	return TriStateFalse
}

// PolicyResolver supplies the host's registration policy for a descriptor:
// it may tighten remote hints (never loosen) and returns the descriptor's
// DescriptorPolicyRevision. RC-05 wires the real resolver; the identity
// resolver is the fail-closed default.
type PolicyResolver interface {
	// Resolve returns the effective descriptor (with tightened security
	// attributes) and the policy revision that produced it.
	Resolve(in ToolDescriptor) (ToolDescriptor, error)
}

// IdentityPolicyResolver returns the descriptor unchanged with an empty
// policy revision; it does not lower any hint.
type IdentityPolicyResolver struct{}

func (IdentityPolicyResolver) Resolve(in ToolDescriptor) (ToolDescriptor, error) {
	return in, nil
}

// ErrPolicyRejected is returned when host policy rejects a descriptor.
var ErrPolicyRejected = errors.New("catalog: descriptor rejected by host policy")

// RegisterMCPBatch validates and registers a batch of remote descriptors
// into the builder. Executor names must match the descriptor names exactly
// (after any prefixing the host applied). Duplicate names fail fast.
func RegisterMCPBatch(builder *Builder, descriptors []ToolDescriptor, executors map[string]tools.Tool, policy PolicyResolver) error {
	if policy == nil {
		policy = IdentityPolicyResolver{}
	}
	for _, raw := range descriptors {
		desc, err := policy.Resolve(raw)
		if err != nil {
			return err
		}
		executor, ok := executors[desc.Name]
		if !ok {
			return ErrMismatchedName
		}
		if executor.Name() != desc.Name {
			return ErrMismatchedName
		}
		if err := builder.Add(desc, executor); err != nil {
			return err
		}
	}
	return nil
}

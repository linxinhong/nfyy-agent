package client

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/linxinhong/xingding-agent-runtime/pkg/mcp"
)

// CatalogRevision fingerprints the policy-relevant fields of a tools/list
// result: each tool's name plus its three trust hints. Descriptions and
// schema details do not affect the revision, so a revision change means an
// authorization decision could be stale and must be re-evaluated.
func CatalogRevision(defs []mcp.ToolDef) string {
	h := sha256.New()
	for _, d := range defs {
		var ro, de, id *bool
		if d.Annotations != nil {
			ro = d.Annotations.ReadOnlyHint
			de = d.Annotations.DestructiveHint
			id = d.Annotations.IdempotentHint
		}
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\n", d.Name, hintState(ro), hintState(de), hintState(id))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ChangedSince reports whether the current policy fingerprint of defs
// differs from prev. An empty prev is treated as "changed" (no baseline).
func ChangedSince(prev string, defs []mcp.ToolDef) bool {
	return prev != CatalogRevision(defs)
}

// hintState renders a three-state hint: "unset" (nil), "true", or "false".
// The unset state is distinct from false so that "server never declared"
// and "server explicitly denied" produce different fingerprints.
func hintState(b *bool) string {
	if b == nil {
		return "unset"
	}
	if *b {
		return "true"
	}
	return "false"
}

// sourceCatalogCanonicalTool is the canonical, versioned representation of
// one tool for SourceCatalogRevisionV1. Unknown metadata that cannot be
// canonicalized (non-JSON values) would already have failed decode; raw
// JSON metadata is included as-is.
type sourceCatalogCanonicalTool struct {
	Name         string
	Description  string
	InputSchema  map[string]any
	OutputSchema map[string]any
	Annotations  *mcp.ToolAnnotations
	Metadata     map[string]json.RawMessage
}

// SourceCatalogRevisionV1 fingerprints a complete tools/list catalog in
// stable order (tools sorted by name): full input/output schema, three-state
// annotations, source identity, and preserved metadata. The canonical bytes
// use Go's deterministic JSON encoding (map keys sorted, matching JCS key
// ordering for JSON basic types, arrays, and objects). Unlike the legacy
// CatalogRevision, schema and description changes alter this revision.
func SourceCatalogRevisionV1(tools []mcp.ToolDef) string {
	ordered := make([]mcp.ToolDef, len(tools))
	copy(ordered, tools)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })

	canonical := make([]sourceCatalogCanonicalTool, 0, len(ordered))
	for _, t := range ordered {
		canonical = append(canonical, sourceCatalogCanonicalTool{
			Name:         t.Name,
			Description:  t.Description,
			InputSchema:  t.InputSchema,
			OutputSchema: t.OutputSchema,
			Annotations:  t.Annotations,
			Metadata:     t.Metadata,
		})
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		// Canonicalizable by construction (all values are JSON).
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

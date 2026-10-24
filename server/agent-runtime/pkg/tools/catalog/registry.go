// Package catalog defines the transport-independent tool descriptor model
// and the immutable tool Registry that becomes the single chokepoint for
// managed tool execution.
package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

// TriState distinguishes "unset" (never declared) from explicit true/false.
// Host policy treats unset as the most restrictive value; remote hints can
// only tighten, never loosen, a host policy.
type TriState string

const (
	TriStateUnset TriState = "unset"
	TriStateTrue  TriState = "true"
	TriStateFalse TriState = "false"
)

// Source identifies where a descriptor came from (provenance).
type Source struct {
	// Kind is e.g. "local" or "mcp".
	Kind string
	// Name is the source name (e.g. MCP server name).
	Name string
	// Version is the source's version (e.g. server version).
	Version string
}

// ToolDescriptor is the canonical, revision-carrying description of a tool.
type ToolDescriptor struct {
	Name         string
	Description  string
	InputSchema  map[string]any
	OutputSchema map[string]any

	ReadOnly    TriState
	Destructive TriState
	Idempotent  TriState

	Source             Source
	SecurityProvenance string

	// Revision fields (see plan D-8). RegistryRevision is filled at build.
	DescriptorPolicyRevision string
	SchemaRevision           string
	SourceCatalogRevision    string
	RegistryRevision         string
}

// Sentinel errors from the catalog package.
var (
	ErrDuplicateTool   = errors.New("catalog: duplicate tool name")
	ErrMismatchedName  = errors.New("catalog: descriptor name does not match executor name")
	ErrMissingExecutor = errors.New("catalog: executor is required")
	ErrFrozen          = errors.New("catalog: registry is frozen")
)

// RegistrySnapshot is an immutable view of a built Registry.
type RegistrySnapshot struct {
	Descriptors map[string]ToolDescriptor
	Revision    string
}

// Registry holds the frozen set of descriptors and their executors. The
// executor map is private: managed components only ever receive descriptors
// and the ToolService execution path.
type Registry struct {
	mu          sync.RWMutex
	descriptors map[string]ToolDescriptor
	executors   map[string]tools.Tool
	revision    string
}

// Get returns the descriptor and executor for a tool name.
func (r *Registry) Get(name string) (ToolDescriptor, tools.Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.descriptors[name]
	if !ok {
		return ToolDescriptor{}, nil, false
	}
	return d, r.executors[name], true
}

// Snapshot returns an immutable copy of the descriptor set.
func (r *Registry) Snapshot() RegistrySnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	descriptors := make(map[string]ToolDescriptor, len(r.descriptors))
	for name, d := range r.descriptors {
		descriptors[name] = d
	}
	return RegistrySnapshot{Descriptors: descriptors, Revision: r.revision}
}

// Revision returns the frozen RegistryRevisionV1.
func (r *Registry) Revision() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.revision
}

// Names returns the sorted tool names in the registry.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.descriptors))
	for name := range r.descriptors {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Builder collects local and remote descriptors and freezes them into a
// Registry. Duplicate names, mismatched descriptor/executor names, and
// missing executors fail fast; there is no last-write-wins.
type Builder struct {
	mu          sync.Mutex
	descriptors map[string]ToolDescriptor
	executors   map[string]tools.Tool
}

// NewBuilder creates an empty builder.
func NewBuilder() *Builder {
	return &Builder{
		descriptors: make(map[string]ToolDescriptor),
		executors:   make(map[string]tools.Tool),
	}
}

// Add registers a local tool with its descriptor. The executor's Name must
// equal the descriptor Name.
func (b *Builder) Add(desc ToolDescriptor, executor tools.Tool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if executor == nil {
		return fmt.Errorf("%w: %s", ErrMissingExecutor, desc.Name)
	}
	if executor.Name() != desc.Name {
		return fmt.Errorf("%w: descriptor %q vs executor %q", ErrMismatchedName, desc.Name, executor.Name())
	}
	if _, exists := b.descriptors[desc.Name]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateTool, desc.Name)
	}
	b.descriptors[desc.Name] = desc
	b.executors[desc.Name] = executor
	return nil
}

// Build freezes the registry: the descriptor set becomes immutable and the
// aggregate RegistryRevisionV1 is computed over stable sorted descriptors.
func (b *Builder) Build() (*Registry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	revision, err := RegistryRevisionV1(b.descriptors)
	if err != nil {
		return nil, err
	}
	descriptors := make(map[string]ToolDescriptor, len(b.descriptors))
	executors := make(map[string]tools.Tool, len(b.executors))
	for name, d := range b.descriptors {
		copy := d
		copy.RegistryRevision = revision
		descriptors[name] = copy
		executors[name] = b.executors[name]
	}
	return &Registry{descriptors: descriptors, executors: executors, revision: revision}, nil
}

// registryCanonicalTool is the versioned canonical form used by
// RegistryRevisionV1.
type registryCanonicalTool struct {
	Name                     string
	Description              string
	InputSchema              map[string]any
	OutputSchema             map[string]any
	ReadOnly                 TriState
	Destructive              TriState
	Idempotent               TriState
	Source                   Source
	SchemaRevision           string
	DescriptorPolicyRevision string
}

// RegistryRevisionV1 fingerprints the aggregate descriptor snapshot in
// stable (name-sorted) order. Any schema, annotation, or policy change
// alters the revision, invalidating prepared plans and approvals that
// reference it.
func RegistryRevisionV1(descriptors map[string]ToolDescriptor) (string, error) {
	names := make([]string, 0, len(descriptors))
	for name := range descriptors {
		names = append(names, name)
	}
	sort.Strings(names)
	canonical := make([]registryCanonicalTool, 0, len(names))
	for _, name := range names {
		d := descriptors[name]
		canonical = append(canonical, registryCanonicalTool{
			Name:                     d.Name,
			Description:              d.Description,
			InputSchema:              d.InputSchema,
			OutputSchema:             d.OutputSchema,
			ReadOnly:                 d.ReadOnly,
			Destructive:              d.Destructive,
			Idempotent:               d.Idempotent,
			Source:                   d.Source,
			SchemaRevision:           d.SchemaRevision,
			DescriptorPolicyRevision: d.DescriptorPolicyRevision,
		})
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("catalog: canonicalize registry: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

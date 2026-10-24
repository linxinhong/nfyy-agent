// Package memory defines the strict HTTP schema for project-scoped memory.
package memory

import mem "github.com/linxinhong/xingding-agent-runtime/pkg/memory"

type IssueTokenRequest struct {
	Action mem.Action `json:"action"`
}

type CreateMemoryRequest struct {
	Content     string   `json:"content"`
	Topics      []string `json:"topics,omitempty"`
	Kind        mem.Kind `json:"kind"`
	RetentionMS int64    `json:"retention_ms,omitempty"`
}

type ReviseMemoryRequest struct {
	ExpectedRevision uint64   `json:"expected_revision"`
	Content          string   `json:"content"`
	Topics           []string `json:"topics,omitempty"`
	RetentionMS      int64    `json:"retention_ms,omitempty"`
}

type ConfirmMemoryRequest struct {
	ExpectedRevision         uint64 `json:"expected_revision"`
	ReplacedMemoryID         string `json:"replaced_memory_id,omitempty"`
	ExpectedReplacedRevision uint64 `json:"expected_replaced_revision,omitempty"`
	SourceRef                string `json:"source_ref"`
	SourceEpoch              uint64 `json:"source_epoch"`
}

// ProposeMemoryRequest contains only an opaque host-resolved source selector.
// Attestations and outbound fragments are never accepted from clients.
type ProposeMemoryRequest struct {
	SourceRef string `json:"source_ref"`
}

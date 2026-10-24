// Package compaction provides composable compaction strategies that reduce
// conversation context in a message-index data structure. Strategies are
// designed to be combined via PipelineStrategy and reused across agents.
package compaction

import "github.com/linxinhong/xingding-agent-runtime/pkg/models"

// GroupKind identifies the role a message group plays during compaction.
type GroupKind int

const (
	GroupKindSystem        GroupKind = iota // one or more system messages
	GroupKindUser                           // a single user message
	GroupKindAssistantText                  // assistant text (no tool calls)
	GroupKindToolCall                       // assistant tool call + tool results
	GroupKindSummary                        // summary produced by compaction
)

const summaryPropertyKey = "_is_summary"

// MessageGroup represents a logical group of messages that must be kept or
// removed together. Groups preserve atomic relationships such as an assistant
// tool call and its corresponding tool-result messages.
type MessageGroup struct {
	Kind         GroupKind
	Messages     []models.Message
	MessageCount int
	ByteCount    int
	TokenCount   int
	TurnIndex    *int `json:",omitzero"`

	// IsExcluded indicates the group is omitted from the projected list.
	IsExcluded bool
	// ExcludeReason optionally explains why.
	ExcludeReason string `json:",omitzero"`
}

func newMessageGroup(kind GroupKind, messages []models.Message, byteCount, tokenCount int, turnIndex *int) *MessageGroup {
	return &MessageGroup{
		Kind:         kind,
		Messages:     messages,
		MessageCount: len(messages),
		ByteCount:    byteCount,
		TokenCount:   tokenCount,
		TurnIndex:    cloneTurnIndex(turnIndex),
	}
}

func cloneTurnIndex(ti *int) *int {
	if ti == nil {
		return nil
	}
	v := *ti
	return &v
}

package compaction

import (
	"encoding/json"
	"slices"
	"unicode/utf8"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

// TokenCounter counts tokens for text content.
type TokenCounter interface {
	// CountTokens returns the token count for the text.
	CountTokens(text string) int
}

// ApproximateTokenCounter estimates tokens from byte and rune counts.
type ApproximateTokenCounter struct{}

// CountTokens estimates with max(byteCount/4, runeCount).
func (ApproximateTokenCounter) CountTokens(text string) int {
	byteEst := (len(text) + 3) / 4
	runeEst := utf8.RuneCountInString(text)
	if runeEst > byteEst {
		return runeEst
	}
	return byteEst
}

// MessageIndex groups a flat message list into atomic units and tracks
// compaction metrics. Groups may be marked as excluded without removal.
type MessageIndex struct {
	Groups       []*MessageGroup
	TokenCounter TokenCounter `json:"-"`

	currentTurn          int
	lastProcessedMessage *models.Message
}

// NewMessageIndex creates an index from pre-built groups.
func NewMessageIndex(groups []*MessageGroup, counter TokenCounter) *MessageIndex {
	idx := &MessageIndex{
		Groups:       groups,
		TokenCounter: counter,
	}
	for i := len(groups) - 1; i >= 0; i-- {
		g := groups[i]
		if idx.lastProcessedMessage == nil && g.Kind != GroupKindSummary && len(g.Messages) > 0 {
			idx.lastProcessedMessage = &g.Messages[len(g.Messages)-1]
		}
		if g.TurnIndex != nil {
			idx.currentTurn = *g.TurnIndex
			if idx.lastProcessedMessage != nil {
				break
			}
		}
	}
	return idx
}

// CreateMessageIndex creates an index from a flat message list.
func CreateMessageIndex(messages []models.Message, counter TokenCounter) *MessageIndex {
	idx := NewMessageIndex(nil, counter)
	idx.appendFromMessages(messages, 0)
	return idx
}

func (idx *MessageIndex) appendFromMessages(messages []models.Message, start int) {
	for i := start; i < len(messages); i++ {
		msg := messages[i]
		switch {
		case msg.Role == models.RoleSystem:
			idx.Groups = append(idx.Groups, idx.newGroup(GroupKindSystem, []models.Message{msg}, nil))
		case msg.Role == models.RoleUser:
			idx.currentTurn++
			turn := idx.currentTurn
			idx.Groups = append(idx.Groups, idx.newGroup(GroupKindUser, []models.Message{msg}, &turn))
		case msg.Role == models.RoleAssistant && len(msg.ToolCalls) > 0:
			turn := idx.currentTurn
			grp := []models.Message{msg}
			i++
			for i < len(messages) && messages[i].Role == models.RoleTool {
				grp = append(grp, messages[i])
				i++
			}
			i-- // loop will advance
			idx.Groups = append(idx.Groups, idx.newGroup(GroupKindToolCall, grp, &turn))
		case msg.Role == models.RoleAssistant && isSummaryMsg(msg):
			turn := idx.currentTurn
			idx.Groups = append(idx.Groups, idx.newGroup(GroupKindSummary, []models.Message{msg}, &turn))
		default:
			turn := idx.currentTurn
			idx.Groups = append(idx.Groups, idx.newGroup(GroupKindAssistantText, []models.Message{msg}, &turn))
		}
	}
	if len(messages) > 0 {
		idx.lastProcessedMessage = &messages[len(messages)-1]
	}
}

// InsertGroup inserts a group at the given index.
func (idx *MessageIndex) InsertGroup(at int, kind GroupKind, messages []models.Message, turnIndex *int) *MessageGroup {
	g := idx.newGroup(kind, messages, turnIndex)
	idx.Groups = slices.Insert(idx.Groups, at, g)
	return g
}

// AddGroup appends a group.
func (idx *MessageIndex) AddGroup(kind GroupKind, messages []models.Message, turnIndex *int) *MessageGroup {
	g := idx.newGroup(kind, messages, turnIndex)
	idx.Groups = append(idx.Groups, g)
	return g
}

// IncludedMessages returns messages from non-excluded groups.
func (idx *MessageIndex) IncludedMessages() []models.Message {
	var msgs []models.Message
	for _, g := range idx.Groups {
		if !g.IsExcluded {
			msgs = append(msgs, g.Messages...)
		}
	}
	return msgs
}

// IncludedGroupCount returns the number of non-excluded groups.
func (idx *MessageIndex) IncludedGroupCount() int {
	n := 0
	for _, g := range idx.Groups {
		if !g.IsExcluded {
			n++
		}
	}
	return n
}

// IncludedMessageCount returns the message count in non-excluded groups.
func (idx *MessageIndex) IncludedMessageCount() int {
	n := 0
	for _, g := range idx.Groups {
		if !g.IsExcluded {
			n += g.MessageCount
		}
	}
	return n
}

// IncludedByteCount returns the byte count in non-excluded groups.
func (idx *MessageIndex) IncludedByteCount() int {
	n := 0
	for _, g := range idx.Groups {
		if !g.IsExcluded {
			n += g.ByteCount
		}
	}
	return n
}

// IncludedTokenCount returns the token count in non-excluded groups.
func (idx *MessageIndex) IncludedTokenCount() int {
	n := 0
	for _, g := range idx.Groups {
		if !g.IsExcluded {
			n += g.TokenCount
		}
	}
	return n
}

// IncludedTurnCount returns the number of distinct user turns with ≥1 non-excluded group.
func (idx *MessageIndex) IncludedTurnCount() int {
	seen := map[int]bool{}
	for _, g := range idx.Groups {
		if !g.IsExcluded && g.TurnIndex != nil && *g.TurnIndex > 0 {
			seen[*g.TurnIndex] = true
		}
	}
	return len(seen)
}

// IncludedNonSystemGroupCount returns non-excluded, non-system groups.
func (idx *MessageIndex) IncludedNonSystemGroupCount() int {
	n := 0
	for _, g := range idx.Groups {
		if !g.IsExcluded && g.Kind != GroupKindSystem {
			n++
		}
	}
	return n
}

func (idx *MessageIndex) newGroup(kind GroupKind, messages []models.Message, turn *int) *MessageGroup {
	bc := computeByteCount(messages)
	tc := bc / 4
	if idx.TokenCounter != nil {
		tc = computeTokenCount(messages, idx.TokenCounter)
	}
	return newMessageGroup(kind, messages, bc, tc, turn)
}

func computeByteCount(messages []models.Message) int {
	total := 0
	for _, m := range messages {
		total += len(m.Content)
		total += len(m.ReasoningContent)
		for _, tc := range m.ToolCalls {
			total += len(tc.ID) + len(tc.Function.Name) + len(tc.Function.Arguments)
		}
		for _, img := range m.Images {
			total += len(img.URL) + len(img.MimeType) + len(img.Base64)
		}
	}
	return total
}

func computeTokenCount(messages []models.Message, counter TokenCounter) int {
	total := 0
	for _, m := range messages {
		total += counter.CountTokens(m.Content)
		total += counter.CountTokens(m.ReasoningContent)
	}
	return total
}

func isSummaryMsg(msg models.Message) bool {
	if msg.AdditionalProperties == nil {
		return false
	}
	v, ok := msg.AdditionalProperties[summaryPropertyKey]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// messageContentEqual checks two messages for equality by comparing their
// JSON representations. Used to detect changes in the processed prefix.
func messageContentEqual(a, b *models.Message) bool {
	if a == nil || b == nil {
		return a == b
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

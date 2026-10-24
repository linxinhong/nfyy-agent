package compaction

import (
	"context"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

func msg(role models.Role, content string) models.Message {
	return models.Message{Role: role, Content: content}
}

func userMsg(content string) models.Message {
	return msg(models.RoleUser, content)
}

func asstMsg(content string) models.Message {
	return msg(models.RoleAssistant, content)
}

func sysMsg(content string) models.Message {
	return msg(models.RoleSystem, content)
}

func toolCallMsg(name, args, callID string) models.Message {
	return models.Message{
		Role: models.RoleAssistant,
		ToolCalls: []models.ToolCall{
			{ID: callID, Function: models.FunctionCall{Name: name, Arguments: args}},
		},
	}
}

func toolResultMsg(callID, name, content string) models.Message {
	return models.Message{
		Role:       models.RoleTool,
		ToolCallID: callID,
		Name:       name,
		Content:    content,
	}
}

// ---------- MessageIndex tests ----------

func TestMessageIndex_GroupsSystemUserAssistant(t *testing.T) {
	messages := []models.Message{
		sysMsg("You are helpful."),
		userMsg("hello"),
		asstMsg("hi there"),
		userMsg("help"),
		asstMsg("sure"),
	}

	idx := CreateMessageIndex(messages, nil)
	if idx.IncludedGroupCount() != 5 {
		t.Fatalf("groups = %d, want 5", idx.IncludedGroupCount())
	}
	if idx.Groups[0].Kind != GroupKindSystem {
		t.Fatalf("group 0 kind = %v, want GroupKindSystem", idx.Groups[0].Kind)
	}
	if idx.Groups[1].Kind != GroupKindUser {
		t.Fatalf("group 1 kind = %v, want GroupKindUser", idx.Groups[1].Kind)
	}
	if idx.Groups[2].Kind != GroupKindAssistantText {
		t.Fatalf("group 2 kind = %v, want GroupKindAssistantText", idx.Groups[2].Kind)
	}
}

func TestMessageIndex_GroupsToolCalls(t *testing.T) {
	messages := []models.Message{
		userMsg("calc 1+1"),
		toolCallMsg("add", `{"a":1,"b":1}`, "call1"),
		toolResultMsg("call1", "add", "2"),
		asstMsg("result is 2"),
	}

	idx := CreateMessageIndex(messages, nil)
	if idx.IncludedGroupCount() != 3 {
		t.Fatalf("groups = %d, want 3 (user, tool-call+result, assistant)", idx.IncludedGroupCount())
	}
	if idx.Groups[1].Kind != GroupKindToolCall {
		t.Fatalf("group 1 kind = %v, want GroupKindToolCall", idx.Groups[1].Kind)
	}
	if idx.Groups[1].MessageCount != 2 {
		t.Fatalf("tool call group message count = %d, want 2", idx.Groups[1].MessageCount)
	}
}

func TestMessageIndex_IncludedMessages(t *testing.T) {
	messages := []models.Message{
		sysMsg("system"),
		userMsg("hello"),
		asstMsg("hi"),
	}

	idx := CreateMessageIndex(messages, nil)
	idx.Groups[1].IsExcluded = true

	included := idx.IncludedMessages()
	if len(included) != 2 {
		t.Fatalf("included = %d, want 2", len(included))
	}
	if included[0].Content != "system" || included[1].Content != "hi" {
		t.Fatalf("included = %v", included)
	}
}

// ---------- Trigger tests ----------

func TestTrigger_TokensExceed(t *testing.T) {
	messages := []models.Message{
		userMsg("hello world this is a longer message"),
		asstMsg("response"),
	}
	idx := CreateMessageIndex(messages, &ApproximateTokenCounter{})

	if TokensExceed(1000)(idx) {
		t.Fatal("tokens should not exceed 1000")
	}
	if !TokensExceed(0)(idx) {
		t.Fatal("tokens should exceed 0")
	}
}

func TestTrigger_All(t *testing.T) {
	messages := []models.Message{userMsg("hi"), asstMsg("hey")}
	idx := CreateMessageIndex(messages, &ApproximateTokenCounter{})

	all := All(TokensExceed(0), GroupsExceed(0))
	if !all(idx) {
		t.Fatal("All should fire when both triggers fire")
	}

	all2 := All(TokensExceed(10000), GroupsExceed(0))
	if all2(idx) {
		t.Fatal("All should not fire when one trigger fails")
	}
}

func TestTrigger_Any(t *testing.T) {
	messages := []models.Message{userMsg("hi"), asstMsg("hey")}
	idx := CreateMessageIndex(messages, nil)

	any := Any(TokensExceed(10000), GroupsExceed(0))
	if !any(idx) {
		t.Fatal("Any should fire when at least one trigger fires")
	}

	any2 := Any(TokensExceed(10000), GroupsExceed(10000))
	if any2(idx) {
		t.Fatal("Any should not fire when no trigger fires")
	}
}

// ---------- Pipeline tests ----------

func TestPipelineStrategy_Ordered(t *testing.T) {
	order := []string{}
	s1 := StrategyFunc(func(ctx context.Context, idx *MessageIndex) (bool, error) {
		order = append(order, "s1")
		return true, nil
	})
	s2 := StrategyFunc(func(ctx context.Context, idx *MessageIndex) (bool, error) {
		order = append(order, "s2")
		return false, nil
	})

	pipeline := &PipelineStrategy{Strategies: []Strategy{s1, s2}}
	idx := CreateMessageIndex([]models.Message{userMsg("a"), asstMsg("b")}, nil)
	compacted, err := pipeline.Compact(context.Background(), idx)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !compacted {
		t.Fatal("expected compacted = true from s1")
	}
	if len(order) != 2 || order[0] != "s1" || order[1] != "s2" {
		t.Fatalf("order = %v, want [s1 s2]", order)
	}
}

// ---------- Truncation tests ----------

func TestTruncationStrategy(t *testing.T) {
	messages := []models.Message{
		sysMsg("sys"),
		userMsg("turn1"),
		asstMsg("a1"),
		userMsg("turn2"),
		asstMsg("a2"),
		userMsg("turn3"),
		asstMsg("a3"),
	}
	idx := CreateMessageIndex(messages, nil)

	strategy := &TruncationStrategy{
		Trigger:                Always(),
		MinimumPreservedGroups: 2,
	}
	compacted, err := strategy.Compact(context.Background(), idx)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !compacted {
		t.Fatal("expected compaction")
	}

	included := idx.IncludedMessages()
	// System + last 2 non-system groups (turn2+turn3)
	if included[0].Content != "sys" {
		t.Fatalf("system not preserved")
	}
}

func TestTruncationStrategy_NoTrigger(t *testing.T) {
	messages := []models.Message{userMsg("hi"), asstMsg("hey")}
	idx := CreateMessageIndex(messages, nil)

	strategy := &TruncationStrategy{
		Trigger: Never(),
	}
	compacted, err := strategy.Compact(context.Background(), idx)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if compacted {
		t.Fatal("should not compact when trigger is Never")
	}
}

// ---------- SlidingWindow tests ----------

func TestSlidingWindowStrategy(t *testing.T) {
	messages := []models.Message{
		sysMsg("sys"),
		userMsg("turn1"),
		asstMsg("a1"),
		userMsg("turn2"),
		asstMsg("a2"),
		userMsg("turn3"),
		asstMsg("a3"),
	}
	idx := CreateMessageIndex(messages, nil)

	strategy := &SlidingWindowStrategy{
		Trigger:               Always(),
		MinimumPreservedTurns: 1,
	}
	compacted, err := strategy.Compact(context.Background(), idx)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !compacted {
		t.Fatal("expected compaction")
	}

	included := idx.IncludedMessages()
	if included[0].Content != "sys" {
		t.Fatalf("system not preserved, got %q", included[0].Content)
	}
	// Last turn (turn3 + a3) should be preserved.
	found := false
	for _, m := range included {
		if m.Content == "turn3" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("turn3 not preserved in %v", included)
	}
}

// ---------- ToolResult tests ----------

func TestToolResultStrategy(t *testing.T) {
	messages := []models.Message{
		sysMsg("sys"),
		userMsg("query"),
		toolCallMsg("search", `{"q":"x"}`, "c1"),
		toolResultMsg("c1", "search", "found x"),
		asstMsg("found it"),
		userMsg("q2"),
		asstMsg("a2"),
		userMsg("q3"),
		asstMsg("a3"),
	}
	idx := CreateMessageIndex(messages, nil)

	strategy := &ToolResultStrategy{
		Trigger:                Always(),
		MinimumPreservedGroups: 3,
	}
	compacted, err := strategy.Compact(context.Background(), idx)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !compacted {
		t.Fatal("expected compaction of tool result")
	}
	// The tool-call group should be excluded and a summary inserted.
	hasSummary := false
	for _, g := range idx.Groups {
		if g.Kind == GroupKindSummary {
			hasSummary = true
		}
	}
	if !hasSummary {
		t.Fatal("expected summary group after tool result compaction")
	}
}

// ---------- Summarization tests ----------

func TestSummarizationStrategy_NoSummarizer(t *testing.T) {
	messages := []models.Message{
		userMsg("q1"), asstMsg("a1"),
		userMsg("q2"), asstMsg("a2"),
	}
	idx := CreateMessageIndex(messages, nil)

	strategy := &SummarizationStrategy{
		Trigger: Always(),
	}
	compacted, err := strategy.Compact(context.Background(), idx)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if compacted {
		t.Fatal("should not compact without summarizer")
	}
}

func TestSummarizationStrategy(t *testing.T) {
	messages := []models.Message{
		userMsg("q1"), asstMsg("a1"),
		userMsg("q2"), asstMsg("a2"),
		userMsg("q3"), asstMsg("a3"),
	}
	idx := CreateMessageIndex(messages, nil)

	summarized := ""
	strategy := &SummarizationStrategy{
		Trigger:                Always(),
		Summarizer:             SummarizerFunc(func(ctx context.Context, msgs []models.Message) (string, error) { return "summary", nil }),
		MinimumPreservedGroups: 1,
	}
	compacted, err := strategy.Compact(context.Background(), idx)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !compacted {
		t.Fatal("expected compaction")
	}
	_ = summarized
}

// ---------- ContextWindow tests ----------

func TestContextWindowStrategy(t *testing.T) {
	messages := []models.Message{
		sysMsg("sys"),
		userMsg("hello"),
		asstMsg("hi there, this is a longer response to increase token count"),
	}
	idx := CreateMessageIndex(messages, &ApproximateTokenCounter{})

	strategy := &ContextWindowStrategy{
		MaxContextWindowTokens: 10,
		MaxOutputTokens:        2,
	}
	compacted, err := strategy.Compact(context.Background(), idx)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	// Budget is 10-2=8, tool eviction at 4, truncation at 6.4
	// Small messages might not trigger anything.
	_ = compacted
}

func TestContextWindowStrategy_InvalidConfig(t *testing.T) {
	idx := CreateMessageIndex(nil, nil)

	strategy := &ContextWindowStrategy{
		MaxContextWindowTokens: 0,
	}
	_, err := strategy.Compact(context.Background(), idx)
	if err == nil {
		t.Fatal("expected error for zero context window")
	}
}

// ---------- Compact helper tests ----------

func TestCompact(t *testing.T) {
	messages := []models.Message{
		sysMsg("sys"),
		userMsg("turn1"), asstMsg("a1"),
		userMsg("turn2"), asstMsg("a2"),
	}

	strategy := &TruncationStrategy{
		Trigger:                Always(),
		MinimumPreservedGroups: 1,
	}

	result, err := Compact(context.Background(), strategy, messages, nil)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(result) == len(messages) {
		t.Fatal("expected messages to be reduced")
	}
}

// ---------- StrategyFunc adapter ----------

// StrategyFunc adapts a function to the Strategy interface.
type StrategyFunc func(context.Context, *MessageIndex) (bool, error)

func (f StrategyFunc) Compact(ctx context.Context, idx *MessageIndex) (bool, error) {
	return f(ctx, idx)
}

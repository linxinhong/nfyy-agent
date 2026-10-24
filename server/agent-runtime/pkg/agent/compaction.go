package agent

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent/compaction"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

// ErrContextBudgetExceeded is returned when the assembled context cannot fit
// within the configured MaxInputTokens without removing protected content
// (system, current input, or MinRecentTurns).
var ErrContextBudgetExceeded = errors.New("context budget exceeded: cannot fit protected content within MaxInputTokens")

// ErrInvalidContextWindowPolicy is returned when a ContextWindowPolicy has
// invalid configuration (negative max, out-of-range ratios, or target >= trigger).
var ErrInvalidContextWindowPolicy = errors.New("invalid context window policy")

// TokenCounter estimates the number of tokens in a message list.
type TokenCounter interface {
	Count(ctx context.Context, messages []models.Message) (int, error)
}

// Summarizer condenses a message list into a short summary string.
type Summarizer interface {
	Summarize(ctx context.Context, messages []models.Message) (string, error)
}

// ContextWindowPolicy budgets the assembled context by token count. MaxInputTokens
// is the hard cap. When the token count reaches TriggerRatio * MaxInputTokens,
// compaction reduces it to TargetRatio * MaxInputTokens. At least MinRecentTurns
// are always preserved from the tail, and system + current input are never removed.
// A MaxInputTokens of zero disables compaction.
type ContextWindowPolicy struct {
	MaxInputTokens int
	TriggerRatio   float64
	TargetRatio    float64
	MinRecentTurns int
}

// ApproximateTokenCounter is a built-in TokenCounter that estimates token
// count from message content, role, tool calls, tool results, and multimodal
// attachments. It is conservative enough for budgeting; precise implementations
// may be injected.
type ApproximateTokenCounter struct{}

// Count estimates text with the larger of UTF-8 bytes/4 and rune count, then
// adds structural overhead for roles, tool calls/results, and attachments.
func (ApproximateTokenCounter) Count(ctx context.Context, messages []models.Message) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	total := 0
	for _, m := range messages {
		total += approximateTextTokens(m.Content)
		// Role overhead.
		total += 1
		// Tool calls: ID + name + arguments.
		for _, tc := range m.ToolCalls {
			total += approximateTextTokens(tc.ID + tc.Function.Name + tc.Function.Arguments)
			total += 1
		}
		// Tool result ID + name.
		if m.Role == models.RoleTool {
			total += approximateTextTokens(m.ToolCallID + m.Name)
			total += 1
		}
		// Multimodal attachments: URL + mime type.
		for _, img := range m.Images {
			total += approximateTextTokens(img.URL + img.MimeType + img.Base64)
			total += 1
		}
		for _, a := range m.Audios {
			total += approximateTextTokens(a.URL + a.MimeType + a.Base64)
			total += 1
		}
		for _, v := range m.Videos {
			total += approximateTextTokens(v.URL + v.MimeType + v.Base64)
			total += 1
		}
		for _, f := range m.Files {
			total += approximateTextTokens(f.URL + f.MimeType + f.Base64 + f.Name)
			total += 1
		}
		if total < 0 {
			total = 1<<31 - 1
		}
	}
	return total, nil
}

func approximateTextTokens(value string) int {
	byteEstimate := (len(value) + 3) / 4
	runeEstimate := utf8.RuneCountInString(value)
	if runeEstimate > byteEstimate {
		return runeEstimate
	}
	return byteEstimate
}

// msgProvenance tracks which segment a message belongs to.
type msgProvenance struct {
	msg     models.Message
	segment int // 0=system, 1=history, 2=current input/run, 3=ephemeral
	runIdx  int // for history messages
}

const (
	segmentSystem  = 0
	segmentHistory = 1
	segmentInput   = 2
	// segmentEphemeral is per-request context between history and input;
	// like system it is protected from compaction.
	segmentEphemeral = 3
)

// CompactionContextProvider is the built-in ContextProvider that applies a
// ContextWindowPolicy to keep the assembled context within token budget. A nil
// Policy (or nil receiver) makes Apply an identity transform.
type CompactionContextProvider struct {
	Policy     *ContextWindowPolicy
	Counter    TokenCounter
	Summarizer Summarizer
	// Pipeline is an optional composable compaction pipeline. When non-nil, it
	// replaces the default fold→summarize→truncate behavior. Use
	// compaction.PipelineStrategy to compose multiple strategies, or
	// compaction.ContextWindowStrategy for the standard two-phase pipeline.
	Pipeline compaction.Strategy
}

// ApplyBeforeModelCall rechecks the hard context budget after tool results have
// extended the in-progress run.
func (p *CompactionContextProvider) ApplyBeforeModelCall(ctx context.Context, req ContextRequest) (ContextRequest, error) {
	return p.Apply(ctx, req)
}

// Apply compacts the context if it reaches the trigger threshold. It never
// removes system or current input, and preserves at least MinRecentTurns
// (with a hard floor of 2 turns per the roadmap).
func (p *CompactionContextProvider) Apply(ctx context.Context, req ContextRequest) (ContextRequest, error) {
	if p == nil || p.Policy == nil || p.Policy.MaxInputTokens == 0 {
		return req, nil
	}
	if err := ctx.Err(); err != nil {
		return req, err
	}

	// Validate policy configuration.
	if p.Policy.MaxInputTokens < 0 {
		return req, fmt.Errorf("%w: max_input_tokens must not be negative", ErrInvalidContextWindowPolicy)
	}
	if p.Policy.TriggerRatio < 0 || p.Policy.TriggerRatio > 1 {
		return req, fmt.Errorf("%w: trigger_ratio must be in [0, 1]", ErrInvalidContextWindowPolicy)
	}
	if p.Policy.TargetRatio < 0 || p.Policy.TargetRatio > 1 {
		return req, fmt.Errorf("%w: target_ratio must be in [0, 1]", ErrInvalidContextWindowPolicy)
	}

	triggerRatio := p.Policy.TriggerRatio
	if triggerRatio == 0 {
		triggerRatio = 0.8
	}
	targetRatio := p.Policy.TargetRatio
	if targetRatio == 0 {
		targetRatio = 0.7
	}
	if targetRatio >= triggerRatio {
		return req, fmt.Errorf("%w: target_ratio must be less than trigger_ratio", ErrInvalidContextWindowPolicy)
	}

	counter := p.Counter
	if counter == nil {
		counter = &ApproximateTokenCounter{}
	}

	// Flatten all messages with provenance tracking.
	currentCount := len(req.Current)
	if currentCount == 0 {
		currentCount = 1
	}
	all := make([]msgProvenance, 0, len(req.System)+len(req.History)+len(req.Ephemeral)+currentCount)
	for _, m := range req.System {
		all = append(all, msgProvenance{msg: m, segment: segmentSystem})
	}
	for ri, r := range req.History {
		for _, m := range r.Messages {
			all = append(all, msgProvenance{msg: m, segment: segmentHistory, runIdx: ri})
		}
	}
	for _, m := range req.Ephemeral {
		all = append(all, msgProvenance{msg: m, segment: segmentEphemeral})
	}
	if len(req.Current) > 0 {
		for _, message := range req.Current {
			all = append(all, msgProvenance{msg: message, segment: segmentInput})
		}
	} else {
		all = append(all, msgProvenance{msg: req.Input, segment: segmentInput})
	}

	// Count tokens.
	msgs := provenanceToMessages(all)
	tokens, err := counter.Count(ctx, msgs)
	if err != nil {
		return req, err
	}
	recordContextTokens(ctx, tokens, tokens, false)

	trigger := float64(p.Policy.MaxInputTokens) * triggerRatio
	// P0-4: trigger when reaching the threshold (inclusive).
	if float64(tokens) < trigger {
		return req, nil
	}
	recordContextTokens(ctx, tokens, tokens, true)

	// Compact: fold tool results → summarize → truncate.
	target := float64(p.Policy.MaxInputTokens) * targetRatio

	compacted, err := p.compact(ctx, all, counter, target)
	if err != nil {
		return req, err
	}

	// Rebuild ContextRequest.
	result, err := rebuildRequest(compacted, req)
	if err != nil {
		return req, err
	}

	// Verify final budget.
	finalTokens, err := counter.Count(ctx, provenanceToMessages(compacted))
	if err != nil {
		return req, err
	}
	recordContextTokens(ctx, tokens, finalTokens, false)
	if finalTokens > p.Policy.MaxInputTokens {
		return req, budgetExceededError(finalTokens, p.Policy.MaxInputTokens)
	}
	return result, nil
}

func recordContextTokens(ctx context.Context, original, final int, compacted bool) {
	metrics := metricsFromContext(ctx)
	if metrics == nil {
		return
	}
	metrics.OriginalContextTokens = original
	metrics.FinalContextTokens = final
	if compacted {
		metrics.ContextCompactions++
		contextCompactions.Add(context.WithoutCancel(ctx), 1)
	}
}

// compact applies the compaction pipeline and returns surviving provenance entries.
func (p *CompactionContextProvider) compact(ctx context.Context, all []msgProvenance, counter TokenCounter, target float64) ([]msgProvenance, error) {
	if p.Pipeline != nil {
		return p.compactViaPipeline(ctx, all, target)
	}

	minRecent := p.Policy.MinRecentTurns
	if minRecent < 2 {
		minRecent = 2 // roadmap hard floor: at least 2 recent turns
	}

	// Step 1: Fold old tool results (truncate content).
	folded := p.foldToolResults(all, minRecent)

	// Step 2: Optionally summarize old turns.
	if p.Summarizer != nil {
		var err error
		folded, err = p.summarizeOldTurns(ctx, folded, minRecent, counter)
		if err != nil {
			return nil, err
		}
	}

	// Step 3: Truncate at atomic group boundaries until under target.
	truncated, err := p.truncateToBudget(ctx, folded, counter, target, minRecent)
	if err != nil {
		return nil, err
	}

	return truncated, nil
}

// compactViaPipeline creates a MessageIndex from provenances, runs the
// configured pipeline strategy, and maps surviving messages back to
// provenance entries. Summary messages produced by compaction receive
// segmentSystem so they appear before history in the final assembly.
func (p *CompactionContextProvider) compactViaPipeline(ctx context.Context, all []msgProvenance, target float64) ([]msgProvenance, error) {
	msgs := provenanceToMessages(all)
	idxCounter := &compaction.ApproximateTokenCounter{}
	idx := compaction.CreateMessageIndex(msgs, idxCounter)

	compacted, err := p.Pipeline.Compact(ctx, idx)
	if err != nil {
		return nil, err
	}
	_ = compacted

	// Map included messages back to provenances. For messages that existed
	// in the original list, preserve their provenance. Summary messages
	// produced by compaction get segmentSystem.
	included := idx.IncludedMessages()
	result := make([]msgProvenance, 0, len(included))
	origIndex := 0
	for _, m := range included {
		// Try to find the matching original message.
		found := false
		for origIndex < len(all) {
			orig := all[origIndex]
			if messagesEqual(orig.msg, m) {
				result = append(result, orig)
				origIndex++
				found = true
				break
			}
			// Skip excluded messages.
			origIndex++
		}
		if !found {
			// Summary or synthesized message: place in system segment.
			if m.Content != "" {
				result = append(result, msgProvenance{
					msg:     m,
					segment: segmentSystem,
				})
			}
		}
	}

	// Check final budget. Fall back to the default counter so a nil
	// provider Counter cannot panic (p.Counter is only an optional
	// override for the default fold→summarize→truncate path).
	counter := p.Counter
	if counter == nil {
		counter = &ApproximateTokenCounter{}
	}
	finalMsgs := provenanceToMessages(result)
	finalTokens, err := counter.Count(ctx, finalMsgs)
	if err != nil {
		return nil, err
	}
	if finalTokens > p.Policy.MaxInputTokens {
		return nil, budgetExceededError(finalTokens, p.Policy.MaxInputTokens)
	}
	return result, nil
}

func messagesEqual(a, b models.Message) bool {
	if a.Role != b.Role {
		return false
	}
	if a.Content != b.Content {
		return false
	}
	if a.ToolCallID != b.ToolCallID {
		return false
	}
	if a.Name != b.Name {
		return false
	}
	if len(a.ToolCalls) != len(b.ToolCalls) {
		return false
	}
	for i := range a.ToolCalls {
		if a.ToolCalls[i].ID != b.ToolCalls[i].ID {
			return false
		}
	}
	return true
}

// foldToolResults truncates tool result content for messages not in the
// most recent minRecent turns. Old messages (before keepFrom) are folded.
func (p *CompactionContextProvider) foldToolResults(all []msgProvenance, minRecent int) []msgProvenance {
	turnStart := findTurnBoundaries(all)
	// Find first history message.
	firstHistory := 0
	for firstHistory < len(all) && all[firstHistory].segment != segmentHistory {
		firstHistory++
	}
	// Protect min(len(turnStart), minRecent) recent history turns from folding.
	var keepFrom int
	if len(turnStart) >= minRecent {
		keepFrom = turnStart[len(turnStart)-minRecent]
	} else if len(turnStart) > 0 {
		keepFrom = turnStart[0]
	} else {
		keepFrom = firstHistory
	}

	out := make([]msgProvenance, len(all))
	copy(out, all)
	for i := range out {
		// Fold old tool results: history messages before keepFrom.
		if out[i].segment == segmentHistory && out[i].msg.Role == models.RoleTool && i < keepFrom {
			if utf8.RuneCountInString(out[i].msg.Content) > 100 {
				out[i].msg.Content = truncateRunes(out[i].msg.Content, 100) + "…[folded]"
			}
		}
	}
	return out
}

// truncateRunes safely truncates s to at most n runes.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n])
}

// summarizeOldTurns replaces old turn groups with a summary message.
// Uses Counter.Count to determine if the summary actually reduces tokens.
// Context cancellation and deadline errors propagate; other errors fall back.
func (p *CompactionContextProvider) summarizeOldTurns(ctx context.Context, all []msgProvenance, minRecent int, counter TokenCounter) ([]msgProvenance, error) {
	turnStart := findTurnBoundaries(all)
	if len(turnStart) <= minRecent {
		return all, nil
	}

	// Collect old messages (before the last minRecent turns).
	oldEnd := turnStart[len(turnStart)-minRecent]
	oldMsgs := make([]models.Message, 0, oldEnd)
	for i := 0; i < oldEnd; i++ {
		if all[i].segment == segmentHistory {
			oldMsgs = append(oldMsgs, all[i].msg)
		}
	}
	if len(oldMsgs) == 0 {
		return all, nil
	}

	summary, err := p.Summarizer.Summarize(ctx, oldMsgs)
	if err != nil {
		// P0-2: propagate context cancellation and deadline errors.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		// Other errors: fall back to truncation.
		return all, nil
	}

	// Build the summarized candidate and compare token counts.
	beforeMsgs := provenanceToMessages(all)
	beforeTokens, err := counter.Count(ctx, beforeMsgs)
	if err != nil {
		return nil, err
	}

	// Construct the summarized candidate.
	candidate := make([]msgProvenance, 0, len(all))
	for i := 0; i < oldEnd; i++ {
		if all[i].segment == segmentSystem {
			candidate = append(candidate, all[i])
		}
	}
	candidate = append(candidate, msgProvenance{
		msg:     models.Message{Role: models.RoleSystem, Content: "Previous conversation summary: " + summary},
		segment: segmentSystem,
	})
	for i := oldEnd; i < len(all); i++ {
		candidate = append(candidate, all[i])
	}

	afterMsgs := provenanceToMessages(candidate)
	afterTokens, err := counter.Count(ctx, afterMsgs)
	if err != nil {
		return nil, err
	}

	// Only use the summary if it strictly reduces token count.
	if afterTokens >= beforeTokens {
		return all, nil
	}
	return candidate, nil
}

// truncateToBudget removes old history messages until the token count is
// within target, respecting atomic group boundaries and protected content.
// System messages and input are never removed.
func (p *CompactionContextProvider) truncateToBudget(ctx context.Context, all []msgProvenance, counter TokenCounter, target float64, minRecent int) ([]msgProvenance, error) {
	// Find the first history message (system messages are always protected).
	firstHistory := 0
	for firstHistory < len(all) && all[firstHistory].segment != segmentHistory {
		firstHistory++
	}

	// Protect min(len(turnStart), minRecent) recent history turns.
	// >= minRecent: start from turnStart[len-minRecent].
	// < minRecent but > 0: start from turnStart[0] (first history user).
	// No history turns: start from firstHistory.
	turnStart := findTurnBoundaries(all)
	var historyProtectedStart int
	if len(turnStart) >= minRecent {
		historyProtectedStart = turnStart[len(turnStart)-minRecent]
	} else if len(turnStart) > 0 {
		historyProtectedStart = turnStart[0]
	} else {
		historyProtectedStart = firstHistory
	}

	// Build atomic groups as [start, end) ranges within truncatable history.
	groups := buildHistoryGroups(all, firstHistory, historyProtectedStart)

	// Try truncating from the head, group by group.
	for _, g := range groups {
		// Keep system + from group end onward (remove head up to g.end).
		// Allocate a new slice to avoid aliasing all's underlying array.
		candidate := make([]msgProvenance, 0, firstHistory+len(all)-g.end)
		candidate = append(candidate, all[:firstHistory]...)
		candidate = append(candidate, all[g.end:]...)
		tokens, err := counter.Count(ctx, provenanceToMessages(candidate))
		if err != nil {
			return nil, err
		}
		if float64(tokens) <= target {
			return candidate, nil
		}
	}

	// Even the protected content alone may exceed budget.
	result := make([]msgProvenance, 0, firstHistory+len(all)-historyProtectedStart)
	result = append(result, all[:firstHistory]...)
	result = append(result, all[historyProtectedStart:]...)
	return result, nil
}

// buildHistoryGroups returns atomic groups as [start, end) ranges within
// the truncatable history segment [firstHistory, protectedStart).
func buildHistoryGroups(all []msgProvenance, firstHistory, protectedStart int) []struct{ start, end int } {
	groups := make([]struct{ start, end int }, 0)
	i := firstHistory
	for i < protectedStart {
		if all[i].msg.Role == models.RoleAssistant && len(all[i].msg.ToolCalls) > 0 {
			callIDs := make(map[string]bool, len(all[i].msg.ToolCalls))
			for _, tc := range all[i].msg.ToolCalls {
				callIDs[tc.ID] = true
			}
			lastMatch := i
			j := i + 1
			for j < protectedStart && all[j].segment == segmentHistory && all[j].msg.Role == models.RoleTool {
				if callIDs[all[j].msg.ToolCallID] {
					lastMatch = j
				}
				j++
			}
			groups = append(groups, struct{ start, end int }{i, lastMatch + 1})
			i = lastMatch + 1
			continue
		}
		groups = append(groups, struct{ start, end int }{i, i + 1})
		i++
	}
	return groups
}

// findTurnBoundaries returns the start indices of user-started turn groups
// within the history segment only. segmentHistory is sufficient to exclude
// system and input; no i>0 guard needed (first history may be at index 0
// when req.System is empty).
func findTurnBoundaries(all []msgProvenance) []int {
	boundaries := make([]int, 0)
	for i, p := range all {
		if p.segment == segmentHistory && p.msg.Role == models.RoleUser {
			boundaries = append(boundaries, i)
		}
	}
	return boundaries
}

// provenanceToMessages extracts messages from provenance entries.
func provenanceToMessages(all []msgProvenance) []models.Message {
	msgs := make([]models.Message, len(all))
	for i, p := range all {
		msgs[i] = p.msg
	}
	return msgs
}

// rebuildRequest reconstructs a ContextRequest from surviving provenance entries.
func rebuildRequest(surviving []msgProvenance, original ContextRequest) (ContextRequest, error) {
	var system []models.Message
	historyMap := make(map[int][]models.Message)
	var current, ephemeral []models.Message

	for _, p := range surviving {
		switch p.segment {
		case segmentSystem:
			system = append(system, p.msg)
		case segmentEphemeral:
			ephemeral = append(ephemeral, p.msg)
		case segmentHistory:
			historyMap[p.runIdx] = append(historyMap[p.runIdx], p.msg)
		case segmentInput:
			current = append(current, p.msg)
		}
	}

	if len(current) == 0 {
		return original, ErrContextBudgetExceeded
	}

	out := ContextRequest{System: system, Ephemeral: ephemeral, Input: current[0]}
	if len(original.Current) > 0 {
		out.Current = current
	}
	if len(historyMap) > 0 {
		seen := make(map[int]bool)
		order := make([]int, 0)
		for _, p := range surviving {
			if p.segment == segmentHistory && !seen[p.runIdx] {
				seen[p.runIdx] = true
				order = append(order, p.runIdx)
			}
		}
		for _, ri := range order {
			out.History = append(out.History, HistoryRun{
				RunID:    original.History[ri].RunID,
				Messages: historyMap[ri],
			})
		}
	}
	return out, nil
}

// budgetExceededError wraps ErrContextBudgetExceeded with detail.
func budgetExceededError(got, max int) error {
	return fmt.Errorf("%w: got %d tokens, max %d", ErrContextBudgetExceeded, got, max)
}

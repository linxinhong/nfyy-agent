package agent

import (
	"context"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

// HistoryRun is a read-only view of one run's messages with its run id. It
// preserves run boundaries so MaxRuns can be computed on real runs rather than
// on a flattened message list.
type HistoryRun struct {
	RunID    string
	Messages []models.Message
}

// ContextRequest carries the assembled message segments that a ContextProvider
// transforms. The framework passes defensively cloned slices so providers cannot
// mutate session-persistent data. Providers return a (possibly modified)
// ContextRequest; the framework performs the final flatten+assemble.
type ContextRequest struct {
	System  []models.Message
	History []HistoryRun
	Input   models.Message
	// Ephemeral carries per-request context (session state, memories,
	// knowledge, current time) assembled after History and before the
	// current input, so the leading messages stay cacheable. It is never
	// persisted in the run transcript, and compaction never removes it.
	Ephemeral []models.Message
	// Current carries the complete in-progress run during resume (user,
	// assistant tool calls, and tool results). When non-empty it replaces Input
	// during assembly; Input remains an alias of Current[0] for compatibility
	// with providers written before resumable runs.
	Current []models.Message
}

// ContextProvider is the top-level extension point invoked during initial
// context assembly and again when a paused Agent run resumes before its next
// model call. It may augment or transform the assembled segments (e.g. inject
// summaries, RAG context, or budget history). Implementations MUST be safe for
// concurrent use across runs.
type ContextProvider interface {
	Apply(ctx context.Context, req ContextRequest) (ContextRequest, error)
}

// PerModelCallContextProvider is an optional capability for providers whose
// invariant must be rechecked before every model call in a tool loop. Ordinary
// ContextProviders are not repeated, preventing duplicate RAG or prompt
// injection side effects.
type PerModelCallContextProvider interface {
	ApplyBeforeModelCall(ctx context.Context, req ContextRequest) (ContextRequest, error)
}

// HistoryPolicy budgets conversation history. MaxRuns truncates by real run
// boundary (keeps the last N runs). MaxTurns and MaxMessages are global budgets
// applied across all remaining HistoryRuns from the tail, preserving HistoryRun
// boundaries but allowing the earliest kept run to be partially truncated.
// A zero value applies no budget.
type HistoryPolicy struct {
	MaxRuns     int
	MaxTurns    int
	MaxMessages int
}

// Select returns the subset of history to keep according to the policy. It
// preserves tool-call/result atomic groups (matched by ToolCallID) and never
// creates new orphans.
//
// Filters apply in order: MaxRuns (drop oldest runs), then the remaining
// messages from all kept runs are flattened and MaxTurns/MaxMessages are
// applied globally from the tail. The earliest kept run may be partially
// truncated.
func (p *HistoryPolicy) Select(ctx context.Context, runs []HistoryRun) ([]HistoryRun, error) {
	if p == nil {
		return runs, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.MaxRuns > 0 && len(runs) > p.MaxRuns {
		runs = runs[len(runs)-p.MaxRuns:]
	}
	if p.MaxTurns == 0 && p.MaxMessages == 0 {
		return runs, nil
	}

	// Flatten all remaining runs into a single message list, tracking which
	// run each message originated from so we can reconstruct HistoryRuns after
	// global truncation.
	type indexedMsg struct {
		msg models.Message
		run int // index into runs
	}
	flat := make([]indexedMsg, 0)
	for i, r := range runs {
		for _, m := range r.Messages {
			flat = append(flat, indexedMsg{msg: m, run: i})
		}
	}
	if len(flat) == 0 {
		return runs, nil
	}

	msgs := make([]models.Message, len(flat))
	for i := range flat {
		msgs[i] = flat[i].msg
	}

	if p.MaxTurns > 0 {
		msgs = applyMaxTurns(msgs, p.MaxTurns)
	}
	if p.MaxMessages > 0 {
		msgs = applyMaxMessages(msgs, p.MaxMessages)
	}

	// Both apply functions return a suffix of the input, so we can map back
	// via the offset.
	offset := len(flat) - len(msgs)
	remaining := flat[offset:]

	// Reconstruct HistoryRuns from surviving messages, preserving run order.
	seen := make(map[int]bool)
	order := make([]int, 0, len(runs))
	groups := make(map[int][]models.Message)
	for _, fm := range remaining {
		if !seen[fm.run] {
			seen[fm.run] = true
			order = append(order, fm.run)
		}
		groups[fm.run] = append(groups[fm.run], fm.msg)
	}

	out := make([]HistoryRun, 0, len(order))
	for _, ri := range order {
		out = append(out, HistoryRun{RunID: runs[ri].RunID, Messages: groups[ri]})
	}
	return out, nil
}

// applyMaxTurns keeps the last n user-started turn groups. A new group begins
// at each user message; messages before the first user form a leading group.
func applyMaxTurns(msgs []models.Message, maxTurns int) []models.Message {
	if len(msgs) == 0 {
		return msgs
	}
	groups := make([][]models.Message, 0, 1)
	cur := make([]models.Message, 0)
	for _, m := range msgs {
		if m.Role == models.RoleUser && len(cur) > 0 {
			groups = append(groups, cur)
			cur = nil
		}
		cur = append(cur, m)
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	if len(groups) <= maxTurns {
		return msgs
	}
	out := make([]models.Message, 0, len(msgs))
	for _, g := range groups[len(groups)-maxTurns:] {
		out = append(out, g...)
	}
	return out
}

// applyMaxMessages applies a soft cap measured in messages, on atomic-group
// boundaries. An atomic group is an assistant message carrying tool calls
// together with the immediately following tool-result messages: the span extends
// from the assistant through the last consecutive RoleTool message whose
// ToolCallID matches one of the assistant's declared call IDs. Non-matching
// tool results between the first and last match are embedded in the atomic group
// and do not form independent split points (they cannot be truncated without
// orphaning a later matching result). Tool results that follow the last matching
// result are standalone units. Units are accumulated from the end; a unit is
// never split. A single unit that alone exceeds the cap is kept whole.
func applyMaxMessages(msgs []models.Message, max int) []models.Message {
	if len(msgs) == 0 || max <= 0 {
		return msgs
	}
	// Build units as [start,end) index ranges.
	type unit struct{ start, end int }
	units := make([]unit, 0, len(msgs))
	for i := 0; i < len(msgs); {
		if msgs[i].Role == models.RoleAssistant && len(msgs[i].ToolCalls) > 0 {
			// Collect the set of call IDs declared by this assistant.
			callIDs := make(map[string]bool, len(msgs[i].ToolCalls))
			for _, tc := range msgs[i].ToolCalls {
				callIDs[tc.ID] = true
			}
			// Scan forward through consecutive RoleTool messages to find the
			// last matching result. The atomic span includes everything from
			// the assistant through that last match; non-matching results in
			// between are embedded and must not be split points.
			lastMatch := i
			j := i + 1
			for j < len(msgs) && msgs[j].Role == models.RoleTool {
				if callIDs[msgs[j].ToolCallID] {
					lastMatch = j
				}
				j++
			}
			if lastMatch > i {
				// At least one match: span covers assistant through last match (inclusive).
				units = append(units, unit{i, lastMatch + 1})
				i = lastMatch + 1
			} else {
				// No matching tool results: assistant is standalone.
				units = append(units, unit{i, i + 1})
				i = i + 1
			}
			continue
		}
		units = append(units, unit{i, i + 1})
		i++
	}
	count := 0
	keepFrom := len(msgs)
	for k := len(units) - 1; k >= 0; k-- {
		size := units[k].end - units[k].start
		if count+size <= max {
			count += size
			keepFrom = units[k].start
			continue
		}
		// Doesn't fit the remaining budget. If nothing has been kept yet and
		// this unit alone exceeds the cap, keep it whole; otherwise stop.
		if count == 0 {
			count = size
			keepFrom = units[k].start
		}
		break
	}
	if keepFrom == 0 {
		return msgs
	}
	return msgs[keepFrom:]
}

// HistoryContextProvider is the built-in ContextProvider that applies a
// HistoryPolicy to budget conversation history. A nil Policy (or nil receiver)
// makes Apply an identity transform.
type HistoryContextProvider struct {
	Policy *HistoryPolicy
}

// Apply runs the policy over the history segment and returns the (possibly
// reduced) request for the next provider or final assembly.
func (p *HistoryContextProvider) Apply(ctx context.Context, req ContextRequest) (ContextRequest, error) {
	if p == nil || p.Policy == nil {
		return req, nil
	}
	selected, err := p.Policy.Select(ctx, req.History)
	if err != nil {
		return req, err
	}
	req.History = selected
	return req, nil
}

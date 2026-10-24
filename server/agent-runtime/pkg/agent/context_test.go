package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models/mock"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// msg is a small helper to build messages in tests.
func msg(role models.Role, content string) models.Message {
	return models.Message{Role: role, Content: content}
}

func TestHistoryPolicy_Select_MaxRuns_KeepsLastNRuns(t *testing.T) {
	runs := []HistoryRun{
		{RunID: "r1", Messages: []models.Message{msg(models.RoleUser, "u1"), msg(models.RoleAssistant, "a1")}},
		{RunID: "r2", Messages: []models.Message{msg(models.RoleUser, "u2"), msg(models.RoleAssistant, "a2")}},
		{RunID: "r3", Messages: []models.Message{msg(models.RoleUser, "u3"), msg(models.RoleAssistant, "a3")}},
	}
	p := &HistoryPolicy{MaxRuns: 2}

	got, err := p.Select(context.Background(), runs)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(runs) = %d, want 2", len(got))
	}
	if got[0].RunID != "r2" || got[1].RunID != "r3" {
		t.Errorf("kept runs = %s,%s; want r2,r3 (last N)", got[0].RunID, got[1].RunID)
	}
}

func TestHistoryPolicy_Select_MaxRuns_ZeroOrUnset_KeepsAll(t *testing.T) {
	runs := []HistoryRun{
		{RunID: "r1", Messages: []models.Message{msg(models.RoleUser, "u1")}},
		{RunID: "r2", Messages: []models.Message{msg(models.RoleUser, "u2")}},
	}
	p := &HistoryPolicy{} // no limits set
	got, err := p.Select(context.Background(), runs)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(runs) = %d, want 2 (no budget)", len(got))
	}
}

// toolCall builds an assistant message requesting a tool call.
func toolCall(ids ...string) models.Message {
	calls := make([]models.ToolCall, 0, len(ids))
	for _, id := range ids {
		calls = append(calls, models.ToolCall{ID: id, Type: "function", Function: models.FunctionCall{Name: "f", Arguments: "{}"}})
	}
	return models.Message{Role: models.RoleAssistant, ToolCalls: calls}
}

// toolResult builds a tool-role message with the given call id.
func toolResult(id, content string) models.Message {
	return models.Message{Role: models.RoleTool, ToolCallID: id, Content: content}
}

func TestHistoryPolicy_Select_MaxMessages_DoesNotSplitAtomicGroup(t *testing.T) {
	// One run, atomic group at the tail: assistant(c1)+tool(c1).
	runs := []HistoryRun{{
		RunID: "r1",
		Messages: []models.Message{
			msg(models.RoleUser, "q1"),
			toolCall("c1"), toolResult("c1", "r1"),
			msg(models.RoleUser, "q2"),
			toolCall("c2"), toolResult("c2", "r2"), // atomic group: 2 msgs
		},
	}}
	p := &HistoryPolicy{MaxMessages: 3} // soft cap: group(2)+user(1)=3 fits exactly
	got, err := p.Select(context.Background(), runs)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	kept := got[0].Messages
	// Expect: user(q2) + assistant(c2) + tool(c2), group intact, no orphan.
	want := []models.Message{msg(models.RoleUser, "q2"), toolCall("c2"), toolResult("c2", "r2")}
	if len(kept) != len(want) {
		t.Fatalf("kept %d msgs, want %d: %+v", len(kept), len(want), kept)
	}
	for i, w := range want {
		if kept[i].Role != w.Role || kept[i].Content != w.Content || kept[i].ToolCallID != w.ToolCallID {
			t.Errorf("msg[%d] = %+v, want %+v", i, kept[i], w)
		}
	}
}

func TestHistoryPolicy_Select_MaxMessages_GroupExceedsCap_KeptWhole(t *testing.T) {
	// A single atomic group (assistant with 3 tool results where only c1 matches
	// the assistant's declared ToolCallIDs; c2/c3 are standalone orphans).
	runs := []HistoryRun{{
		RunID: "r1",
		Messages: []models.Message{
			msg(models.RoleUser, "q"),
			toolCall("c1"), toolResult("c1", "r1"), toolResult("c2", "r2"), toolResult("c3", "r3"),
		},
	}}
	p := &HistoryPolicy{MaxMessages: 2}
	got, err := p.Select(context.Background(), runs)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	kept := got[0].Messages
	// assistant(c1)+toolResult(c1) = 2 (matched by ToolCallID). c2,c3 are standalone orphans.
	// MaxMessages=2, from tail: c3(1)+c2(1)=2 fits. Group(c1+r1)=2 exceeds, stop.
	// Result: [toolResult(c2), toolResult(c3)].
	if len(kept) != 2 {
		t.Fatalf("kept %d msgs, want 2: %+v", len(kept), kept)
	}
	if kept[0].ToolCallID != "c2" || kept[1].ToolCallID != "c3" {
		t.Errorf("kept = %+v, want orphan c2 then c3", kept)
	}
}

func TestHistoryPolicy_Select_PreservesOriginalOrphanToolResult(t *testing.T) {
	// Input already broken: a tool result with no owning assistant call.
	runs := []HistoryRun{{
		RunID: "r1",
		Messages: []models.Message{
			msg(models.RoleUser, "q"),
			toolResult("orphan", "ghost"), // no preceding assistant(toolcalls)
		},
	}}
	p := &HistoryPolicy{MaxMessages: 1}
	got, err := p.Select(context.Background(), runs)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	kept := got[0].Messages
	// Conservative: keep the orphan rather than dropping it silently.
	found := false
	for _, m := range kept {
		if m.Role == models.RoleTool && m.ToolCallID == "orphan" {
			found = true
		}
	}
	if !found {
		t.Errorf("orphan tool result was dropped; conservative strategy should keep it: %+v", kept)
	}
}

func TestHistoryPolicy_Select_MaxTurns_KeepsLastNTurnGroups(t *testing.T) {
	// Turn group = user-started segment until next user or run end.
	runs := []HistoryRun{{
		RunID: "r1",
		Messages: []models.Message{
			msg(models.RoleUser, "q1"), msg(models.RoleAssistant, "a1"),
			msg(models.RoleUser, "q2"), msg(models.RoleAssistant, "a2"),
			msg(models.RoleUser, "q3"), msg(models.RoleAssistant, "a3"),
		},
	}}
	p := &HistoryPolicy{MaxTurns: 1}
	got, err := p.Select(context.Background(), runs)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	kept := got[0].Messages
	want := []models.Message{msg(models.RoleUser, "q3"), msg(models.RoleAssistant, "a3")}
	if len(kept) != len(want) {
		t.Fatalf("kept %d, want %d (last turn group): %+v", len(kept), len(want), kept)
	}
	for i, w := range want {
		if kept[i].Content != w.Content {
			t.Errorf("msg[%d] content = %q, want %q", i, kept[i].Content, w.Content)
		}
	}
}

// sessionWithRuns builds an AgentSession with the given runs (messages each).
func sessionWithRuns(t *testing.T, runs ...[]models.Message) *session.AgentSession {
	t.Helper()
	out := make([]*session.RunOutput, len(runs))
	for i, msgs := range runs {
		out[i] = &session.RunOutput{RunID: fmtRunID(i), Messages: msgs}
	}
	return &session.AgentSession{Runs: out}
}

func fmtRunID(i int) string { return "r" + string(rune('1'+i)) }

func newTestAgent(t *testing.T, providers ...ContextProvider) *Agent {
	t.Helper()
	m := mock.New()
	m.ResponseData = &models.ModelResponse{Content: "ok"}
	ag, err := New(AgentConfig{
		Name:             "ctx-test",
		Model:            m,
		Instructions:     "sys",
		ContextProviders: providers,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ag
}

func TestHistoryContextProvider_NilPolicy_Identity(t *testing.T) {
	p := &HistoryContextProvider{} // Policy nil
	req := ContextRequest{
		System:  []models.Message{msg(models.RoleSystem, "s")},
		History: []HistoryRun{{RunID: "r1", Messages: []models.Message{msg(models.RoleUser, "u")}}},
		Input:   msg(models.RoleUser, "in"),
	}
	got, err := p.Apply(context.Background(), req)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(got.History) != 1 || got.History[0].RunID != "r1" {
		t.Errorf("identity expected; got history %+v", got.History)
	}
}

func TestHistoryContextProvider_AppliesPolicy(t *testing.T) {
	p := &HistoryContextProvider{Policy: &HistoryPolicy{MaxRuns: 1}}
	req := ContextRequest{
		History: []HistoryRun{
			{RunID: "r1", Messages: []models.Message{msg(models.RoleUser, "old")}},
			{RunID: "r2", Messages: []models.Message{msg(models.RoleUser, "new")}},
		},
	}
	got, err := p.Apply(context.Background(), req)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(got.History) != 1 || got.History[0].RunID != "r2" {
		t.Errorf("policy not applied; got %+v", got.History)
	}
}

func TestBuildRunMessages_NoProviders_CurrentBehavior(t *testing.T) {
	ag := newTestAgent(t) // no providers
	sess := sessionWithRuns(t,
		[]models.Message{msg(models.RoleUser, "u1"), msg(models.RoleAssistant, "a1")},
		[]models.Message{msg(models.RoleUser, "u2"), msg(models.RoleAssistant, "a2")},
	)
	messages, runMessages, err := ag.buildRunMessages(context.Background(), sess, "input")
	if err != nil {
		t.Fatalf("buildRunMessages: %v", err)
	}
	// Expect: [system, u1, a1, u2, a2, per-request context, input]
	if len(messages) != 7 {
		t.Fatalf("len(messages) = %d, want 7: %+v", len(messages), messages)
	}
	if messages[0].Role != models.RoleSystem || messages[0].Content != "sys" {
		t.Errorf("messages[0] = %+v, want stable system instructions", messages[0])
	}
	if ctxMsg := messages[len(messages)-2]; ctxMsg.Role != models.RoleSystem || !strings.Contains(ctxMsg.Content, "Current time:") {
		t.Errorf("per-request context = %+v, want system message with current time", ctxMsg)
	}
	if messages[len(messages)-1].Content != "input" {
		t.Errorf("last message = %q, want input", messages[len(messages)-1].Content)
	}
	if len(runMessages) != 1 || runMessages[0].Content != "input" {
		t.Errorf("runMessages = %+v, want [input]", runMessages)
	}
}

func TestBuildRunMessages_WithProviders_ChainsAndAssembles(t *testing.T) {
	ag := newTestAgent(t, &HistoryContextProvider{Policy: &HistoryPolicy{MaxRuns: 1}})
	sess := sessionWithRuns(t,
		[]models.Message{msg(models.RoleUser, "u1"), msg(models.RoleAssistant, "a1")},
		[]models.Message{msg(models.RoleUser, "u2"), msg(models.RoleAssistant, "a2")},
	)
	messages, _, err := ag.buildRunMessages(context.Background(), sess, "input")
	if err != nil {
		t.Fatalf("buildRunMessages: %v", err)
	}
	// Only last run kept: [system, u2, a2, per-request context, input]
	if len(messages) != 5 {
		t.Fatalf("len(messages) = %d, want 5: %+v", len(messages), messages)
	}
	if messages[1].Content != "u2" {
		t.Errorf("messages[1] = %q, want u2", messages[1].Content)
	}
}

// mutatingProvider appends to history to prove the framework cloned first.
type mutatingProvider struct{ seen []models.Message }

func (m *mutatingProvider) Apply(ctx context.Context, req ContextRequest) (ContextRequest, error) {
	if len(req.History) > 0 {
		m.seen = req.History[0].Messages
		// Mutate the slice we received.
		req.History[0].Messages = append(req.History[0].Messages, msg(models.RoleUser, "injected"))
	}
	return req, nil
}

func TestBuildRunMessages_ClonesSlices_ProviderCannotMutateSession(t *testing.T) {
	mp := &mutatingProvider{}
	ag := newTestAgent(t, mp)
	orig := []models.Message{msg(models.RoleUser, "u1")}
	sess := sessionWithRuns(t, orig)
	if _, _, err := ag.buildRunMessages(context.Background(), sess, "input"); err != nil {
		t.Fatalf("buildRunMessages: %v", err)
	}
	// Session's persistent messages must be untouched.
	if len(sess.Runs[0].Messages) != 1 || sess.Runs[0].Messages[0].Content != "u1" {
		t.Errorf("session mutated by provider: %+v", sess.Runs[0].Messages)
	}
}

type errorProvider struct{}

func (errorProvider) Apply(ctx context.Context, _ ContextRequest) (ContextRequest, error) {
	return ContextRequest{}, errors.New("boom")
}

type legacyReconstructingProvider struct{}

func (legacyReconstructingProvider) Apply(_ context.Context, req ContextRequest) (ContextRequest, error) {
	input := req.Input
	input.Content = "rewritten input"
	// This is valid legacy behavior from before ContextRequest.Current existed:
	// reconstruct the known fields instead of mutating req in place.
	return ContextRequest{System: req.System, History: req.History, Input: input}, nil
}

func TestContextProviders_LegacyReconstructionPreservesResumeCurrentTail(t *testing.T) {
	value := newTestAgent(t, legacyReconstructingProvider{})
	current := []models.Message{
		msg(models.RoleUser, "original input"),
		toolCall("legacy-call"),
		toolResult("legacy-call", "result"),
	}
	request, err := value.applyContextProviders(context.Background(), ContextRequest{
		Input: current[0], Current: cloneMessages(current),
	})
	if err != nil {
		t.Fatalf("applyContextProviders: %v", err)
	}
	if len(request.Current) != 3 {
		t.Fatalf("Current = %+v, want complete user+assistant+tool sequence", request.Current)
	}
	if request.Current[0].Content != "rewritten input" || request.Input.Content != "rewritten input" {
		t.Fatalf("Input alias was not synchronized: Input=%+v Current=%+v", request.Input, request.Current)
	}
	if len(request.Current[1].ToolCalls) != 1 || request.Current[1].ToolCalls[0].ID != "legacy-call" ||
		request.Current[2].ToolCallID != "legacy-call" || request.Current[2].Content != "result" {
		t.Fatalf("legacy provider dropped current tail: %+v", request.Current)
	}
}

func TestBuildRunMessages_ProviderError_Propagates(t *testing.T) {
	ag := newTestAgent(t, &HistoryContextProvider{Policy: &HistoryPolicy{MaxRuns: 1}}, errorProvider{})
	sess := sessionWithRuns(t, []models.Message{msg(models.RoleUser, "u1")})
	_, _, err := ag.buildRunMessages(context.Background(), sess, "input")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected propagated 'boom' error, got %v", err)
	}
}

// countingProvider records the order it was applied and appends a marker.
type countingProvider struct {
	id     int
	order  *[]int
	marker string
}

func (c *countingProvider) Apply(ctx context.Context, req ContextRequest) (ContextRequest, error) {
	*c.order = append(*c.order, c.id)
	if c.marker != "" {
		req.System = append(req.System, msg(models.RoleSystem, c.marker))
	}
	return req, nil
}

func TestBuildRunMessages_ProviderOrder(t *testing.T) {
	order := make([]int, 0)
	p1 := &countingProvider{id: 1, order: &order, marker: "first"}
	p2 := &countingProvider{id: 2, order: &order, marker: "second"}
	p3 := &countingProvider{id: 3, order: &order, marker: "third"}
	ag := newTestAgent(t, p1, p2, p3)
	sess := sessionWithRuns(t, []models.Message{msg(models.RoleUser, "u1")})
	messages, _, err := ag.buildRunMessages(context.Background(), sess, "input")
	if err != nil {
		t.Fatalf("buildRunMessages: %v", err)
	}
	// Verify order: 1, 2, 3.
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("provider order = %v, want [1 2 3]", order)
	}
	// Verify all markers present in output.
	for _, want := range []string{"first", "second", "third"} {
		found := false
		for _, m := range messages {
			if m.Role == models.RoleSystem && m.Content == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("marker %q not found in assembled messages", want)
		}
	}
}

func TestHistoryPolicy_ConcurrentBuildRunMessages(t *testing.T) {
	// Ensure HistoryPolicy.Select and buildRunMessages are safe for concurrent use.
	ag := newTestAgent(t, &HistoryContextProvider{Policy: &HistoryPolicy{MaxRuns: 2, MaxTurns: 1, MaxMessages: 5}})
	run := func() {
		sess := sessionWithRuns(t,
			[]models.Message{msg(models.RoleUser, "a"), msg(models.RoleAssistant, "b")},
			[]models.Message{msg(models.RoleUser, "c"), msg(models.RoleAssistant, "d")},
			[]models.Message{msg(models.RoleUser, "e"), msg(models.RoleAssistant, "f")},
		)
		_, _, err := ag.buildRunMessages(context.Background(), sess, "input")
		if err != nil {
			t.Errorf("buildRunMessages: %v", err)
		}
	}
	// Run sequentially first to confirm baseline, then concurrently for -race.
	run()
	done := make(chan struct{}, 10)
	for i := 0; i < 10; i++ {
		go func() {
			run()
			done <- struct{}{}
		}()
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}

func TestHistoryPolicy_Select_MaxTurns_GlobalAcrossRuns(t *testing.T) {
	// Three runs, each with 1 turn group. Global MaxTurns=2 → keep last 2 turns
	// from the flattened tail, which means the last 2 runs.
	runs := []HistoryRun{
		{RunID: "r1", Messages: []models.Message{msg(models.RoleUser, "u1"), msg(models.RoleAssistant, "a1")}},
		{RunID: "r2", Messages: []models.Message{msg(models.RoleUser, "u2"), msg(models.RoleAssistant, "a2")}},
		{RunID: "r3", Messages: []models.Message{msg(models.RoleUser, "u3"), msg(models.RoleAssistant, "a3")}},
	}
	p := &HistoryPolicy{MaxTurns: 2}
	got, err := p.Select(context.Background(), runs)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(runs) = %d, want 2 (global MaxTurns=2)", len(got))
	}
	if got[0].RunID != "r2" || got[1].RunID != "r3" {
		t.Fatalf("kept runs = %s,%s; want r2,r3", got[0].RunID, got[1].RunID)
	}
}

func TestHistoryPolicy_Select_MaxMessages_GlobalAcrossRuns(t *testing.T) {
	// Two runs, 3 msgs each. Global MaxMessages=4 → keep last 4 from tail.
	runs := []HistoryRun{
		{RunID: "r1", Messages: []models.Message{msg(models.RoleUser, "u1"), msg(models.RoleAssistant, "a1")}},
		{RunID: "r2", Messages: []models.Message{msg(models.RoleUser, "u2"), msg(models.RoleAssistant, "a2")}},
		{RunID: "r3", Messages: []models.Message{msg(models.RoleUser, "u3"), msg(models.RoleAssistant, "a3")}},
	}
	p := &HistoryPolicy{MaxMessages: 4}
	got, err := p.Select(context.Background(), runs)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	// Total: 6 msgs. Keep last 4: u2,a2,u3,a3. r1 is fully dropped.
	total := 0
	for _, r := range got {
		total += len(r.Messages)
	}
	if total != 4 {
		t.Fatalf("total kept = %d, want 4 (global MaxMessages=4): %+v", total, got)
	}
	if len(got) != 2 || got[0].RunID != "r2" || got[1].RunID != "r3" {
		t.Fatalf("kept = %+v; want r2 partially, r3 fully", got)
	}
}

func TestHistoryPolicy_Select_MaxTurnsThenMaxMessages_Global(t *testing.T) {
	// MaxTurns=2 (last 2 turns), then MaxMessages=3 further restricts.
	runs := []HistoryRun{
		{RunID: "r1", Messages: []models.Message{msg(models.RoleUser, "u1"), msg(models.RoleAssistant, "a1")}},
		{RunID: "r2", Messages: []models.Message{msg(models.RoleUser, "u2"), msg(models.RoleAssistant, "a2")}},
		{RunID: "r3", Messages: []models.Message{msg(models.RoleUser, "u3"), msg(models.RoleAssistant, "a3")}},
	}
	p := &HistoryPolicy{MaxTurns: 2, MaxMessages: 3}
	got, err := p.Select(context.Background(), runs)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	// MaxTurns=2: keep [u2,a2,u3,a3]. MaxMessages=3: keep last 3 → [a2,u3,a3].
	total := 0
	for _, r := range got {
		total += len(r.Messages)
	}
	if total != 3 {
		t.Fatalf("total kept = %d, want 3: %+v", total, got)
	}
}

func TestHistoryPolicy_Select_ToolCallIDMatching(t *testing.T) {
	// Assistant declares call c1, only tool result with ToolCallID=c1 is grouped.
	// Tool result with non-matching ToolCallID=c2 is a standalone orphan.
	runs := []HistoryRun{{
		RunID: "r1",
		Messages: []models.Message{
			msg(models.RoleUser, "q"),
			toolCall("c1"),
			toolResult("c1", "matched"),   // matches c1 → in group
			toolResult("c2", "unrelated"), // does NOT match c1 → standalone
		},
	}}
	p := &HistoryPolicy{MaxMessages: 3}
	got, err := p.Select(context.Background(), runs)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	kept := got[0].Messages
	// Units: [user], [assistant(c1)+tool(c1,matched)], [tool(c2,unrelated)]
	// MaxMessages=3: from tail → c2(1)+group(2)=3 fits.
	if len(kept) != 3 {
		t.Fatalf("kept %d msgs, want 3: %+v", len(kept), kept)
	}
	if kept[0].Role != models.RoleAssistant || len(kept[0].ToolCalls) != 1 || kept[0].ToolCalls[0].ID != "c1" {
		t.Errorf("first msg = %+v, want assistant with c1", kept[0])
	}
	if kept[1].ToolCallID != "c1" || kept[1].Content != "matched" {
		t.Errorf("second msg = %+v, want matched tool result c1", kept[1])
	}
	if kept[2].ToolCallID != "c2" || kept[2].Content != "unrelated" {
		t.Errorf("third msg = %+v, want unrelated orphan c2", kept[2])
	}
}

func TestHistoryPolicy_Select_InterleavedUnmatched_PreservesAssistantWithLastMatch(t *testing.T) {
	// Regression: assistant(c1,c2) → tool(c1) → tool(c3 unmatched) → tool(c2).
	// MaxMessages=1 must keep the full atomic span (4 msgs) rather than
	// creating a new orphan out of tool(c2).
	runs := []HistoryRun{{
		RunID: "r1",
		Messages: []models.Message{
			msg(models.RoleUser, "q"),
			assistantWithCalls("c1", "c2"),
			toolResult("c1", "r1"),        // matches c1
			toolResult("c3", "unmatched"), // does NOT match c1 or c2
			toolResult("c2", "r2"),        // matches c2 — last match
		},
	}}
	p := &HistoryPolicy{MaxMessages: 1}
	got, err := p.Select(context.Background(), runs)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	kept := got[0].Messages
	// The atomic span is [assistant .. lastMatch(tool(c2))] = 4 msgs.
	// MaxMessages=1: unit alone exceeds cap, kept whole. Must include assistant and c2.
	if len(kept) != 4 {
		t.Fatalf("kept %d msgs, want 4 (whole atomic span): %+v", len(kept), kept)
	}
	if kept[0].Role != models.RoleAssistant || len(kept[0].ToolCalls) != 2 {
		t.Errorf("first msg = %+v, want assistant with c1 and c2", kept[0])
	}
	// tool(c2) must be present — no orphan created.
	foundC2 := false
	for _, m := range kept {
		if m.ToolCallID == "c2" && m.Content == "r2" {
			foundC2 = true
		}
	}
	if !foundC2 {
		t.Error("tool(c2) was dropped (new orphan)")
	}
	// tool(c3) stays embedded.
	foundC3 := false
	for _, m := range kept {
		if m.ToolCallID == "c3" {
			foundC3 = true
		}
	}
	if !foundC3 {
		t.Error("tool(c3) was dropped")
	}
}

func TestHistoryPolicy_Select_TrailingUnmatched_CanBeIndependentlySelected(t *testing.T) {
	// assistant(c1) → tool(c1) → tool(c3 trailing unmatched).
	// MaxMessages=1 should select the trailing orphan independently.
	runs := []HistoryRun{{
		RunID: "r1",
		Messages: []models.Message{
			msg(models.RoleUser, "q"),
			toolCall("c1"),
			toolResult("c1", "r1"),       // matches c1 → last match
			toolResult("c3", "trailing"), // does NOT match c1 → trailing orphan
		},
	}}
	p := &HistoryPolicy{MaxMessages: 1}
	got, err := p.Select(context.Background(), runs)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	kept := got[0].Messages
	// Units: [user], [assistant(c1)+tool(c1,r1)], [tool(c3,trailing)]
	// MaxMessages=1: from tail → tool(c3) = 1 fits.
	if len(kept) != 1 || kept[0].ToolCallID != "c3" || kept[0].Content != "trailing" {
		t.Fatalf("kept = %+v, want [tool(c3,trailing)]", kept)
	}
}

func TestHistoryPolicy_Select_MultiToolCalls_MatchesByID(t *testing.T) {
	// Assistant declares calls c1 and c2. The atomic span is [assistant .. lastMatch(tool(c2))]
	// = assistant + tool(c1) + tool(c3) + tool(c2) = 4 msgs. c3 is embedded unmatched
	// inside the span (not a split point); c2 is the last match and closes the span.
	runs := []HistoryRun{{
		RunID: "r1",
		Messages: []models.Message{
			msg(models.RoleUser, "q"),
			assistantWithCalls("c1", "c2"),
			toolResult("c1", "r1"), // matches c1
			toolResult("c3", "r3"), // embedded unmatched (no split point)
			toolResult("c2", "r2"), // matches c2 → last match, closes atomic span
		},
	}}
	p := &HistoryPolicy{MaxMessages: 10}
	got, err := p.Select(context.Background(), runs)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	kept := got[0].Messages
	// Atomic span = [assistant(c1,c2), tool(c1,r1), tool(c3,r3), tool(c2,r2)]; user q leads.
	// Total = 5 msgs. No budget applied (MaxMessages=10 keeps all).
	if len(kept) != 5 {
		t.Fatalf("kept %d msgs, want 5: %+v", len(kept), kept)
	}
	// Embedded unmatched c3 should be preserved in its original position.
	foundC3 := false
	for _, m := range kept {
		if m.ToolCallID == "c3" {
			foundC3 = true
		}
	}
	if !foundC3 {
		t.Error("embedded c3 was dropped")
	}
}

// assistantWithCalls builds an assistant message with multiple tool calls.
func assistantWithCalls(ids ...string) models.Message {
	calls := make([]models.ToolCall, 0, len(ids))
	for _, id := range ids {
		calls = append(calls, models.ToolCall{ID: id, Type: "function", Function: models.FunctionCall{Name: "f", Arguments: "{}"}})
	}
	return models.Message{Role: models.RoleAssistant, ToolCalls: calls}
}

// mutatingDeepProvider mutates ToolCalls[0].Function.Name and Images[0] to
// verify deep cloning.
type mutatingDeepProvider struct {
	modifiedToolCallName string
	modifiedImageURL     string
}

func (m *mutatingDeepProvider) Apply(ctx context.Context, req ContextRequest) (ContextRequest, error) {
	if len(req.History) > 0 && len(req.History[0].Messages) > 0 {
		msg := &req.History[0].Messages[0]
		if len(msg.ToolCalls) > 0 {
			msg.ToolCalls[0].Function.Name = m.modifiedToolCallName
		}
		if len(msg.Images) > 0 {
			msg.Images[0].URL = m.modifiedImageURL
		}
	}
	return req, nil
}

func TestBuildRunMessages_DeepClone_PreventsNestedMutation(t *testing.T) {
	mp := &mutatingDeepProvider{modifiedToolCallName: "hijacked", modifiedImageURL: "evil"}
	ag := newTestAgent(t, mp)
	origMsg := models.Message{
		Role:    models.RoleAssistant,
		Content: "original",
		ToolCalls: []models.ToolCall{
			{ID: "c1", Type: "function", Function: models.FunctionCall{Name: "safe_tool", Arguments: "{}"}},
		},
		Images: []models.Image{
			{URL: "https://safe.example/img.png", MimeType: "image/png"},
		},
		Audios: []models.Audio{
			{URL: "https://safe.example/audio.mp3", MimeType: "audio/mp3"},
		},
		Videos: []models.Video{
			{URL: "https://safe.example/video.mp4", MimeType: "video/mp4"},
		},
		Files: []models.File{
			{URL: "https://safe.example/doc.pdf", Name: "doc.pdf"},
		},
	}
	sess := sessionWithRuns(t, []models.Message{origMsg})
	if _, _, err := ag.buildRunMessages(context.Background(), sess, "input"); err != nil {
		t.Fatalf("buildRunMessages: %v", err)
	}
	// Session must be untouched.
	persisted := sess.Runs[0].Messages[0]
	if persisted.Content != "original" {
		t.Errorf("Content mutated: %q", persisted.Content)
	}
	if len(persisted.ToolCalls) != 1 || persisted.ToolCalls[0].Function.Name != "safe_tool" {
		t.Errorf("ToolCalls[0].Function.Name mutated: %q", persisted.ToolCalls[0].Function.Name)
	}
	if len(persisted.Images) != 1 || persisted.Images[0].URL != "https://safe.example/img.png" {
		t.Errorf("Images[0].URL mutated: %q", persisted.Images[0].URL)
	}
	if len(persisted.Audios) != 1 || persisted.Audios[0].URL != "https://safe.example/audio.mp3" {
		t.Errorf("Audios[0].URL mutated: %q", persisted.Audios[0].URL)
	}
	if len(persisted.Videos) != 1 || persisted.Videos[0].URL != "https://safe.example/video.mp4" {
		t.Errorf("Videos[0].URL mutated: %q", persisted.Videos[0].URL)
	}
	if len(persisted.Files) != 1 || persisted.Files[0].URL != "https://safe.example/doc.pdf" {
		t.Errorf("Files[0].URL mutated: %q", persisted.Files[0].URL)
	}
	// Verify nil slices stayed nil.
	for _, m := range sess.Runs[0].Messages[1:] {
		if m.ToolCalls != nil || m.Images != nil || m.Audios != nil || m.Videos != nil || m.Files != nil {
			t.Errorf("nil slices were allocated: %+v", m)
		}
	}
}

func TestBuildRunMessages_NilProviderSkipped(t *testing.T) {
	order := make([]int, 0)
	ag := newTestAgent(t,
		nil, // should be silently skipped
		&countingProvider{id: 1, order: &order},
		nil, // should be silently skipped
		&countingProvider{id: 2, order: &order},
		nil, // should be silently skipped
	)
	sess := sessionWithRuns(t, []models.Message{msg(models.RoleUser, "u1")})
	_, _, err := ag.buildRunMessages(context.Background(), sess, "input")
	if err != nil {
		t.Fatalf("buildRunMessages: %v", err)
	}
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("provider order = %v, want [1 2] (nil entries skipped)", order)
	}
}

func TestBuildRunMessages_ContextCancelled(t *testing.T) {
	ag := newTestAgent(t, &HistoryContextProvider{Policy: &HistoryPolicy{MaxRuns: 1}})
	sess := sessionWithRuns(t, []models.Message{msg(models.RoleUser, "u1")})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := ag.buildRunMessages(ctx, sess, "input")
	if err == nil {
		t.Fatalf("expected cancellation error, got nil")
	}
}

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// buildRunMessages constructs the full message list for a run: the stable
// system message, prior conversation history, the per-request context
// message (session state, memories, knowledge, current time), and the
// current user message. Placing the per-request context after the history
// keeps the leading messages byte-identical across turns, so provider
// prompt caches reuse them (see AgentConfig.LegacySystemContext).
//
// When ContextProviders are configured on the Agent, the history is structured
// into HistoryRuns (defensively deep-cloned), and the providers are applied in
// order to a ContextRequest during the initial context assembly phase of the
// Agent run.
func (a *Agent) buildRunMessages(ctx context.Context, sess *session.AgentSession, input string) ([]models.Message, []models.Message, error) {
	system, ephemeral, err := a.buildContextMessages(ctx, sess, input)
	if err != nil {
		return nil, nil, err
	}
	userMessage := models.Message{
		Role:    models.RoleUser,
		Content: input,
	}
	req := ContextRequest{
		System:    system,
		History:   sessionHistory(sess, "", a.cfg.MaxHistoryRuns),
		Ephemeral: ephemeral,
		Input:     userMessage,
	}
	if len(a.cfg.ContextProviders) > 0 {
		req, err = a.applyContextProviders(ctx, req)
		if err != nil {
			return nil, nil, err
		}
	}
	return assembleContextRequest(req), []models.Message{userMessage}, nil
}

func (a *Agent) applyContextProviders(ctx context.Context, req ContextRequest) (ContextRequest, error) {
	for _, provider := range a.cfg.ContextProviders {
		if provider == nil {
			continue
		}
		previousInput := cloneMessages([]models.Message{req.Input})[0]
		previousCurrent := cloneMessages(req.Current)
		var previousCurrentInput models.Message
		if len(previousCurrent) > 0 {
			previousCurrentInput = previousCurrent[0]
		}
		previousEphemeral := req.Ephemeral
		next, err := provider.Apply(ctx, req)
		if err != nil {
			return req, fmt.Errorf("context provider: %w", err)
		}
		if next.Ephemeral == nil {
			// Providers written before Ephemeral existed rebuild the request
			// from its original fields; keep the per-request context.
			next.Ephemeral = previousEphemeral
		}
		if len(previousCurrent) > 0 && len(next.Current) == 0 {
			// Providers compiled against the original ContextRequest commonly
			// reconstruct the known fields. Preserve the resume continuation
			// tail and treat their Input as the replacement for Current[0].
			next.Current = previousCurrent
			next.Current[0] = next.Input
		}
		if len(next.Current) > 0 {
			inputChanged := !reflect.DeepEqual(next.Input, previousInput)
			currentChanged := !reflect.DeepEqual(next.Current[0], previousCurrentInput)
			if inputChanged && !currentChanged {
				next.Current[0] = next.Input
			} else {
				next.Input = next.Current[0]
			}
		}
		req = next
	}
	return req, nil
}

func (a *Agent) applyPerModelCallContextProviders(
	ctx context.Context,
	messages []models.Message,
	currentMessageCount int,
) ([]models.Message, error) {
	if currentMessageCount <= 0 || currentMessageCount > len(messages) {
		return nil, errors.New("agent: invalid current message boundary")
	}
	hasProvider := false
	for _, provider := range a.cfg.ContextProviders {
		if _, ok := provider.(PerModelCallContextProvider); ok {
			hasProvider = true
			break
		}
	}
	if !hasProvider {
		return messages, nil
	}

	prefixEnd := len(messages) - currentMessageCount
	prefix := cloneMessages(messages[:prefixEnd])
	current := cloneMessages(messages[prefixEnd:])
	systemEnd := 0
	for systemEnd < len(prefix) && prefix[systemEnd].Role == models.RoleSystem {
		systemEnd++
	}
	// Trailing system messages after the history are the per-request
	// context; keep them out of the truncatable history.
	historyEnd := len(prefix)
	for historyEnd > systemEnd && prefix[historyEnd-1].Role == models.RoleSystem {
		historyEnd--
	}
	request := ContextRequest{
		System:  cloneMessages(prefix[:systemEnd]),
		Input:   current[0],
		Current: current,
	}
	if historyEnd < len(prefix) {
		request.Ephemeral = cloneMessages(prefix[historyEnd:])
	}
	if systemEnd < historyEnd {
		request.History = []HistoryRun{{
			RunID: "assembled-history", Messages: cloneMessages(prefix[systemEnd:historyEnd]),
		}}
	}

	for _, provider := range a.cfg.ContextProviders {
		perCall, ok := provider.(PerModelCallContextProvider)
		if !ok {
			continue
		}
		protected := cloneMessages(request.Current)
		next, err := perCall.ApplyBeforeModelCall(ctx, request)
		if err != nil {
			return nil, fmt.Errorf("per-model-call context provider: %w", err)
		}
		if !reflect.DeepEqual(next.Current, protected) {
			return nil, errors.New("agent: per-model-call context provider changed protected current messages")
		}
		request = next
	}
	return assembleContextRequest(request), nil
}

// buildContextMessages returns the stable system messages (instructions
// and output schema) and the per-request context message (session state,
// memories, knowledge, current time). Memory and knowledge lookups run
// concurrently. With LegacySystemContext everything is merged into a single
// leading system message.
func (a *Agent) buildContextMessages(ctx context.Context, sess *session.AgentSession, input string) (system, ephemeral []models.Message, err error) {
	var stableParts []string
	if a.cfg.Instructions != "" {
		stableParts = append(stableParts, a.cfg.Instructions)
	}
	if a.cfg.OutputSchema != nil {
		schemaJSON, err := schemaAsJSON(a.cfg.OutputSchema)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal output schema: %w", err)
		}
		stableParts = append(stableParts, fmt.Sprintf("Your entire response must be a single JSON object matching this schema. Do not explain or wrap in markdown:\n%s", schemaJSON))
	}

	var dynamicParts []string
	state := mergeSessionState(a.cfg.SessionState, sess.SessionState)
	if len(state) > 0 {
		b, err := json.Marshal(state)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal session state: %w", err)
		}
		dynamicParts = append(dynamicParts, fmt.Sprintf("Session state:\n%s", string(b)))
	}
	retrieved, err := a.retrieveContext(ctx, input)
	if err != nil {
		return nil, nil, err
	}
	dynamicParts = append(dynamicParts, retrieved...)
	dynamicParts = append(dynamicParts, fmt.Sprintf("Current time: %s", time.Now().UTC().Format(time.RFC3339)))

	if a.cfg.LegacySystemContext {
		parts := append(stableParts, dynamicParts...)
		return []models.Message{{Role: models.RoleSystem, Content: strings.Join(parts, "\n\n")}}, nil, nil
	}
	if len(stableParts) > 0 {
		system = []models.Message{{Role: models.RoleSystem, Content: strings.Join(stableParts, "\n\n")}}
	}
	ephemeral = []models.Message{{
		Role:                 models.RoleSystem,
		Content:              strings.Join(dynamicParts, "\n\n"),
		AdditionalProperties: map[string]any{models.EphemeralContextProperty: true},
	}}
	return system, ephemeral, nil
}

// retrieveContext runs the memory and knowledge lookups concurrently and
// returns their rendered sections in a fixed order.
func (a *Agent) retrieveContext(ctx context.Context, input string) ([]string, error) {
	if a.cfg.Memory == nil && a.cfg.Knowledge == nil {
		return nil, nil
	}
	var (
		wg                       sync.WaitGroup
		memorySection, kbSection string
		memoryErr, kbErr         error
	)
	if a.cfg.Memory != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			memories, err := a.cfg.Memory.GetMemories(ctx, a.cfg.UserID, a.ID(), 10)
			if err != nil {
				memoryErr = fmt.Errorf("get memories: %w", err)
				return
			}
			if len(memories) > 0 {
				lines := make([]string, 0, len(memories))
				for _, m := range memories {
					lines = append(lines, fmt.Sprintf("- %s", m.Memory))
				}
				memorySection = "User memories:\n" + strings.Join(lines, "\n")
			}
		}()
	}
	if a.cfg.Knowledge != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			docs, err := a.cfg.Knowledge.Search(ctx, input, 5)
			if err != nil {
				kbErr = fmt.Errorf("search knowledge: %w", err)
				return
			}
			if len(docs) > 0 {
				lines := make([]string, 0, len(docs))
				for _, d := range docs {
					lines = append(lines, fmt.Sprintf("- %s", d.Content))
				}
				kbSection = "Relevant knowledge:\n" + strings.Join(lines, "\n")
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(memoryErr, kbErr); err != nil {
		return nil, err
	}
	var sections []string
	for _, section := range []string{memorySection, kbSection} {
		if section != "" {
			sections = append(sections, section)
		}
	}
	return sections, nil
}

// cloneMessages returns a deep copy of in where all slice fields (ToolCalls,
// Images, Audios, Videos, Files) are allocated on new backing arrays so a
// provider cannot mutate session-persistent data through shared references.
func cloneMessages(in []models.Message) []models.Message {
	if in == nil {
		return nil
	}
	out := make([]models.Message, len(in))
	for i, m := range in {
		out[i] = m
		if m.ToolCalls != nil {
			out[i].ToolCalls = make([]models.ToolCall, len(m.ToolCalls))
			copy(out[i].ToolCalls, m.ToolCalls)
		}
		if m.Images != nil {
			out[i].Images = make([]models.Image, len(m.Images))
			copy(out[i].Images, m.Images)
		}
		if m.Audios != nil {
			out[i].Audios = make([]models.Audio, len(m.Audios))
			copy(out[i].Audios, m.Audios)
		}
		if m.Videos != nil {
			out[i].Videos = make([]models.Video, len(m.Videos))
			copy(out[i].Videos, m.Videos)
		}
		if m.Files != nil {
			out[i].Files = make([]models.File, len(m.Files))
			copy(out[i].Files, m.Files)
		}
	}
	return out
}

// assembleContextRequest flattens a ContextRequest into the final ordered
// message list: system, history (per run), ephemeral, then Current or the
// legacy Input.
func assembleContextRequest(req ContextRequest) []models.Message {
	n := len(req.System) + len(req.Ephemeral) + len(req.Current)
	if len(req.Current) == 0 {
		n++
	}
	for _, r := range req.History {
		n += len(r.Messages)
	}
	out := make([]models.Message, 0, n)
	out = append(out, req.System...)
	for _, r := range req.History {
		out = append(out, r.Messages...)
	}
	out = append(out, req.Ephemeral...)
	if len(req.Current) > 0 {
		out = append(out, req.Current...)
	} else {
		out = append(out, req.Input)
	}
	return out
}

func schemaAsJSON(schema any) (string, error) {
	rf, err := responseFormatForSchema(schema)
	if err != nil {
		return "", err
	}
	if rf == nil || rf.JSONSchema == nil {
		return "", nil
	}
	inner, ok := rf.JSONSchema["schema"].(map[string]any)
	if !ok {
		inner = rf.JSONSchema
	}
	b, err := json.Marshal(inner)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func mergeSessionState(cfgState, sessState map[string]any) map[string]any {
	if len(cfgState) == 0 && len(sessState) == 0 {
		return nil
	}
	merged := make(map[string]any, len(cfgState)+len(sessState))
	for k, v := range cfgState {
		merged[k] = v
	}
	for k, v := range sessState {
		merged[k] = v
	}
	return merged
}

// sessionHistory returns the provider-valid history of a session, excluding
// excludeRunID and keeping at most the newest maxRuns runs (0 keeps all).
// Reasoning content of earlier runs is dropped: providers only need it
// within the current run's tool loop, and R1-style reasoning would
// otherwise dominate the context. Runs still in flight (running, or paused for approval) are
// skipped: they belong to another request (pending runs likewise). Runs that ended mid tool loop
// (error, cancel, crash) are repaired so every assistant tool call has its
// result and no tool result is orphaned; OpenAI-compatible APIs (including
// DeepSeek) reject either with HTTP 400, which would poison the session.
// Messages are deep-cloned so callers cannot mutate persisted data.
func sessionHistory(sess *session.AgentSession, excludeRunID string, maxRuns int) []HistoryRun {
	out := make([]HistoryRun, 0, len(sess.Runs))
	for _, r := range sess.Runs {
		if r == nil || r.RunID == excludeRunID {
			continue
		}
		if r.Status == session.RunStatusPending || r.Status == session.RunStatusRunning || r.Status == session.RunStatusPaused {
			continue
		}
		if r.RecoveredBy != "" {
			continue // its successor run carries the transcript
		}
		msgs := repairToolPairs(cloneMessages(r.Messages))
		if len(msgs) == 0 {
			continue
		}
		for i := range msgs {
			msgs[i].ReasoningContent = ""
		}
		out = append(out, HistoryRun{RunID: r.RunID, Messages: msgs})
	}
	if maxRuns > 0 && len(out) > maxRuns {
		out = out[len(out)-maxRuns:]
	}
	return out
}

// repairToolPairs drops tool calls without results, assistant messages left
// empty by that, and tool results without a matching call.
func repairToolPairs(msgs []models.Message) []models.Message {
	answered := make(map[string]bool)
	for _, m := range msgs {
		if m.Role == models.RoleTool && m.ToolCallID != "" {
			answered[m.ToolCallID] = true
		}
	}
	called := make(map[string]bool)
	out := msgs[:0]
	for _, m := range msgs {
		switch m.Role {
		case models.RoleAssistant:
			if len(m.ToolCalls) > 0 {
				kept := m.ToolCalls[:0]
				for _, tc := range m.ToolCalls {
					if answered[tc.ID] {
						kept = append(kept, tc)
						called[tc.ID] = true
					}
				}
				m.ToolCalls = kept
				if len(kept) == 0 {
					m.ToolCalls = nil
					if m.Content == "" {
						continue
					}
				}
			}
		case models.RoleTool:
			if !called[m.ToolCallID] {
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

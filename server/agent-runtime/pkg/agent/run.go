// Package agent implements the core Agent runtime, including configuration,
// execution, streaming, and guardrails.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/linxinhong/xingding-agent-runtime/pkg/db"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	runmanager "github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/builtin"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools/service"
)

// applyTrustedRunIdentity snapshots host-provided scope and principal into a
// durable run. HTTP input never contributes either value.
func applyTrustedRunIdentity(ctx context.Context, output *session.RunOutput) error {
	namespace, scoped := execution.NamespaceFromContext(ctx)
	if scoped {
		output.Namespace = namespace
	}
	if principal, err := service.PrincipalFromContext(ctx); err == nil {
		if scoped && namespace != principal.Namespace {
			return ErrTrustedIdentityNamespaceMismatch
		}
		output.Principal = principal
	}
	return nil
}

func (a *Agent) persistRun(ctx context.Context, sess *session.AgentSession, output *session.RunOutput) error {
	if owner, managed, err := managedRootOwner(ctx, output); managed {
		if err != nil {
			return err
		}
		_, err = owner.Create(ctx, output)
		return err
	}
	if a.cfg.DB == nil {
		return nil
	}
	return a.cfg.DB.AppendRun(ctx, output)
}

func (a *Agent) updateRun(ctx context.Context, output *session.RunOutput) error {
	finishMetrics(output)
	if owner, managed, err := managedRootOwner(ctx, output); managed {
		if err != nil {
			return err
		}
		_, err = owner.Update(ctx, output, session.RunStatusRunning)
		return err
	}
	if a.cfg.DB == nil {
		return nil
	}
	return runmanager.Finalize(ctx, a.cfg.DB, output, session.RunStatusRunning)
}

func managedRootOwner(ctx context.Context, output *session.RunOutput) (*runmanager.ManagedRunOwner, bool, error) {
	owner, ok := runmanager.ManagedRunOwnerFromContext(ctx)
	if !ok || output == nil || output.ParentRunID != "" {
		return nil, false, nil
	}
	if output.RunID != owner.RunID() {
		return owner, true, runmanager.ErrInvalidClaim
	}
	if output.Namespace == "" {
		output.Namespace = owner.Scope().Namespace
	}
	if output.Namespace != owner.Scope().Namespace {
		return owner, true, runmanager.ErrInvalidClaim
	}
	return owner, true, nil
}

func (a *Agent) loadOrCreateSession(ctx context.Context) (*session.AgentSession, error) {
	if a.cfg.DB == nil {
		return newSession(a.ID(), a.cfg.UserID), nil
	}

	if a.cfg.SessionID != "" {
		sess, err := a.readSession(ctx, a.cfg.SessionID)
		if err != nil {
			return nil, err
		}
		if sess != nil {
			return sess, nil
		}
	}

	sess := newSession(a.ID(), a.cfg.UserID)
	if err := a.cfg.DB.CreateSession(ctx, sess); err != nil {
		return nil, err
	}
	a.cfg.SessionID = sess.SessionID
	return sess, nil
}

// readSession loads a session for history assembly. With MaxHistoryRuns
// and a store implementing db.RecentSessionReader only the newest runs are
// read (one extra covers the run being resumed); session updates never
// rewrite runs, so the trimmed view is safe to save back.
func (a *Agent) readSession(ctx context.Context, sessionID string) (*session.AgentSession, error) {
	if reader, ok := a.cfg.DB.(db.RecentSessionReader); ok && a.cfg.MaxHistoryRuns > 0 {
		return reader.ReadSessionRecent(ctx, sessionID, a.cfg.MaxHistoryRuns+1)
	}
	return a.cfg.DB.ReadSession(ctx, sessionID)
}

func (a *Agent) saveSession(ctx context.Context, sess *session.AgentSession) error {
	if a.cfg.DB == nil {
		return nil
	}
	existing, err := a.cfg.DB.ReadSession(ctx, sess.SessionID)
	if err != nil {
		return err
	}
	if existing == nil {
		return a.cfg.DB.CreateSession(ctx, sess)
	}
	return a.cfg.DB.UpdateSession(ctx, sess)
}

func newSession(agentID, userID string) *session.AgentSession {
	now := time.Now().UTC()
	return &session.AgentSession{
		SessionID: uuid.NewString(),
		AgentID:   agentID,
		UserID:    userID,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// resolveTools returns the tool set computed once in New. Agents built
// without New (tests) compute it on demand.
func (a *Agent) resolveTools() (map[string]tools.Tool, []models.Tool, error) {
	if a.toolSet != nil {
		return a.toolSet.byName, a.toolSet.schemas, nil
	}
	set, err := buildToolSet(a.cfg)
	if err != nil {
		return nil, nil, err
	}
	return set.byName, set.schemas, nil
}

// toolSet is the immutable, validated tool catalog of one Agent.
type toolSet struct {
	byName  map[string]tools.Tool
	schemas []models.Tool
}

// ErrDuplicateTool rejects two tools sharing a name; the model could not
// tell them apart and the last registration would silently win.
var ErrDuplicateTool = errors.New("agent: duplicate tool name")

func buildToolSet(cfg AgentConfig) (*toolSet, error) {
	all := make([]tools.Tool, 0, len(cfg.Tools)+2)
	for _, t := range cfg.Tools {
		if t != nil {
			all = append(all, t)
		}
	}
	if cfg.Knowledge != nil {
		t, err := builtin.NewKnowledgeSearchTool()
		if err != nil {
			return nil, fmt.Errorf("knowledge search tool: %w", err)
		}
		all = append(all, t)
	}
	if cfg.Memory != nil {
		t, err := builtin.NewMemoryUpdateTool()
		if err != nil {
			return nil, fmt.Errorf("memory update tool: %w", err)
		}
		all = append(all, t)
	}
	if len(all) == 0 {
		return &toolSet{}, nil
	}
	set := &toolSet{byName: make(map[string]tools.Tool, len(all)), schemas: make([]models.Tool, 0, len(all))}
	for _, t := range all {
		name := t.Name()
		if _, exists := set.byName[name]; exists {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateTool, name)
		}
		set.byName[name] = t
		set.schemas = append(set.schemas, schemaFromTool(t))
	}
	return set, nil
}

func schemaFromTool(t tools.Tool) models.Tool {
	schema := t.Schema()
	fn, _ := schema["function"].(map[string]any)
	name, _ := fn["name"].(string)
	desc, _ := fn["description"].(string)
	params, _ := fn["parameters"].(map[string]any)
	return models.Tool{
		Type: "function",
		Function: models.ToolFunction{
			Name:        name,
			Description: desc,
			Parameters:  params,
		},
	}
}

const (
	defaultMaxToolResultBytes = 256 << 10
	maxToolErrorBytes         = 2 << 10
)

// toolFailure classifies a tool error for ToolExecution.Status.
type toolFailure struct {
	status session.ToolExecutionStatus
	err    error
}

func (f *toolFailure) Error() string { return f.err.Error() }
func (f *toolFailure) Unwrap() error { return f.err }

// toolOutcome is the result of one tool invocation, recorded on the run in
// call order once a batch (or parallel group) completes.
type toolOutcome struct {
	message   models.Message
	execution session.ToolExecution
}

// executeToolCalls runs calls in order. Consecutive read-only tools run
// concurrently (bounded by MaxParallelTools) in unmanaged mode; results are
// always recorded in call order. On cancellation it returns the messages
// of calls that finished.
func (a *Agent) executeToolCalls(ctx context.Context, sess *session.AgentSession, output *session.RunOutput, toolMap map[string]tools.Tool, toolCalls []models.ToolCall) ([]models.Message, error) {
	rc := &builtin.RunContext{
		Session:   sess,
		Knowledge: a.cfg.Knowledge,
		Memory:    a.cfg.Memory,
		AgentID:   a.ID(),
		UserID:    a.cfg.UserID,
		RunID:     output.RunID, SessionID: output.SessionID, ParentRunID: output.ParentRunID, NodeID: output.NodeID,
		Metadata: output.Metadata,
	}
	toolCtx := builtin.ContextWithRunContext(ctx, rc)
	em := emitterFromContext(ctx)

	messages := make([]models.Message, 0, len(toolCalls))
	record := func(outcomes []toolOutcome) {
		for _, o := range outcomes {
			output.ToolExecutions = append(output.ToolExecutions, o.execution)
			output.Metrics.ToolCalls++
			messages = append(messages, o.message)
		}
	}
	for i := 0; i < len(toolCalls); {
		if err := ctx.Err(); err != nil {
			return messages, err
		}
		end := i + 1
		if a.parallelToolLimit() > 1 && a.isParallelSafe(toolMap, toolCalls[i]) {
			for end < len(toolCalls) && a.isParallelSafe(toolMap, toolCalls[end]) {
				end++
			}
		}
		group := make([]toolOutcome, end-i)
		if len(group) == 1 {
			group[0] = a.invokeTool(toolCtx, output, toolMap, toolCalls[i], em)
		} else {
			sem := make(chan struct{}, a.parallelToolLimit())
			var wg sync.WaitGroup
			for j := range group {
				wg.Add(1)
				sem <- struct{}{}
				go func(j int) {
					defer wg.Done()
					defer func() { <-sem }()
					group[j] = a.invokeTool(toolCtx, output, toolMap, toolCalls[i+j], em)
				}(j)
			}
			wg.Wait()
		}
		record(group)
		i = end
	}
	return messages, nil
}

func (a *Agent) parallelToolLimit() int {
	if a.cfg.ToolService != nil {
		return 1
	}
	return a.cfg.MaxParallelTools
}

// isParallelSafe admits only declared read-only, approval-free tools.
func (a *Agent) isParallelSafe(toolMap map[string]tools.Tool, call models.ToolCall) bool {
	tool, ok := toolMap[call.Function.Name]
	return ok && tools.IsReadOnly(tool) && effectiveApproval(tool) == tools.ApprovalNone
}

// invokeTool runs one call through the tool middleware chain and renders
// the model-facing result. It never mutates output, so it is safe to run
// concurrently.
func (a *Agent) invokeTool(ctx context.Context, output *session.RunOutput, toolMap map[string]tools.Tool, tc models.ToolCall, em emitter) toolOutcome {
	call := tc
	em.emit(Event{Type: EventTypeToolStarted, ToolCall: &call})
	startedAt := time.Now()
	result, callErr := runToolChain(a.toolMWs, ctx, &call, func(ctx context.Context, call *models.ToolCall) (any, error) {
		if a.cfg.ToolService != nil {
			// Resume path: execute the frozen plan the batch was
			// approved against (never re-prepare). Sync path: prepare +
			// execute inline.
			if ref, ok := output.PendingPlans[call.ID]; ok {
				if ref.Denied != "" {
					return nil, errors.New(ref.Denied)
				}
				return a.executeManagedPlan(ctx, service.PlanRef{ID: ref.ID, Version: ref.Version})
			}
			return a.executeManagedTool(ctx, call)
		}
		tool, ok := toolMap[call.Function.Name]
		if !ok {
			return nil, &toolFailure{status: session.ToolExecutionStatusNotFound, err: fmt.Errorf("tool %q not found", call.Function.Name)}
		}
		var args map[string]any
		if call.Function.Arguments != "" {
			if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
				return nil, &toolFailure{status: session.ToolExecutionStatusInvalidArguments, err: fmt.Errorf("error parsing arguments: %v", err)}
			}
		}
		callCtx, cancel := context.WithTimeout(ctx, a.cfg.ToolTimeout)
		defer cancel()
		return tool.Call(callCtx, args)
	})

	execution := session.ToolExecution{CallID: tc.ID, Name: tc.Function.Name, Status: session.ToolExecutionStatusSuccess, DurationMS: time.Since(startedAt).Milliseconds()}
	var content string
	if callErr != nil {
		execution.Status = session.ToolExecutionStatusFailed
		var failure *toolFailure
		if errors.As(callErr, &failure) {
			execution.Status = failure.status
		}
		execution.Error = callErr.Error()
		content = a.renderToolError(tc, execution.Status, callErr)
	} else {
		content = a.renderToolResult(result)
	}
	em.emit(Event{Type: EventTypeToolResult, ToolCall: &call, ToolResult: content})
	return toolOutcome{
		message: models.Message{
			Role:       models.RoleTool,
			ToolCallID: tc.ID,
			Name:       tc.Function.Name,
			Content:    content,
		},
		execution: execution,
	}
}

// renderToolError produces the model-facing failure text. Hosts redact
// internal details through AgentConfig.ToolErrorFormatter.
func (a *Agent) renderToolError(call models.ToolCall, status session.ToolExecutionStatus, err error) string {
	if a.cfg.ToolErrorFormatter != nil {
		return a.cfg.ToolErrorFormatter(call, status, err)
	}
	return "error: " + truncateBytes(err.Error(), maxToolErrorBytes)
}

// renderToolResult serializes a result and enforces MaxToolResultBytes so
// one oversized result cannot blow the context window.
func (a *Agent) renderToolResult(result any) string {
	content := toolResultToString(result)
	limit := a.cfg.MaxToolResultBytes
	if limit == 0 {
		limit = defaultMaxToolResultBytes
	}
	if limit < 0 || len(content) <= limit {
		return content
	}
	return truncateBytes(content, limit) + fmt.Sprintf("\n…[truncated %d bytes]", len(content)-limit)
}

// truncateBytes cuts s to at most n bytes on a UTF-8 boundary.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// prepareManagedTool freezes one tool call into an execution plan via the
// managed ToolService and reports whether it needs approval.
func (a *Agent) prepareManagedTool(ctx context.Context, call *models.ToolCall) (service.PlanRef, bool, error) {
	var args json.RawMessage
	if call.Function.Arguments != "" {
		args = json.RawMessage(call.Function.Arguments)
	}
	prepared, err := a.cfg.ToolService.Prepare(ctx, service.PrepareRequest{
		ToolName:  call.Function.Name,
		Arguments: args,
	})
	if err != nil {
		return service.PlanRef{}, false, err
	}
	return prepared.Ref, prepared.ApprovalRequired, nil
}

// executeManagedPlan executes a frozen plan and normalizes the result.
func (a *Agent) executeManagedPlan(ctx context.Context, ref service.PlanRef) (any, error) {
	result, err := a.cfg.ToolService.Execute(ctx, ref)
	if err != nil {
		return nil, err
	}
	if result.IsError {
		if result.Error != nil {
			return nil, fmt.Errorf("%s", result.Error.Message)
		}
		return nil, fmt.Errorf("tool execution failed")
	}
	return result.Content, nil
}

// executeManagedTool routes one tool invocation through the managed
// ToolService (Prepare -> Execute), so authorization, approval, and audit all
// flow through the single governed execution path (RC-05). Approval-required
// tools fail closed here; the pause/resume bridge uses the split
// prepare/execute steps instead.
func (a *Agent) executeManagedTool(ctx context.Context, call *models.ToolCall) (any, error) {
	callCtx, cancel := context.WithTimeout(ctx, a.cfg.ToolTimeout)
	defer cancel()
	ref, approvalRequired, err := a.prepareManagedTool(callCtx, call)
	if err != nil {
		return nil, err
	}
	if approvalRequired {
		return nil, service.ErrApprovalRequired
	}
	return a.executeManagedPlan(callCtx, ref)
}

func toolResultToString(result any) string {
	if result == nil {
		return ""
	}
	if s, ok := result.(string); ok {
		return s
	}
	b, err := json.Marshal(result)
	if err != nil {
		return fmt.Sprintf("%v", result)
	}
	return string(b)
}

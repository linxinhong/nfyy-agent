// Package team implements multi-agent team orchestration.
package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/linxinhong/xingding-agent-runtime/pkg/agent"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

const delegateToolName = "delegate_to_member"

// delegateArgs is the single-argument shape accepted by the delegate tool.
type delegateArgs struct {
	MemberID string `json:"member_id"`
	Task     string `json:"task"`
}

// delegateSchema is the function schema of the delegate tool, derived once
// from a typed function so it matches the argument struct.
var delegateSchema = sync.OnceValues(func() (map[string]any, error) {
	fn, err := tools.NewFunction(delegateToolName, "Delegate a task to a member agent and return its response.",
		func(context.Context, delegateArgs) (string, error) { return "", nil })
	if err != nil {
		return nil, fmt.Errorf("create delegate tool: %w", err)
	}
	return fn.Schema(), nil
})

// delegateTool is the coordinator's tool for one team run. It runs members
// as children of the team run and, when streaming, forwards their events
// into the team stream after the coordinator events that precede the call.
type delegateTool struct {
	team   *Team
	parent *session.RunOutput
	schema map[string]any
	// emit forwards member events; nil for synchronous runs.
	emit func(agent.Event) bool
	// forwarded is signalled by the stream translator each time it has
	// forwarded the coordinator's tool_started event for this tool, so a
	// member's events never overtake the coordinator's own.
	forwarded chan struct{}
}

func (d *delegateTool) Name() string { return delegateToolName }
func (d *delegateTool) Description() string {
	return "Delegate a task to a member agent and return its response."
}
func (d *delegateTool) Schema() map[string]any { return d.schema }

func (d *delegateTool) Call(ctx context.Context, raw map[string]any) (any, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var args delegateArgs
	if err := json.Unmarshal(encoded, &args); err != nil {
		return nil, fmt.Errorf("invalid delegate arguments: %w", err)
	}
	member := d.team.findMember(args.MemberID)
	if member == nil {
		return nil, fmt.Errorf("member %q not found", args.MemberID)
	}
	ctx, cancel := context.WithTimeout(ctx, d.team.cfg.DelegationTimeout)
	defer cancel()
	request := session.RunRequest{
		Input: session.RunInput{Message: args.Task}, UserID: d.parent.UserID,
		ParentRunID: d.parent.RunID, NodeID: d.parent.NodeID, Metadata: d.parent.Metadata,
	}
	if d.emit == nil {
		output, err := member.RunWith(ctx, request)
		if err != nil {
			return nil, err
		}
		if output == nil {
			return "", nil
		}
		return output.Content, nil
	}

	// Delegations run one at a time (the tool is not read-only), so each
	// invocation pairs with the next forwarded tool_started.
	select {
	case <-d.forwarded:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	events, err := member.RunStreamWith(ctx, request)
	if err != nil {
		return nil, err
	}
	var result string
	var memberErr error
	completed := false
	for ev := range events {
		d.emit(ev)
		if ev.Type == agent.EventTypeRunCompleted && ev.RunOutput != nil {
			result = ev.RunOutput.Content
			completed = true
		}
		if ev.Type == agent.EventTypeRunError && ev.Err != nil {
			memberErr = ev.Err
		}
	}
	if memberErr == nil && !completed {
		memberErr = errors.New("member stream ended without a completed run")
	}
	if memberErr != nil {
		return nil, memberErr
	}
	return result, nil
}

func (t *Team) findMember(memberID string) *agent.Agent {
	for _, m := range t.cfg.Members {
		if m.ID() == memberID || m.Name() == memberID {
			return m
		}
	}
	return nil
}

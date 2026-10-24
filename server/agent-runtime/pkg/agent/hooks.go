// Package agent implements the core Agent runtime, including configuration,
// execution, streaming, and guardrails.
package agent

import (
	"context"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

// Hooks holds optional lifecycle hooks for an Agent run.
type Hooks struct {
	// PreRun is called before the run loop starts.
	PreRun PreRunHook
	// PostRun is called after the run loop finishes.
	PostRun PostRunHook
	// PreTool is called before a tool is executed.
	PreTool PreToolHook
	// PostTool is called after a tool is executed.
	PostTool PostToolHook
}

// PreRunHook is called before the run loop starts.
type PreRunHook interface {
	PreRun(ctx context.Context, input *session.RunInput) error
}

// PostRunHook is called after the run loop finishes.
type PostRunHook interface {
	PostRun(ctx context.Context, output *session.RunOutput) error
}

// PreToolHook is called before a tool is executed.
type PreToolHook interface {
	PreTool(ctx context.Context, call *models.ToolCall) error
}

// PostToolHook is called after a tool is executed.
type PostToolHook interface {
	PostTool(ctx context.Context, call *models.ToolCall, result any, err error) error
}

func (a *Agent) runPreRunHooks(ctx context.Context, input *session.RunInput) error {
	if a.cfg.Hooks == nil || a.cfg.Hooks.PreRun == nil {
		return nil
	}
	return a.cfg.Hooks.PreRun.PreRun(ctx, input)
}

func (a *Agent) runPostRunHooks(ctx context.Context, output *session.RunOutput) error {
	if a.cfg.Hooks == nil || a.cfg.Hooks.PostRun == nil {
		return nil
	}
	return a.cfg.Hooks.PostRun.PostRun(ctx, output)
}

func (a *Agent) runPreToolHooks(ctx context.Context, call *models.ToolCall) error {
	if a.cfg.Hooks == nil || a.cfg.Hooks.PreTool == nil {
		return nil
	}
	return a.cfg.Hooks.PreTool.PreTool(ctx, call)
}

func (a *Agent) runPostToolHooks(ctx context.Context, call *models.ToolCall, result any, err error) error {
	if a.cfg.Hooks == nil || a.cfg.Hooks.PostTool == nil {
		return nil
	}
	return a.cfg.Hooks.PostTool.PostTool(ctx, call, result, err)
}

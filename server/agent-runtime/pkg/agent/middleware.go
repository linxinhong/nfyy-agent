// Package agent implements the core Agent runtime, including configuration,
// execution, streaming, and guardrails.
package agent

import (
	"context"

	"github.com/linxinhong/xingding-agent-runtime/pkg/models"
)

// RunFunc is the signature of a single synchronous model call in the agent
// run loop. RunMiddleware implementations wrap this function, allowing them
// to inspect or modify messages and options before the call, intercept the
// response or error, and chain with other middlewares.
type RunFunc = func(ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error)

// StreamFunc is the streaming variant of RunFunc.
type StreamFunc = func(ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error)

// RunMiddleware wraps a single synchronous model-call step. Implementations
// receive the messages and options about to be sent to the model and must
// forward to next (or short-circuit by returning without calling next).
//
// A type that implements both RunMiddleware and StreamMiddleware can intercept
// model calls uniformly across the sync and streaming code paths.
type RunMiddleware interface {
	Run(next RunFunc, ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error)
}

// StreamMiddleware wraps a single streaming model-call step.
type StreamMiddleware interface {
	RunStream(next StreamFunc, ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error)
}

// RunMiddlewareFunc adapts a plain function to RunMiddleware.
type RunMiddlewareFunc func(next RunFunc, ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error)

// Run calls the underlying function.
func (f RunMiddlewareFunc) Run(next RunFunc, ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
	return f(next, ctx, messages, opts)
}

// StreamMiddlewareFunc adapts a plain function to StreamMiddleware.
type StreamMiddlewareFunc func(next StreamFunc, ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error)

// RunStream calls the underlying function.
func (f StreamMiddlewareFunc) RunStream(next StreamFunc, ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error) {
	return f(next, ctx, messages, opts)
}

// ToolMiddleware intercepts individual tool invocations during the run loop.
// Unlike the older Hooks pattern, PreTool may modify or replace the ToolCall
// (e.g. rewriting arguments) before execution, and PostTool sees both the
// result and any error from the tool.
type ToolMiddleware interface {
	PreTool(ctx context.Context, call *models.ToolCall) (*models.ToolCall, error)
	PostTool(ctx context.Context, call *models.ToolCall, result any, err error) error
}

// buildRunChain composes a slice of RunMiddleware into a single RunFunc by
// wrapping base from the inside out: the first middleware in the slice is
// outermost and runs first.
func buildRunChain(middlewares []RunMiddleware, base RunFunc) RunFunc {
	for i := len(middlewares) - 1; i >= 0; i-- {
		mw := middlewares[i]
		inner := base
		base = func(ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
			return mw.Run(inner, ctx, messages, opts)
		}
	}
	return base
}

// buildStreamChain composes a slice of StreamMiddleware into a single
// StreamFunc, analogous to buildRunChain.
func buildStreamChain(middlewares []StreamMiddleware, base StreamFunc) StreamFunc {
	for i := len(middlewares) - 1; i >= 0; i-- {
		mw := middlewares[i]
		inner := base
		base = func(ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error) {
			return mw.RunStream(inner, ctx, messages, opts)
		}
	}
	return base
}

// runToolChain runs the PreTool chain in order, then executes the tool, then
// runs the PostTool chain in reverse order (mirroring stack semantics).
func runToolChain(middlewares []ToolMiddleware, ctx context.Context, call *models.ToolCall, execute func(context.Context, *models.ToolCall) (any, error)) (any, error) {
	currentCall := call
	for _, mw := range middlewares {
		modified, err := mw.PreTool(ctx, currentCall)
		if err != nil {
			return nil, err
		}
		if modified != nil {
			currentCall = modified
		}
	}

	result, execErr := execute(ctx, currentCall)

	for i := len(middlewares) - 1; i >= 0; i-- {
		if err := middlewares[i].PostTool(ctx, currentCall, result, execErr); err != nil {
			if execErr == nil {
				execErr = err
			}
		}
	}

	return result, execErr
}

// wrappedCallModel builds the run middleware chain on each call with
// a.callModel as the innermost function, then invokes it. When no run
// middlewares are configured the method delegates directly to a.callModel.
func (a *Agent) wrappedCallModel(ctx context.Context, messages []models.Message, opts models.ModelOptions) (*models.ModelResponse, error) {
	if len(a.runMWs) == 0 {
		return a.callModel(ctx, messages, opts)
	}
	return buildRunChain(a.runMWs, RunFunc(a.callModel))(ctx, messages, opts)
}

// wrappedCallModelStream builds the stream middleware chain analogously.
func (a *Agent) wrappedCallModelStream(ctx context.Context, messages []models.Message, opts models.ModelOptions) (<-chan models.StreamEvent, error) {
	if len(a.streamMWs) == 0 {
		return a.callModelStream(ctx, messages, opts)
	}
	return buildStreamChain(a.streamMWs, StreamFunc(a.callModelStream))(ctx, messages, opts)
}

// hooksAsToolMiddleware adapts the legacy Hooks into a ToolMiddleware.
func hooksAsToolMiddleware(hooks *Hooks) ToolMiddleware {
	if hooks == nil {
		return nil
	}
	return &hooksToolAdapter{hooks: hooks}
}

type hooksToolAdapter struct {
	hooks *Hooks
}

func (a *hooksToolAdapter) PreTool(ctx context.Context, call *models.ToolCall) (*models.ToolCall, error) {
	if a.hooks.PreTool == nil {
		return call, nil
	}
	if err := a.hooks.PreTool.PreTool(ctx, call); err != nil {
		return nil, err
	}
	return call, nil
}

func (a *hooksToolAdapter) PostTool(ctx context.Context, call *models.ToolCall, result any, err error) error {
	if a.hooks.PostTool == nil {
		return nil
	}
	return a.hooks.PostTool.PostTool(ctx, call, result, err)
}

// MergeToolMiddlewares combines legacy Hooks (if present) with explicit
// ToolMiddlewares, with explicit middlewares wrapping outside.
func MergeToolMiddlewares(hooks *Hooks, middlewares []ToolMiddleware) []ToolMiddleware {
	legacy := hooksAsToolMiddleware(hooks)
	if legacy == nil {
		return middlewares
	}
	return append([]ToolMiddleware{legacy}, middlewares...)
}

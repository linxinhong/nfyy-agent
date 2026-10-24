// Package server implements an MCP (Model Context Protocol) server that
// exposes tools, resources, and prompts over JSON-RPC 2.0 transports.
//
// The server wraps existing pkg/tools.Tool instances and converts their schemas
// to MCP-compatible format automatically. Resources and prompts follow the same
// registration pattern.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/linxinhong/xingding-agent-runtime/pkg/mcp"
	"github.com/linxinhong/xingding-agent-runtime/pkg/observability"
	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

// MethodHandler is a function that processes a JSON-RPC request.
type MethodHandler func(ctx context.Context, params map[string]any) (any, error)

// Server exposes local tools, resources, and prompts over an MCP transport.
type Server struct {
	name    string
	version string
	inst    mcp.ImplementationInfo

	toolkit       *tools.Toolkit
	resources     map[string]mcp.Resource
	prompts       map[string]mcp.Prompt
	resHandler    mcp.ResourceHandler
	promptHandler mcp.PromptHandler

	handlers map[string]MethodHandler
	mu       sync.RWMutex
}

// ServerOption customizes a Server.
type ServerOption func(*Server)

// WithInstructions sets optional instructions text returned during initialize.
func WithInstructions(instructions string) ServerOption {
	return func(s *Server) {
		s.inst = mcp.ImplementationInfo{
			Name:    s.name,
			Version: s.version,
		}
	}
}

// NewServer creates an MCP server.
//
//	name, version are reported to clients during initialize.
func NewServer(name, version string, opts ...ServerOption) *Server {
	s := &Server{
		name:    name,
		version: version,
		inst: mcp.ImplementationInfo{
			Name:    name,
			Version: version,
		},
		toolkit:   tools.NewToolkit(name),
		resources: make(map[string]mcp.Resource),
		prompts:   make(map[string]mcp.Prompt),
	}
	s.handlers = map[string]MethodHandler{
		mcp.MethodInitialize:    s.handleInitialize,
		mcp.MethodPing:          s.handlePing,
		mcp.MethodToolsList:     s.handleToolsList,
		mcp.MethodToolsCall:     s.handleToolsCall,
		mcp.MethodResourcesList: s.handleResourcesList,
		mcp.MethodResourcesRead: s.handleResourcesRead,
		mcp.MethodPromptsList:   s.handlePromptsList,
		mcp.MethodPromptsGet:    s.handlePromptsGet,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// RegisterTool adds a single tool to the server.
func (s *Server) RegisterTool(t tools.Tool) {
	s.toolkit.Add(t)
}

// annotatedTool carries trust annotations next to a tools.Tool so the
// tools/list handler can expose them to clients.
type annotatedTool struct {
	tools.Tool
	annotations *mcp.ToolAnnotations
}

// MCPAnnotations returns the tool's trust annotations.
func (t *annotatedTool) MCPAnnotations() *mcp.ToolAnnotations {
	return t.annotations
}

// RegisterToolWithAnnotations registers a tool together with its trust
// annotations, which are exposed to clients in tools/list. A nil
// annotations argument behaves like RegisterTool.
func (s *Server) RegisterToolWithAnnotations(t tools.Tool, annotations *mcp.ToolAnnotations) {
	if annotations == nil {
		s.toolkit.Add(t)
		return
	}
	s.toolkit.Add(&annotatedTool{Tool: t, annotations: annotations})
}

// RegisterToolkit adds all tools from a toolkit to the server.
func (s *Server) RegisterToolkit(tk *tools.Toolkit) {
	for _, t := range tk.List() {
		s.toolkit.Add(t)
	}
}

// RegisterResource adds a static resource. For dynamic resources, use RegisterResourceHandler.
func (s *Server) RegisterResource(r mcp.Resource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resources[r.URI] = r
}

// RegisterResourceHandler sets a handler that serves resource content on demand.
func (s *Server) RegisterResourceHandler(h mcp.ResourceHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resHandler = h
}

// RegisterPrompt adds a prompt template.
func (s *Server) RegisterPrompt(p mcp.Prompt) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts[p.Name] = p
}

// RegisterPromptHandler sets a handler that resolves prompt templates on demand.
func (s *Server) RegisterPromptHandler(h mcp.PromptHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.promptHandler = h
}

// Serve starts the server loop on the given transport. It blocks until the
// transport returns an error or ctx is cancelled.
func (s *Server) Serve(ctx context.Context, transport mcp.Transport) error {
	if err := transport.Start(ctx); err != nil {
		return fmt.Errorf("mcp server: start transport: %w", err)
	}
	defer transport.Close()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		raw, err := transport.Receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("mcp server: receive: %w", err)
		}

		go s.processMessage(ctx, transport, raw)
	}
}

// processMessage handles a single JSON-RPC message on its own goroutine.
func (s *Server) processMessage(ctx context.Context, transport mcp.Transport, raw json.RawMessage) {
	// Parse as request.
	var req mcp.Request
	if err := json.Unmarshal(raw, &req); err != nil {
		s.sendError(ctx, transport, nil, mcp.ErrParse, "parse error: "+err.Error())
		return
	}

	// Validate jsonrpc field.
	if req.JSONRPC != "2.0" {
		s.sendError(ctx, transport, req.ID, mcp.ErrInvalidRequest, "invalid jsonrpc version")
		return
	}

	// Notifications have no id — process and discard.
	if req.ID == nil {
		s.handleNotification(ctx, &req)
		return
	}

	// Route to handler.
	handler, ok := s.handlers[req.Method]
	if !ok {
		s.sendError(ctx, transport, req.ID, mcp.ErrMethodNotFound, "method not found: "+req.Method)
		return
	}

	result, err := handler(ctx, req.Params)
	if err != nil {
		s.sendError(ctx, transport, req.ID, mcp.ErrInternal, err.Error())
		return
	}

	resp := mcp.NewSuccessResponse(req.ID, result)
	if sendErr := transport.Send(ctx, resp); sendErr != nil {
		observability.Event(ctx, slog.LevelError, "mcp.server.send_response", observability.Err(sendErr))
	}
}

func (s *Server) sendError(ctx context.Context, transport mcp.Transport, id any, code int, message string) {
	resp := mcp.NewErrorResponse(id, code, message)
	if id == nil {
		// Notification errors are not sent per JSON-RPC spec — just log.
		observability.Event(ctx, slog.LevelWarn, "mcp.server.notification_error",
			slog.Int("code", code), slog.String("message", message))
		return
	}
	if err := transport.Send(context.Background(), resp); err != nil {
		observability.Event(ctx, slog.LevelError, "mcp.server.send_error_response", observability.Err(err))
	}
}

func (s *Server) handleNotification(ctx context.Context, req *mcp.Request) {
	if req.Method == mcp.MethodInitialized {
		observability.Event(ctx, slog.LevelInfo, "mcp.server.initialized")
	}
	// Other notifications are silently accepted.
}

// Tools returns the list of registered tools. Useful for introspection.
func (s *Server) Tools() []tools.Tool {
	return s.toolkit.List()
}

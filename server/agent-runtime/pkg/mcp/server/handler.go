package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/linxinhong/xingding-agent-runtime/pkg/mcp"
)

// ---------------------------------------------------------------------------
// initialize
// ---------------------------------------------------------------------------

func (s *Server) handleInitialize(ctx context.Context, params map[string]any) (any, error) {
	capabilities := mcp.ServerCapabilities{
		Tools:     &mcp.ToolsCapability{},
		Resources: &mcp.ResourcesCapability{},
		Prompts:   &mcp.PromptsCapability{},
	}

	s.mu.RLock()
	hasResources := len(s.resources) > 0 || s.resHandler != nil
	hasPrompts := len(s.prompts) > 0 || s.promptHandler != nil
	s.mu.RUnlock()

	if !hasResources {
		capabilities.Resources = nil
	}
	if !hasPrompts {
		capabilities.Prompts = nil
	}

	return mcp.InitializeResponse{
		ProtocolVersion: mcp.ProtocolVersion,
		Capabilities:    capabilities,
		ServerInfo:      s.inst,
	}, nil
}

// ---------------------------------------------------------------------------
// ping
// ---------------------------------------------------------------------------

func (s *Server) handlePing(ctx context.Context, params map[string]any) (any, error) {
	return map[string]any{}, nil
}

// ---------------------------------------------------------------------------
// tools/list
// ---------------------------------------------------------------------------

func (s *Server) handleToolsList(ctx context.Context, params map[string]any) (any, error) {
	toolList := s.toolkit.List()
	mcpTools := make([]mcp.ToolDef, 0, len(toolList))
	for _, t := range toolList {
		mcpTools = append(mcpTools, toolToMCPDef(t))
	}
	return mcp.ToolsListResult{Tools: mcpTools}, nil
}

// ---------------------------------------------------------------------------
// tools/call
// ---------------------------------------------------------------------------

func (s *Server) handleToolsCall(ctx context.Context, params map[string]any) (any, error) {
	name, _ := params["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("missing required parameter 'name'")
	}

	tool, ok := s.toolkit.Get(name)
	if !ok {
		return nil, fmt.Errorf("tool %q not found", name)
	}

	args, _ := params["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}

	result, err := tool.Call(ctx, args)
	if err != nil {
		// MCP tools/call returns content with isError=true for tool errors.
		return mcp.ToolsCallResult{
			Content: []mcp.ContentBlock{mcp.NewTextContent(err.Error())},
			IsError: true,
		}, nil
	}

	return mcp.ToolsCallResult{
		Content: formatToolResult(result),
		IsError: false,
	}, nil
}

// ---------------------------------------------------------------------------
// resources/list
// ---------------------------------------------------------------------------

func (s *Server) handleResourcesList(ctx context.Context, params map[string]any) (any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	resources := make([]mcp.Resource, 0, len(s.resources))
	for _, r := range s.resources {
		resources = append(resources, r)
	}
	return mcp.ResourcesListResult{Resources: resources}, nil
}

// ---------------------------------------------------------------------------
// resources/read
// ---------------------------------------------------------------------------

func (s *Server) handleResourcesRead(ctx context.Context, params map[string]any) (any, error) {
	uri, _ := params["uri"].(string)
	if uri == "" {
		return nil, fmt.Errorf("missing required parameter 'uri'")
	}

	s.mu.RLock()
	handler := s.resHandler
	s.mu.RUnlock()

	if handler != nil {
		return handler(ctx, uri)
	}

	// Static resource fallback — return the resource metadata as content.
	s.mu.RLock()
	r, ok := s.resources[uri]
	s.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("resource %q not found", uri)
	}

	return mcp.ResourcesReadResult{
		Contents: []mcp.ResourceContent{{
			URI:      r.URI,
			MimeType: r.MimeType,
			Text:     r.Description,
		}},
	}, nil
}

// ---------------------------------------------------------------------------
// prompts/list
// ---------------------------------------------------------------------------

func (s *Server) handlePromptsList(ctx context.Context, params map[string]any) (any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	prompts := make([]mcp.Prompt, 0, len(s.prompts))
	for _, p := range s.prompts {
		prompts = append(prompts, p)
	}
	return mcp.PromptsListResult{Prompts: prompts}, nil
}

// ---------------------------------------------------------------------------
// prompts/get
// ---------------------------------------------------------------------------

func (s *Server) handlePromptsGet(ctx context.Context, params map[string]any) (any, error) {
	name, _ := params["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("missing required parameter 'name'")
	}
	args, _ := params["arguments"].(map[string]any)

	s.mu.RLock()
	handler := s.promptHandler
	s.mu.RUnlock()

	if handler != nil {
		return handler(ctx, name, args)
	}

	s.mu.RLock()
	p, ok := s.prompts[name]
	s.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("prompt %q not found", name)
	}

	// Return the prompt template itself as a single assistant message.
	return mcp.PromptsGetResult{
		Description: p.Description,
		Messages: []mcp.PromptMessage{{
			Role: "assistant",
			Content: mcp.PromptContent{
				Type: "text",
				Text: p.Description,
			},
		}},
	}, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// toolToMCPDef converts a tools.Tool.Schema() (OpenAI format) to an MCP ToolDef.
func toolToMCPDef(t interface {
	Name() string
	Description() string
	Schema() map[string]any
}) mcp.ToolDef {
	schema := t.Schema()
	fn, _ := schema["function"].(map[string]any)
	name, _ := fn["name"].(string)
	desc, _ := fn["description"].(string)
	params, _ := fn["parameters"].(map[string]any)

	def := mcp.ToolDef{
		Name:        name,
		Description: desc,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}
	if ann, ok := t.(interface{ MCPAnnotations() *mcp.ToolAnnotations }); ok {
		def.Annotations = ann.MCPAnnotations()
	}
	if params != nil {
		if p, ok := params["properties"]; ok {
			def.InputSchema["properties"] = p
		}
		if r, ok := params["required"]; ok {
			def.InputSchema["required"] = r
		}
		if t, ok := params["type"]; ok {
			def.InputSchema["type"] = t
		}
	}
	return def
}

// formatToolResult converts a tool call result into MCP content blocks.
func formatToolResult(result any) []mcp.ContentBlock {
	if result == nil {
		return []mcp.ContentBlock{mcp.NewTextContent("")}
	}
	switch v := result.(type) {
	case string:
		return []mcp.ContentBlock{mcp.NewTextContent(v)}
	case []byte:
		return []mcp.ContentBlock{mcp.NewTextContent(string(v))}
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return []mcp.ContentBlock{mcp.NewTextContent(fmt.Sprintf("%v", v))}
		}
		return []mcp.ContentBlock{mcp.NewTextContent(string(b))}
	}
}

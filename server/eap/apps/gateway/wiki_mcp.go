package main

// Knowledge MCP server (POST /mcp/wiki): the local agent's query tools over
// the center knowledge base — search, read a page, list the tree.
//
// Why: local agents speak MCP, so the center answers queries as a
// Streamable-HTTP MCP server instead of a bespoke client. It is built in,
// not a registry-proxied upstream (/mcp/{server}): the tools are the
// /enterprise/v1/wiki reads, with the same caller authentication (employee
// key or session) and the same content authorization, so a tool never
// returns what the reader could not open on the website.
//
// Pitfalls:
//   - Every tool result passes the employee egress DLP scan, as for proxied
//     MCP tools; a tool error is an isError result, not a JSON-RPC error.
//   - Results are query answers for one conversation; nothing here writes.
//   - Notifications (no id) are acknowledged with 202 and no body, as the
//     Streamable-HTTP transport requires.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"enterprise-agent-platform/packages/content"
	"enterprise-agent-platform/packages/errs"
	mcp "enterprise-agent-platform/packages/protocol-mcp"
)

const wikiMCPInstructions = "知识服务中心知识库：检索并阅读本组织沉淀的、你有权访问的 Wiki 页面。回答涉及经验、裁判观点、办案要点时先用 search_wiki 检索，再用 read_wiki_page 阅读原文并注明页面路径。"

type wikiMCPTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

var wikiMCPTools = []wikiMCPTool{
	{Name: "search_wiki", Description: "在中心知识库中检索有权访问的已发布页面，返回页面 id、路径、标题、一句话结论与片段。",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "检索词"},
			"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 20, "description": "最多返回条数，默认 8"},
		}, "required": []string{"query"}}},
	{Name: "read_wiki_page", Description: "按页面 id 或路径（书架/书/章/页）阅读页面全文（Markdown），附标签、可打开的站内链接与来源投稿。",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"page_id": map[string]any{"type": "string"},
			"path":    map[string]any{"type": "string", "description": "书架/书/章/页"},
		}}},
	{Name: "list_wiki_tree", Description: "列出知识库目录（书架、书、章）与有权访问的已发布页面路径。",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
}

// handleWikiMCP implements POST /mcp/wiki.
func (s *Server) handleWikiMCP(w http.ResponseWriter, r *http.Request) {
	c, r, ok := s.requireContentCaller(w, r)
	if !ok {
		return
	}
	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := decodeJSONBody(w, r, &envelope); err != nil {
		writeProblem(w, r, err)
		return
	}
	if envelope.JSONRPC != "2.0" || envelope.Method == "" {
		writeMCPRPCError(w, r, envelope.ID, mcp.CodeInvalidRequest, "invalid JSON-RPC 2.0 envelope")
		return
	}
	if len(envelope.ID) == 0 || string(envelope.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch envelope.Method {
	case "initialize":
		if sid := mcpSessionID(); sid != "" {
			w.Header().Set("Mcp-Session-Id", sid)
		}
		writeMCPResult(w, r, envelope.ID, map[string]any{
			"protocolVersion": mcp.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "mindlink-wiki", "version": Version},
			"instructions":    wikiMCPInstructions,
		})
	case "ping":
		writeMCPResult(w, r, envelope.ID, map[string]any{})
	case "tools/list":
		writeMCPResult(w, r, envelope.ID, map[string]any{"tools": wikiMCPTools})
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(envelope.Params, &params); err != nil {
			writeMCPRPCError(w, r, envelope.ID, mcp.CodeInvalidParams, "tools/call params must be an object")
			return
		}
		value, err := s.callWikiTool(r, c, params.Name, params.Arguments)
		if err == errUnknownWikiTool {
			writeMCPRPCError(w, r, envelope.ID, mcp.CodeInvalidParams, "unknown tool: "+params.Name)
			return
		}
		text, isError := "", false
		if err != nil {
			text, isError = toolErrorText(err), true
		} else {
			raw, _ := json.Marshal(value)
			text = s.dlpScanEmployeeText(r, c.session, string(raw))
		}
		writeMCPResult(w, r, envelope.ID, map[string]any{
			"content": []mcp.TextContent{{Type: "text", Text: text}},
			"isError": isError,
		})
	default:
		writeMCPRPCError(w, r, envelope.ID, mcp.CodeMethodNotFound, "method not supported by this server")
	}
}

var errUnknownWikiTool = fmt.Errorf("unknown wiki tool")

func argString(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return strings.TrimSpace(v)
}

func toolErrorText(err error) string {
	if problem, ok := errs.From(err); ok {
		return string(problem.Code) + ": " + problem.Message
	}
	return "tool failed"
}

func (s *Server) callWikiTool(r *http.Request, c contentCaller, name string, args map[string]any) (any, error) {
	ws := c.session.Workspace.WorkspaceID
	switch name {
	case "search_wiki":
		limit := 8
		if n, ok := args["limit"].(float64); ok && n >= 1 && n <= 20 {
			limit = int(n)
		}
		hits, err := s.Content.Search(r.Context(), ws, c.actor, argString(args, "query"), limit)
		if err != nil {
			return nil, contentProblem(err)
		}
		return map[string]any{"results": hits}, nil
	case "read_wiki_page":
		id := argString(args, "page_id")
		if id == "" {
			resolved, err := s.Content.ResolvePath(r.Context(), ws, argString(args, "path"))
			if err != nil {
				return nil, contentProblem(err)
			}
			id = resolved
		}
		item, err := s.Content.Get(r.Context(), ws, c.actor, id)
		if err == nil && item.Kind != "wiki" {
			err = content.ErrNotFound
		}
		if err != nil {
			return nil, contentProblem(err)
		}
		page, err := s.buildPage(r, c, item)
		if err != nil {
			return nil, err
		}
		// The agent reads Markdown; the rendered HTML is for browsers only.
		page.HTML = ""
		return page, nil
	case "list_wiki_tree":
		nodes, err := s.Wiki.Tree(r.Context(), ws)
		if err != nil {
			return nil, wikiProblem(err)
		}
		pages, err := s.Content.TreePages(r.Context(), ws, c.actor)
		if err != nil {
			return nil, contentProblem(err)
		}
		return map[string]any{"nodes": nodes, "pages": pages}, nil
	default:
		return nil, errUnknownWikiTool
	}
}

package client

import (
	"context"
	"fmt"

	"github.com/linxinhong/xingding-agent-runtime/pkg/tools"
)

// DiscoverAndCache fetches the server's tool list and caches each tool as an
// MCPTool ready for use by agents.
func (c *Client) DiscoverAndCache(ctx context.Context) error {
	defs, err := c.ListTools(ctx)
	if err != nil {
		return fmt.Errorf("discover tools: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.tools = make(map[string]*MCPTool, len(defs))
	for _, def := range defs {
		c.tools[def.Name] = NewMCPTool(c, def)
	}
	return nil
}

// Tools returns all cached MCP tools. Call DiscoverAndCache first.
func (c *Client) Tools() []*MCPTool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]*MCPTool, 0, len(c.tools))
	for _, t := range c.tools {
		out = append(out, t)
	}
	return out
}

// GetTool returns a cached tool by name, or nil if not found.
func (c *Client) GetTool(name string) *MCPTool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tools[name]
}

// AsToolkit exports all cached MCP tools as a tools.Toolkit for easy
// integration with agent configuration.
func (c *Client) AsToolkit() *tools.Toolkit {
	tk := tools.NewToolkit("mcp")
	for _, t := range c.Tools() {
		tk.Add(t)
	}
	return tk
}

// AsToolList exports all cached MCP tools as a []tools.Tool slice, suitable
// for assignment to AgentConfig.Tools.
func (c *Client) AsToolList() []tools.Tool {
	ts := c.Tools()
	out := make([]tools.Tool, len(ts))
	for i, t := range ts {
		out[i] = t
	}
	return out
}

// ---------------------------------------------------------------------------
// Pool — manages multiple MCP client connections
// ---------------------------------------------------------------------------

// Pool manages multiple MCP client connections with optional tool namespacing.
type Pool struct {
	clients map[string]*Client
	prefix  bool // whether to prefix tool names with server name
}

// NewPool creates an empty client pool.
func NewPool() *Pool {
	return &Pool{
		clients: make(map[string]*Client),
		prefix:  true, // namespacing on by default to avoid collisions
	}
}

// Add registers a client under a server name, performing initialize and
// tool discovery.
func (p *Pool) Add(ctx context.Context, name string, transport ClientTransport) (*Client, error) {
	client := NewClient(transport)
	if err := client.Initialize(ctx); err != nil {
		transport.Close()
		return nil, fmt.Errorf("pool add %q: %w", name, err)
	}
	if err := client.DiscoverAndCache(ctx); err != nil {
		client.Close()
		return nil, fmt.Errorf("pool add %q: %w", name, err)
	}
	p.clients[name] = client
	return client, nil
}

// Get returns a client by server name.
func (p *Pool) Get(name string) *Client {
	return p.clients[name]
}

// AllTools collects tools from all connected MCP servers into a single toolkit.
// When prefixing is enabled, tool names become "server::toolname" so tools from
// different servers never collide.
func (p *Pool) AllTools() *tools.Toolkit {
	tk := tools.NewToolkit("mcp-pool")
	for name, client := range p.clients {
		for _, t := range client.Tools() {
			if p.prefix {
				tk.Add(&prefixedTool{MCPTool: t, prefix: name + "::"})
			} else {
				tk.Add(t)
			}
		}
	}
	return tk
}

// Close shuts down all clients in the pool.
func (p *Pool) Close() error {
	var errs []error
	for name, client := range p.clients {
		if err := client.Close(); err != nil {
			errs = append(errs, fmt.Errorf("pool close %q: %w", name, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("pool close errors: %v", errs)
	}
	return nil
}

// prefixedTool wraps an MCPTool with a namespaced name to avoid collisions
// across multiple MCP servers.
type prefixedTool struct {
	*MCPTool
	prefix string
}

// Name returns the prefixed tool name.
func (t *prefixedTool) Name() string {
	return t.prefix + t.MCPTool.Name()
}

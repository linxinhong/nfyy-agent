// Package tools defines the tool abstraction used by agents.
package tools

import "context"

// Tool is anything an agent can invoke.
type Tool interface {
	// Name returns the unique tool name.
	Name() string
	// Description returns a short description for the model.
	Description() string
	// Schema returns the JSON-serializable tool definition.
	// For function tools this is typically {"type":"function","function":{...}}.
	Schema() map[string]any
	// Call executes the tool with the provided arguments.
	Call(ctx context.Context, args map[string]any) (any, error)
}

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"enterprise-agent-platform/packages/mcpgateway"
)

// ListToolsForWorkspace returns the registry tools visible to one workspace.
// The visibility filter IS the execution policy: discovery and execution use
// the same predicate (xingding-mcpa semantics, ADR-0005).
func (s *Store) ListToolsForWorkspace(ctx context.Context, server, workspaceID string) ([]mcpgateway.RegisteredTool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, server, description, risk_level, workspaces, endpoint_url, endpoint_method, COALESCE(credential_ref, '')
		FROM mcp_tools
		WHERE server = $1 AND workspaces @> ARRAY[$2::text]
		ORDER BY name`, server, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list mcp tools: %w", err)
	}
	defer rows.Close()
	var out []mcpgateway.RegisteredTool
	for rows.Next() {
		tool, scanErr := scanTool(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, tool)
	}
	return out, rows.Err()
}

// GetToolForWorkspace resolves one tool only when the workspace may execute
// it; any other case is ErrNotFound (fail closed, no existence leak).
func (s *Store) GetToolForWorkspace(ctx context.Context, name, server, workspaceID string) (mcpgateway.RegisteredTool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT name, server, description, risk_level, workspaces, endpoint_url, endpoint_method, COALESCE(credential_ref, '')
		FROM mcp_tools
		WHERE name = $1 AND server = $2 AND workspaces @> ARRAY[$3::text]`, name, server, workspaceID)
	return scanTool(row)
}

func scanTool(row rowScanner) (mcpgateway.RegisteredTool, error) {
	var t mcpgateway.RegisteredTool
	err := row.Scan(&t.Name, &t.Server, &t.Description, &t.RiskLevel, &t.Workspaces, &t.EndpointURL, &t.EndpointMethod, &t.CredentialRef)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return mcpgateway.RegisteredTool{}, ErrNotFound
		}
		return mcpgateway.RegisteredTool{}, fmt.Errorf("scan mcp tool: %w", err)
	}
	return t, nil
}

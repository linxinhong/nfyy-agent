package store

import (
	"context"
	"fmt"
)

// ReleasedSkill is the latest released skill version visible to a workspace
// (capability discovery, TASK-034). Descriptions come from the skill.yaml
// payload; V1 surfaces the name itself.
type ReleasedSkill struct {
	Name    string
	Version string
}

// ListReleasedSkillsForWorkspace returns the latest released version of each
// skill visible to the workspace.
func (s *Store) ListReleasedSkillsForWorkspace(ctx context.Context, workspaceID string) ([]ReleasedSkill, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (name) name, version
		FROM (
			SELECT sp.name, sp.version, string_to_array(sp.version, '.')::int[] AS v
			FROM skill_packages sp
			WHERE sp.workspace_id = $1 AND sp.status = 'released' AND sp.visibility = 'organization' AND sp.protected_body IS NULL
		) t
		ORDER BY name, v DESC`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list released skills: %w", err)
	}
	defer rows.Close()
	out := []ReleasedSkill{}
	for rows.Next() {
		var e ReleasedSkill
		if err := rows.Scan(&e.Name, &e.Version); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ResolveReleasedSkill verifies an exact released skill version (empty
// version = latest released) is visible to the workspace.
func (s *Store) ResolveReleasedSkill(ctx context.Context, workspaceID, name, version string) (ReleasedSkill, error) {
	var out ReleasedSkill
	if version != "" {
		err := s.pool.QueryRow(ctx, `
			SELECT name, version FROM skill_packages
			WHERE name = $1 AND version = $2 AND workspace_id = $3 AND status = 'released'
			  AND visibility = 'organization' AND protected_body IS NULL`,
			name, version, workspaceID).Scan(&out.Name, &out.Version)
		return out, err
	}
	err := s.pool.QueryRow(ctx, `
		SELECT name, version FROM skill_packages
		WHERE name = $1 AND workspace_id = $2 AND status = 'released'
		  AND visibility = 'organization' AND protected_body IS NULL
		ORDER BY string_to_array(version, '.')::int[] DESC
		LIMIT 1`, name, workspaceID).Scan(&out.Name, &out.Version)
	return out, err
}

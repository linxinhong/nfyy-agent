// Package capability implements the EAP Capability Gateway (TASK-034,
// ADR-0023): one governed discovery+execution abstraction over every
// enterprise capability. MCP tools and Skills are the first sources; the
// design follows Den's composite registry (studied at
// ~/git/openwork/ee/apps/den-api/src/mcp/) with EAP's differentiation:
// permission-aware discovery and a full governance chain on execution.
package capability

import (
	"context"
	"sort"
	"strings"
)

// Status is the permission-aware availability vocabulary. It is UX and
// planning information only — execution revalidates everything; a status
// of available is never an authorization token.
const (
	StatusAvailable      = "available"
	StatusNeedsApproval  = "needs_approval" // R2+ risk: approval continuation required
	StatusDeniedByPolicy = "denied_by_policy"
	StatusDisabled       = "disabled"
)

// Risk thresholds: capabilities at or above this level require an approval
// continuation before execution (mirrors the MCP gateway's R2+ gate).
const ApprovalRiskFloor = "R2"

// Capability is one governed enterprise capability as seen by an agent.
type Capability struct {
	Name        string `json:"name"` // mcp:<server>:<tool> | skill:<name>[@version]
	Description string `json:"description"`
	Type        string `json:"type"`       // mcp_tool | skill
	RiskLevel   string `json:"risk_level"` // R0..R5 (R0 = informational/skill)
	Status      string `json:"status"`     // availability vocabulary above
	InputHint   string `json:"input_hint"` // short argument guidance

	score int `json:"-"`
}

// SearchQuery constrains a discovery call.
type SearchQuery struct {
	Text  string
	Limit int
}

// Subject is the caller's governance context for discovery and execution.
type Subject struct {
	UserID      string
	WorkspaceID string
	Roles       []string
}

// ExecuteInput carries one capability invocation.
type ExecuteInput struct {
	Name          string         // exact capability name
	Args          map[string]any // MCP tool arguments
	ApprovalToken string         // one-time continuation for R2+ capabilities
}

// ExecuteResult is the governed outcome.
type ExecuteResult struct {
	Content   string // released (DLP-scanned) result text
	RiskLevel string
	Kind      string // mcp_tool | skill
}

// Source is one capability origin (ADR-0023: composite registry, each source
// adapts existing governed control data — never a second source of truth).
type Source interface {
	Kind() string
	// Search returns capabilities visible to the subject, already filtered
	// by workspace authority and annotated with policy/risk status.
	Search(ctx context.Context, sub Subject, q SearchQuery) ([]Capability, error)
	// Parse resolves an exact capability name; nil when the source does not
	// own the name.
	Parse(ctx context.Context, sub Subject, name string) (*Parsed, error)
	// Execute runs the full governance chain for a parsed capability.
	Execute(ctx context.Context, sub Subject, parsed *Parsed, in ExecuteInput) (ExecuteResult, error)
}

// Parsed is a name resolved against one source, carrying the governance
// metadata the execute chain needs.
type Parsed struct {
	Source Kind
	Name   string
	Risk   string
}

// Kind identifies source implementations.
type Kind string

const (
	KindMCP   Kind = "mcp"
	KindSkill Kind = "skill"
)

// Registry fans search across sources and dispatches execution by prefix.
type Registry struct {
	Sources []Source
}

// Search merges every source's permission-filtered results by score.
func (r *Registry) Search(ctx context.Context, sub Subject, q SearchQuery) ([]Capability, error) {
	if q.Limit <= 0 || q.Limit > 20 {
		q.Limit = 5
	}
	tokens := Tokenize(q.Text)
	var all []Capability
	for _, src := range r.Sources {
		caps, err := src.Search(ctx, sub, q)
		if err != nil {
			return nil, err
		}
		all = append(all, caps...)
	}
	// Empty query means "list everything I can use" (ADR-0023 refinement):
	// sources already applied the workspace/policy filters, so return the
	// visible capabilities ordered by name instead of an empty result.
	if len(tokens) == 0 {
		sort.SliceStable(all, func(a, b int) bool { return all[a].Name < all[b].Name })
		if len(all) > q.Limit {
			all = all[:q.Limit]
		}
		return all, nil
	}
	// Re-score against the query (sources already workspace/policy filtered;
	// scoring orders relevance) and cut to the limit.
	for i := range all {
		all[i].score = ScoreCapability(all[i], tokens)
	}
	sort.SliceStable(all, func(a, b int) bool {
		if all[a].score != all[b].score {
			return all[a].score > all[b].score
		}
		return all[a].Name < all[b].Name
	})
	out := make([]Capability, 0, q.Limit)
	for _, c := range all {
		if c.score <= 0 {
			continue
		}
		out = append(out, c)
		if len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

// Execute resolves the name across sources (first Parse wins) and runs the
// owning source's governance chain. Unknown names return ErrUnknown.
func (r *Registry) Execute(ctx context.Context, sub Subject, in ExecuteInput) (ExecuteResult, error) {
	for _, src := range r.Sources {
		parsed, err := src.Parse(ctx, sub, in.Name)
		if err != nil {
			return ExecuteResult{}, err
		}
		if parsed == nil {
			continue
		}
		return src.Execute(ctx, sub, parsed, in)
	}
	return ExecuteResult{}, &Error{Code: "unknown_capability", RetrySearch: true,
		Message: "No capability named " + in.Name + ". Call search_capabilities to find a valid name."}
}

// Error is the structured capability error taxonomy with retry guidance.
type Error struct {
	Code         string // unknown_capability | forbidden | needs_approval | policy_blocked | invalid_arguments | internal
	Message      string
	RetrySearch  bool   `json:"retry_search"` // hint: call search_capabilities again
	RetryAction  string `json:"retry_action"` // e.g. "request_approval"
	ApprovalHint string `json:"approval_hint,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// ErrUnknown builds the canonical unknown-capability error.
func ErrUnknown(name string) *Error {
	return &Error{Code: "unknown_capability", Message: "No capability named " + name + ".", RetrySearch: true}
}

// Tokenize splits a query/tool name into lowercase tokens; camelCase
// boundaries split too so "organization" matches "getOrganizations"
// (Den's algorithm).
func Tokenize(s string) []string {
	var out []string
	for _, raw := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		out = append(out, raw)
	}
	return out
}

// TokenizeName additionally splits camelCase and colon/underscore segments
// so mcp:gitlab:git_read_file contributes {mcp gitlab git read file}.
func TokenizeName(s string) []string {
	var out []string
	for _, tok := range Tokenize(s) {
		var cur strings.Builder
		prevLower := false
		for _, r := range tok {
			isDigit := r >= '0' && r <= '9'
			isLower := r >= 'a' && r <= 'z'
			if (r >= 'A' && r <= 'Z') && prevLower {
				if cur.Len() > 0 {
					out = append(out, cur.String())
					cur.Reset()
				}
			}
			cur.WriteRune(r)
			prevLower = isLower || isDigit
		}
		if cur.Len() > 0 {
			out = append(out, cur.String())
		}
	}
	return out
}

// ScoreCapability ranks one capability against query tokens: name tokens
// +5 (exact) / +3 (prefix), description tokens +2, extra (type) +1.
func ScoreCapability(c Capability, queryTokens []string) int {
	if c.score != 0 {
		return c.score
	}
	nameTokens := TokenizeName(c.Name)
	descTokens := Tokenize(c.Description)
	typeTokens := Tokenize(c.Type)
	score := 0
	for _, q := range queryTokens {
		hit := false
		for _, n := range nameTokens {
			if n == q {
				score += 5
				hit = true
				break
			}
			if len(q) > 1 && strings.HasPrefix(n, q) {
				score += 3
				hit = true
				break
			}
		}
		if hit {
			continue
		}
		for _, d := range descTokens {
			if d == q {
				score += 2
				hit = true
				break
			}
		}
		if hit {
			continue
		}
		for _, t := range typeTokens {
			if t == q {
				score++
				break
			}
		}
	}
	return score
}

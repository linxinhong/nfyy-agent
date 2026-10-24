// Package rbac turns a member's Workspace roles and team roles into the
// permissions every MindLink authorization check asks about.
//
// Why: law firms name and split duties their own way, so roles are data an
// admin edits; code only ever asks "may this member do P (in this team)?".
// The permission catalog is code, because each permission is a check
// somewhere in the gateway.
//
// Invariants and pitfalls:
//   - Role keys stay the identifiers other EAP layers already key on (Policy
//     and DLP except_roles, IdP group mappings), so built-in keys never change
//     and an unknown key simply grants nothing.
//   - The built-in admin role is locked to every permission: an edit can never
//     leave a Workspace without someone able to manage roles and members.
//   - Team roles hold only team-grantable permissions and apply only inside
//     the member's own group; Workspace roles apply everywhere.
//   - Assigning a role needs every permission it grants (Covers), otherwise
//     members.manage alone would escalate to anything.
package rbac

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Permission is one checkable capability.
type Permission string

const (
	PlatformAdmin   Permission = "platform.admin"   // models, usage, MCP, DLP, audit consoles
	MembersManage   Permission = "members.manage"   // members, teams, local accounts
	RolesManage     Permission = "roles.manage"     // define and edit roles
	KnowledgeManage Permission = "knowledge.manage" // wiki structure, publishing into any team
	KnowledgeReview Permission = "knowledge.review" // review content and curation proposals
	CurationSweep   Permission = "curation.sweep"   // trigger a global re-curation
	SkillReview     Permission = "skill.review"     // approve and release skills
	SkillRequire    Permission = "skill.require"    // mark skills required for members
)

// PermissionInfo describes one permission for role editors.
type PermissionInfo struct {
	Key           Permission `json:"key"`
	TeamGrantable bool       `json:"team_grantable"`
}

// Permissions is the full catalog in display order.
var Permissions = []PermissionInfo{
	{PlatformAdmin, false}, {MembersManage, false}, {RolesManage, false},
	{KnowledgeManage, false}, {KnowledgeReview, true}, {CurationSweep, false},
	{SkillReview, true}, {SkillRequire, true},
}

const (
	ScopeWorkspace = "workspace"
	ScopeTeam      = "team"
)

// Built-in role keys.
const (
	RoleAdmin     = "admin"
	RoleReviewer  = "reviewer"
	RoleDeveloper = "developer" // the base member role
	RoleLead      = "lead"      // team lead
)

// Role is one named set of permissions granted at one scope.
type Role struct {
	Key         string       `json:"key"`
	Name        string       `json:"name"`
	Scope       string       `json:"scope"`
	Permissions []Permission `json:"permissions"`
	Builtin     bool         `json:"builtin"`
	Locked      bool         `json:"locked"`
}

func all() []Permission {
	out := make([]Permission, 0, len(Permissions))
	for _, p := range Permissions {
		out = append(out, p.Key)
	}
	return out
}

// Builtin returns the default roles every Workspace starts with.
func Builtin() []Role {
	return []Role{
		{Key: RoleAdmin, Name: "管理员", Scope: ScopeWorkspace, Permissions: all(), Builtin: true, Locked: true},
		{Key: RoleReviewer, Name: "审核人", Scope: ScopeWorkspace, Permissions: []Permission{KnowledgeReview, SkillReview}, Builtin: true},
		{Key: "maintainer", Name: "技能维护人", Scope: ScopeWorkspace, Permissions: []Permission{SkillReview}, Builtin: true},
		{Key: "auditor", Name: "审计员", Scope: ScopeWorkspace, Permissions: []Permission{}, Builtin: true},
		{Key: RoleDeveloper, Name: "成员", Scope: ScopeWorkspace, Permissions: []Permission{}, Builtin: true},
		{Key: RoleLead, Name: "团队负责人", Scope: ScopeTeam, Permissions: []Permission{KnowledgeReview, SkillReview, SkillRequire}, Builtin: true},
	}
}

// Merge applies a Workspace's stored roles over the built-ins: a stored row
// with a built-in key edits it (except locked ones), any other row is custom.
func Merge(stored []Role) []Role {
	out := Builtin()
	index := map[string]int{}
	for i, r := range out {
		index[r.Key] = i
	}
	for _, r := range stored {
		if i, ok := index[r.Key]; ok {
			if !out[i].Locked {
				out[i].Name, out[i].Permissions = r.Name, r.Permissions
			}
			continue
		}
		r.Builtin, r.Locked = false, false
		out = append(out, r)
	}
	return out
}

// Find returns the role with key from catalog.
func Find(catalog []Role, key string) (Role, bool) {
	for _, r := range catalog {
		if r.Key == key {
			return r, true
		}
	}
	return Role{}, false
}

var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

var (
	ErrInvalid = errors.New("invalid role")
	ErrLocked  = errors.New("role is locked")
)

// Validate checks a role definition against the catalog it will join.
func Validate(catalog []Role, r Role) error {
	if !keyPattern.MatchString(r.Key) {
		return fmt.Errorf("%w: key must match %s", ErrInvalid, keyPattern)
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(r.Name)); n < 1 || n > 40 {
		return fmt.Errorf("%w: name must be 1-40 characters", ErrInvalid)
	}
	if r.Scope != ScopeWorkspace && r.Scope != ScopeTeam {
		return fmt.Errorf("%w: scope must be workspace or team", ErrInvalid)
	}
	if existing, ok := Find(catalog, r.Key); ok {
		if existing.Locked {
			return ErrLocked
		}
		if existing.Scope != r.Scope {
			return fmt.Errorf("%w: a role's scope cannot change", ErrInvalid)
		}
	}
	for _, other := range catalog {
		if other.Key != r.Key && other.Name == strings.TrimSpace(r.Name) {
			return fmt.Errorf("%w: another role is named %q", ErrInvalid, r.Name)
		}
	}
	for _, p := range r.Permissions {
		info, ok := lookup(p)
		if !ok {
			return fmt.Errorf("%w: unknown permission %q", ErrInvalid, p)
		}
		if r.Scope == ScopeTeam && !info.TeamGrantable {
			return fmt.Errorf("%w: %q cannot be granted inside a team", ErrInvalid, p)
		}
	}
	return nil
}

func lookup(p Permission) (PermissionInfo, bool) {
	for _, info := range Permissions {
		if info.Key == p {
			return info, true
		}
	}
	return PermissionInfo{}, false
}

// Normalize trims the name and dedupes permissions in catalog order.
func Normalize(r Role) Role {
	r.Name = strings.TrimSpace(r.Name)
	perms := []Permission{}
	for _, info := range Permissions {
		if slices.Contains(r.Permissions, info.Key) {
			perms = append(perms, info.Key)
		}
	}
	r.Permissions = perms
	return r
}

// Set is a set of permissions.
type Set map[Permission]bool

func setOf(catalog []Role, keys []string, scope string) Set {
	s := Set{}
	for _, key := range keys {
		if r, ok := Find(catalog, key); ok && r.Scope == scope {
			for _, p := range r.Permissions {
				s[p] = true
			}
		}
	}
	return s
}

// Grants are one member's effective permissions in a Workspace.
type Grants struct {
	Workspace Set
	Team      Set
	GroupID   string // "" when the member belongs to no team
}

// Resolve computes grants from the member's role keys.
func Resolve(catalog []Role, roles, teamRoles []string, groupID string) Grants {
	g := Grants{Workspace: setOf(catalog, roles, ScopeWorkspace), GroupID: groupID, Team: Set{}}
	if groupID != "" {
		g.Team = setOf(catalog, teamRoles, ScopeTeam)
	}
	return g
}

// Of grants exactly perms Workspace-wide (service actors, tests).
func Of(perms ...Permission) Grants {
	s := Set{}
	for _, p := range perms {
		s[p] = true
	}
	return Grants{Workspace: s, Team: Set{}}
}

// Has reports a Workspace-wide permission.
func (g Grants) Has(p Permission) bool { return g.Workspace[p] }

// HasIn reports a permission over content or skills of groupID.
func (g Grants) HasIn(p Permission, groupID string) bool {
	return g.Workspace[p] || (groupID != "" && groupID == g.GroupID && g.Team[p])
}

// HasAny reports p at either scope (e.g. whether to show a review queue).
func (g Grants) HasAny(p Permission) bool {
	return g.Workspace[p] || (g.GroupID != "" && g.Team[p])
}

// Covers reports whether g holds every permission r grants at r's scope —
// the condition for assigning r to someone else.
func (g Grants) Covers(r Role) bool {
	for _, p := range r.Permissions {
		if !g.Workspace[p] {
			return false
		}
	}
	return true
}

// List returns the Workspace and team permissions in catalog order.
func (g Grants) List() (workspace, team []Permission) {
	workspace, team = []Permission{}, []Permission{}
	for _, info := range Permissions {
		if g.Workspace[info.Key] {
			workspace = append(workspace, info.Key)
		}
		if g.Team[info.Key] {
			team = append(team, info.Key)
		}
	}
	return workspace, team
}

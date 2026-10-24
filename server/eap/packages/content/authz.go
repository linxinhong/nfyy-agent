// Package content stores shared news, Wiki and capability notes with a
// draft → submitted → approved/rejected review flow, scoped to one Workspace.
//
// Authorization is a pure function over the actor and server-side resource
// facts. Queries may narrow candidates for speed, but Allowed is the final
// boundary: every row a query returns is re-checked before it leaves.
//
// Pitfalls:
//   - knowledge.review granted through a team role stops at the member's own
//     group and at team-visible content; granted Workspace-wide it covers all.
//   - Nobody reviews their own resource, admins included.
package content

import "enterprise-agent-platform/packages/rbac"

// Action is one resource operation.
type Action string

const (
	Create   Action = "create"
	Read     Action = "read"
	Edit     Action = "edit"
	Submit   Action = "submit"
	Review   Action = "review"
	Withdraw Action = "withdraw"
)

const (
	VisibilityTeam         = "team"
	VisibilityOrganization = "organization"
)

// Actor is the authenticated member inside the selected Workspace.
type Actor struct {
	UserID  string
	Grants  rbac.Grants
	GroupID string // "" when the member belongs to no group
	// CiteContributions lets a service actor cite any contribution of the
	// Workspace in a revision's source chain. Only the curation store sets
	// it, after checking every source is the contribution its job is about.
	CiteContributions bool
}

// Facts are the server-side resource attributes a decision depends on.
type Facts struct {
	OwnerID    string
	Visibility string
	GroupID    string
	Published  bool
}

// broad reports Workspace-wide review rights: every team's content is in reach.
func (a Actor) broad() bool { return a.Grants.Has(rbac.KnowledgeReview) }

func (a Actor) inGroup(groupID string) bool {
	return a.GroupID != "" && a.GroupID == groupID
}

// CanReviewAny reports whether the actor may see a review queue at all.
func (a Actor) CanReviewAny() bool {
	return a.Grants.HasAny(rbac.KnowledgeReview)
}

// Allowed decides one action on one resource.
func Allowed(a Actor, action Action, f Facts) bool {
	if a.UserID == "" {
		return false
	}
	switch action {
	case Create:
		switch f.Visibility {
		case VisibilityOrganization:
			return true
		case VisibilityTeam:
			return a.Grants.Has(rbac.KnowledgeManage) || a.inGroup(f.GroupID)
		}
		return false
	case Read:
		if a.UserID == f.OwnerID || Allowed(a, Review, f) {
			return true
		}
		if !f.Published {
			return false
		}
		return f.Visibility == VisibilityOrganization ||
			(f.Visibility == VisibilityTeam && a.inGroup(f.GroupID))
	case Edit, Submit:
		return a.UserID == f.OwnerID
	case Review:
		if a.UserID == f.OwnerID {
			return false
		}
		if a.broad() {
			return true
		}
		return f.Visibility == VisibilityTeam && a.inGroup(f.GroupID) && a.Grants.HasIn(rbac.KnowledgeReview, f.GroupID)
	case Withdraw:
		return a.UserID == f.OwnerID || Allowed(a, Review, f)
	}
	return false
}

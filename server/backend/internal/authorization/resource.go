// Package authorization evaluates current actor and resource facts. Role
// grants are loaded from PostgreSQL for each authenticated request.
package authorization

import "yuanpu.dev/server/internal/identity"

type Action string

const (
	Create   Action = "create"
	Read     Action = "read"
	Edit     Action = "edit"
	Submit   Action = "submit"
	Review   Action = "review"
	Withdraw Action = "withdraw"
)

type ResourceFacts struct {
	OwnerID    string
	Visibility string
	TeamID     *string
	Published  bool
	Status     string
}

// Live requests carry database grants. These defaults support isolated policy
// tests and mirror the initial migration values.
var defaultGrants = map[string]map[string]string{
	"admin":     {"create": "all", "read": "all", "edit": "own", "submit": "own", "review": "all", "withdraw": "all"},
	"reviewer":  {"create": "all", "read": "all", "edit": "own", "submit": "own", "review": "all", "withdraw": "none"},
	"team_lead": {"create": "all", "read": "team", "edit": "own", "submit": "own", "review": "team", "withdraw": "none"},
	"member":    {"create": "all", "read": "team", "edit": "own", "submit": "own", "review": "none", "withdraw": "none"},
}

func grant(user identity.User, action Action) string {
	if user.Permissions != nil {
		return user.Permissions[string(action)]
	}
	return defaultGrants[user.Role][string(action)]
}

func inScope(user identity.User, scope string, resource ResourceFacts) bool {
	switch scope {
	case "all":
		return true
	case "team":
		return sameTeam(user.TeamID, resource.TeamID)
	case "own":
		return resource.OwnerID != "" && user.ID == resource.OwnerID
	}
	return false
}

func Allowed(user identity.User, action Action, resource ResourceFacts) bool {
	if !user.Active {
		return false
	}
	scope := grant(user, action)
	if scope == "" || scope == "none" {
		return false
	}
	switch action {
	case Create:
		return (resource.Visibility == "organization" && (scope == "all" || scope == "own")) ||
			(resource.Visibility == "team" && sameTeam(user.TeamID, resource.TeamID))
	case Read:
		if user.ID == resource.OwnerID {
			return true
		}
		if resource.Status == "submitted" && Allowed(user, Review, resource) {
			return true
		}
		if !resource.Published {
			return false
		}
		return (resource.Visibility == "organization" && scope != "own") ||
			(resource.Visibility == "team" && inScope(user, scope, resource))
	case Edit:
		return inScope(user, scope, resource)
	case Submit:
		return resource.OwnerID == user.ID && inScope(user, scope, resource)
	case Review:
		return user.ID != resource.OwnerID &&
			(scope == "all" || (scope == "team" && resource.Visibility == "team" && sameTeam(user.TeamID, resource.TeamID)))
	case Withdraw:
		return inScope(user, scope, resource)
	default:
		return false
	}
}

func CanReviewAny(user identity.User) bool {
	return user.Active && grant(user, Review) != "" && grant(user, Review) != "none"
}

func sameTeam(a, b *string) bool {
	return a != nil && b != nil && *a != "" && *a == *b
}

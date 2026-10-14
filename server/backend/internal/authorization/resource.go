// Package authorization evaluates current actor and resource facts. Database
// queries may narrow candidates, but this policy remains the final boundary.
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
}

func Allowed(user identity.User, action Action, resource ResourceFacts) bool {
	if !user.Active {
		return false
	}
	switch action {
	case Create:
		if resource.Visibility == "organization" {
			return true
		}
		return resource.Visibility == "team" &&
			(user.Role == "admin" || sameTeam(user.TeamID, resource.TeamID))
	case Read:
		if user.ID == resource.OwnerID || Allowed(user, Review, resource) {
			return true
		}
		if !resource.Published {
			return false
		}
		return resource.Visibility == "organization" ||
			(resource.Visibility == "team" && sameTeam(user.TeamID, resource.TeamID))
	case Edit, Submit:
		return user.ID == resource.OwnerID
	case Review:
		if user.ID == resource.OwnerID {
			return false
		}
		if user.Role == "admin" || user.Role == "reviewer" {
			return true
		}
		return user.Role == "team_lead" && resource.Visibility == "team" &&
			sameTeam(user.TeamID, resource.TeamID)
	case Withdraw:
		return user.ID == resource.OwnerID || Allowed(user, Review, resource)
	default:
		return false
	}
}

func CanReviewAny(user identity.User) bool {
	return user.Active && (user.Role == "admin" || user.Role == "reviewer" || user.Role == "team_lead")
}

func sameTeam(a, b *string) bool {
	return a != nil && b != nil && *a != "" && *a == *b
}

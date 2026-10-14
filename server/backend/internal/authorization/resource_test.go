package authorization

import (
	"testing"

	"yuanpu.dev/server/internal/identity"
)

func TestResourceScopeAndReviewBoundary(t *testing.T) {
	teamA, teamB := "team-a", "team-b"
	owner := identity.User{ID: "owner", Role: "member", TeamID: &teamA, Active: true}
	reader := identity.User{ID: "reader", Role: "member", TeamID: &teamA, Active: true}
	outsider := identity.User{ID: "outsider", Role: "team_lead", TeamID: &teamB, Active: true}
	reviewer := identity.User{ID: "reviewer", Role: "reviewer", TeamID: &teamB, Active: true}
	facts := ResourceFacts{OwnerID: owner.ID, Visibility: "team", TeamID: &teamA}
	if !Allowed(owner, Read, facts) || Allowed(reader, Read, facts) || Allowed(outsider, Read, facts) {
		t.Fatal("draft visibility crossed the author boundary")
	}
	facts.Published = true
	if !Allowed(reader, Read, facts) || Allowed(outsider, Read, facts) || !Allowed(reviewer, Read, facts) {
		t.Fatal("published team visibility or reviewer access is wrong")
	}
	if Allowed(owner, Review, facts) || Allowed(outsider, Review, facts) || !Allowed(reviewer, Review, facts) {
		t.Fatal("reviewer scope or self-review is wrong")
	}
	if Allowed(reader, Withdraw, facts) || !Allowed(owner, Withdraw, facts) {
		t.Fatal("withdrawal did not follow ownership")
	}
	owner.Active = false
	if Allowed(owner, Read, facts) || Allowed(owner, Edit, facts) || Allowed(owner, Withdraw, facts) {
		t.Fatal("deactivated owner kept resource access")
	}
	if Allowed(reader, Action("unknown"), facts) {
		t.Fatal("unknown action was allowed")
	}
}

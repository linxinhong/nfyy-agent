package content

import (
	"testing"

	"enterprise-agent-platform/packages/rbac"
)

// member resolves an actor from built-in role keys, as ActorFor does.
func member(id string, roles []string, group string, teamRoles ...string) Actor {
	g := rbac.Resolve(rbac.Builtin(), roles, teamRoles, group)
	return Actor{UserID: id, Grants: g, GroupID: group}
}

func TestAllowedMatrix(t *testing.T) {
	dev := []string{"developer"}
	owner := member("u-owner", dev, "g-a")
	teammate := member("u-mate", dev, "g-a")
	outsider := member("u-out", dev, "g-b")
	lead := member("u-lead", dev, "g-a", "lead")
	otherLead := member("u-lead-b", dev, "g-b", "lead")
	reviewer := member("u-rev", []string{"reviewer"}, "")
	admin := member("u-admin", []string{"admin"}, "")
	anonymous := Actor{}

	team := Facts{OwnerID: "u-owner", Visibility: VisibilityTeam, GroupID: "g-a"}
	teamPublished := team
	teamPublished.Published = true
	org := Facts{OwnerID: "u-owner", Visibility: VisibilityOrganization}
	orgPublished := org
	orgPublished.Published = true

	cases := []struct {
		name   string
		actor  Actor
		action Action
		facts  Facts
		want   bool
	}{
		{"anyone creates org content", outsider, Create, org, true},
		{"group member creates team content", teammate, Create, team, true},
		{"outsider cannot create for another team", outsider, Create, team, false},
		{"admin creates for any team", admin, Create, team, true},
		{"draft hidden from teammate", teammate, Read, team, false},
		{"owner reads own draft", owner, Read, team, true},
		{"teammate reads published team content", teammate, Read, teamPublished, true},
		{"outsider never reads team content", outsider, Read, teamPublished, false},
		{"outsider reads published org content", outsider, Read, orgPublished, true},
		{"reviewer reads drafts to review them", reviewer, Read, team, true},
		{"only owner edits", teammate, Edit, team, false},
		{"owner submits", owner, Submit, team, true},
		{"owner never reviews own resource", member("u-owner", []string{"admin"}, ""), Review, team, false},
		{"lead reviews own team", lead, Review, team, true},
		{"lead cannot review other team", otherLead, Review, team, false},
		{"lead cannot review org content", lead, Review, org, false},
		{"reviewer reviews org content", reviewer, Review, org, true},
		{"teammate cannot withdraw", teammate, Withdraw, teamPublished, false},
		{"lead withdraws team content", lead, Withdraw, teamPublished, true},
		{"anonymous denied", anonymous, Read, orgPublished, false},
		{"unknown action denied", admin, Action("delete"), org, false},
	}
	for _, c := range cases {
		if got := Allowed(c.actor, c.action, c.facts); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
	if member("x", dev, "", "lead").CanReviewAny() {
		t.Error("a team role without a group must not open the review queue")
	}
	custom := rbac.Merge([]rbac.Role{{Key: "team-reviewer", Name: "团队审核人", Scope: rbac.ScopeTeam,
		Permissions: []rbac.Permission{rbac.KnowledgeReview}}})
	g := rbac.Resolve(custom, dev, []string{"team-reviewer"}, "g-a")
	if !Allowed(Actor{UserID: "u-tr", Grants: g, GroupID: "g-a"}, Review, team) {
		t.Error("a custom team role with knowledge.review reviews its own team")
	}
	if Allowed(Actor{UserID: "u-tr", Grants: g, GroupID: "g-a"}, Create, Facts{Visibility: VisibilityTeam, GroupID: "g-b"}) {
		t.Error("team review rights do not publish into another team")
	}
}

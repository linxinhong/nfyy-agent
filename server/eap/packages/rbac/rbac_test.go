package rbac

import (
	"errors"
	"testing"
)

func TestMergeEditsBuiltinsButNotAdmin(t *testing.T) {
	catalog := Merge([]Role{
		{Key: RoleAdmin, Name: "所长", Scope: ScopeWorkspace, Permissions: nil},
		{Key: RoleReviewer, Name: "知识审核", Scope: ScopeWorkspace, Permissions: []Permission{KnowledgeReview}},
		{Key: "partner", Name: "合伙人", Scope: ScopeWorkspace, Permissions: []Permission{SkillRequire}, Builtin: true},
	})
	admin, _ := Find(catalog, RoleAdmin)
	if admin.Name != "管理员" || len(admin.Permissions) != len(Permissions) {
		t.Fatalf("admin stays locked: %+v", admin)
	}
	reviewer, _ := Find(catalog, RoleReviewer)
	if reviewer.Name != "知识审核" || len(reviewer.Permissions) != 1 || !reviewer.Builtin {
		t.Fatalf("built-in edit applies and stays built-in: %+v", reviewer)
	}
	partner, ok := Find(catalog, "partner")
	if !ok || partner.Builtin || partner.Locked {
		t.Fatalf("a stored row cannot claim to be built-in: %+v", partner)
	}
}

func TestValidate(t *testing.T) {
	catalog := Builtin()
	cases := []struct {
		name string
		role Role
		want error
	}{
		{"custom workspace role", Role{Key: "partner", Name: "合伙人", Scope: ScopeWorkspace, Permissions: []Permission{MembersManage}}, nil},
		{"custom team role", Role{Key: "team-reviewer", Name: "团队审核人", Scope: ScopeTeam, Permissions: []Permission{KnowledgeReview, SkillReview}}, nil},
		{"team role with workspace-only permission", Role{Key: "x", Name: "x", Scope: ScopeTeam, Permissions: []Permission{RolesManage}}, ErrInvalid},
		{"unknown permission", Role{Key: "x", Name: "x", Scope: ScopeWorkspace, Permissions: []Permission{"everything"}}, ErrInvalid},
		{"bad key", Role{Key: "Partner", Name: "x", Scope: ScopeWorkspace}, ErrInvalid},
		{"empty name", Role{Key: "x", Name: "  ", Scope: ScopeWorkspace}, ErrInvalid},
		{"bad scope", Role{Key: "x", Name: "x", Scope: "firm"}, ErrInvalid},
		{"admin is locked", Role{Key: RoleAdmin, Name: "管理员", Scope: ScopeWorkspace}, ErrLocked},
		{"built-in scope is fixed", Role{Key: RoleLead, Name: "团队负责人", Scope: ScopeWorkspace}, ErrInvalid},
		{"name taken by another role", Role{Key: "x", Name: "审核人", Scope: ScopeWorkspace}, ErrInvalid},
		{"renaming a role to its own name", Role{Key: RoleReviewer, Name: "审核人", Scope: ScopeWorkspace}, nil},
	}
	for _, c := range cases {
		if err := Validate(catalog, c.role); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v want %v", c.name, err, c.want)
		}
	}
}

func TestTeamGrantsStayInTheTeam(t *testing.T) {
	g := Resolve(Builtin(), []string{RoleDeveloper, "unknown-idp-role"}, []string{RoleLead}, "g-a")
	if g.Has(KnowledgeReview) || !g.HasIn(KnowledgeReview, "g-a") || g.HasIn(KnowledgeReview, "g-b") || g.HasIn(KnowledgeReview, "") {
		t.Fatalf("lead grants only inside g-a: %+v", g)
	}
	if !g.HasAny(SkillRequire) {
		t.Fatal("a team permission shows the entry point")
	}
	if Resolve(Builtin(), nil, []string{RoleLead}, "").HasAny(KnowledgeReview) {
		t.Fatal("team roles without a team grant nothing")
	}
	if Resolve(Builtin(), []string{RoleLead}, nil, "").Has(KnowledgeReview) {
		t.Fatal("a team role held as a Workspace role grants nothing")
	}
	reviewer := Resolve(Builtin(), []string{RoleReviewer}, nil, "")
	if !reviewer.HasIn(KnowledgeReview, "g-b") {
		t.Fatal("Workspace-wide review covers every team")
	}
}

func TestCoversPreventsEscalation(t *testing.T) {
	catalog := Merge([]Role{{Key: "staff", Name: "行政", Scope: ScopeWorkspace, Permissions: []Permission{MembersManage}}})
	staff := Resolve(catalog, []string{"staff"}, nil, "")
	reviewer, _ := Find(catalog, RoleReviewer)
	developer, _ := Find(catalog, RoleDeveloper)
	self, _ := Find(catalog, "staff")
	if staff.Covers(reviewer) || !staff.Covers(developer) || !staff.Covers(self) {
		t.Fatal("members.manage alone assigns only roles within its own rights")
	}
	admin := Resolve(catalog, []string{RoleAdmin}, nil, "")
	lead, _ := Find(catalog, RoleLead)
	if !admin.Covers(lead) {
		t.Fatal("admin assigns everything")
	}
}

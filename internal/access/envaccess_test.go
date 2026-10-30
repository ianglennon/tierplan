// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package access_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/ianglennon/coaming/internal/access"
	"github.com/ianglennon/coaming/internal/rbac"
)

// The environment Access tab's data (plan 03-20, AUTH-06, D-06, D-12, D-25).

func (f *ownedFixture) addTeam(t *testing.T, id string, members ...string) {
	t.Helper()
	f.exec(t, `INSERT INTO teams (id, name, created_at, updated_at) VALUES (?, ?, 1, 1)`, id, id)
	for _, m := range members {
		f.exec(t, `INSERT INTO team_members (team_id, user_id, created_at) VALUES (?, ?, 1)`, id, m)
	}
}

func personByID(people []access.EnvAccessPerson, id string) (access.EnvAccessPerson, bool) {
	for _, p := range people {
		if p.UserID == id {
			return p, true
		}
	}
	return access.EnvAccessPerson{}, false
}

func kinds(src []rbac.Source) []string {
	var out []string
	for _, s := range src {
		out = append(out, string(s.Kind))
	}
	return out
}

func TestEnvAccessListFoldsEverySourceIntoOneRowPerPersonAndAlwaysListsInstanceAdmins(t *testing.T) {
	f := newOwnedFixture(t)
	f.addEnv(t, "env-a", "Alpha")
	f.addEnv(t, "env-b", "Beta")
	f.addAdmin(t, "root")
	for _, u := range []string{"carol", "dave", "erin", "frank", "gone"} {
		f.addUser(t, u)
	}
	f.addTeam(t, "ops", "dave", "erin", "gone")
	f.addTeam(t, "idle")
	f.exec(t, `UPDATE users SET disabled = 1 WHERE id = 'gone'`)

	grant := func(in access.GrantInput) {
		t.Helper()
		if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, in); err != nil {
			t.Fatal(err)
		}
	}
	grant(envGrant("carol", "env-a", "admin"))
	grant(envGrant("dave", "env-a", "viewer"))
	grant(access.GrantInput{SubjectType: "team", SubjectID: "ops", Scope: access.ScopeEnv, EnvID: "env-a", Role: "operator", Extras: []string{"containers.exec"}})
	grant(access.GrantInput{SubjectType: "team", SubjectID: "idle", Scope: access.ScopeAll, Role: "viewer"})
	grant(access.GrantInput{SubjectType: "user", SubjectID: "frank", Scope: access.ScopeAll, Role: "viewer"})
	grant(envGrant("erin", "env-b", "admin")) // another environment: not listed here

	teams, people, err := f.svc.EnvAccessList(f.ctx, "env-a", "carol")
	if err != nil {
		t.Fatal(err)
	}

	// Teams: ops (operator, direct) then idle (viewer, all environments, no members).
	if len(teams) != 2 || teams[0].TeamID != "ops" || teams[1].TeamID != "idle" {
		t.Fatalf("teams = %+v", teams)
	}
	ops := teams[0]
	if ops.Role != "operator" || !slices.Equal(ops.Extras, []string{"containers.exec"}) || !slices.Equal(kinds(ops.Sources), []string{"direct"}) ||
		ops.GrantID == "" || ops.GrantRole != "operator" {
		t.Fatalf("ops = %+v", ops)
	}
	if len(ops.Members) != 2 || ops.Members[0].UserID != "dave" || ops.Members[1].UserID != "erin" || ops.Members[0].Role != "operator" {
		t.Fatalf("ops members = %+v, want the two who can sign in, with the role through the team", ops.Members)
	}
	idle := teams[1]
	if idle.GrantID != "" || !slices.Equal(kinds(idle.Sources), []string{"all"}) || idle.Role != "viewer" || len(idle.Members) != 0 {
		t.Fatalf("idle = %+v, want a read-only row from the all-environments grant", idle)
	}

	// People: the instance admin and the env Admin first, then the rest.
	carol, _ := personByID(people, "carol")
	if !carol.Self || carol.Role != "admin" || carol.GrantID == "" || !slices.Equal(kinds(carol.Sources), []string{"direct"}) {
		t.Fatalf("carol = %+v", carol)
	}
	root, ok := personByID(people, "root")
	if !ok || !root.InstanceAdmin || root.Role != "admin" || root.GrantID != "" || !slices.Equal(kinds(root.Sources), []string{"instance"}) ||
		len(root.Extras) < 4 || root.Self {
		t.Fatalf("root = %+v, want Admin with every extra from the instance and no grant to edit", root)
	}
	dave, _ := personByID(people, "dave")
	if dave.Role != "operator" || !slices.Equal(dave.Extras, []string{"containers.exec"}) || !slices.Equal(kinds(dave.Sources), []string{"team", "direct"}) ||
		dave.GrantID == "" || dave.GrantRole != "viewer" {
		t.Fatalf("dave = %+v, want the team's Operator over the direct Viewer, both sources, and the direct grant to edit", dave)
	}
	erin, _ := personByID(people, "erin")
	if erin.Role != "operator" || !slices.Equal(kinds(erin.Sources), []string{"team"}) || erin.GrantID != "" {
		t.Fatalf("erin = %+v, want Operator through the team only (her Admin is on env-b)", erin)
	}
	frank, _ := personByID(people, "frank")
	if frank.Role != "viewer" || !slices.Equal(kinds(frank.Sources), []string{"all"}) || frank.GrantID != "" {
		t.Fatalf("frank = %+v", frank)
	}
	if _, listed := personByID(people, "gone"); listed {
		t.Fatal("a disabled user holds nothing and must not be listed")
	}
	var order []string
	for _, p := range people {
		order = append(order, p.UserID)
	}
	if !slices.Equal(order, []string{"carol", "root", "dave", "erin", "frank"}) {
		t.Fatalf("order = %v, want by role (Admin, Operator, Viewer) then name", order)
	}

	// It agrees with the evaluator for everyone it lists.
	for _, p := range people {
		a, err := f.svc.Checker.AccessQ(f.ctx, f.st.Q, p.UserID)
		if err != nil {
			t.Fatal(err)
		}
		if got := a.For("env-a").Role.ID(); got != p.Role {
			t.Fatalf("%s: listed %s, evaluator says %s", p.UserID, p.Role, got)
		}
	}
}

func TestEnvAccessListOfAnEnvironmentNobodyHoldsStillListsTheInstanceAdminsAndAUnknownOneIsNotFound(t *testing.T) {
	f := newOwnedFixture(t)
	f.addEnv(t, "env-a", "Alpha")
	f.addAdmin(t, "root")
	teams, people, err := f.svc.EnvAccessList(f.ctx, "env-a", "someone")
	if err != nil {
		t.Fatal(err)
	}
	if len(teams) != 0 || len(people) != 1 || people[0].UserID != "root" {
		t.Fatalf("teams = %+v, people = %+v", teams, people)
	}
	if _, _, err := f.svc.EnvAccessList(f.ctx, "no-such", "someone"); !errors.Is(err, access.ErrEnvironmentNotFound) {
		t.Fatalf("an unknown environment: %v, want ErrEnvironmentNotFound", err)
	}
}

func TestAccessCandidatesLeaveOutWhoAlreadyHasAGrantHereInstanceAdminsAndUsersWhoCannotSignIn(t *testing.T) {
	f := newOwnedFixture(t)
	f.addEnv(t, "env-a", "Alpha")
	f.addAdmin(t, "root")
	for _, u := range []string{"carol", "dave", "off", "Zed", "alex"} {
		f.addUser(t, u)
	}
	f.exec(t, `UPDATE users SET disabled = 1 WHERE id = 'off'`)
	f.exec(t, `UPDATE users SET display_name = 'Alexandra' WHERE id = 'alex'`)
	f.addTeam(t, "ops")
	f.addTeam(t, "dev")
	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("carol", "env-a", "admin")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, access.GrantInput{SubjectType: "team", SubjectID: "ops", Scope: access.ScopeEnv, EnvID: "env-a", Role: "viewer"}); err != nil {
		t.Fatal(err)
	}
	// A grant on every environment is not a grant here: dave is still offered.
	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, access.GrantInput{SubjectType: "user", SubjectID: "dave", Scope: access.ScopeAll, Role: "viewer"}); err != nil {
		t.Fatal(err)
	}

	users, teams, err := f.svc.AccessCandidates(f.ctx, "env-a")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, u := range users {
		names = append(names, u.Username)
	}
	if !slices.Equal(names, []string{"alex", "dave", "Zed"}) {
		t.Fatalf("users = %v, want alex, dave, Zed (not carol, root or off)", names)
	}
	if users[0].DisplayName != "Alexandra" {
		t.Fatalf("users[0] = %+v", users[0])
	}
	if len(teams) != 1 || teams[0].ID != "dev" {
		t.Fatalf("teams = %+v, want dev only", teams)
	}
	if _, _, err := f.svc.AccessCandidates(f.ctx, "no-such"); !errors.Is(err, access.ErrEnvironmentNotFound) {
		t.Fatalf("an unknown environment: %v", err)
	}
}

func TestADelegatedGrantOnTheEnvironmentWorksThereAndNowhereElse(t *testing.T) {
	f := newOwnedFixture(t)
	f.addEnv(t, "env-a", "Alpha")
	f.addEnv(t, "env-b", "Beta")
	f.addUser(t, "dave")

	in := envGrant("dave", "env-a", "operator", "containers.exec")
	in.DelegatedEnv = "env-a"
	g, created, err := f.svc.PutGrant(f.ctx, testAdmin, in)
	if err != nil || !created || g.EnvID != "env-a" {
		t.Fatalf("delegated PutGrant = %+v, %v, %v", g, created, err)
	}
	a, err := f.svc.Checker.AccessQ(f.ctx, f.st.Q, "dave")
	if err != nil {
		t.Fatal(err)
	}
	if a.For("env-a").Role != rbac.RoleOperator || a.For("env-b").Role != rbac.RoleNone {
		t.Fatalf("dave: A %v, B %v", a.For("env-a").Role, a.For("env-b").Role)
	}
}

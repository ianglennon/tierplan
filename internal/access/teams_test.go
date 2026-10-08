// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package access_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/access"
)

// Teams and their membership (plan 03-17, AUTH-05, D-11, D-25, D-27). Negative
// cases are mandatory (D-81).

func (f *ownedFixture) createTeam(t *testing.T, name string) access.Team {
	t.Helper()
	team, err := f.svc.CreateTeam(f.ctx, testAdmin, name)
	if err != nil {
		t.Fatalf("CreateTeam(%q): %v", name, err)
	}
	return team
}

func TestCreateTeamStoresItAuditsItAndListsItWithCounts(t *testing.T) {
	f := newOwnedFixture(t)
	team := f.createTeam(t, "  ops  ")
	if team.Name != "ops" || team.MemberCount != 0 || team.GrantCount != 0 || team.ID == "" {
		t.Fatalf("team = %+v", team)
	}
	f.createTeam(t, "Alpha")

	teams, err := f.svc.ListTeams(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(teams) != 2 || teams[0].Name != "Alpha" || teams[1].Name != "ops" {
		t.Fatalf("teams = %+v, want Alpha then ops (sorted ignoring case)", teams)
	}
	if got := f.auditDetails(t, "team.created"); !strings.Contains(got, `"name":`) {
		t.Fatalf("details = %s", got)
	}
	if n := f.auditCount(t, "team.created"); n != 2 {
		t.Fatalf("team.created entries = %d, want 2", n)
	}
}

func TestATeamNameIs1To64CharactersAndUniqueIgnoringCase(t *testing.T) {
	f := newOwnedFixture(t)
	f.createTeam(t, "Ops")
	for _, bad := range []string{"", "   ", strings.Repeat("x", 65), "bad\nname"} {
		if _, err := f.svc.CreateTeam(f.ctx, testAdmin, bad); !errors.Is(err, access.ErrInvalidTeamName) {
			t.Fatalf("CreateTeam(%q) = %v, want ErrInvalidTeamName", bad, err)
		}
	}
	if _, err := f.svc.CreateTeam(f.ctx, testAdmin, strings.Repeat("é", 64)); err != nil {
		t.Fatalf("a 64 character name was refused: %v", err)
	}
	if _, err := f.svc.CreateTeam(f.ctx, testAdmin, "oPS"); !errors.Is(err, access.ErrTeamNameTaken) {
		t.Fatalf("a name differing only in case = %v, want ErrTeamNameTaken", err)
	}
	if n := f.auditCount(t, "team.created"); n != 2 {
		t.Fatalf("team.created entries = %d, want only the two that worked", n)
	}
}

func TestAddMembersAddsAuditsOnePerUserAndTheTeamPageListsThem(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addUser(t, "bob")
	team := f.createTeam(t, "ops")

	n, err := f.svc.AddMembers(f.ctx, testAdmin, team.ID, []string{"alice", "bob", "alice"})
	if err != nil || n != 2 {
		t.Fatalf("AddMembers = %d, %v, want 2", n, err)
	}
	if c := f.auditCount(t, "team.member_added"); c != 2 {
		t.Fatalf("team.member_added entries = %d, want one per user added", c)
	}
	d, err := f.svc.GetTeam(f.ctx, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.MemberCount != 2 || len(d.Members) != 2 || d.Members[0].Username != "alice" || d.Members[0].Status != "active" || d.Members[0].InstanceAdmin {
		t.Fatalf("detail = %+v", d)
	}
	u, err := f.svc.GetUser(f.ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Teams) != 1 || u.Teams[0].Name != "ops" || u.TeamCount != 1 {
		t.Fatalf("the user page lists %+v (count %d), want ops", u.Teams, u.TeamCount)
	}
}

func TestAddingAMemberAgainChangesNothingAndWritesNoEntry(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	team := f.createTeam(t, "ops")
	if _, err := f.svc.AddMembers(f.ctx, testAdmin, team.ID, []string{"alice"}); err != nil {
		t.Fatal(err)
	}
	n, err := f.svc.AddMembers(f.ctx, testAdmin, team.ID, []string{"alice"})
	if err != nil || n != 0 {
		t.Fatalf("second AddMembers = %d, %v, want added 0", n, err)
	}
	if c := f.auditCount(t, "team.member_added"); c != 1 {
		t.Fatalf("team.member_added entries = %d, want 1 (the repeat leaves none)", c)
	}
}

func TestATombstonedOrUnknownUserCannotBeAddedAndNothingIsAdded(t *testing.T) {
	f := newOwnedFixture(t)
	f.addAdmin(t, "root")
	f.addUser(t, "alice")
	f.addUser(t, "gone")
	if err := f.svc.DeleteUser(f.ctx, testAdmin, "gone"); err != nil {
		t.Fatal(err)
	}
	team := f.createTeam(t, "ops")

	for _, bad := range []string{"gone", "nobody"} {
		_, err := f.svc.AddMembers(f.ctx, testAdmin, team.ID, []string{"alice", bad})
		if !errors.Is(err, access.ErrNotFound) || errors.Is(err, access.ErrTeamNotFound) || !strings.Contains(err.Error(), bad) {
			t.Fatalf("AddMembers with %q = %v, want ErrNotFound naming the user", bad, err)
		}
	}
	if c := f.count(t, `SELECT COUNT(*) FROM team_members WHERE team_id = ?`, team.ID); c != 0 {
		t.Fatalf("members = %d, want none: an add is all or nothing", c)
	}
	if _, err := f.svc.AddMembers(f.ctx, testAdmin, "no-team", []string{"alice"}); !errors.Is(err, access.ErrTeamNotFound) || !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("AddMembers to an unknown team = %v, want ErrTeamNotFound", err)
	}
}

func TestMembershipNeverMakesAnInstanceAdmin(t *testing.T) {
	f := newOwnedFixture(t)
	f.addAdmin(t, "root")
	f.addUser(t, "alice")
	team := f.createTeam(t, "admins")
	if _, err := f.svc.AddMembers(f.ctx, testAdmin, team.ID, []string{"alice", "root"}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT instance_admin FROM users WHERE id = 'alice'`); n != 0 {
		t.Fatal("joining a team made alice an instance admin")
	}
	a, err := f.svc.Checker.AccessQ(f.ctx, f.st.Q, "alice")
	if err != nil || a.InstanceAdmin {
		t.Fatalf("alice's access = %+v, %v, want no instance authority", a, err)
	}
}

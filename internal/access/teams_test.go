// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package access_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ianglennon/coaming/internal/access"
	"github.com/ianglennon/coaming/internal/rbac"
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

// --- rename, delete and remove with the impact preview (plan 03-17 task 2) ---

// assignTeam gives a team a role on one environment ("" is every environment).
func (f *ownedFixture) assignTeam(t *testing.T, teamID, role, envID string) {
	t.Helper()
	var env any
	if envID != "" {
		env = envID
	}
	f.exec(t, `INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at) VALUES (?, 'team', ?, ?, ?, 1)`,
		"rt-"+teamID+"-"+envID, teamID, role, env)
}

func (f *ownedFixture) join(t *testing.T, teamID string, users ...string) {
	t.Helper()
	if _, err := f.svc.AddMembers(f.ctx, testAdmin, teamID, users); err != nil {
		t.Fatal(err)
	}
}

// snapshot is the rows a preview must leave exactly as they were.
func (f *ownedFixture) snapshot(t *testing.T) string {
	t.Helper()
	var members, assignments, teams, entries, extras int
	for q, dst := range map[string]*int{
		`SELECT COUNT(*) FROM team_members`:      &members,
		`SELECT COUNT(*) FROM role_assignments`:  &assignments,
		`SELECT COUNT(*) FROM teams`:             &teams,
		`SELECT COUNT(*) FROM audit_log`:         &entries,
		`SELECT COUNT(*) FROM assignment_extras`: &extras,
	} {
		*dst = f.count(t, q)
	}
	return fmt.Sprintf("members=%d assignments=%d teams=%d audit=%d extras=%d", members, assignments, teams, entries, extras)
}

func TestRemoveMemberWritesOneEntryWithTheImpactAndTheUserKeepsOnlyTheirOtherGrants(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.assign(t, "alice", "viewer", "env-a")
	team := f.createTeam(t, "ops")
	f.assignTeam(t, team.ID, "operator", "env-a")
	f.join(t, team.ID, "alice")

	rows, err := f.svc.RemoveMember(f.ctx, testAdmin, team.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Before != rbac.RoleOperator || rows[0].After != rbac.RoleViewer {
		t.Fatalf("returned impact = %+v", rows)
	}
	if n := f.auditCount(t, "team.member_removed"); n != 1 {
		t.Fatalf("team.member_removed entries = %d, want exactly one", n)
	}
	var d map[string]string
	if err := json.Unmarshal([]byte(f.auditDetails(t, "team.member_removed")), &d); err != nil {
		t.Fatal(err)
	}
	if d["team"] != "ops" || d["username"] != "alice" || d["impact_count"] != "1" || d["impact"] != "alice: env-a operator→viewer" {
		t.Fatalf("details = %v", d)
	}
	a, err := f.svc.Checker.AccessQ(f.ctx, f.st.Q, "alice")
	if err != nil {
		t.Fatal(err)
	}
	env := a.For("env-a")
	if env.Role != rbac.RoleViewer || len(env.Sources) != 1 || env.Sources[0].Kind != rbac.SourceDirect {
		t.Fatalf("alice on env-a = %+v, want Viewer from the direct grant only", env)
	}
}

func TestRemovingANonMemberIsMemberNotFoundAndWritesNothing(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	team := f.createTeam(t, "ops")
	before := f.snapshot(t)
	if _, err := f.svc.RemoveMember(f.ctx, testAdmin, team.ID, "alice"); !errors.Is(err, access.ErrMemberNotFound) {
		t.Fatalf("RemoveMember of a non-member = %v, want ErrMemberNotFound", err)
	}
	if _, err := f.svc.RemoveMember(f.ctx, testAdmin, "no-team", "alice"); !errors.Is(err, access.ErrTeamNotFound) {
		t.Fatalf("RemoveMember on an unknown team = %v, want ErrTeamNotFound", err)
	}
	if got := f.snapshot(t); got != before {
		t.Fatalf("a refused removal changed the database: %s, was %s", got, before)
	}
}

func TestDeleteTeamLeavesNoOrphanAssignmentOrMembershipAndAuditsOnce(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addUser(t, "bob")
	team := f.createTeam(t, "ops")
	other := f.createTeam(t, "keep")
	f.assignTeam(t, team.ID, "operator", "env-a")
	f.assignTeam(t, team.ID, "viewer", "")
	f.assignTeam(t, other.ID, "viewer", "env-a")
	f.exec(t, `INSERT INTO assignment_extras (assignment_id, permission) VALUES (?, 'test.deploy')`, "rt-"+team.ID+"-env-a")
	f.join(t, team.ID, "alice", "bob")
	f.join(t, other.ID, "alice")

	rows, err := f.svc.DeleteTeam(f.ctx, testAdmin, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("deleting a team that grants access returned no impact")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM teams WHERE id = ?`, team.ID); n != 0 {
		t.Fatalf("the team is still there")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM team_members WHERE team_id = ?`, team.ID); n != 0 {
		t.Fatalf("%d memberships refer to the deleted team", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM role_assignments WHERE principal_type = 'team' AND principal_id = ?`, team.ID); n != 0 {
		t.Fatalf("%d assignments refer to the deleted team", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM assignment_extras WHERE assignment_id NOT IN (SELECT id FROM role_assignments)`); n != 0 {
		t.Fatalf("%d extras were left without an assignment", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM role_assignments WHERE principal_id = ?`, other.ID); n != 1 {
		t.Fatal("another team's grant went with it")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM team_members WHERE team_id = ?`, other.ID); n != 1 {
		t.Fatal("another team's membership went with it")
	}
	if n := f.auditCount(t, "team.deleted"); n != 1 {
		t.Fatalf("team.deleted entries = %d, want one", n)
	}
	var d map[string]string
	if err := json.Unmarshal([]byte(f.auditDetails(t, "team.deleted")), &d); err != nil {
		t.Fatal(err)
	}
	if d["name"] != "ops" || d["members"] != "2" || d["impact_count"] != strconv.Itoa(len(rows)) || d["impact"] == "" {
		t.Fatalf("details = %v", d)
	}
	if _, err := f.svc.DeleteTeam(f.ctx, testAdmin, team.ID); !errors.Is(err, access.ErrTeamNotFound) {
		t.Fatalf("deleting it again = %v, want ErrTeamNotFound", err)
	}
	if n := f.auditCount(t, "team.deleted"); n != 1 {
		t.Fatal("deleting a team that is gone wrote an entry")
	}
}

func TestLeavingATeamPausesTheAutomationsOfAMemberWhoNoLongerQualifies(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	team := f.createTeam(t, "ops")
	f.assignTeam(t, team.ID, "operator", "env-a")
	f.join(t, team.ID, "alice")
	f.register(t, syncResource("alice"))

	if _, err := f.svc.RemoveMember(f.ctx, testAdmin, team.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if got := f.state(t, kindTestSync, "r1"); got != access.OwnedPausedLacksPermission {
		t.Fatalf("state after the member was removed = %s, want paused_lacks_permission", got)
	}
}

func TestDeletingATeamPausesTheAutomationsOfItsMembersWhoNoLongerQualify(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	team := f.createTeam(t, "ops")
	f.assignTeam(t, team.ID, "operator", "env-a")
	f.join(t, team.ID, "alice")
	f.register(t, syncResource("alice"))

	if _, err := f.svc.DeleteTeam(f.ctx, testAdmin, team.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.state(t, kindTestSync, "r1"); got != access.OwnedPausedLacksPermission {
		t.Fatalf("state after the team was deleted = %s, want paused_lacks_permission", got)
	}
}

func TestRenameTeamIsCheckedAgainstTheVersionAndTheNamesOfOtherTeams(t *testing.T) {
	f := newOwnedFixture(t)
	team := f.createTeam(t, "ops")
	f.createTeam(t, "Dev")

	if _, err := f.svc.RenameTeam(f.ctx, testAdmin, team.ID, "dEV", team.UpdatedAt); !errors.Is(err, access.ErrTeamNameTaken) {
		t.Fatalf("a name another team holds in another case = %v, want ErrTeamNameTaken", err)
	}
	if _, err := f.svc.RenameTeam(f.ctx, testAdmin, team.ID, "", team.UpdatedAt); !errors.Is(err, access.ErrInvalidTeamName) {
		t.Fatalf("an empty name = %v, want ErrInvalidTeamName", err)
	}
	if _, err := f.svc.RenameTeam(f.ctx, testAdmin, team.ID, "Ops", team.UpdatedAt.Add(-1)); !errors.Is(err, access.ErrStaleChange) {
		t.Fatalf("an old version = %v, want ErrStaleChange", err)
	}
	if _, err := f.svc.RenameTeam(f.ctx, testAdmin, "no-team", "x", team.UpdatedAt); !errors.Is(err, access.ErrTeamNotFound) {
		t.Fatalf("an unknown team = %v, want ErrTeamNotFound", err)
	}
	if n := f.auditCount(t, "team.renamed"); n != 0 {
		t.Fatalf("a refused rename wrote %d entries", n)
	}

	// The team's own name in another case is a rename, not a clash.
	renamed, err := f.svc.RenameTeam(f.ctx, testAdmin, team.ID, " Ops ", team.UpdatedAt)
	if err != nil || renamed.Name != "Ops" || !renamed.UpdatedAt.After(team.UpdatedAt) {
		t.Fatalf("rename = %+v, %v, want Ops at a later version", renamed, err)
	}
	if got := f.auditDetails(t, "team.renamed"); !strings.Contains(got, `"old_name":"ops"`) || !strings.Contains(got, `"name":"Ops"`) {
		t.Fatalf("details = %s", got)
	}
	// The version it was based on is now stale.
	if _, err := f.svc.RenameTeam(f.ctx, testAdmin, team.ID, "Other", team.UpdatedAt); !errors.Is(err, access.ErrStaleChange) {
		t.Fatalf("a second rename from the old version = %v, want ErrStaleChange", err)
	}
	// Saving the name it already has changes nothing.
	again, err := f.svc.RenameTeam(f.ctx, testAdmin, team.ID, "Ops", renamed.UpdatedAt)
	if err != nil || !again.UpdatedAt.Equal(renamed.UpdatedAt) || f.auditCount(t, "team.renamed") != 1 {
		t.Fatalf("a rename to the same name = %+v, %v (entries %d), want nothing written", again, err, f.auditCount(t, "team.renamed"))
	}
}

func TestTeamDeleteRaceWithAddMembersLeavesNothingPointingAtTheDeletedTeam(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "carol")
	for i := 0; i < 20; i++ {
		team := f.createTeam(t, fmt.Sprintf("ops-%d", i))
		f.assignTeam(t, team.ID, "operator", "env-a")

		var wg sync.WaitGroup
		var addErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := f.svc.DeleteTeam(f.ctx, testAdmin, team.ID)
			if err != nil {
				t.Errorf("iteration %d: DeleteTeam: %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			_, addErr = f.svc.AddMembers(f.ctx, testAdmin, team.ID, []string{"carol"})
		}()
		wg.Wait()

		if addErr != nil && !errors.Is(addErr, access.ErrNotFound) {
			t.Fatalf("iteration %d: AddMembers = %v, want success before the delete or ErrNotFound", i, addErr)
		}
		if n := f.count(t, `SELECT COUNT(*) FROM teams WHERE id = ?`, team.ID); n != 0 {
			t.Fatalf("iteration %d: the team survived", i)
		}
		if n := f.count(t, `SELECT COUNT(*) FROM team_members WHERE team_id = ?`, team.ID); n != 0 {
			t.Fatalf("iteration %d: %d memberships refer to the deleted team", i, n)
		}
		if n := f.count(t, `SELECT COUNT(*) FROM role_assignments WHERE principal_type = 'team' AND principal_id = ?`, team.ID); n != 0 {
			t.Fatalf("iteration %d: %d assignments refer to the deleted team", i, n)
		}
	}
}

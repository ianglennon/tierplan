// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package access_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/access"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// The impact preview (plan 03-17, D-27, D-06): what a change to a team takes
// away, worked out by the same evaluator enforcement uses over a rolled-back
// transaction. Negative cases are mandatory (D-81).

func TestImpactPreviewOfRemovingAMemberShowsOnlyWhatFallsAndChangesNothing(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addUser(t, "bob")
	f.assign(t, "alice", "viewer", "env-a") // alice keeps Viewer directly
	team := f.createTeam(t, "ops")
	f.assignTeam(t, team.ID, "operator", "env-a")
	f.join(t, team.ID, "alice", "bob")
	before := f.snapshot(t)

	rows, err := f.svc.TeamImpact(f.ctx, team.ID, access.TeamOp{Kind: access.OpRemoveMember, UserID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Username != "alice" || rows[0].EnvID != "env-a" || rows[0].Before != rbac.RoleOperator || rows[0].After != rbac.RoleViewer {
		t.Fatalf("alice's impact = %+v, want operator to viewer on env-a", rows)
	}
	rows, err = f.svc.TeamImpact(f.ctx, team.ID, access.TeamOp{Kind: access.OpRemoveMember, UserID: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Username != "bob" || rows[0].Before != rbac.RoleOperator || rows[0].After != rbac.RoleNone {
		t.Fatalf("bob's impact = %+v, want operator to none", rows)
	}
	if got := f.snapshot(t); got != before {
		t.Fatalf("the preview changed the database: %s, was %s", got, before)
	}
	a, err := f.svc.Checker.AccessQ(f.ctx, f.st.Q, "alice")
	if err != nil || a.For("env-a").Role != rbac.RoleOperator {
		t.Fatalf("alice's access after the preview = %+v (%v), want unchanged Operator", a.For("env-a"), err)
	}
}

func TestImpactPreviewOfDeletingATeamListsEveryMemberWhoLosesAccessInUsernameOrder(t *testing.T) {
	f := newOwnedFixture(t)
	for _, u := range []string{"carol", "alice", "bob", "dave"} {
		f.addUser(t, u)
	}
	f.assign(t, "dave", "admin", "env-a") // dave is covered elsewhere
	team := f.createTeam(t, "ops")
	f.assignTeam(t, team.ID, "operator", "env-b")
	f.assignTeam(t, team.ID, "viewer", "env-a")
	f.join(t, team.ID, "carol", "alice", "bob", "dave")

	rows, err := f.svc.TeamImpact(f.ctx, team.ID, access.TeamOp{Kind: access.OpDeleteTeam})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, fmt.Sprintf("%s %s %s>%s", r.Username, r.EnvID, r.Before, r.After))
	}
	want := []string{
		"alice env-a viewer>none", "alice env-b operator>none",
		"bob env-a viewer>none", "bob env-b operator>none",
		"carol env-a viewer>none", "carol env-b operator>none",
		"dave env-b operator>none", // dave still has Admin on env-a
	}
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Fatalf("impact = %v\nwant     %v", got, want)
	}
}

func TestImpactNamesAnAllEnvironmentsGrantAndIgnoresInstanceAdminsAndDisabledMembers(t *testing.T) {
	f := newOwnedFixture(t)
	f.addAdmin(t, "root")
	f.addUser(t, "alice")
	f.addUser(t, "gone")
	f.exec(t, `UPDATE users SET disabled = 1 WHERE id = 'gone'`)
	team := f.createTeam(t, "everything")
	f.assignTeam(t, team.ID, "admin", "")
	f.join(t, team.ID, "root", "alice", "gone")

	rows, err := f.svc.TeamImpact(f.ctx, team.ID, access.TeamOp{Kind: access.OpDeleteTeam})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Username != "alice" || rows[0].EnvID != rbac.AllEnvironments || rows[0].EnvName != "All environments" ||
		rows[0].Before != rbac.RoleAdmin || rows[0].After != rbac.RoleNone {
		t.Fatalf("impact = %+v, want only alice on All environments (an instance admin and a disabled user lose nothing)", rows)
	}
}

func TestImpactOfATeamThatGrantsNothingIsEmpty(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	team := f.createTeam(t, "empty")
	f.join(t, team.ID, "alice")
	rows, err := f.svc.TeamImpact(f.ctx, team.ID, access.TeamOp{Kind: access.OpDeleteTeam})
	if err != nil || len(rows) != 0 {
		t.Fatalf("impact = %+v, %v, want none", rows, err)
	}
}

func TestImpactPreviewRefusesAnUnknownTeamAMissingMemberAndAnUnknownKind(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	team := f.createTeam(t, "ops")
	if _, err := f.svc.TeamImpact(f.ctx, "no-team", access.TeamOp{Kind: access.OpDeleteTeam}); !errors.Is(err, access.ErrTeamNotFound) {
		t.Fatalf("unknown team = %v", err)
	}
	if _, err := f.svc.TeamImpact(f.ctx, team.ID, access.TeamOp{Kind: access.OpRemoveMember, UserID: "alice"}); !errors.Is(err, access.ErrMemberNotFound) {
		t.Fatalf("a user who is not a member = %v", err)
	}
	for _, op := range []access.TeamOp{{Kind: "rename"}, {Kind: access.OpRemoveMember}} {
		if _, err := f.svc.TeamImpact(f.ctx, team.ID, op); !errors.Is(err, access.ErrInvalidTeamOp) {
			t.Fatalf("op %+v = %v, want ErrInvalidTeamOp", op, err)
		}
	}
}

func TestImpactOfFifteenUsersRecordsTheCountAndTenItemsWithinTheAuditLimit(t *testing.T) {
	f := newOwnedFixture(t)
	team := f.createTeam(t, "big")
	f.assignTeam(t, team.ID, "operator", "env-with-a-rather-long-identifier-a")
	var ids []string
	for i := 0; i < 15; i++ {
		id := fmt.Sprintf("a-user-with-a-long-name-%02d", i)
		f.addUser(t, id)
		ids = append(ids, id)
	}
	f.join(t, team.ID, ids...)

	rows, err := f.svc.DeleteTeam(f.ctx, testAdmin, team.ID)
	if err != nil || len(rows) != 15 {
		t.Fatalf("impact = %d rows, %v, want 15", len(rows), err)
	}
	var d map[string]string
	if err := json.Unmarshal([]byte(f.auditDetails(t, "team.deleted")), &d); err != nil {
		t.Fatal(err)
	}
	if d["impact_count"] != "15" || len(d["impact"]) > 512 || strings.Count(d["impact"], "; ") != 9 {
		t.Fatalf("impact_count %q, impact %d bytes with %d separators, want 15, at most 512 and ten items", d["impact_count"], len(d["impact"]), strings.Count(d["impact"], "; "))
	}
}

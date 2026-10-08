// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package access_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/ianglennon/bayplan/internal/access"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// Grants (plan 03-19, AUTH-06, AUTH-07, D-05, D-15 to D-17, D-21). Negative
// cases are mandatory (D-81).

// addEnv makes an environment row, so a grant on it passes the existence check.
func (f *ownedFixture) addEnv(t *testing.T, id, name string) {
	t.Helper()
	f.exec(t, `INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at)
		VALUES (?, 'agent', ?, '', 1, 1, 1)`, id, name)
}

func envGrant(userID, envID, role string, extras ...string) access.GrantInput {
	return access.GrantInput{
		SubjectType: access.SubjectUser, SubjectID: userID, Scope: access.ScopeEnv, EnvID: envID, Role: role, Extras: extras,
	}
}

func (f *ownedFixture) auditEnvID(t *testing.T, action string) string {
	t.Helper()
	var env *string
	if err := f.st.Read.QueryRow(`SELECT env_id FROM audit_log WHERE action = ? ORDER BY seq DESC LIMIT 1`, action).Scan(&env); err != nil {
		t.Fatal(err)
	}
	if env == nil {
		return ""
	}
	return *env
}

func (f *ownedFixture) detailsMap(t *testing.T, action string) map[string]string {
	t.Helper()
	var d map[string]string
	if err := json.Unmarshal([]byte(f.auditDetails(t, action)), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestAViewerGrantOnOneEnvironmentGivesThatRoleThereAndNothingOnAnother(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addEnv(t, "env-a", "Alpha")
	f.addEnv(t, "env-b", "Beta")

	g, created, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-a", "viewer"))
	if err != nil || !created {
		t.Fatalf("PutGrant = %+v, created %v, %v, want a new grant", g, created, err)
	}
	if g.ID == "" || g.Role != "viewer" || g.EnvID != "env-a" || g.EnvName != "Alpha" || len(g.Extras) != 0 || g.SubjectType != "user" || g.SubjectID != "alice" {
		t.Fatalf("grant = %+v", g)
	}
	a, err := f.svc.Checker.AccessQ(f.ctx, f.st.Q, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got := a.For("env-a").Role; got != rbac.RoleViewer {
		t.Fatalf("role on env-a = %v, want viewer", got)
	}
	if got := a.For("env-b").Role; got != rbac.RoleNone {
		t.Fatalf("role on env-b = %v, want none", got)
	}

	if n := f.auditCount(t, "access.granted"); n != 1 {
		t.Fatalf("access.granted entries = %d, want 1", n)
	}
	if env := f.auditEnvID(t, "access.granted"); env != "env-a" {
		t.Fatalf("the entry's env_id = %q, want env-a", env)
	}
	d := f.detailsMap(t, "access.granted")
	if d["subject_type"] != "user" || d["subject"] != "alice" || d["role"] != "viewer" || d["scope"] != "env-a" || d["extras"] != "" {
		t.Fatalf("details = %v", d)
	}
}

func TestListGrantsShowsAUsersOwnGrantsWithTheAllEnvironmentsOneFirst(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addEnv(t, "env-a", "Zulu")
	f.addEnv(t, "env-b", "alpha")
	for _, in := range []access.GrantInput{
		envGrant("alice", "env-a", "operator", "containers.exec"),
		envGrant("alice", "env-b", "viewer"),
		{SubjectType: "user", SubjectID: "alice", Scope: access.ScopeAll, Role: "viewer"},
	} {
		if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, in); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.svc.ListGrants(f.ctx, access.SubjectUser, "alice")
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, g := range got {
		order = append(order, g.EnvID)
	}
	if !slices.Equal(order, []string{"", "env-b", "env-a"}) {
		t.Fatalf("order = %q, want every environment, then by name (alpha, Zulu)", order)
	}
	if !slices.Equal(got[2].Extras, []string{"containers.exec"}) || got[2].EnvName != "Zulu" || got[1].Extras == nil {
		t.Fatalf("grants = %+v", got)
	}
}

func TestAGrantToAnUnknownUserTeamOrEnvironmentIsRefusedAndWritesNothing(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addEnv(t, "env-a", "Alpha")
	before := f.snapshot(t)

	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("nobody", "env-a", "viewer")); !errors.Is(err, access.ErrSubjectNotFound) {
		t.Fatalf("unknown user = %v, want ErrSubjectNotFound", err)
	}
	in := envGrant("no-team", "env-a", "viewer")
	in.SubjectType = access.SubjectTeam
	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, in); !errors.Is(err, access.ErrSubjectNotFound) {
		t.Fatalf("unknown team = %v, want ErrSubjectNotFound", err)
	}
	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-gone", "viewer")); !errors.Is(err, access.ErrEnvironmentNotFound) {
		t.Fatalf("unknown environment = %v, want ErrEnvironmentNotFound", err)
	}
	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-a", "owner")); !errors.Is(err, access.ErrInvalidRole) {
		t.Fatalf("role owner = %v, want ErrInvalidRole", err)
	}
	for _, bad := range []access.GrantInput{
		{SubjectType: "user", SubjectID: "alice", Scope: "env", Role: "viewer"},
		{SubjectType: "user", SubjectID: "alice", Scope: "everywhere", Role: "viewer"},
	} {
		if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, bad); !errors.Is(err, access.ErrInvalidScope) {
			t.Fatalf("scope %q with env %q = %v, want ErrInvalidScope", bad.Scope, bad.EnvID, err)
		}
	}
	if got := f.snapshot(t); got != before {
		t.Fatalf("a refused grant wrote something: %s, was %s", got, before)
	}

	f.exec(t, `UPDATE users SET deleted_at = 5 WHERE id = 'alice'`)
	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-a", "viewer")); !errors.Is(err, access.ErrSubjectNotFound) {
		t.Fatalf("a deleted user = %v, want ErrSubjectNotFound (tombstones hold no grants)", err)
	}
}

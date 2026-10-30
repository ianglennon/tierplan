// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package access_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/coaming/internal/access"
	"github.com/ianglennon/coaming/internal/rbac"
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

// --- extras, one grant per subject and scope, team and all-environments grants,
// stale edits, and removal or downgrade with its impact (plan 03-19 task 2) ---

func TestExtrasAreAcceptedOnlyOnOperatorGrantsAndOnlyFromTheRegistry(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addEnv(t, "env-a", "Alpha")
	before := f.snapshot(t)

	for _, c := range []struct {
		role   string
		extras []string
		want   error
	}{
		{"viewer", []string{"containers.exec"}, access.ErrViewerExtras},
		{"admin", []string{"containers.exec"}, access.ErrExtrasImplied},
		{"admin", []string{"secrets.reveal", "janitor.policy"}, access.ErrExtrasImplied},
		{"operator", []string{"environments.read"}, access.ErrUnknownExtra},
		{"operator", []string{"users.manage"}, access.ErrUnknownExtra},
		{"operator", []string{"containers.exec", "no.such"}, access.ErrUnknownExtra},
		{"viewer", []string{"no.such"}, access.ErrUnknownExtra},
	} {
		if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-a", c.role, c.extras...)); !errors.Is(err, c.want) {
			t.Fatalf("%s with %q = %v, want %v", c.role, c.extras, err, c.want)
		}
	}
	if got := f.snapshot(t); got != before {
		t.Fatalf("a refused grant wrote something: %s, was %s", got, before)
	}

	// An Operator may carry any of the four, once each, and they are kept sorted.
	g, _, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-a", "operator", "volumes.delete", "containers.exec", "containers.exec"))
	if err != nil || !slices.Equal(g.Extras, []string{"containers.exec", "volumes.delete"}) {
		t.Fatalf("Operator grant = %+v, %v, want the two extras once each, sorted", g, err)
	}
	if d := f.detailsMap(t, "access.granted"); d["extras"] != "containers.exec,volumes.delete" {
		t.Fatalf("audit extras = %q", d["extras"])
	}
}

func TestOperatorExtrasAreHeldSeparatelyFromTheRole(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addEnv(t, "env-a", "Alpha")
	f.addEnv(t, "env-b", "Beta")
	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-a", "operator", "containers.exec")); err != nil {
		t.Fatal(err)
	}
	checker := rbac.NewChecker(f.st) // the real registry, which has the extras
	alice := &rbac.Principal{Type: rbac.PrincipalUser, ID: "alice"}
	allowed := func(p rbac.Perm, env string) bool {
		return checker.Check(f.ctx, alice, rbac.Require(p), env).Allowed
	}
	if !allowed(rbac.PermContainersExec, "env-a") {
		t.Error("an Operator carrying the container shell was refused it")
	}
	if allowed(rbac.PermSecretsReveal, "env-a") {
		t.Error("an Operator without the reveal-secrets extra was allowed to reveal secrets")
	}
	if allowed(rbac.PermContainersExec, "env-b") {
		t.Error("the container shell on A reached B")
	}

	// Withdrawing the extra leaves the role and takes the shell away.
	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-a", "operator")); err != nil {
		t.Fatal(err)
	}
	if allowed(rbac.PermContainersExec, "env-a") {
		t.Error("the container shell survived withdrawing it")
	}
	a, _ := checker.Access(f.ctx, alice)
	if a.For("env-a").Role != rbac.RoleOperator {
		t.Errorf("role after withdrawing the extra = %v, want Operator", a.For("env-a").Role)
	}
}

func TestUpsertEditsTheSameRowForASubjectAndScopeAndAnotherScopeIsAnotherRow(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addEnv(t, "env-a", "Alpha")
	first, created, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-a", "viewer"))
	if err != nil || !created {
		t.Fatal(first, created, err)
	}
	second, created, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-a", "operator"))
	if err != nil || created || second.ID != first.ID || second.Role != "operator" {
		t.Fatalf("second save = %+v, created %v, %v, want the same row edited", second, created, err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM role_assignments WHERE principal_id = 'alice'`); n != 1 {
		t.Fatalf("rows for alice = %d, want 1", n)
	}
	if f.auditCount(t, "access.granted") != 1 || f.auditCount(t, "access.updated") != 1 {
		t.Fatalf("entries: %d granted, %d updated, want 1 and 1", f.auditCount(t, "access.granted"), f.auditCount(t, "access.updated"))
	}
	if env := f.auditEnvID(t, "access.updated"); env != "env-a" {
		t.Fatalf("the update's env_id = %q", env)
	}

	// Saving what is already there changes nothing and writes no entry.
	again, created, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-a", "operator"))
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("repeat = %+v, %v, %v", again, created, err)
	}
	if f.auditCount(t, "access.updated") != 1 {
		t.Fatal("a save that changed nothing was audited")
	}

	// All environments is a second scope: a second row.
	all, created, err := f.svc.PutGrant(f.ctx, testAdmin, access.GrantInput{SubjectType: "user", SubjectID: "alice", Scope: access.ScopeAll, Role: "viewer"})
	if err != nil || !created || all.ID == first.ID || all.EnvID != "" {
		t.Fatalf("all-environments grant = %+v, %v, %v", all, created, err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM role_assignments WHERE principal_id = 'alice'`); n != 2 {
		t.Fatalf("rows for alice = %d, want 2", n)
	}
	if d := f.detailsMap(t, "access.updated"); d["scope"] != "env-a" || d["role"] != "operator" {
		t.Fatalf("update details = %v", d)
	}
}

func TestAllEnvironmentsGivesEveryEnvironmentAndMarksOnesWithoutAnOwnGrantInherited(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addEnv(t, "env-a", "Alpha")
	f.addEnv(t, "env-b", "Beta")
	f.addEnv(t, "env-c", "Gamma")
	for _, in := range []access.GrantInput{
		{SubjectType: "user", SubjectID: "alice", Scope: access.ScopeAll, Role: "viewer"},
		envGrant("alice", "env-b", "operator", "secrets.reveal"),
	} {
		if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, in); err != nil {
			t.Fatal(err)
		}
	}
	// A new environment is reached through All environments at once.
	f.addEnv(t, "env-d", "Delta")
	_, rows, err := f.svc.EffectiveAccess(f.ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.EnvName+":"+r.Role+":"+fmt.Sprint(r.Inherited))
	}
	want := []string{"Alpha:viewer:true", "Beta:operator:false", "Delta:viewer:true", "Gamma:viewer:true"}
	if !slices.Equal(got, want) {
		t.Fatalf("rows = %q, want %q", got, want)
	}
	beta := rows[1]
	if !slices.Equal(beta.Extras, []string{"secrets.reveal"}) || !slices.Equal(sourceKinds(beta), []rbac.SourceKind{rbac.SourceAllEnvironments, rbac.SourceDirect}) {
		t.Fatalf("Beta = %+v, want the extra and the sources All environments then Direct", beta)
	}
}

func TestTeamGrantGivesEveryMemberTheRoleWithTheTeamAsSourceAndSeveralSourcesShowTogether(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addUser(t, "bob")
	f.addEnv(t, "env-a", "Alpha")
	team := f.createTeam(t, "ops")
	f.join(t, team.ID, "alice", "bob")
	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, access.GrantInput{
		SubjectType: access.SubjectTeam, SubjectID: team.ID, Scope: access.ScopeEnv, EnvID: "env-a", Role: "operator", Extras: []string{"containers.exec"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-a", "viewer")); err != nil {
		t.Fatal(err)
	}

	_, bob, err := f.svc.EffectiveAccess(f.ctx, "bob")
	if err != nil || len(bob) != 1 || bob[0].Role != "operator" || bob[0].Inherited ||
		len(bob[0].Sources) != 1 || bob[0].Sources[0].Kind != rbac.SourceTeam || bob[0].Sources[0].TeamName != "ops" {
		t.Fatalf("bob = %+v, %v, want Operator from Team ops", bob, err)
	}
	_, alice, err := f.svc.EffectiveAccess(f.ctx, "alice")
	if err != nil || len(alice) != 1 || alice[0].Role != "operator" || !slices.Equal(alice[0].Extras, []string{"containers.exec"}) ||
		!slices.Equal(sourceKinds(alice[0]), []rbac.SourceKind{rbac.SourceTeam, rbac.SourceDirect}) {
		t.Fatalf("alice = %+v, %v, want one row: the highest role, the extra and both sources", alice, err)
	}
	if d := f.detailsMap(t, "access.granted"); d["subject_type"] == "" {
		t.Fatalf("details = %v", d)
	}
	var teamEntry string
	if err := f.st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = 'access.granted' ORDER BY seq LIMIT 1`).Scan(&teamEntry); err != nil || !strings.Contains(teamEntry, `"subject":"ops"`) || !strings.Contains(teamEntry, `"subject_type":"team"`) {
		t.Fatalf("the team grant's entry = %s (%v), want the team named", teamEntry, err)
	}
}

func TestAStaleEditIsRefusedAndChangesNothing(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addEnv(t, "env-a", "Alpha")
	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-a", "viewer")); err != nil {
		t.Fatal(err)
	}
	// Another admin makes it Operator while the first has the dialog open.
	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-a", "operator")); err != nil {
		t.Fatal(err)
	}
	stale := envGrant("alice", "env-a", "admin")
	stale.Expected = &access.GrantState{Role: "viewer", Extras: []string{}}
	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, stale); !errors.Is(err, access.ErrStaleChange) {
		t.Fatalf("a stale edit = %v, want ErrStaleChange", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM role_assignments WHERE role_id = 'admin'`); n != 0 {
		t.Fatal("a stale edit was applied")
	}
	// An edit based on the current state goes through; so does one of a grant that
	// has since been removed, as a stale edit, not as a quiet re-creation.
	fresh := envGrant("alice", "env-a", "admin")
	fresh.Expected = &access.GrantState{Role: "operator", Extras: []string{}}
	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, fresh); err != nil {
		t.Fatalf("an edit from the current state = %v", err)
	}
	f.exec(t, `DELETE FROM role_assignments WHERE principal_id = 'alice'`)
	fresh.Expected = &access.GrantState{Role: "admin", Extras: []string{}}
	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, fresh); !errors.Is(err, access.ErrStaleChange) {
		t.Fatalf("an edit of a removed grant = %v, want ErrStaleChange", err)
	}
}

func TestGrantImpactEqualsWhatTheChangeProducesAndTheDatabaseIsUnchangedByAskingAndAGrantHasNoExpiry(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addUser(t, "bob")
	f.addEnv(t, "env-a", "Alpha")
	team := f.createTeam(t, "ops")
	f.join(t, team.ID, "alice", "bob")
	f.assign(t, "alice", "viewer", "env-a") // alice keeps Viewer directly
	tg, _, err := f.svc.PutGrant(f.ctx, testAdmin, access.GrantInput{
		SubjectType: access.SubjectTeam, SubjectID: team.ID, Scope: access.ScopeEnv, EnvID: "env-a", Role: "operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	before := f.snapshot(t)

	del, err := f.svc.GrantImpact(f.ctx, access.SubjectTeam, team.ID, tg.ID, access.GrantChange{Op: access.OpDeleteGrant}, access.GrantScopeLimit{})
	if err != nil {
		t.Fatal(err)
	}
	down, err := f.svc.GrantImpact(f.ctx, access.SubjectTeam, team.ID, tg.ID, access.GrantChange{Op: access.OpUpdateGrant, Role: "viewer"}, access.GrantScopeLimit{})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.snapshot(t); got != before {
		t.Fatalf("a preview changed the database: %s, was %s", got, before)
	}
	describe := func(rows []access.ImpactRow) string {
		var out []string
		for _, r := range rows {
			out = append(out, fmt.Sprintf("%s %s %s>%s", r.Username, r.EnvID, r.Before, r.After))
		}
		return strings.Join(out, "; ")
	}
	// Removing the team's Operator leaves alice her own Viewer and bob nothing; lowering it
	// to Viewer takes both of them from Operator to Viewer.
	if got, want := describe(del), "alice env-a operator>viewer; bob env-a operator>none"; got != want {
		t.Fatalf("delete impact = %q, want %q", got, want)
	}
	if got, want := describe(down), "alice env-a operator>viewer; bob env-a operator>viewer"; got != want {
		t.Fatalf("downgrade impact = %q, want %q", got, want)
	}

	rows, err := f.svc.DeleteGrant(f.ctx, testAdmin, access.SubjectTeam, team.ID, tg.ID, access.GrantScopeLimit{})
	if err != nil {
		t.Fatal(err)
	}
	if describe(rows) != describe(del) {
		t.Fatalf("the removal produced %q, the preview said %q", describe(rows), describe(del))
	}
	if n := f.auditCount(t, "access.revoked"); n != 1 {
		t.Fatalf("access.revoked entries = %d, want exactly one", n)
	}
	d := f.detailsMap(t, "access.revoked")
	if d["subject_type"] != "team" || d["subject"] != "ops" || d["role"] != "operator" || d["scope"] != "env-a" || d["impact_count"] != "2" ||
		!strings.Contains(d["impact"], "alice: Alpha operator→viewer") || !strings.Contains(d["impact"], "bob: Alpha operator→none") {
		t.Fatalf("details = %v", d)
	}
	if env := f.auditEnvID(t, "access.revoked"); env != "env-a" {
		t.Fatalf("the entry's env_id = %q", env)
	}
	a, _ := f.svc.Checker.AccessQ(f.ctx, f.st.Q, "bob")
	if a.For("env-a").Role != rbac.RoleNone {
		t.Fatalf("bob's role after the removal = %v, want none", a.For("env-a").Role)
	}

	// No grant carries an expiry anywhere (D-21).
	cols, err := f.st.Read.Query(`SELECT name FROM pragma_table_info('role_assignments')`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cols.Close() }()
	for cols.Next() {
		var name string
		if err := cols.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(name, "expir") || strings.Contains(name, "until") {
			t.Fatalf("role_assignments has a %q column: a grant lasts until it is removed", name)
		}
	}
}

func TestRemovingOrLoweringAGrantPausesTheOwnedAutomationsOfAnOwnerWhoNoLongerQualifies(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addEnv(t, "env-a", "Alpha")
	g, _, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-a", "operator"))
	if err != nil {
		t.Fatal(err)
	}
	f.register(t, syncResource("alice"))
	if s := f.state(t, kindTestSync, "r1"); s != "active" {
		t.Fatalf("state with Operator = %s", s)
	}

	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-a", "viewer")); err != nil {
		t.Fatal(err)
	}
	if s := f.state(t, kindTestSync, "r1"); s != "paused_lacks_permission" {
		t.Fatalf("state after the downgrade = %s, want paused_lacks_permission", s)
	}
	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-a", "operator")); err != nil {
		t.Fatal(err)
	}
	if s := f.state(t, kindTestSync, "r1"); s != "active" {
		t.Fatalf("state after the role came back = %s, want active", s)
	}
	if _, err := f.svc.DeleteGrant(f.ctx, testAdmin, access.SubjectUser, "alice", g.ID, access.GrantScopeLimit{}); err != nil {
		t.Fatal(err)
	}
	if s := f.state(t, kindTestSync, "r1"); s != "paused_lacks_permission" {
		t.Fatalf("state after the removal = %s, want paused_lacks_permission", s)
	}
}

func TestAGrantThatIsNotTheSubjectsIsNotFoundForPreviewAndRemovalAndNothingIsWritten(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addUser(t, "bob")
	f.addEnv(t, "env-a", "Alpha")
	g, _, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-a", "viewer"))
	if err != nil {
		t.Fatal(err)
	}
	before := f.snapshot(t)
	if _, err := f.svc.DeleteGrant(f.ctx, testAdmin, access.SubjectUser, "bob", g.ID, access.GrantScopeLimit{}); !errors.Is(err, access.ErrGrantNotFound) {
		t.Fatalf("removing alice's grant as bob's = %v, want ErrGrantNotFound", err)
	}
	if _, err := f.svc.DeleteGrant(f.ctx, testAdmin, access.SubjectUser, "alice", "no-such", access.GrantScopeLimit{}); !errors.Is(err, access.ErrGrantNotFound) {
		t.Fatalf("removing an unknown grant = %v, want ErrGrantNotFound", err)
	}
	if _, err := f.svc.GrantImpact(f.ctx, access.SubjectUser, "bob", g.ID, access.GrantChange{Op: access.OpDeleteGrant}, access.GrantScopeLimit{}); !errors.Is(err, access.ErrGrantNotFound) {
		t.Fatalf("previewing alice's grant as bob's = %v, want ErrGrantNotFound", err)
	}
	if _, err := f.svc.GrantImpact(f.ctx, access.SubjectUser, "ghost", g.ID, access.GrantChange{Op: access.OpDeleteGrant}, access.GrantScopeLimit{}); !errors.Is(err, access.ErrSubjectNotFound) {
		t.Fatalf("previewing for an unknown user = %v, want ErrSubjectNotFound", err)
	}
	for _, bad := range []access.GrantChange{
		{Op: "rename"},
		{Op: access.OpUpdateGrant, Role: "owner"},
		{Op: access.OpUpdateGrant, Role: "viewer", Extras: []string{"containers.exec"}},
	} {
		if _, err := f.svc.GrantImpact(f.ctx, access.SubjectUser, "alice", g.ID, bad, access.GrantScopeLimit{}); err == nil {
			t.Fatalf("GrantImpact(%+v) was accepted", bad)
		}
	}
	if got := f.snapshot(t); got != before {
		t.Fatalf("refusals wrote something: %s, was %s", got, before)
	}
}

func TestLoweringOnlyTheExtrasIsNotAnImpactRowButIsStillAudited(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addEnv(t, "env-a", "Alpha")
	g, _, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-a", "operator", "containers.exec"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := f.svc.GrantImpact(f.ctx, access.SubjectUser, "alice", g.ID, access.GrantChange{Op: access.OpUpdateGrant, Role: "operator"}, access.GrantScopeLimit{})
	if err != nil || len(rows) != 0 {
		t.Fatalf("impact of dropping an extra = %+v, %v, want none (the role does not fall)", rows, err)
	}
	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-a", "operator")); err != nil {
		t.Fatal(err)
	}
	d := f.detailsMap(t, "access.updated")
	if d["extras"] != "" || d["impact_count"] != "0" {
		t.Fatalf("details = %v, want no extras and impact_count 0", d)
	}
}

// Delegation (plan 03-20, AUTH-06, D-12): an env Admin changes grants on the
// environment they administer and nothing else.

func delegated(env string, in access.GrantInput) access.GrantInput {
	in.DelegatedEnv = env
	return in
}

func (f *ownedFixture) assignmentCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM role_assignments`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDelegatedPutRefusesEveryScopeButTheEnvironmentAndInstanceAdminSubjects(t *testing.T) {
	f := newOwnedFixture(t)
	f.addEnv(t, "env-a", "Alpha")
	f.addEnv(t, "env-b", "Beta")
	f.addUser(t, "dave")
	f.addAdmin(t, "root")
	f.addTeam(t, "ops", "dave")
	before := f.assignmentCount(t)

	for name, in := range map[string]access.GrantInput{
		"every environment":          {SubjectType: "user", SubjectID: "dave", Scope: access.ScopeAll, Role: "viewer"},
		"another environment":        envGrant("dave", "env-b", "viewer"),
		"an env scope with no env":   {SubjectType: "user", SubjectID: "dave", Scope: access.ScopeEnv, Role: "viewer"},
		"an instance admin":          envGrant("root", "env-a", "viewer"),
		"every environment for team": {SubjectType: "team", SubjectID: "ops", Scope: access.ScopeAll, Role: "viewer"},
	} {
		if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, delegated("env-a", in)); !errors.Is(err, access.ErrDelegationDenied) {
			t.Fatalf("delegated PutGrant for %s: %v, want ErrDelegationDenied", name, err)
		}
	}
	if after := f.assignmentCount(t); after != before {
		t.Fatalf("assignments %d -> %d, want nothing written", before, after)
	}
	if n := f.auditCount(t, "access.granted"); n != 0 {
		t.Fatalf("access.granted entries = %d, want none", n)
	}

	// The same grant on the environment is fine, for a user and for a team.
	for _, in := range []access.GrantInput{
		envGrant("dave", "env-a", "viewer"),
		{SubjectType: "team", SubjectID: "ops", Scope: access.ScopeEnv, EnvID: "env-a", Role: "operator"},
	} {
		if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, delegated("env-a", in)); err != nil {
			t.Fatalf("delegated PutGrant on the environment: %v", err)
		}
	}
}

func TestDelegatedRemovalAndPreviewRefuseGrantsBeyondTheEnvironmentAndInstanceAdminsAndChangeNothing(t *testing.T) {
	f := newOwnedFixture(t)
	f.addEnv(t, "env-a", "Alpha")
	f.addEnv(t, "env-b", "Beta")
	for _, u := range []string{"dave", "erin", "frank"} {
		f.addUser(t, u)
	}
	f.addAdmin(t, "root")
	put := func(in access.GrantInput) access.Grant {
		t.Helper()
		g, _, err := f.svc.PutGrant(f.ctx, testAdmin, in)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	own := put(envGrant("dave", "env-a", "operator"))
	all := put(access.GrantInput{SubjectType: "user", SubjectID: "erin", Scope: access.ScopeAll, Role: "viewer"})
	other := put(envGrant("frank", "env-b", "viewer"))
	stale := put(envGrant("root", "env-a", "viewer"))
	limit := access.GrantScopeLimit{DelegatedEnv: "env-a"}
	before := f.assignmentCount(t)

	for name, g := range map[string]access.Grant{"every environment": all, "another environment": other, "an instance admin": stale} {
		if _, err := f.svc.GrantImpact(f.ctx, g.SubjectType, g.SubjectID, g.ID, access.GrantChange{Op: access.OpDeleteGrant}, limit); !errors.Is(err, access.ErrDelegationDenied) {
			t.Fatalf("delegated preview of %s: %v, want ErrDelegationDenied", name, err)
		}
		if _, err := f.svc.DeleteGrant(f.ctx, testAdmin, g.SubjectType, g.SubjectID, g.ID, limit); !errors.Is(err, access.ErrDelegationDenied) {
			t.Fatalf("delegated removal of %s: %v, want ErrDelegationDenied", name, err)
		}
		if _, _, err := f.svc.GrantSubject(f.ctx, g.ID, limit); !errors.Is(err, access.ErrDelegationDenied) {
			t.Fatalf("delegated lookup of %s: %v, want ErrDelegationDenied", name, err)
		}
	}
	if after := f.assignmentCount(t); after != before {
		t.Fatalf("assignments %d -> %d, want nothing removed", before, after)
	}
	if n := f.auditCount(t, "access.revoked"); n != 0 {
		t.Fatalf("access.revoked entries = %d, want none", n)
	}

	// The environment's own grant: found, previewed, removed, audited with the environment.
	typ, id, err := f.svc.GrantSubject(f.ctx, own.ID, limit)
	if err != nil || typ != "user" || id != "dave" {
		t.Fatalf("GrantSubject = %q, %q, %v", typ, id, err)
	}
	preview, err := f.svc.GrantImpact(f.ctx, typ, id, own.ID, access.GrantChange{Op: access.OpDeleteGrant}, limit)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := f.svc.DeleteGrant(f.ctx, testAdmin, typ, id, own.ID, limit)
	if err != nil || len(removed) != len(preview) || len(removed) != 1 {
		t.Fatalf("removal = %+v, %v; preview = %+v", removed, err, preview)
	}
	if env := f.auditEnvID(t, "access.revoked"); env != "env-a" {
		t.Fatalf("access.revoked env_id = %q, want env-a", env)
	}
	if _, _, err := f.svc.GrantSubject(f.ctx, "no-such", limit); !errors.Is(err, access.ErrGrantNotFound) {
		t.Fatalf("GrantSubject of nothing: %v, want ErrGrantNotFound", err)
	}
}

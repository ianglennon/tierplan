// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rbac_test

import (
	"context"
	"slices"
	"testing"

	"github.com/ianglennon/bayplan/internal/rbac"
)

// The role matrix suite pins what each role, source and flag gives against a
// registry the test owns, so the Operator boundary is testable before the real
// registry has an Operator-level permission that is not an extra (Phase 4). It
// uses rbac.WithRegistry; nothing here depends on the production registry.

const (
	permRead    rbac.Perm = "test.read"
	permOperate rbac.Perm = "test.operate"
	permShell   rbac.Perm = "test.shell"
	permSecret  rbac.Perm = "test.secret"
	permManage  rbac.Perm = "test.manage"
	permGlobal  rbac.Perm = "test.global"
)

func matrixRegistry() rbac.Registry {
	return rbac.Registry{
		{Name: permRead, Scope: rbac.ScopeEnvironment, MinRole: rbac.RoleViewer},
		{Name: permOperate, Scope: rbac.ScopeEnvironment, MinRole: rbac.RoleOperator},
		{Name: permShell, Scope: rbac.ScopeEnvironment, MinRole: rbac.RoleOperator, Extra: true},
		{Name: permSecret, Scope: rbac.ScopeEnvironment, MinRole: rbac.RoleOperator, Extra: true},
		{Name: permManage, Scope: rbac.ScopeEnvironment, MinRole: rbac.RoleAdmin},
		{Name: permGlobal, Scope: rbac.ScopeGlobal},
	}
}

func newMatrixFx(t *testing.T) *fx {
	t.Helper()
	return newFx(t, rbac.WithRegistry(matrixRegistry()))
}

// can asks the Checker with a requirement built from the matrix registry, not the
// production one (rbac.Require would look the name up there).
func can(f *fx, p *rbac.Principal, perm rbac.Perm, env string) bool {
	reg, ok := matrixRegistry().Lookup(perm)
	if !ok {
		panic("not in the matrix registry: " + string(perm))
	}
	return f.c.Check(context.Background(), p, rbac.Requirement{Perm: perm, Scope: reg.Scope}, env).Allowed
}

func envAccess(t *testing.T, f *fx, p *rbac.Principal, env string) rbac.EnvAccess {
	t.Helper()
	ea, err := f.c.EnvAccess(context.Background(), p, env)
	if err != nil {
		t.Fatal(err)
	}
	return ea
}

func TestAccessMatrixTheOperatorBoundaryOfAnInjectedRegistry(t *testing.T) {
	f := newMatrixFx(t)
	viewer := f.addUser(t, "viewer")
	f.assign(t, "user", "viewer", "viewer", "env-a")
	operator := f.addUser(t, "operator")
	f.assign(t, "user", "operator", "operator", "env-a")
	shell := f.addUser(t, "shell")
	f.assign(t, "user", "shell", "operator", "env-a", string(permShell))
	admin := f.addUser(t, "admin")
	f.assign(t, "user", "admin", "admin", "env-a")

	for _, c := range []struct {
		who  string
		p    *rbac.Principal
		perm rbac.Perm
		want bool
	}{
		{"viewer", viewer, permRead, true},
		{"viewer", viewer, permOperate, false},
		{"viewer", viewer, permShell, false},
		{"viewer", viewer, permManage, false},
		{"operator", operator, permRead, true},
		{"operator", operator, permOperate, true},
		{"operator", operator, permShell, false},
		{"operator", operator, permManage, false},
		{"operator with test.shell", shell, permOperate, true},
		{"operator with test.shell", shell, permShell, true},
		{"operator with test.shell", shell, permSecret, false},
		{"admin", admin, permOperate, true},
		{"admin", admin, permShell, true},
		{"admin", admin, permSecret, true},
		{"admin", admin, permManage, true},
	} {
		t.Run(c.who+" "+string(c.perm), func(t *testing.T) {
			if got := can(f, c.p, c.perm, "env-a"); got != c.want {
				t.Errorf("allowed = %v, want %v", got, c.want)
			}
			// No role carries over to an environment it holds no grant on.
			if can(f, c.p, c.perm, "env-b") {
				t.Errorf("allowed on env-b, where there is no grant")
			}
			// And no environment role is a global permission.
			if can(f, c.p, permGlobal, "") {
				t.Error("an environment role was allowed a global permission")
			}
		})
	}
}

func TestAccessUnionAViewerDirectAndAnOperatorThroughATeamIsAnOperatorWithBothSources(t *testing.T) {
	f := newMatrixFx(t)
	p := f.addUser(t, "u1")
	f.team(t, "t-ops", "ops", "u1")
	f.assign(t, "user", "u1", "viewer", "env-a")
	f.assign(t, "team", "t-ops", "operator", "env-a")

	ea := envAccess(t, f, p, "env-a")
	want := []rbac.Source{{Kind: rbac.SourceTeam, TeamID: "t-ops", TeamName: "ops"}, {Kind: rbac.SourceDirect}}
	if ea.Role != rbac.RoleOperator || !slices.Equal(ea.Sources, want) {
		t.Fatalf("access = %+v, want operator with sources %+v", ea, want)
	}
	if !can(f, p, permOperate, "env-a") {
		t.Error("the team's Operator role did not apply")
	}
}

func TestAccessUnionAViewerOnAllAndAnAdminOnOneIsAdminThereAndViewerElsewhere(t *testing.T) {
	f := newMatrixFx(t)
	p := f.addUser(t, "u1")
	f.assign(t, "user", "u1", "viewer", "")
	f.assign(t, "user", "u1", "admin", "env-a")

	a := envAccess(t, f, p, "env-a")
	if a.Role != rbac.RoleAdmin || !slices.Equal(a.Sources, []rbac.Source{{Kind: rbac.SourceAllEnvironments}, {Kind: rbac.SourceDirect}}) {
		t.Errorf("env-a access = %+v, want admin from all environments and direct", a)
	}
	b := envAccess(t, f, p, "env-b")
	if b.Role != rbac.RoleViewer || !slices.Equal(b.Sources, []rbac.Source{{Kind: rbac.SourceAllEnvironments}}) {
		t.Errorf("env-b access = %+v, want viewer with the one source all", b)
	}
	if !can(f, p, permManage, "env-a") || can(f, p, permManage, "env-b") || !can(f, p, permRead, "env-b") {
		t.Error("the matrix did not follow the per-environment role")
	}
}

func TestAccessUnionADirectViewerAndATeamViewerIsOneViewerWithBothSources(t *testing.T) {
	f := newMatrixFx(t)
	p := f.addUser(t, "u1")
	f.team(t, "t-1", "Ops", "u1")
	f.assign(t, "user", "u1", "viewer", "env-a")
	f.assign(t, "team", "t-1", "viewer", "env-a")

	ea := envAccess(t, f, p, "env-a")
	want := []rbac.Source{{Kind: rbac.SourceTeam, TeamID: "t-1", TeamName: "Ops"}, {Kind: rbac.SourceDirect}}
	if ea.Role != rbac.RoleViewer || !slices.Equal(ea.Sources, want) {
		t.Fatalf("access = %+v, want one viewer with sources %+v", ea, want)
	}
}

// Whatever order the rows went in, sources come out instance, all, team by name,
// direct (D-06).
func TestAccessUnionSourceOrderDoesNotDependOnInsertionOrder(t *testing.T) {
	type grant func(t *testing.T, f *fx)
	direct := func(t *testing.T, f *fx) { f.assign(t, "user", "u1", "viewer", "env-a") }
	all := func(t *testing.T, f *fx) { f.assign(t, "user", "u1", "viewer", "") }
	zulu := func(t *testing.T, f *fx) { f.assign(t, "team", "t-z", "viewer", "env-a") }
	alpha := func(t *testing.T, f *fx) { f.assign(t, "team", "t-a", "viewer", "env-a") }
	want := []rbac.Source{
		{Kind: rbac.SourceAllEnvironments},
		{Kind: rbac.SourceTeam, TeamID: "t-a", TeamName: "alpha"},
		{Kind: rbac.SourceTeam, TeamID: "t-z", TeamName: "zulu"},
		{Kind: rbac.SourceDirect},
	}
	for name, order := range map[string][]grant{
		"direct first":  {direct, zulu, alpha, all},
		"all first":     {all, alpha, zulu, direct},
		"teams reverse": {zulu, alpha, direct, all},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMatrixFx(t)
			p := f.addUser(t, "u1")
			f.team(t, "t-z", "zulu", "u1")
			f.team(t, "t-a", "alpha", "u1")
			for _, g := range order {
				g(t, f)
			}
			if got := envAccess(t, f, p, "env-a").Sources; !slices.Equal(got, want) {
				t.Errorf("sources = %+v, want %+v", got, want)
			}
		})
	}
}

func TestAccessExtrasAreTheUnionOfADirectOperatorAndATeamOperator(t *testing.T) {
	f := newMatrixFx(t)
	p := f.addUser(t, "u1")
	f.team(t, "t-1", "ops", "u1")
	f.assign(t, "user", "u1", "operator", "env-a", string(permShell))
	f.assign(t, "team", "t-1", "operator", "env-a", string(permSecret))

	ea := envAccess(t, f, p, "env-a")
	if want := []rbac.Perm{permSecret, permShell}; !slices.Equal(ea.Extras, want) {
		t.Fatalf("extras = %v, want the union %v", ea.Extras, want)
	}
	if !can(f, p, permShell, "env-a") || !can(f, p, permSecret, "env-a") {
		t.Error("an extra from either source must be held")
	}
}

func TestAccessAnExtraCarriedByAViewerGrantNeverCounts(t *testing.T) {
	f := newMatrixFx(t)
	p := f.addUser(t, "u1")
	f.assign(t, "user", "u1", "viewer", "env-a", string(permShell))
	if can(f, p, permShell, "env-a") {
		t.Error("a Viewer cannot hold an extra")
	}
}

func TestAccessInstanceAdminIsAllowedEveryRegisteredPermissionOnAnUnknownID(t *testing.T) {
	f := newMatrixFx(t)
	p := f.instanceAdmin(t, "root")
	for _, reg := range matrixRegistry() {
		env := "never-created"
		if reg.Scope == rbac.ScopeGlobal {
			env = ""
		}
		if !can(f, p, reg.Name, env) {
			t.Errorf("instance admin refused %s", reg.Name)
		}
	}
	ea := envAccess(t, f, p, "never-created")
	if ea.Role != rbac.RoleAdmin || len(ea.Sources) != 1 || ea.Sources[0].Kind != rbac.SourceInstanceAdmin {
		t.Errorf("access = %+v, want admin from the instance source", ea)
	}
}

func TestAccessADisabledInstanceAdminAndATombstonedUserAreAllowedNothing(t *testing.T) {
	f := newMatrixFx(t)
	disabled := f.instanceAdmin(t, "disabled-root")
	f.assign(t, "user", "disabled-root", "admin", "env-a")
	f.exec(t, `UPDATE users SET disabled = 1 WHERE id = 'disabled-root'`)
	gone := f.addUser(t, "gone")
	f.assign(t, "user", "gone", "admin", "")
	f.exec(t, `UPDATE users SET deleted_at = 7 WHERE id = 'gone'`)

	for name, p := range map[string]*rbac.Principal{"disabled instance admin": disabled, "tombstoned user": gone} {
		for _, reg := range matrixRegistry() {
			env := "env-a"
			if reg.Scope == rbac.ScopeGlobal {
				env = ""
			}
			if can(f, p, reg.Name, env) {
				t.Errorf("%s was allowed %s", name, reg.Name)
			}
		}
		if ea := envAccess(t, f, p, "env-a"); ea.Role != rbac.RoleNone || len(ea.Sources) != 0 {
			t.Errorf("%s has access %+v, want none", name, ea)
		}
	}
}

func TestAccessAnAdminOnEveryEnvironmentIsRefusedEveryGlobalPermission(t *testing.T) {
	f := newMatrixFx(t)
	p := f.addUser(t, "all-admin")
	f.assign(t, "user", "all-admin", "admin", "")
	if can(f, p, permGlobal, "") {
		t.Error("Admin on every environment was allowed a global permission; only an instance admin holds one")
	}
	for _, perm := range []rbac.Perm{permRead, permOperate, permShell, permSecret, permManage} {
		if !can(f, p, perm, "any-environment") {
			t.Errorf("Admin on every environment refused %s", perm)
		}
	}
}

func TestAccessASystemPrincipalIsRefusedEvenWithAnAssignmentRowUnderItsName(t *testing.T) {
	f := newMatrixFx(t)
	f.assign(t, "user", "system:janitor", "admin", "")
	f.assign(t, "system", "system:janitor", "admin", "env-a")
	p := rbac.System("system:janitor")
	for _, reg := range matrixRegistry() {
		env := "env-a"
		if reg.Scope == rbac.ScopeGlobal {
			env = ""
		}
		if can(f, p, reg.Name, env) {
			t.Errorf("system:janitor was allowed %s", reg.Name)
		}
	}
}

// TestAccessMatrixInvariantCheckEnvAccessAndEffectivePermissionsAgree is the
// 03-01 invariant: the three ways to ask what a principal may do give one answer.
func TestAccessMatrixInvariantCheckEnvAccessAndEffectivePermissionsAgree(t *testing.T) {
	f := newMatrixFx(t)
	ctx := context.Background()
	reg := matrixRegistry()

	root := f.instanceAdmin(t, "root")
	disabled := f.instanceAdmin(t, "disabled")
	f.exec(t, `UPDATE users SET disabled = 1 WHERE id = 'disabled'`)
	tomb := f.addUser(t, "tomb")
	f.assign(t, "user", "tomb", "admin", "")
	f.exec(t, `UPDATE users SET deleted_at = 3 WHERE id = 'tomb'`)
	viewerA := f.addUser(t, "viewer-a")
	f.assign(t, "user", "viewer-a", "viewer", "env-a")
	opA := f.addUser(t, "op-a")
	f.assign(t, "user", "op-a", "operator", "env-a")
	shellA := f.addUser(t, "shell-a")
	f.assign(t, "user", "shell-a", "operator", "env-a", string(permShell))
	viaTeam := f.addUser(t, "via-team")
	f.team(t, "t-1", "ops", "via-team")
	f.assign(t, "team", "t-1", "operator", "env-b", string(permSecret))
	f.assign(t, "user", "via-team", "viewer", "env-a")
	allAdmin := f.addUser(t, "all-admin")
	f.assign(t, "user", "all-admin", "admin", "")
	mixed := f.addUser(t, "mixed")
	f.assign(t, "user", "mixed", "viewer", "")
	f.assign(t, "user", "mixed", "admin", "env-a")
	none := f.addUser(t, "none")
	ghost := user("ghost")

	principals := map[string]*rbac.Principal{
		"instance admin": root, "disabled instance admin": disabled, "tombstoned": tomb,
		"viewer on A": viewerA, "operator on A": opA, "operator with shell on A": shellA,
		"team operator on B and direct viewer on A": viaTeam, "admin on all": allAdmin,
		"viewer on all and admin on A": mixed, "no grant": none, "no row": ghost,
		"system:janitor": rbac.System("system:janitor"), "anonymous": nil,
	}
	for name, p := range principals {
		t.Run(name, func(t *testing.T) {
			global, perEnv, err := f.c.EffectivePermissions(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			for _, perm := range reg {
				if perm.Scope == rbac.ScopeGlobal {
					want := slices.Contains(global, perm.Name)
					if got := can(f, p, perm.Name, ""); got != want {
						t.Errorf("%s: Check = %v, EffectivePermissions = %v", perm.Name, got, want)
					}
					continue
				}
				for _, env := range []string{"env-a", "env-b", "missing"} {
					ea, err := f.c.EnvAccess(ctx, p, env)
					if err != nil {
						t.Fatal(err)
					}
					// An environment with no entry of its own inherits "*".
					list, ok := perEnv[env]
					if !ok {
						list = perEnv[rbac.AllEnvironments]
					}
					check, access, effective := can(f, p, perm.Name, env), ea.Allows(perm), slices.Contains(list, perm.Name)
					if check != access || check != effective {
						t.Errorf("%s on %s: Check = %v, EnvAccess.Allows = %v, EffectivePermissions = %v", perm.Name, env, check, access, effective)
					}
				}
			}
		})
	}
}

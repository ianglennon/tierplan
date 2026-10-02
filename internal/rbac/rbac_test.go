// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rbac_test

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/internal/store/storetest"
)

func perms(t *testing.T) map[rbac.Perm]rbac.Permission {
	t.Helper()
	m := map[rbac.Perm]rbac.Permission{}
	for _, p := range rbac.All() {
		if _, dup := m[p.Name]; dup {
			t.Fatalf("permission %s registered twice", p.Name)
		}
		m[p.Name] = p
	}
	return m
}

func TestRegistryHoldsThePhase1Permissions(t *testing.T) {
	m := perms(t)
	want := map[rbac.Perm]rbac.Scope{
		"environments.read":  rbac.ScopeEnvironment,
		"environments.write": rbac.ScopeGlobal,
		"containers.read":    rbac.ScopeEnvironment,
		"images.read":        rbac.ScopeEnvironment,
		"volumes.read":       rbac.ScopeEnvironment,
		"networks.read":      rbac.ScopeEnvironment,
		"audit.read":         rbac.ScopeGlobal,
		"settings.read":      rbac.ScopeGlobal,
		"settings.write":     rbac.ScopeGlobal,
		"containers.exec":    rbac.ScopeEnvironment,
		"secrets.reveal":     rbac.ScopeEnvironment,
		"volumes.delete":     rbac.ScopeEnvironment,
		"janitor.policy":     rbac.ScopeEnvironment,
	}
	for name, scope := range want {
		p, ok := m[name]
		if !ok {
			t.Errorf("%s is not registered", name)
			continue
		}
		if p.Scope != scope {
			t.Errorf("%s scope = %d, want %d", name, p.Scope, scope)
		}
		if got, found := rbac.Lookup(name); !found || got != p {
			t.Errorf("Lookup(%s) = %+v, %v", name, got, found)
		}
	}
	if len(m) != len(want) {
		t.Errorf("registry has %d permissions, want exactly %d", len(m), len(want))
	}
	if _, found := rbac.Lookup("nope.read"); found {
		t.Error("Lookup found a permission that is not registered")
	}
}

func TestExactlyTheFourDangerousPermissionsAreFlagged(t *testing.T) {
	var got []string
	for _, p := range rbac.All() {
		if p.Dangerous {
			got = append(got, string(p.Name))
		}
	}
	slices.Sort(got)
	want := []string{"containers.exec", "janitor.policy", "secrets.reveal", "volumes.delete"}
	if !slices.Equal(got, want) {
		t.Fatalf("dangerous = %v, want %v", got, want)
	}
}

func TestMustBeValidPanicsForAnUnregisteredPermission(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a requirement for an unregistered permission must panic")
		}
	}()
	rbac.Requirement{Perm: "nope.read", Scope: rbac.ScopeGlobal}.MustBeValid()
}

func TestMustBeValidPanicsWhenTheScopeDiffersFromTheRegistry(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a requirement whose scope differs from the registry must panic")
		}
	}()
	// audit.read is global; declaring it environment-scoped must be refused.
	rbac.Requirement{Perm: rbac.PermAuditRead, Scope: rbac.ScopeEnvironment}.MustBeValid()
}

func TestMustBeValidAcceptsRegisteredRequirements(t *testing.T) {
	rbac.Public.MustBeValid()
	rbac.SelfService.MustBeValid()
	rbac.Require(rbac.PermAuditRead).MustBeValid()
	if got := rbac.Require(rbac.PermContainersRead); got.Scope != rbac.ScopeEnvironment || got.Perm != rbac.PermContainersRead {
		t.Fatalf("Require(containers.read) = %+v", got)
	}
}

func seeded(t *testing.T) (*store.Store, *rbac.Checker) {
	t.Helper()
	s := storetest.New(t)
	if err := rbac.SeedAdminRole(context.Background(), s); err != nil {
		t.Fatalf("SeedAdminRole: %v", err)
	}
	return s, rbac.NewChecker(s)
}

func rolePerms(t *testing.T, s *store.Store, role string) []string {
	t.Helper()
	got, err := s.RQ.ListRolePermissions(context.Background(), role)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestSeedAdminRoleHoldsEveryPermissionAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s, _ := seeded(t)
	if err := rbac.SeedAdminRole(ctx, s); err != nil {
		t.Fatal(err)
	}
	got := rolePerms(t, s, "admin")
	if len(got) != len(rbac.All()) {
		t.Fatalf("admin holds %d permissions after two seeds, want %d: %v", len(got), len(rbac.All()), got)
	}
	for _, p := range rbac.All() {
		if !slices.Contains(got, string(p.Name)) {
			t.Errorf("admin lacks %s", p.Name)
		}
	}
}

func TestSeedAdminRoleRemovesAPermissionThatIsNoLongerRegistered(t *testing.T) {
	ctx := context.Background()
	s, _ := seeded(t)
	if err := s.Q.InsertRolePermission(ctx, sqlc.InsertRolePermissionParams{RoleID: "admin", Permission: "retired.read"}); err != nil {
		t.Fatal(err)
	}
	if err := rbac.SeedAdminRole(ctx, s); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(rolePerms(t, s, "admin"), "retired.read") {
		t.Fatal("the stale permission survived a re-seed")
	}
}

func user(id string) *rbac.Principal {
	return &rbac.Principal{Type: rbac.PrincipalUser, ID: id, Name: id}
}

func assign(t *testing.T, s *store.Store, p *rbac.Principal) {
	t.Helper()
	tx, err := s.Write.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rbac.AssignGlobalRole(context.Background(), tx, s.Q, p.Type, p.ID, "admin"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("AssignGlobalRole: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRefusesANilPrincipalAsUnauthenticated(t *testing.T) {
	_, c := seeded(t)
	d := c.Check(context.Background(), nil, rbac.Require(rbac.PermAuditRead), "")
	if d.Allowed || d.Reason != "unauthenticated" {
		t.Fatalf("decision = %+v, want denied unauthenticated", d)
	}
	if d := c.Check(context.Background(), nil, rbac.SelfService, ""); d.Allowed || d.Reason != "unauthenticated" {
		t.Fatalf("anonymous SelfService = %+v, want denied unauthenticated", d)
	}
	if d := c.Check(context.Background(), nil, rbac.Public, ""); !d.Allowed {
		t.Fatalf("anonymous Public = %+v, want allowed", d)
	}
}

func TestCheckAllowsAGlobalAdminEverywhere(t *testing.T) {
	s, c := seeded(t)
	p := user("u-admin")
	assign(t, s, p)
	ctx := context.Background()
	if d := c.Check(ctx, p, rbac.Require(rbac.PermAuditRead), ""); !d.Allowed {
		t.Fatalf("admin audit.read = %+v", d)
	}
	for _, env := range []string{"env-a", "env-b"} {
		if d := c.Check(ctx, p, rbac.Require(rbac.PermContainersRead), env); !d.Allowed {
			t.Fatalf("admin containers.read in %s = %+v", env, d)
		}
	}
	if d := c.Check(ctx, p, rbac.SelfService, ""); !d.Allowed {
		t.Fatalf("admin SelfService = %+v", d)
	}
}

func TestCheckDeniesByDefaultWithoutAnAssignment(t *testing.T) {
	_, c := seeded(t)
	p := user("u-nobody")
	for _, perm := range rbac.All() {
		d := c.Check(context.Background(), p, rbac.Require(perm.Name), "env-a")
		if d.Allowed || d.Reason != "forbidden" {
			t.Errorf("%s: decision = %+v, want denied forbidden", perm.Name, d)
		}
	}
	if d := c.Check(context.Background(), p, rbac.SelfService, ""); !d.Allowed {
		t.Errorf("a signed-in user must pass SelfService, got %+v", d)
	}
}

func TestCheckDeniesAPermissionThatIsNotRegistered(t *testing.T) {
	s, c := seeded(t)
	p := user("u-admin")
	assign(t, s, p)
	d := c.Check(context.Background(), p, rbac.Requirement{Perm: "nope.read", Scope: rbac.ScopeGlobal}, "")
	if d.Allowed {
		t.Fatalf("an unregistered permission must be denied even for an admin: %+v", d)
	}
}

func TestSystemPrincipalsHaveExplicitEmptySets(t *testing.T) {
	for _, name := range []string{"system:cli", "system:janitor", "system:gitsync", "system:webhook"} {
		perms, ok := rbac.SystemPrincipals[name]
		if !ok {
			t.Errorf("%s has no explicit permission set", name)
			continue
		}
		if len(perms) != 0 {
			t.Errorf("%s holds %v, want none in Phase 1", name, perms)
		}
		p := rbac.System(name)
		if p == nil || p.Type != rbac.PrincipalSystem || p.ID != name || p.Name != name {
			t.Errorf("System(%s) = %+v", name, p)
			continue
		}
		if a := p.AuditActor(); a.Type != audit.ActorSystem || a.ID != name || a.Name != name {
			t.Errorf("AuditActor(%s) = %+v", name, a)
		}
	}
	if rbac.System("system:unknown") != nil {
		t.Error("System returned a principal for a name that is not defined")
	}
}

func TestCheckDeniesASystemPrincipalWithAnEmptySet(t *testing.T) {
	s, c := seeded(t)
	// Even a role assignment row for the system principal is not consulted:
	// system principals use their explicit in-code set only.
	tx, _ := s.Write.BeginTx(context.Background(), nil)
	if err := rbac.AssignGlobalRole(context.Background(), tx, s.Q, rbac.PrincipalSystem, "system:janitor", "admin"); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit()
	d := c.Check(context.Background(), rbac.System("system:janitor"), rbac.Require(rbac.PermContainersRead), "env-a")
	if d.Allowed || d.Reason != "forbidden" {
		t.Fatalf("system:janitor containers.read = %+v, want denied forbidden", d)
	}
}

func TestUserAuditActor(t *testing.T) {
	a := user("u1").AuditActor()
	if a.Type != audit.ActorUser || a.ID != "u1" {
		t.Fatalf("AuditActor = %+v", a)
	}
}

func TestEffectivePermissionsForTheAdmin(t *testing.T) {
	s, c := seeded(t)
	p := user("u-admin")
	assign(t, s, p)
	global, perEnv, err := c.EffectivePermissions(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	var wantGlobal, wantEnv []string
	for _, perm := range rbac.All() {
		if perm.Scope == rbac.ScopeGlobal {
			wantGlobal = append(wantGlobal, string(perm.Name))
		} else {
			wantEnv = append(wantEnv, string(perm.Name))
		}
	}
	slices.Sort(wantGlobal)
	slices.Sort(wantEnv)
	var gotGlobal, gotEnv []string
	for _, g := range global {
		gotGlobal = append(gotGlobal, string(g))
	}
	for _, e := range perEnv["*"] {
		gotEnv = append(gotEnv, string(e))
	}
	slices.Sort(gotGlobal)
	slices.Sort(gotEnv)
	if !slices.Equal(gotGlobal, wantGlobal) {
		t.Errorf("global = %v, want %v", gotGlobal, wantGlobal)
	}
	if !slices.Equal(gotEnv, wantEnv) {
		t.Errorf("perEnv[*] = %v, want %v", gotEnv, wantEnv)
	}
	if len(perEnv) != 1 {
		t.Errorf("perEnv has keys beyond *: %v", perEnv)
	}
}

func TestEffectivePermissionsForAUserWithNoAssignmentIsEmpty(t *testing.T) {
	_, c := seeded(t)
	global, perEnv, err := c.EffectivePermissions(context.Background(), user("u-nobody"))
	if err != nil || len(global) != 0 || len(perEnv) != 0 {
		t.Fatalf("got %v %v %v, want empty", global, perEnv, err)
	}
}

func TestAnEnvironmentAssignmentOnlyAppliesToThatEnvironment(t *testing.T) {
	ctx := context.Background()
	s, c := seeded(t)
	p := user("u-env")
	tx, err := s.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Q.WithTx(tx).InsertRoleAssignment(ctx, sqlc.InsertRoleAssignmentParams{
		ID: "ra-1", PrincipalType: string(p.Type), PrincipalID: p.ID, RoleID: "admin", EnvID: nullStr("env-a"), CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if d := c.Check(ctx, p, rbac.Require(rbac.PermContainersRead), "env-a"); !d.Allowed {
		t.Errorf("env-a containers.read = %+v, want allowed", d)
	}
	if d := c.Check(ctx, p, rbac.Require(rbac.PermContainersRead), "env-b"); d.Allowed {
		t.Errorf("env-b containers.read = %+v, want denied", d)
	}
	if d := c.Check(ctx, p, rbac.Require(rbac.PermAuditRead), ""); d.Allowed {
		t.Errorf("an environment assignment must not grant global audit.read: %+v", d)
	}
	if d := c.Check(ctx, p, rbac.Require(rbac.PermContainersRead), ""); d.Allowed {
		t.Errorf("a check with no environment must not match an environment assignment: %+v", d)
	}
	global, perEnv, err := c.EffectivePermissions(ctx, p)
	if err != nil || len(global) != 0 || len(perEnv["env-a"]) == 0 || len(perEnv["*"]) != 0 {
		t.Errorf("effective = %v %v %v", global, perEnv, err)
	}
}

func TestAssignGlobalRoleIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s, _ := seeded(t)
	p := user("u-admin")
	assign(t, s, p)
	assign(t, s, p)
	var n int
	if err := s.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM role_assignments`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("assignments = %d (%v), want 1", n, err)
	}
}

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }

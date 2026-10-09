// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rbac_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/storetest"
)

// retiredEnvironmentsWrite is the permission D-49 split up. It is spelled out
// only here, so no production code can name it without this test noticing.
const retiredEnvironmentsWrite = "environments" + ".write"

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

func TestRegistryPinsEveryPermissionsScopeMinRoleAndExtra(t *testing.T) {
	m := perms(t)
	type pin struct {
		scope     rbac.Scope
		min       rbac.Role
		extra     bool
		dangerous bool
	}
	g := func(dangerous bool) pin { return pin{scope: rbac.ScopeGlobal, dangerous: dangerous} }
	e := func(min rbac.Role) pin { return pin{scope: rbac.ScopeEnvironment, min: min} }
	x := pin{scope: rbac.ScopeEnvironment, min: rbac.RoleOperator, extra: true, dangerous: true}
	want := map[rbac.Perm]pin{
		"environments.read":      e(rbac.RoleViewer),
		"environments.create":    g(false),
		"environments.manage":    g(false),
		"environments.token":     g(true),
		"environments.revoke":    e(rbac.RoleAdmin),
		"environments.delete":    g(true),
		"environments.configure": e(rbac.RoleAdmin),
		"environments.access":    e(rbac.RoleAdmin),
		"environments.audit":     e(rbac.RoleAdmin),
		"containers.read":        e(rbac.RoleViewer),
		"images.read":            e(rbac.RoleViewer),
		"volumes.read":           e(rbac.RoleViewer),
		"networks.read":          e(rbac.RoleViewer),
		"containers.control":     e(rbac.RoleOperator),
		"containers.remove":      e(rbac.RoleOperator),
		"audit.read":             g(false),
		"settings.read":          g(false),
		"settings.write":         g(false),
		"users.manage":           g(false),
		"teams.manage":           g(false),
		"backup.manage":          g(true),
		"containers.exec":        x,
		"secrets.reveal":         x,
		"volumes.delete":         x,
		"janitor.policy":         x,
	}
	for name, w := range want {
		p, ok := m[name]
		if !ok {
			t.Errorf("%s is not registered", name)
			continue
		}
		got := pin{scope: p.Scope, min: p.MinRole, extra: p.Extra, dangerous: p.Dangerous}
		if got != w {
			t.Errorf("%s = %+v, want %+v", name, got, w)
		}
		if l, found := rbac.Lookup(name); !found || l != p {
			t.Errorf("Lookup(%s) = %+v, %v", name, l, found)
		}
		if p.Scope == rbac.ScopeGlobal && p.MinRole != rbac.RoleNone {
			t.Errorf("%s is global but has MinRole %v; no environment role holds a global permission", name, p.MinRole)
		}
	}
	if len(m) != len(want) {
		t.Errorf("registry has %d permissions, want exactly %d", len(m), len(want))
	}
	if _, found := rbac.Lookup("nope.read"); found {
		t.Error("Lookup found a permission that is not registered")
	}
}

func TestEnrolmentPermissionsDescribeWhatTheyNowCover(t *testing.T) {
	m := perms(t)
	if got := m["environments.revoke"].Description; got != "Revoke, block and unblock this environment's agents" {
		t.Errorf("environments.revoke description = %q", got)
	}
	if got := m["environments.manage"].Description; got != "Fold the local host into an environment" {
		t.Errorf("environments.manage description = %q", got)
	}
}

func TestTheRetiredEnvironmentsWritePermissionIsNoLongerRegistered(t *testing.T) {
	if _, found := rbac.Lookup(retiredEnvironmentsWrite); found {
		t.Fatal("environments.write is still registered; D-49 split it into create, manage, token, revoke and delete")
	}
}

func TestExactlyTheDangerousPermissionsAreFlagged(t *testing.T) {
	var got []string
	for _, p := range rbac.All() {
		if p.Dangerous {
			got = append(got, string(p.Name))
		}
	}
	slices.Sort(got)
	want := []string{"backup.manage", "containers.exec", "environments.delete", "environments.token", "janitor.policy", "secrets.reveal", "volumes.delete"}
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

// fx is a store with the migrated schema and a Checker over it.
type fx struct {
	s *store.Store
	c *rbac.Checker
}

func newFx(t *testing.T, opts ...rbac.CheckerOption) *fx {
	t.Helper()
	s := storetest.New(t)
	return &fx{s: s, c: rbac.NewChecker(s, opts...)}
}

func (f *fx) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.s.Write.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// addUser inserts a real users row (a grant to an ID with no row is a no-op).
func (f *fx) addUser(t *testing.T, id string) *rbac.Principal {
	t.Helper()
	f.exec(t, `INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES (?, ?, 'x', 1, 1, 1)`, id, id)
	return user(id)
}

func (f *fx) instanceAdmin(t *testing.T, id string) *rbac.Principal {
	t.Helper()
	p := f.addUser(t, id)
	tx, err := f.s.Write.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rbac.GrantInstanceAdminTx(context.Background(), tx, f.s.Q, id); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return p
}

var assignSeq int

// assign grants role to a principal ("user" or "team") on envID, or on every
// environment when envID is empty, with the given extras.
func (f *fx) assign(t *testing.T, principalType, principalID, role, envID string, extras ...string) {
	t.Helper()
	assignSeq++
	id := fmt.Sprintf("ra-%d", assignSeq)
	var env any
	if envID != "" {
		env = envID
	}
	f.exec(t, `INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, principalType, principalID, role, env, assignSeq)
	for _, x := range extras {
		f.exec(t, `INSERT INTO assignment_extras (assignment_id, permission) VALUES (?, ?)`, id, x)
	}
}

func (f *fx) team(t *testing.T, id, name string, members ...string) {
	t.Helper()
	f.exec(t, `INSERT INTO teams (id, name, created_at, updated_at) VALUES (?, ?, 1, 1)`, id, name)
	for _, m := range members {
		f.exec(t, `INSERT INTO team_members (team_id, user_id, created_at) VALUES (?, ?, 1)`, id, m)
	}
}

func user(id string) *rbac.Principal {
	return &rbac.Principal{Type: rbac.PrincipalUser, ID: id, Name: id}
}

func allowed(f *fx, p *rbac.Principal, perm rbac.Perm, env string) bool {
	return f.c.Check(context.Background(), p, rbac.Require(perm), env).Allowed
}

func TestCheckRefusesANilPrincipalAsUnauthenticated(t *testing.T) {
	f := newFx(t)
	d := f.c.Check(context.Background(), nil, rbac.Require(rbac.PermAuditRead), "")
	if d.Allowed || d.Reason != "unauthenticated" {
		t.Fatalf("decision = %+v, want denied unauthenticated", d)
	}
	if d := f.c.Check(context.Background(), nil, rbac.SelfService, ""); d.Allowed || d.Reason != "unauthenticated" {
		t.Fatalf("anonymous SelfService = %+v, want denied unauthenticated", d)
	}
	if d := f.c.Check(context.Background(), nil, rbac.Public, ""); !d.Allowed {
		t.Fatalf("anonymous Public = %+v, want allowed", d)
	}
}

func TestAnInstanceAdminIsAllowedEveryRegisteredPermissionOnAnyEnvironment(t *testing.T) {
	f := newFx(t)
	p := f.instanceAdmin(t, "u-admin")
	for _, perm := range rbac.All() {
		for _, env := range []string{"env-a", "never-created", ""} {
			if perm.Scope == rbac.ScopeGlobal && env != "" {
				continue
			}
			if perm.Scope == rbac.ScopeEnvironment && env == "" {
				continue
			}
			if !allowed(f, p, perm.Name, env) {
				t.Errorf("instance admin refused %s on %q", perm.Name, env)
			}
		}
	}
	if d := f.c.Check(context.Background(), p, rbac.SelfService, ""); !d.Allowed {
		t.Fatalf("admin SelfService = %+v", d)
	}
}

func TestAUserWithNoAssignmentIsDeniedEverything(t *testing.T) {
	f := newFx(t)
	p := f.addUser(t, "u-nobody")
	for _, perm := range rbac.All() {
		d := f.c.Check(context.Background(), p, rbac.Require(perm.Name), "env-a")
		if d.Allowed || d.Reason != "forbidden" {
			t.Errorf("%s: decision = %+v, want denied forbidden", perm.Name, d)
		}
	}
	if d := f.c.Check(context.Background(), p, rbac.SelfService, ""); !d.Allowed {
		t.Errorf("a signed-in user must pass SelfService, got %+v", d)
	}
}

func TestACheckForAnUnregisteredPermissionIsDeniedEvenForAnInstanceAdmin(t *testing.T) {
	f := newFx(t)
	p := f.instanceAdmin(t, "u-admin")
	if d := f.c.Check(context.Background(), p, rbac.Requirement{Perm: "nope.read", Scope: rbac.ScopeGlobal}, ""); d.Allowed {
		t.Fatalf("an unregistered permission must be denied: %+v", d)
	}
	// A registered permission asked for in the wrong scope is refused too.
	if d := f.c.Check(context.Background(), p, rbac.Requirement{Perm: rbac.PermAuditRead, Scope: rbac.ScopeEnvironment}, "env-a"); d.Allowed {
		t.Fatalf("a mis-scoped permission must be denied: %+v", d)
	}
}

func TestEachRoleHoldsExactlyWhatTheRegistryGivesIt(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	for _, role := range []string{"viewer", "operator", "admin"} {
		f.addUser(t, "u-"+role)
		f.assign(t, "user", "u-"+role, role, "env-a")
	}
	r := map[string]rbac.Role{"viewer": rbac.RoleViewer, "operator": rbac.RoleOperator, "admin": rbac.RoleAdmin}
	for role, rr := range r {
		p := user("u-" + role)
		for _, perm := range rbac.All() {
			if perm.Scope != rbac.ScopeEnvironment {
				continue
			}
			// Extras reach an Operator only when the assignment carries them.
			want := rr >= perm.MinRole && (!perm.Extra || rr == rbac.RoleAdmin)
			if got := allowed(f, p, perm.Name, "env-a"); got != want {
				t.Errorf("%s on %s = %v, want %v", role, perm.Name, got, want)
			}
			if allowed(f, p, perm.Name, "env-b") {
				t.Errorf("%s was allowed %s on an environment it holds no assignment on", role, perm.Name)
			}
		}
		// No environment role grants a global permission (D-01, D-07).
		for _, perm := range rbac.All() {
			if perm.Scope == rbac.ScopeGlobal && f.c.Check(ctx, p, rbac.Require(perm.Name), "").Allowed {
				t.Errorf("%s was allowed global %s", role, perm.Name)
			}
		}
	}
}

func TestAnAdminOnEveryEnvironmentIsStillNotAnInstanceAdmin(t *testing.T) {
	f := newFx(t)
	p := f.addUser(t, "u-all")
	f.assign(t, "user", "u-all", "admin", "")
	if allowed(f, p, rbac.PermEnvironmentsCreate, "") {
		t.Error("Admin on all environments was allowed a global permission")
	}
	if !allowed(f, p, rbac.PermEnvironmentsAccess, "any-env") || !allowed(f, p, rbac.PermContainersExec, "any-env") {
		t.Error("Admin on all environments must hold every environment permission on any environment")
	}
}

func TestExtrasAreAdminAlwaysOperatorWhenCarriedViewerNever(t *testing.T) {
	f := newFx(t)
	f.addUser(t, "u-op-plain")
	f.assign(t, "user", "u-op-plain", "operator", "env-a")
	f.addUser(t, "u-op-exec")
	f.assign(t, "user", "u-op-exec", "operator", "env-a", "containers.exec")
	f.addUser(t, "u-viewer")
	f.assign(t, "user", "u-viewer", "viewer", "env-a", "containers.exec") // a Viewer cannot hold an extra
	f.addUser(t, "u-admin")
	f.assign(t, "user", "u-admin", "admin", "env-a")

	for _, c := range []struct {
		who  string
		perm rbac.Perm
		want bool
	}{
		{"u-op-plain", rbac.PermContainersExec, false},
		{"u-op-exec", rbac.PermContainersExec, true},
		{"u-op-exec", rbac.PermSecretsReveal, false}, // only the extra it carries
		{"u-viewer", rbac.PermContainersExec, false},
		{"u-admin", rbac.PermContainersExec, true},
		{"u-admin", rbac.PermJanitorPolicy, true},
	} {
		if got := allowed(f, user(c.who), c.perm, "env-a"); got != c.want {
			t.Errorf("%s %s = %v, want %v", c.who, c.perm, got, c.want)
		}
	}
}

func TestAccessIsTheUnionOfDirectTeamAndAllEnvironmentGrants(t *testing.T) {
	f := newFx(t)
	p := f.addUser(t, "u1")
	f.team(t, "t-b", "Bravo", "u1")
	f.team(t, "t-a", "Alpha", "u1")
	f.assign(t, "user", "u1", "viewer", "env-a", "containers.exec") // direct, ignored extra on viewer is carried but cannot count
	f.assign(t, "team", "t-a", "operator", "env-a", "secrets.reveal")
	f.assign(t, "team", "t-b", "operator", "env-a", "volumes.delete")
	f.assign(t, "user", "u1", "viewer", "")

	ea, err := f.c.EnvAccess(context.Background(), p, "env-a")
	if err != nil {
		t.Fatal(err)
	}
	if ea.Role != rbac.RoleOperator {
		t.Errorf("role = %v, want operator (highest wins)", ea.Role)
	}
	wantExtras := []rbac.Perm{"containers.exec", "secrets.reveal", "volumes.delete"}
	if !slices.Equal(ea.Extras, wantExtras) {
		t.Errorf("extras = %v, want the union %v", ea.Extras, wantExtras)
	}
	// Sources: all-environments, then teams by name, then direct.
	wantSrc := []rbac.Source{
		{Kind: rbac.SourceAllEnvironments},
		{Kind: rbac.SourceTeam, TeamID: "t-a", TeamName: "Alpha"},
		{Kind: rbac.SourceTeam, TeamID: "t-b", TeamName: "Bravo"},
		{Kind: rbac.SourceDirect},
	}
	if !slices.Equal(ea.Sources, wantSrc) {
		t.Errorf("sources = %+v, want %+v", ea.Sources, wantSrc)
	}
	// On an environment with no assignment of its own only the all-environments grant applies.
	other, err := f.c.EnvAccess(context.Background(), p, "env-b")
	if err != nil {
		t.Fatal(err)
	}
	if other.Role != rbac.RoleViewer || len(other.Extras) != 0 || len(other.Sources) != 1 || other.Sources[0].Kind != rbac.SourceAllEnvironments {
		t.Errorf("env-b access = %+v, want viewer from all environments only", other)
	}
}

func TestTwoSameRoleGrantsFromDifferentSourcesMergeIntoOneWithBothSources(t *testing.T) {
	f := newFx(t)
	p := f.addUser(t, "u1")
	f.team(t, "t-1", "Ops", "u1")
	f.assign(t, "user", "u1", "operator", "env-a")
	f.assign(t, "team", "t-1", "operator", "env-a")
	ea, err := f.c.EnvAccess(context.Background(), p, "env-a")
	if err != nil {
		t.Fatal(err)
	}
	want := []rbac.Source{{Kind: rbac.SourceTeam, TeamID: "t-1", TeamName: "Ops"}, {Kind: rbac.SourceDirect}}
	if ea.Role != rbac.RoleOperator || !slices.Equal(ea.Sources, want) {
		t.Fatalf("access = %+v, want operator with sources %+v", ea, want)
	}
}

func TestTeamSourcesSortByNameNotByID(t *testing.T) {
	f := newFx(t)
	p := f.addUser(t, "u1")
	// Team names are unique case-insensitively, so the ID tie-break can never
	// fire from the database; here the IDs sort opposite to the names.
	f.team(t, "t-2", "ops", "u1")
	f.team(t, "t-1", "ops-b", "u1")
	f.assign(t, "team", "t-2", "viewer", "env-a")
	f.assign(t, "team", "t-1", "viewer", "env-a")
	ea, err := f.c.EnvAccess(context.Background(), p, "env-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(ea.Sources) != 2 || ea.Sources[0].TeamName != "ops" || ea.Sources[1].TeamName != "ops-b" {
		t.Fatalf("sources = %+v, want ops then ops-b", ea.Sources)
	}
}

func TestAnInstanceAdminsAccessIsAdminWithEveryExtraAndOneSource(t *testing.T) {
	f := newFx(t)
	p := f.instanceAdmin(t, "u-admin")
	ea, err := f.c.EnvAccess(context.Background(), p, "env-anything")
	if err != nil {
		t.Fatal(err)
	}
	wantExtras := []rbac.Perm{"containers.exec", "janitor.policy", "secrets.reveal", "volumes.delete"}
	if ea.Role != rbac.RoleAdmin || !slices.Equal(ea.Extras, wantExtras) ||
		len(ea.Sources) != 1 || ea.Sources[0].Kind != rbac.SourceInstanceAdmin {
		t.Fatalf("access = %+v", ea)
	}
}

func TestADisabledUserIsAllowedNothingWhateverTheirRowSays(t *testing.T) {
	f := newFx(t)
	p := f.instanceAdmin(t, "u-1")
	f.assign(t, "user", "u-1", "admin", "env-a")
	f.exec(t, `UPDATE users SET disabled = 1 WHERE id = 'u-1'`)
	for _, perm := range rbac.All() {
		env := "env-a"
		if perm.Scope == rbac.ScopeGlobal {
			env = ""
		}
		if allowed(f, p, perm.Name, env) {
			t.Errorf("a disabled instance admin was allowed %s", perm.Name)
		}
	}
	a, err := f.c.Access(context.Background(), p)
	if err != nil || a.Active || a.InstanceAdmin || len(a.Envs) != 0 {
		t.Fatalf("access = %+v, %v; want empty and inactive", a, err)
	}
}

func TestADeletedTombstonedUserIsAllowedNothing(t *testing.T) {
	f := newFx(t)
	p := f.instanceAdmin(t, "u-1")
	f.assign(t, "user", "u-1", "admin", "")
	f.exec(t, `UPDATE users SET deleted_at = 5 WHERE id = 'u-1'`)
	if allowed(f, p, rbac.PermAuditRead, "") || allowed(f, p, rbac.PermContainersRead, "env-a") {
		t.Fatal("a tombstoned user was allowed something")
	}
}

func TestAPrincipalWithNoUserRowIsAllowedNothing(t *testing.T) {
	f := newFx(t)
	f.assign(t, "user", "ghost", "admin", "")
	if allowed(f, user("ghost"), rbac.PermContainersRead, "env-a") {
		t.Fatal("an assignment for an ID with no user row must not grant anything")
	}
}

func TestSystemPrincipalsHaveExplicitEmptySets(t *testing.T) {
	for _, name := range []string{
		"system:cli", "system:janitor", "system:gitsync", "system:webhook",
		"system:agent-enrol", "system:agent-update", "system:agent-reaper", "system:agent-hub",
	} {
		perms, ok := rbac.SystemPrincipals[name]
		if !ok {
			t.Errorf("%s has no explicit permission set", name)
			continue
		}
		if len(perms) != 0 {
			t.Errorf("%s holds %v, want none", name, perms)
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

func TestASystemPrincipalNeverConsultsAssignments(t *testing.T) {
	f := newFx(t)
	f.assign(t, "system", "system:janitor", "admin", "")
	d := f.c.Check(context.Background(), rbac.System("system:janitor"), rbac.Require(rbac.PermContainersRead), "env-a")
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

func sorted(ps []rbac.Perm) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, string(p))
	}
	slices.Sort(out)
	return out
}

func TestEffectivePermissionsForTheInstanceAdminAreEverything(t *testing.T) {
	f := newFx(t)
	p := f.instanceAdmin(t, "u-admin")
	global, perEnv, err := f.c.EffectivePermissions(context.Background(), p)
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
	if got := sorted(global); !slices.Equal(got, wantGlobal) {
		t.Errorf("global = %v, want %v", got, wantGlobal)
	}
	if got := sorted(perEnv["*"]); !slices.Equal(got, wantEnv) {
		t.Errorf("perEnv[*] = %v, want %v", got, wantEnv)
	}
	if len(perEnv) != 1 {
		t.Errorf("perEnv has keys beyond *: %v", perEnv)
	}
}

func TestEffectivePermissionsAgreeWithCheck(t *testing.T) {
	f := newFx(t)
	p := f.addUser(t, "u1")
	f.addUser(t, "u2")
	f.team(t, "t1", "Ops", "u1")
	f.assign(t, "team", "t1", "operator", "env-a", "containers.exec")
	f.assign(t, "user", "u1", "viewer", "")
	global, perEnv, err := f.c.EffectivePermissions(context.Background(), p)
	if err != nil || len(global) != 0 {
		t.Fatalf("global = %v, err = %v; a non-admin holds no global permission", global, err)
	}
	for env, list := range perEnv {
		for _, perm := range rbac.All() {
			if perm.Scope != rbac.ScopeEnvironment {
				continue
			}
			checkEnv := env
			if env == "*" {
				checkEnv = "env-zzz"
			}
			if got, want := slices.Contains(list, perm.Name), allowed(f, p, perm.Name, checkEnv); got != want {
				t.Errorf("env %s %s: EffectivePermissions %v, Check %v", env, perm.Name, got, want)
			}
		}
	}
	if !slices.Contains(perEnv["env-a"], rbac.PermContainersExec) || slices.Contains(perEnv["*"], rbac.PermContainersExec) {
		t.Errorf("perEnv = %v", perEnv)
	}
	// A user with nothing is empty.
	g2, e2, err := f.c.EffectivePermissions(context.Background(), user("u2"))
	if err != nil || len(g2) != 0 || len(e2) != 0 {
		t.Fatalf("got %v %v %v, want empty", g2, e2, err)
	}
}

func TestACheckerCanBeGivenItsOwnRegistry(t *testing.T) {
	custom := rbac.Registry{
		{Name: "widgets.read", Scope: rbac.ScopeEnvironment, MinRole: rbac.RoleViewer},
		{Name: "widgets.burn", Scope: rbac.ScopeEnvironment, MinRole: rbac.RoleOperator, Extra: true},
		{Name: "widgets.admin", Scope: rbac.ScopeGlobal},
	}
	f := newFx(t, rbac.WithRegistry(custom))
	v := f.addUser(t, "v")
	f.assign(t, "user", "v", "viewer", "env-a")
	o := f.addUser(t, "o")
	f.assign(t, "user", "o", "operator", "env-a", "widgets.burn")
	root := f.instanceAdmin(t, "root")
	ctx := context.Background()
	check := func(p *rbac.Principal, perm rbac.Perm, scope rbac.Scope, env string) bool {
		return f.c.Check(ctx, p, rbac.Requirement{Perm: perm, Scope: scope}, env).Allowed
	}
	if !check(v, "widgets.read", rbac.ScopeEnvironment, "env-a") || check(v, "widgets.burn", rbac.ScopeEnvironment, "env-a") {
		t.Error("viewer on the custom registry")
	}
	if !check(o, "widgets.burn", rbac.ScopeEnvironment, "env-a") {
		t.Error("operator carrying the custom extra")
	}
	if !check(root, "widgets.admin", rbac.ScopeGlobal, "") || !check(root, "widgets.burn", rbac.ScopeEnvironment, "env-z") {
		t.Error("instance admin on the custom registry")
	}
	if check(root, "containers.read", rbac.ScopeEnvironment, "env-a") {
		t.Error("a permission outside the custom registry must be denied")
	}
	if got := sorted(func() []rbac.Perm { a, _ := f.c.Access(ctx, root); return a.For("x").Extras }()); !slices.Equal(got, []string{"widgets.burn"}) {
		t.Errorf("instance admin extras = %v", got)
	}
}

func TestEnvAccessAllowsRules(t *testing.T) {
	read := rbac.Permission{Name: "a.read", Scope: rbac.ScopeEnvironment, MinRole: rbac.RoleViewer}
	exec := rbac.Permission{Name: "a.exec", Scope: rbac.ScopeEnvironment, MinRole: rbac.RoleOperator, Extra: true}
	glob := rbac.Permission{Name: "a.glob", Scope: rbac.ScopeGlobal}
	if (rbac.EnvAccess{}).Allows(read) {
		t.Error("the zero EnvAccess must allow nothing")
	}
	if (rbac.EnvAccess{Role: rbac.RoleAdmin}).Allows(glob) {
		t.Error("no environment role holds a global permission")
	}
	if !(rbac.EnvAccess{Role: rbac.RoleOperator}).Allows(read) {
		t.Error("operator reads")
	}
	if (rbac.EnvAccess{Role: rbac.RoleOperator}).Allows(exec) {
		t.Error("operator without the extra must not exec")
	}
	if !(rbac.EnvAccess{Role: rbac.RoleOperator, Extras: []rbac.Perm{"a.exec"}}).Allows(exec) {
		t.Error("operator with the extra execs")
	}
	if (rbac.EnvAccess{Role: rbac.RoleViewer, Extras: []rbac.Perm{"a.exec"}}).Allows(exec) {
		t.Error("a viewer never holds an extra")
	}
	if !(rbac.EnvAccess{Role: rbac.RoleAdmin}).Allows(exec) {
		t.Error("admin always holds an extra")
	}
}

func TestGrantInstanceAdminTx(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	f.addUser(t, "u1")
	for i := 0; i < 2; i++ { // granting twice is fine
		tx, err := f.s.Write.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := rbac.GrantInstanceAdminTx(ctx, tx, f.s.Q, "u1"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	var flag int
	if err := f.s.Read.QueryRowContext(ctx, `SELECT instance_admin FROM users WHERE id = 'u1'`).Scan(&flag); err != nil || flag != 1 {
		t.Fatalf("instance_admin = %d, %v", flag, err)
	}
	tx, err := f.s.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := rbac.GrantInstanceAdminTx(ctx, tx, f.s.Q, "no-such-user"); err == nil {
		t.Fatal("granting to a user with no row must fail, not silently do nothing")
	}
}

func TestRoleParsing(t *testing.T) {
	for _, id := range []string{"viewer", "operator", "admin"} {
		r, ok := rbac.ParseRole(id)
		if !ok || r.ID() != id || r.String() != id {
			t.Errorf("ParseRole(%q) = %v, %v", id, r, ok)
		}
	}
	for _, bad := range []string{"", "owner", "Admin", "none"} {
		if r, ok := rbac.ParseRole(bad); ok || r != rbac.RoleNone {
			t.Errorf("ParseRole(%q) = %v, %v; want none", bad, r, ok)
		}
	}
	if rbac.RoleNone.String() != "none" || rbac.RoleNone.ID() != "" {
		t.Error("RoleNone naming")
	}
	if rbac.RoleViewer >= rbac.RoleOperator || rbac.RoleOperator >= rbac.RoleAdmin {
		t.Error("roles must be ordered viewer < operator < admin")
	}
}

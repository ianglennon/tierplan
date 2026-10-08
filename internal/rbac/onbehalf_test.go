// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rbac_test

import (
	"context"
	"testing"

	"github.com/ianglennon/bayplan/internal/rbac"
)

// check asks the matrix registry's requirement of perm for p on env.
func check(f *fx, p *rbac.Principal, perm rbac.Perm, env string) rbac.Decision {
	reg, ok := matrixRegistry().Lookup(perm)
	if !ok {
		panic("not in the matrix registry: " + string(perm))
	}
	return f.c.Check(context.Background(), p, rbac.Requirement{Perm: perm, Scope: reg.Scope}, env)
}

func TestOnBehalfOfAnOperatorIsAllowedWhatAnOperatorIsAndNoMore(t *testing.T) {
	f := newMatrixFx(t)
	f.addUser(t, "alice")
	f.assign(t, "user", "alice", "operator", "env-a")
	p := rbac.OnBehalf("system:gitsync", "alice")
	if p == nil || p.OnBehalfOf != "alice" || p.Type != rbac.PrincipalSystem || p.ID != "system:gitsync" {
		t.Fatalf("OnBehalf = %+v", p)
	}

	if d := check(f, p, permOperate, "env-a"); !d.Allowed {
		t.Errorf("an automation for an Operator was refused test.operate on its environment: %+v", d)
	}
	if d := check(f, p, permOperate, "env-b"); d.Allowed || d.Reason != rbac.ReasonOwnerLacksPermission {
		t.Errorf("another environment = %+v, want owner_lacks_permission", d)
	}
	if d := check(f, p, permManage, "env-a"); d.Allowed || d.Reason != rbac.ReasonOwnerLacksPermission {
		t.Errorf("an Admin permission = %+v, want owner_lacks_permission", d)
	}
	if d := check(f, p, permGlobal, ""); d.Allowed || d.Reason != rbac.ReasonOwnerLacksPermission {
		t.Errorf("a global permission = %+v, want owner_lacks_permission", d)
	}
	if d := check(f, p, permShell, "env-a"); d.Allowed {
		t.Errorf("an extra the owner's assignment does not carry was allowed: %+v", d)
	}
}

func TestOnBehalfOfAnInstanceAdminIsAllowedEverything(t *testing.T) {
	f := newMatrixFx(t)
	f.instanceAdmin(t, "root")
	p := rbac.OnBehalf("system:webhook", "root")
	for _, perm := range []rbac.Perm{permRead, permOperate, permShell, permManage, permGlobal} {
		env := "env-a"
		if perm == permGlobal {
			env = ""
		}
		if d := check(f, p, perm, env); !d.Allowed {
			t.Errorf("an automation for an instance admin was refused %s: %+v", perm, d)
		}
	}
}

func TestOnBehalfIsRefusedForAnUnknownSystemPrincipalAndForAnInactiveOwner(t *testing.T) {
	f := newMatrixFx(t)
	f.addUser(t, "alice")
	f.assign(t, "user", "alice", "admin", "env-a")
	f.addUser(t, "off")
	f.assign(t, "user", "off", "admin", "env-a")
	f.exec(t, `UPDATE users SET disabled = 1 WHERE id = 'off'`)
	f.addUser(t, "gone")
	f.assign(t, "user", "gone", "admin", "env-a")
	f.exec(t, `UPDATE users SET deleted_at = 5 WHERE id = 'gone'`)

	cases := []struct {
		name   string
		p      *rbac.Principal
		reason string
	}{
		{"a system principal that is not listed", &rbac.Principal{Type: rbac.PrincipalSystem, ID: "system:unknown", OnBehalfOf: "alice"}, rbac.ReasonForbidden},
		{"an owner who does not exist", &rbac.Principal{Type: rbac.PrincipalSystem, ID: "system:gitsync", OnBehalfOf: "nope"}, rbac.ReasonOwnerInactive},
		{"a disabled owner", rbac.OnBehalf("system:gitsync", "off"), rbac.ReasonOwnerInactive},
		{"a deleted owner", rbac.OnBehalf("system:gitsync", "gone"), rbac.ReasonOwnerInactive},
	}
	for _, c := range cases {
		d := check(f, c.p, permRead, "env-a")
		if d.Allowed || d.Reason != c.reason || d.Err != nil {
			t.Errorf("%s: %+v, want a refusal with reason %s", c.name, d, c.reason)
		}
	}
	if rbac.OnBehalf("system:unknown", "alice") != nil {
		t.Error("OnBehalf returned a principal for a system name that is not listed")
	}
}

func TestOnBehalfEnvAccessIsTheOwnersAndAnUnknownSystemPrincipalHasNone(t *testing.T) {
	f := newMatrixFx(t)
	f.addUser(t, "alice")
	f.assign(t, "user", "alice", "operator", "env-a", string(permShell))
	want := envAccess(t, f, user("alice"), "env-a")
	got := envAccess(t, f, rbac.OnBehalf("system:gitsync", "alice"), "env-a")
	if got.Role != want.Role || len(got.Extras) != 1 || got.Extras[0] != permShell {
		t.Errorf("EnvAccess on behalf = %+v, want the owner's %+v", got, want)
	}
	forged := &rbac.Principal{Type: rbac.PrincipalSystem, ID: "system:unknown", OnBehalfOf: "alice"}
	if ea := envAccess(t, f, forged, "env-a"); ea.Role != rbac.RoleNone {
		t.Errorf("EnvAccess of an unlisted system principal = %+v, want none", ea)
	}
}

func TestAccessQSeesTheChangesOfTheTransactionItRunsIn(t *testing.T) {
	f := newMatrixFx(t)
	f.addUser(t, "alice")
	f.assign(t, "user", "alice", "operator", "env-a")
	ctx := context.Background()

	tx, err := f.s.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM role_assignments WHERE principal_id = 'alice'`); err != nil {
		t.Fatal(err)
	}
	inTx, err := f.c.AccessQ(ctx, f.s.Q.WithTx(tx), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !inTx.Active || inTx.For("env-a").Role != rbac.RoleNone {
		t.Errorf("inside the transaction alice has %v, want no role", inTx.For("env-a").Role)
	}
	outside, err := f.c.Access(ctx, user("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if outside.For("env-a").Role != rbac.RoleOperator {
		t.Errorf("outside the transaction alice has %v, want the uncommitted deletion unseen", outside.For("env-a").Role)
	}
}

func TestOnBehalfEffectivePermissionsAreTheOwnersAndAgreeWithCheck(t *testing.T) {
	f := newMatrixFx(t)
	f.addUser(t, "alice")
	f.assign(t, "user", "alice", "operator", "env-a")
	ctx := context.Background()

	_, ownerEnv, err := f.c.EffectivePermissions(ctx, user("alice"))
	if err != nil {
		t.Fatal(err)
	}
	p := rbac.OnBehalf("system:gitsync", "alice")
	global, onBehalfEnv, err := f.c.EffectivePermissions(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(global) != 0 || len(onBehalfEnv["env-a"]) == 0 || len(onBehalfEnv["env-a"]) != len(ownerEnv["env-a"]) {
		t.Fatalf("on behalf = %v %v, owner = %v", global, onBehalfEnv, ownerEnv)
	}
	for _, perm := range onBehalfEnv["env-a"] {
		if !check(f, p, perm, "env-a").Allowed {
			t.Errorf("EffectivePermissions lists %s but Check refuses it", perm)
		}
	}
	forged := &rbac.Principal{Type: rbac.PrincipalSystem, ID: "system:unknown", OnBehalfOf: "alice"}
	if g, e, err := f.c.EffectivePermissions(ctx, forged); err != nil || len(g) != 0 || len(e) != 0 {
		t.Errorf("a forged principal lists %v %v, %v", g, e, err)
	}
}

func TestOnBehalfAPlainSystemPrincipalStillHoldsOnlyItsOwnEmptySet(t *testing.T) {
	f := newMatrixFx(t)
	f.instanceAdmin(t, "root")
	// system:gitsync acting for no one never borrows an owner's rights.
	if d := check(f, rbac.System("system:gitsync"), permRead, "env-a"); d.Allowed {
		t.Errorf("a system principal with no owner was allowed: %+v", d)
	}
}

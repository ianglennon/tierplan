// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package rbac

import (
	"slices"
	"testing"
)

// These tests are in package rbac because they reset the package registry, which
// no other package can do.

func testOwnedRegistry() Registry {
	return Registry{
		{Name: "test.deploy", Scope: ScopeEnvironment, MinRole: RoleOperator},
		{Name: "test.global", Scope: ScopeGlobal},
	}
}

func mustPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s did not panic", what)
		}
	}()
	f()
}

func TestRegisteredOwnedKindsAreFoundAndListedSortedByKind(t *testing.T) {
	resetOwnedKinds()
	t.Cleanup(resetOwnedKinds)
	reg := testOwnedRegistry()
	RegisterOwnedKindIn(reg, OwnedKind{Kind: "test.webhook", Label: "Webhook", Perm: "test.deploy"})
	RegisterOwnedKindIn(reg, OwnedKind{Kind: "test.sync", Label: "Git sync", Perm: "test.deploy"})

	k, ok := LookupOwnedKind("test.sync")
	if !ok || k.Label != "Git sync" || k.Perm != "test.deploy" {
		t.Fatalf("LookupOwnedKind = %+v, %v", k, ok)
	}
	if _, ok := LookupOwnedKind("test.nope"); ok {
		t.Error("an unregistered kind was found")
	}
	var names []string
	for _, k := range OwnedKinds() {
		names = append(names, k.Kind)
	}
	if !slices.Equal(names, []string{"test.sync", "test.webhook"}) {
		t.Errorf("OwnedKinds = %v, want sorted by kind", names)
	}
}

func TestRegisteringAnOwnedKindPanicsOnAnEmptyDuplicateOrUnusablePermission(t *testing.T) {
	resetOwnedKinds()
	t.Cleanup(resetOwnedKinds)
	reg := testOwnedRegistry()
	RegisterOwnedKindIn(reg, OwnedKind{Kind: "test.sync", Perm: "test.deploy"})

	mustPanic(t, "an empty kind", func() { RegisterOwnedKindIn(reg, OwnedKind{Perm: "test.deploy"}) })
	mustPanic(t, "a duplicate kind", func() { RegisterOwnedKindIn(reg, OwnedKind{Kind: "test.sync", Perm: "test.deploy"}) })
	mustPanic(t, "a global permission", func() { RegisterOwnedKindIn(reg, OwnedKind{Kind: "test.g", Perm: "test.global"}) })
	mustPanic(t, "an unregistered permission", func() { RegisterOwnedKindIn(reg, OwnedKind{Kind: "test.u", Perm: "test.none"}) })
	// The default registry has no test.deploy, and containers.read is the only
	// kind of permission it accepts: an environment-scoped one.
	mustPanic(t, "a permission missing from the default registry", func() { RegisterOwnedKind(OwnedKind{Kind: "test.d", Perm: "test.deploy"}) })
	mustPanic(t, "a global permission of the default registry", func() { RegisterOwnedKind(OwnedKind{Kind: "test.d", Perm: PermUsersManage}) })
	RegisterOwnedKind(OwnedKind{Kind: "test.ok", Label: "Reader", Perm: PermContainersRead})
	if _, ok := LookupOwnedKind("test.ok"); !ok {
		t.Error("an environment-scoped permission of the default registry was refused")
	}
}

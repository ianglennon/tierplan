// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package rbac holds the policy model: permissions, scopes and the
// Requirement every route must declare when it is registered (D-44).
package rbac

import "fmt"

// Perm is a "resource.action" permission name.
type Perm string

// Scope says whether a permission is checked globally or per environment.
type Scope int

const (
	// ScopeGlobal permissions apply to the whole installation.
	ScopeGlobal Scope = iota + 1
	// ScopeEnvironment permissions are checked against one environment.
	ScopeEnvironment
)

// Requirement is what a route needs from its caller. Exactly one of Public,
// SelfService or Perm must be set.
type Requirement struct {
	Perm        Perm
	Scope       Scope
	Public      bool
	SelfService bool
}

var (
	// Public marks a route reachable without authentication.
	Public = Requirement{Public: true}
	// SelfService marks a route any signed-in user may call about themselves.
	SelfService = Requirement{SelfService: true}
)

// MustBeValid panics at startup when a route declares no requirement, a
// contradictory one, a permission that is not in the registry, or a scope that
// differs from the registered one. A route that forgot to say who may call it must never run.
func (r Requirement) MustBeValid() {
	switch {
	case r.Public && r.Perm != "":
		panic("rbac: requirement is both Public and a permission (" + string(r.Perm) + ")")
	case r.Public && r.SelfService, r.SelfService && r.Perm != "":
		panic("rbac: requirement sets more than one of Public, SelfService and Perm")
	case !r.Public && !r.SelfService && r.Perm == "":
		panic("rbac: route registered without a requirement: set Public, SelfService or a Perm")
	case r.Perm != "" && r.Scope != ScopeGlobal && r.Scope != ScopeEnvironment:
		panic(fmt.Sprintf("rbac: permission %q has no scope", r.Perm))
	}
	if r.Perm == "" {
		return
	}
	reg, ok := Lookup(r.Perm)
	switch {
	case !ok:
		panic(fmt.Sprintf("rbac: permission %q is not in the registry", r.Perm))
	case reg.Scope != r.Scope:
		panic(fmt.Sprintf("rbac: permission %q is registered with scope %d, not %d", r.Perm, reg.Scope, r.Scope))
	}
}

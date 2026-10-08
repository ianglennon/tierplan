// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rbac

import (
	"fmt"
	"slices"
	"strings"
	"sync"
)

// OwnedKind is a kind of automation that acts on a user's authority while that
// user is not present: a git sync, a webhook (D-14). The owner must hold Perm on
// the automation's environment every time it runs, and the automation pauses
// when the owner stops holding it.
type OwnedKind struct {
	// Kind is the stable key stored on owned_resources rows.
	Kind string
	// Label names the kind to people.
	Label string
	// Perm is the environment-scoped permission the owner must hold to run it.
	Perm Perm
}

var (
	ownedMu    sync.Mutex
	ownedKinds = map[string]OwnedKind{}
)

// RegisterOwnedKind adds k to the registry. It panics on an empty or repeated
// kind and on a permission that is not an environment-scoped permission of the
// default registry, so a mistake fails at startup. Phase 3 registers no
// production kind; the features that own automations register theirs.
func RegisterOwnedKind(k OwnedKind) { RegisterOwnedKindIn(defaultRegistry, k) }

// RegisterOwnedKindIn is RegisterOwnedKind judged against reg, so a test can
// register a kind over a permission of its own.
func RegisterOwnedKindIn(reg Registry, k OwnedKind) {
	if strings.TrimSpace(k.Kind) == "" {
		panic("rbac: owned kind registered with an empty name")
	}
	p, ok := reg.Lookup(k.Perm)
	switch {
	case !ok:
		panic(fmt.Sprintf("rbac: owned kind %q needs permission %q, which is not registered", k.Kind, k.Perm))
	case p.Scope != ScopeEnvironment:
		panic(fmt.Sprintf("rbac: owned kind %q needs permission %q, which is not environment-scoped", k.Kind, k.Perm))
	}
	ownedMu.Lock()
	defer ownedMu.Unlock()
	if _, dup := ownedKinds[k.Kind]; dup {
		panic(fmt.Sprintf("rbac: owned kind %q registered twice", k.Kind))
	}
	ownedKinds[k.Kind] = k
}

// LookupOwnedKind finds a registered kind.
func LookupOwnedKind(kind string) (OwnedKind, bool) {
	ownedMu.Lock()
	defer ownedMu.Unlock()
	k, ok := ownedKinds[kind]
	return k, ok
}

// OwnedKinds lists every registered kind, sorted by kind.
func OwnedKinds() []OwnedKind {
	ownedMu.Lock()
	defer ownedMu.Unlock()
	out := make([]OwnedKind, 0, len(ownedKinds))
	for _, k := range ownedKinds {
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b OwnedKind) int { return strings.Compare(a.Kind, b.Kind) })
	return out
}

// resetOwnedKinds empties the registry. Tests in this package use it.
func resetOwnedKinds() {
	ownedMu.Lock()
	defer ownedMu.Unlock()
	ownedKinds = map[string]OwnedKind{}
}

// OwnerQualifies reports whether an owner whose access is a may run an
// automation of this kind on envID: the kind is registered, the owner holds the
// kind's permission there, and holds env Admin when the automation needs it
// (needsEnvAdmin, set when its last run used host-escalating compose keys, D-13).
// An unregistered kind, or a permission the Checker's registry no longer has,
// qualifies nobody, so the automation pauses rather than runs.
func (c *Checker) OwnerQualifies(a Access, kind, envID string, needsEnvAdmin bool) bool {
	k, ok := LookupOwnedKind(kind)
	if !ok {
		return false
	}
	reg, ok := c.reg.Lookup(k.Perm)
	if !ok || reg.Scope != ScopeEnvironment {
		return false
	}
	ea := a.For(envID)
	if needsEnvAdmin && ea.Role < RoleAdmin {
		return false
	}
	return ea.Allows(reg)
}

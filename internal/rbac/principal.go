// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rbac

import "github.com/ianglennon/bayplan/internal/audit"

// PrincipalType says what kind of caller a Principal is.
type PrincipalType string

// Principal types.
const (
	PrincipalUser   PrincipalType = "user"
	PrincipalSystem PrincipalType = "system"
)

// Principal is an authenticated caller.
type Principal struct {
	Type PrincipalType
	// ID is the user ID, or the system principal name.
	ID, Name string
	// SessionIDHash identifies the session a user principal authenticated
	// with (plan 12); it is empty for system principals.
	SessionIDHash []byte
}

// AuditActor is how the principal appears in the audit log: a user under its
// ID and name, a system principal under its own name (D-51).
func (p *Principal) AuditActor() audit.Actor {
	t := audit.ActorUser
	if p.Type == PrincipalSystem {
		t = audit.ActorSystem
	}
	return audit.Actor{Type: t, ID: p.ID, Name: p.Name}
}

// SystemPrincipals maps each system principal to its explicit permission set
// (D-51). No system actor runs in Phase 1, so every set is empty and every
// check by one is denied. A later phase grants a set here, in code, where a
// reviewer sees it; system principals never consult role assignments.
var SystemPrincipals = map[string][]Perm{
	"system:cli":     {},
	"system:janitor": {},
	"system:gitsync": {},
	"system:webhook": {},
}

// System returns the named system principal, or nil for a name that is not in
// SystemPrincipals. A nil principal is anonymous, so an unknown name fails
// closed.
func System(name string) *Principal {
	if _, ok := SystemPrincipals[name]; !ok {
		return nil
	}
	return &Principal{Type: PrincipalSystem, ID: name, Name: name}
}

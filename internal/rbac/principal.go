// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package rbac

import "github.com/ianglennon/tierplan/internal/audit"

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
	// OnBehalfOf is the user ID of the owner a system principal acts for (D-14).
	// The Checker then judges every request as that user, as of the moment of the
	// request. It is empty for users and for system principals that act for no
	// one.
	OnBehalfOf string
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
// (D-51). Every set is empty, so every check by a system principal is denied. A later phase grants a set here, in code, where a
// reviewer sees it; system principals never consult role assignments.
var SystemPrincipals = map[string][]Perm{
	"system:cli":     {},
	"system:janitor": {},
	"system:gitsync": {},
	"system:webhook": {},
	// system:agent-enrol records an agent that enrolled with a join token. It
	// holds no permission: it only appears as the actor of audit entries.
	"system:agent-enrol": {},
	// system:agent-update, -reaper and -hub are the actors of the agent
	// auto-update, the revoke-on-leave sweep and the tunnel hub. Like
	// system:agent-enrol they hold no permission and only appear in the audit
	// log (D-13).
	"system:agent-update": {},
	"system:agent-reaper": {},
	"system:agent-hub":    {},
	// system:restore is the actor of a restore and of the audit.reanchored entry
	// it appends; system:backup is the actor of a scheduled backup. Neither holds
	// a permission (D-28, D-29).
	"system:restore": {},
	"system:backup":  {},
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

// OnBehalf returns the named system principal acting for the owner with this
// user ID, or nil for a system name that is not in SystemPrincipals. A nil
// principal is anonymous, so an unknown name fails closed.
func OnBehalf(systemID, ownerID string) *Principal {
	p := System(systemID)
	if p == nil {
		return nil
	}
	p.OnBehalfOf = ownerID
	return p
}

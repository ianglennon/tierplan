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
	Type          PrincipalType
	ID, Name      string
	SessionIDHash []byte
}

// AuditActor is how the principal appears in the audit log.
func (p *Principal) AuditActor() audit.Actor { return audit.Actor{} }

// SystemPrincipals maps each system principal name to its explicit permissions.
var SystemPrincipals = map[string][]Perm{}

// System returns the named system principal, or nil when there is none.
func System(name string) *Principal { return nil }

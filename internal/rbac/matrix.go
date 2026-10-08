// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rbac

import (
	"slices"
	"strings"
)

// NeedKind says what kind of grant a permission asks for.
type NeedKind string

// What a permission needs from the caller.
const (
	// NeedRole is a permission an environment role holds: Need.Role and above.
	NeedRole NeedKind = "role"
	// NeedExtra is a permission outside the role matrix: an Admin holds it
	// always, an Operator only when the assignment carries it (D-15, D-16).
	NeedExtra NeedKind = "extra"
	// NeedInstance is a global permission, an instance admin's alone (D-01).
	NeedInstance NeedKind = "instance"
)

// Need is what one permission asks for. Role is the lowest environment role
// that can hold it, and RoleNone for NeedInstance.
type Need struct {
	Kind NeedKind
	Role Role
}

// RequirementOf says what a caller needs to hold p. The SPA uses it to explain
// why a control is disabled (D-19), and the matrix documents it (D-15b).
func RequirementOf(p Permission) Need {
	switch {
	case p.Scope == ScopeGlobal:
		return Need{Kind: NeedInstance}
	case p.Extra:
		return Need{Kind: NeedExtra, Role: p.MinRole}
	default:
		return Need{Kind: NeedRole, Role: p.MinRole}
	}
}

// MatrixRow is one permission as the role matrix lists it. Viewer, Operator and
// Admin say whether that role holds the permission by role alone. Extra marks a
// permission an Operator holds only when the assignment carries it. An Admin
// holds every environment permission, and an instance admin holds all of them.
type MatrixRow struct {
	Name        string
	Scope       string
	Viewer      bool
	Operator    bool
	Admin       bool
	Extra       bool
	Dangerous   bool
	Description string
	Need        Need
}

// MatrixRows lists every permission of r in name order, so the generated
// documents are byte-identical between runs.
func MatrixRows(r Registry) []MatrixRow {
	rows := make([]MatrixRow, 0, len(r))
	for _, p := range r {
		env := p.Scope == ScopeEnvironment
		row := MatrixRow{
			Name:        string(p.Name),
			Scope:       "global",
			Extra:       env && p.Extra,
			Dangerous:   p.Dangerous,
			Description: p.Description,
			Need:        RequirementOf(p),
		}
		if env {
			row.Scope = "environment"
			row.Admin = true
			row.Viewer = !p.Extra && p.MinRole <= RoleViewer
			row.Operator = !p.Extra && p.MinRole <= RoleOperator
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b MatrixRow) int { return strings.Compare(a.Name, b.Name) })
	return rows
}

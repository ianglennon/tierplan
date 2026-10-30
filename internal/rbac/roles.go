// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package rbac

// Role is one of the three fixed environment roles (D-04). The values are
// ordered, so a higher role holds everything a lower one does: Admin is above
// Operator, which is above Viewer. RoleNone is the zero value and means "no
// access". Instance authority is not a role; it is the users.instance_admin
// flag (D-01).
type Role int

// The environment roles, lowest first.
const (
	RoleNone Role = iota
	RoleViewer
	RoleOperator
	RoleAdmin
)

// ParseRole turns a role ID ("viewer", "operator" or "admin") into a Role. It
// reports false for anything else, including the empty string.
func ParseRole(s string) (Role, bool) {
	switch s {
	case "viewer":
		return RoleViewer, true
	case "operator":
		return RoleOperator, true
	case "admin":
		return RoleAdmin, true
	}
	return RoleNone, false
}

// String names the role the way the API and the UI do. RoleNone is "none".
func (r Role) String() string {
	if id := r.ID(); id != "" {
		return id
	}
	return "none"
}

// ID is the role's primary key in the roles table, or the empty string for
// RoleNone (which has no row).
func (r Role) ID() string {
	switch r {
	case RoleViewer:
		return "viewer"
	case RoleOperator:
		return "operator"
	case RoleAdmin:
		return "admin"
	}
	return ""
}

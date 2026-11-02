// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package rbac

import (
	"slices"
	"strings"
)

// SourceKind says where a grant came from (D-06).
type SourceKind string

// Where a grant can come from.
const (
	// SourceInstanceAdmin is the instance-admin flag: Admin on every environment.
	SourceInstanceAdmin SourceKind = "instance"
	// SourceAllEnvironments is an assignment made to the user that applies to
	// every environment (D-05).
	SourceAllEnvironments SourceKind = "all"
	// SourceTeam is an assignment made to a team the user belongs to.
	SourceTeam SourceKind = "team"
	// SourceDirect is an assignment made to the user on one environment.
	SourceDirect SourceKind = "direct"
)

// rank orders the kinds as the user page lists them.
func (k SourceKind) rank() int {
	switch k {
	case SourceInstanceAdmin:
		return 0
	case SourceAllEnvironments:
		return 1
	case SourceTeam:
		return 2
	}
	return 3
}

// Source is one grant that contributes to a user's access. TeamID and TeamName
// are set for SourceTeam only.
type Source struct {
	Kind     SourceKind
	TeamID   string
	TeamName string
}

func compareSources(a, b Source) int {
	if c := a.Kind.rank() - b.Kind.rank(); c != 0 {
		return c
	}
	if c := strings.Compare(a.TeamName, b.TeamName); c != 0 {
		return c
	}
	return strings.Compare(a.TeamID, b.TeamID)
}

// EnvAccess is a user's effective access to one environment: the highest role
// any grant gives, the union of the dangerous extras, and every source that
// contributed (D-06). The zero value is no access.
type EnvAccess struct {
	Role    Role
	Extras  []Perm
	Sources []Source
}

// Allows reports whether the access holds an environment-scoped permission: the
// role is at least the permission's MinRole, and a permission that is an Extra
// is held by an Admin always, by an Operator only when an assignment carries it,
// and by a Viewer never (D-15, D-16, D-17).
func (a EnvAccess) Allows(reg Permission) bool {
	if reg.Scope != ScopeEnvironment || a.Role == RoleNone || a.Role < reg.MinRole {
		return false
	}
	if !reg.Extra || a.Role == RoleAdmin {
		return true
	}
	return a.Role >= RoleOperator && slices.Contains(a.Extras, reg.Name)
}

// merge folds b into a: the higher role, the union of extras and of sources.
func (a EnvAccess) merge(b EnvAccess) EnvAccess {
	out := EnvAccess{Role: max(a.Role, b.Role)}
	out.Extras = append(append([]Perm(nil), a.Extras...), b.Extras...)
	slices.Sort(out.Extras)
	out.Extras = slices.Compact(out.Extras)
	out.Sources = append(append([]Source(nil), a.Sources...), b.Sources...)
	slices.SortStableFunc(out.Sources, compareSources)
	out.Sources = slices.Compact(out.Sources)
	if len(out.Extras) == 0 {
		out.Extras = nil
	}
	if len(out.Sources) == 0 {
		out.Sources = nil
	}
	return out
}

// Merge folds b into a by the rules the evaluator itself uses: the higher role,
// the union of extras and of sources. Callers that explain access (the
// environment Access tab) fold with it so they cannot disagree with enforcement.
func (a EnvAccess) Merge(b EnvAccess) EnvAccess { return a.merge(b) }

// Access is everything one user may do: whether the account is usable at all,
// whether it is an instance admin, and its access to each environment. Envs is
// keyed by environment ID, with AllEnvironments for grants that apply to every
// environment.
type Access struct {
	// Active is false for a disabled or deleted user, who is allowed nothing
	// whatever the rest of the struct says (D-22c).
	Active bool
	// InstanceAdmin is the users.instance_admin flag. It only counts while Active.
	InstanceAdmin bool
	Envs          map[string]EnvAccess

	// adminExtras is the extras an instance admin's synthesised access carries:
	// every Extra permission of the registry the Access was built with.
	adminExtras []Perm
}

// For is the effective access to one environment: the AllEnvironments grants
// merged with the environment's own. An instance admin has Admin with every
// extra and the one source SourceInstanceAdmin. An environment ID of "" matches
// the all-environments grants only.
func (a Access) For(envID string) EnvAccess {
	if !a.Active {
		return EnvAccess{}
	}
	if a.InstanceAdmin {
		return EnvAccess{
			Role:    RoleAdmin,
			Extras:  slices.Clone(a.adminExtras),
			Sources: []Source{{Kind: SourceInstanceAdmin}},
		}
	}
	out := a.Envs[AllEnvironments].merge(EnvAccess{})
	if envID != "" && envID != AllEnvironments {
		out = out.merge(a.Envs[envID])
	}
	return out
}

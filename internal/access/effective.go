// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package access

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/ianglennon/tierplan/internal/rbac"
)

// EnvRow is a user's effective access to one environment, the "why" view of the
// Access card (D-06): the highest role any source gives, the union of the
// dangerous extras and every source that contributed, in the order Instance
// admin, All environments, Team, Direct.
type EnvRow struct {
	EnvID, EnvName string
	Role           string
	// Extras are sorted. An Admin's list is the registry's every extra.
	Extras  []string
	Sources []rbac.Source
	// Inherited is true when the environment is reached only through grants on
	// every environment: the user has no grant of their own, nor through a team,
	// on this environment.
	Inherited bool
}

// EffectiveAccess is what one user may do on each environment and why. The
// answer comes from Checker.Access, the one function enforcement also uses, so
// the page can never disagree with what the server does.
//
// An instance admin holds every environment (including ones added later) and so
// has no rows: instanceAdmin is true and the page shows one summary line. For
// anyone else the rows are the existing environments the user can reach, sorted
// by environment name, with grants on every environment expanded to each of
// them. A disabled or deleted user holds nothing and has no rows.
func (s *Service) EffectiveAccess(ctx context.Context, userID string) (instanceAdmin bool, rows []EnvRow, err error) {
	acc, err := s.Checker.Access(ctx, &rbac.Principal{Type: rbac.PrincipalUser, ID: userID})
	if err != nil {
		return false, nil, fmt.Errorf("access: effective access: %w", err)
	}
	if acc.InstanceAdmin {
		return true, []EnvRow{}, nil
	}
	envs, err := s.Store.RQ.ListEnvironments(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("access: list environments: %w", err)
	}
	rows = []EnvRow{}
	for _, e := range envs {
		ea := acc.For(e.ID)
		if ea.Role == rbac.RoleNone {
			continue
		}
		_, own := acc.Envs[e.ID]
		extras := extrasOfAccess(ea.Extras)
		if ea.Role == rbac.RoleAdmin {
			extras = adminExtras() // an Admin holds every extra without listing any
		}
		rows = append(rows, EnvRow{
			EnvID: e.ID, EnvName: e.Name, Role: ea.Role.ID(), Extras: extras,
			Sources: ea.Sources, Inherited: !own,
		})
	}
	slices.SortFunc(rows, func(a, b EnvRow) int {
		if c := strings.Compare(strings.ToLower(a.EnvName), strings.ToLower(b.EnvName)); c != 0 {
			return c
		}
		return strings.Compare(a.EnvID, b.EnvID)
	})
	return false, rows, nil
}

// extrasOfAccess turns the permissions of an EnvAccess into the strings the API
// carries, never nil.
func extrasOfAccess(in []rbac.Perm) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		out = append(out, string(p))
	}
	return out
}

// adminExtras lists every extra permission of the registry, sorted: what an
// Admin holds on an environment (D-16).
func adminExtras() []string {
	out := []string{}
	for _, p := range rbac.DefaultRegistry() {
		if p.Scope == rbac.ScopeEnvironment && p.Extra {
			out = append(out, string(p.Name))
		}
	}
	slices.Sort(out)
	return out
}

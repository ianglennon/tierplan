// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rbac

import (
	"context"
	"database/sql"

	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// Decision is the outcome of Checker.Check.
type Decision struct {
	Allowed bool
	Reason  string
	Err     error
}

// Checker evaluates requirements against role assignments.
type Checker struct{ s *store.Store }

// NewChecker returns a Checker over s.
func NewChecker(s *store.Store) *Checker { return &Checker{s: s} }

// Check decides whether p satisfies req for envID.
func (c *Checker) Check(ctx context.Context, p *Principal, req Requirement, envID string) Decision {
	return Decision{}
}

// EffectivePermissions lists what p may do, per environment where scoped.
func (c *Checker) EffectivePermissions(ctx context.Context, p *Principal) (global []Perm, perEnv map[string][]Perm, err error) {
	return nil, nil, nil
}

// SeedAdminRole makes the admin role hold exactly the registered permissions.
func SeedAdminRole(ctx context.Context, s *store.Store) error { return nil }

// AssignGlobalRole assigns roleID to a principal for every environment.
func AssignGlobalRole(ctx context.Context, tx *sql.Tx, q *sqlc.Queries, principalType PrincipalType, principalID, roleID string) error {
	return nil
}

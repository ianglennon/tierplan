// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rbac

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// AdminRoleID is the seeded role that holds every registered permission (D-45).
const AdminRoleID = "admin"

// AllEnvironments is the key EffectivePermissions uses for assignments that
// apply to every environment (D-50).
const AllEnvironments = "*"

// Decision reasons.
const (
	ReasonUnauthenticated = "unauthenticated"
	ReasonForbidden       = "forbidden"
)

// Decision is the outcome of Checker.Check.
type Decision struct {
	Allowed bool
	// Reason is ReasonUnauthenticated or ReasonForbidden when Allowed is false.
	Reason string
	// Err is set when the decision could not be made (a database error). The
	// decision is then a denial, and the caller answers 500.
	Err error
}

// Checker evaluates requirements against role assignments. It is default-deny:
// only a permission that a role grants the principal is allowed.
type Checker struct{ s *store.Store }

// NewChecker returns a Checker over s.
func NewChecker(s *store.Store) *Checker { return &Checker{s: s} }

func deny(reason string) Decision { return Decision{Reason: reason} }

// Check decides whether p satisfies req for envID. envID is only consulted for
// environment-scoped permissions.
func (c *Checker) Check(ctx context.Context, p *Principal, req Requirement, envID string) Decision {
	if req.Public {
		return Decision{Allowed: true}
	}
	if p == nil {
		return deny(ReasonUnauthenticated)
	}
	if req.SelfService {
		return Decision{Allowed: true}
	}
	reg, ok := Lookup(req.Perm)
	if !ok || reg.Scope != req.Scope {
		return deny(ReasonForbidden)
	}
	switch p.Type {
	case PrincipalSystem:
		if slices.Contains(SystemPrincipals[p.ID], req.Perm) {
			return Decision{Allowed: true}
		}
		return deny(ReasonForbidden)
	case PrincipalUser:
		if reg.Scope == ScopeGlobal {
			envID = "" // a global check never matches an environment assignment
		}
		allowed, err := c.s.RQ.PrincipalHasPermission(ctx, sqlc.PrincipalHasPermissionParams{
			PrincipalType: string(p.Type),
			PrincipalID:   p.ID,
			Permission:    string(req.Perm),
			EnvID:         envID,
		})
		if err != nil {
			return Decision{Reason: ReasonForbidden, Err: fmt.Errorf("rbac: check %s: %w", req.Perm, err)}
		}
		if allowed {
			return Decision{Allowed: true}
		}
		return deny(ReasonForbidden)
	default:
		return deny(ReasonForbidden)
	}
}

// EffectivePermissions lists what p may do (D-50). global holds the granted
// global-scope permissions. perEnv maps an environment ID to the granted
// environment-scope permissions, with AllEnvironments ("*") for assignments
// that apply to every environment. A permission granted to a role at a scope
// the registry does not give it is ignored, matching Check.
func (c *Checker) EffectivePermissions(ctx context.Context, p *Principal) (global []Perm, perEnv map[string][]Perm, err error) {
	perEnv = map[string][]Perm{}
	if p == nil {
		return nil, perEnv, nil
	}
	var granted []sqlc.ListPrincipalPermissionsRow
	switch p.Type {
	case PrincipalSystem:
		for _, perm := range SystemPrincipals[p.ID] {
			granted = append(granted, sqlc.ListPrincipalPermissionsRow{Permission: string(perm)})
		}
	case PrincipalUser:
		granted, err = c.s.RQ.ListPrincipalPermissions(ctx, sqlc.ListPrincipalPermissionsParams{
			PrincipalType: string(p.Type), PrincipalID: p.ID,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("rbac: effective permissions: %w", err)
		}
	}
	for _, g := range granted {
		reg, ok := Lookup(Perm(g.Permission))
		if !ok {
			continue
		}
		switch reg.Scope {
		case ScopeGlobal:
			if !g.EnvID.Valid && !slices.Contains(global, reg.Name) {
				global = append(global, reg.Name)
			}
		default:
			key := AllEnvironments
			if g.EnvID.Valid {
				key = g.EnvID.String
			}
			if !slices.Contains(perEnv[key], reg.Name) {
				perEnv[key] = append(perEnv[key], reg.Name)
			}
		}
	}
	slices.Sort(global)
	for _, list := range perEnv {
		slices.Sort(list)
	}
	return global, perEnv, nil
}

// SeedAdminRole makes the admin role hold exactly the registered permissions:
// it adds any that are missing and removes any that are no longer registered.
// It runs at every startup, in one transaction, so a new permission reaches
// admins on upgrade and a retired one stops being granted (D-45).
func SeedAdminRole(ctx context.Context, s *store.Store) error {
	tx, err := s.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rbac: seed admin role: %w", err)
	}
	if err := seedAdminRole(ctx, s.Q.WithTx(tx)); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rbac: seed admin role: commit: %w", err)
	}
	return nil
}

func seedAdminRole(ctx context.Context, q *sqlc.Queries) error {
	if err := q.UpsertRole(ctx, sqlc.UpsertRoleParams{ID: AdminRoleID, Name: "admin", Builtin: 1}); err != nil {
		return fmt.Errorf("rbac: seed admin role: %w", err)
	}
	have, err := q.ListRolePermissions(ctx, AdminRoleID)
	if err != nil {
		return fmt.Errorf("rbac: seed admin role: %w", err)
	}
	for _, name := range have {
		if _, ok := Lookup(Perm(name)); !ok {
			if err := q.DeleteRolePermission(ctx, sqlc.DeleteRolePermissionParams{RoleID: AdminRoleID, Permission: name}); err != nil {
				return fmt.Errorf("rbac: seed admin role: %w", err)
			}
		}
	}
	for _, perm := range registry {
		if err := q.InsertRolePermission(ctx, sqlc.InsertRolePermissionParams{RoleID: AdminRoleID, Permission: string(perm.Name)}); err != nil {
			return fmt.Errorf("rbac: seed admin role: %w", err)
		}
	}
	return nil
}

// AssignGlobalRole assigns roleID to a principal for every environment. tx
// must be a write transaction; setup completion calls it in the same
// transaction that creates the first admin (plan 10). Assigning twice is a
// no-op.
func AssignGlobalRole(ctx context.Context, tx *sql.Tx, q *sqlc.Queries, principalType PrincipalType, principalID, roleID string) error {
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("rbac: assign role: %w", err)
	}
	err = q.WithTx(tx).InsertRoleAssignment(ctx, sqlc.InsertRoleAssignmentParams{
		ID:            id.String(),
		PrincipalType: string(principalType),
		PrincipalID:   principalID,
		RoleID:        roleID,
		EnvID:         sql.NullString{},
		CreatedAt:     time.Now().UTC().UnixNano(),
	})
	if err != nil {
		return fmt.Errorf("rbac: assign role %s to %s: %w", roleID, principalID, err)
	}
	return nil
}

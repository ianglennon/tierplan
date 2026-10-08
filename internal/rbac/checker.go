// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rbac

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// AllEnvironments is the key Access and EffectivePermissions use for
// assignments that apply to every environment (D-50).
const AllEnvironments = "*"

// Decision reasons.
const (
	ReasonUnauthenticated = "unauthenticated"
	ReasonForbidden       = "forbidden"
	// ReasonOwnerInactive is an automation whose owner does not exist, is
	// disabled or is deleted (D-14).
	ReasonOwnerInactive = "owner_inactive"
	// ReasonOwnerLacksPermission is an automation whose owner is active but does
	// not hold the permission asked for (D-14).
	ReasonOwnerLacksPermission = "owner_lacks_permission"
)

// Decision is the outcome of Checker.Check.
type Decision struct {
	Allowed bool
	// Reason is one of the Reason constants when Allowed is false.
	Reason string
	// Err is set when the decision could not be made (a database error). The
	// decision is then a denial, and the caller answers 500.
	Err error
}

// Checker evaluates requirements against a user's access. It is default-deny:
// only a permission the user's access holds is allowed. Every decision, Check,
// EffectivePermissions and the environment lists, resolves through Access.
type Checker struct {
	s   *store.Store
	reg Registry
}

// CheckerOption configures a Checker.
type CheckerOption func(*Checker)

// WithRegistry makes the Checker decide against reg instead of the default
// registry. Tests use it to pin the matrix without editing the real one.
func WithRegistry(reg Registry) CheckerOption { return func(c *Checker) { c.reg = reg } }

// NewChecker returns a Checker over s.
func NewChecker(s *store.Store, opts ...CheckerOption) *Checker {
	c := &Checker{s: s, reg: DefaultRegistry()}
	for _, o := range opts {
		o(c)
	}
	return c
}

func deny(reason string) Decision { return Decision{Reason: reason} }

// userFlags is what decides whether a user may act at all.
type userFlags struct{ active, instanceAdmin bool }

// flags reads the user row's access flags. A user with no row (never created, or
// removed) is inactive, so it is allowed nothing.
func (c *Checker) flags(ctx context.Context, userID string) (userFlags, error) {
	return c.flagsQ(ctx, c.s.RQ, userID)
}

// flagsQ is flags over the given queries, which may be bound to a transaction.
func (c *Checker) flagsQ(ctx context.Context, q *sqlc.Queries, userID string) (userFlags, error) {
	f, err := q.GetUserAccessFlags(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return userFlags{}, nil
	}
	if err != nil {
		return userFlags{}, fmt.Errorf("rbac: read access flags: %w", err)
	}
	active := f.Disabled == 0 && !f.DeletedAt.Valid
	return userFlags{active: active, instanceAdmin: active && f.InstanceAdmin != 0}, nil
}

// adminExtras lists every Extra permission of the registry, sorted: what an
// instance admin's synthesised access carries.
func (c *Checker) adminExtras() []Perm {
	var extras []Perm
	for _, p := range c.reg {
		if p.Scope == ScopeEnvironment && p.Extra {
			extras = append(extras, p.Name)
		}
	}
	slices.Sort(extras)
	return extras
}

// assignments reads every assignment that reaches the user and folds them into
// per-environment access, with AllEnvironments for NULL env_id rows.
func (c *Checker) assignments(ctx context.Context, q *sqlc.Queries, userID string) (map[string]EnvAccess, error) {
	rows, err := q.ListUserAssignments(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("rbac: list assignments: %w", err)
	}
	envs := map[string]EnvAccess{}
	for _, row := range rows {
		role, ok := ParseRole(row.RoleID)
		if !ok {
			continue // a role this binary does not know grants nothing
		}
		var extras []Perm
		if err := json.Unmarshal([]byte(row.Extras), &extras); err != nil {
			return nil, fmt.Errorf("rbac: assignment %s extras: %w", row.ID, err)
		}
		key := AllEnvironments
		if row.EnvID.Valid {
			key = row.EnvID.String
		}
		src := Source{Kind: SourceDirect}
		switch {
		case row.PrincipalType == "team":
			src = Source{Kind: SourceTeam, TeamID: row.PrincipalID, TeamName: row.TeamName}
		case !row.EnvID.Valid:
			src = Source{Kind: SourceAllEnvironments}
		}
		envs[key] = envs[key].merge(EnvAccess{Role: role, Extras: extras, Sources: []Source{src}})
	}
	return envs, nil
}

// Access is everything the principal may do. A disabled or deleted user, a user
// that has no row, and any principal that is not a user are inactive and hold
// nothing. An instance admin is Admin on every environment without any
// assignment being read. A system principal acting on behalf of an owner holds
// exactly what the owner holds now (D-14), provided it is one of
// SystemPrincipals: a forged system name holds nothing.
func (c *Checker) Access(ctx context.Context, p *Principal) (Access, error) {
	switch {
	case p == nil:
		return Access{}, nil
	case p.Type == PrincipalUser:
		return c.AccessQ(ctx, c.s.RQ, p.ID)
	case p.Type == PrincipalSystem && p.OnBehalfOf != "":
		if _, listed := SystemPrincipals[p.ID]; listed {
			return c.AccessQ(ctx, c.s.RQ, p.OnBehalfOf)
		}
	}
	return Access{}, nil
}

// AccessQ is the Access of the user with this ID, read through q. q may be bound
// to a write transaction, so a recheck inside the transaction that just changed
// a user or a grant sees that change before it commits. An ID with no row is
// inactive.
func (c *Checker) AccessQ(ctx context.Context, q *sqlc.Queries, userID string) (Access, error) {
	f, err := c.flagsQ(ctx, q, userID)
	if err != nil {
		return Access{}, err
	}
	if !f.active {
		return Access{}, nil
	}
	if f.instanceAdmin {
		return Access{Active: true, InstanceAdmin: true, adminExtras: c.adminExtras()}, nil
	}
	envs, err := c.assignments(ctx, q, userID)
	if err != nil {
		return Access{}, err
	}
	return Access{Active: true, Envs: envs}, nil
}

// EnvAccess is the principal's effective access to one environment, by the same
// rules as Access. It is the hot path of every environment-scoped request.
func (c *Checker) EnvAccess(ctx context.Context, p *Principal, envID string) (EnvAccess, error) {
	a, err := c.Access(ctx, p)
	if err != nil {
		return EnvAccess{}, err
	}
	return a.For(envID), nil
}

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
	reg, ok := c.reg.Lookup(req.Perm)
	if !ok || reg.Scope != req.Scope {
		return deny(ReasonForbidden)
	}
	switch p.Type {
	case PrincipalSystem:
		if p.OnBehalfOf != "" {
			return c.checkOnBehalf(ctx, p, reg, envID)
		}
		if slices.Contains(SystemPrincipals[p.ID], req.Perm) {
			return Decision{Allowed: true}
		}
		return deny(ReasonForbidden)
	case PrincipalUser:
		if reg.Scope == ScopeGlobal {
			// A global permission is an instance admin's alone: no environment
			// assignment grants one (D-01, D-07), so the assignments are not read.
			f, err := c.flags(ctx, p.ID)
			if err != nil {
				return Decision{Reason: ReasonForbidden, Err: fmt.Errorf("rbac: check %s: %w", req.Perm, err)}
			}
			if f.instanceAdmin {
				return Decision{Allowed: true}
			}
			return deny(ReasonForbidden)
		}
		ea, err := c.EnvAccess(ctx, p, envID)
		if err != nil {
			return Decision{Reason: ReasonForbidden, Err: fmt.Errorf("rbac: check %s: %w", req.Perm, err)}
		}
		if ea.Allows(reg) {
			return Decision{Allowed: true}
		}
		return deny(ReasonForbidden)
	default:
		return deny(ReasonForbidden)
	}
}

// checkOnBehalf decides for a system principal acting for an owner (D-14). The
// actor must be a listed system principal, the owner must be active, and the
// requirement is then judged as the owner's own, as of now. The owner's rights
// are read on every call, never cached on the principal, so an automation never
// outlives them.
func (c *Checker) checkOnBehalf(ctx context.Context, p *Principal, reg Permission, envID string) Decision {
	if _, listed := SystemPrincipals[p.ID]; !listed {
		return deny(ReasonForbidden)
	}
	a, err := c.AccessQ(ctx, c.s.RQ, p.OnBehalfOf)
	if err != nil {
		return Decision{Reason: ReasonForbidden, Err: fmt.Errorf("rbac: check %s on behalf of %s: %w", reg.Name, p.OnBehalfOf, err)}
	}
	if !a.Active {
		return deny(ReasonOwnerInactive)
	}
	if reg.Scope == ScopeGlobal {
		if a.InstanceAdmin {
			return Decision{Allowed: true}
		}
	} else if a.For(envID).Allows(reg) {
		return Decision{Allowed: true}
	}
	return deny(ReasonOwnerLacksPermission)
}

// EffectivePermissions lists what p may do (D-50). global holds the allowed
// global-scope permissions. perEnv maps an environment ID to the allowed
// environment-scope permissions, with AllEnvironments ("*") for access that
// applies to every environment; an environment's list includes what it inherits
// from "*". An instance admin holds every global permission and every
// environment permission under "*". It resolves through Access, so it agrees
// with Check.
func (c *Checker) EffectivePermissions(ctx context.Context, p *Principal) (global []Perm, perEnv map[string][]Perm, err error) {
	perEnv = map[string][]Perm{}
	if p == nil {
		return nil, perEnv, nil
	}
	if p.Type == PrincipalSystem && p.OnBehalfOf == "" {
		for _, perm := range SystemPrincipals[p.ID] {
			reg, ok := c.reg.Lookup(perm)
			if !ok {
				continue
			}
			if reg.Scope == ScopeGlobal {
				global = append(global, reg.Name)
			} else {
				perEnv[AllEnvironments] = append(perEnv[AllEnvironments], reg.Name)
			}
		}
		return sortedPerms(global, perEnv), perEnv, nil
	}
	a, err := c.Access(ctx, p)
	if err != nil {
		return nil, nil, fmt.Errorf("rbac: effective permissions: %w", err)
	}
	if !a.Active {
		return nil, perEnv, nil
	}
	if a.InstanceAdmin {
		for _, reg := range c.reg {
			if reg.Scope == ScopeGlobal {
				global = append(global, reg.Name)
			}
		}
		perEnv[AllEnvironments] = c.envPerms(a.For(AllEnvironments))
		return sortedPerms(global, perEnv), perEnv, nil
	}
	for key := range a.Envs {
		if list := c.envPerms(a.For(key)); len(list) > 0 {
			perEnv[key] = list
		}
	}
	return nil, perEnv, nil
}

// envPerms lists the environment-scoped permissions ea allows, sorted.
func (c *Checker) envPerms(ea EnvAccess) []Perm {
	var out []Perm
	for _, reg := range c.reg {
		if ea.Allows(reg) {
			out = append(out, reg.Name)
		}
	}
	slices.Sort(out)
	return out
}

func sortedPerms(global []Perm, perEnv map[string][]Perm) []Perm {
	slices.Sort(global)
	for _, list := range perEnv {
		slices.Sort(list)
	}
	return global
}

// GrantInstanceAdminTx makes the user an instance admin (D-01). tx must be a
// write transaction; setup completion calls it in the same transaction that
// creates the first user. The user row must already exist: granting to an ID
// with no row is an error rather than a silent no-op. Granting twice is fine.
func GrantInstanceAdminTx(ctx context.Context, tx *sql.Tx, q *sqlc.Queries, userID string) error {
	n, err := q.WithTx(tx).SetUserInstanceAdmin(ctx, sqlc.SetUserInstanceAdminParams{
		InstanceAdmin: 1,
		UpdatedAt:     time.Now().UTC().UnixNano(),
		ID:            userID,
	})
	if err != nil {
		return fmt.Errorf("rbac: grant instance admin to %s: %w", userID, err)
	}
	if n == 0 {
		return fmt.Errorf("rbac: grant instance admin to %s: no such user", userID)
	}
	return nil
}

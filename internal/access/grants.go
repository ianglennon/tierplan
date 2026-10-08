// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// The two kinds of subject a grant is made to, and the two scopes it can have.
const (
	SubjectUser = "user"
	SubjectTeam = "team"
	// ScopeEnv is a grant on one environment; ScopeAll applies to every
	// environment, including ones added later (D-05).
	ScopeEnv = "env"
	ScopeAll = "all"
)

// Errors of the grant functions. They are stable so the API can map each to a
// status and a problem type.
var (
	// ErrViewerExtras is dangerous extras on a Viewer assignment (D-17). A Viewer
	// is read only, whoever grants it and through whichever route.
	ErrViewerExtras = errors.New("access: a Viewer cannot hold extra permissions")
	// ErrExtrasImplied is extras on an Admin assignment: an Admin holds every one
	// already, so listing them would only say it twice (D-16).
	ErrExtrasImplied = errors.New("access: an Admin holds every extra permission already")
	// ErrUnknownExtra is a permission that is not one of the registry's extras.
	ErrUnknownExtra = errors.New("access: that is not an extra permission")
	// ErrInvalidRole is a role other than viewer, operator or admin.
	ErrInvalidRole = errors.New("access: that is not a role")
	// ErrInvalidScope is a scope other than env or all, or an env scope with no
	// environment.
	ErrInvalidScope = errors.New("access: that is not a grant scope")
	// ErrSubjectNotFound is a user (or a user who is a tombstone) or a team that
	// does not exist. It is an ErrNotFound too.
	ErrSubjectNotFound = fmt.Errorf("%w: grant subject", ErrNotFound)
	// ErrEnvironmentNotFound is an environment that does not exist.
	ErrEnvironmentNotFound = errors.New("access: that environment does not exist")
	// ErrGrantNotFound is a grant that does not exist, or is not the subject's.
	ErrGrantNotFound = errors.New("access: that grant does not exist")
	// ErrDelegationDenied is a change an env Admin asked for through an
	// environment's Access tab that is beyond what they may delegate (D-12): a
	// grant on every environment or on another environment, a grant whose scope
	// is not the environment, or an instance admin's access. Nothing is written.
	ErrDelegationDenied = errors.New("access: that is beyond the access you can give")
)

// GrantScopeLimit narrows what a change to a grant may touch. The zero value is
// no limit: an instance admin through the user and team pages. DelegatedEnv is
// the environment an env Admin administers (D-12): when set, the change must be
// a grant on that environment alone, to a subject who is not an instance admin;
// anything else is ErrDelegationDenied and changes nothing.
type GrantScopeLimit struct {
	DelegatedEnv string
}

// check judges a grant row (and the subject it belongs to) against the limit.
func (l GrantScopeLimit) check(a sqlc.RoleAssignment, target grantTarget) error {
	if l.DelegatedEnv == "" {
		return nil
	}
	if !a.EnvID.Valid || a.EnvID.String != l.DelegatedEnv || target.instanceAdmin {
		return ErrDelegationDenied
	}
	return nil
}

// GrantState is the state of a grant a save was based on: the role and the
// extras the person editing it saw.
type GrantState struct {
	Role   string
	Extras []string
}

// GrantInput is a grant to save: the role and extras a user or a team holds on
// one environment (Scope env, with EnvID) or on all of them (Scope all). There is
// at most one grant per subject and scope, so saving a combination that exists
// edits it.
type GrantInput struct {
	// SubjectType is SubjectUser or SubjectTeam; SubjectID is the user or team ID.
	SubjectType, SubjectID string
	// Scope is ScopeEnv or ScopeAll. EnvID is the environment for ScopeEnv and is
	// ignored for ScopeAll.
	Scope, EnvID string
	// Role is viewer, operator or admin.
	Role string
	// Extras are the dangerous permissions the grant carries. Only an Operator
	// grant may carry any.
	Extras []string
	// Expected, when set, is the state the editor started from: if the grant is
	// now different (or gone) the save is ErrStaleChange and nothing changes.
	Expected *GrantState
	// DelegatedEnv, when set, is the environment the caller administers and may
	// give access on (D-12): the grant must be Scope env on exactly that
	// environment, to a subject who is not an instance admin, or the save is
	// ErrDelegationDenied. It is set from the route's environment, never from
	// the request body.
	DelegatedEnv string
}

// Grant is one assignment as the Access card shows it. EnvID is empty for a grant
// on every environment.
type Grant struct {
	ID, SubjectType, SubjectID string
	EnvID, EnvName             string
	Role                       string
	// Extras are sorted.
	Extras    []string
	CreatedAt time.Time
}

// GrantChange is a change to a grant that can reduce access and so is previewed
// before it is confirmed (D-27): Op is OpDeleteGrant, or OpUpdateGrant with the
// Role and Extras it would be saved with.
type GrantChange struct {
	Op     string
	Role   string
	Extras []string
}

// The kinds of GrantChange.
const (
	OpDeleteGrant = "delete"
	OpUpdateGrant = "update"
)

// ErrInvalidGrantOp is a GrantChange that is not one of the kinds above.
var ErrInvalidGrantOp = errors.New("access: that is not a grant change that can be previewed")

// cleanExtras checks a grant's extras against the registry and the role and
// returns them sorted and without repeats. Every extra must be a permission the
// registry marks Extra at environment scope; an Admin may carry none (it holds
// them all), and a Viewer may carry none (D-15, D-16, D-17).
func cleanExtras(role rbac.Role, in []string) ([]string, error) {
	reg := rbac.DefaultRegistry()
	out := make([]string, 0, len(in))
	for _, x := range in {
		p, ok := reg.Lookup(rbac.Perm(x))
		if !ok || p.Scope != rbac.ScopeEnvironment || !p.Extra {
			return nil, fmt.Errorf("%w: %q", ErrUnknownExtra, x)
		}
		out = append(out, x)
	}
	slices.Sort(out)
	out = slices.Compact(out)
	switch {
	case role == rbac.RoleViewer && len(out) > 0:
		return nil, ErrViewerExtras
	case role == rbac.RoleAdmin && len(out) > 0:
		return nil, ErrExtrasImplied
	}
	return out, nil
}

// grantTarget is a grant's subject as the audit entry and the recheck need it:
// who it is, the audit target and every user the grant reaches.
type grantTarget struct {
	typ, id, name string
	users         []string
	// instanceAdmin is true for a user who holds the instance-admin flag. Such a
	// user's access is not an environment grant's to give or take (D-12).
	instanceAdmin bool
}

// subjectTx reads the subject of a grant inside the caller's transaction and the
// users a grant to it reaches: the user, or the members of the team. A user who
// does not exist, a tombstone and an unknown team are ErrSubjectNotFound.
func subjectTx(ctx context.Context, q *sqlc.Queries, typ, id string) (grantTarget, error) {
	switch typ {
	case SubjectUser:
		u, err := liveUserTx(ctx, q, id)
		if errors.Is(err, ErrNotFound) {
			return grantTarget{}, ErrSubjectNotFound
		}
		if err != nil {
			return grantTarget{}, err
		}
		return grantTarget{typ: typ, id: id, name: u.Username, users: []string{id}, instanceAdmin: u.InstanceAdmin != 0}, nil
	case SubjectTeam:
		t, err := q.GetTeam(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return grantTarget{}, ErrSubjectNotFound
		}
		if err != nil {
			return grantTarget{}, fmt.Errorf("access: read team: %w", err)
		}
		members, err := q.ListTeamMemberIDs(ctx, id)
		if err != nil {
			return grantTarget{}, fmt.Errorf("access: list members: %w", err)
		}
		return grantTarget{typ: typ, id: id, name: t.Name, users: members}, nil
	}
	return grantTarget{}, ErrSubjectNotFound
}

// scopeKey is the value the unique index is on: the environment ID, or "" for a
// grant on every environment.
func scopeKey(scope, envID string) (string, error) {
	switch {
	case scope == ScopeAll:
		return "", nil
	case scope == ScopeEnv && envID != "":
		return envID, nil
	}
	return "", ErrInvalidScope
}

// scopeLabel is how an audit entry names a grant's scope: the environment ID, or
// "all".
func scopeLabel(envKey string) string {
	if envKey == "" {
		return ScopeAll
	}
	return envKey
}

// extrasOf reads an assignment's extras inside the caller's transaction.
func extrasOf(ctx context.Context, q *sqlc.Queries, assignmentID string) ([]string, error) {
	extras, err := q.ListAssignmentExtras(ctx, assignmentID)
	if err != nil {
		return nil, fmt.Errorf("access: list extras: %w", err)
	}
	if extras == nil {
		extras = []string{}
	}
	return extras, nil
}

// sameState says whether a role and extras are the state an editor saw.
func sameState(role string, extras []string, want GrantState) bool {
	a, b := slices.Clone(extras), slices.Clone(want.Extras)
	slices.Sort(a)
	slices.Sort(b)
	return role == want.Role && slices.Equal(a, b)
}

// applyGrantEdit is the change an edit makes, shared by the preview and the real
// thing: the role, then the extras replaced by the new set.
func applyGrantEdit(ctx context.Context, q *sqlc.Queries, id, role string, extras []string) error {
	if err := q.UpdateAssignmentRole(ctx, sqlc.UpdateAssignmentRoleParams{RoleID: role, ID: id}); err != nil {
		return fmt.Errorf("access: update grant: %w", err)
	}
	if err := q.DeleteAssignmentExtras(ctx, id); err != nil {
		return fmt.Errorf("access: clear extras: %w", err)
	}
	for _, x := range extras {
		if err := q.InsertAssignmentExtra(ctx, sqlc.InsertAssignmentExtraParams{AssignmentID: id, Permission: x}); err != nil {
			return fmt.Errorf("access: add extra: %w", err)
		}
	}
	return nil
}

// applyGrantDelete is the change a removal makes, shared by the preview and the
// real thing. A grant that is gone is ErrGrantNotFound.
func applyGrantDelete(ctx context.Context, q *sqlc.Queries, id string) error {
	n, err := q.DeleteAssignment(ctx, id)
	if err != nil {
		return fmt.Errorf("access: delete grant: %w", err)
	}
	if n == 0 {
		return ErrGrantNotFound
	}
	return nil
}

// PutGrant gives a user or a team a role on one environment or on all of them,
// with the dangerous extras of an Operator, in one transaction with its audit
// entry. A subject holds at most one grant per scope: a combination that exists
// is edited (access.updated), a new one is created (access.granted), and the
// bool says which. Saving what is already there changes nothing and writes no
// entry. Extras are accepted only on Operator grants, and only the registry's
// Extra permissions (ErrViewerExtras, ErrExtrasImplied, ErrUnknownExtra). When
// the input carries the Expected state the editor started from and the grant is
// now different or gone, the save is ErrStaleChange. A grant never expires
// (D-21).
//
// A change that lowers someone's role is recorded with who lost what (the
// impact, worked out in this transaction by the evaluator enforcement uses), and
// the automations of every user the grant reaches are rechecked in the same
// transaction, so a request after the commit is judged on the new grant. The
// caller has checked users.manage or teams.manage.
func (s *Service) PutGrant(ctx context.Context, actor audit.Actor, in GrantInput, opts ...Option) (Grant, bool, error) {
	o := optionsOf(opts)
	if in.DelegatedEnv != "" && (in.Scope != ScopeEnv || in.EnvID != in.DelegatedEnv) {
		return Grant{}, false, ErrDelegationDenied
	}
	role, ok := rbac.ParseRole(in.Role)
	if !ok {
		return Grant{}, false, ErrInvalidRole
	}
	extras, err := cleanExtras(role, in.Extras)
	if err != nil {
		return Grant{}, false, err
	}
	envKey, err := scopeKey(in.Scope, in.EnvID)
	if err != nil {
		return Grant{}, false, err
	}

	tx, err := s.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return Grant{}, false, fmt.Errorf("access: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.Store.Q.WithTx(tx)

	target, err := subjectTx(ctx, q, in.SubjectType, in.SubjectID)
	if err != nil {
		return Grant{}, false, err
	}
	if in.DelegatedEnv != "" && target.instanceAdmin {
		return Grant{}, false, ErrDelegationDenied
	}
	envName := ""
	if envKey != "" {
		e, err := q.GetEnvironment(ctx, envKey)
		if errors.Is(err, sql.ErrNoRows) {
			return Grant{}, false, ErrEnvironmentNotFound
		}
		if err != nil {
			return Grant{}, false, fmt.Errorf("access: read environment: %w", err)
		}
		envName = e.Name
	}

	existing, err := q.GetAssignmentForSubjectScope(ctx, sqlc.GetAssignmentForSubjectScopeParams{
		PrincipalType: in.SubjectType, PrincipalID: in.SubjectID, Scope: envKey,
	})
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Grant{}, false, fmt.Errorf("access: read grant: %w", err)
	}
	var oldExtras []string
	if found {
		if oldExtras, err = extrasOf(ctx, q, existing.ID); err != nil {
			return Grant{}, false, err
		}
	}
	if in.Expected != nil && (!found || !sameState(existing.RoleID, oldExtras, *in.Expected)) {
		return Grant{}, false, ErrStaleChange
	}
	if found && sameState(existing.RoleID, oldExtras, GrantState{Role: in.Role, Extras: extras}) {
		return grantOf(existing, envName, oldExtras), false, nil // nothing to change, nothing to record
	}

	details := map[string]string{
		"subject_type": in.SubjectType, "subject": target.name, "role": in.Role,
		"scope": scopeLabel(envKey), "extras": strings.Join(extras, ","),
	}
	action := audit.ActionAccessGranted
	var out Grant
	if found {
		action = audit.ActionAccessUpdated
		rows, err := s.impactTx(ctx, tx, q, target.users, func(_ *sql.Tx, q *sqlc.Queries) error {
			return applyGrantEdit(ctx, q, existing.ID, in.Role, extras)
		})
		if err != nil {
			return Grant{}, false, err
		}
		for k, v := range impactDetails(rows) {
			details[k] = v
		}
		existing.RoleID = in.Role
		out = grantOf(existing, envName, extras)
	} else {
		id, err := uuid.NewV7()
		if err != nil {
			return Grant{}, false, fmt.Errorf("access: generate grant id: %w", err)
		}
		created := s.now()
		var env sql.NullString
		if envKey != "" {
			env = sql.NullString{String: envKey, Valid: true}
		}
		if err := q.InsertAssignment(ctx, sqlc.InsertAssignmentParams{
			ID: id.String(), PrincipalType: in.SubjectType, PrincipalID: in.SubjectID,
			RoleID: in.Role, EnvID: env, CreatedAt: created.UnixNano(),
		}); err != nil {
			return Grant{}, false, fmt.Errorf("access: insert grant: %w", err)
		}
		for _, x := range extras {
			if err := q.InsertAssignmentExtra(ctx, sqlc.InsertAssignmentExtraParams{AssignmentID: id.String(), Permission: x}); err != nil {
				return Grant{}, false, fmt.Errorf("access: add extra: %w", err)
			}
		}
		out = Grant{
			ID: id.String(), SubjectType: in.SubjectType, SubjectID: in.SubjectID, EnvID: envKey, EnvName: envName,
			Role: in.Role, Extras: extras, CreatedAt: created.UTC(),
		}
	}
	if _, err := s.RecheckOwnedTx(ctx, tx, q, target.users, actor); err != nil {
		return Grant{}, false, err
	}
	if err := s.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: action, TargetType: in.SubjectType, TargetID: in.SubjectID, EnvID: envKey,
		Result: audit.ResultSuccess, IP: o.ip, UserAgent: o.userAgent, Details: details,
	}); err != nil {
		return Grant{}, false, fmt.Errorf("access: audit %s: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return Grant{}, false, fmt.Errorf("access: commit: %w", err)
	}
	return out, !found, nil
}

// grantOf builds a Grant from an assignment row.
func grantOf(a sqlc.RoleAssignment, envName string, extras []string) Grant {
	if extras == nil {
		extras = []string{}
	}
	return Grant{
		ID: a.ID, SubjectType: a.PrincipalType, SubjectID: a.PrincipalID, EnvID: a.EnvID.String, EnvName: envName,
		Role: a.RoleID, Extras: extras, CreatedAt: time.Unix(0, a.CreatedAt).UTC(),
	}
}

// ListGrants returns the grants made to one user or team: the one on every
// environment first, then those on single environments by name. An unknown
// subject is ErrSubjectNotFound.
func (s *Service) ListGrants(ctx context.Context, subjectType, subjectID string) ([]Grant, error) {
	if _, err := subjectTx(ctx, s.Store.RQ, subjectType, subjectID); err != nil {
		return nil, err
	}
	rows, err := s.Store.RQ.ListSubjectAssignments(ctx, sqlc.ListSubjectAssignmentsParams{
		PrincipalType: subjectType, PrincipalID: subjectID,
	})
	if err != nil {
		return nil, fmt.Errorf("access: list grants: %w", err)
	}
	out := make([]Grant, 0, len(rows))
	for _, r := range rows {
		var extras []string
		if err := json.Unmarshal([]byte(r.Extras), &extras); err != nil {
			return nil, fmt.Errorf("access: grant %s extras: %w", r.ID, err)
		}
		if extras == nil {
			extras = []string{}
		}
		out = append(out, Grant{
			ID: r.ID, SubjectType: r.PrincipalType, SubjectID: r.PrincipalID, EnvID: r.EnvID.String, EnvName: r.EnvName,
			Role: r.RoleID, Extras: extras, CreatedAt: time.Unix(0, r.CreatedAt).UTC(),
		})
	}
	return out, nil
}

// grantOfSubjectTx reads a grant and checks it belongs to the subject: a grant
// that is gone, or is another subject's, is ErrGrantNotFound.
func grantOfSubjectTx(ctx context.Context, q *sqlc.Queries, subjectType, subjectID, grantID string) (sqlc.RoleAssignment, error) {
	a, err := q.GetAssignment(ctx, grantID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (a.PrincipalType != subjectType || a.PrincipalID != subjectID)) {
		return sqlc.RoleAssignment{}, ErrGrantNotFound
	}
	if err != nil {
		return sqlc.RoleAssignment{}, fmt.Errorf("access: read grant: %w", err)
	}
	return a, nil
}

// GrantImpact previews a change to one of a subject's grants: who would lose which
// environment access if it were removed (OpDeleteGrant) or saved with a new role
// and extras (OpUpdateGrant). It applies the change in a write transaction with
// the same function the real change uses, reads the access of everyone it
// reaches through the real evaluator and rolls back, so nothing is written. A
// grant that is not the subject's is ErrGrantNotFound, and one beyond the limit
// (an env Admin's delegation, D-12) is ErrDelegationDenied.
func (s *Service) GrantImpact(ctx context.Context, subjectType, subjectID, grantID string, change GrantChange, limit GrantScopeLimit) ([]ImpactRow, error) {
	var role rbac.Role
	var extras []string
	switch change.Op {
	case OpDeleteGrant:
	case OpUpdateGrant:
		var ok bool
		if role, ok = rbac.ParseRole(change.Role); !ok {
			return nil, ErrInvalidRole
		}
		var err error
		if extras, err = cleanExtras(role, change.Extras); err != nil {
			return nil, err
		}
	default:
		return nil, ErrInvalidGrantOp
	}
	return s.impact(ctx, func(q *sqlc.Queries) ([]string, func(*sql.Tx, *sqlc.Queries) error, error) {
		target, err := subjectTx(ctx, q, subjectType, subjectID)
		if err != nil {
			return nil, nil, err
		}
		a, err := grantOfSubjectTx(ctx, q, subjectType, subjectID, grantID)
		if err != nil {
			return nil, nil, err
		}
		if err := limit.check(a, target); err != nil {
			return nil, nil, err
		}
		if change.Op == OpDeleteGrant {
			return target.users, func(_ *sql.Tx, q *sqlc.Queries) error { return applyGrantDelete(ctx, q, a.ID) }, nil
		}
		return target.users, func(_ *sql.Tx, q *sqlc.Queries) error { return applyGrantEdit(ctx, q, a.ID, role.ID(), extras) }, nil
	})
}

// DeleteGrant removes one of a subject's grants, with one audit entry
// (access.revoked) that carries the role and scope it had and who lost which
// access, computed inside the same transaction (so it is what this removal does,
// not what an earlier preview saw). The automations of every user the grant
// reached are rechecked in the same transaction. It returns the impact. A grant
// that is not the subject's is ErrGrantNotFound, one beyond the limit (an env
// Admin's delegation, D-12) is ErrDelegationDenied, and nothing is written.
func (s *Service) DeleteGrant(ctx context.Context, actor audit.Actor, subjectType, subjectID, grantID string, limit GrantScopeLimit, opts ...Option) ([]ImpactRow, error) {
	o := optionsOf(opts)
	tx, err := s.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("access: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.Store.Q.WithTx(tx)

	target, err := subjectTx(ctx, q, subjectType, subjectID)
	if err != nil {
		return nil, err
	}
	a, err := grantOfSubjectTx(ctx, q, subjectType, subjectID, grantID)
	if err != nil {
		return nil, err
	}
	if err := limit.check(a, target); err != nil {
		return nil, err
	}
	rows, err := s.impactTx(ctx, tx, q, target.users, func(_ *sql.Tx, q *sqlc.Queries) error {
		return applyGrantDelete(ctx, q, a.ID)
	})
	if err != nil {
		return nil, err
	}
	if _, err := s.RecheckOwnedTx(ctx, tx, q, target.users, actor); err != nil {
		return nil, err
	}
	details := impactDetails(rows)
	details["subject_type"], details["subject"] = subjectType, target.name
	details["role"], details["scope"] = a.RoleID, scopeLabel(a.EnvID.String)
	if err := s.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionAccessRevoked, TargetType: subjectType, TargetID: subjectID, EnvID: a.EnvID.String,
		Result: audit.ResultSuccess, IP: o.ip, UserAgent: o.userAgent, Details: details,
	}); err != nil {
		return nil, fmt.Errorf("access: audit access revoked: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("access: commit: %w", err)
	}
	return rows, nil
}

// GrantSubject names the subject of a grant, for the routes under an
// environment, whose path names the grant but not who holds it. A grant that is
// gone is ErrGrantNotFound, and one beyond the limit is ErrDelegationDenied
// (the environment routes never reveal a grant of another scope). The change
// that follows re-checks everything inside its own transaction.
func (s *Service) GrantSubject(ctx context.Context, grantID string, limit GrantScopeLimit) (subjectType, subjectID string, err error) {
	a, err := s.Store.RQ.GetAssignment(ctx, grantID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrGrantNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("access: read grant: %w", err)
	}
	target, err := subjectTx(ctx, s.Store.RQ, a.PrincipalType, a.PrincipalID)
	if err != nil {
		if errors.Is(err, ErrSubjectNotFound) {
			return "", "", ErrGrantNotFound
		}
		return "", "", err
	}
	if err := limit.check(a, target); err != nil {
		return "", "", err
	}
	return a.PrincipalType, a.PrincipalID, nil
}

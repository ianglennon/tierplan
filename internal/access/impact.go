// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// The size of an impact as an audit entry carries it (D-27, D-06): how many
// items are named, and how many bytes the list may take (the audit log's own
// limit on one detail value).
const (
	impactItemsInAudit = 10
	impactBytesInAudit = 512
)

// AllEnvironmentsLabel is how an impact row names the environment "*", the
// assignments that apply to every environment.
const AllEnvironmentsLabel = "All environments"

// ImpactRow is one place a change takes access away: user UserID held role
// Before on environment EnvID (rbac.AllEnvironments for a grant that applies to
// every environment) and would hold After once the change is applied. After is
// below Before, and RoleNone when the access ends.
type ImpactRow struct {
	UserID, Username string
	EnvID            string
	// EnvName is the environment's name, or AllEnvironmentsLabel for EnvID "*".
	EnvName       string
	Before, After rbac.Role
}

// TeamOp is a change to a team that can reduce access and so is previewed
// before it is confirmed (D-27).
type TeamOp struct {
	// Kind is OpDeleteTeam or OpRemoveMember.
	Kind string
	// UserID is the member removed, for OpRemoveMember.
	UserID string
}

// The kinds of TeamOp.
const (
	OpDeleteTeam   = "delete"
	OpRemoveMember = "remove_member"
)

// ErrInvalidTeamOp is a TeamOp that is not one of the kinds above, or a
// remove_member that names no user.
var ErrInvalidTeamOp = errors.New("access: that is not a team change that can be previewed")

// impactTx works out what a change takes away, inside the caller's write
// transaction (q is bound to tx). It reads the access of every affected user,
// runs apply, then reads it again through the same transaction, so the "after"
// is exactly what the change produces, judged by the same evaluator enforcement
// uses (Checker.AccessQ). The caller decides whether to commit what apply did.
func (s *Service) impactTx(ctx context.Context, tx *sql.Tx, q *sqlc.Queries, affected []string,
	apply func(tx *sql.Tx, q *sqlc.Queries) error) ([]ImpactRow, error) {
	type snapshot struct {
		username string
		acc      rbac.Access
	}
	before := make(map[string]snapshot, len(affected))
	for _, id := range affected {
		u, err := q.GetUserByID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("access: read user for impact: %w", err)
		}
		a, err := s.Checker.AccessQ(ctx, q, id)
		if err != nil {
			return nil, err
		}
		before[id] = snapshot{username: u.Username, acc: a}
	}
	if err := apply(tx, q); err != nil {
		return nil, err
	}
	names, err := envNames(ctx, q)
	if err != nil {
		return nil, err
	}
	var rows []ImpactRow
	for _, id := range affected {
		after, err := s.Checker.AccessQ(ctx, q, id)
		if err != nil {
			return nil, err
		}
		rows = append(rows, diffAccess(id, before[id].username, before[id].acc, after, names)...)
	}
	sortImpact(rows)
	return rows, nil
}

// impactPlan is what a preview works on: whom a change reaches and the change
// itself. It is built inside the preview's transaction, so the list of people
// and the state they are judged in are the same.
type impactPlan func(q *sqlc.Queries) (affected []string, apply func(tx *sql.Tx, q *sqlc.Queries) error, err error)

// impact runs a change in a write transaction, reports what it takes away, and
// rolls the change back: the database is exactly as it was afterwards.
func (s *Service) impact(ctx context.Context, plan impactPlan) ([]ImpactRow, error) {
	tx, err := s.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("access: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // the preview never commits
	q := s.Store.Q.WithTx(tx)
	affected, apply, err := plan(q)
	if err != nil {
		return nil, err
	}
	rows, err := s.impactTx(ctx, tx, q, affected, apply)
	if err != nil {
		return nil, err
	}
	if err := tx.Rollback(); err != nil {
		return nil, fmt.Errorf("access: roll back the preview: %w", err)
	}
	return rows, nil
}

// envNames maps an environment ID to its name.
func envNames(ctx context.Context, q *sqlc.Queries) (map[string]string, error) {
	envs, err := q.ListEnvironments(ctx)
	if err != nil {
		return nil, fmt.Errorf("access: list environments: %w", err)
	}
	out := make(map[string]string, len(envs)+1)
	for _, e := range envs {
		out[e.ID] = e.Name
	}
	out[rbac.AllEnvironments] = AllEnvironmentsLabel
	return out, nil
}

// diffAccess lists every environment where the role fell. The keys of Envs are
// the environments a user has a grant on, plus rbac.AllEnvironments; each one is
// judged by what the user would hold there (a grant on all environments counts
// toward every one of them). An instance admin or an inactive user has no Envs
// and so loses nothing.
func diffAccess(userID, username string, before, after rbac.Access, names map[string]string) []ImpactRow {
	keys := map[string]struct{}{}
	for k := range before.Envs {
		keys[k] = struct{}{}
	}
	for k := range after.Envs {
		keys[k] = struct{}{}
	}
	var rows []ImpactRow
	for k := range keys {
		var b, a rbac.Role
		if k == rbac.AllEnvironments {
			b, a = before.Envs[k].Role, after.Envs[k].Role
		} else {
			b, a = before.For(k).Role, after.For(k).Role
		}
		if a >= b {
			continue
		}
		name, ok := names[k]
		if !ok {
			name = k
		}
		rows = append(rows, ImpactRow{UserID: userID, Username: username, EnvID: k, EnvName: name, Before: b, After: a})
	}
	return rows
}

// sortImpact orders rows by username, then environment name, then ID, so a
// preview, the list in the audit entry and a test all read the same way.
func sortImpact(rows []ImpactRow) {
	slices.SortFunc(rows, func(a, b ImpactRow) int {
		if c := strings.Compare(a.Username, b.Username); c != 0 {
			return c
		}
		if c := strings.Compare(a.EnvName, b.EnvName); c != 0 {
			return c
		}
		return strings.Compare(a.EnvID, b.EnvID)
	})
}

// impactDetails is what an audit entry carries about an impact: how many
// changes there were and the first ten, "username: environment before→after",
// joined by "; " and cut to the audit log's 512 bytes (D-27).
func impactDetails(rows []ImpactRow) map[string]string {
	n := min(len(rows), impactItemsInAudit)
	items := make([]string, 0, n)
	for _, r := range rows[:n] {
		items = append(items, fmt.Sprintf("%s: %s %s→%s", r.Username, r.EnvName, r.Before, r.After))
	}
	return map[string]string{
		"impact_count": strconv.Itoa(len(rows)),
		"impact":       cutBytes(strings.Join(items, "; "), impactBytesInAudit),
	}
}

// cutBytes shortens s to at most max bytes without splitting a character.
func cutBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// TeamImpact previews a change to a team: who would lose which environment
// access if it were applied. It applies the change in a write transaction with
// the same function the real change uses, reads the access of everyone it
// reaches through the real evaluator, and rolls back, so nothing is written. The
// caller has checked teams.manage.
func (s *Service) TeamImpact(ctx context.Context, teamID string, op TeamOp) ([]ImpactRow, error) {
	if op.Kind != OpDeleteTeam && (op.Kind != OpRemoveMember || op.UserID == "") {
		return nil, ErrInvalidTeamOp
	}
	return s.impact(ctx, func(q *sqlc.Queries) ([]string, func(*sql.Tx, *sqlc.Queries) error, error) {
		if _, err := q.GetTeam(ctx, teamID); errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrTeamNotFound
		} else if err != nil {
			return nil, nil, fmt.Errorf("access: read team: %w", err)
		}
		if op.Kind == OpDeleteTeam {
			members, err := q.ListTeamMemberIDs(ctx, teamID)
			if err != nil {
				return nil, nil, fmt.Errorf("access: list members: %w", err)
			}
			return members, func(_ *sql.Tx, q *sqlc.Queries) error { return applyDeleteTeam(ctx, q, teamID) }, nil
		}
		members, err := q.ListTeamMemberIDs(ctx, teamID)
		if err != nil {
			return nil, nil, fmt.Errorf("access: list members: %w", err)
		}
		if !slices.Contains(members, op.UserID) {
			return nil, nil, ErrMemberNotFound
		}
		return []string{op.UserID}, func(_ *sql.Tx, q *sqlc.Queries) error { return applyRemoveMember(ctx, q, teamID, op.UserID) }, nil
	})
}

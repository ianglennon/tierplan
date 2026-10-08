// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

var (
	// ErrLastInstanceAdmin is a change that would leave Bayplan with no active
	// instance admin: demoting, disabling or deleting the last one (D-02). It is
	// counted after the change, inside the same write transaction, so concurrent
	// attempts cannot both succeed.
	ErrLastInstanceAdmin = errors.New("access: Bayplan needs at least one active instance admin")
	// ErrStaleChange is a profile save based on a version of the user that has
	// since changed (T-03-57).
	ErrStaleChange = errors.New("access: the user changed since it was read")
)

// UserDetail is a user as the user page shows them: the table row plus the
// times the page needs.
type UserDetail struct {
	UserSummary
	CreatedAt time.Time
	// UpdatedAt is the version a profile save is checked against.
	UpdatedAt time.Time
}

// GetUser returns one user. A user who does not exist, or who has been deleted,
// is ErrNotFound. The caller has checked users.manage.
func (s *Service) GetUser(ctx context.Context, id string) (UserDetail, error) {
	r, err := s.Store.RQ.GetUserDetail(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return UserDetail{}, ErrNotFound
	}
	if err != nil {
		return UserDetail{}, fmt.Errorf("access: read user: %w", err)
	}
	if r.DeletedAt.Valid {
		return UserDetail{}, ErrNotFound
	}
	now := s.now()
	u := sqlc.User{PasswordHash: r.PasswordHash, Disabled: r.Disabled, LockedUntil: r.LockedUntil}
	d := UserDetail{
		UserSummary: UserSummary{
			ID: r.ID, Username: r.Username, DisplayName: r.DisplayName, Email: r.Email,
			InstanceAdmin: r.InstanceAdmin != 0, TeamCount: int(r.TeamCount),
			Status: Status(u, now, r.LinkExpiresAt.Valid && r.LinkExpiresAt.Int64 > now.UnixNano()),
		},
		CreatedAt: time.Unix(0, r.CreatedAt).UTC(),
		UpdatedAt: time.Unix(0, r.UpdatedAt).UTC(),
	}
	if u.LockedUntil.Valid && u.LockedUntil.Int64 > now.UnixNano() {
		t := time.Unix(0, u.LockedUntil.Int64).UTC()
		d.LockedUntil = &t
	}
	if r.LastLoginAt.Valid {
		t := time.Unix(0, r.LastLoginAt.Int64).UTC()
		d.LastLoginAt = &t
	}
	return d, nil
}

// liveUserTx reads a user inside the caller's transaction. One who does not
// exist, or is a tombstone, is ErrNotFound.
func liveUserTx(ctx context.Context, q *sqlc.Queries, id string) (sqlc.User, error) {
	u, err := q.GetUserByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlc.User{}, ErrNotFound
	}
	if err != nil {
		return sqlc.User{}, fmt.Errorf("access: read user: %w", err)
	}
	if u.DeletedAt.Valid {
		return sqlc.User{}, ErrNotFound
	}
	return u, nil
}

// isActiveAdmin says whether a user is an instance admin who can sign in.
func isActiveAdmin(u sqlc.User) bool {
	return u.InstanceAdmin != 0 && u.Disabled == 0 && !u.DeletedAt.Valid
}

// guardLastAdminTx refuses a change that took the last active instance admin
// away. It runs after the change, in the same write transaction, and only for a
// user who was an active instance admin before it: the count is of what is left.
func guardLastAdminTx(ctx context.Context, q *sqlc.Queries, was sqlc.User) error {
	if !isActiveAdmin(was) {
		return nil
	}
	n, err := q.CountActiveInstanceAdmins(ctx)
	if err != nil {
		return fmt.Errorf("access: count instance admins: %w", err)
	}
	if n == 0 {
		return ErrLastInstanceAdmin
	}
	return nil
}

func optionsOf(opts []Option) options {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// DisableUser switches a user off: every session of theirs ends at once, their
// links stop working, sign-in is refused, and the automations they own pause.
// Their grants and team memberships are kept, so Enable restores everything
// (D-22c). Disabling a disabled user changes nothing and writes no entry. The
// last active instance admin cannot be disabled (ErrLastInstanceAdmin). actor is
// the admin; the caller has checked users.manage.
func (s *Service) DisableUser(ctx context.Context, actor audit.Actor, id string, opts ...Option) error {
	o := optionsOf(opts)
	tx, err := s.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("access: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.Store.Q.WithTx(tx)

	u, err := liveUserTx(ctx, q, id)
	if err != nil {
		return err
	}
	if u.Disabled != 0 {
		return nil
	}
	if err := q.SetUserDisabled(ctx, sqlc.SetUserDisabledParams{Disabled: 1, UpdatedAt: s.now().UnixNano(), ID: id}); err != nil {
		return fmt.Errorf("access: disable user: %w", err)
	}
	ended, err := q.DeleteAllUserSessions(ctx, id)
	if err != nil {
		return fmt.Errorf("access: end sessions: %w", err)
	}
	if err := q.DeleteUserLinks(ctx, id); err != nil {
		return fmt.Errorf("access: delete links: %w", err)
	}
	if err := guardLastAdminTx(ctx, q, u); err != nil {
		return err
	}
	paused, err := s.RecheckOwnedTx(ctx, tx, q, []string{id}, actor)
	if err != nil {
		return err
	}
	if err := s.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionUserDisabled, TargetType: "user", TargetID: id,
		Result: audit.ResultSuccess, IP: o.ip, UserAgent: o.userAgent,
		Details: map[string]string{"username": u.Username, "ended": strconv.FormatInt(ended, 10), "paused": strconv.Itoa(paused)},
	}); err != nil {
		return fmt.Errorf("access: audit user disabled: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("access: commit: %w", err)
	}
	return nil
}

// EnableUser switches a disabled user back on. They keep every grant and team
// they had, and the automations paused only because they were disabled resume
// if they still qualify. Enabling an enabled user changes nothing and writes no
// entry.
func (s *Service) EnableUser(ctx context.Context, actor audit.Actor, id string, opts ...Option) error {
	o := optionsOf(opts)
	tx, err := s.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("access: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.Store.Q.WithTx(tx)

	u, err := liveUserTx(ctx, q, id)
	if err != nil {
		return err
	}
	if u.Disabled == 0 {
		return nil
	}
	if err := q.SetUserDisabled(ctx, sqlc.SetUserDisabledParams{Disabled: 0, UpdatedAt: s.now().UnixNano(), ID: id}); err != nil {
		return fmt.Errorf("access: enable user: %w", err)
	}
	if _, err := s.RecheckOwnedTx(ctx, tx, q, []string{id}, actor); err != nil {
		return err
	}
	if err := s.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionUserEnabled, TargetType: "user", TargetID: id,
		Result: audit.ResultSuccess, IP: o.ip, UserAgent: o.userAgent,
		Details: map[string]string{"username": u.Username},
	}); err != nil {
		return fmt.Errorf("access: audit user enabled: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("access: commit: %w", err)
	}
	return nil
}

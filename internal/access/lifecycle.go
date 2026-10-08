// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	last := false
	if r.InstanceAdmin != 0 && r.Disabled == 0 {
		n, err := s.Store.RQ.CountActiveInstanceAdmins(ctx)
		if err != nil {
			return UserDetail{}, fmt.Errorf("access: count instance admins: %w", err)
		}
		last = n == 1
	}
	d := UserDetail{
		UserSummary: UserSummary{
			ID: r.ID, Username: r.Username, DisplayName: r.DisplayName, Email: r.Email,
			InstanceAdmin: r.InstanceAdmin != 0, TeamCount: int(r.TeamCount),
			Status:            Status(u, now, r.LinkExpiresAt.Valid && r.LinkExpiresAt.Int64 > now.UnixNano()),
			LastInstanceAdmin: last,
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

// nextVersion is the updated_at a change stamps: the clock, but always later
// than the version it replaces, so a save based on the old version can tell it
// is stale even when two changes land within the clock's resolution.
func nextVersion(old int64, now time.Time) int64 {
	n := now.UnixNano()
	if n <= old {
		n = old + 1
	}
	return n
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
	if err := q.SetUserDisabled(ctx, sqlc.SetUserDisabledParams{Disabled: 1, UpdatedAt: nextVersion(u.UpdatedAt, s.now()), ID: id}); err != nil {
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
	if err := q.SetUserDisabled(ctx, sqlc.SetUserDisabledParams{Disabled: 0, UpdatedAt: nextVersion(u.UpdatedAt, s.now()), ID: id}); err != nil {
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

// ProfileInput is a profile save. A nil field is left as it is. UpdatedAt is the
// version of the user the save was based on.
type ProfileInput struct {
	DisplayName, Email *string
	UpdatedAt          time.Time
}

// DeleteUser turns a user into a tombstone (D-22c): disabled, deleted_at set,
// password, email and display name cleared and instance admin off, with their
// sessions, links, direct assignments and team memberships removed, all in one
// transaction. The username stays reserved and the delete cannot be undone. The
// automations they own pause as "owner deleted". The last active instance admin
// cannot be deleted (ErrLastInstanceAdmin). A user who is already a tombstone is
// ErrNotFound.
func (s *Service) DeleteUser(ctx context.Context, actor audit.Actor, id string, opts ...Option) error {
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
	n, err := q.TombstoneUser(ctx, sqlc.TombstoneUserParams{
		DeletedAt: sql.NullInt64{Int64: s.now().UnixNano(), Valid: true},
		UpdatedAt: nextVersion(u.UpdatedAt, s.now()),
		ID:        id,
	})
	if err != nil {
		return fmt.Errorf("access: tombstone user: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	ended, err := q.DeleteAllUserSessions(ctx, id)
	if err != nil {
		return fmt.Errorf("access: end sessions: %w", err)
	}
	if err := q.DeleteUserLinks(ctx, id); err != nil {
		return fmt.Errorf("access: delete links: %w", err)
	}
	if _, err := q.DeleteUserAssignments(ctx, id); err != nil {
		return fmt.Errorf("access: delete assignments: %w", err)
	}
	if _, err := q.DeleteUserMemberships(ctx, id); err != nil {
		return fmt.Errorf("access: delete memberships: %w", err)
	}
	if err := guardLastAdminTx(ctx, q, u); err != nil {
		return err
	}
	paused, err := s.RecheckOwnedTx(ctx, tx, q, []string{id}, actor)
	if err != nil {
		return err
	}
	if err := s.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionUserDeleted, TargetType: "user", TargetID: id,
		Result: audit.ResultSuccess, IP: o.ip, UserAgent: o.userAgent,
		// reassigned is 0 until automations can be handed to another owner.
		Details: map[string]string{
			"username": u.Username, "ended": strconv.FormatInt(ended, 10),
			"paused": strconv.Itoa(paused), "reassigned": "0",
		},
	}); err != nil {
		return fmt.Errorf("access: audit user deleted: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("access: commit: %w", err)
	}
	return nil
}

// SetInstanceAdmin turns instance authority on or off for one user (D-02). It is
// a flag on the user and is never given through a team. Setting what is already
// set changes nothing and writes no entry. Turning it off for the last active
// instance admin is ErrLastInstanceAdmin. The automations the user owns are
// rechecked, because what they may do changed with it.
func (s *Service) SetInstanceAdmin(ctx context.Context, actor audit.Actor, id string, enabled bool, opts ...Option) error {
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
	if (u.InstanceAdmin != 0) == enabled {
		return nil
	}
	var flag int64
	action := audit.ActionUserAdminRevoked
	if enabled {
		flag, action = 1, audit.ActionUserAdminGranted
	}
	if _, err := q.SetUserInstanceAdmin(ctx, sqlc.SetUserInstanceAdminParams{
		InstanceAdmin: flag, UpdatedAt: nextVersion(u.UpdatedAt, s.now()), ID: id,
	}); err != nil {
		return fmt.Errorf("access: set instance admin: %w", err)
	}
	if !enabled {
		if err := guardLastAdminTx(ctx, q, u); err != nil {
			return err
		}
	}
	if _, err := s.RecheckOwnedTx(ctx, tx, q, []string{id}, actor); err != nil {
		return err
	}
	if err := s.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: action, TargetType: "user", TargetID: id,
		Result: audit.ResultSuccess, IP: o.ip, UserAgent: o.userAgent,
		Details: map[string]string{"username": u.Username},
	}); err != nil {
		return fmt.Errorf("access: audit %s: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("access: commit: %w", err)
	}
	return nil
}

// UnlockUser clears the lock and the failure count of a locked account, audited
// as auth.unlocked with the admin as actor. A user who is not locked is left
// alone and nothing is written.
func (s *Service) UnlockUser(ctx context.Context, actor audit.Actor, id string, opts ...Option) error {
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
	if !u.LockedUntil.Valid || u.LockedUntil.Int64 <= s.now().UnixNano() {
		return nil
	}
	if err := q.ResetLoginFailures(ctx, id); err != nil {
		return fmt.Errorf("access: unlock user: %w", err)
	}
	if err := s.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionAuthUnlocked, TargetType: "user", TargetID: id,
		Result: audit.ResultSuccess, IP: o.ip, UserAgent: o.userAgent,
		Details: map[string]string{"username": u.Username},
	}); err != nil {
		return fmt.Errorf("access: audit unlock: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("access: commit: %w", err)
	}
	return nil
}

// UpdateProfile saves a user's display name and email, which an admin may edit;
// the username never changes. The save must be based on the version the admin
// read (in.UpdatedAt), or it is ErrStaleChange and nothing is saved. Fields are
// trimmed and checked like those of CreateUser, and every problem is returned
// at once. A save that changes nothing writes no entry. The entry names the
// fields that changed.
func (s *Service) UpdateProfile(ctx context.Context, actor audit.Actor, id string, in ProfileInput, opts ...Option) error {
	o := optionsOf(opts)
	var invalid []error
	var displayName, email *string
	if in.DisplayName != nil {
		v := strings.TrimSpace(*in.DisplayName)
		if utf8.RuneCountInString(v) > maxDisplayNameChars {
			invalid = append(invalid, ErrDisplayNameTooLong)
		}
		displayName = &v
	}
	if in.Email != nil {
		v, err := cleanEmail(*in.Email)
		if err != nil {
			invalid = append(invalid, err)
		}
		email = &v
	}
	if len(invalid) > 0 {
		return errors.Join(invalid...)
	}

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
	if !time.Unix(0, u.UpdatedAt).UTC().Equal(in.UpdatedAt) {
		return ErrStaleChange
	}
	var changed []string
	newName, newEmail := u.DisplayName, u.Email
	if displayName != nil && *displayName != u.DisplayName {
		newName = *displayName
		changed = append(changed, "displayName")
	}
	if email != nil && *email != u.Email {
		newEmail = *email
		changed = append(changed, "email")
	}
	if len(changed) == 0 {
		return nil
	}
	n, err := q.UpdateUserProfile(ctx, sqlc.UpdateUserProfileParams{
		DisplayName: newName, Email: newEmail, UpdatedAt: nextVersion(u.UpdatedAt, s.now()), ID: id, UpdatedAt_2: u.UpdatedAt,
	})
	if err != nil {
		return fmt.Errorf("access: update profile: %w", err)
	}
	if n == 0 {
		return ErrStaleChange
	}
	if err := s.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionUserUpdated, TargetType: "user", TargetID: id,
		Result: audit.ResultSuccess, IP: o.ip, UserAgent: o.userAgent,
		Details: map[string]string{"username": u.Username, "fields": strings.Join(changed, ",")},
	}); err != nil {
		return fmt.Errorf("access: audit user updated: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("access: commit: %w", err)
	}
	return nil
}

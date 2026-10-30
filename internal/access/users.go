// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/auth"
	"github.com/ianglennon/coaming/internal/store/sqlc"
)

// Profile limits (D-24).
const (
	maxDisplayNameChars = 64
	maxEmailChars       = 254
)

// CreateUserInput is what an instance admin types to add someone.
type CreateUserInput struct {
	Username    string
	DisplayName string
	Email       string
	// IP and UserAgent are the admin's, recorded in the audit entries.
	IP, UserAgent string
}

// CreateUser adds a user with no password and a one-time invite link, in one
// transaction with its audit entries (D-22a). actor is the admin; the caller has
// already checked users.manage. The returned user has an empty password hash
// until the link is accepted.
func (s *Service) CreateUser(ctx context.Context, actor audit.Actor, in CreateUserInput) (sqlc.User, Link, error) {
	// Every field is checked, and the errors joined, so a form can show all the
	// problems at once.
	var invalid []error
	username, err := auth.NormalizeUsername(in.Username)
	if err != nil {
		invalid = append(invalid, ErrInvalidUsername)
	}
	displayName := strings.TrimSpace(in.DisplayName)
	if utf8.RuneCountInString(displayName) > maxDisplayNameChars {
		invalid = append(invalid, ErrDisplayNameTooLong)
	}
	email, err := cleanEmail(in.Email)
	if err != nil {
		invalid = append(invalid, err)
	}
	if len(invalid) > 0 {
		return sqlc.User{}, Link{}, errors.Join(invalid...)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return sqlc.User{}, Link{}, fmt.Errorf("access: generate user id: %w", err)
	}

	tx, err := s.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return sqlc.User{}, Link{}, fmt.Errorf("access: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.Store.Q.WithTx(tx)

	switch existing, err := q.GetUserByUsernameAny(ctx, username); {
	case err == nil && existing.DeletedAt.Valid:
		return sqlc.User{}, Link{}, ErrUsernameReserved
	case err == nil:
		return sqlc.User{}, Link{}, ErrUsernameTaken
	case !errors.Is(err, sql.ErrNoRows):
		return sqlc.User{}, Link{}, fmt.Errorf("access: look up username: %w", err)
	}

	now := s.now().UnixNano()
	if err := q.InsertInvitedUser(ctx, sqlc.InsertInvitedUserParams{
		ID: id.String(), Username: username, DisplayName: displayName, Email: email,
		CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now,
	}); err != nil {
		return sqlc.User{}, Link{}, fmt.Errorf("access: insert user: %w", err)
	}
	link, err := s.issueLinkTx(ctx, q, id.String(), LinkInvite, actor.ID)
	if err != nil {
		return sqlc.User{}, Link{}, err
	}
	if err := s.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionUserCreated, TargetType: "user", TargetID: id.String(),
		Result: audit.ResultSuccess, IP: in.IP, UserAgent: in.UserAgent,
		Details: map[string]string{"username": username, "link_kind": LinkInvite, "display_name": displayName},
	}); err != nil {
		return sqlc.User{}, Link{}, fmt.Errorf("access: audit user created: %w", err)
	}
	if err := s.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionUserInviteCreated, TargetType: "user", TargetID: id.String(),
		Result: audit.ResultSuccess, IP: in.IP, UserAgent: in.UserAgent,
		Details: map[string]string{"username": username, "expires_at": link.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00")},
	}); err != nil {
		return sqlc.User{}, Link{}, fmt.Errorf("access: audit invite created: %w", err)
	}
	u, err := q.GetUserByID(ctx, id.String())
	if err != nil {
		return sqlc.User{}, Link{}, fmt.Errorf("access: read user: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return sqlc.User{}, Link{}, fmt.Errorf("access: commit: %w", err)
	}
	return u, link, nil
}

// cleanEmail accepts an empty email or a bare address of at most 254
// characters: "Alice <a@example.test>" parses as an address but is not one, so
// the parsed address must equal what was typed.
func cleanEmail(raw string) (string, error) {
	email := strings.TrimSpace(raw)
	if email == "" {
		return "", nil
	}
	if utf8.RuneCountInString(email) > maxEmailChars {
		return "", ErrInvalidEmail
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return "", ErrInvalidEmail
	}
	return email, nil
}

// User statuses, in the order that decides which one wins when several apply
// (UI-SPEC: users table statuses).
const (
	StatusDeleted       = "deleted"
	StatusLocked        = "locked"
	StatusDisabled      = "disabled"
	StatusInviteExpired = "invite_expired"
	StatusInvitePending = "invite_pending"
	StatusActive        = "active"
)

// Status is a user's one-word state. When several apply the first of deleted,
// locked, disabled, invite expired, invite pending wins. An invited user is one
// who has not yet set a password; linkLive says whether they still hold an
// unexpired link.
func Status(u sqlc.User, now time.Time, linkLive bool) string {
	switch {
	case u.DeletedAt.Valid:
		return StatusDeleted
	case u.LockedUntil.Valid && u.LockedUntil.Int64 > now.UTC().UnixNano():
		return StatusLocked
	case u.Disabled != 0:
		return StatusDisabled
	case u.PasswordHash == "" && !linkLive:
		return StatusInviteExpired
	case u.PasswordHash == "":
		return StatusInvitePending
	}
	return StatusActive
}

// UserSummary is a user as the admin's users table shows them.
type UserSummary struct {
	ID            string
	Username      string
	DisplayName   string
	Email         string
	InstanceAdmin bool
	Status        string
	// LockedUntil is set only while the account is locked.
	LockedUntil *time.Time
	// LastLoginAt is nil for a user who has never signed in.
	LastLoginAt *time.Time
	TeamCount   int
	// LastInstanceAdmin is true for the one instance admin who can still sign in
	// when there is no other: demoting, disabling or deleting them is refused
	// (D-02), so a screen can say so up front.
	LastInstanceAdmin bool
}

// ListUsers returns every user who is not deleted, sorted by username, with the
// status computed at the service's clock. The caller has checked users.manage.
func (s *Service) ListUsers(ctx context.Context) ([]UserSummary, error) {
	rows, err := s.Store.RQ.ListUsersForAdmin(ctx)
	if err != nil {
		return nil, fmt.Errorf("access: list users: %w", err)
	}
	now := s.now()
	out := make([]UserSummary, 0, len(rows))
	activeAdmins := 0
	for _, r := range rows {
		if r.InstanceAdmin != 0 && r.Disabled == 0 {
			activeAdmins++
		}
	}
	for _, r := range rows {
		u := sqlc.User{PasswordHash: r.PasswordHash, Disabled: r.Disabled, LockedUntil: r.LockedUntil}
		sum := UserSummary{
			ID: r.ID, Username: r.Username, DisplayName: r.DisplayName, Email: r.Email,
			InstanceAdmin: r.InstanceAdmin != 0, TeamCount: int(r.TeamCount),
			Status:            Status(u, now, r.LinkExpiresAt.Valid && r.LinkExpiresAt.Int64 > now.UnixNano()),
			LastInstanceAdmin: activeAdmins == 1 && r.InstanceAdmin != 0 && r.Disabled == 0,
		}
		if u.LockedUntil.Valid && u.LockedUntil.Int64 > now.UnixNano() {
			t := time.Unix(0, u.LockedUntil.Int64).UTC()
			sum.LockedUntil = &t
		}
		if r.LastLoginAt.Valid {
			t := time.Unix(0, r.LastLoginAt.Int64).UTC()
			sum.LastLoginAt = &t
		}
		out = append(out, sum)
	}
	return out, nil
}

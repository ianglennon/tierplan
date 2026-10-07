// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
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

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package access is the service layer behind every user, team, grant and
// session operation (AUTH-04 and the rest of Phase 3). The API routes call it
// and add nothing of their own beyond translating its errors, so the rules that
// protect an installation (a username is never reused, a link works once, the
// last instance admin cannot be removed) live in one place and are tested once.
//
// Every write goes through store.Write with its audit entry recorded in the same
// transaction, and every password is hashed before the transaction opens so the
// single write connection is never held up behind argon2id.
package access

import (
	"errors"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/auth"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/store"
)

// DefaultInviteTTL is how long an invite or reset link works when
// TIERPLAN_INVITE_TTL does not say otherwise (D-24).
const DefaultInviteTTL = 72 * time.Hour

// Errors the service returns. They are stable so the API can map each to a
// status and a problem type.
var (
	// ErrUsernameTaken is a username a live user holds (409 username_taken).
	ErrUsernameTaken = errors.New("access: that username is taken")
	// ErrUsernameReserved is a username that belonged to a deleted user. It stays
	// reserved so the audit log never reads as if the old user were someone else
	// (D-22c).
	ErrUsernameReserved = errors.New("access: that username belonged to a deleted user and stays reserved")
	// ErrInvalidUsername is a username outside the policy of 3 to 32 characters
	// from a-z, 0-9, dot, dash and underscore (D-35).
	ErrInvalidUsername = auth.ErrInvalidUsername
	// ErrInvalidEmail is an email that is not a bare address.
	ErrInvalidEmail = errors.New("access: that is not an email address")
	// ErrDisplayNameTooLong is a display name over 64 characters.
	ErrDisplayNameTooLong = errors.New("access: the display name is longer than 64 characters")
	// ErrInvalidLink is every link that cannot be used: unknown, malformed,
	// expired, already used, replaced by a newer one, or belonging to a disabled
	// or deleted user. The caller must not be able to tell them apart.
	ErrInvalidLink = errors.New("access: this link doesn't work")
	// ErrNotInvited is a user who has already set a password, so there is no
	// invite to regenerate.
	ErrNotInvited = errors.New("access: this user is not waiting on an invite")
	// ErrNotFound is a user, team or session that does not exist.
	ErrNotFound = errors.New("access: not found")
	// ErrUserDisabled is a reset for a disabled user. A disabled user cannot sign
	// in, and a link would not be usable, so an admin enables them first.
	ErrUserDisabled = errors.New("access: that user is disabled")
)

// Link is a freshly issued one-time link. Token is the secret; only its SHA-256
// is stored, so it can be shown exactly once, to the admin who asked for it.
type Link struct {
	Token     string
	ExpiresAt time.Time
}

// Service is the access service.
type Service struct {
	Store   *store.Store
	Audit   *audit.Writer
	Checker *rbac.Checker
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// InviteTTL is how long an invite or reset link works.
	InviteTTL time.Duration
}

// New returns a Service. A zero inviteTTL means DefaultInviteTTL.
func New(st *store.Store, w *audit.Writer, checker *rbac.Checker, inviteTTL time.Duration) *Service {
	if inviteTTL <= 0 {
		inviteTTL = DefaultInviteTTL
	}
	return &Service{Store: st, Audit: w, Checker: checker, Now: time.Now, InviteTTL: inviteTTL}
}

func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

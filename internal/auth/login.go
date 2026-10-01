// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package auth

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// Sign-in errors.
var (
	// ErrInvalidCredentials is the one error every failed sign-in returns,
	// whatever the cause (D-30).
	ErrInvalidCredentials = errors.New("auth: invalid username or password")
	// ErrUserNotFound is returned by Unlock for a name that is not a user.
	ErrUserNotFound = errors.New("auth: no such user")
)

// LoginInput is one sign-in attempt.
type LoginInput struct {
	Username, Password string
	Remember           bool
	IP, UserAgent      string
	// PreviousSessionHash is the session the browser already holds, if any. It
	// is deleted when the sign-in succeeds (rotation, D-32).
	PreviousSessionHash []byte
}

// LoginResult is a successful sign-in.
type LoginResult struct {
	User         sqlc.User
	RawSessionID string
	IDHash       []byte
}

// Login signs users in and out.
type Login struct {
	st          *store.Store
	sess        *Sessions
	aud         *audit.Writer
	maxFailures int
	lockout     time.Duration
	ipLimiter   *IPLimiter
	log         *slog.Logger
	now         func() time.Time
	dummy       func(string)
}

// NewLogin returns a Login. A nil now means time.Now.
func NewLogin(s *store.Store, sess *Sessions, w *audit.Writer, maxFailures int, lockout time.Duration, ipLimiter *IPLimiter, log *slog.Logger, now func() time.Time) *Login {
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &Login{st: s, sess: sess, aud: w, maxFailures: maxFailures, lockout: lockout, ipLimiter: ipLimiter, log: log, now: now, dummy: DummyVerify}
}

// Login checks credentials. Not implemented yet.
func (l *Login) Login(_ context.Context, _ LoginInput) (LoginResult, error) {
	return LoginResult{}, errors.New("not implemented")
}

// Logout ends the principal's current session. Not implemented yet.
func (l *Login) Logout(_ context.Context, _ *rbac.Principal, _, _ string) error {
	return errors.New("not implemented")
}

// Unlock clears an account's lock from the command line. Not implemented yet.
func Unlock(_ context.Context, _ *store.Store, _ *audit.Writer, _ string) error {
	return errors.New("not implemented")
}

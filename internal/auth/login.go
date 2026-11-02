// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// Sign-in errors.
var (
	// ErrInvalidCredentials is the one error every failed sign-in returns,
	// whatever the cause: unknown user, wrong password, locked, disabled or
	// rate limited. The caller must not be able to tell them apart (D-30).
	ErrInvalidCredentials = errors.New("auth: invalid username or password")
	// ErrUserNotFound is returned by Unlock for a name that is not a user.
	ErrUserNotFound = errors.New("auth: no such user")
)

// Reasons recorded with auth.login_failed. They live only in the audit log:
// the response is the same for all of them (D-30).
const (
	reasonRateLimited = "rate_limited"
	reasonUnknownUser = "unknown_user"
	reasonDisabled    = "disabled"
	reasonLocked      = "locked"
	reasonBadPassword = "bad_password"
	// reasonInvitePending is a sign-in as a user who has not yet chosen a password
	// with their invite link (AUTH-04). Like the others it never reaches the
	// caller: the answer is the generic one.
	reasonInvitePending = "invite_pending"
)

// What a refused sign-in records as its target when no stored username can be
// named. The submitted name is never stored or logged unless it is the stored
// username of a real account: a value typed into the username box is often a
// password, and the audit log, its exports and the container log must not hold
// credentials (CR-01, D-39).
const (
	// targetUnknownUser stands for every refusal where no account matched, a
	// malformed name or an unknown one. The audit entry keeps only the length of
	// what was typed, as the name_chars detail.
	targetUnknownUser = "(unknown user)"
	// targetNotChecked stands for a refusal by the per-address limiter, which
	// runs before any lookup, so nothing was matched or even parsed.
	targetNotChecked = "(not checked)"
)

// LoginInput is one sign-in attempt.
type LoginInput struct {
	Username, Password string
	Remember           bool
	IP, UserAgent      string
	// PreviousSessionHash is the session the browser already holds, if any. It
	// is deleted when the sign-in succeeds, so a session ID is never reused
	// across a change of identity (rotation, D-32).
	PreviousSessionHash []byte
}

// LoginResult is a successful sign-in.
type LoginResult struct {
	User sqlc.User
	// RawSessionID goes in the cookie; it is never stored.
	RawSessionID string
	// IDHash is its SHA-256, the sessions table key.
	IDHash []byte
}

// Login signs users in and out (AUTH-02, AUTH-03).
type Login struct {
	st          *store.Store
	sess        *Sessions
	aud         *audit.Writer
	maxFailures int
	lockout     time.Duration
	ipLimiter   *IPLimiter
	log         *slog.Logger
	now         func() time.Time
	// anon, when set, records the rate_limited refusal instead of aud, so a flood
	// of them from one address cannot starve the single write connection (WR-04).
	anon AuditRecorder
	// dummy is the stand-in password check for accounts that do not exist or
	// may not sign in. It is DummyVerify; tests count its calls.
	dummy func(string)
}

// NewLogin returns a Login that locks an account for lockout after
// maxFailures consecutive wrong passwords and lets each client address attempt
// as often as ipLimiter allows. A nil now means time.Now.
func NewLogin(s *store.Store, sess *Sessions, w *audit.Writer, maxFailures int, lockout time.Duration, ipLimiter *IPLimiter, log *slog.Logger, now func() time.Time) *Login {
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &Login{
		st: s, sess: sess, aud: w, maxFailures: maxFailures, lockout: lockout,
		ipLimiter: ipLimiter, log: log, now: now, dummy: DummyVerify,
	}
}

// SetAnonymousAuditor makes the per-address rate_limited refusal, and only it,
// go through a (normally an AuditThrottle). Every other refusal stays a direct,
// synchronous write: a failed sign-in that passed the limiter is one row each
// (T-01-35, D-05).
func (l *Login) SetAnonymousAuditor(a AuditRecorder) { l.anon = a }

// Login checks a username and password and, if they are right, creates a
// session. Every refusal is ErrInvalidCredentials: any other error is a failure
// of the store or the audit log, which the caller reports as a server error.
//
// A refusal is audited as auth.login_failed. Its target is the stored username
// of the account the attempt matched, or the fixed targetUnknownUser (no account
// matched; only the length of the typed name is kept) or targetNotChecked (the
// per-address limit refused it first). The typed name is never recorded
// otherwise, because it may be a password (CR-01).
//
// The checks run in a fixed order. The per-address limit comes first and
// costs nothing. An unknown, disabled or locked account pays one dummy argon2id
// verification, so it cannot be told from a wrong password by timing (D-30). A
// wrong password counts toward the lock; the attempt that reaches the limit
// locks the account. The lock is visible only in the audit log and the
// container log.
func (l *Login) Login(ctx context.Context, in LoginInput) (LoginResult, error) {
	now := l.now()
	name, nameErr := NormalizeUsername(in.Username)
	nameChars := strconv.Itoa(utf8.RuneCountInString(in.Username))

	if l.ipLimiter != nil && !l.ipLimiter.Allow(in.IP) {
		return LoginResult{}, l.refuseVia(ctx, l.anon, in, targetNotChecked, reasonRateLimited, "", "")
	}

	var u sqlc.User
	if nameErr == nil {
		var err error
		u, err = l.st.RQ.GetUserByUsername(ctx, name)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			nameErr = ErrUserNotFound // treated exactly like a malformed name
		case err != nil:
			return LoginResult{}, fmt.Errorf("auth: look up user: %w", err)
		}
	}
	switch {
	case nameErr != nil:
		l.dummy(in.Password)
		return LoginResult{}, l.refuse(ctx, in, targetUnknownUser, reasonUnknownUser, "", nameChars)
	case u.Disabled != 0:
		l.dummy(in.Password)
		return LoginResult{}, l.refuse(ctx, in, u.Username, reasonDisabled, u.ID, "")
	case u.LockedUntil.Valid && u.LockedUntil.Int64 > now.UTC().UnixNano():
		l.dummy(in.Password)
		return LoginResult{}, l.refuse(ctx, in, u.Username, reasonLocked, u.ID, "")
	case u.PasswordHash == "":
		// An invited user has no password yet. This is a normal state, not an
		// unreadable hash, and it must neither count toward a lock nor be told
		// from a wrong password by timing.
		l.dummy(in.Password)
		return LoginResult{}, l.refuse(ctx, in, u.Username, reasonInvitePending, u.ID, "")
	}

	ok, err := VerifyPassword(in.Password, u.PasswordHash)
	if err != nil {
		// A stored hash that cannot be read is a data problem, not a reason to
		// let anyone in: it counts as a wrong password.
		l.log.Error("stored password hash is unreadable", "user_id", u.ID, "error", err)
		ok = false
	}
	if !ok {
		return LoginResult{}, l.badPassword(ctx, in, u)
	}
	return l.succeed(ctx, in, u)
}

// refuse records a refusal that changes no state and returns the one error.
// target is a stored username or one of the fixed placeholders, never raw input.
func (l *Login) refuse(ctx context.Context, in LoginInput, target, reason, userID, nameChars string) error {
	return l.refuseVia(ctx, nil, in, target, reason, userID, nameChars)
}

// refuseVia is refuse with a recorder chosen by the caller; nil means the writer.
func (l *Login) refuseVia(ctx context.Context, via AuditRecorder, in LoginInput, target, reason, userID, nameChars string) error {
	if via == nil {
		via = l.aud
	}
	if err := via.Record(ctx, l.failureEntry(in, target, reason, userID, nameChars)); err != nil {
		return fmt.Errorf("auth: audit failed sign-in: %w", err)
	}
	return ErrInvalidCredentials
}

// failureEntry builds the auth.login_failed entry. Its details name the account
// when one matched (user_id) and otherwise hold only how many characters were
// typed (name_chars), so no client input is stored (CR-01).
func (l *Login) failureEntry(in LoginInput, target, reason, userID, nameChars string) audit.Entry {
	e := audit.Entry{
		Actor:      audit.Actor{Type: audit.ActorAnonymous},
		Action:     audit.ActionAuthLoginFailed,
		TargetType: "user",
		TargetID:   target,
		Result:     audit.ResultFailure,
		Reason:     reason,
		IP:         in.IP,
		UserAgent:  in.UserAgent,
	}
	switch {
	case userID != "":
		e.Details = map[string]string{"user_id": userID}
	case nameChars != "":
		e.Details = map[string]string{"name_chars": nameChars}
	}
	return e
}

// badPassword counts a wrong password and, when it is the one that reaches the
// limit, locks the account, all in one write transaction with the audit entries
// so the count, the lock and the log agree.
func (l *Login) badPassword(ctx context.Context, in LoginInput, u sqlc.User) error {
	now := l.now().UTC()
	tx, err := l.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("auth: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := l.st.Q.WithTx(tx)

	if u.LockedUntil.Valid && u.LockedUntil.Int64 <= now.UnixNano() {
		// The lock has run out: this is the first failure of a new run, not the
		// sixth of the old one.
		if err := q.ResetLoginFailures(ctx, u.ID); err != nil {
			return fmt.Errorf("auth: clear expired lock: %w", err)
		}
	}
	count, err := q.RecordLoginFailure(ctx, u.ID)
	if err != nil {
		return fmt.Errorf("auth: count failure: %w", err)
	}
	if err := l.aud.RecordTx(ctx, tx, l.failureEntry(in, u.Username, reasonBadPassword, u.ID, "")); err != nil {
		return fmt.Errorf("auth: audit failed sign-in: %w", err)
	}
	var lockedUntil time.Time
	if count >= int64(l.maxFailures) {
		lockedUntil = now.Add(l.lockout)
		if err := q.LockUser(ctx, sqlc.LockUserParams{LockedUntil: sql.NullInt64{Int64: lockedUntil.UnixNano(), Valid: true}, ID: u.ID}); err != nil {
			return fmt.Errorf("auth: lock account: %w", err)
		}
		if err := l.aud.RecordTx(ctx, tx, audit.Entry{
			Actor:      audit.Actor{Type: audit.ActorAnonymous},
			Action:     audit.ActionAuthLocked,
			TargetType: "user",
			TargetID:   u.ID,
			Result:     audit.ResultFailure,
			Reason:     "too_many_failures",
			IP:         in.IP,
			UserAgent:  in.UserAgent,
			Details:    map[string]string{"username": u.Username, "failures": strconv.FormatInt(count, 10), "until": lockedUntil.Format(time.RFC3339)},
		}); err != nil {
			return fmt.Errorf("auth: audit lock: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("auth: commit: %w", err)
	}
	if !lockedUntil.IsZero() {
		l.log.Warn("account locked", "username", u.Username, "until", lockedUntil.Format(time.RFC3339), "failures", count, "ip", in.IP)
	}
	return ErrInvalidCredentials
}

// succeed ends the failure run, retires the browser's old session, creates the
// new one and audits the sign-in, in one write transaction.
func (l *Login) succeed(ctx context.Context, in LoginInput, u sqlc.User) (LoginResult, error) {
	tx, err := l.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return LoginResult{}, fmt.Errorf("auth: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := l.st.Q.WithTx(tx)

	if err := q.ResetLoginFailures(ctx, u.ID); err != nil {
		return LoginResult{}, fmt.Errorf("auth: reset failures: %w", err)
	}
	if err := q.SetUserLastLogin(ctx, sqlc.SetUserLastLoginParams{LastLoginAt: sql.NullInt64{Int64: l.now().UTC().UnixNano(), Valid: true}, ID: u.ID}); err != nil {
		return LoginResult{}, fmt.Errorf("auth: stamp last sign-in: %w", err)
	}
	if len(in.PreviousSessionHash) > 0 {
		if err := q.DeleteSession(ctx, in.PreviousSessionHash); err != nil {
			return LoginResult{}, fmt.Errorf("auth: rotate session: %w", err)
		}
	}
	raw, hash, err := l.sess.CreateTx(ctx, tx, u.ID, in.Remember, in.IP, in.UserAgent)
	if err != nil {
		return LoginResult{}, err
	}
	if err := l.aud.RecordTx(ctx, tx, audit.Entry{
		Actor:      audit.Actor{Type: audit.ActorUser, ID: u.ID, Name: u.Username},
		Action:     audit.ActionAuthLogin,
		TargetType: "user",
		TargetID:   u.ID,
		Result:     audit.ResultSuccess,
		IP:         in.IP,
		UserAgent:  in.UserAgent,
		Details:    map[string]string{"remember": strconv.FormatBool(in.Remember)},
	}); err != nil {
		return LoginResult{}, fmt.Errorf("auth: audit sign-in: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return LoginResult{}, fmt.Errorf("auth: commit: %w", err)
	}
	u.FailedCount = 0
	u.LockedUntil = sql.NullInt64{}
	u.LastLoginAt = sql.NullInt64{Int64: l.now().UTC().UnixNano(), Valid: true}
	return LoginResult{User: u, RawSessionID: raw, IDHash: hash}, nil
}

// Logout ends the principal's current session and nothing else: a user's other
// devices stay signed in (D-33). The principal must carry the session it
// authenticated with.
func (l *Login) Logout(ctx context.Context, p *rbac.Principal, ip, userAgent string) error {
	if p == nil || len(p.SessionIDHash) == 0 {
		return errors.New("auth: logout needs a signed-in session")
	}
	tx, err := l.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("auth: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed

	if err := l.st.Q.WithTx(tx).DeleteSession(ctx, p.SessionIDHash); err != nil {
		return fmt.Errorf("auth: delete session: %w", err)
	}
	if err := l.aud.RecordTx(ctx, tx, audit.Entry{
		Actor:      p.AuditActor(),
		Action:     audit.ActionAuthLogout,
		TargetType: "user",
		TargetID:   p.ID,
		Result:     audit.ResultSuccess,
		IP:         ip,
		UserAgent:  userAgent,
	}); err != nil {
		return fmt.Errorf("auth: audit sign-out: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("auth: commit: %w", err)
	}
	return nil
}

// Unlock clears an account's lock and failure count, for the operator who ran
// `tierplan user unlock` (D-29). It is audited as system:cli. A name that is not
// a user is ErrUserNotFound and writes nothing.
func Unlock(ctx context.Context, s *store.Store, w *audit.Writer, username string) error {
	name, err := NormalizeUsername(username)
	if err != nil {
		return ErrUserNotFound
	}
	u, err := s.RQ.GetUserByUsername(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("auth: look up user: %w", err)
	}
	tx, err := s.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("auth: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed

	if err := s.Q.WithTx(tx).ResetLoginFailures(ctx, u.ID); err != nil {
		return fmt.Errorf("auth: unlock: %w", err)
	}
	if err := w.RecordTx(ctx, tx, audit.Entry{
		Actor:      audit.SystemCLI,
		Action:     audit.ActionAuthUnlocked,
		TargetType: "user",
		TargetID:   u.ID,
		Result:     audit.ResultSuccess,
		Details:    map[string]string{"username": u.Username},
	}); err != nil {
		return fmt.Errorf("auth: audit unlock: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("auth: commit: %w", err)
	}
	return nil
}

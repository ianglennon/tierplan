// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// CSRFHeader is the request header that carries the session's CSRF token (D-36).
const CSRFHeader = "X-CSRF-Token"

// ErrCSRF means a mutating request did not carry its session's CSRF token.
var ErrCSRF = errors.New("auth: missing or invalid CSRF token")

const (
	// touchInterval is the most often last_seen_at is written for one session,
	// so a busy page does not turn every request into a database write (D-32).
	touchInterval = time.Minute
	// maxSessionIDLength bounds what is hashed: a real ID is 43 characters.
	maxSessionIDLength = 128
	cleanupInterval    = time.Hour
)

// HashSessionID is the SHA-256 of a raw session ID, the sessions table key.
func HashSessionID(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

type authenticator struct {
	st    *store.Store
	idle  time.Duration
	scope func() CookieScope
	now   func() time.Time
}

// NewAuthenticator resolves the session cookie to a principal (D-31, D-32). A
// browser-session cookie that has been idle for longer than idle, a remembered
// one past its absolute expiry, and any session of a disabled user are treated
// as anonymous, and the expired or orphaned row is deleted. A nil now means
// time.Now.
//
// scope reports the path the public URL is served under; it is read on every
// request because an administrator can change the URL at run time. Only the
// cookie named for the current scope is believed (__Host- at the root,
// __Secure- under a base), so a cookie tossed from a sibling host, under the
// bare name or under another scope is ignored (WR-01, D-12, D-50). A nil scope
// means the root, so the __Host- cookie is read. Changing the base therefore
// signs existing browser sessions out once, which is the accepted cost.
func NewAuthenticator(s *store.Store, idle time.Duration, scope func() CookieScope, now func() time.Time) httpx.Authenticator {
	if now == nil {
		now = time.Now
	}
	if scope == nil {
		scope = func() CookieScope { return CookieScope{} }
	}
	return &authenticator{st: s, idle: idle, scope: scope, now: now}
}

// sessionCookie returns the session ID in the one cookie the current
// configuration would set, and nothing from any other cookie name.
func (a *authenticator) sessionCookie(r *http.Request) string {
	if c, err := r.Cookie(CookieName(a.scope())); err == nil {
		return c.Value
	}
	return ""
}

// Authenticate implements httpx.Authenticator.
func (a *authenticator) Authenticate(r *http.Request) (*rbac.Principal, error) {
	raw := a.sessionCookie(r)
	if raw == "" || len(raw) > maxSessionIDLength {
		return nil, nil
	}
	ctx := r.Context()
	hash := HashSessionID(raw)
	row, err := a.st.RQ.GetSessionByHash(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("auth: look up session: %w", err)
	}

	now := a.now().UTC().UnixNano()
	if a.expired(row, now) {
		return nil, a.drop(ctx, hash)
	}
	u, err := a.st.RQ.GetUserByID(ctx, row.UserID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, a.drop(ctx, hash)
	case err != nil:
		return nil, fmt.Errorf("auth: look up session user: %w", err)
	case u.Disabled != 0:
		return nil, a.drop(ctx, hash)
	}

	if now-row.LastSeenAt >= int64(touchInterval) {
		// Best effort: failing to record that the session was used must not
		// sign the user out.
		if err := a.st.Q.TouchSession(ctx, sqlc.TouchSessionParams{LastSeenAt: now, IDHash: hash}); err != nil {
			slog.Warn("could not update a session's last-seen time", "error", err)
		}
	}
	return &rbac.Principal{Type: rbac.PrincipalUser, ID: u.ID, Name: u.Username, SessionIDHash: hash}, nil
}

// Recheck reports whether the session behind p is still valid: the row exists,
// it has not expired, it belongs to p's user and that user is not disabled
// (WR-02). It is for long-lived routes that must end when the session does, so
// unlike Authenticate it never writes: it does not touch last_seen_at, which
// would let an open stream keep an idle session alive (D-31), and it does not
// delete an expired row, which is left to the next request or the cleanup.
func (a *authenticator) Recheck(ctx context.Context, p *rbac.Principal) (bool, error) {
	if p == nil || len(p.SessionIDHash) == 0 {
		return false, nil
	}
	row, err := a.st.RQ.GetSessionByHash(ctx, p.SessionIDHash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("auth: recheck session: %w", err)
	}
	if a.expired(row, a.now().UTC().UnixNano()) || row.UserID != p.ID {
		return false, nil
	}
	u, err := a.st.RQ.GetUserByID(ctx, row.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("auth: recheck session user: %w", err)
	}
	return u.Disabled == 0, nil
}

// expired applies D-31. A browser-session cookie ends after idle without use. A
// remembered session ignores idle time and ends at its absolute expiry; one
// that somehow has none is refused rather than made immortal.
func (a *authenticator) expired(row sqlc.Session, now int64) bool {
	if row.Remember == 0 {
		return now-row.LastSeenAt > int64(a.idle)
	}
	return !row.AbsoluteExpiresAt.Valid || now > row.AbsoluteExpiresAt.Int64
}

// drop deletes a session that must not be used again. The caller is anonymous
// either way, so a failed delete is a server error only because it leaves a row
// that should be gone.
func (a *authenticator) drop(ctx context.Context, hash []byte) error {
	if err := a.st.Q.DeleteSession(ctx, hash); err != nil {
		return fmt.Errorf("auth: delete expired session: %w", err)
	}
	return nil
}

type csrfGuard struct{ sess *Sessions }

// NewCSRFGuard returns the MutationGuard that requires X-CSRF-Token to equal
// the session's own token on every mutating request a signed-in caller makes
// (D-36). The comparison is constant-time. Anonymous callers have no session to
// forge a request against and are left to the Origin guard.
func NewCSRFGuard(sess *Sessions) httpx.MutationGuard { return csrfGuard{sess: sess} }

// Check implements httpx.MutationGuard.
func (g csrfGuard) Check(r *http.Request, p *rbac.Principal) error {
	if p == nil {
		return nil
	}
	if len(p.SessionIDHash) == 0 {
		return ErrCSRF
	}
	want := g.sess.CSRFToken(p.SessionIDHash)
	if subtle.ConstantTimeCompare([]byte(r.Header.Get(CSRFHeader)), []byte(want)) != 1 {
		return ErrCSRF
	}
	return nil
}

// RunSessionCleanup deletes expired sessions now and then every hour until ctx
// ends. Expiry is enforced on lookup; this only keeps the table from growing
// with sessions nobody presents again (T-01-53).
func RunSessionCleanup(ctx context.Context, s *store.Store, idle time.Duration, now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	run := func() {
		t := now().UTC()
		n, err := s.Q.DeleteExpiredSessions(ctx, sqlc.DeleteExpiredSessionsParams{
			IdleCutoff: t.Add(-idle).UnixNano(),
			Now:        t.UnixNano(),
		})
		switch {
		case ctx.Err() != nil:
		case err != nil:
			slog.Error("session cleanup failed", "error", err)
		case n > 0:
			slog.Info("expired sessions removed", "count", n)
		}
	}
	run()
	tick := time.NewTicker(cleanupInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			run()
		}
	}
}

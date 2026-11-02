// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/vault"
)

// Cookie names (D-12, D-32, D-50). The __Host- prefix makes a browser accept the
// cookie only with Secure, Path=/ and no Domain, so it is used whenever the
// public URL is https and has no path. Under a base path the cookie needs
// Path=<base>, which __Host- forbids, so https uses __Secure- (Secure is still
// required, Path and Domain are free). Over plain http either prefix would make
// the browser drop the cookie, so the bare name is used.
const (
	hostCookieName   = "__Host-tierplan_session"
	secureCookieName = "__Secure-tierplan_session"
	plainCookieName  = "tierplan_session"
)

// csrfLabel is the vault subkey label the CSRF tokens are keyed with (D-36).
const csrfLabel = "tierplan/csrf/v1"

const (
	sessionIDBytes = 32 // 256 bits
	// publicIDBytes is the size of a session's public ID: 128 bits, 32 hex characters.
	publicIDBytes = 16
)

// Sessions creates and describes server-side sessions. The database holds only
// the SHA-256 of each session ID, so a copy of it contains nothing a browser
// could present (D-32).
type Sessions struct {
	st       *store.Store
	idle     time.Duration
	remember time.Duration
	now      func() time.Time
	csrfKey  []byte
}

// NewSessions returns a session manager. idle is the idle timeout of
// browser-session cookies and remember the absolute lifetime of "keep me signed
// in" sessions (D-31). A nil now means time.Now.
func NewSessions(s *store.Store, v *vault.Vault, idle, remember time.Duration, now func() time.Time) *Sessions {
	if now == nil {
		now = time.Now
	}
	return &Sessions{st: s, idle: idle, remember: remember, now: now, csrfKey: v.Subkey(csrfLabel)}
}

// IdleTimeout is how long a browser-session cookie may go unused.
func (m *Sessions) IdleTimeout() time.Duration { return m.idle }

// RememberLifetime is the absolute lifetime of a "keep me signed in" session.
func (m *Sessions) RememberLifetime() time.Duration { return m.remember }

// CreateTx stores a new session for userID inside the caller's write
// transaction and returns the raw ID, which goes in the cookie and is never
// stored, and its SHA-256, which is the database key. The session exists only
// if the caller commits. A remembered session gets an absolute expiry; the
// others end by idle timeout.
func (m *Sessions) CreateTx(ctx context.Context, tx *sql.Tx, userID string, remember bool, ip, userAgent string) (rawID string, idHash []byte, err error) {
	buf := make([]byte, sessionIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("generate session id: %w", err)
	}
	rawID = base64.RawURLEncoding.EncodeToString(buf)
	idHash = HashSessionID(rawID)
	// The public ID is the handle a session list names a session by. It is
	// random and unrelated to the cookie, so showing it to the user gives away
	// nothing a browser could present (RESEARCH A9, T-03-59).
	pub := make([]byte, publicIDBytes)
	if _, err := rand.Read(pub); err != nil {
		return "", nil, fmt.Errorf("generate session public id: %w", err)
	}

	now := m.now().UTC().UnixNano()
	p := sqlc.InsertSessionParams{
		IDHash:     idHash,
		UserID:     userID,
		CreatedAt:  now,
		LastSeenAt: now,
		Ip:         ip,
		UserAgent:  userAgent,
		PublicID:   sql.NullString{String: hex.EncodeToString(pub), Valid: true},
	}
	if remember {
		p.Remember = 1
		p.AbsoluteExpiresAt = sql.NullInt64{Int64: m.now().Add(m.remember).UTC().UnixNano(), Valid: true}
	}
	if err := m.st.Q.WithTx(tx).InsertSession(ctx, p); err != nil {
		return "", nil, fmt.Errorf("store session: %w", err)
	}
	return rawID, idHash, nil
}

// CookieScope is what decides the session cookie's name, Path and Secure flag:
// whether the public URL is https, and the path of the public URL (D-50).
type CookieScope struct {
	// Secure is true when the public URL is https.
	Secure bool
	// BasePath is the path of the public URL, "" for the root.
	BasePath string
}

// CookieName is the session cookie's name for the scope: __Host- for https at
// the root, __Secure- for https under a base path, bare over http (D-50). The
// authenticator reads only this name, so a cookie tossed under another name or
// scope is ignored (T-02-12).
func CookieName(scope CookieScope) string {
	switch {
	case scope.Secure && scope.BasePath == "":
		return hostCookieName
	case scope.Secure:
		return secureCookieName
	default:
		return plainCookieName
	}
}

// cookiePath is "/" at the root and the base path under one, so a browser sends
// the cookie only to TierPlan's own path and not to a neighbour on the same host.
func cookiePath(scope CookieScope) string {
	if scope.BasePath == "" {
		return "/"
	}
	return scope.BasePath
}

// Cookie builds the session cookie. It is returned by value so a huma handler
// can hand it to the response through a `header:"Set-Cookie"` output field.
// Without remember there is no Max-Age, so the browser drops it when it closes;
// with remember it lasts the remember lifetime. It is HttpOnly, SameSite=Lax,
// Path=/ (or the base path, D-50) and never has a Domain.
func (m *Sessions) Cookie(rawID string, remember bool, scope CookieScope) http.Cookie {
	c := http.Cookie{ //nolint:gosec // Secure follows the public URL scheme (D-12): the __Host- prefix needs it, and over plain http a Secure cookie would never be stored
		Name:     CookieName(scope),
		Value:    rawID,
		Path:     cookiePath(scope),
		HttpOnly: true,
		Secure:   scope.Secure,
		SameSite: http.SameSiteLaxMode,
	}
	if remember {
		c.MaxAge = int(m.remember / time.Second)
	}
	return c
}

// ClearCookie builds the cookie that makes a browser delete the session cookie.
func ClearCookie(scope CookieScope) http.Cookie {
	return http.Cookie{ //nolint:gosec // Secure must match the cookie being cleared (D-12)
		Name:     CookieName(scope),
		Value:    "",
		Path:     cookiePath(scope),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   scope.Secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// CSRFToken is the per-session token the SPA sends as X-CSRF-Token on every
// mutating request (D-36): HMAC-SHA256 of the session's ID hash under the
// vault's tierplan/csrf/v1 subkey, in unpadded base64url. It needs no storage,
// is stable for the life of the session and reveals neither the session ID nor
// its hash.
func (m *Sessions) CSRFToken(idHash []byte) string {
	mac := hmac.New(sha256.New, m.csrfKey)
	mac.Write(idHash)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"

	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/internal/vault"
)

// Cookie names (D-12, D-32). The __Host- prefix makes a browser accept the
// cookie only with Secure, Path=/ and no Domain, so it is used whenever the
// public URL is https; over plain http the prefix would make the browser drop
// the cookie, so the bare name is used.
const (
	secureCookieName = "__Host-bayplan_session"
	plainCookieName  = "bayplan_session"
)

// csrfLabel is the vault subkey label the CSRF tokens are keyed with (D-36).
const csrfLabel = "bayplan/csrf/v1"

const sessionIDBytes = 32 // 256 bits

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

	now := m.now().UTC().UnixNano()
	p := sqlc.InsertSessionParams{
		IDHash:     idHash,
		UserID:     userID,
		CreatedAt:  now,
		LastSeenAt: now,
		Ip:         ip,
		UserAgent:  userAgent,
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

// CookieName is the session cookie's name for a public URL that is (secure) or
// is not https.
func CookieName(secure bool) string {
	if secure {
		return secureCookieName
	}
	return plainCookieName
}

// Cookie builds the session cookie. It is returned by value so a huma handler
// can hand it to the response through a `header:"Set-Cookie"` output field.
// Without remember there is no Max-Age, so the browser drops it when it closes;
// with remember it lasts the remember lifetime. It is HttpOnly, SameSite=Lax,
// Path=/ and never has a Domain.
func (m *Sessions) Cookie(rawID string, remember, secure bool) http.Cookie {
	c := http.Cookie{ //nolint:gosec // Secure follows the public URL scheme (D-12): the __Host- prefix needs it, and over plain http a Secure cookie would never be stored
		Name:     CookieName(secure),
		Value:    rawID,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
	if remember {
		c.MaxAge = int(m.remember / time.Second)
	}
	return c
}

// ClearCookie builds the cookie that makes a browser delete the session cookie.
func ClearCookie(secure bool) http.Cookie {
	return http.Cookie{ //nolint:gosec // Secure must match the cookie being cleared (D-12)
		Name:     CookieName(secure),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// CSRFToken is the per-session token the SPA sends as X-CSRF-Token on every
// mutating request (D-36): HMAC-SHA256 of the session's ID hash under the
// vault's bayplan/csrf/v1 subkey, in unpadded base64url. It needs no storage,
// is stable for the life of the session and reveals neither the session ID nor
// its hash.
func (m *Sessions) CSRFToken(idHash []byte) string {
	mac := hmac.New(sha256.New, m.csrfKey)
	mac.Write(idHash)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package auth

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/vault"
)

// RED stub: signatures only, so the account tests compile and fail on their assertions.

type Sessions struct{}

func NewSessions(s *store.Store, v *vault.Vault, idle, remember time.Duration, now func() time.Time) *Sessions {
	return &Sessions{}
}

func (m *Sessions) CreateTx(ctx context.Context, tx *sql.Tx, userID string, remember bool, ip, userAgent string) (string, []byte, error) {
	return "", nil, nil
}
func CookieName(secure bool) string                                        { return "" }
func (m *Sessions) Cookie(rawID string, remember, secure bool) http.Cookie { return http.Cookie{} }
func ClearCookie(secure bool) http.Cookie                                  { return http.Cookie{} }
func (m *Sessions) CSRFToken(idHash []byte) string                         { return "" }

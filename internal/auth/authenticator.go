// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
)

// ErrCSRF means a mutating request did not carry its session's CSRF token.
var ErrCSRF = errors.New("auth: missing or invalid CSRF token")

type authenticator struct{}

func (authenticator) Authenticate(*http.Request) (*rbac.Principal, error) { return nil, nil }

// NewAuthenticator resolves the session cookie to a principal. Not implemented yet.
func NewAuthenticator(_ *store.Store, _ time.Duration, _ func() time.Time) httpx.Authenticator {
	return authenticator{}
}

type csrfGuard struct{}

func (csrfGuard) Check(*http.Request, *rbac.Principal) error { return nil }

// NewCSRFGuard checks X-CSRF-Token. Not implemented yet.
func NewCSRFGuard(_ *Sessions) httpx.MutationGuard { return csrfGuard{} }

// RunSessionCleanup deletes expired sessions hourly. Not implemented yet.
func RunSessionCleanup(_ context.Context, _ *store.Store, _ time.Duration, _ func() time.Time) {}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package httpx

import (
	"context"
	"errors"
	"net/http"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// Authenticator resolves the caller of a request.
type Authenticator interface {
	Authenticate(r *http.Request) (*rbac.Principal, error)
}

// Env is a resolved environment.
type Env struct {
	ID, Name string
	Enabled  bool
}

// Environment lookup errors.
var (
	ErrEnvNotFound = errors.New("httpx: environment not found")
	ErrEnvDisabled = errors.New("httpx: environment disabled")
)

// EnvResolver looks an environment up by ID.
type EnvResolver interface {
	Resolve(ctx context.Context, envID string) (Env, error)
}

// Auditor records audit entries.
type Auditor interface {
	Record(ctx context.Context, e audit.Entry) error
}

// MutationGuard vets mutating requests.
type MutationGuard interface {
	Check(r *http.Request, p *rbac.Principal) error
}

// PrincipalFrom returns the authenticated principal of the request.
func PrincipalFrom(ctx context.Context) *rbac.Principal { return nil }

// EnvFrom returns the resolved environment of the request.
func EnvFrom(ctx context.Context) *Env { return nil }

// ClientIP returns the resolved client address.
func ClientIP(ctx context.Context) string { return "" }

// Guarded mounts a plain handler behind the policy.
func (r *Router) Guarded(method, path string, need rbac.Requirement, h http.Handler) {}

// VerifyAllGuarded checks every mounted route is guarded.
func VerifyAllGuarded(r *Router) error { return nil }

// AnonymousAuthenticator authenticates nobody.
type AnonymousAuthenticator struct{}

// Authenticate implements Authenticator.
func (AnonymousAuthenticator) Authenticate(*http.Request) (*rbac.Principal, error) { return nil, nil }

// OriginOf returns the origin of a URL.
func OriginOf(raw string) string { return "" }

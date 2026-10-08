// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"log/slog"
	"time"

	"github.com/ianglennon/bayplan/internal/access"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/instance"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// Problem types of the access routes. The SPA maps each to its copy.
const (
	problemUsernameTaken    = "username_taken"
	problemUsernameReserved = "username_reserved"
	problemInvalidLink      = "invalid_link"
	problemNotInvited       = "not_invited"
	problemUserNotFound     = "user_not_found"
	// problemLastInstanceAdmin is a change that would leave no active instance admin.
	problemLastInstanceAdmin = "last_instance_admin"
	// problemStaleChange is a save based on a version that has since changed.
	problemStaleChange = "stale_change"
	// problemUserDisabled is a password reset for a user who is disabled.
	problemUserDisabled = "user_disabled"
)

// invalidLinkDetail is the one message every unusable link carries: expired,
// used, replaced, unknown, malformed and a disabled or deleted user's must not be
// told apart.
const invalidLinkDetail = "This link doesn't work."

// AccessDeps are the dependencies of the user, team, grant and session routes.
// Each access plan registers its routes in RegisterAccess and adds nothing here
// that this struct does not already carry.
type AccessDeps struct {
	Access   *access.Service
	Store    *store.Store
	Checker  *rbac.Checker
	Instance *instance.Instance
	Sessions *auth.Sessions
	// Limiter throttles the public invite routes per client address.
	Limiter *auth.IPLimiter
	// AnonAudit records the refusals an anonymous caller can repeat without
	// limit; it is normally an auth.AuditThrottle. Nil means the access
	// service's own writer.
	AnonAudit httpx.Auditor
	// Now is the clock statuses are judged by. Nil means time.Now.
	Now func() time.Time
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

func (d AccessDeps) now() time.Time {
	if d.Now == nil {
		return time.Now()
	}
	return d.Now()
}

// RegisterAccess registers the access routes.
func RegisterAccess(r *httpx.Router, d AccessDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	registerUsers(r, d)
	registerTeams(r, d)
	registerGrants(r, d)
	registerInvite(r, d)
	registerSessions(r, d)
	registerProfile(r, d)
}

// linkURL is the address an admin hands to a user: access.LinkURL on this
// instance's public URL and base path, which is also what the CLI prints.
func linkURL(inst *instance.Instance, token string) string {
	return access.LinkURL(inst.PublicURL(), inst.BasePath(), token)
}

// userStatus is a user's one-word state: access.Status, which holds the
// precedence. linkLive says whether they still hold an unexpired link.
func userStatus(u sqlc.User, now time.Time, linkLive bool) string {
	return access.Status(u, now, linkLive)
}

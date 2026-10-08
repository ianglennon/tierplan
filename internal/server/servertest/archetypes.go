// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package servertest

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/server"
	"github.com/ianglennon/bayplan/internal/store"
)

// Archetype names a kind of caller the enforcement test drives every route as.
type Archetype string

// The principal archetypes. Environment A and B are the two environments
// SeedArchetypes creates.
const (
	// ArchetypeInstanceAdmin holds the instance-admin flag.
	ArchetypeInstanceAdmin Archetype = "instance-admin"
	// ArchetypeEnvAdminA is Admin on environment A only.
	ArchetypeEnvAdminA Archetype = "env-admin-A"
	// ArchetypeOperatorA is Operator on A with no extra permission.
	ArchetypeOperatorA Archetype = "operator-A"
	// ArchetypeOperatorAExec is Operator on A carrying the containers.exec extra.
	ArchetypeOperatorAExec Archetype = "operator-A-exec"
	// ArchetypeViewerA is Viewer on A only.
	ArchetypeViewerA Archetype = "viewer-A"
	// ArchetypeViewerAll is Viewer on every environment.
	ArchetypeViewerAll Archetype = "viewer-all"
	// ArchetypeViewerB is Viewer on B only.
	ArchetypeViewerB Archetype = "viewer-B"
	// ArchetypeNoGrant is a signed-in user with no grant at all.
	ArchetypeNoGrant Archetype = "no-grant"
	// ArchetypeDisabled has a live session row but the user is disabled; the
	// account also carries the strongest grants, so only the disabled flag can be
	// the reason it is refused.
	ArchetypeDisabled Archetype = "disabled"
	// ArchetypeAnonymous has no session at all.
	ArchetypeAnonymous Archetype = "anonymous"
)

// Archetypes lists every archetype in a fixed order.
func Archetypes() []Archetype {
	return []Archetype{
		ArchetypeInstanceAdmin, ArchetypeEnvAdminA, ArchetypeOperatorA, ArchetypeOperatorAExec,
		ArchetypeViewerA, ArchetypeViewerAll, ArchetypeViewerB, ArchetypeNoGrant,
		ArchetypeDisabled, ArchetypeAnonymous,
	}
}

// Grants is what an archetype holds, written out so a test can compute what each
// route must answer without asking the Checker it is testing.
type Grants struct {
	// Anonymous is true when the archetype has no account.
	Anonymous bool
	// Disabled is true when the account is disabled, whatever it is granted.
	Disabled bool
	// InstanceAdmin is the users.instance_admin flag.
	InstanceAdmin bool
	// OnA, OnB and OnAll are the environment roles on environment A, on B and on
	// every environment; rbac.RoleNone means no grant.
	OnA, OnB, OnAll rbac.Role
	// Extras are the dangerous permissions the Operator assignment on A carries.
	Extras []rbac.Perm
}

// GrantsOf is the table SeedArchetypes writes.
func GrantsOf(a Archetype) Grants {
	switch a {
	case ArchetypeInstanceAdmin:
		return Grants{InstanceAdmin: true}
	case ArchetypeEnvAdminA:
		return Grants{OnA: rbac.RoleAdmin}
	case ArchetypeOperatorA:
		return Grants{OnA: rbac.RoleOperator}
	case ArchetypeOperatorAExec:
		return Grants{OnA: rbac.RoleOperator, Extras: []rbac.Perm{rbac.PermContainersExec}}
	case ArchetypeViewerA:
		return Grants{OnA: rbac.RoleViewer}
	case ArchetypeViewerAll:
		return Grants{OnAll: rbac.RoleViewer}
	case ArchetypeViewerB:
		return Grants{OnB: rbac.RoleViewer}
	case ArchetypeNoGrant:
		return Grants{}
	case ArchetypeDisabled:
		return Grants{Disabled: true, InstanceAdmin: true, OnA: rbac.RoleAdmin, OnB: rbac.RoleAdmin, OnAll: rbac.RoleAdmin}
	}
	return Grants{Anonymous: true}
}

// UserID is the ID of the account SeedArchetypes creates for the archetype, or
// the empty string for the anonymous one.
func UserID(a Archetype) string {
	if a == ArchetypeAnonymous {
		return ""
	}
	return "arch-" + string(a)
}

// EnsureEnvironments makes each ID an enabled agent environment, creating the
// row when it is missing and re-enabling it when a call disabled it. The
// enforcement test calls it between requests, so one allowed call that changes an
// environment cannot change what the next one is expected to see.
func EnsureEnvironments(t testing.TB, st *store.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := st.Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at)
VALUES (?, 'agent', ?, '', 1, 1, 1)
ON CONFLICT (id) DO UPDATE SET enabled = 1`, id, "env "+id); err != nil {
			t.Fatalf("servertest: ensure environment %s: %v", id, err)
		}
	}
}

// SeedArchetypes creates environments A and B (agent type, enabled) when they are
// absent and one real user per archetype with its grants, and returns each
// archetype's user ID. The anonymous archetype maps to the empty string. Running
// it twice is harmless.
func SeedArchetypes(t testing.TB, st *store.Store, envA, envB string) map[Archetype]string {
	t.Helper()
	EnsureEnvironments(t, st, envA, envB)

	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashPassword(base64.RawURLEncoding.EncodeToString(raw)) // never printed, never used to sign in
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.Write.Exec(q, args...); err != nil {
			t.Fatalf("servertest: %s: %v", q, err)
		}
	}
	ids := map[Archetype]string{}
	for _, a := range Archetypes() {
		ids[a] = UserID(a)
		if a == ArchetypeAnonymous {
			continue
		}
		g, id := GrantsOf(a), UserID(a)
		exec(`INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at)
VALUES (?, ?, ?, 1, 1, 1) ON CONFLICT DO NOTHING`, id, id, hash)
		if g.InstanceAdmin {
			exec(`UPDATE users SET instance_admin = 1 WHERE id = ?`, id)
		}
		if g.Disabled {
			exec(`UPDATE users SET disabled = 1 WHERE id = ?`, id)
		}
	}
	EnsureGrants(t, st, envA, envB)
	return ids
}

// EnsureGrants gives every archetype its grants on environments A and B (and on
// all of them) when they are missing. Removing an environment, or folding it away,
// deletes the grants on it, so the enforcement test calls this between requests
// like EnsureEnvironments: one allowed call that removes environment A cannot
// change what the next is expected to see. It is harmless to run twice.
func EnsureGrants(t testing.TB, st *store.Store, envA, envB string) {
	t.Helper()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.Write.Exec(q, args...); err != nil {
			t.Fatalf("servertest: %s: %v", q, err)
		}
	}
	assign := func(userID string, role rbac.Role, envID string, extras ...rbac.Perm) {
		t.Helper()
		var env any
		if envID != "" {
			env = envID
		}
		id := "ra-" + userID + "-" + envID
		exec(`INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at)
VALUES (?, 'user', ?, ?, ?, 1) ON CONFLICT DO NOTHING`, id, userID, role.ID(), env)
		for _, x := range extras {
			exec(`INSERT INTO assignment_extras (assignment_id, permission) VALUES (?, ?) ON CONFLICT DO NOTHING`, id, string(x))
		}
	}
	for _, a := range Archetypes() {
		if a == ArchetypeAnonymous {
			continue
		}
		g, id := GrantsOf(a), UserID(a)
		if g.OnA != rbac.RoleNone {
			assign(id, g.OnA, envA, g.Extras...)
		}
		if g.OnB != rbac.RoleNone {
			assign(id, g.OnB, envB)
		}
		if g.OnAll != rbac.RoleNone {
			assign(id, g.OnAll, "")
		}
	}
}

// SessionFor opens a fresh session for the user straight in the store and returns
// the cookie a browser would send and the CSRF token for its mutating requests.
// It makes a new session on every call, so a route that ends or revokes sessions
// cannot affect the next request.
func SessionFor(t testing.TB, srv *server.Server, userID string) (*http.Cookie, string) {
	t.Helper()
	ctx := context.Background()
	tx, err := srv.Store().Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	raw, idHash, err := srv.Sessions().CreateTx(ctx, tx, userID, false, "192.0.2.50", "servertest-archetype")
	if err != nil {
		t.Fatalf("servertest: open a session for %s: %v", userID, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	c := srv.Sessions().Cookie(raw, false, srv.Instance().CookieScope())
	return &c, srv.Sessions().CSRFToken(idHash)
}

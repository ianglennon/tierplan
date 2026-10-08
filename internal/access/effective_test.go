// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package access_test

import (
	"slices"
	"testing"

	"github.com/ianglennon/bayplan/internal/access"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// Effective access, the "why" view of the Access card (plan 03-19, D-06).

func sourceKinds(r access.EnvRow) []rbac.SourceKind {
	var out []rbac.SourceKind
	for _, s := range r.Sources {
		out = append(out, s.Kind)
	}
	return out
}

func TestEffectiveAccessOfADirectViewerGrantIsOneRowWithTheDirectSource(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addEnv(t, "env-a", "Alpha")
	f.addEnv(t, "env-b", "Beta")
	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-a", "viewer")); err != nil {
		t.Fatal(err)
	}
	admin, rows, err := f.svc.EffectiveAccess(f.ctx, "alice")
	if err != nil || admin {
		t.Fatalf("EffectiveAccess = %v, %v, want a plain user", admin, err)
	}
	if len(rows) != 1 || rows[0].EnvID != "env-a" || rows[0].EnvName != "Alpha" || rows[0].Role != "viewer" ||
		rows[0].Inherited || len(rows[0].Extras) != 0 || !slices.Equal(sourceKinds(rows[0]), []rbac.SourceKind{rbac.SourceDirect}) {
		t.Fatalf("rows = %+v, want one direct Viewer row on Alpha", rows)
	}
}

func TestEffectiveAccessOfAnInstanceAdminIsTheFlagAndNoRows(t *testing.T) {
	f := newOwnedFixture(t)
	f.addAdmin(t, "root")
	f.addEnv(t, "env-a", "Alpha")
	admin, rows, err := f.svc.EffectiveAccess(f.ctx, "root")
	if err != nil || !admin || len(rows) != 0 || rows == nil {
		t.Fatalf("EffectiveAccess = %v, %+v, %v, want the flag and an empty, non-nil list", admin, rows, err)
	}
}

func TestEffectiveAccessOfAUserWithNoGrantsOrWhoIsDisabledIsEmpty(t *testing.T) {
	f := newOwnedFixture(t)
	f.addUser(t, "alice")
	f.addEnv(t, "env-a", "Alpha")
	if _, rows, err := f.svc.EffectiveAccess(f.ctx, "alice"); err != nil || len(rows) != 0 || rows == nil {
		t.Fatalf("a user with no grants = %+v, %v, want an empty, non-nil list", rows, err)
	}
	if _, _, err := f.svc.PutGrant(f.ctx, testAdmin, envGrant("alice", "env-a", "admin")); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE users SET disabled = 1 WHERE id = 'alice'`)
	if _, rows, err := f.svc.EffectiveAccess(f.ctx, "alice"); err != nil || len(rows) != 0 {
		t.Fatalf("a disabled user = %+v, %v, want no access at all", rows, err)
	}
}

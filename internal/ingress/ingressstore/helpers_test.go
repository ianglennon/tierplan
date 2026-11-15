// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"log/slog"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
)

// fixedNow is the clock every test store runs on.
var fixedNow = time.Date(2026, time.November, 15, 12, 0, 0, 0, time.UTC)

// newTestStore returns a Store over a migrated database, a vault in a temporary directory and a
// fixed clock, with the raw database for assertions the Store does not offer.
func newTestStore(t *testing.T) (*Store, *store.Store, *vault.Vault) {
	t.Helper()
	st := storetest.New(t)
	v, err := vault.LoadOrCreate("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	return New(Deps{St: st, Vault: v, Now: func() time.Time { return fixedNow }}), st, v
}

// seedEnv inserts a standalone environment.
func seedEnv(t *testing.T, st *store.Store, id, name string) {
	t.Helper()
	if _, err := st.Write.Exec(
		`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type) VALUES (?, 'agent', ?, '', 1, 1, 1, 'standalone')`,
		id, name); err != nil {
		t.Fatalf("seed environment %s: %v", id, err)
	}
}

// seedStack inserts a standalone stack in an environment.
func seedStack(t *testing.T, st *store.Store, id, envID, name string) {
	t.Helper()
	if _, err := st.Write.Exec(
		`INSERT INTO stacks (id, env_id, name, target, created_by, created_at) VALUES (?, ?, ?, 'standalone', 'tester', 1)`,
		id, envID, name); err != nil {
		t.Fatalf("seed stack %s: %v", id, err)
	}
}

// configVersion reads an environment's ingress config_version, 0 when it has no row.
func configVersion(t *testing.T, st *store.Store, envID string) int64 {
	t.Helper()
	var v int64
	err := st.Read.QueryRow(`SELECT COALESCE((SELECT config_version FROM ingress_env WHERE env_id = ?), 0)`, envID).Scan(&v)
	if err != nil {
		t.Fatalf("read config_version: %v", err)
	}
	return v
}

// count returns the number of rows a query counts.
func count(t *testing.T, st *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := st.Read.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

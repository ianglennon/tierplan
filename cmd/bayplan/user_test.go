// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/internal/vault"
)

// lockedAdmin adds a user named admin who is locked out for another half hour.
func lockedAdmin(t *testing.T, st *store.Store) {
	t.Helper()
	n := time.Now().UnixNano()
	if err := st.Q.InsertUser(context.Background(), sqlc.InsertUserParams{
		ID: "u-admin", Username: "admin", PasswordHash: "x", CreatedAt: n, UpdatedAt: n, PasswordChangedAt: n,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write.Exec(`UPDATE users SET failed_count = 5, locked_until = ? WHERE id = 'u-admin'`, time.Now().Add(30*time.Minute).UnixNano()); err != nil {
		t.Fatal(err)
	}
}

func TestUserUnlockClearsTheLockAndRecordsSystemCLI(t *testing.T) {
	dir, st := seedDataDir(t, 1)
	lockedAdmin(t, st)

	out, errOut, err := runCLI(t, dir, "user", "unlock", "Admin")
	if err != nil || strings.TrimSpace(out) != "unlocked Admin" {
		t.Fatalf("unlock = %q, %q, %v", out, errOut, err)
	}
	u, err := st.RQ.GetUserByUsername(context.Background(), "admin")
	if err != nil || u.FailedCount != 0 || u.LockedUntil.Valid {
		t.Fatalf("user after unlock = %+v, %v", u, err)
	}
	var actor, target string
	if err := st.Read.QueryRow(`SELECT actor_id, target_id FROM audit_log WHERE action = 'auth.unlocked'`).Scan(&actor, &target); err != nil {
		t.Fatalf("no auth.unlocked entry: %v", err)
	}
	if actor != "system:cli" || target != "u-admin" {
		t.Fatalf("auth.unlocked actor %q target %q, want system:cli and u-admin", actor, target)
	}
	// The CLI signs with the same key, so the server's chain stays intact.
	if vout, _, err := runCLI(t, dir, "audit", "verify"); err != nil || strings.TrimSpace(vout) != "audit chain OK: 2 entries" {
		t.Fatalf("audit verify after unlock = %q, %v", vout, err)
	}
}

func TestUserUnlockOfAnUnknownUserExitsNonZeroAndSaysSo(t *testing.T) {
	dir, st := seedDataDir(t, 0)
	lockedAdmin(t, st)

	out, errOut, err := runCLI(t, dir, "user", "unlock", "nobody")
	if err == nil {
		t.Fatal("unlocking an unknown user exited 0")
	}
	if strings.TrimSpace(errOut) != "no user named nobody" || out != "" {
		t.Fatalf("stdout %q, stderr %q; want only 'no user named nobody' on stderr", out, errOut)
	}
	if u, _ := st.RQ.GetUserByUsername(context.Background(), "admin"); !u.LockedUntil.Valid {
		t.Fatal("unlocking an unknown user unlocked someone else")
	}
}

func TestUserUnlockNeedsAnArgumentAndNeverGeneratesAKey(t *testing.T) {
	dir, _ := seedDataDir(t, 0)
	if _, _, err := runCLI(t, dir, "user", "unlock"); err == nil {
		t.Fatal("unlock without a name exited 0")
	}

	other := t.TempDir() // a data directory with no key
	_, errOut, err := runCLI(t, other, "user", "unlock", "admin")
	if err == nil || !strings.Contains(errOut, "no encryption key") {
		t.Fatalf("unlock without a key = %v, stderr %q", err, errOut)
	}
	if matches, _ := bayplanKeyFiles(other); len(matches) != 0 {
		t.Fatalf("unlock created a key file: %v", matches)
	}
}

func TestUserUnlockWithoutADatabaseSaysSo(t *testing.T) {
	dir := t.TempDir()
	if _, err := vault.LoadOrCreate("", dir, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	_, errOut, err := runCLI(t, dir, "user", "unlock", "admin")
	if err == nil || !strings.Contains(errOut, "no database") {
		t.Fatalf("unlock without a database = %v, stderr %q", err, errOut)
	}
}

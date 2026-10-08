// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package main

import (
	"context"
	"log/slog"
	"regexp"
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

// --- user reset-password (plan 03-15, AUTH-04, D-22b) -----------------------------

// activeAdmin adds the only instance admin, with a password and two sessions.
func activeAdmin(t *testing.T, st *store.Store) {
	t.Helper()
	n := time.Now().UnixNano()
	if err := st.Q.InsertUser(context.Background(), sqlc.InsertUserParams{
		ID: "u-admin", Username: "admin", PasswordHash: "x", CreatedAt: n, UpdatedAt: n, PasswordChangedAt: n,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write.Exec(`UPDATE users SET instance_admin = 1 WHERE id = 'u-admin'`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"s1", "s2"} {
		if _, err := st.Write.Exec(`INSERT INTO sessions (id_hash, user_id, created_at, last_seen_at, remember, ip, user_agent, public_id)
			VALUES (?, 'u-admin', 1, 1, 0, '', '', ?)`, []byte(id), id); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUserResetPasswordPrintsOneLinkEndsSessionsAndRecordsSystemCLI(t *testing.T) {
	dir, st := seedDataDir(t, 0)
	activeAdmin(t, st)

	out, errOut, err := runCLI(t, dir, "user", "reset-password", "Admin")
	if err != nil {
		t.Fatalf("reset-password = %q, %q, %v", out, errOut, err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "/invite#token=") {
		t.Fatalf("stdout = %q, want one line holding /invite#token=", out)
	}
	m := regexp.MustCompile(`/invite#token=([A-Za-z0-9_-]{43})$`).FindStringSubmatch(lines[0])
	if m == nil {
		t.Fatalf("line %q does not end in a 43 character token", lines[0])
	}
	if strings.Contains(errOut, m[1]) {
		t.Fatalf("stderr repeats the token: %q", errOut)
	}
	var n int
	if err := st.Read.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = 'u-admin'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("sessions left = %d (%v), want none", n, err)
	}
	var kind string
	if err := st.Read.QueryRow(`SELECT kind FROM user_links WHERE user_id = 'u-admin'`).Scan(&kind); err != nil || kind != "reset" {
		t.Fatalf("link kind = %q (%v), want reset", kind, err)
	}
	var actor, target, details string
	if err := st.Read.QueryRow(`SELECT actor_id, target_id, details FROM audit_log WHERE action = 'user.password_reset_issued'`).Scan(&actor, &target, &details); err != nil {
		t.Fatalf("no user.password_reset_issued entry: %v", err)
	}
	if actor != "system:cli" || target != "u-admin" || !strings.Contains(details, `"ended":"2"`) || strings.Contains(details, m[1]) {
		t.Fatalf("audit actor %q target %q details %s", actor, target, details)
	}
	// The CLI signs with the same key, so the server's chain stays intact.
	if vout, _, err := runCLI(t, dir, "audit", "verify"); err != nil || strings.TrimSpace(vout) != "audit chain OK: 1 entries" {
		t.Fatalf("audit verify after reset = %q, %v", vout, err)
	}
}

func TestUserResetPasswordBuildsTheLinkOnTheStoredPublicURLAndBasePath(t *testing.T) {
	dir, st := seedDataDir(t, 0)
	activeAdmin(t, st)
	if _, err := st.Write.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('public_url', 'https://bayplan.example.test/base', 1)`); err != nil {
		t.Fatal(err)
	}
	out, errOut, err := runCLI(t, dir, "user", "reset-password", "admin")
	if err != nil || !strings.HasPrefix(strings.TrimSpace(out), "https://bayplan.example.test/base/invite#token=") {
		t.Fatalf("reset-password = %q, %q, %v; want a link on the stored public URL", out, errOut, err)
	}
	if strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Fatalf("stdout = %q, want exactly one line with a public URL", out)
	}
}

func TestUserResetPasswordWithoutAPublicURLPrintsTheFragmentFormAndSaysWhereToOpenIt(t *testing.T) {
	dir, st := seedDataDir(t, 0)
	activeAdmin(t, st)
	out, errOut, err := runCLI(t, dir, "user", "reset-password", "admin")
	if err != nil {
		t.Fatalf("reset-password = %q, %q, %v", out, errOut, err)
	}
	if !strings.Contains(out, "/invite#token=") || strings.Contains(out, "http") {
		t.Fatalf("stdout = %q, want the fragment form with no address", out)
	}
	if !strings.Contains(out+errOut, "Open it on your Bayplan address") {
		t.Fatalf("output %q / %q does not say where to open the link", out, errOut)
	}
}

func TestUserResetPasswordOfAnUnknownDeletedOrDisabledUserExitsNonZeroAndChangesNothing(t *testing.T) {
	dir, st := seedDataDir(t, 0)
	activeAdmin(t, st)
	n := time.Now().UnixNano()
	for _, name := range []string{"gone", "off"} {
		if err := st.Q.InsertUser(context.Background(), sqlc.InsertUserParams{
			ID: "u-" + name, Username: name, PasswordHash: "x", CreatedAt: n, UpdatedAt: n, PasswordChangedAt: n,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Write.Exec(`UPDATE users SET deleted_at = 5 WHERE id = 'u-gone'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write.Exec(`UPDATE users SET disabled = 1 WHERE id = 'u-off'`); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ name, say string }{
		{"nobody", "no user named nobody"},
		{"gone", "no user named gone"},
		{"off", "off is disabled"},
	} {
		out, errOut, err := runCLI(t, dir, "user", "reset-password", c.name)
		if err == nil {
			t.Fatalf("reset-password %s exited 0", c.name)
		}
		if out != "" || !strings.Contains(errOut, c.say) {
			t.Fatalf("%s: stdout %q, stderr %q; want only %q on stderr", c.name, out, errOut, c.say)
		}
	}
	var sessions, links, entries int
	if err := st.Read.QueryRow(`SELECT (SELECT COUNT(*) FROM sessions), (SELECT COUNT(*) FROM user_links), (SELECT COUNT(*) FROM audit_log WHERE action = 'user.password_reset_issued')`).Scan(&sessions, &links, &entries); err != nil {
		t.Fatal(err)
	}
	if sessions != 2 || links != 0 || entries != 0 {
		t.Fatalf("after refusals: sessions %d, links %d, entries %d; want 2, 0, 0", sessions, links, entries)
	}
}

func TestUserResetPasswordNeedsAnArgumentAndNeverGeneratesAKey(t *testing.T) {
	dir, _ := seedDataDir(t, 0)
	if _, _, err := runCLI(t, dir, "user", "reset-password"); err == nil {
		t.Fatal("reset-password without a name exited 0")
	}
	other := t.TempDir() // a data directory with no key
	_, errOut, err := runCLI(t, other, "user", "reset-password", "admin")
	if err == nil || !strings.Contains(errOut, "no encryption key") {
		t.Fatalf("reset-password without a key = %v, stderr %q", err, errOut)
	}
	if matches, _ := bayplanKeyFiles(other); len(matches) != 0 {
		t.Fatalf("reset-password created a key file: %v", matches)
	}
}

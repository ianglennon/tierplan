// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package main

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/instance"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/vault"
)

// httpsStore puts a public URL in the settings row, as Settings or the wizard would.
func httpsStore(t *testing.T, st *store.Store, url string) {
	t.Helper()
	if err := st.Q.UpsertSetting(context.Background(), sqlc.UpsertSettingParams{
		Key: instance.SettingPublicURL, Value: url, UpdatedAt: time.Now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
}

// httpsStored reads the public URL back; "" when none is stored.
func httpsStored(t *testing.T, st *store.Store) string {
	t.Helper()
	v, err := st.RQ.GetSetting(context.Background(), instance.SettingPublicURL)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// httpsAuditCount counts the settings.updated rows.
func httpsAuditCount(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.Read.QueryRow(`SELECT count(*) FROM audit_log WHERE action = 'settings.updated'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestHTTPSDisableFlipsTheStoredURLAndAuditsIt(t *testing.T) {
	dir, st := seedDataDir(t, 0)
	httpsStore(t, st, "https://tierplan.example.com:8443/tp")

	out, errOut, err := runCLI(t, dir, "https", "disable")
	if err != nil {
		t.Fatalf("https disable = %q, %q, %v", out, errOut, err)
	}
	for _, want := range []string{
		"changed public URL: https://tierplan.example.com:8443/tp -> http://tierplan.example.com:8443/tp",
		"the public URL is now http://tierplan.example.com:8443/tp",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout lacks %q:\n%s", want, out)
		}
	}
	if got := httpsStored(t, st); got != "http://tierplan.example.com:8443/tp" {
		t.Fatalf("stored public URL = %q", got)
	}

	var actor, action, target, details string
	if err := st.Read.QueryRow(`SELECT actor_id, action, target_id, details FROM audit_log WHERE action = 'settings.updated'`).
		Scan(&actor, &action, &target, &details); err != nil {
		t.Fatalf("no settings.updated entry: %v", err)
	}
	if actor != "system:cli" || target != "settings" {
		t.Fatalf("audit actor %q target %q, want system:cli and settings", actor, target)
	}
	for _, want := range []string{
		`"public_url_from":"https://tierplan.example.com:8443/tp"`,
		`"public_url_to":"http://tierplan.example.com:8443/tp"`,
		`"via":"cli"`,
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("audit details %s lack %s", details, want)
		}
	}
	if n := httpsAuditCount(t, st); n != 1 {
		t.Fatalf("%d settings.updated rows, want 1", n)
	}
	if vout, _, err := runCLI(t, dir, "audit", "verify"); err != nil || strings.TrimSpace(vout) != "audit chain OK: 1 entries" {
		t.Fatalf("audit verify after the change = %q, %v", vout, err)
	}
}

func TestHTTPSDisableLockedExitsOneAndNamesTheVariable(t *testing.T) {
	dir, st := seedDataDir(t, 0)
	httpsStore(t, st, "https://stored.example.com:8443")
	t.Setenv("TIERPLAN_PUBLIC_URL", "https://fixed.example.com")

	out, errOut, err := runCLI(t, dir, "https", "disable")
	if err == nil {
		t.Fatal("https disable under the TIERPLAN_PUBLIC_URL lock exited 0")
	}
	if !strings.Contains(errOut, "TIERPLAN_PUBLIC_URL") || !strings.Contains(errOut, "nothing was changed") || out != "" {
		t.Fatalf("stdout %q, stderr %q; want the variable and 'nothing was changed' on stderr only", out, errOut)
	}
	if got := httpsStored(t, st); got != "https://stored.example.com:8443" {
		t.Fatalf("stored public URL = %q, want it unchanged", got)
	}
	if n := httpsAuditCount(t, st); n != 0 {
		t.Fatalf("%d settings.updated rows, want 0", n)
	}
}

func TestHTTPSDisableOnAnEnvTLSInstallExitsOneAndChangesNothing(t *testing.T) {
	dir, st := seedDataDir(t, 0)
	httpsStore(t, st, "https://tierplan.example.com:8443/tp")
	t.Setenv("TIERPLAN_TLS_SELF_SIGNED", "true")

	out, errOut, err := runCLI(t, dir, "https", "disable")
	if err == nil {
		t.Fatal("https disable on an env TLS install exited 0")
	}
	for _, want := range []string{"TIERPLAN_TLS_SELF_SIGNED", "nothing was changed", "https://tierplan.example.com:8443/tp", "restart"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	if out != "" {
		t.Fatalf("stdout = %q, want it empty", out)
	}
	if got := httpsStored(t, st); got != "https://tierplan.example.com:8443/tp" {
		t.Fatalf("stored public URL = %q, want it unchanged", got)
	}
	if n := httpsAuditCount(t, st); n != 0 {
		t.Fatalf("%d settings.updated rows, want 0", n)
	}
}

func TestHTTPSDisableTwicePrintsNothingToChange(t *testing.T) {
	dir, st := seedDataDir(t, 0)
	httpsStore(t, st, "https://tierplan.example.com:8443")

	if _, errOut, err := runCLI(t, dir, "https", "disable"); err != nil {
		t.Fatalf("first run: %v, %q", err, errOut)
	}
	out, errOut, err := runCLI(t, dir, "https", "disable")
	if err != nil || errOut != "" {
		t.Fatalf("second run = %q, %q, %v; want exit 0", out, errOut, err)
	}
	if !strings.Contains(out, "nothing to change") || strings.Contains(out, "changed public URL") {
		t.Fatalf("second run stdout = %q, want 'nothing to change' only", out)
	}
	if n := httpsAuditCount(t, st); n != 1 {
		t.Fatalf("%d settings.updated rows after two runs, want 1", n)
	}
}

func TestHTTPSDisableWithoutAKeyNeverCreatesOne(t *testing.T) {
	dir, st := seedDataDir(t, 0)
	httpsStore(t, st, "https://tierplan.example.com")
	keyPath := filepath.Join(dir, vault.KeyFile)
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}

	out, errOut, err := runCLI(t, dir, "https", "disable")
	if err == nil {
		t.Fatal("https disable without a key exited 0")
	}
	if !strings.Contains(errOut, "TIERPLAN_ENCRYPTION_KEY") || out != "" {
		t.Fatalf("stdout %q, stderr %q; want the key variable named on stderr", out, errOut)
	}
	if _, err := os.Stat(keyPath); err == nil {
		t.Fatal("https disable generated a key file")
	}
	if got := httpsStored(t, st); got != "https://tierplan.example.com" {
		t.Fatalf("stored public URL = %q, want it unchanged", got)
	}
}

func TestHTTPSDisableWithoutADatabaseSaysWhereItLooked(t *testing.T) {
	dir := t.TempDir()
	// A key but no database: the command must not create one.
	if _, err := vault.LoadOrCreate("", dir, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}

	out, errOut, err := runCLI(t, dir, "https", "disable")
	if err == nil {
		t.Fatal("https disable without a database exited 0")
	}
	if !strings.Contains(errOut, filepath.Join(dir, store.DatabaseFile)) || out != "" {
		t.Fatalf("stdout %q, stderr %q; want the database path on stderr", out, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, store.DatabaseFile)); err == nil {
		t.Fatal("https disable created a database")
	}
}

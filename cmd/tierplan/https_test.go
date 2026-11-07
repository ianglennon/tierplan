// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/instance"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
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

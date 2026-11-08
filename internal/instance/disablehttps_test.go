// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package instance_test

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/instance"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
)

// disableFixture is a migrated store and an audit writer that signs with a key made
// in a temp directory, as the CLI's writer signs with the server's.
func disableFixture(t *testing.T) (*store.Store, *audit.Writer) {
	t.Helper()
	st := storetest.New(t)
	v, err := vault.LoadOrCreate("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return st, audit.NewWriter(st, v, slog.New(slog.DiscardHandler), time.Now)
}

func disableStore(t *testing.T, st *store.Store, url string) {
	t.Helper()
	if err := st.Q.UpsertSetting(context.Background(), sqlc.UpsertSettingParams{
		Key: instance.SettingPublicURL, Value: url, UpdatedAt: time.Now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
}

// disableStored is the public_url row, "" when there is none.
func disableStored(t *testing.T, st *store.Store) string {
	t.Helper()
	v, err := st.RQ.GetSetting(context.Background(), instance.SettingPublicURL)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// disableAuditCount counts the settings.updated rows.
func disableAuditCount(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.Read.QueryRow(`SELECT count(*) FROM audit_log WHERE action = 'settings.updated'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func disableNotes(res instance.DisableResult) string { return strings.Join(res.Notes, "\n") }

func TestDisableHTTPSUsesTheListenPortWhenTheStoredURLHasNone(t *testing.T) {
	for _, tc := range []struct{ listen, port string }{{":7480", "7480"}, {":8080", "8080"}, {"0.0.0.0:9000", "9000"}} {
		st, w := disableFixture(t)
		disableStore(t, st, "https://tierplan.example.com/p")

		res, err := instance.DisableHTTPS(context.Background(), st, config.Config{Listen: tc.listen}, w)
		if err != nil {
			t.Fatalf("listen %s: %v", tc.listen, err)
		}
		want := "http://tierplan.example.com:" + tc.port + "/p"
		if res.To != want || disableStored(t, st) != want {
			t.Fatalf("listen %s: To %q, stored %q, want %q", tc.listen, res.To, disableStored(t, st), want)
		}
		if notes := disableNotes(res); !strings.Contains(notes, "port "+tc.port+" is the port of TIERPLAN_LISTEN") {
			t.Fatalf("listen %s: the notes do not name TIERPLAN_LISTEN as the port's source:\n%s", tc.listen, notes)
		}
	}
}

func TestDisableHTTPSKeepsAnExplicitPortAndAnIPv6Host(t *testing.T) {
	st, w := disableFixture(t)
	disableStore(t, st, "https://[2001:db8::1]:8443")

	res, err := instance.DisableHTTPS(context.Background(), st, config.Config{Listen: ":7480"}, w)
	if err != nil {
		t.Fatal(err)
	}
	if got := disableStored(t, st); got != "http://[2001:db8::1]:8443" || res.To != got {
		t.Fatalf("stored %q, To %q, want http://[2001:db8::1]:8443", got, res.To)
	}
	notes := disableNotes(res)
	if !strings.Contains(notes, "port 8443 is the port in the stored URL") {
		t.Fatalf("the notes do not name the stored URL as the port's source:\n%s", notes)
	}
	// The correction hint keeps the IPv6 brackets, so it can be pasted as is.
	if !strings.Contains(notes, "TIERPLAN_PUBLIC_URL=http://[2001:db8::1]:<port>") {
		t.Fatalf("the notes do not show how to correct the port:\n%s", notes)
	}
}

func TestDisableHTTPSRefusesAListenPortOfZero(t *testing.T) {
	st, w := disableFixture(t)
	disableStore(t, st, "https://tierplan.example.com")

	_, err := instance.DisableHTTPS(context.Background(), st, config.Config{Listen: ":0"}, w)
	if err == nil || !strings.Contains(err.Error(), "TIERPLAN_LISTEN") {
		t.Fatalf("err = %v, want one naming TIERPLAN_LISTEN", err)
	}
	if got := disableStored(t, st); got != "https://tierplan.example.com" {
		t.Fatalf("stored = %q, want it unchanged", got)
	}
	if n := disableAuditCount(t, st); n != 0 {
		t.Fatalf("%d settings.updated rows, want 0", n)
	}
}

func TestDisableHTTPSTwiceSaysNothingToChangeTheSecondTime(t *testing.T) {
	st, w := disableFixture(t)
	disableStore(t, st, "https://tierplan.example.com:8443")
	cfg := config.Config{Listen: ":7480"}

	first, err := instance.DisableHTTPS(context.Background(), st, cfg, w)
	if err != nil || len(first.Changes) != 1 {
		t.Fatalf("first run = %+v, %v; want one change", first, err)
	}
	second, err := instance.DisableHTTPS(context.Background(), st, cfg, w)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Changes) != 0 || len(second.Notes) == 0 || !strings.HasPrefix(second.Notes[0], "nothing to change") {
		t.Fatalf("second run = %+v, want no changes and a note starting 'nothing to change'", second)
	}
	if n := disableAuditCount(t, st); n != 1 {
		t.Fatalf("%d settings.updated rows after two runs, want 1", n)
	}
}

func TestDisableHTTPSWithNoStoredURLSaysNothingToChange(t *testing.T) {
	st, w := disableFixture(t)

	res, err := instance.DisableHTTPS(context.Background(), st, config.Config{Listen: ":7480"}, w)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 0 || len(res.Notes) == 0 || !strings.HasPrefix(res.Notes[0], "nothing to change") {
		t.Fatalf("result = %+v, want no changes and a note starting 'nothing to change'", res)
	}
	if got := disableStored(t, st); got != "" {
		t.Fatalf("a public_url row appeared: %q", got)
	}
	if n := disableAuditCount(t, st); n != 0 {
		t.Fatalf("%d settings.updated rows, want 0", n)
	}
}

func TestDisableHTTPSLockedByEnvChangesNothing(t *testing.T) {
	for name, cfg := range map[string]config.Config{
		"locked":            {PublicURL: "https://tierplan.example.com", Listen: ":7480"},
		"locked and TLS on": {PublicURL: "https://tierplan.example.com", Listen: ":7480", TLSSelfSigned: true},
	} {
		t.Run(name, func(t *testing.T) {
			st, w := disableFixture(t)
			disableStore(t, st, "https://stored.example.com")

			res, err := instance.DisableHTTPS(context.Background(), st, cfg, w)
			// The lock is checked first (D-18 order): it wins over an env TLS mode.
			if !errors.Is(err, instance.ErrPublicURLLocked) || errors.Is(err, instance.ErrTLSSetByEnv) {
				t.Fatalf("err = %v, want ErrPublicURLLocked", err)
			}
			if len(res.Changes) != 0 {
				t.Fatalf("changes = %v, want none", res.Changes)
			}
			if got := disableStored(t, st); got != "https://stored.example.com" {
				t.Fatalf("stored = %q, want it unchanged", got)
			}
			if n := disableAuditCount(t, st); n != 0 {
				t.Fatalf("%d settings.updated rows, want 0", n)
			}
		})
	}
}

func TestDisableHTTPSRefusesOnAnEnvTLSInstall(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    config.Config
		stored string
		want   []string
	}{
		{
			name:   "self-signed with a stored https URL",
			cfg:    config.Config{Listen: ":7480", TLSSelfSigned: true},
			stored: "https://tierplan.example.com:8443/tp",
			want: []string{
				"TIERPLAN_TLS_SELF_SIGNED", "nothing was changed",
				"The way in is https://tierplan.example.com:8443/tp", "restart",
			},
		},
		{
			name:   "certificate files",
			cfg:    config.Config{Listen: ":7480", TLSCertFile: "/c.pem", TLSKeyFile: "/k.pem"},
			stored: "https://tierplan.example.com",
			want:   []string{"TIERPLAN_TLS_CERT_FILE", "nothing was changed", "The way in is https://tierplan.example.com."},
		},
		{
			name:   "a stored http URL",
			cfg:    config.Config{Listen: ":7480", TLSSelfSigned: true},
			stored: "http://tierplan.example.com:7480",
			want: []string{
				"The way in is https://tierplan.example.com:7480.",
				"TIERPLAN_PUBLIC_URL=https://tierplan.example.com:7480 on the container",
			},
		},
		{
			name: "nothing stored",
			cfg:  config.Config{Listen: ":7480", TLSSelfSigned: true},
			want: []string{"The way in is https://<host>:7480."},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, w := disableFixture(t)
			if tc.stored != "" {
				disableStore(t, st, tc.stored)
			}

			res, err := instance.DisableHTTPS(context.Background(), st, tc.cfg, w)
			if !errors.Is(err, instance.ErrTLSSetByEnv) {
				t.Fatalf("err = %v, want ErrTLSSetByEnv", err)
			}
			if len(res.Changes) != 0 {
				t.Fatalf("changes = %v, want none", res.Changes)
			}
			notes := disableNotes(res)
			for _, want := range tc.want {
				if !strings.Contains(notes, want) {
					t.Fatalf("the notes lack %q:\n%s", want, notes)
				}
			}
			// Only an http stored URL needs the extra line about the public URL.
			hasURLLine := strings.Contains(notes, "sign-in over https:// is refused")
			if hasURLLine != strings.HasPrefix(tc.stored, "http://") {
				t.Fatalf("public URL line present = %v for stored %q:\n%s", hasURLLine, tc.stored, notes)
			}
			if got := disableStored(t, st); got != tc.stored {
				t.Fatalf("stored = %q, want it unchanged (%q)", got, tc.stored)
			}
			if n := disableAuditCount(t, st); n != 0 {
				t.Fatalf("%d settings.updated rows, want 0", n)
			}
		})
	}
}

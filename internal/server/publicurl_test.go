// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/ianglennon/tierplan/internal/backup"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/instance"
	"github.com/ianglennon/tierplan/internal/store"
)

func loadConfig(env map[string]string) (config.Config, error) {
	fl := pflag.NewFlagSet("test", pflag.ContinueOnError)
	config.BindFlags(fl)
	return config.Load(fl, func(k string) string { return env[k] }, os.ReadFile)
}

func TestAnHTTPPublicURLVariableStopsStartup(t *testing.T) {
	for _, name := range []string{"TIERPLAN_PUBLIC_URL", "TIERPLAN_AGENT_URL"} {
		_, err := loadConfig(map[string]string{name: "http://tierplan.test"})
		if err == nil {
			t.Fatalf("%s=http://tierplan.test started", name)
		}
		if !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "serves HTTPS only") {
			t.Errorf("%s: error = %v, want the variable name and the HTTPS-only reason", name, err)
		}
	}
}

func TestAnHTTPSPublicURLVariableStarts(t *testing.T) {
	cfg := testConfig(t, map[string]string{
		"TIERPLAN_DATA_DIR":   t.TempDir() + "/data",
		"TIERPLAN_PUBLIC_URL": "https://tierplan.test",
		"TIERPLAN_AGENT_URL":  "https://agents.tierplan.test",
	})
	if cfg.PublicURL != "https://tierplan.test" || cfg.AgentURL != "https://agents.tierplan.test" {
		t.Fatalf("urls = %q %q", cfg.PublicURL, cfg.AgentURL)
	}
	srv, err := Build(context.Background(), cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("Build with https URLs: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
}

// storeSettings starts a fresh data directory once so its database exists, then writes the
// given settings straight into it, as an upgraded plain-HTTP install or a restored backup
// would hold them. It returns the data directory.
func storeSettings(t *testing.T, settings map[string]string) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir() + "/data"
	first, err := Build(ctx, testConfig(t, map[string]string{"TIERPLAN_DATA_DIR": dir}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, dir, settings)
	return dir
}

func writeSettings(t *testing.T, dir string, settings map[string]string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range settings {
		if _, err := st.Write.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, 1)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAStoredHTTPPublicURLStopsStartup(t *testing.T) {
	dir := storeSettings(t, map[string]string{instance.SettingPublicURL: "http://old.example"})
	srv, err := Build(context.Background(), testConfig(t, map[string]string{"TIERPLAN_DATA_DIR": dir}), slog.New(slog.DiscardHandler))
	if err == nil {
		_ = srv.Close()
		t.Fatal("Build started with a stored http public URL")
	}
	for _, want := range []string{"http://old.example", "TierPlan serves HTTPS only", "TIERPLAN_PUBLIC_URL", "docs/upgrade.md", "Upgrading from a plain-HTTP install"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
}

func TestAStoredHTTPAgentURLStopsStartup(t *testing.T) {
	dir := storeSettings(t, map[string]string{instance.SettingAgentURL: "http://agents.example"})
	srv, err := Build(context.Background(), testConfig(t, map[string]string{"TIERPLAN_DATA_DIR": dir}), slog.New(slog.DiscardHandler))
	if err == nil {
		_ = srv.Close()
		t.Fatal("Build started with a stored http agent URL")
	}
	for _, want := range []string{"http://agents.example", "TIERPLAN_AGENT_URL", "docs/upgrade.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
}

func TestTheVariableWinsOverAStoredHTTPURL(t *testing.T) {
	dir := storeSettings(t, map[string]string{instance.SettingPublicURL: "http://old.example"})
	var logs bytes.Buffer
	srv, err := Build(context.Background(), testConfig(t, map[string]string{
		"TIERPLAN_DATA_DIR":   dir,
		"TIERPLAN_PUBLIC_URL": "https://new.example",
	}), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("Build with the variable set over a stored http URL: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	if n := strings.Count(logs.String(), "is ignored while TIERPLAN_PUBLIC_URL is set"); n != 1 {
		t.Errorf("%d warnings say the stored http value is ignored, want exactly one:\n%s", n, logs.String())
	}
}

func TestARestoredHTTPURLRollsTheRestoreBack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir() + "/data"
	old, err := Build(ctx, testConfig(t, map[string]string{"TIERPLAN_DATA_DIR": dir}), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	seedAdmin(t, old, "oldadmin")
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	snapshot, err := backup.PreRestoreSnapshot(ctx, dir, now)
	if err != nil {
		t.Fatal(err)
	}
	// The restored database holds an http public URL, and a setting the old one lacks.
	writeSettings(t, dir, map[string]string{
		instance.SettingPublicURL: "http://old.example",
		"from_the_restore":        "yes",
	})
	if err := backup.WriteMarker(dir, snapshot, now); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	srv, err := Build(ctx, testConfig(t, map[string]string{"TIERPLAN_DATA_DIR": dir}), slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("Build with a restore pending whose settings hold an http URL: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	// The pre-restore database is back.
	var n int
	if err := srv.Store().Read.QueryRow(`SELECT COUNT(*) FROM users WHERE username = 'oldadmin'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the old admin is missing after the rollback: %d, %v", n, err)
	}
	if err := srv.Store().Read.QueryRow(`SELECT COUNT(*) FROM settings WHERE key IN ('from_the_restore', 'public_url')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the restored settings are still in use: %d, %v", n, err)
	}
	if _, pending, _ := backup.ReadMarker(dir); pending {
		t.Error("the marker survived the rollback")
	}
	if !strings.Contains(logs.String(), "going back to the pre-restore snapshot") {
		t.Error("the rollback was not logged")
	}
}

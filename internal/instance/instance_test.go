// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package instance_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/instance"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/storetest"
)

func load(t *testing.T, cfg config.Config) (*instance.Instance, *store.Store) {
	t.Helper()
	st := storetest.New(t)
	i, err := instance.Load(context.Background(), st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return i, st
}

func set(t *testing.T, i *instance.Instance, st *store.Store, name, url string) error {
	t.Helper()
	tx, err := st.Write.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := i.SetTx(context.Background(), tx, st.Q, name, url); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return nil
}

func TestLoadOnAFreshStoreIsEmptyAndUnlocked(t *testing.T) {
	i, _ := load(t, config.Config{})
	if i.Name() != "" || i.PublicURL() != "" || i.PublicOrigin() != "" || i.Secure() || i.Locked() {
		t.Fatalf("fresh instance = %q %q %q secure=%v locked=%v", i.Name(), i.PublicURL(), i.PublicOrigin(), i.Secure(), i.Locked())
	}
}

func TestSetTxPersistsAndLoadReadsItBack(t *testing.T) {
	i, st := load(t, config.Config{})
	if err := set(t, i, st, "Home lab", "https://tierplan.example/ui"); err != nil {
		t.Fatal(err)
	}
	i.Apply("Home lab", "https://tierplan.example/ui")
	if i.Name() != "Home lab" || i.PublicURL() != "https://tierplan.example/ui" {
		t.Fatalf("after Apply: %q %q", i.Name(), i.PublicURL())
	}
	if i.PublicOrigin() != "https://tierplan.example" || !i.Secure() {
		t.Fatalf("origin %q secure %v, want https://tierplan.example true", i.PublicOrigin(), i.Secure())
	}

	again, err := instance.Load(context.Background(), st, config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Name() != "Home lab" || again.PublicURL() != "https://tierplan.example/ui" {
		t.Fatalf("reloaded: %q %q", again.Name(), again.PublicURL())
	}
}

func TestSetTxChangesAreInvisibleUntilAppliedAndLostOnRollback(t *testing.T) {
	i, st := load(t, config.Config{})
	tx, err := st.Write.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := i.SetTx(context.Background(), tx, st.Q, "Lab", "https://127.0.0.1:7480"); err != nil {
		t.Fatal(err)
	}
	if i.Name() != "" || i.PublicURL() != "" {
		t.Fatal("SetTx changed the cache before the transaction committed")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	again, err := instance.Load(context.Background(), st, config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Name() != "" || again.PublicURL() != "" {
		t.Fatalf("a rolled-back SetTx was stored: %q %q", again.Name(), again.PublicURL())
	}
}

func TestPlainHTTPURLIsNotSecureAndDefaultPortsDropFromTheOrigin(t *testing.T) {
	i, _ := load(t, config.Config{})
	i.Apply("x", "http://TierPlan.Example:80")
	if i.Secure() || i.PublicOrigin() != "http://tierplan.example" {
		t.Fatalf("origin %q secure %v", i.PublicOrigin(), i.Secure())
	}
}

func TestEnvironmentURLWinsAndLocksTheInstance(t *testing.T) {
	i, st := load(t, config.Config{PublicURL: "https://env.example"})
	if !i.Locked() || i.PublicURL() != "https://env.example" || !i.Secure() {
		t.Fatalf("locked=%v url=%q secure=%v", i.Locked(), i.PublicURL(), i.Secure())
	}
	// A stored value never overrides the environment.
	if err := set(t, i, st, "Lab", "https://env.example"); err != nil {
		t.Fatal(err)
	}
	again, err := instance.Load(context.Background(), st, config.Config{PublicURL: "https://env.example"})
	if err != nil {
		t.Fatal(err)
	}
	if again.PublicURL() != "https://env.example" || again.Name() != "Lab" {
		t.Fatalf("reloaded: %q %q", again.PublicURL(), again.Name())
	}
}

func TestSetTxRefusesToChangeALockedURLButStillStoresTheName(t *testing.T) {
	i, st := load(t, config.Config{PublicURL: "https://env.example"})
	if err := set(t, i, st, "Lab", "https://attacker.example"); !errors.Is(err, instance.ErrPublicURLLocked) {
		t.Fatalf("SetTx with a different URL while locked = %v, want ErrPublicURLLocked", err)
	}
	if err := set(t, i, st, "Lab", "https://env.example"); err != nil {
		t.Fatalf("SetTx with the locked URL itself: %v", err)
	}
	i.Apply("Lab", "https://attacker.example")
	if i.PublicURL() != "https://env.example" {
		t.Fatalf("Apply changed a locked URL to %q", i.PublicURL())
	}
	if i.Name() != "Lab" {
		t.Fatalf("name = %q, want Lab", i.Name())
	}
}

func TestStoredURLIsUsedWhenTheEnvironmentIsUnset(t *testing.T) {
	i, st := load(t, config.Config{})
	if err := set(t, i, st, "Lab", "https://10.0.0.5:7480"); err != nil {
		t.Fatal(err)
	}
	again, err := instance.Load(context.Background(), st, config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Locked() || again.PublicURL() != "https://10.0.0.5:7480" || again.PublicOrigin() != "https://10.0.0.5:7480" {
		t.Fatalf("locked=%v url=%q origin=%q", again.Locked(), again.PublicURL(), again.PublicOrigin())
	}
}

func TestBasePathIsThePathOfThePublicURL(t *testing.T) {
	cases := []struct{ url, want string }{
		{"", ""},
		{"https://example.com", ""},
		{"https://example.com/tierplan", "/tierplan"},
		{"https://example.com/tools/tierplan", "/tools/tierplan"},
		{"http://localhost:8080", ""},
		{"https://example.com/a%20b", "/a%20b"},
	}
	for _, tc := range cases {
		i, _ := load(t, config.Config{PublicURL: tc.url})
		if got := i.BasePath(); got != tc.want {
			t.Errorf("BasePath for %q = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestBasePathFollowsApply(t *testing.T) {
	i, _ := load(t, config.Config{})
	i.Apply("x", "https://example.com/tierplan")
	if got := i.BasePath(); got != "/tierplan" {
		t.Fatalf("after Apply, BasePath = %q", got)
	}
	i.Apply("x", "")
	if got := i.BasePath(); got != "" {
		t.Fatalf("after clearing the URL, BasePath = %q", got)
	}
}

func setAgentURL(t *testing.T, i *instance.Instance, st *store.Store, url string) error {
	t.Helper()
	tx, err := st.Write.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := i.SetAgentURLTx(context.Background(), tx, st.Q, url); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return nil
}

func TestAgentEndpointFollowsThePublicURLAndItsBasePath(t *testing.T) {
	for _, tc := range []struct{ public, want string }{
		{"", ""},
		{"https://example.com", "wss://example.com/agent/connect"},
		{"https://example.com/tierplan", "wss://example.com/tierplan/agent/connect"},
		{"http://10.0.0.5:7480", ""},
	} {
		i, _ := load(t, config.Config{})
		i.Apply("x", tc.public)
		if got := i.AgentEndpoint(); got != tc.want {
			t.Errorf("AgentEndpoint for public URL %q = %q, want %q", tc.public, got, tc.want)
		}
		if i.AgentURL() != "" || i.AgentURLLocked() {
			t.Errorf("a fresh instance has agent URL %q locked=%v", i.AgentURL(), i.AgentURLLocked())
		}
	}
}

func TestAgentURLReplacesThePublicURLForTheEndpoint(t *testing.T) {
	i, st := load(t, config.Config{})
	i.Apply("x", "https://example.com/tierplan")
	if err := setAgentURL(t, i, st, "https://tierplan.lan:7480"); err != nil {
		t.Fatal(err)
	}
	i.ApplyAgentURL("https://tierplan.lan:7480")
	if got, want := i.AgentEndpoint(), "wss://tierplan.lan:7480/agent/connect"; got != want {
		t.Errorf("AgentEndpoint = %q, want %q", got, want)
	}
	if got := i.AgentBase(); got != "https://tierplan.lan:7480" {
		t.Errorf("AgentBase = %q, want the agent URL", got)
	}
	// The browser-facing URL and base path are untouched.
	if i.PublicURL() != "https://example.com/tierplan" || i.BasePath() != "/tierplan" {
		t.Errorf("public URL %q base %q changed", i.PublicURL(), i.BasePath())
	}

	// With the agent URL cleared the endpoint is the public one again.
	if err := setAgentURL(t, i, st, ""); err != nil {
		t.Fatal(err)
	}
	i.ApplyAgentURL("")
	if got, want := i.AgentEndpoint(), "wss://example.com/tierplan/agent/connect"; got != want {
		t.Errorf("AgentEndpoint after clearing = %q, want %q", got, want)
	}
	if got := i.AgentBase(); got != "https://example.com/tierplan" {
		t.Errorf("AgentBase after clearing = %q, want the public URL", got)
	}
}

func TestAgentURLIsStoredAndReadBackButNotBeforeApply(t *testing.T) {
	i, st := load(t, config.Config{})
	if err := setAgentURL(t, i, st, "https://tierplan.lan:7480"); err != nil {
		t.Fatal(err)
	}
	if i.AgentURL() != "" {
		t.Fatal("SetAgentURLTx changed the cache before Apply")
	}
	var stored string
	if err := st.Read.QueryRow(`SELECT value FROM settings WHERE key = 'agent_url'`).Scan(&stored); err != nil || stored != "https://tierplan.lan:7480" {
		t.Fatalf("stored = %q, %v; want the agent_url setting", stored, err)
	}
	again, err := instance.Load(context.Background(), st, config.Config{})
	if err != nil || again.AgentURL() != "https://tierplan.lan:7480" || again.AgentURLLocked() {
		t.Fatalf("reloaded: %q locked=%v, %v", again.AgentURL(), again.AgentURLLocked(), err)
	}
}

func TestEnvironmentAgentURLWinsLocksAndIsNeverCopiedIntoTheSettingsTable(t *testing.T) {
	cfg := config.Config{AgentURL: "https://env.internal:7480"}
	i, st := load(t, cfg)
	if !i.AgentURLLocked() || i.AgentURL() != "https://env.internal:7480" {
		t.Fatalf("locked=%v url=%q", i.AgentURLLocked(), i.AgentURL())
	}
	if i.Locked() {
		t.Error("an agent URL lock must not lock the public URL")
	}
	if err := setAgentURL(t, i, st, "https://attacker.example"); !errors.Is(err, instance.ErrAgentURLLocked) {
		t.Fatalf("SetAgentURLTx with a different URL while locked = %v, want ErrAgentURLLocked", err)
	}
	if err := setAgentURL(t, i, st, "https://env.internal:7480"); err != nil {
		t.Fatalf("SetAgentURLTx with the locked URL itself: %v", err)
	}
	i.ApplyAgentURL("https://attacker.example")
	if i.AgentURL() != "https://env.internal:7480" {
		t.Fatalf("ApplyAgentURL changed a locked URL to %q", i.AgentURL())
	}
	var n int
	if err := st.Read.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = 'agent_url'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d agent_url rows (%v): a locked value must not be copied into the table", n, err)
	}

	// Removing the variable later falls back to what was stored before.
	if err := setAgentURL(t, mustLoad(t, st, config.Config{}), st, "https://stored.example"); err != nil {
		t.Fatal(err)
	}
	again, err := instance.Load(context.Background(), st, cfg)
	if err != nil || again.AgentURL() != "https://env.internal:7480" {
		t.Fatalf("with the variable: %q, %v", again.AgentURL(), err)
	}
	unlocked, err := instance.Load(context.Background(), st, config.Config{})
	if err != nil || unlocked.AgentURL() != "https://stored.example" {
		t.Fatalf("without the variable: %q, %v", unlocked.AgentURL(), err)
	}
}

func mustLoad(t *testing.T, st *store.Store, cfg config.Config) *instance.Instance {
	t.Helper()
	i, err := instance.Load(context.Background(), st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func setShell(t *testing.T, i *instance.Instance, st *store.Store, idle, capN int) error {
	t.Helper()
	tx, err := st.Write.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := i.SetShellLimitsTx(context.Background(), tx, st.Q, idle, capN); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return nil
}

func TestShellLimitsAreThirtyMinutesAndTenShellsUntilStored(t *testing.T) {
	i, _ := load(t, config.Config{})
	if i.ShellIdle() != 30*time.Minute || i.ShellCap() != 10 {
		t.Fatalf("defaults = %v and %d, want 30m and 10", i.ShellIdle(), i.ShellCap())
	}
}

func TestShellLimitsPersistAndApplyOnlyAfterTheCommit(t *testing.T) {
	i, st := load(t, config.Config{})
	if err := setShell(t, i, st, 45, 3); err != nil {
		t.Fatal(err)
	}
	if i.ShellIdle() != 30*time.Minute || i.ShellCap() != 10 {
		t.Fatalf("before Apply = %v and %d, want the cache untouched", i.ShellIdle(), i.ShellCap())
	}
	i.ApplyShellLimits(45, 3)
	if i.ShellIdle() != 45*time.Minute || i.ShellCap() != 3 {
		t.Fatalf("after Apply = %v and %d, want 45m and 3", i.ShellIdle(), i.ShellCap())
	}
	again, err := instance.Load(context.Background(), st, config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if again.ShellIdle() != 45*time.Minute || again.ShellCap() != 3 {
		t.Fatalf("reloaded = %v and %d, want 45m and 3", again.ShellIdle(), again.ShellCap())
	}
}

func TestShellLimitsOutsideTheirRangeAreRefusedAndNeverCached(t *testing.T) {
	i, st := load(t, config.Config{})
	for _, c := range [][2]int{{4, 10}, {241, 10}, {30, 0}, {30, 51}} {
		if err := setShell(t, i, st, c[0], c[1]); err == nil {
			t.Errorf("limits %v were stored, want a refusal", c)
		}
	}
	i.ApplyShellLimits(4, 51)
	if i.ShellIdle() != 30*time.Minute || i.ShellCap() != 10 {
		t.Fatalf("after Apply of out-of-range values = %v and %d, want the defaults kept", i.ShellIdle(), i.ShellCap())
	}
}

func TestAStoredShellLimitOutsideItsRangeOrNotANumberLoadsAsTheDefault(t *testing.T) {
	st := storetest.New(t)
	for k, v := range map[string]string{instance.SettingShellIdleMinutes: "9999", instance.SettingShellMaxPerUser: "lots"} {
		if _, err := st.Write.Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, 0)`, k, v); err != nil {
			t.Fatal(err)
		}
	}
	i, err := instance.Load(context.Background(), st, config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if i.ShellIdle() != 30*time.Minute || i.ShellCap() != 10 {
		t.Fatalf("loaded = %v and %d, want the defaults", i.ShellIdle(), i.ShellCap())
	}
}

func setKept(t *testing.T, i *instance.Instance, st *store.Store, n int) error {
	t.Helper()
	tx, err := st.Write.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := i.SetStackRevisionsKeptTx(context.Background(), tx, st.Q, n); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return nil
}

func TestStackRevisionsKeptBounds(t *testing.T) {
	i, st := load(t, config.Config{})
	if i.StackRevisionsKept() != 20 {
		t.Fatalf("default = %d, want 20", i.StackRevisionsKept())
	}
	// The ends of the range are stored; the values just outside are refused and never cached.
	for _, n := range []int{5, 200} {
		if err := setKept(t, i, st, n); err != nil {
			t.Fatalf("%d was refused: %v", n, err)
		}
	}
	for _, n := range []int{4, 201, 0, -1} {
		if err := setKept(t, i, st, n); err == nil {
			t.Errorf("%d was stored, want a refusal", n)
		}
	}
	i.ApplyStackRevisionsKept(4)
	i.ApplyStackRevisionsKept(201)
	if i.StackRevisionsKept() != 20 {
		t.Fatalf("after Apply of out-of-range values = %d, want the default kept", i.StackRevisionsKept())
	}
}

func TestStackRevisionsKeptPersistsAndAppliesOnlyAfterTheCommit(t *testing.T) {
	i, st := load(t, config.Config{})
	if err := setKept(t, i, st, 7); err != nil {
		t.Fatal(err)
	}
	if i.StackRevisionsKept() != 20 {
		t.Fatalf("before Apply = %d, want the cache untouched", i.StackRevisionsKept())
	}
	i.ApplyStackRevisionsKept(7)
	if i.StackRevisionsKept() != 7 {
		t.Fatalf("after Apply = %d, want 7", i.StackRevisionsKept())
	}
	if again := mustLoad(t, st, config.Config{}); again.StackRevisionsKept() != 7 {
		t.Fatalf("reloaded = %d, want 7", again.StackRevisionsKept())
	}
}

func TestAStoredStackRevisionsKeptOutsideItsRangeOrNotANumberLoadsAsTheDefault(t *testing.T) {
	for _, v := range []string{"3", "9999", "many"} {
		st := storetest.New(t)
		if _, err := st.Write.Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, 0)`, instance.SettingStackRevisionsKept, v); err != nil {
			t.Fatal(err)
		}
		if i := mustLoad(t, st, config.Config{}); i.StackRevisionsKept() != 20 {
			t.Errorf("stored %q loaded as %d, want the default 20", v, i.StackRevisionsKept())
		}
	}
}

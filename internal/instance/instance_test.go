// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package instance_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/instance"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/storetest"
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
	if err := set(t, i, st, "Home lab", "https://bayplan.example/ui"); err != nil {
		t.Fatal(err)
	}
	i.Apply("Home lab", "https://bayplan.example/ui")
	if i.Name() != "Home lab" || i.PublicURL() != "https://bayplan.example/ui" {
		t.Fatalf("after Apply: %q %q", i.Name(), i.PublicURL())
	}
	if i.PublicOrigin() != "https://bayplan.example" || !i.Secure() {
		t.Fatalf("origin %q secure %v, want https://bayplan.example true", i.PublicOrigin(), i.Secure())
	}

	again, err := instance.Load(context.Background(), st, config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Name() != "Home lab" || again.PublicURL() != "https://bayplan.example/ui" {
		t.Fatalf("reloaded: %q %q", again.Name(), again.PublicURL())
	}
}

func TestSetTxChangesAreInvisibleUntilAppliedAndLostOnRollback(t *testing.T) {
	i, st := load(t, config.Config{})
	tx, err := st.Write.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := i.SetTx(context.Background(), tx, st.Q, "Lab", "http://127.0.0.1:7480"); err != nil {
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
	i.Apply("x", "http://Bayplan.Example:80")
	if i.Secure() || i.PublicOrigin() != "http://bayplan.example" {
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
	if err := set(t, i, st, "Lab", "http://10.0.0.5:7480"); err != nil {
		t.Fatal(err)
	}
	again, err := instance.Load(context.Background(), st, config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Locked() || again.PublicURL() != "http://10.0.0.5:7480" || again.PublicOrigin() != "http://10.0.0.5:7480" {
		t.Fatalf("locked=%v url=%q origin=%q", again.Locked(), again.PublicURL(), again.PublicOrigin())
	}
}

func TestBasePathIsThePathOfThePublicURL(t *testing.T) {
	cases := []struct{ url, want string }{
		{"", ""},
		{"https://example.com", ""},
		{"https://example.com/bayplan", "/bayplan"},
		{"https://example.com/tools/bayplan", "/tools/bayplan"},
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
	i.Apply("x", "https://example.com/bayplan")
	if got := i.BasePath(); got != "/bayplan" {
		t.Fatalf("after Apply, BasePath = %q", got)
	}
	i.Apply("x", "")
	if got := i.BasePath(); got != "" {
		t.Fatalf("after clearing the URL, BasePath = %q", got)
	}
}

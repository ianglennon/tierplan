// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package instance_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/instance"
)

func TestReloadNeverOverwritesALockedPublicURL(t *testing.T) {
	server, other := watchDataDir(t)
	inst := mustLoad(t, server, config.Config{
		PublicURL: "https://env.example.com",
		AgentURL:  "https://agents.env.example.com",
	})

	watchPut(t, other, instance.SettingPublicURL, "http://stored.example.com")
	watchPut(t, other, instance.SettingAgentURL, "https://agents.stored.example.com")

	changed, err := inst.Reload(context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Fatalf("changed = %v, want nothing: both URLs are fixed by the environment", changed)
	}
	if inst.PublicURL() != "https://env.example.com" || inst.AgentURL() != "https://agents.env.example.com" {
		t.Fatalf("locked values moved: public %q, agent %q", inst.PublicURL(), inst.AgentURL())
	}
}

func TestReloadLocksAreIndependent(t *testing.T) {
	server, other := watchDataDir(t)
	inst := mustLoad(t, server, config.Config{PublicURL: "https://env.example.com"})

	watchPut(t, other, instance.SettingPublicURL, "http://stored.example.com")
	watchPut(t, other, instance.SettingAgentURL, "https://agents.stored.example.com")

	changed, err := inst.Reload(context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{instance.SettingAgentURL}; !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed = %v, want %v", changed, want)
	}
	if inst.PublicURL() != "https://env.example.com" || inst.AgentURL() != "https://agents.stored.example.com" {
		t.Fatalf("public %q, agent %q", inst.PublicURL(), inst.AgentURL())
	}
}

func TestReloadReportsNothingWhenNothingChanged(t *testing.T) {
	server, other := watchDataDir(t)
	watchPut(t, other, instance.SettingName, "Home lab")
	inst := mustLoad(t, server, config.Config{})

	for range 2 {
		changed, err := inst.Reload(context.Background(), server)
		if err != nil {
			t.Fatal(err)
		}
		if len(changed) != 0 {
			t.Fatalf("changed = %v, want none", changed)
		}
	}
}

func TestReloadFillsAFreshInstance(t *testing.T) {
	server, other := watchDataDir(t)
	watchPut(t, other, instance.SettingName, "Home lab")
	var inst instance.Instance

	changed, err := inst.Reload(context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) == 0 || inst.Name() != "Home lab" {
		t.Fatalf("a zero Instance after Reload: changed %v, name %q", changed, inst.Name())
	}
}

func TestReloadAppliesNothingWhenAReadFails(t *testing.T) {
	server, other := watchDataDir(t)
	watchPut(t, other, instance.SettingName, "Home lab")
	watchPut(t, other, instance.SettingPublicURL, "https://tierplan.example.com")
	inst := mustLoad(t, server, config.Config{})
	if err := setShell(t, inst, server, 45, 7); err != nil {
		t.Fatal(err)
	}
	inst.ApplyShellLimits(45, 7)

	// The database moves on, then the server's handle goes away.
	watchPut(t, other, instance.SettingName, "Renamed")
	watchPut(t, other, instance.SettingPublicURL, "http://tierplan.example.com")
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}

	changed, err := inst.Reload(context.Background(), server)
	if err == nil {
		t.Fatalf("Reload on a closed store = %v, nil; want an error", changed)
	}
	if len(changed) != 0 {
		t.Fatalf("changed = %v on a failed read", changed)
	}
	if inst.Name() != "Home lab" || inst.PublicURL() != "https://tierplan.example.com" ||
		inst.ShellIdle() != 45*time.Minute || inst.ShellCap() != 7 {
		t.Fatalf("a failed Reload moved values: %q %q %v %d", inst.Name(), inst.PublicURL(), inst.ShellIdle(), inst.ShellCap())
	}
}

func TestReloadReadsAnOutOfRangeShellLimitAsTheDefault(t *testing.T) {
	server, other := watchDataDir(t)
	watchPut(t, other, instance.SettingShellIdleMinutes, "45")
	inst := mustLoad(t, server, config.Config{})
	if inst.ShellIdle() != 45*time.Minute {
		t.Fatalf("loaded ShellIdle = %v", inst.ShellIdle())
	}

	watchPut(t, other, instance.SettingShellIdleMinutes, "9999")
	changed, err := inst.Reload(context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{instance.SettingShellIdleMinutes}; !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed = %v, want %v", changed, want)
	}
	if inst.ShellIdle() != instance.DefaultShellIdleMinutes*time.Minute {
		t.Fatalf("ShellIdle = %v, want the %d minute default", inst.ShellIdle(), instance.DefaultShellIdleMinutes)
	}
}

func TestReloadReportsChangedKeysInAFixedOrder(t *testing.T) {
	server, other := watchDataDir(t)
	inst := mustLoad(t, server, config.Config{})

	// Written in the reverse of the reported order.
	watchPut(t, other, instance.SettingStackRevisionsKept, "50")
	watchPut(t, other, instance.SettingPublicURL, "https://tierplan.example.com")
	watchPut(t, other, instance.SettingName, "Home lab")

	changed, err := inst.Reload(context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{instance.SettingName, instance.SettingPublicURL, instance.SettingStackRevisionsKept}
	if !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed = %v, want %v", changed, want)
	}

	// Every key, in the documented order.
	watchPut(t, other, instance.SettingName, "Again")
	watchPut(t, other, instance.SettingPublicURL, "http://tierplan.example.com")
	watchPut(t, other, instance.SettingAgentURL, "https://agents.example.com")
	watchPut(t, other, instance.SettingShellIdleMinutes, "60")
	watchPut(t, other, instance.SettingShellMaxPerUser, "3")
	watchPut(t, other, instance.SettingStackRevisionsKept, "60")
	changed, err = inst.Reload(context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{
		instance.SettingName, instance.SettingPublicURL, instance.SettingAgentURL,
		instance.SettingShellIdleMinutes, instance.SettingShellMaxPerUser, instance.SettingStackRevisionsKept,
	}
	if !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed = %v, want %v", changed, want)
	}
}

func TestReloadConvergesAfterARacingApply(t *testing.T) {
	server, other := watchDataDir(t)
	watchPut(t, other, instance.SettingPublicURL, "https://old.example.com")
	inst := mustLoad(t, server, config.Config{})

	// The database holds the newer value; a slower writer then applies the stale one.
	watchPut(t, other, instance.SettingPublicURL, "https://new.example.com")
	inst.Apply("", "https://old.example.com")

	changed, err := inst.Reload(context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{instance.SettingPublicURL}; !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed = %v, want %v", changed, want)
	}
	if inst.PublicURL() != "https://new.example.com" {
		t.Fatalf("PublicURL = %q, want the database value", inst.PublicURL())
	}
}

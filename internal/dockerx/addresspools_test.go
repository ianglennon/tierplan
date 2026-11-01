// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package dockerx_test

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
)

// The address pools an engine reports (plan 10-01, NET-02, T-10-01). Docker is the fake
// daemon behind the real client.

func readPools(t *testing.T, cfg dockertest.FakeConfig) (dockerx.AddressPools, *dockertest.Fake) {
	t.Helper()
	fake := dockertest.NewFake(t, cfg)
	got, err := newClient(t, fake).AddressPools(context.Background())
	if err != nil {
		t.Fatalf("AddressPools: %v", err)
	}
	return got, fake
}

func TestAddressPoolsAManagerReportsItsSwarmPool(t *testing.T) {
	got, fake := readPools(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true, SwarmClusterID: "cluster-1",
		SwarmDefaultAddrPool: []string{"10.60.0.0/16", "10.70.0.0/16"}, SwarmSubnetSize: 26,
	})
	want := []netip.Prefix{netip.MustParsePrefix("10.60.0.0/16"), netip.MustParsePrefix("10.70.0.0/16")}
	if !got.SwarmKnown || !slices.Equal(got.SwarmPools, want) || got.SwarmSubnetSize != 26 {
		t.Fatalf("pools = %+v, want the swarm's two pools at /26", got)
	}
	if len(got.DaemonPools) != 0 {
		t.Errorf("daemon pools = %+v, want none: the field is absent when not configured", got.DaemonPools)
	}
	for _, r := range fake.Requests() {
		if strings.HasSuffix(r, "/swarm") {
			t.Errorf("request %q: the pools are read from /info, never from /swarm, whose answer carries the join tokens", r)
		}
	}
}

func TestAddressPoolsAManagerWithANullPoolReportsNoPoolsButKnowsTheSwarm(t *testing.T) {
	got, _ := readPools(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true, SwarmClusterID: "cluster-1", SwarmSubnetSize: 24,
	})
	if !got.SwarmKnown || len(got.SwarmPools) != 0 || got.SwarmSubnetSize != 24 {
		t.Fatalf("pools = %+v, want a known swarm with no pool and mask 24", got)
	}
}

func TestAddressPoolsAWorkerDoesNotKnowTheSwarm(t *testing.T) {
	got, _ := readPools(t, dockertest.FakeConfig{SwarmState: "active"})
	if got.SwarmKnown || len(got.SwarmPools) != 0 || got.SwarmSubnetSize != 0 {
		t.Fatalf("pools = %+v, want no swarm facts from a worker", got)
	}
}

func TestAddressPoolsAStandaloneEngineWithDaemonPools(t *testing.T) {
	got, _ := readPools(t, dockertest.FakeConfig{DefaultAddressPools: []dockertest.FakeAddressPool{
		{Base: "10.50.0.0/16", Size: 26}, {Base: "10.51.0.0/16", Size: 24},
	}})
	want := []dockerx.DaemonPool{
		{Base: netip.MustParsePrefix("10.50.0.0/16"), Size: 26},
		{Base: netip.MustParsePrefix("10.51.0.0/16"), Size: 24},
	}
	if got.SwarmKnown || !slices.Equal(got.DaemonPools, want) {
		t.Fatalf("pools = %+v, want the daemon's two pools in order", got)
	}
}

func TestAddressPoolsAStandaloneEngineWithoutDaemonPoolsReportsNone(t *testing.T) {
	got, _ := readPools(t, dockertest.FakeConfig{})
	if got.SwarmKnown || len(got.DaemonPools) != 0 || len(got.SwarmPools) != 0 {
		t.Fatalf("pools = %+v, want nothing: Docker's built-in defaults are the caller's to apply", got)
	}
}

func TestAddressPoolsAFailedInfoIsAnError(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{FailInfo: true})
	if _, err := newClient(t, fake).AddressPools(context.Background()); err == nil {
		t.Fatal("AddressPools answered although /info failed")
	}
}

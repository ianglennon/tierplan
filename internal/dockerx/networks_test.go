// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

func networkFake(t *testing.T) (*dockerx.Client, *dockertest.Fake) {
	t.Helper()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Networks: []dockertest.FakeNetwork{
		{ID: "n1", Name: "bridge", Driver: "bridge", Scope: "local", Subnets: []string{"172.17.0.0/16"}},
	}})
	return newClient(t, fake), fake
}

func TestNetworkCreateSendsTheSpecOnceAndReturnsTheID(t *testing.T) {
	c, fake := networkFake(t)
	id, warning, err := c.NetworkCreate(context.Background(), dockerx.NetworkCreateSpec{
		Name: "app-net", Driver: "bridge", Internal: true,
		Labels:  map[string]string{"team": "a"},
		Options: map[string]string{"com.docker.network.bridge.name": "br-app"},
		Subnet:  "10.20.0.0/24", Gateway: "10.20.0.1", IPRange: "10.20.0.128/25",
	})
	if err != nil || id == "" || warning != "" {
		t.Fatalf("NetworkCreate = %q, %q, %v", id, warning, err)
	}
	if got := fake.Writes(); !slices.Equal(got, []string{"POST /networks/create"}) {
		t.Fatalf("writes = %v, want one create and no retry", got)
	}
	got := fake.NetworkCreates()
	if len(got) != 1 {
		t.Fatalf("creates = %+v", got)
	}
	req := got[0]
	if req.Name != "app-net" || req.Driver != "bridge" || !req.Internal || req.Attachable || req.Labels["team"] != "a" ||
		req.Options["com.docker.network.bridge.name"] != "br-app" {
		t.Errorf("body = %+v", req)
	}
	if req.Ingress || req.ConfigOnly || req.ConfigFrom != nil {
		t.Errorf("a create must never be ingress, config-only or config-from: %+v", req)
	}
	if req.IPAM == nil || len(req.IPAM.Config) != 1 {
		t.Fatalf("IPAM = %+v", req.IPAM)
	}
	cfg := req.IPAM.Config[0]
	if cfg.Subnet.String() != "10.20.0.0/24" || cfg.Gateway.String() != "10.20.0.1" || cfg.IPRange.String() != "10.20.0.128/25" {
		t.Errorf("IPAM config = %+v", cfg)
	}
	if req.EnableIPv6 != nil {
		t.Errorf("EnableIPv6 = %v, want unset unless asked for", *req.EnableIPv6)
	}
}

func TestNetworkCreateSetsIPv6OnlyWhenAskedAndSendsNoIPAMWithoutAddressing(t *testing.T) {
	c, fake := networkFake(t)
	if _, _, err := c.NetworkCreate(context.Background(), dockerx.NetworkCreateSpec{Name: "v6", Driver: "bridge", IPv6: true}); err != nil {
		t.Fatal(err)
	}
	req := fake.NetworkCreates()[0]
	if req.EnableIPv6 == nil || !*req.EnableIPv6 {
		t.Errorf("EnableIPv6 = %v, want true", req.EnableIPv6)
	}
	if req.IPAM != nil {
		t.Errorf("IPAM = %+v, want none so the engine picks", req.IPAM)
	}
}

func TestNetworkCreateASwarmScopeOverlayIsAttachableWhenAsked(t *testing.T) {
	c, fake := networkFake(t)
	if _, _, err := c.NetworkCreate(context.Background(), dockerx.NetworkCreateSpec{Name: "ovl", Driver: "overlay", Scope: dockerx.ScopeSwarm, Attachable: true}); err != nil {
		t.Fatal(err)
	}
	req := fake.NetworkCreates()[0]
	if req.Driver != "overlay" || req.Scope != "swarm" || !req.Attachable {
		t.Errorf("body = %+v", req)
	}
}

func TestNetworkCreateRefusesADriverOutsideTheFourBeforeAnyCall(t *testing.T) {
	c, fake := networkFake(t)
	for _, driver := range []string{"", "host", "null", "ingress", "weave", "Bridge"} {
		_, _, err := c.NetworkCreate(context.Background(), dockerx.NetworkCreateSpec{Name: "x", Driver: driver})
		if !errors.Is(err, dockerx.ErrDriverNotAllowed) {
			t.Errorf("driver %q: err = %v, want ErrDriverNotAllowed", driver, err)
		}
	}
	if got := fake.Writes(); len(got) != 0 {
		t.Fatalf("a refused driver sent %v", got)
	}
}

func TestNetworkCreateRefusesUnparsableAddressingBeforeAnyCall(t *testing.T) {
	c, fake := networkFake(t)
	for _, spec := range []dockerx.NetworkCreateSpec{
		{Name: "x", Driver: "bridge", Subnet: "10.0.0.0"},
		{Name: "x", Driver: "bridge", Subnet: "10.0.0.0/24", Gateway: "not-an-ip"},
		{Name: "x", Driver: "bridge", Subnet: "10.0.0.0/24", IPRange: "10.0.0.5"},
	} {
		if _, _, err := c.NetworkCreate(context.Background(), spec); !errors.Is(err, dockerx.ErrAddressing) {
			t.Errorf("%+v: err = %v, want ErrAddressing", spec, err)
		}
	}
	if got := fake.Writes(); len(got) != 0 {
		t.Fatalf("a refused spec sent %v", got)
	}
}

func TestNetworkCreateATakenNameIsAConflictAndTheCreatedNetworkIsListed(t *testing.T) {
	c, _ := networkFake(t)
	ctx := context.Background()
	if _, _, err := c.NetworkCreate(ctx, dockerx.NetworkCreateSpec{Name: "app-net", Driver: "bridge", Subnet: "10.30.0.0/24"}); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.NetworkCreate(ctx, dockerx.NetworkCreateSpec{Name: "app-net", Driver: "bridge"})
	if err == nil || !dockerx.IsConflict(err) {
		t.Fatalf("err = %v, want a conflict", err)
	}
	snap, err := c.Snapshot(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, n := range snap.Networks {
		if n.Name == "app-net" && n.Driver == "bridge" && slices.Equal(n.Subnets, []string{"10.30.0.0/24"}) {
			found = true
		}
	}
	if !found {
		t.Fatalf("networks = %+v, want the created one", snap.Networks)
	}
}

func TestIsBuiltinNetworkNamesTheFiveAndAnythingWithTheIngressFlag(t *testing.T) {
	for _, name := range []string{"bridge", "host", "none", "ingress", "docker_gwbridge"} {
		if !dockerx.IsBuiltinNetwork(name, false) {
			t.Errorf("%s is not reported built in", name)
		}
	}
	if !dockerx.IsBuiltinNetwork("my-mesh", true) {
		t.Error("a network with the ingress flag is built in whatever it is called")
	}
	for _, name := range []string{"app-net", "bridge2", "Bridge", ""} {
		if dockerx.IsBuiltinNetwork(name, false) {
			t.Errorf("%q is reported built in", name)
		}
	}
}

func TestNetworkRemoveSendsOneDeleteAndTheNetworkLeavesTheList(t *testing.T) {
	c, fake := networkFake(t)
	id, _, err := c.NetworkCreate(context.Background(), dockerx.NetworkCreateSpec{Name: "app-net", Driver: "bridge"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.NetworkRemove(context.Background(), id); err != nil {
		t.Fatalf("NetworkRemove = %v", err)
	}
	if got := fake.Writes(); !slices.Equal(got, []string{"POST /networks/create", "DELETE /networks/" + id}) {
		t.Fatalf("writes = %v, want a create then one delete and no retry", got)
	}
	snap, err := c.Snapshot(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range snap.Networks {
		if n.ID == id {
			t.Fatalf("the removed network is still listed: %+v", snap.Networks)
		}
	}
	if err := c.NetworkRemove(context.Background(), id); !dockerx.IsNotFound(err) {
		t.Fatalf("a second remove = %v, want not found", err)
	}
}

func TestNetworkRemoveOfANetworkWithAContainerIsTheEnginesInUseRefusal(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		Networks: []dockertest.FakeNetwork{{ID: "n9", Name: "app-net", Driver: "bridge", Scope: "local"}},
		Containers: []dockertest.FakeContainer{{ID: "c1", Names: []string{"/web"}, State: "exited",
			Networks: map[string]dockertest.FakeEndpoint{"app-net": {NetworkID: "n9"}}}},
	})
	c := newClient(t, fake)
	err := c.NetworkRemove(context.Background(), "n9")
	if err == nil || !dockerx.IsNetworkInUse(err) {
		t.Fatalf("NetworkRemove = %v, want the in-use refusal", err)
	}
	if dockerx.IsNetworkInUse(errors.New("agent_policy_denied: DELETE /networks/n9")) {
		t.Error("a policy refusal is not an in-use refusal")
	}
	if dockerx.IsNetworkInUse(nil) {
		t.Error("no error is not an in-use refusal")
	}
}

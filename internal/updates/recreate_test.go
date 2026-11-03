// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"context"
	"encoding/json"
	"net/netip"
	"slices"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/updates"
)

const (
	oldID    = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	oldShort = "abcdef012345"
)

// oldContainer is what the engine documents for a busy plain container: a hostname that is the
// engine's default, an environment, a command, a healthcheck, a published port, a named volume, a
// bind and an anonymous volume, and two networks with aliases, a static address and a MAC address.
func oldContainer() container.InspectResponse {
	port := network.MustParsePort("8080/tcp")
	retries := 3
	mac := network.HardwareAddr{0x02, 0x42, 0x0a, 0x14, 0x00, 0x05}
	return container.InspectResponse{
		ID:   oldID,
		Name: "/web",
		Config: &container.Config{
			Hostname:     oldShort,
			User:         "app:app",
			Env:          []string{"MODE=prod", "TOKEN=abc"},
			Cmd:          []string{"serve", "--fast"},
			Entrypoint:   []string{"/entry.sh"},
			WorkingDir:   "/srv",
			Image:        "registry.example/app:1.0",
			Labels:       map[string]string{"app": "web", "tier": "front"},
			ExposedPorts: network.PortSet{port: {}},
			Volumes:      map[string]struct{}{"/data": {}, "/cache": {}},
			Healthcheck:  &container.HealthConfig{Test: []string{"CMD", "true"}, Retries: retries},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:   "front",
			Binds:         []string{"named:/named", "/srv/host:/hostdir:ro"},
			PortBindings:  network.PortMap{port: {{HostPort: "8081"}}},
			RestartPolicy: container.RestartPolicy{Name: "on-failure", MaximumRetryCount: 5},
		},
		Mounts: []container.MountPoint{
			{Type: mount.TypeVolume, Name: "named", Destination: "/named", RW: true},
			{Type: mount.TypeBind, Source: "/srv/host", Destination: "/hostdir"},
			{Type: mount.TypeVolume, Name: "anon0123", Destination: "/data", RW: true},
			{Type: mount.TypeVolume, Name: "anon4567", Destination: "/cache", RW: false},
		},
		NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
			"front": {
				NetworkID: "net-front-id", EndpointID: "ep1", Gateway: netip.MustParseAddr("10.20.0.1"),
				IPAddress: netip.MustParseAddr("10.20.0.5"), IPPrefixLen: 24, MacAddress: mac,
				Aliases:    []string{"web", oldShort, "frontend"},
				IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr("10.20.0.5")},
				DNSNames:   []string{"web", oldShort},
			},
			"back": {
				NetworkID: "net-back-id", EndpointID: "ep2", IPAddress: netip.MustParseAddr("10.30.0.7"), IPPrefixLen: 24,
				Aliases: []string{"web-db", oldShort}, DriverOpts: map[string]string{"k": "v"},
			},
		}},
	}
}

// fakeWithOld is a daemon that has oldContainer and its two networks.
func fakeWithOld(t *testing.T) (*dockerx.Client, *dockertest.Fake) {
	t.Helper()
	doc := oldContainer()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		Networks: []dockertest.FakeNetwork{
			{ID: "net-front-id", Name: "front", Driver: "bridge", Scope: "local"},
			{ID: "net-back-id", Name: "back", Driver: "bridge", Scope: "local"},
		},
		Containers: []dockertest.FakeContainer{{
			ID: oldID, Names: []string{"/web"}, Image: doc.Config.Image, State: "running", Status: "Up 1 hour",
			Labels:   doc.Config.Labels,
			Networks: map[string]dockertest.FakeEndpoint{"front": {NetworkID: "net-front-id"}, "back": {NetworkID: "net-back-id"}},
			Inspect:  dockertest.FakeInspect{Document: &doc},
		}},
	})
	c, err := dockerx.New(fake)
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, fake
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestRecreateOnFakeEngine is the tracer of the recreate: the replacement is created next to the
// running container from its inspect with only the image changed, its second network is
// connected, and the two names are swapped, in that order.
func TestRecreateOnFakeEngine(t *testing.T) {
	ctx := context.Background()
	c, fake := fakeWithOld(t)

	old, err := c.ContainerInspectFull(ctx, oldID)
	if err != nil {
		t.Fatalf("ContainerInspectFull: %v", err)
	}
	const image = "registry.example/app:2.0"
	cfg, host, first, more := updates.RecreateSpec(old, image)

	newName := "web-tierplan-new-" + oldShort
	newID, _, err := c.ContainerCreate(ctx, newName, cfg, host, first)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	for _, name := range slices.Sorted(mapKeys(more)) {
		if err := c.NetworkConnectEndpoint(ctx, name, newID, more[name]); err != nil {
			t.Fatalf("NetworkConnectEndpoint(%s): %v", name, err)
		}
	}
	if err := c.ContainerRename(ctx, oldID, "web-tierplan-old-"+oldShort); err != nil {
		t.Fatalf("rename old: %v", err)
	}
	if err := c.ContainerRename(ctx, newID, "web"); err != nil {
		t.Fatalf("rename new: %v", err)
	}

	creates := fake.ContainerCreates()
	if len(creates) != 1 || creates[0].Name != newName || creates[0].ID != newID {
		t.Fatalf("creates = %+v, want one under %q", creates, newName)
	}
	want := *old.Config
	want.Image = image
	want.Hostname = "" // the engine's default hostname is the old short ID; the replacement gets its own
	want.Volumes = nil // the anonymous volumes are attached by name instead
	got := *creates[0].Config
	got.Volumes = nil
	if asJSON(t, got) != asJSON(t, want) {
		t.Errorf("create config differs from the old one beyond the image:\n got  %s\n want %s", asJSON(t, got), asJSON(t, want))
	}
	if creates[0].HostConfig.RestartPolicy != old.HostConfig.RestartPolicy || !slices.Equal(creates[0].HostConfig.Binds, old.HostConfig.Binds) {
		t.Errorf("host config = %+v, want the old restart policy and binds", creates[0].HostConfig)
	}
	if conns := fake.NetworkConnects(); len(conns) != 1 || conns[0].Container != newID {
		t.Errorf("connects = %+v, want one for the replacement", conns)
	}
	wantRenames := []dockertest.FakeRename{
		{ID: oldID, From: "web", To: "web-tierplan-old-" + oldShort},
		{ID: newID, From: newName, To: "web"},
	}
	if got := fake.ContainerRenames(); !slices.Equal(got, wantRenames) {
		t.Errorf("renames = %+v, want %+v", got, wantRenames)
	}
	// Create, then the connect, then the two renames.
	wantWrites := []string{
		"POST /containers/create?name=" + newName,
		"POST /networks/back/connect",
		"POST /containers/" + oldID + "/rename?name=web-tierplan-old-" + oldShort,
		"POST /containers/" + newID + "/rename?name=web",
	}
	if got := fake.Writes(); !slices.Equal(got, wantWrites) {
		t.Errorf("writes = %v, want %v", got, wantWrites)
	}
	// The names are swapped as an inspect by name now sees them.
	if det, err := c.ContainerInspectFull(ctx, "web"); err != nil || det.ID != newID {
		t.Errorf("inspect web = %v, %v, want the replacement %s", det.ID, err, newID)
	}
}

func mapKeys[K comparable, V any](m map[K]V) func(yield func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

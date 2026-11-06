// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"context"
	"encoding/json"
	"net/netip"
	"reflect"
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
	mac := network.HardwareAddr{0x02, 0xaa, 0x0a, 0x14, 0x00, 0x05} // chosen by somebody: not the 02:42 + address the engine derives
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

// recreateCase is one row of the keep-list table: how the old container differs from oldContainer
// and what the replacement must then be.
type recreateCase struct {
	name   string
	change func(*container.InspectResponse)
	check  func(t *testing.T, old container.InspectResponse, cfg *container.Config, host *container.HostConfig, first *network.NetworkingConfig, more map[string]*network.EndpointSettings)
}

func onlyEndpoint(t *testing.T, first *network.NetworkingConfig) (string, *network.EndpointSettings) {
	t.Helper()
	if first == nil || len(first.EndpointsConfig) != 1 {
		t.Fatalf("first = %+v, want exactly one endpoint", first)
	}
	for n, es := range first.EndpointsConfig {
		return n, es
	}
	return "", nil
}

func TestRecreateSpecKeepsEverythingButImage(t *testing.T) {
	const image = "registry.example/app:2.0"
	cases := []recreateCase{
		{
			name: "only the image changes in the container's own settings",
			check: func(t *testing.T, old container.InspectResponse, cfg *container.Config, host *container.HostConfig, _ *network.NetworkingConfig, _ map[string]*network.EndpointSettings) {
				if cfg.Image != image {
					t.Errorf("image = %q, want %q", cfg.Image, image)
				}
				want := *old.Config
				want.Image, want.Hostname, want.Volumes = image, "", nil
				got := *cfg
				got.Volumes = nil
				if !reflect.DeepEqual(got, want) {
					t.Errorf("config = %+v, want the old one on the new image: %+v", got, want)
				}
				if !slices.Equal(cfg.Env, []string{"MODE=prod", "TOKEN=abc"}) || !slices.Equal(cfg.Cmd, []string{"serve", "--fast"}) ||
					!slices.Equal(cfg.Entrypoint, []string{"/entry.sh"}) || cfg.User != "app:app" || cfg.WorkingDir != "/srv" {
					t.Errorf("env, command, entrypoint, user or working directory lost: %+v", cfg)
				}
				if cfg.Labels["app"] != "web" || cfg.Labels["tier"] != "front" {
					t.Errorf("labels = %v", cfg.Labels)
				}
				if cfg.Healthcheck == nil || cfg.Healthcheck.Retries != 3 || !slices.Equal(cfg.Healthcheck.Test, []string{"CMD", "true"}) {
					t.Errorf("healthcheck = %+v", cfg.Healthcheck)
				}
				if host.RestartPolicy.Name != "on-failure" || host.RestartPolicy.MaximumRetryCount != 5 {
					t.Errorf("restart policy = %+v", host.RestartPolicy)
				}
				port := network.MustParsePort("8080/tcp")
				if b := host.PortBindings[port]; len(b) != 1 || b[0].HostPort != "8081" {
					t.Errorf("port bindings = %+v", host.PortBindings)
				}
				if _, ok := cfg.ExposedPorts[port]; !ok {
					t.Errorf("exposed ports = %+v", cfg.ExposedPorts)
				}
				if !slices.Equal(host.Binds, []string{"named:/named", "/srv/host:/hostdir:ro"}) {
					t.Errorf("binds = %v: a named volume and a bind mount are kept as they were", host.Binds)
				}
			},
		},
		{
			name: "an anonymous volume is attached again by its name and is not asked for as a new one",
			check: func(t *testing.T, _ container.InspectResponse, cfg *container.Config, host *container.HostConfig, _ *network.NetworkingConfig, _ map[string]*network.EndpointSettings) {
				want := []mount.Mount{
					{Type: mount.TypeVolume, Source: "anon0123", Target: "/data"},
					{Type: mount.TypeVolume, Source: "anon4567", Target: "/cache", ReadOnly: true},
				}
				got := slices.Clone(host.Mounts)
				slices.SortFunc(got, func(a, b mount.Mount) int { return compareStrings(a.Target, b.Target) })
				slices.SortFunc(want, func(a, b mount.Mount) int { return compareStrings(a.Target, b.Target) })
				if !reflect.DeepEqual(got, want) {
					t.Errorf("mounts = %+v, want %+v", got, want)
				}
				if len(cfg.Volumes) != 0 {
					t.Errorf("Config.Volumes = %v, want none: it would make the engine create new empty volumes", cfg.Volumes)
				}
			},
		},
		{
			name: "a volume that a mount or a bind already attaches is not attached twice",
			change: func(old *container.InspectResponse) {
				old.HostConfig.Mounts = []mount.Mount{{Type: mount.TypeVolume, Source: "anon0123", Target: "/data"}}
			},
			check: func(t *testing.T, _ container.InspectResponse, cfg *container.Config, host *container.HostConfig, _ *network.NetworkingConfig, _ map[string]*network.EndpointSettings) {
				n := 0
				for _, m := range host.Mounts {
					if m.Target == "/data" {
						n++
					}
				}
				if n != 1 {
					t.Errorf("/data is mounted %d times in %+v, want once", n, host.Mounts)
				}
			},
		},
		{
			name: "a volume that comes from another container stays with VolumesFrom",
			change: func(old *container.InspectResponse) {
				old.HostConfig.VolumesFrom = []string{"sidecar"}
				old.Mounts = append(old.Mounts, container.MountPoint{Type: mount.TypeVolume, Name: "theirs", Destination: "/shared", RW: true})
			},
			check: func(t *testing.T, _ container.InspectResponse, _ *container.Config, host *container.HostConfig, _ *network.NetworkingConfig, _ map[string]*network.EndpointSettings) {
				if !slices.Equal(host.VolumesFrom, []string{"sidecar"}) {
					t.Errorf("VolumesFrom = %v", host.VolumesFrom)
				}
				for _, m := range host.Mounts {
					if m.Target == "/shared" {
						t.Errorf("mounts = %+v: the sidecar's volume is not this container's to mount", host.Mounts)
					}
				}
			},
		},
		{
			name: "two networks: the one the network mode names is in the create, the other is connected, and what somebody chose is kept",
			check: func(t *testing.T, old container.InspectResponse, _ *container.Config, _ *container.HostConfig, first *network.NetworkingConfig, more map[string]*network.EndpointSettings) {
				name, front := onlyEndpoint(t, first)
				if name != "front" {
					t.Fatalf("first network = %q, want the network mode's front", name)
				}
				if len(more) != 1 || more["back"] == nil {
					t.Fatalf("more = %+v, want back only", more)
				}
				if !slices.Equal(front.Aliases, []string{"web", "frontend"}) {
					t.Errorf("front aliases = %v, want them without the old short ID %s", front.Aliases, oldShort)
				}
				if !slices.Equal(more["back"].Aliases, []string{"web-db"}) {
					t.Errorf("back aliases = %v", more["back"].Aliases)
				}
				if front.IPAMConfig == nil || front.IPAMConfig.IPv4Address != netip.MustParseAddr("10.20.0.5") {
					t.Errorf("static IPv4 = %+v, want 10.20.0.5", front.IPAMConfig)
				}
				if !slices.Equal(front.MacAddress, old.NetworkSettings.Networks["front"].MacAddress) || len(front.MacAddress) == 0 {
					t.Errorf("MAC address = %v, want the old one", front.MacAddress)
				}
				if more["back"].DriverOpts["k"] != "v" {
					t.Errorf("driver options = %v", more["back"].DriverOpts)
				}
				for n, es := range map[string]*network.EndpointSettings{"front": front, "back": more["back"]} {
					if es.IPAddress.IsValid() || es.Gateway.IsValid() || es.EndpointID != "" || es.NetworkID != "" || es.IPPrefixLen != 0 || len(es.DNSNames) != 0 {
						t.Errorf("%s keeps what the engine assigned: %+v", n, es)
					}
				}
			},
		},
		{
			name: "a MAC address the engine derived from the old address is left to be derived again, one somebody set is kept",
			change: func(old *container.InspectResponse) {
				old.NetworkSettings.Networks["back"].MacAddress = network.HardwareAddr{0x02, 0x42, 0x0a, 0x1e, 0x00, 0x07}
				old.NetworkSettings.Networks["back"].IPAddress = netip.MustParseAddr("10.30.0.7")
			},
			check: func(t *testing.T, old container.InspectResponse, _ *container.Config, _ *container.HostConfig, first *network.NetworkingConfig, more map[string]*network.EndpointSettings) {
				_, front := onlyEndpoint(t, first)
				if len(front.MacAddress) == 0 {
					t.Errorf("front MAC = %v, want the one somebody set", front.MacAddress)
				}
				if len(more["back"].MacAddress) != 0 {
					t.Errorf("back MAC = %v, want none: the engine derived it from the old address", more["back"].MacAddress)
				}
			},
		},
		{
			name: "the default network mode is the bridge",
			change: func(old *container.InspectResponse) {
				old.HostConfig.NetworkMode = "default"
				old.NetworkSettings.Networks = map[string]*network.EndpointSettings{
					"bridge": {NetworkID: "b", IPAddress: netip.MustParseAddr("172.17.0.2")},
					"alpha":  {NetworkID: "a"},
				}
			},
			check: func(t *testing.T, _ container.InspectResponse, _ *container.Config, _ *container.HostConfig, first *network.NetworkingConfig, more map[string]*network.EndpointSettings) {
				if name, _ := onlyEndpoint(t, first); name != "bridge" {
					t.Errorf("first network = %q, want bridge", name)
				}
				if len(more) != 1 || more["alpha"] == nil {
					t.Errorf("more = %+v, want alpha", more)
				}
			},
		},
		{
			name: "a network mode that names none of the networks falls back to the first by name",
			change: func(old *container.InspectResponse) {
				old.HostConfig.NetworkMode = "somethingelse"
			},
			check: func(t *testing.T, _ container.InspectResponse, _ *container.Config, _ *container.HostConfig, first *network.NetworkingConfig, _ map[string]*network.EndpointSettings) {
				if name, _ := onlyEndpoint(t, first); name != "back" {
					t.Errorf("first network = %q, want back (first by name), so the choice is the same every time", name)
				}
			},
		},
		{
			name: "a hostname equal to the old short ID is reset",
			check: func(t *testing.T, _ container.InspectResponse, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ map[string]*network.EndpointSettings) {
				if cfg.Hostname != "" {
					t.Errorf("hostname = %q, want it reset", cfg.Hostname)
				}
			},
		},
		{
			name:   "a hostname somebody chose is kept",
			change: func(old *container.InspectResponse) { old.Config.Hostname = "web-1.internal" },
			check: func(t *testing.T, _ container.InspectResponse, cfg *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ map[string]*network.EndpointSettings) {
				if cfg.Hostname != "web-1.internal" {
					t.Errorf("hostname = %q, want web-1.internal", cfg.Hostname)
				}
			},
		},
		{
			name: "a container with no network and no host configuration still gets a document",
			change: func(old *container.InspectResponse) {
				old.NetworkSettings, old.HostConfig, old.Mounts = nil, nil, nil
			},
			check: func(t *testing.T, _ container.InspectResponse, cfg *container.Config, host *container.HostConfig, first *network.NetworkingConfig, more map[string]*network.EndpointSettings) {
				if cfg == nil || host == nil || cfg.Image != image {
					t.Fatalf("cfg = %+v, host = %+v", cfg, host)
				}
				if first != nil || len(more) != 0 {
					t.Errorf("first = %+v, more = %+v, want none", first, more)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := oldContainer()
			if tc.change != nil {
				tc.change(&old)
			}
			cfg, host, first, more := updates.RecreateSpec(old, image)
			tc.check(t, old, cfg, host, first, more)
		})
	}
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// TestRecreateSpecDoesNotMutateInput proves the old container's inspect is the same after the
// call as before it, and that changing what comes back does not reach into it.
func TestRecreateSpecDoesNotMutateInput(t *testing.T) {
	old := oldContainer()
	before := asJSON(t, old)
	cfg, host, first, more := updates.RecreateSpec(old, "registry.example/app:2.0")
	if !reflect.DeepEqual(old, oldContainer()) || asJSON(t, old) != before {
		t.Fatalf("the old inspect changed:\n got  %s\n want %s", asJSON(t, old), before)
	}
	// The parts RecreateSpec itself rewrites are copies: the caller can change them freely.
	cfg.Image = "other"
	cfg.Volumes["/extra"] = struct{}{}
	host.Mounts = append(host.Mounts, mount.Mount{Target: "/extra"})
	for _, es := range first.EndpointsConfig {
		es.Aliases = append(es.Aliases, "extra")
		es.IPAMConfig.IPv4Address = netip.MustParseAddr("10.20.0.99")
	}
	for _, es := range more {
		es.DriverOpts["x"] = "y"
	}
	if asJSON(t, old) != before {
		t.Errorf("changing the replacement's document changed the old inspect:\n got  %s\n want %s", asJSON(t, old), before)
	}
}

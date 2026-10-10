// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
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

// Service references (plan 04-13 task 2, D-41, RESEARCH Pattern 8): what a swarm service
// mentions, whatever its tasks are doing.

func refsManager() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		Networks: []dockertest.FakeNetwork{
			{ID: "ovl-id-1", Name: "ovl", Driver: "overlay", Scope: "swarm"},
			{ID: "ovl-id-2", Name: "other", Driver: "overlay", Scope: "swarm"},
		},
		ServiceRows: []dockertest.FakeService{
			{ID: "s2", Name: "web", Mode: "replicated", Running: 1, Desired: 1,
				Networks: []string{"ovl-id-1"},
				Mounts:   []dockertest.FakeServiceMount{{Type: "volume", Source: "data"}, {Type: "bind", Source: "/srv/x"}, {Type: "tmpfs"}}},
			// Named by name in the spec, and scaled to zero: it still mentions the network.
			{ID: "s1", Name: "idle", Mode: "replicated", Running: 0, Desired: 0, Networks: []string{"other"}},
			{ID: "s3", Name: "plain", Mode: "global", Running: 3, Desired: 3},
		},
	}
}

func TestServiceRefsListsEachServicesNetworksAndVolumeMountsByIDWhateverItsTasksDo(t *testing.T) {
	fake := dockertest.NewFake(t, refsManager())
	got, err := newClient(t, fake).ServiceRefs(context.Background())
	if err != nil {
		t.Fatalf("ServiceRefs: %v", err)
	}
	want := []dockerx.ServiceRef{
		{ID: "s1", Name: "idle", Networks: []string{"ovl-id-2"}, Volumes: []string{}},
		{ID: "s3", Name: "plain", Networks: []string{}, Volumes: []string{}},
		{ID: "s2", Name: "web", Networks: []string{"ovl-id-1"}, Volumes: []string{"data"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ServiceRefs = %+v, want %+v (sorted by name, names resolved to IDs, only volume mounts)", got, want)
	}
	if w := fake.Writes(); len(w) != 0 {
		t.Fatalf("a read wrote: %v", w)
	}
}

func TestClusterCarriesTheServiceRefsOfTheSameRead(t *testing.T) {
	got, err := newClient(t, dockertest.NewFake(t, refsManager())).Cluster(context.Background())
	if err != nil {
		t.Fatalf("Cluster: %v", err)
	}
	if len(got.ServiceRefs) != 3 || got.ServiceRefs[2].Name != "web" || got.ServiceRefs[2].Networks[0] != "ovl-id-1" {
		t.Fatalf("ServiceRefs = %+v", got.ServiceRefs)
	}
}

func TestNetworkRowsCarryLabelsAndFlags(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Networks: []dockertest.FakeNetwork{
		{ID: "n1", Name: "mesh", Driver: "overlay", Scope: "swarm", Ingress: true},
		{ID: "n2", Name: "app", Driver: "bridge", Scope: "local", Internal: true, Attachable: true,
			Labels: map[string]string{"com.docker.stack.namespace": "shop"}},
	}})
	snap, err := newClient(t, fake).Snapshot(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]dockerx.NetworkRow{}
	for _, n := range snap.Networks {
		by[n.Name] = n
	}
	if !by["mesh"].Ingress || by["mesh"].Internal {
		t.Errorf("mesh = %+v", by["mesh"])
	}
	if a := by["app"]; !a.Internal || !a.Attachable || a.Ingress || a.Labels["com.docker.stack.namespace"] != "shop" {
		t.Errorf("app = %+v", a)
	}
}

// Network detail (plan 04-14, D-42): one network read on its own, with its addressing,
// its options and, for a swarm-scoped network read on a manager, the services on it.

func detailManager() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		Networks: []dockertest.FakeNetwork{
			{ID: "ovl-id-1", Name: "ovl", Driver: "overlay", Scope: "swarm", Attachable: true,
				Subnets: []string{"10.0.1.0/24", "fd00:1::/64"}, Gateways: []string{"10.0.1.1"}, IPv6: true,
				Options: map[string]string{"com.docker.network.driver.overlay.vxlanid_list": "4097"},
				Labels:  map[string]string{"com.docker.stack.namespace": "shop"}},
			{ID: "br-id", Name: "bridge", Driver: "bridge", Scope: "local"},
		},
		ServiceRows: []dockertest.FakeService{
			{ID: "s1", Name: "shop_web", Mode: "replicated", Running: 2, Desired: 2, Networks: []string{"ovl-id-1"}},
			{ID: "s2", Name: "shop_idle", Mode: "replicated", Running: 0, Desired: 0, Networks: []string{"ovl"}},
			{ID: "s3", Name: "other", Mode: "replicated", Running: 1, Desired: 1},
		},
	}
}

func TestNetworkDetailReadsAddressingOptionsLabelsAndFlags(t *testing.T) {
	fake := dockertest.NewFake(t, detailManager())
	d, raw, err := newClient(t, fake).NetworkDetail(context.Background(), "ovl-id-1", false)
	if err != nil {
		t.Fatalf("NetworkDetail: %v", err)
	}
	if d.ID != "ovl-id-1" || d.Name != "ovl" || d.Driver != "overlay" || d.Scope != "swarm" || !d.Attachable || !d.IPv6 || d.Internal || d.Ingress {
		t.Errorf("detail = %+v", d)
	}
	if d.Options["com.docker.network.driver.overlay.vxlanid_list"] != "4097" || d.Labels["com.docker.stack.namespace"] != "shop" {
		t.Errorf("options %v, labels %v", d.Options, d.Labels)
	}
	want := []dockerx.IPAMRow{{Subnet: "10.0.1.0/24", Gateway: "10.0.1.1"}, {Subnet: "fd00:1::/64"}}
	if !reflect.DeepEqual(d.IPAM, want) {
		t.Errorf("IPAM = %+v, want %+v", d.IPAM, want)
	}
	if len(d.Services) != 0 {
		t.Errorf("services without verbose = %+v", d.Services)
	}
	if !json.Valid(raw) || !strings.Contains(string(raw), `"ovl"`) {
		t.Errorf("raw = %s, want the inspect document", raw)
	}
	if w := fake.Writes(); len(w) != 0 {
		t.Fatalf("a read wrote: %v", w)
	}
}

func TestNetworkDetailVerboseListsTheServicesOnASwarmNetworkWithTheirTaskCounts(t *testing.T) {
	d, _, err := newClient(t, dockertest.NewFake(t, detailManager())).NetworkDetail(context.Background(), "ovl-id-1", true)
	if err != nil {
		t.Fatalf("NetworkDetail: %v", err)
	}
	want := []dockerx.NetworkService{{Name: "shop_idle", Tasks: 0}, {Name: "shop_web", Tasks: 2}}
	if !reflect.DeepEqual(d.Services, want) {
		t.Errorf("services = %+v, want %+v (by name; an unreferenced service is not listed)", d.Services, want)
	}
}

func TestNetworkDetailOfAnUnknownNetworkIsNotFound(t *testing.T) {
	_, _, err := newClient(t, dockertest.NewFake(t, detailManager())).NetworkDetail(context.Background(), "nope", false)
	if !dockerx.IsNotFound(err) {
		t.Fatalf("err = %v, want not found", err)
	}
}

// Connecting a container to a network (plan 04-15 task 1, D-43, T-04-49): the endpoint
// body carries an alias and addresses and nothing else, once and never retried.

func connectFake(t *testing.T) (*dockerx.Client, *dockertest.Fake) {
	t.Helper()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		Networks: []dockertest.FakeNetwork{
			{ID: "app0", Name: "app-net", Driver: "bridge", Scope: "local", Subnets: []string{"10.20.0.0/24"}},
			{ID: "ovl0", Name: "ovl", Driver: "overlay", Scope: "swarm"},
		},
		Containers: []dockertest.FakeContainer{
			{ID: "c1", Names: []string{"/web"}, State: "running"},
			{ID: "c2", Names: []string{"/db"}, State: "running", Networks: map[string]dockertest.FakeEndpoint{"app-net": {NetworkID: "app0"}}},
		},
	})
	return newClient(t, fake), fake
}

func TestNetworkConnectSendsTheAliasAndAddressesOnceAndTheContainerIsOnTheNetwork(t *testing.T) {
	c, fake := connectFake(t)
	err := c.NetworkConnect(context.Background(), "app0", "c1", dockerx.EndpointSpec{Alias: "api", IPv4: "10.20.0.5", IPv6: "fd00:20::5"})
	if err != nil {
		t.Fatalf("NetworkConnect = %v", err)
	}
	if got := fake.Writes(); !slices.Equal(got, []string{"POST /networks/app0/connect"}) {
		t.Fatalf("writes = %v, want one connect and no retry", got)
	}
	got := fake.NetworkConnects()
	if len(got) != 1 || got[0].Container != "c1" || got[0].EndpointConfig == nil {
		t.Fatalf("connects = %+v", got)
	}
	ep := got[0].EndpointConfig
	if !slices.Equal(ep.Aliases, []string{"api"}) || ep.IPAMConfig == nil ||
		ep.IPAMConfig.IPv4Address.String() != "10.20.0.5" || ep.IPAMConfig.IPv6Address.String() != "fd00:20::5" {
		t.Errorf("endpoint = %+v", ep)
	}
	if len(ep.Links) != 0 || len(ep.DriverOpts) != 0 || ep.MacAddress.String() != "" {
		t.Errorf("an endpoint may carry no links, driver options or MAC: %+v", ep)
	}
	detail, _, err := c.ContainerDetail(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Networks) != 1 || detail.Networks[0].Name != "app-net" || detail.Networks[0].IPv4 != "10.20.0.5" ||
		!slices.Contains(detail.Networks[0].Aliases, "api") {
		t.Errorf("container networks = %+v, want app-net with the alias and address", detail.Networks)
	}
}

func TestNetworkConnectWithNothingToSayAboutTheEndpointSendsNoEndpointConfig(t *testing.T) {
	c, fake := connectFake(t)
	if err := c.NetworkConnect(context.Background(), "app0", "c1", dockerx.EndpointSpec{}); err != nil {
		t.Fatal(err)
	}
	if got := fake.NetworkConnects(); len(got) != 1 || got[0].EndpointConfig != nil {
		t.Errorf("connects = %+v, want no endpoint config", got)
	}
}

func TestNetworkConnectRefusesAnAddressOfTheWrongFamilyBeforeAnyCall(t *testing.T) {
	c, fake := connectFake(t)
	for name, ep := range map[string]dockerx.EndpointSpec{
		"text as IPv4":     {IPv4: "nope"},
		"an IPv6 as IPv4":  {IPv4: "fd00::1"},
		"an IPv4 as IPv6":  {IPv6: "10.0.0.1"},
		"a mapped as IPv6": {IPv6: "::ffff:10.0.0.1"},
		"a prefix as IPv4": {IPv4: "10.0.0.0/24"},
		"a padded as IPv4": {IPv4: " 10.0.0.1"},
		"text as IPv6":     {IPv6: "nope"},
	} {
		if err := c.NetworkConnect(context.Background(), "app0", "c1", ep); !errors.Is(err, dockerx.ErrAddressing) {
			t.Errorf("%s: err = %v, want ErrAddressing", name, err)
		}
	}
	if w := fake.Writes(); len(w) != 0 {
		t.Fatalf("a refused address reached the engine: %v", w)
	}
}

func TestNetworkConnectAContainerAlreadyOnItAnUnknownOneAndANonAttachableOverlayAreTheEnginesRefusals(t *testing.T) {
	c, _ := connectFake(t)
	ctx := context.Background()
	if err := c.NetworkConnect(ctx, "app0", "c2", dockerx.EndpointSpec{}); err == nil || !strings.Contains(err.Error(), "already exists in network") {
		t.Errorf("already connected: %v", err)
	}
	if err := c.NetworkConnect(ctx, "nope", "c1", dockerx.EndpointSpec{}); !dockerx.IsNotFound(err) {
		t.Errorf("unknown network: %v, want not found", err)
	}
	if err := c.NetworkConnect(ctx, "app0", "ghost", dockerx.EndpointSpec{}); !dockerx.IsNotFound(err) {
		t.Errorf("unknown container: %v, want not found", err)
	}
	if err := c.NetworkConnect(ctx, "ovl0", "c1", dockerx.EndpointSpec{}); err == nil || !strings.Contains(err.Error(), "not manually attachable") {
		t.Errorf("non-attachable overlay: %v", err)
	}
}

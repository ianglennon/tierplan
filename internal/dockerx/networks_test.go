// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package dockerx_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
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
				Mounts:   []dockertest.FakeServiceMount{{Type: "volume", Source: "data", Target: "/var/lib/data", ReadOnly: true}, {Type: "bind", Source: "/srv/x"}, {Type: "tmpfs"}}},
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
		{ID: "s1", Name: "idle", Networks: []string{"ovl-id-2"}, PreviousNetworks: []string{}, VIPNetworks: []string{"ovl-id-2"}, Volumes: []string{}, VolumeMounts: []dockerx.ServiceVolumeMount{}, Secrets: []dockerx.ServiceObjectRef{}, Configs: []dockerx.ServiceObjectRef{}},
		{ID: "s3", Name: "plain", Networks: []string{}, PreviousNetworks: []string{}, VIPNetworks: []string{}, Volumes: []string{}, VolumeMounts: []dockerx.ServiceVolumeMount{}, Secrets: []dockerx.ServiceObjectRef{}, Configs: []dockerx.ServiceObjectRef{}},
		{ID: "s2", Name: "web", Networks: []string{"ovl-id-1"}, PreviousNetworks: []string{}, VIPNetworks: []string{"ovl-id-1"}, Volumes: []string{"data"},
			VolumeMounts: []dockerx.ServiceVolumeMount{{Source: "data", Target: "/var/lib/data", ReadOnly: true}},
			Secrets:      []dockerx.ServiceObjectRef{}, Configs: []dockerx.ServiceObjectRef{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ServiceRefs = %+v, want %+v (sorted by name, names resolved to IDs, only volume mounts)", got, want)
	}
	if w := fake.Writes(); len(w) != 0 {
		t.Fatalf("a read wrote: %v", w)
	}
}

// TestServiceRefsPreviousAndVIP: the networks a service's previous spec names (a rollback
// returns to them) and the ones its endpoint holds a virtual IP on are references too, even
// when the current spec names neither, each as an ID and once.
func TestServiceRefsPreviousAndVIP(t *testing.T) {
	cfg := refsManager()
	cfg.Networks = append(cfg.Networks,
		dockertest.FakeNetwork{ID: "old-id", Name: "old", Driver: "overlay", Scope: "swarm"},
		dockertest.FakeNetwork{ID: "vip-id", Name: "vip", Driver: "overlay", Scope: "swarm"})
	cfg.ServiceRows = []dockertest.FakeService{
		// Updated away from "old" (named by name in the previous spec, twice) and onto "ovl".
		{ID: "s1", Name: "rolled", Mode: "replicated", Running: 1, Desired: 1, Networks: []string{"ovl"},
			PreviousSpec: &swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{Networks: []swarm.NetworkAttachmentConfig{{Target: "old"}, {Target: "old"}, {Target: "ovl-id-1"}}}}},
		// The spec names nothing; only the endpoint still has a virtual IP.
		{ID: "s2", Name: "vipped", Mode: "replicated", Running: 1, Desired: 1},
	}
	cfg.ServiceVIPNetworks = map[string][]string{"vipped": {"vip-id", "vip-id"}}
	got, err := newClient(t, dockertest.NewFake(t, cfg)).ServiceRefs(context.Background())
	if err != nil {
		t.Fatalf("ServiceRefs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ServiceRefs = %+v, want two", got)
	}
	rolled, vipped := got[0], got[1]
	if rolled.Name != "rolled" || !slices.Equal(rolled.Networks, []string{"ovl-id-1"}) ||
		!slices.Equal(rolled.PreviousNetworks, []string{"old-id", "ovl-id-1"}) || !slices.Equal(rolled.VIPNetworks, []string{"ovl-id-1"}) {
		t.Errorf("rolled = %+v, want current ovl, previous old+ovl as IDs once each, a VIP on the current network", rolled)
	}
	if vipped.Name != "vipped" || len(vipped.Networks) != 0 || len(vipped.PreviousNetworks) != 0 || !slices.Equal(vipped.VIPNetworks, []string{"vip-id"}) {
		t.Errorf("vipped = %+v, want only the virtual IP network, once", vipped)
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

func TestNetworkRowsCarryConfigOnlyAndConfigFrom(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Networks: []dockertest.FakeNetwork{
		{ID: "tpl", Name: "tpl", Driver: "macvlan", Scope: "local", ConfigOnly: true},
		{ID: "mv", Name: "mv", Driver: "macvlan", Scope: "swarm", ConfigFrom: "tpl"},
		{ID: "plain", Name: "plain", Driver: "bridge", Scope: "local"},
	}})
	snap, err := newClient(t, fake).Snapshot(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]dockerx.NetworkRow{}
	for _, n := range snap.Networks {
		by[n.Name] = n
	}
	if !by["tpl"].ConfigOnly || by["tpl"].ConfigFrom != "" {
		t.Errorf("tpl = %+v, want a config-only template", by["tpl"])
	}
	if by["mv"].ConfigOnly || by["mv"].ConfigFrom != "tpl" {
		t.Errorf("mv = %+v, want a network configured from tpl", by["mv"])
	}
	if p := by["plain"]; p.ConfigOnly || p.ConfigFrom != "" {
		t.Errorf("plain = %+v, want neither", p)
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

// Disconnecting (plan 04-15 task 2, D-43): one POST, never forced.

func TestNetworkDisconnectSendsOneUnforcedPostAndTheContainerLeavesTheNetwork(t *testing.T) {
	c, fake := connectFake(t)
	if err := c.NetworkDisconnect(context.Background(), "app0", "c2"); err != nil {
		t.Fatalf("NetworkDisconnect = %v", err)
	}
	if got := fake.Writes(); !slices.Equal(got, []string{"POST /networks/app0/disconnect"}) {
		t.Fatalf("writes = %v, want one disconnect and no retry", got)
	}
	got := fake.NetworkDisconnects()
	if len(got) != 1 || got[0].Container != "c2" || got[0].Force {
		t.Fatalf("disconnects = %+v, want c2 and no force", got)
	}
	detail, _, err := c.ContainerDetail(context.Background(), "c2")
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Networks) != 0 {
		t.Errorf("container networks = %+v, want none", detail.Networks)
	}
}

func TestNetworkDisconnectOfAContainerNotOnTheNetworkAnUnknownContainerAndAnUnknownNetworkAreTheEnginesAnswers(t *testing.T) {
	c, _ := connectFake(t)
	ctx := context.Background()
	if err := c.NetworkDisconnect(ctx, "app0", "c1"); err == nil || !strings.Contains(dockerx.DaemonText(err), "is not connected to network") {
		t.Errorf("not connected: %v (%q)", err, err)
	}
	if err := c.NetworkDisconnect(ctx, "app0", "ghost"); !dockerx.IsNotFound(err) {
		t.Errorf("unknown container: %v, want not found", err)
	}
	if err := c.NetworkDisconnect(ctx, "nope", "c2"); !dockerx.IsNotFound(err) {
		t.Errorf("unknown network: %v, want not found", err)
	}
}

func TestDaemonTextIsTheEnginesSentenceWithoutTheClientsWrapping(t *testing.T) {
	c, _ := connectFake(t)
	err := c.NetworkConnect(context.Background(), "nope", "c1", dockerx.EndpointSpec{})
	if got := dockerx.DaemonText(err); got != "network nope not found" {
		t.Errorf("DaemonText = %q", got)
	}
	if got := dockerx.DaemonText(errors.New("  plain  ")); got != "plain" {
		t.Errorf("DaemonText of a plain error = %q", got)
	}
	if dockerx.IsRefusal(errors.New("connection refused")) {
		t.Error("a failed connection is not a refusal")
	}
}

// Engine 24 answers a connect address outside the network's subnets with a server error
// (Engine 29 says 400); it is still the engine refusing the address, not a failed node.
// Found by the Engine 24 integration run.
func TestAnOlderEnginesAddressRefusalSentAsAServerErrorIsARefusal(t *testing.T) {
	old := fmt.Errorf("connect to network on tcp://h: Error response from daemon: Invalid address 10.231.99.9: It does not belong to any of this network's subnets: %w", cerrdefs.ErrInternal)
	if !dockerx.IsRefusal(old) {
		t.Errorf("IsRefusal(%v) = false, want true", old)
	}
	other := fmt.Errorf("connect to network on tcp://h: Error response from daemon: failed to set up container networking: %w", cerrdefs.ErrInternal)
	if dockerx.IsRefusal(other) {
		t.Errorf("IsRefusal(%v) = true, want false: any other server error is not a refusal", other)
	}
	if dockerx.IsRefusal(nil) {
		t.Error("IsRefusal(nil) = true")
	}
}

func TestNetworkCreateSendsEveryPoolAndTheIPAMDriver(t *testing.T) {
	c, fake := networkFake(t)
	_, _, err := c.NetworkCreate(context.Background(), dockerx.NetworkCreateSpec{
		Name: "stack_backend", Driver: "overlay", Scope: dockerx.ScopeSwarm, IPAMDriver: "default",
		Pools: []dockerx.NetworkPool{
			{Subnet: "10.50.0.0/24", Gateway: "10.50.0.1", IPRange: "10.50.0.128/25"},
			{Subnet: "10.51.0.0/24"},
		},
	})
	if err != nil {
		t.Fatalf("NetworkCreate: %v", err)
	}
	req := fake.NetworkCreates()[0]
	if req.IPAM == nil || req.IPAM.Driver != "default" || len(req.IPAM.Config) != 2 {
		t.Fatalf("IPAM = %+v, want the default driver and two pools", req.IPAM)
	}
	first, second := req.IPAM.Config[0], req.IPAM.Config[1]
	if first.Subnet.String() != "10.50.0.0/24" || first.Gateway.String() != "10.50.0.1" || first.IPRange.String() != "10.50.0.128/25" {
		t.Errorf("first pool = %+v", first)
	}
	if second.Subnet.String() != "10.51.0.0/24" || second.Gateway.IsValid() || second.IPRange.IsValid() {
		t.Errorf("second pool = %+v, want only its subnet", second)
	}
}

func TestNetworkCreateRefusesPoolsTogetherWithTheSingleAddressFieldsBeforeAnyCall(t *testing.T) {
	c, fake := networkFake(t)
	for _, spec := range []dockerx.NetworkCreateSpec{
		{Name: "x", Driver: "bridge", Subnet: "10.1.0.0/24", Pools: []dockerx.NetworkPool{{Subnet: "10.2.0.0/24"}}},
		{Name: "x", Driver: "bridge", Gateway: "10.1.0.1", Pools: []dockerx.NetworkPool{{Subnet: "10.2.0.0/24"}}},
		{Name: "x", Driver: "bridge", IPRange: "10.1.0.0/25", Pools: []dockerx.NetworkPool{{Subnet: "10.2.0.0/24"}}},
	} {
		if _, _, err := c.NetworkCreate(context.Background(), spec); !errors.Is(err, dockerx.ErrAddressing) {
			t.Errorf("NetworkCreate(%+v) err = %v, want ErrAddressing", spec, err)
		}
	}
	if w := fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, a refused spec must send nothing", w)
	}
}

func TestNetworkCreateRefusesAnUnparsablePoolBeforeAnyCall(t *testing.T) {
	c, fake := networkFake(t)
	for _, pool := range []dockerx.NetworkPool{{Subnet: "not-a-cidr"}, {Subnet: "10.2.0.0/24", Gateway: "x"}, {Subnet: "10.2.0.0/24", IPRange: "y"}} {
		spec := dockerx.NetworkCreateSpec{Name: "x", Driver: "bridge", Pools: []dockerx.NetworkPool{{Subnet: "10.9.0.0/24"}, pool}}
		if _, _, err := c.NetworkCreate(context.Background(), spec); !errors.Is(err, dockerx.ErrAddressing) {
			t.Errorf("NetworkCreate(pool %+v) err = %v, want ErrAddressing", pool, err)
		}
	}
	if w := fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
}

func TestNetworkCreateKeepsTheSingleAddressFieldsWorkingWithoutPools(t *testing.T) {
	c, fake := networkFake(t)
	if _, _, err := c.NetworkCreate(context.Background(), dockerx.NetworkCreateSpec{Name: "one", Driver: "bridge", Subnet: "10.3.0.0/24"}); err != nil {
		t.Fatal(err)
	}
	req := fake.NetworkCreates()[0]
	if req.IPAM == nil || len(req.IPAM.Config) != 1 || req.IPAM.Config[0].Subnet.String() != "10.3.0.0/24" || req.IPAM.Driver != "" {
		t.Errorf("IPAM = %+v, want one pool and no driver", req.IPAM)
	}
}

// Address use (plan 07-48, G-07-5): an Engine 29 manager reports each subnet's addresses
// in use and still available in the network inspect, and that is the one reliable sign
// that a network is full.

func TestNetworkDetailReadsTheAddressUsageAManagerReports(t *testing.T) {
	fake := dockertest.NewFake(t, detailManager())
	fake.SetNetworkUsage("ovl",
		dockertest.FakeSubnetUsage{Subnet: "fd00:1::/64", InUse: 3, Available: 1 << 40},
		dockertest.FakeSubnetUsage{Subnet: "10.0.1.0/24", InUse: 256, Available: 0})
	d, raw, err := newClient(t, fake).NetworkDetail(context.Background(), "ovl-id-1", false)
	if err != nil {
		t.Fatalf("NetworkDetail: %v", err)
	}
	want := []dockerx.SubnetUsage{
		{Subnet: "10.0.1.0/24", InUse: 256, Available: 0},
		{Subnet: "fd00:1::/64", InUse: 3, Available: 1 << 40},
	}
	if !reflect.DeepEqual(d.Usage, want) {
		t.Errorf("Usage = %+v, want %+v (sorted by subnet, a full subnet with Available 0)", d.Usage, want)
	}
	wantIPAM := []dockerx.IPAMRow{{Subnet: "10.0.1.0/24", Gateway: "10.0.1.1"}, {Subnet: "fd00:1::/64"}}
	if !reflect.DeepEqual(d.IPAM, wantIPAM) || d.ID != "ovl-id-1" || d.Name != "ovl" || !d.Attachable {
		t.Errorf("existing fields moved: %+v", d)
	}
	if !strings.Contains(string(raw), `"DynamicIPsAvailable"`) {
		t.Errorf("raw = %s, want the Status document", raw)
	}
}

func TestNetworkDetailOfAnEngineThatReportsNoUsageHasNone(t *testing.T) {
	d, raw, err := newClient(t, dockertest.NewFake(t, detailManager())).NetworkDetail(context.Background(), "ovl-id-1", false)
	if err != nil {
		t.Fatalf("NetworkDetail: %v", err)
	}
	if d.Usage != nil {
		t.Errorf("Usage = %+v, want nil: an engine with no Status must not read as a full network", d.Usage)
	}
	if strings.Contains(string(raw), `"Status"`) {
		t.Errorf("raw has a Status key: %s", raw)
	}
}

func TestTheFakeReportsUsageForANetworkTheClientCreated(t *testing.T) {
	fake := dockertest.NewFake(t, detailManager())
	c := newClient(t, fake)
	id, _, err := c.NetworkCreate(context.Background(), dockerx.NetworkCreateSpec{Name: "made", Driver: "overlay", Subnet: "10.9.0.0/28"})
	if err != nil {
		t.Fatalf("NetworkCreate: %v", err)
	}
	fake.SetNetworkUsage("made", dockertest.FakeSubnetUsage{Subnet: "10.9.0.0/28", InUse: 16, Available: 0})
	d, _, err := c.NetworkDetail(context.Background(), id, false)
	if err != nil {
		t.Fatalf("NetworkDetail: %v", err)
	}
	want := []dockerx.SubnetUsage{{Subnet: "10.9.0.0/28", InUse: 16, Available: 0}}
	if !reflect.DeepEqual(d.Usage, want) {
		t.Errorf("Usage = %+v, want %+v", d.Usage, want)
	}
}

// Engine 29 answers an inspect by a network's full ID from the node's own instance of it,
// and only a name or partial ID brings the swarm leader's count (UAT G-07-8). NetworkDetail
// makes one read and shows that answer. NetworkSwarmUsage reads a full ID again by the ID less
// its last character, for the swarm's count.

const bigNetID = "skvqz4pk12gec9lje3akizuef"

func bigNetFake(t *testing.T) *dockertest.Fake {
	t.Helper()
	return dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		Networks: []dockertest.FakeNetwork{
			{ID: bigNetID, Name: "big_default", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.2.0/24"}},
		},
	})
}

// dupNetFake has two swarm networks whose IDs differ only in the last character, so the ID of
// one less its last character is also a prefix of the other. dup1 is listed first, so the
// engine's first match for the shared 24 character prefix is dup1 on every run. A real engine
// answers that prefix as ambiguous, also an error.
func dupNetFake(t *testing.T) *dockertest.Fake {
	t.Helper()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		Networks: []dockertest.FakeNetwork{
			{ID: "dupnetaaaaaaaaaaaaaaaaaa1", Name: "dup1", Driver: "overlay", Scope: "swarm"},
			{ID: "dupnetaaaaaaaaaaaaaaaaaa2", Name: "dup2", Driver: "overlay", Scope: "swarm"},
		},
	})
	fake.SetNetworkUsage("dup1", dockertest.FakeSubnetUsage{Subnet: "10.0.3.0/24", InUse: 256, Available: 0})
	fake.SetNetworkUsage("dup2", dockertest.FakeSubnetUsage{Subnet: "10.0.4.0/24", InUse: 10, Available: 246})
	return fake
}

// networkInspects counts the GETs of one network the fake has served.
func networkInspects(f *dockertest.Fake) int {
	n := 0
	for _, p := range f.Requests() {
		if strings.Contains(p, "/networks/") {
			n++
		}
	}
	return n
}

// The detail page and Inspect ask once and show what the engine answers for that ID, which is
// what docker network inspect shows on the same node. On a node holding a local instance of a
// swarm network that is the node's own count, not the swarm's.
func TestNetworkDetailReadsAFullIDOnceAndAnswersWithTheNodesOwnCount(t *testing.T) {
	fake := bigNetFake(t)
	fake.SetNetworkUsage("big_default", dockertest.FakeSubnetUsage{Subnet: "10.0.2.0/24", InUse: 256, Available: 0})
	fake.SetNetworkLocalUsage("big_default", dockertest.FakeSubnetUsage{Subnet: "10.0.2.0/24", InUse: 46, Available: 210})
	c := newClient(t, fake)
	for _, verbose := range []bool{false, true} {
		before := networkInspects(fake)
		d, raw, err := c.NetworkDetail(context.Background(), bigNetID, verbose)
		if err != nil {
			t.Fatalf("verbose=%v: NetworkDetail: %v", verbose, err)
		}
		if got := networkInspects(fake) - before; got != 1 {
			t.Errorf("verbose=%v: %d network reads, want 1: the page would cost the engine a second scan", verbose, got)
		}
		reqs := fake.Requests()
		if last := reqs[len(reqs)-1]; !strings.HasSuffix(last, "/networks/"+bigNetID) {
			t.Errorf("verbose=%v: the read was %q, want one by the full ID", verbose, last)
		}
		if d.ID != bigNetID || d.Name != "big_default" {
			t.Errorf("verbose=%v: ID, Name = %q, %q", verbose, d.ID, d.Name)
		}
		want := []dockerx.SubnetUsage{{Subnet: "10.0.2.0/24", InUse: 46, Available: 210}}
		if !reflect.DeepEqual(d.Usage, want) {
			t.Errorf("verbose=%v: Usage = %+v, want %+v: the detail page would show the swarm's count, not this node's own", verbose, d.Usage, want)
		}
		if !strings.Contains(string(raw), `"IPsInUse":46`) || strings.Contains(string(raw), `"IPsInUse":256`) {
			t.Errorf("verbose=%v: raw = %s, want the node's own Status: the Inspect tab would disagree with docker network inspect", verbose, raw)
		}
	}
}

// Two networks whose IDs share all but the last character must not make the detail page fail:
// NetworkDetail never reads by a prefix, so it answers the network asked for as itself.
func TestNetworkDetailAnswersTheNetworkAskedForWhenAnotherSharesItsPrefix(t *testing.T) {
	fake := dupNetFake(t)
	before := networkInspects(fake)
	d, _, err := newClient(t, fake).NetworkDetail(context.Background(), "dupnetaaaaaaaaaaaaaaaaaa2", false)
	if err != nil {
		t.Fatalf("NetworkDetail: %v: the detail page would be an error for a network that exists", err)
	}
	if d.ID != "dupnetaaaaaaaaaaaaaaaaaa2" {
		t.Errorf("ID = %q, want dup2's", d.ID)
	}
	want := []dockerx.SubnetUsage{{Subnet: "10.0.4.0/24", InUse: 10, Available: 246}}
	if !reflect.DeepEqual(d.Usage, want) {
		t.Errorf("Usage = %+v, want %+v", d.Usage, want)
	}
	if got := networkInspects(fake) - before; got != 1 {
		t.Errorf("%d network reads, want 1", got)
	}
}

func TestNetworkSwarmUsageReadsTheSwarmsCountWhenTheNodeHoldsTheNetworkLocally(t *testing.T) {
	fake := bigNetFake(t)
	fake.SetNetworkUsage("big_default", dockertest.FakeSubnetUsage{Subnet: "10.0.2.0/24", InUse: 256, Available: 0})
	fake.SetNetworkLocalUsage("big_default", dockertest.FakeSubnetUsage{Subnet: "10.0.2.0/24", InUse: 46, Available: 210})
	c := newClient(t, fake)
	for _, tc := range []struct {
		name, ref string
		reads     int
	}{
		{"the full ID", bigNetID, 2},
		{"the name", "big_default", 1},
		{"a 12 character prefix", bigNetID[:12], 1},
	} {
		before := networkInspects(fake)
		d, err := c.NetworkSwarmUsage(context.Background(), tc.ref)
		if err != nil {
			t.Fatalf("%s: NetworkSwarmUsage: %v", tc.name, err)
		}
		want := []dockerx.SubnetUsage{{Subnet: "10.0.2.0/24", InUse: 256, Available: 0}}
		if !reflect.DeepEqual(d.Usage, want) {
			t.Errorf("%s: Usage = %+v, want the swarm's %+v", tc.name, d.Usage, want)
		}
		if d.ID != bigNetID || d.Name != "big_default" {
			t.Errorf("%s: ID, Name = %q, %q", tc.name, d.ID, d.Name)
		}
		if got := networkInspects(fake) - before; got != tc.reads {
			t.Errorf("%s: %d network reads, want %d", tc.name, got, tc.reads)
		}
	}
}

func TestNetworkSwarmUsageReadsALocalNetworkOrAnOldEngineOnce(t *testing.T) {
	t.Run("a local network", func(t *testing.T) {
		fake := dockertest.NewFake(t, dockertest.FakeConfig{Networks: []dockertest.FakeNetwork{
			{ID: "br-local-id", Name: "br", Driver: "bridge", Scope: "local", Subnets: []string{"172.30.0.0/24"}},
		}})
		fake.SetNetworkLocalUsage("br", dockertest.FakeSubnetUsage{Subnet: "172.30.0.0/24", InUse: 5, Available: 250})
		before := networkInspects(fake)
		d, err := newClient(t, fake).NetworkSwarmUsage(context.Background(), "br-local-id")
		if err != nil {
			t.Fatalf("NetworkSwarmUsage: %v", err)
		}
		want := []dockerx.SubnetUsage{{Subnet: "172.30.0.0/24", InUse: 5, Available: 250}}
		if !reflect.DeepEqual(d.Usage, want) {
			t.Errorf("Usage = %+v, want %+v", d.Usage, want)
		}
		if got := networkInspects(fake) - before; got != 1 {
			t.Errorf("%d network reads, want 1", got)
		}
	})
	t.Run("an engine older than 29", func(t *testing.T) {
		fake := dockertest.NewFake(t, detailManager())
		before := networkInspects(fake)
		d, err := newClient(t, fake).NetworkSwarmUsage(context.Background(), "ovl-id-1")
		if err != nil {
			t.Fatalf("NetworkSwarmUsage: %v", err)
		}
		if d.Usage != nil {
			t.Errorf("Usage = %+v, want nil", d.Usage)
		}
		if got := networkInspects(fake) - before; got != 1 {
			t.Errorf("%d network reads, want 1", got)
		}
	})
}

func TestNetworkSwarmUsageRefusesAnAnswerForAnotherNetwork(t *testing.T) {
	fake := dupNetFake(t)
	c := newClient(t, fake)
	d, err := c.NetworkSwarmUsage(context.Background(), "dupnetaaaaaaaaaaaaaaaaaa")
	if err != nil || d.ID != "dupnetaaaaaaaaaaaaaaaaaa1" {
		t.Fatalf("a partial ID = %+v, %v, want dup1", d, err)
	}
	d, err = c.NetworkSwarmUsage(context.Background(), "dupnetaaaaaaaaaaaaaaaaaa2")
	if err == nil || dockerx.IsNotFound(err) {
		t.Fatalf("err = %v, want an error that is not a not-found", err)
	}
	if d.Usage != nil || d.ID != "" {
		t.Errorf("detail = %+v, want none: dup1's numbers must not come back for dup2", d)
	}
}

func TestNetworkSwarmUsageHasNoUsageWhenThePartialIDReadBringsNoSwarmCount(t *testing.T) {
	for _, tc := range []struct {
		name string
		tune func(*dockertest.Fake)
	}{
		{"the swarm answers with no Status", func(f *dockertest.Fake) {
			f.SetNetworkUsage("big_default", dockertest.FakeSubnetUsage{Subnet: "10.0.2.0/24", InUse: 256, Available: 0})
			f.SetNetworkNoSwarmStatus("big_default", true)
		}},
		{"the node cannot ask the swarm", func(*dockertest.Fake) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := bigNetFake(t)
			fake.SetNetworkLocalUsage("big_default", dockertest.FakeSubnetUsage{Subnet: "10.0.2.0/24", InUse: 46, Available: 210})
			tc.tune(fake)
			before := networkInspects(fake)
			d, err := newClient(t, fake).NetworkSwarmUsage(context.Background(), bigNetID)
			if err != nil {
				t.Fatalf("NetworkSwarmUsage: %v", err)
			}
			if d.Usage != nil {
				t.Errorf("Usage = %+v, want nil: the node's own count is not the swarm's", d.Usage)
			}
			if d.ID != bigNetID || d.Name != "big_default" {
				t.Errorf("ID, Name = %q, %q", d.ID, d.Name)
			}
			if got := networkInspects(fake) - before; got != 2 {
				t.Errorf("%d network reads, want 2", got)
			}
		})
	}
	t.Run("a one-read lookup by name cannot tell", func(t *testing.T) {
		fake := bigNetFake(t)
		fake.SetNetworkLocalUsage("big_default", dockertest.FakeSubnetUsage{Subnet: "10.0.2.0/24", InUse: 46, Available: 210})
		before := networkInspects(fake)
		d, err := newClient(t, fake).NetworkSwarmUsage(context.Background(), "big_default")
		if err != nil {
			t.Fatalf("NetworkSwarmUsage: %v", err)
		}
		want := []dockerx.SubnetUsage{{Subnet: "10.0.2.0/24", InUse: 46, Available: 210}}
		if !reflect.DeepEqual(d.Usage, want) {
			t.Errorf("Usage = %+v, want %+v", d.Usage, want)
		}
		if got := networkInspects(fake) - before; got != 1 {
			t.Errorf("%d network reads, want 1", got)
		}
	})
}

// allocFake is a swarm manager whose managers' allocator leaves the networks of the given subnets
// unallocated for that many inspects (negative: for good).
func allocFake(t *testing.T, unalloc map[string]int, removeErrs map[string]string) (*dockerx.Client, *dockertest.Fake) {
	t.Helper()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true, SwarmUnallocated: unalloc, RemoveErrors: removeErrs,
	})
	return newClient(t, fake), fake
}

// atOnce is an AllocationWait whose clock fires at once, so no test waits.
func atOnce(attempts int) dockerx.AllocationWait {
	return dockerx.AllocationWait{
		Attempts: attempts,
		After: func(time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			ch <- time.Time{}
			return ch
		},
	}
}

// readsOf is how many times the fake was asked to inspect the network. Requests lists writes too,
// so the removals are taken off.
func readsOf(fake *dockertest.Fake, id string) int {
	n := 0
	for _, p := range fake.Requests() {
		if strings.HasSuffix(p, "/networks/"+id) {
			n++
		}
	}
	for _, w := range fake.Writes() {
		if w == "DELETE /networks/"+id {
			n--
		}
	}
	return n
}

func swarmSpec(subnet string) dockerx.NetworkCreateSpec {
	return dockerx.NetworkCreateSpec{Name: "ovl", Driver: "overlay", Scope: dockerx.ScopeSwarm, Subnet: subnet}
}

func TestNetworkCreateAllocatedReady(t *testing.T) {
	c, fake := allocFake(t, nil, nil)
	id, warning, err := c.NetworkCreateAllocated(context.Background(), swarmSpec("10.9.0.0/24"), atOnce(5))
	if err != nil || id == "" || warning != "" {
		t.Fatalf("NetworkCreateAllocated = %q, %q, %v", id, warning, err)
	}
	if got := fake.Writes(); !slices.Equal(got, []string{"POST /networks/create"}) {
		t.Fatalf("writes = %v, want one create and no removal", got)
	}
	if n := readsOf(fake, id); n != 1 {
		t.Errorf("reads = %d, want 1", n)
	}
}

func TestNetworkCreateAllocatedLate(t *testing.T) {
	c, fake := allocFake(t, map[string]int{"10.9.0.0/24": 2}, nil)
	id, _, err := c.NetworkCreateAllocated(context.Background(), swarmSpec("10.9.0.0/24"), atOnce(5))
	if err != nil || id == "" {
		t.Fatalf("NetworkCreateAllocated = %q, %v", id, err)
	}
	if n := readsOf(fake, id); n != 3 {
		t.Errorf("reads = %d, want 3", n)
	}
	if got := fake.Writes(); !slices.Equal(got, []string{"POST /networks/create"}) {
		t.Fatalf("writes = %v, want no removal for a network that was only late", got)
	}
}

func TestNetworkCreateAllocatedNever(t *testing.T) {
	c, fake := allocFake(t, map[string]int{"10.9.0.0/24": -1}, nil)
	id, _, err := c.NetworkCreateAllocated(context.Background(), swarmSpec("10.9.0.0/24"), atOnce(4))
	var na *dockerx.NotAllocatedError
	if !errors.As(err, &na) || !errors.Is(err, dockerx.ErrNetworkNotAllocated) {
		t.Fatalf("err = %v, want a *NotAllocatedError", err)
	}
	if !na.Removed || na.RemoveErr != nil || na.ID != id || na.Name != "ovl" || na.Subnet != "10.9.0.0/24" {
		t.Errorf("error = %+v", na)
	}
	if n := readsOf(fake, id); n != 4 {
		t.Errorf("reads = %d, want 4 (Attempts)", n)
	}
	if got := fake.Writes(); !slices.Equal(got, []string{"POST /networks/create", "DELETE /networks/" + id}) {
		t.Fatalf("writes = %v, want the create then one removal by ID", got)
	}
	if strings.Contains(strings.ToLower(err.Error()), "overlap") || dockerx.IsPoolOverlap(err) {
		t.Errorf("a not allocated error must not read as an overlap refusal: %v", err)
	}
}

func TestNetworkCreateAllocatedStubLeft(t *testing.T) {
	c, _ := allocFake(t, map[string]int{"10.9.0.0/24": -1}, map[string]string{"ovl": "boom"})
	_, _, err := c.NetworkCreateAllocated(context.Background(), swarmSpec("10.9.0.0/24"), atOnce(2))
	var na *dockerx.NotAllocatedError
	if !errors.As(err, &na) || !errors.Is(err, dockerx.ErrNetworkNotAllocated) {
		t.Fatalf("err = %v, want a *NotAllocatedError", err)
	}
	if na.Removed || na.RemoveErr == nil || !strings.Contains(na.RemoveErr.Error(), "boom") {
		t.Errorf("error = %+v, want Removed false and the removal's error", na)
	}
}

func TestNetworkCreateAllocatedLocalNotRead(t *testing.T) {
	c, fake := networkFake(t)
	id, _, err := c.NetworkCreateAllocated(context.Background(), dockerx.NetworkCreateSpec{Name: "lan", Driver: "bridge", Subnet: "10.9.0.0/24"}, atOnce(3))
	if err != nil || id == "" {
		t.Fatalf("NetworkCreateAllocated = %q, %v", id, err)
	}
	if n := readsOf(fake, id); n != 0 {
		t.Errorf("reads = %d, want none for a local network", n)
	}
}

func TestNetworkCreateAllocatedContextEnds(t *testing.T) {
	c, fake := allocFake(t, map[string]int{"10.9.0.0/24": -1}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := dockerx.AllocationWait{
		Attempts: 5,
		After: func(time.Duration) <-chan time.Time {
			cancel() // the caller gives up while the wait is on; the clock never fires
			return nil
		},
	}
	id, _, err := c.NetworkCreateAllocated(ctx, swarmSpec("10.9.0.0/24"), w)
	if !errors.Is(err, context.Canceled) || errors.Is(err, dockerx.ErrNetworkNotAllocated) {
		t.Fatalf("err = %v, want the context's error and not a not allocated one", err)
	}
	if id == "" {
		t.Errorf("id is empty: the caller needs it to find the network it may have left")
	}
	if got := fake.Writes(); !slices.Equal(got, []string{"POST /networks/create"}) {
		t.Fatalf("writes = %v, want no removal", got)
	}
}

func TestNetworkCreateAllocatedReadFailureLeavesTheNetwork(t *testing.T) {
	c, fake := allocFake(t, nil, nil)
	fake.Reconfigure(dockertest.FakeConfig{SwarmState: "active", SwarmControl: true, PolicyDeny: true})
	_, _, err := c.NetworkCreateAllocated(context.Background(), swarmSpec("10.9.0.0/24"), atOnce(3))
	if err == nil || errors.Is(err, dockerx.ErrNetworkNotAllocated) {
		t.Fatalf("err = %v, want the failure, not a not allocated one", err)
	}
}

func TestIsPoolOverlap(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"the default IPAM", errors.New("Error response from daemon: invalid pool request: Pool overlaps with other one on this address space"), true},
		{"the bridge driver", errors.New("Error response from daemon: networks have overlapping IPv4"), true},
		{"a taken name", fmt.Errorf("create: %w", cerrdefs.ErrConflict.WithMessage("network with name x already exists")), false},
		{"not found", cerrdefs.ErrNotFound.WithMessage("network gone"), false},
		{"a policy denial", errors.New("agent_policy_denied: POST /networks/create overlap"), false},
		{"not allocated", &dockerx.NotAllocatedError{Name: "x", Removed: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dockerx.IsPoolOverlap(tc.err); got != tc.want {
				t.Errorf("IsPoolOverlap(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

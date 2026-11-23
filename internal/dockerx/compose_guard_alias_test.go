// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/ianglennon/tierplan/internal/ingress"
)

// recordingEngine stands in for the engine client behind the guard: it records what a create or a connect
// would have sent and answers a container inspect with a fixed container.
type recordingEngine struct {
	client.APIClient
	created   []client.ContainerCreateOptions
	connected []client.NetworkConnectOptions
	networks  []string
	inspected container.InspectResponse
	// nets maps what a create or connect may name a network by (a name, an ID or an ID prefix) to the network's name.
	// The ingress network is known by its name and by ingressID; any other reference is not found.
	nets       map[string]string
	netCreated []string
}

const ingressID = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"

func (r *recordingEngine) NetworkInspect(_ context.Context, ref string, _ client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	if ref == ingress.NetworkName || (len(ref) >= 12 && strings.HasPrefix(ingressID, ref)) {
		return client.NetworkInspectResult{Network: network.Inspect{Network: network.Network{ID: ingressID, Name: ingress.NetworkName}}}, nil
	}
	name, ok := r.nets[ref]
	if !ok && ref == "shop_default" {
		name, ok = ref, true
	}
	if !ok {
		return client.NetworkInspectResult{}, cerrdefs.ErrNotFound
	}
	return client.NetworkInspectResult{Network: network.Inspect{Network: network.Network{ID: "id-" + name, Name: name}}}, nil
}

func (r *recordingEngine) ContainerCreate(_ context.Context, o client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	r.created = append(r.created, o)
	return client.ContainerCreateResult{ID: "c1"}, nil
}

func (r *recordingEngine) NetworkConnect(_ context.Context, n string, o client.NetworkConnectOptions) (client.NetworkConnectResult, error) {
	r.networks = append(r.networks, n)
	r.connected = append(r.connected, o)
	return client.NetworkConnectResult{}, nil
}

func (r *recordingEngine) ContainerInspect(_ context.Context, _ string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{Container: r.inspected}, nil
}

// uniqueAlias is what the deployer puts on the ingress endpoint of shop's web.
var uniqueAlias = ingress.Alias("shop", "web")

// shopWebLabels are the labels the SDK puts on replica 1 of service web in project shop.
var shopWebLabels = map[string]string{composeProjectLabel: "shop", composeServiceLabel: "web", composeNumberLabel: "1"}

func TestGuardStripsTheBareServiceAliasOnTheIngressNetwork(t *testing.T) {
	t.Parallel()
	eng := &recordingEngine{}
	g := guardClient{APIClient: eng}

	ingressEP := &network.EndpointSettings{Aliases: []string{"web", "shop-web-1", uniqueAlias}}
	defaultEP := &network.EndpointSettings{Aliases: []string{"web", "shop-web-1"}}
	in := client.ContainerCreateOptions{
		Name:   "shop-web-1",
		Config: &container.Config{Image: "nginx", Labels: shopWebLabels},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			"shop_default":      defaultEP,
			ingress.NetworkName: ingressEP,
		}},
	}

	if _, err := g.ContainerCreate(context.Background(), in); err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if len(eng.created) != 1 {
		t.Fatalf("the engine saw %d creates, want 1", len(eng.created))
	}
	sent := eng.created[0].NetworkingConfig.EndpointsConfig
	if got := sent[ingress.NetworkName].Aliases; !reflect.DeepEqual(got, []string{uniqueAlias}) {
		t.Errorf("the ingress endpoint reached the engine with aliases %v, want only %q", got, uniqueAlias)
	}
	if got := sent["shop_default"].Aliases; !reflect.DeepEqual(got, []string{"web", "shop-web-1"}) {
		t.Errorf("another network's endpoint was changed: %v", got)
	}
	// The caller's options and endpoints are the SDK's own and must be as it built them.
	if got := ingressEP.Aliases; !reflect.DeepEqual(got, []string{"web", "shop-web-1", uniqueAlias}) {
		t.Errorf("the caller's ingress endpoint was edited: %v", got)
	}
	if got := in.NetworkingConfig.EndpointsConfig[ingress.NetworkName]; got != ingressEP {
		t.Errorf("the caller's endpoints map was edited")
	}
}

// A create that replaces a container is made under the temporary name <id12>_<name>, while the SDK's alias is the final
// name: the guard drops that one too (WR-C2).
func TestGuardStripsTheContainerNameAliasOfARecreate(t *testing.T) {
	t.Parallel()
	eng := &recordingEngine{}
	g := guardClient{APIClient: eng}
	in := client.ContainerCreateOptions{
		Name:   "0123456789ab_shop-web-1",
		Config: &container.Config{Image: "nginx", Labels: shopWebLabels},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			ingress.NetworkName: {Aliases: []string{"web", "shop-web-1", uniqueAlias}},
		}},
	}
	if _, err := g.ContainerCreate(context.Background(), in); err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if got := eng.created[0].NetworkingConfig.EndpointsConfig[ingress.NetworkName].Aliases; !reflect.DeepEqual(got, []string{uniqueAlias}) {
		t.Errorf("a recreate's ingress endpoint reached the engine with aliases %v, want only %q", got, uniqueAlias)
	}
}

func TestGuardForwardsACreateWithNoIngressEndpointAsGiven(t *testing.T) {
	t.Parallel()
	eng := &recordingEngine{}
	g := guardClient{APIClient: eng}
	cfg := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
		"shop_default": {Aliases: []string{"web"}},
	}}
	in := client.ContainerCreateOptions{Name: "shop-web-1", Config: &container.Config{Image: "nginx"}, NetworkingConfig: cfg}
	if _, err := g.ContainerCreate(context.Background(), in); err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if eng.created[0].NetworkingConfig != cfg {
		t.Errorf("a create with no ingress endpoint was not forwarded untouched")
	}
	if _, err := g.ContainerCreate(context.Background(), client.ContainerCreateOptions{Config: &container.Config{Image: "nginx"}}); err != nil {
		t.Fatalf("ContainerCreate without a networking config: %v", err)
	}
}

func TestGuardRewritesNetworkConnectOnTheIngressNetwork(t *testing.T) {
	t.Parallel()
	eng := &recordingEngine{inspected: container.InspectResponse{
		Name:   "/shop-web-1",
		Config: &container.Config{Labels: shopWebLabels},
	}}
	g := guardClient{APIClient: eng}

	ep := &network.EndpointSettings{Aliases: []string{"web", "shop-web-1", uniqueAlias}}
	if _, err := g.NetworkConnect(context.Background(), ingress.NetworkName, client.NetworkConnectOptions{Container: "c1", EndpointConfig: ep}); err != nil {
		t.Fatalf("NetworkConnect: %v", err)
	}
	if got := eng.connected[0].EndpointConfig.Aliases; !reflect.DeepEqual(got, []string{uniqueAlias}) {
		t.Errorf("the ingress connect reached the engine with aliases %v, want only %q", got, uniqueAlias)
	}
	if !reflect.DeepEqual(ep.Aliases, []string{"web", "shop-web-1", uniqueAlias}) {
		t.Errorf("the caller's endpoint was edited: %v", ep.Aliases)
	}

	other := &network.EndpointSettings{Aliases: []string{"web", "shop-web-1"}}
	if _, err := g.NetworkConnect(context.Background(), "shop_default", client.NetworkConnectOptions{Container: "c1", EndpointConfig: other}); err != nil {
		t.Fatalf("NetworkConnect: %v", err)
	}
	if eng.connected[1].EndpointConfig != other {
		t.Errorf("a connect to another network was not forwarded as given")
	}
}

func TestIngressAliases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		in        []string
		service   string
		container string
		want      []string
	}{
		{"keeps the rest in order", []string{"web", "x", "shop-web-1", "y"}, "web", "shop-web-1", []string{"x", "y"}},
		{"a recreate's temporary name drops the final name", []string{"web", "shop-web-1", "u"}, "web", "0123456789ab_shop-web-1", []string{"u"}},
		{"a temporary name with a slash", []string{"shop-web-1", "u"}, "web", "/0123456789ab_shop-web-1", []string{"u"}},
		{"a name that only looks like a temporary one keeps nothing else", []string{"abc_shop-web-1", "shop-web-1", "u"}, "web", "abc_shop-web-1", []string{"shop-web-1", "u"}},
		{"leading slash on the container name", []string{"shop-web-1", "u"}, "web", "/shop-web-1", []string{"u"}},
		{"nothing left is nil", []string{"web", "shop-web-1"}, "web", "shop-web-1", nil},
		{"no service label drops only the container name", []string{"web", "shop-web-1"}, "", "shop-web-1", []string{"web"}},
	}
	for _, c := range cases {
		if got := ingressAliases(c.in, c.service, c.container); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// On the ingress network a container keeps the SDK's name for its replica (T-25-47): a create or a connect under any
// other name, such as another stack's alias set by container_name, is refused before the engine sees it. The SDK's own
// name, and a recreate's temporary one, pass; a create with no ingress endpoint is not judged.
func TestGuardRefusesAChosenContainerNameOnTheIngressNetwork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	create := func(name string, labels map[string]string, net string) error {
		eng := &recordingEngine{}
		g := guardClient{APIClient: eng}
		_, err := g.ContainerCreate(ctx, client.ContainerCreateOptions{
			Name:   name,
			Config: &container.Config{Image: "nginx", Labels: labels},
			NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
				net: {Aliases: []string{"web", name}},
			}},
		})
		if err != nil && len(eng.created) != 0 {
			t.Errorf("a refused create %q reached the engine", name)
		}
		return err
	}
	for _, tc := range []struct {
		name   string
		labels map[string]string
		net    string
		ok     bool
	}{
		{"shop-web-1", shopWebLabels, ingress.NetworkName, true},
		{"0123456789ab_shop-web-1", shopWebLabels, ingress.NetworkName, true},
		{"other-web-b86c19eb", shopWebLabels, ingress.NetworkName, false},
		{"shop-web-2", shopWebLabels, ingress.NetworkName, false},
		{"shop-web-1", map[string]string{composeServiceLabel: "web"}, ingress.NetworkName, false},
		{"other-web-b86c19eb", shopWebLabels, "shop_default", true},
	} {
		err := create(tc.name, tc.labels, tc.net)
		if tc.ok && err != nil {
			t.Errorf("create %q on %s: %v, want it forwarded", tc.name, tc.net, err)
		}
		if !tc.ok && !errors.Is(err, ErrIngressName) {
			t.Errorf("create %q on %s: %v, want ErrIngressName", tc.name, tc.net, err)
		}
	}

	eng := &recordingEngine{inspected: container.InspectResponse{Name: "/other-web-b86c19eb", Config: &container.Config{Labels: shopWebLabels}}}
	g := guardClient{APIClient: eng}
	_, err := g.NetworkConnect(ctx, ingress.NetworkName, client.NetworkConnectOptions{Container: "c1", EndpointConfig: &network.EndpointSettings{}})
	if !errors.Is(err, ErrIngressName) || len(eng.connected) != 0 {
		t.Errorf("connect of a chosen name: err %v, %d connects reached the engine; want ErrIngressName and none", err, len(eng.connected))
	}
}

// The guard judges the network the engine resolves, not the string a file used (T-25-47 re-audit): the ingress network
// named by its ID, an ID prefix or as the network mode is the ingress network, and a container there may not bring a
// hostname of its own or an alias other than its stack's. A user-defined network that is not the ingress network is free.
func TestGuardJudgesTheIngressNetworkHoweverItIsNamed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	own := ingress.Alias("shop", "web")
	type create struct {
		what     string
		name     string
		hostname string
		key      string
		mode     string
		aliases  []string
		ok       bool
	}
	for _, tc := range []create{
		{what: "the SDK's create with its own alias", name: "shop-web-1", key: ingress.NetworkName, aliases: []string{"web", "shop-web-1", own}, ok: true},
		{what: "a chosen name on the ingress network by ID", name: "other-web-b86c19eb", key: ingressID, ok: false},
		{what: "a chosen name on the ingress network by ID prefix", name: "other-web-b86c19eb", key: ingressID[:12], ok: false},
		{what: "a chosen name with the ingress network as network mode", name: "other-web-b86c19eb", mode: ingressID, ok: false},
		{what: "the SDK's name with the ingress network as network mode", name: "shop-web-1", mode: ingress.NetworkName, ok: true},
		{what: "a hostname of its own", name: "shop-web-1", hostname: "other-web-b86c19eb", key: ingress.NetworkName, ok: false},
		{what: "a hostname with the network as network mode", name: "shop-web-1", hostname: "other-web-b86c19eb", mode: ingress.NetworkName, ok: false},
		{what: "a foreign alias on the ingress network by ID", name: "shop-web-1", key: ingressID, aliases: []string{"other-web-b86c19eb"}, ok: false},
		{what: "a chosen name and a hostname off the ingress network", name: "other-web-b86c19eb", hostname: "x", key: "shop_default", aliases: []string{"anything"}, ok: true},
	} {
		eng := &recordingEngine{nets: map[string]string{"shop_default": "shop_default"}}
		g := guardClient{APIClient: eng}
		o := client.ContainerCreateOptions{
			Name:   tc.name,
			Config: &container.Config{Image: "nginx", Hostname: tc.hostname, Labels: shopWebLabels},
		}
		if tc.key != "" {
			o.NetworkingConfig = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{tc.key: {Aliases: tc.aliases}}}
		}
		if tc.mode != "" {
			o.HostConfig = &container.HostConfig{NetworkMode: container.NetworkMode(tc.mode)}
		}
		_, err := g.ContainerCreate(ctx, o)
		switch {
		case tc.ok && err != nil:
			t.Errorf("%s: %v, want it forwarded", tc.what, err)
		case !tc.ok && !errors.Is(err, ErrIngressName):
			t.Errorf("%s: %v, want ErrIngressName", tc.what, err)
		case !tc.ok && len(eng.created) != 0:
			t.Errorf("%s: a refused create reached the engine", tc.what)
		}
		if tc.ok && tc.key == ingress.NetworkName && err == nil {
			if got := eng.created[0].NetworkingConfig.EndpointsConfig[tc.key].Aliases; !reflect.DeepEqual(got, []string{own}) {
				t.Errorf("%s: aliases %v reached the engine, want only %s", tc.what, got, own)
			}
		}
	}

	// A connect names the network by ID: the container's hostname is its own ID's prefix (the engine's default) and passes;
	// one of its own is refused.
	connect := func(hostname string) error {
		eng := &recordingEngine{inspected: container.InspectResponse{
			ID: "abcdef0123456789abcdef", Name: "/shop-web-1",
			Config: &container.Config{Hostname: hostname, Labels: shopWebLabels},
		}}
		_, err := guardClient{APIClient: eng}.NetworkConnect(ctx, ingressID, client.NetworkConnectOptions{
			Container: "c1", EndpointConfig: &network.EndpointSettings{Aliases: []string{"web", "shop-web-1", own}},
		})
		return err
	}
	if err := connect("abcdef012345"); err != nil {
		t.Errorf("connect with the engine's default hostname: %v", err)
	}
	if err := connect("other-web-b86c19eb"); !errors.Is(err, ErrIngressName) {
		t.Errorf("connect with a hostname of its own: %v, want ErrIngressName", err)
	}
}

// A failure to read a network other than not-found stops the create: the guard fails closed.
func TestGuardFailsClosedWhenANetworkCannotBeRead(t *testing.T) {
	t.Parallel()
	eng := &failingInspect{recordingEngine: &recordingEngine{}}
	_, err := guardClient{APIClient: eng}.ContainerCreate(context.Background(), client.ContainerCreateOptions{
		Name:             "shop-web-1",
		Config:           &container.Config{Image: "nginx", Labels: shopWebLabels},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{"edge": {}}},
	})
	if err == nil || len(eng.created) != 0 {
		t.Fatalf("create with an unreadable network: err %v, %d creates reached the engine; want an error and none", err, len(eng.created))
	}
}

type failingInspect struct{ *recordingEngine }

func (failingInspect) NetworkInspect(context.Context, string, client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	return client.NetworkInspectResult{}, errors.New("engine unreachable")
}

// The engine looks a network up again when a container starts, by name and then by ID prefix, so a reference that is a
// prefix of the ingress network's ID is the ingress network even while another network answers to it (T-25-47 second
// re-audit: a decoy named so is removed between the create and the start). A reference the engine does not know stops
// the create.
func TestGuardTreatsAnIngressIDPrefixAsTheIngressNetwork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	decoy := ingressID[:12]
	eng := &recordingEngine{nets: map[string]string{decoy: "decoy"}}
	// The fake would answer the decoy name as the ingress network; make the decoy win, as a name does in the engine.
	g := guardClient{APIClient: decoyFirst{eng, decoy}}
	_, err := g.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:             "other-web-b86c19eb",
		Config:           &container.Config{Image: "nginx", Labels: shopWebLabels},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{decoy: {}}},
	})
	if !errors.Is(err, ErrIngressName) || len(eng.created) != 0 {
		t.Errorf("create on a decoy named as an ingress ID prefix: err %v, %d creates; want ErrIngressName and none", err, len(eng.created))
	}
	_, err = guardClient{APIClient: eng}.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:             "shop-web-1",
		Config:           &container.Config{Image: "nginx", Labels: shopWebLabels},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{"nowhere": {}}},
	})
	if err == nil || len(eng.created) != 0 {
		t.Errorf("create on an unknown network: err %v, %d creates; want an error and none", err, len(eng.created))
	}
}

// decoyFirst answers a network inspect of the decoy reference with the decoy, as the engine does when a network is named
// so, and anything else as the engine behind it.
type decoyFirst struct {
	*recordingEngine
	decoy string
}

func (d decoyFirst) NetworkInspect(ctx context.Context, ref string, o client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	if ref == d.decoy {
		return client.NetworkInspectResult{Network: network.Inspect{Network: network.Network{ID: "id-decoy", Name: "decoy"}}}, nil
	}
	return d.recordingEngine.NetworkInspect(ctx, ref, o)
}

// A decoy named with the start of the ingress ID and referenced by its own full ID is still the ingress network: the
// engine keeps the endpoint under the decoy's name and resolves that name to the ingress network once the decoy is gone
// (T-25-47, third re-audit). A network may not be created under such a name, and any other name is created.
func TestGuardTreatsANetworkNamedWithAnIngressIDPrefixAsTheIngressNetwork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	decoyName := ingressID[:9]
	eng := &recordingEngine{}
	g := guardClient{APIClient: decoyByID{eng, decoyName}}
	_, err := g.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       "other-web-b86c19eb",
		Config:     &container.Config{Image: "nginx", Labels: shopWebLabels},
		HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(decoyFullID)},
	})
	if !errors.Is(err, ErrIngressName) || len(eng.created) != 0 {
		t.Errorf("create on a decoy by its ID: err %v, %d creates; want ErrIngressName and none", err, len(eng.created))
	}
	_, err = g.NetworkConnect(ctx, decoyFullID, client.NetworkConnectOptions{Container: "c1", EndpointConfig: &network.EndpointSettings{}})
	if !errors.Is(err, ErrIngressName) || len(eng.connected) != 0 {
		t.Errorf("connect to a decoy by its ID: err %v; want ErrIngressName and no connect", err)
	}

	if _, err := g.NetworkCreate(ctx, decoyName, client.NetworkCreateOptions{}); !errors.Is(err, ErrIngressNetworkName) {
		t.Errorf("create a network named %s: %v, want ErrIngressNetworkName", decoyName, err)
	}
	if _, err := g.NetworkCreate(ctx, strings.ToUpper(decoyName), client.NetworkCreateOptions{}); !errors.Is(err, ErrIngressNetworkName) {
		t.Errorf("create a network named %s: %v, want ErrIngressNetworkName", strings.ToUpper(decoyName), err)
	}
	if _, err := g.NetworkCreate(ctx, "shop_default", client.NetworkCreateOptions{}); err != nil || len(eng.netCreated) != 1 {
		t.Errorf("create shop_default: %v, %d creates; want it forwarded", err, len(eng.netCreated))
	}
}

const decoyFullID = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

// decoyByID answers the decoy's full ID with the decoy, a network named name, and anything else as the engine behind it.
type decoyByID struct {
	*recordingEngine
	name string
}

func (d decoyByID) NetworkInspect(ctx context.Context, ref string, o client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	if ref == decoyFullID {
		return client.NetworkInspectResult{Network: network.Inspect{Network: network.Network{ID: decoyFullID, Name: d.name}}}, nil
	}
	return d.recordingEngine.NetworkInspect(ctx, ref, o)
}

func (r *recordingEngine) NetworkCreate(_ context.Context, name string, _ client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	r.netCreated = append(r.netCreated, name)
	return client.NetworkCreateResult{ID: "n-" + name}, nil
}

// A container whose endpoint is stored under the start of the ingress network's ID reaches that network when it starts
// (T-25-47, fourth re-audit): the start is refused unless it carries only a stack deploy's names, and allowed for a
// routed replica (an engine before API 1.45 lists its short ID among the aliases) and for a container off the network.
func TestStartIsRefusedForAForeignNameStoredUnderTheIngressNetwork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const cid = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	start := func(name, hostname string, labels map[string]string, nets map[string][]string, mode string) error {
		insp := container.InspectResponse{
			ID: cid, Name: "/" + name,
			Config:          &container.Config{Hostname: hostname, Labels: labels},
			HostConfig:      &container.HostConfig{NetworkMode: container.NetworkMode(mode)},
			NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{}},
		}
		for n, a := range nets {
			insp.NetworkSettings.Networks[n] = &network.EndpointSettings{Aliases: a}
		}
		eng := &recordingEngine{inspected: insp}
		return ingressStartRefusal(ctx, eng, cid)
	}
	decoy := ingressID[:1]
	if err := start("x", "abcdef012345", nil, map[string][]string{decoy: {uniqueAlias}}, "bridge"); !errors.Is(err, ErrIngressName) {
		t.Errorf("a chosen container on a decoy name: %v, want ErrIngressName", err)
	}
	if err := start("x", "abcdef012345", nil, nil, ingressID[:6]); !errors.Is(err, ErrIngressName) {
		t.Errorf("a chosen container with a decoy network mode: %v, want ErrIngressName", err)
	}
	if err := start("shop-web-1", "abcdef012345", shopWebLabels, map[string][]string{ingress.NetworkName: {"other-web-b86c19eb"}}, "bridge"); !errors.Is(err, ErrIngressName) {
		t.Errorf("a replica with a foreign alias: %v, want ErrIngressName", err)
	}
	if err := start("shop-web-1", "abcdef012345", shopWebLabels, map[string][]string{ingress.NetworkName: {uniqueAlias, "abcdef012345"}, "shop_default": {"web"}}, "shop_default"); err != nil {
		t.Errorf("a routed replica: %v, want it started", err)
	}
	if err := start("anything", "custom", nil, map[string][]string{"front": {"x"}}, "front"); err != nil {
		t.Errorf("a container off the ingress network: %v, want it started", err)
	}
}

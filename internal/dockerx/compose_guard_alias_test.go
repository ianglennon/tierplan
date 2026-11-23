// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx

import (
	"context"
	"reflect"
	"testing"

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

const uniqueAlias = "shop-web-4f2a9c1d7b3e"

func TestGuardStripsTheBareServiceAliasOnTheIngressNetwork(t *testing.T) {
	t.Parallel()
	eng := &recordingEngine{}
	g := guardClient{APIClient: eng}

	ingressEP := &network.EndpointSettings{Aliases: []string{"web", "shop-web-1", uniqueAlias}}
	defaultEP := &network.EndpointSettings{Aliases: []string{"web", "shop-web-1"}}
	in := client.ContainerCreateOptions{
		Name:   "shop-web-1",
		Config: &container.Config{Image: "nginx", Labels: map[string]string{composeServiceLabel: "web"}},
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
		Config: &container.Config{Image: "nginx", Labels: map[string]string{composeServiceLabel: "web"}},
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
		Config: &container.Config{Labels: map[string]string{composeServiceLabel: "web"}},
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

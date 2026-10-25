// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rebuild_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/rebuild"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// loadYAML reads a rebuilt file the way every deploy does.
func loadYAML(t *testing.T, name string, raw []byte) *stacks.Loaded {
	t.Helper()
	loaded, err := stacks.Load(testsupport.Context(t), stacks.LoadInput{
		Name: name, Target: stacks.TargetSwarm,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: raw}}},
	})
	if err != nil {
		t.Fatalf("Load: %v\n%s", err, raw)
	}
	return loaded
}

func coreService() rebuild.SwarmService {
	replicas := uint64(3)
	return rebuild.SwarmService{
		ID: "svc-web", Name: "shop_web",
		Spec: swarm.ServiceSpec{
			Annotations: swarm.Annotations{Name: "shop_web", Labels: map[string]string{
				"com.docker.stack.namespace": "shop", "com.docker.stack.image": "nginx:1.27",
				"bayplan.stack.id": "stack-1", "team": "web",
			}},
			TaskTemplate: swarm.TaskSpec{
				ContainerSpec: &swarm.ContainerSpec{
					Image: "nginx:1.27@" + digest,
					Labels: map[string]string{
						"com.docker.stack.namespace": "shop", "bayplan.stack.id": "stack-1", "tier": "front",
					},
					Command: []string{"/entry.sh"},
					Args:    []string{"nginx", "-g", "daemon off;"},
					Env:     []string{"PW=pa$word", "MODE=prod"},
				},
				Networks: []swarm.NetworkAttachmentConfig{{Target: "shop_default", Aliases: []string{"web"}}},
			},
			Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
			EndpointSpec: &swarm.EndpointSpec{Mode: swarm.ResolutionModeVIP, Ports: []swarm.PortConfig{
				{Protocol: "tcp", TargetPort: 80, PublishedPort: 8080, PublishMode: swarm.PortConfigPublishModeIngress},
			}},
		},
	}
}

func TestSwarmRebuildsTheCore(t *testing.T) {
	t.Parallel()
	res, err := rebuild.Swarm(rebuild.SwarmInput{
		Namespace: "shop",
		Services:  []rebuild.SwarmService{coreService()},
		Networks:  []rebuild.NetworkInfo{{ID: "n1", Name: "shop_default", Driver: "overlay", Labels: map[string]string{"com.docker.stack.namespace": "shop"}}},
	})
	if err != nil {
		t.Fatalf("Swarm: %v", err)
	}

	text := string(res.YAML)
	if got := res.Pins["shop_web"]; got != "nginx:1.27@"+digest {
		t.Errorf("pin = %q, want the full image of the spec", got)
	}
	for _, want := range []string{"image: nginx:1.27\n", `"8080:80"`, "replicas: 3", "tier: front", "team: web", "PW: pa$$word"} {
		if !strings.Contains(text, want) {
			t.Errorf("the file lacks %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"sha256", "com.docker.stack", "bayplan", "networks:", "endpoint_mode", "mode: replicated", "shop_web"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("the file has %q, which a hand-written file would not:\n%s", unwanted, text)
		}
	}
	if len(res.Notes) != 0 {
		t.Errorf("notes = %+v, want none for a stack deployed by docker stack deploy", res.Notes)
	}

	svc := loadYAML(t, "shop", res.YAML).Project.Services["web"]
	if svc.Image != "nginx:1.27" || len(svc.Entrypoint) != 1 || len(svc.Command) != 3 {
		t.Errorf("service = image %q entrypoint %v command %v", svc.Image, svc.Entrypoint, svc.Command)
	}
	if pw := svc.Environment["PW"]; pw == nil || *pw != "pa$word" {
		t.Errorf("PW loads as %v, want pa$word", pw)
	}
	if svc.Deploy == nil || svc.Deploy.Replicas == nil || *svc.Deploy.Replicas != 3 {
		t.Errorf("deploy = %+v, want 3 replicas", svc.Deploy)
	}
}

func TestSwarmKeepsAServiceWithoutThePrefix(t *testing.T) {
	t.Parallel()
	svc := coreService()
	svc.Name = "web-standalone"

	res, err := rebuild.Swarm(rebuild.SwarmInput{Namespace: "shop", Services: []rebuild.SwarmService{svc}})
	if err != nil {
		t.Fatalf("Swarm: %v", err)
	}

	if _, ok := res.Project.Services["web-standalone"]; !ok {
		t.Errorf("services = %v, want the service under its own name", res.Project.Services)
	}
	if len(res.Notes) != 1 || res.Notes[0].Code != rebuild.NoteServiceNamePrefix || res.Notes[0].Service != "web-standalone" {
		t.Errorf("notes = %+v, want one service_name_prefix", res.Notes)
	}
}

// TestRebuiltYAMLOmitsEngineDefaults: what the engine fills in is not written, so the file reads like a hand-written
// one, and the round trip shows nothing is lost by leaving it out.
func TestRebuiltYAMLOmitsEngineDefaults(t *testing.T) {
	t.Parallel()
	one := uint64(1)
	svc := coreService()
	svc.Spec.Mode = swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &one}}
	svc.Spec.EndpointSpec = &swarm.EndpointSpec{Mode: swarm.ResolutionModeVIP, Ports: []swarm.PortConfig{
		{Protocol: "tcp", TargetPort: 80, PublishedPort: 8080, PublishMode: swarm.PortConfigPublishModeIngress},
	}}
	svc.Spec.UpdateConfig = &swarm.UpdateConfig{Parallelism: 2, FailureAction: swarm.UpdateFailureActionPause, Order: swarm.UpdateOrderStopFirst}
	svc.Spec.TaskTemplate.ContainerSpec.Isolation = "default"
	svc.Spec.TaskTemplate.Placement = &swarm.Placement{}
	svc.Spec.TaskTemplate.Resources = &swarm.ResourceRequirements{}

	res, err := rebuild.Swarm(rebuild.SwarmInput{
		Namespace: "shop", Services: []rebuild.SwarmService{svc},
		Networks: []rebuild.NetworkInfo{{ID: "n1", Name: "shop_default", Driver: "overlay", Labels: map[string]string{"com.docker.stack.namespace": "shop"}}},
	})
	if err != nil {
		t.Fatalf("Swarm: %v", err)
	}

	text := string(res.YAML)
	if !strings.Contains(text, "parallelism: 2") {
		t.Errorf("the update policy lost the one value that is not a default:\n%s", text)
	}
	for _, unwanted := range []string{
		"replicas", "endpoint_mode", "protocol", "mode: ingress", "failure_action", "order:", "isolation",
		"networks", "placement", "resources",
	} {
		if strings.Contains(text, unwanted) {
			t.Errorf("the file has %q, which is what the engine fills in:\n%s", unwanted, text)
		}
	}
}

func TestRebuildNotes(t *testing.T) {
	t.Parallel()
	unprefixed := coreService()
	unprefixed.Name = "legacy-web"
	unprefixed.Spec.TaskTemplate.ContainerSpec.Image = "nginx:1.27"
	unprefixed.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "shop_lan"}}
	owned := map[string]string{"com.docker.stack.namespace": "shop"}

	res, err := rebuild.Swarm(rebuild.SwarmInput{
		Namespace: "shop", Services: []rebuild.SwarmService{unprefixed},
		Networks: []rebuild.NetworkInfo{{ID: "n1", Name: "shop_lan", Driver: "macvlan", Labels: owned}},
	})
	if err != nil {
		t.Fatalf("Swarm: %v", err)
	}

	got := map[string]string{}
	for _, n := range res.Notes {
		got[n.Code] = n.Service
		if n.Message == "" {
			t.Errorf("the note %s has no sentence", n.Code)
		}
	}
	want := map[string]string{
		rebuild.NoteServiceNamePrefix: "legacy-web",
		rebuild.NoteImageUnpinned:     "legacy-web",
		rebuild.NoteNetworkNotOverlay: "",
	}
	for code, service := range want {
		if s, ok := got[code]; !ok || s != service {
			t.Errorf("notes = %+v, want %s about %q", res.Notes, code, service)
		}
	}
	if got := res.Pins["legacy-web"]; got != "nginx:1.27" {
		t.Errorf("pin = %q, want the reference as it is", got)
	}
	nets := loadYAML(t, "shop", res.YAML).Project.Networks
	var lan *types.NetworkConfig
	for _, n := range nets {
		if n.Name == "shop_lan" {
			lan = &n
		}
	}
	if len(nets) != 1 || lan == nil || !bool(lan.External) {
		t.Errorf("networks = %+v, want shop_lan external under its own name", nets)
	}
	var kinds []string
	for _, o := range res.Objects {
		kinds = append(kinds, o.Kind+"/"+o.Name+"/"+o.Treatment)
	}
	if !slices.Contains(kinds, "network/shop_lan/external") {
		t.Errorf("objects = %v, want the macvlan network listed as external", kinds)
	}
}

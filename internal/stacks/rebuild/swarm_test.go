// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package rebuild_test

import (
	"strings"
	"testing"

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

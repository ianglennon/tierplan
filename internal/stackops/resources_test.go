// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"slices"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The Resources tab of a stack (plan 07-25, STK-10, D-22): what the stack owns on its environment, found by
// Bayplan's labels. Docker is a fake engine behind the real aggregator and cluster reader.

var resourcesT0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// resourcesTask builds a task created age before resourcesT0.
func resourcesTask(id, service, node string, slot int, state, desired string, age time.Duration) swarm.Task {
	t := swarm.Task{ID: id, ServiceID: service, NodeID: node, Slot: slot, DesiredState: swarm.TaskState(desired)}
	t.CreatedAt = resourcesT0.Add(-age)
	t.UpdatedAt = resourcesT0.Add(-age + time.Second)
	t.Status = swarm.TaskStatus{Timestamp: resourcesT0.Add(-age + 2*time.Second), State: swarm.TaskState(state)}
	return t
}

func ownedBy(ns, id string) map[string]string {
	return map[string]string{"com.docker.stack.namespace": ns, "bayplan.stack.id": id}
}

func TestResourcesOfASwarmStackListTheCurrentTaskOfEachSlot(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.TasksFollowWrites = false
		c.NodeRows = append(c.NodeRows, dockertest.FakeNode{ID: "node-2", Hostname: "wrk-1", Role: "worker", EngineVersion: "29.1.0", OS: "linux"})
		c.Tasks = []swarm.Task{
			resourcesTask("t-app-1-old", "svc-app", "node-1", 1, "failed", "shutdown", 10*time.Minute),
			resourcesTask("t-app-1", "svc-app", "node-1", 1, "running", "running", time.Minute),
			resourcesTask("t-app-2", "svc-app", "node-2", 2, "running", "running", time.Minute),
			resourcesTask("t-app-3-gone", "svc-app", "node-2", 3, "shutdown", "shutdown", 20*time.Minute),
			resourcesTask("t-db-1", "svc-db", "node-1", 1, "running", "running", time.Minute),
			resourcesTask("t-agent-1", "svc-agent", "node-1", 0, "running", "running", time.Minute),
			resourcesTask("t-agent-2", "svc-agent", "node-2", 0, "running", "running", time.Minute),
			resourcesTask("t-other-1", "svc-other", "node-1", 1, "running", "running", time.Minute),
		}
	},
		dockertest.FakeService{ID: "svc-app", Name: "web_app", Mode: "replicated", Running: 2, Desired: 2, Image: "x", Labels: ownedBy("web", "stack-web")},
		dockertest.FakeService{ID: "svc-db", Name: "web_db", Mode: "replicated", Running: 1, Desired: 1, Image: "y", Labels: ownedBy("web", "stack-web")},
		dockertest.FakeService{ID: "svc-agent", Name: "web_agent", Mode: "global", Running: 2, Desired: 2, Image: "z", Labels: ownedBy("web", "stack-web")},
		dockertest.FakeService{ID: "svc-other", Name: "blog_app", Mode: "replicated", Running: 1, Desired: 1, Image: "w", Labels: ownedBy("blog", "stack-blog")},
	)

	res := rig.deps.Resources(testsupport.Context(t), rig.envID, swarmStack())

	if res.Tasks.Error != "" {
		t.Fatalf("tasks error = %q", res.Tasks.Error)
	}
	var got []string
	for _, r := range res.Tasks.Items {
		got = append(got, r.Task.ID)
	}
	want := []string{"t-agent-1", "t-agent-2", "t-app-1", "t-app-2", "t-db-1"}
	if !slices.Equal(got, want) {
		t.Fatalf("tasks = %v, want the current task of each slot or node of this stack's services, by service then slot: %v", got, want)
	}
	agent, app1, app2 := res.Tasks.Items[0], res.Tasks.Items[2], res.Tasks.Items[3]
	if agent.Service != "agent" || !agent.Global || agent.ServiceID != "svc-agent" {
		t.Errorf("agent = %+v", agent)
	}
	if app1.Service != "app" || app1.Global || app1.Task.Slot != 1 || app1.Node.Hostname != "mgr-1" || !app1.NodeListed {
		t.Errorf("app.1 = %+v", app1)
	}
	if app2.Node.Hostname != "wrk-1" || !app2.NodeListed {
		t.Errorf("app.2 = %+v", app2)
	}
	if len(res.Containers.Items) != 0 || res.Containers.Error != "" {
		t.Errorf("a swarm stack has no containers card: %+v", res.Containers)
	}
}

func TestResourcesOfASwarmStackWithNoServicesAreAnEmptyTasksCardNotAnError(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)

	res := rig.deps.Resources(testsupport.Context(t), rig.envID, swarmStack())

	if res.Tasks.Error != "" || res.Tasks.Items == nil || len(res.Tasks.Items) != 0 {
		t.Errorf("tasks = %+v, want an empty card", res.Tasks)
	}
	if res.Networks.Error != "" || res.Networks.Items == nil || len(res.Networks.Items) != 0 {
		t.Errorf("networks = %+v, want an empty card", res.Networks)
	}
}

func TestResourcesOfASwarmStackListTheNetworksCarryingTheStacksLabelsAsOwnedByIt(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{
			{ID: "n-default", Name: "web_default", Driver: "overlay", Scope: "swarm", Labels: ownedBy("web", "stack-web")},
			{ID: "n-back", Name: "web_back", Driver: "overlay", Scope: "swarm", Labels: ownedBy("web", "stack-web")},
			// The same namespace without the stack's ID is somebody else's, and so is another stack's.
			{ID: "n-hand", Name: "web_hand", Driver: "overlay", Scope: "swarm", Labels: map[string]string{"com.docker.stack.namespace": "web"}},
			{ID: "n-blog", Name: "blog_default", Driver: "overlay", Scope: "swarm", Labels: ownedBy("blog", "stack-blog")},
			{ID: "n-shared", Name: "shared", Driver: "overlay", Scope: "swarm"},
		}
	})

	res := rig.deps.Resources(testsupport.Context(t), rig.envID, swarmStack())

	if res.Networks.Error != "" {
		t.Fatalf("networks error = %q", res.Networks.Error)
	}
	if len(res.Networks.Items) != 2 {
		t.Fatalf("networks = %+v, want the two the stack owns", res.Networks.Items)
	}
	back, def := res.Networks.Items[0], res.Networks.Items[1]
	if back.Name != "web_back" || def.Name != "web_default" || def.ID != "n-default" || def.Driver != "overlay" || def.Scope != "swarm" {
		t.Errorf("networks = %+v", res.Networks.Items)
	}
	for _, n := range res.Networks.Items {
		if n.Ownership != stackops.OwnerStack {
			t.Errorf("%s ownership = %q, want stack", n.Name, n.Ownership)
		}
	}
}

func TestResourcesOfAStandaloneStackListItsContainersAndNetworks(t *testing.T) {
	t.Parallel()
	h := hostStack(t, "demo", hostedCompose, nil)
	h.Containers = append(h.Containers, dockertest.FakeContainer{
		ID: "oneoff-1", Names: []string{"/demo-web-run-1"}, Image: "nginx", State: "running",
		Labels: map[string]string{"com.docker.compose.project": "demo", "com.docker.compose.service": "web", "bayplan.stack.id": "stack-demo", "com.docker.compose.oneoff": "True"},
	}, dockertest.FakeContainer{
		ID: "other-1", Names: []string{"/other-web-1"}, Image: "nginx", State: "running",
		Labels: map[string]string{"com.docker.compose.project": "other", "com.docker.compose.service": "web", "bayplan.stack.id": "stack-other"},
	}, dockertest.FakeContainer{
		ID: "foreign-1", Names: []string{"/demo-foreign-1"}, Image: "nginx", State: "running",
		Labels: map[string]string{"com.docker.compose.project": "demo", "com.docker.compose.service": "foreign"},
	})
	h.Networks = append(h.Networks, dockertest.FakeNetwork{ID: "n-foreign", Name: "demo_hand", Driver: "bridge", Scope: "local", Labels: map[string]string{"com.docker.compose.project": "demo"}})
	rig := hostRig(t, h)

	res := rig.deps.Resources(testsupport.Context(t), rig.envID, standaloneStack("demo"))

	if res.Containers.Error != "" {
		t.Fatalf("containers error = %q", res.Containers.Error)
	}
	if len(res.Containers.Items) != 2 || res.Containers.Items[0].Name != "demo-db-1" || res.Containers.Items[1].Name != "demo-web-1" {
		t.Fatalf("containers = %+v, want the two services' containers by name, no one-off and none of another project", res.Containers.Items)
	}
	web := res.Containers.Items[1]
	if web.ID != h.IDs["web"] || web.Service != "web" || web.State != "running" || web.Image != h.Pins["web"] {
		t.Errorf("web = %+v", web)
	}
	if len(res.Tasks.Items) != 0 || res.Tasks.Error != "" {
		t.Errorf("a standalone stack has no tasks card: %+v", res.Tasks)
	}
	if res.Networks.Error != "" || len(res.Networks.Items) != 1 || res.Networks.Items[0].Name != "demo_default" || res.Networks.Items[0].Ownership != stackops.OwnerStack {
		t.Errorf("networks = %+v, want the project's own network alone", res.Networks)
	}
}

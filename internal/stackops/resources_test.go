// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/envs"
	"github.com/ianglennon/coaming/internal/stackops"
	"github.com/ianglennon/coaming/internal/stacks"
	"github.com/ianglennon/coaming/proto/testsupport"
)

// The Resources tab of a stack (plan 07-25, STK-10, D-22): what the stack owns on its environment, found by
// Coaming's labels. Docker is a fake engine behind the real aggregator and cluster reader.

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
	return map[string]string{"com.docker.stack.namespace": ns, "coaming.stack.id": id}
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

	res := rig.deps.Resources(testsupport.Context(t), rig.envID, swarmStack(), stackops.Declared{})

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

	res := rig.deps.Resources(testsupport.Context(t), rig.envID, swarmStack(), stackops.Declared{})

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

	res := rig.deps.Resources(testsupport.Context(t), rig.envID, swarmStack(), stackops.Declared{})

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
		Labels: map[string]string{"com.docker.compose.project": "demo", "com.docker.compose.service": "web", "coaming.stack.id": "stack-demo", "com.docker.compose.oneoff": "True"},
	}, dockertest.FakeContainer{
		ID: "other-1", Names: []string{"/other-web-1"}, Image: "nginx", State: "running",
		Labels: map[string]string{"com.docker.compose.project": "other", "com.docker.compose.service": "web", "coaming.stack.id": "stack-other"},
	}, dockertest.FakeContainer{
		ID: "foreign-1", Names: []string{"/demo-foreign-1"}, Image: "nginx", State: "running",
		Labels: map[string]string{"com.docker.compose.project": "demo", "com.docker.compose.service": "foreign", "coaming.stack.id": "stack-stranger"},
	})
	h.Networks = append(h.Networks, dockertest.FakeNetwork{ID: "n-foreign", Name: "demo_hand", Driver: "bridge", Scope: "local", Labels: map[string]string{"com.docker.compose.project": "demo"}})
	rig := hostRig(t, h)

	res := rig.deps.Resources(testsupport.Context(t), rig.envID, standaloneStack("demo"), stackops.Declared{})

	if res.Containers.Error != "" {
		t.Fatalf("containers error = %q", res.Containers.Error)
	}
	if len(res.Containers.Items) != 2 || res.Containers.Items[0].Name != "demo-db-1" || res.Containers.Items[1].Name != "demo-web-1" {
		t.Fatalf("containers = %+v, want the two services' containers by name, no one-off, none of another project and none of another stack's (a container with no ID is TestResourcesOfAStandaloneStackListAContainerTheHostCLIAdded)", res.Containers.Items)
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

// TestResourcesOfAStandaloneStackListAContainerTheHostCLIAdded: docker compose up --scale web=2 adds web-2 to the
// project with none of Coaming's labels. Beside web-1, which carries the stack's ID, the Resources tab lists it
// (G-08-6, user decision 2026-10-27). A container with another stack's ID, and the containers of a project the stack
// owns none of, stay out (P7 D-12, T-08-47).
func TestResourcesOfAStandaloneStackListAContainerTheHostCLIAdded(t *testing.T) {
	t.Parallel()
	names := func(res stackops.Resources) []string {
		out := make([]string, 0, len(res.Containers.Items))
		for _, c := range res.Containers.Items {
			out = append(out, c.Name)
		}
		return out
	}

	h := hostStack(t, "demo", hostedCompose, nil)
	h.Containers = append(h.Containers, cliContainer(t, h, hostedCompose, "web", 2))
	stranger := cliContainer(t, h, hostedCompose, "web", 3)
	stranger.Labels[stacks.LabelStackID] = "stack-other"
	h.Containers = append(h.Containers, stranger)
	rig := hostRig(t, h)

	res := rig.deps.Resources(testsupport.Context(t), rig.envID, standaloneStack("demo"), stackops.Declared{})

	if res.Containers.Error != "" {
		t.Fatalf("containers error = %q", res.Containers.Error)
	}
	if got, want := names(res), []string{"demo-db-1", "demo-web-1", "demo-web-2"}; !slices.Equal(got, want) {
		t.Errorf("containers = %v, want %v: the one the host CLI added is listed, another stack's is not", got, want)
	}

	// A project the stack owns nothing of: the stack's records carry no container of it, so none is listed.
	bare := hostStack(t, "demo", hostedCompose, nil)
	bare.Containers = []dockertest.FakeContainer{cliContainer(t, h, hostedCompose, "web", 2)}
	bareRig := hostRig(t, bare)
	if res := bareRig.deps.Resources(testsupport.Context(t), bareRig.envID, standaloneStack("demo"), stackops.Declared{}); len(res.Containers.Items) != 0 {
		t.Errorf("containers = %v, want none: a project the stack owns nothing of is not taken over", names(res))
	}
}

// declaredCompose is a deployed revision that uses two networks, three volumes, a secret, an external config and
// an external volume, and declares a volume and a secret no service uses.
const declaredCompose = `services:
  web:
    image: nginx:1.27
    networks: [default, proxy]
    volumes:
      - data:/data
      - shared:/shared
    secrets: [tok]
    configs: [cfg]
  db:
    image: postgres:16
    volumes: [data:/var/lib/postgresql/data]
    secrets: [tok]
networks:
  proxy:
    external: true
volumes:
  data: {}
  shared:
    external: true
    name: real-shared
  spare: {}
secrets:
  tok:
    environment: TOK
  spare:
    environment: TOK
configs:
  cfg:
    external: true
    name: real-cfg
`

func declaredFrom(t *testing.T, target stacks.Target, compose string) stackops.Declared {
	t.Helper()
	loaded, err := stacks.Load(context.Background(), stacks.LoadInput{
		Name: "demo", Target: target, Copy: stacks.WorkingCopy{
			Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}},
			Vars:  []stacks.Var{{Name: "TOK", Value: "x"}},
		},
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return stackops.DeclaredOf(loaded.Project)
}

func byKey(objs []stackops.DeclaredObject, key string) (stackops.DeclaredObject, bool) {
	i := slices.IndexFunc(objs, func(o stackops.DeclaredObject) bool { return o.Key == key })
	if i < 0 {
		return stackops.DeclaredObject{}, false
	}
	return objs[i], true
}

func TestDeclaredOfListsTheNetworksVolumesSecretsAndConfigsTheServicesUse(t *testing.T) {
	t.Parallel()
	decl := declaredFrom(t, stacks.TargetSwarm, declaredCompose)

	if !decl.Known || decl.Err != nil {
		t.Fatalf("declared = %+v", decl)
	}
	if proxy, ok := byKey(decl.Networks, "proxy"); !ok || !proxy.External || proxy.Name != "proxy" || !slices.Equal(proxy.UsedBy, []string{"web"}) {
		t.Errorf("proxy = %+v (found %v)", proxy, ok)
	}
	if def, ok := byKey(decl.Networks, "default"); !ok || def.External || !slices.Equal(def.UsedBy, []string{"db", "web"}) {
		t.Errorf("default = %+v (found %v)", def, ok)
	}
	if data, ok := byKey(decl.Volumes, "data"); !ok || data.External || !slices.Equal(data.UsedBy, []string{"db", "web"}) {
		t.Errorf("data = %+v (found %v)", data, ok)
	}
	if shared, ok := byKey(decl.Volumes, "shared"); !ok || !shared.External || shared.Name != "real-shared" {
		t.Errorf("shared = %+v (found %v)", shared, ok)
	}
	if _, ok := byKey(decl.Volumes, "spare"); ok {
		t.Errorf("a volume no service uses is not the stack's to list: %+v", decl.Volumes)
	}
	if tok, ok := byKey(decl.Secrets, "tok"); !ok || tok.External || !slices.Equal(tok.UsedBy, []string{"db", "web"}) {
		t.Errorf("tok = %+v (found %v)", tok, ok)
	}
	if _, ok := byKey(decl.Secrets, "spare"); ok {
		t.Errorf("a secret no service uses is not listed: %+v", decl.Secrets)
	}
	if cfg, ok := byKey(decl.Configs, "cfg"); !ok || !cfg.External || cfg.Name != "real-cfg" || !slices.Equal(cfg.UsedBy, []string{"web"}) {
		t.Errorf("cfg = %+v (found %v)", cfg, ok)
	}
}

func volumeRow(name, node, nodeID string, labels map[string]string) envs.VolumeRow {
	return envs.VolumeRow{VolumeRow: dockerx.VolumeRow{Name: name, Driver: "local", Labels: labels}, Node: node, NodeID: nodeID}
}

func TestVolumesFromGivesOneRowForEachNodeThatHoldsAVolumeOfTheStack(t *testing.T) {
	t.Parallel()
	decl := declaredFrom(t, stacks.TargetSwarm, declaredCompose)
	agg := envs.Aggregate{
		Volumes: []envs.VolumeRow{
			volumeRow("web_data", "wrk-1", "n2", ownedBy("web", "stack-web")),
			volumeRow("web_data", "mgr-1", "n1", ownedBy("web", "stack-web")),
			// The same name on a node where nobody labelled it, another stack's volume, and an unrelated one.
			volumeRow("web_data", "wrk-3", "n3", nil),
			volumeRow("blog_data", "mgr-1", "n1", ownedBy("blog", "stack-blog")),
			volumeRow("scratch", "mgr-1", "n1", nil),
			// The external volume the file refers to, by its real name, on two nodes.
			volumeRow("real-shared", "mgr-1", "n1", nil),
			volumeRow("real-shared", "wrk-1", "n2", nil),
		},
		NotReporting: []string{"wrk-2"},
	}

	rows, notReporting := stackops.VolumesFrom(swarmStack(), decl, agg)

	type row struct{ name, node, owner string }
	var got []row
	for _, r := range rows {
		got = append(got, row{r.Name, r.Node, r.Ownership})
	}
	want := []row{
		{"real-shared", "mgr-1", stackops.OwnerExternal},
		{"real-shared", "wrk-1", stackops.OwnerExternal},
		{"web_data", "mgr-1", stackops.OwnerStack},
		{"web_data", "wrk-1", stackops.OwnerStack},
	}
	if !slices.Equal(got, want) {
		t.Errorf("volumes = %v, want %v: one row for each node, the stack's own by its labels and an external by the name the file gives", got, want)
	}
	if len(rows) == 4 && (rows[2].NodeID != "n1" || rows[2].Driver != "local") {
		t.Errorf("row = %+v", rows[2])
	}
	if !slices.Equal(notReporting, []string{"wrk-2"}) {
		t.Errorf("notReporting = %v, want the node whose volumes are left out", notReporting)
	}
}

func TestVolumesFromNeverCallsAnExternalVolumeTheStacksOwn(t *testing.T) {
	t.Parallel()
	// Without the stack's labels a volume is not the stack's, whatever it is called, and one the file declares
	// external stays external even when it carries the labels of another stack.
	decl := declaredFrom(t, stacks.TargetSwarm, declaredCompose)
	agg := envs.Aggregate{Volumes: []envs.VolumeRow{
		volumeRow("real-shared", "mgr-1", "n1", ownedBy("blog", "stack-blog")),
		volumeRow("web_data", "mgr-1", "n1", map[string]string{"com.docker.stack.namespace": "web"}),
	}}

	rows, _ := stackops.VolumesFrom(swarmStack(), decl, agg)

	if len(rows) != 1 || rows[0].Name != "real-shared" || rows[0].Ownership != stackops.OwnerExternal {
		t.Errorf("volumes = %+v, want the external volume alone, External", rows)
	}
}

func TestResourcesOfASwarmStackReadAHashedSecretAsItsNameAndVersionWithItsUsers(t *testing.T) {
	t.Parallel()
	hashed := func(name, hash string) map[string]string {
		l := ownedBy("web", "stack-web")
		l["coaming.stack.object"], l["coaming.stack.object.hash"] = name, hash
		return l
	}
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.TasksFollowWrites = false
		c.Secrets = []dockertest.FakeSecret{
			{ID: "sec-tok", Name: "web_tok_1a2b3c4d", Labels: hashed("tok", "1a2b3c4d")},
			{ID: "sec-tok-old", Name: "web_tok_0000aaaa", Labels: hashed("tok", "0000aaaa")},
			{ID: "sec-ext", Name: "shared_cert"},
			{ID: "sec-hand", Name: "web_hand", Labels: map[string]string{"com.docker.stack.namespace": "web"}},
			{ID: "sec-blog", Name: "blog_tok_1111bbbb", Labels: ownedBy("blog", "stack-blog")},
			{ID: "sec-free", Name: "unrelated"},
		}
	},
		dockertest.FakeService{ID: "svc-app", Name: "web_app", Mode: "replicated", Running: 1, Desired: 1, Image: "x", Labels: ownedBy("web", "stack-web"),
			Secrets: []dockertest.FakeServiceObject{{ID: "sec-tok", Name: "web_tok_1a2b3c4d", File: "tok"}, {ID: "sec-ext", Name: "shared_cert", File: "cert"}}},
		dockertest.FakeService{ID: "svc-db", Name: "web_db", Mode: "replicated", Running: 1, Desired: 1, Image: "y", Labels: ownedBy("web", "stack-web"),
			Secrets: []dockertest.FakeServiceObject{{ID: "sec-tok", Name: "web_tok_1a2b3c4d", File: "tok"}}},
		// A service of another stack that uses the same external secret is not this stack's user of it.
		dockertest.FakeService{ID: "svc-blog", Name: "blog_app", Mode: "replicated", Running: 1, Desired: 1, Image: "z", Labels: ownedBy("blog", "stack-blog"),
			Secrets: []dockertest.FakeServiceObject{{ID: "sec-ext", Name: "shared_cert", File: "cert"}}},
	)
	decl := stackops.Declared{Known: true, Secrets: []stackops.DeclaredObject{
		{Key: "tok", Name: "tok", UsedBy: []string{"app", "db"}},
		{Key: "cert", Name: "shared_cert", External: true, UsedBy: []string{"app"}},
	}}

	res := rig.deps.Resources(testsupport.Context(t), rig.envID, swarmStack(), decl)

	if res.Secrets.Error != "" {
		t.Fatalf("secrets error = %q", res.Secrets.Error)
	}
	type row struct {
		name, full, hash, owner string
		users                   []string
	}
	var got []row
	for _, r := range res.Secrets.Items {
		var users []string
		for _, u := range r.UsedBy {
			users = append(users, u.Name)
		}
		got = append(got, row{r.Name, r.FullName, r.Hash, r.Ownership, users})
	}
	want := []row{
		{"shared_cert", "shared_cert", "", stackops.OwnerExternal, []string{"app"}},
		{"tok", "web_tok_0000aaaa", "0000aaaa", stackops.OwnerStack, nil},
		{"tok", "web_tok_1a2b3c4d", "1a2b3c4d", stackops.OwnerStack, []string{"app", "db"}},
	}
	if len(got) != len(want) {
		t.Fatalf("secrets = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].name != want[i].name || got[i].full != want[i].full || got[i].hash != want[i].hash || got[i].owner != want[i].owner || !slices.Equal(got[i].users, want[i].users) {
			t.Errorf("secret %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if res.Secrets.Items[2].ID != "sec-tok" || res.Secrets.Items[2].UsedBy[0].ID != "svc-app" {
		t.Errorf("tok = %+v, want the secret's ID and its users' service IDs", res.Secrets.Items[2])
	}
}

func TestResourcesOfASwarmStackListAConfigTheSameWay(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.TasksFollowWrites = false
		c.Configs = []dockertest.FakeConfigObject{
			{ID: "cfg-site", Name: "web_site_9f8e7d6c", Labels: map[string]string{
				"com.docker.stack.namespace": "web", "coaming.stack.id": "stack-web", "coaming.stack.object": "site", "coaming.stack.object.hash": "9f8e7d6c",
			}},
			{ID: "cfg-other", Name: "other"},
		}
	},
		dockertest.FakeService{ID: "svc-app", Name: "web_app", Mode: "replicated", Running: 1, Desired: 1, Image: "x", Labels: ownedBy("web", "stack-web"),
			Configs: []dockertest.FakeServiceObject{{ID: "cfg-site", Name: "web_site_9f8e7d6c", File: "/etc/site"}}},
	)

	res := rig.deps.Resources(testsupport.Context(t), rig.envID, swarmStack(), stackops.Declared{Known: true})

	if res.Configs.Error != "" || len(res.Configs.Items) != 1 {
		t.Fatalf("configs = %+v", res.Configs)
	}
	site := res.Configs.Items[0]
	if site.Name != "site" || site.Hash != "9f8e7d6c" || site.FullName != "web_site_9f8e7d6c" || site.Ownership != stackops.OwnerStack || len(site.UsedBy) != 1 || site.UsedBy[0].Name != "app" {
		t.Errorf("site = %+v", site)
	}
}

func TestResourcesOfASwarmStackListAnExternalNetworkAsExternalAndLeaveOutOneThatIsNotThere(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{
			{ID: "n-default", Name: "web_default", Driver: "overlay", Scope: "swarm", Labels: ownedBy("web", "stack-web")},
			{ID: "n-proxy", Name: "proxy", Driver: "overlay", Scope: "swarm"},
			{ID: "n-other", Name: "elsewhere", Driver: "overlay", Scope: "swarm"},
		}
	})
	decl := stackops.Declared{Known: true, Networks: []stackops.DeclaredObject{
		{Key: "default", Name: "default", UsedBy: []string{"app"}},
		{Key: "proxy", Name: "proxy", External: true, UsedBy: []string{"app"}},
		{Key: "gone", Name: "was-deleted", External: true, UsedBy: []string{"app"}},
	}}

	res := rig.deps.Resources(testsupport.Context(t), rig.envID, swarmStack(), decl)

	type row struct{ name, owner string }
	var got []row
	for _, n := range res.Networks.Items {
		got = append(got, row{n.Name, n.Ownership})
	}
	want := []row{{"proxy", stackops.OwnerExternal}, {"web_default", stackops.OwnerStack}}
	if !slices.Equal(got, want) {
		t.Errorf("networks = %v, want %v", got, want)
	}
}

func TestAnObjectTheStackDoesNotLabelIsNeverCalledTheStacksOwn(t *testing.T) {
	t.Parallel()
	// A network, a secret and a config that the file refers to as external, and others under the stack's name that
	// carry no labels: none of them reads Stack.
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{
			{ID: "n-proxy", Name: "proxy", Driver: "overlay", Scope: "swarm"},
			{ID: "n-web", Name: "web_hand", Driver: "overlay", Scope: "swarm"},
		}
		c.Secrets = []dockertest.FakeSecret{{ID: "sec-ext", Name: "shared_cert"}, {ID: "sec-web", Name: "web_cert"}}
		c.Configs = []dockertest.FakeConfigObject{{ID: "cfg-ext", Name: "shared_cfg"}, {ID: "cfg-web", Name: "web_cfg"}}
	})
	decl := stackops.Declared{Known: true,
		Networks: []stackops.DeclaredObject{{Key: "proxy", Name: "proxy", External: true}},
		Secrets:  []stackops.DeclaredObject{{Key: "cert", Name: "shared_cert", External: true}},
		Configs:  []stackops.DeclaredObject{{Key: "cfg", Name: "shared_cfg", External: true}},
	}

	res := rig.deps.Resources(testsupport.Context(t), rig.envID, swarmStack(), decl)

	for name, owners := range map[string][]string{
		"networks": ownersOf(res.Networks.Items, func(n stackops.NetworkResource) string { return n.Name + ":" + n.Ownership }),
		"secrets":  ownersOf(res.Secrets.Items, func(o stackops.ObjectResource) string { return o.Name + ":" + o.Ownership }),
		"configs":  ownersOf(res.Configs.Items, func(o stackops.ObjectResource) string { return o.Name + ":" + o.Ownership }),
	} {
		if len(owners) != 1 || owners[0][len(owners[0])-len(stackops.OwnerExternal):] != stackops.OwnerExternal {
			t.Errorf("%s = %v, want the one external object, read External", name, owners)
		}
	}
}

func ownersOf[T any](items []T, f func(T) string) []string {
	var out []string
	for _, it := range items {
		out = append(out, f(it))
	}
	return out
}

func TestResourcesOfAStandaloneStackListItsSecretsAndConfigsFromTheDeployedDefinition(t *testing.T) {
	t.Parallel()
	h := hostStack(t, "demo", hostedCompose, nil)
	rig := hostRig(t, h)
	decl := declaredFrom(t, stacks.TargetStandalone, `services:
  web:
    image: nginx:1.27
    secrets: [tok]
    configs: [site]
  db:
    image: postgres:16
    secrets: [tok]
secrets:
  tok:
    environment: TOK
configs:
  site:
    content: hello
`)

	res := rig.deps.Resources(testsupport.Context(t), rig.envID, standaloneStack("demo"), decl)

	if res.Secrets.Error != "" || len(res.Secrets.Items) != 1 {
		t.Fatalf("secrets = %+v", res.Secrets)
	}
	tok := res.Secrets.Items[0]
	if tok.Name != "tok" || tok.ID != "" || tok.Hash != "" || tok.Ownership != stackops.OwnerStack || len(tok.UsedBy) != 2 || tok.UsedBy[0].Name != "db" || tok.UsedBy[1].Name != "web" {
		t.Errorf("tok = %+v, want a file of the containers, the stack's own, used by db and web", tok)
	}
	if res.Configs.Error != "" || len(res.Configs.Items) != 1 || res.Configs.Items[0].Name != "site" || res.Configs.Items[0].Ownership != stackops.OwnerStack {
		t.Errorf("configs = %+v", res.Configs)
	}
}

func TestResourcesOfAStandaloneStackNeverDeployedHaveNoSecretsOrConfigs(t *testing.T) {
	t.Parallel()
	h := hostStack(t, "demo", hostedCompose, nil)
	rig := hostRig(t, h)

	res := rig.deps.Resources(testsupport.Context(t), rig.envID, standaloneStack("demo"), stackops.Declared{})

	if res.Secrets.Error != "" || res.Configs.Error != "" || res.Secrets.Items == nil || len(res.Secrets.Items) != 0 || len(res.Configs.Items) != 0 {
		t.Errorf("secrets = %+v configs = %+v, want empty cards", res.Secrets, res.Configs)
	}
}

func TestResourcesKeepEachCardsErrorToItself(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.TasksFollowWrites = false
		c.SecretListError = "secrets are not available"
		c.Networks = []dockertest.FakeNetwork{{ID: "n-default", Name: "web_default", Driver: "overlay", Scope: "swarm", Labels: ownedBy("web", "stack-web")}}
		c.Configs = []dockertest.FakeConfigObject{{ID: "cfg-site", Name: "web_site", Labels: ownedBy("web", "stack-web")}}
	},
		dockertest.FakeService{ID: "svc-app", Name: "web_app", Mode: "replicated", Running: 1, Desired: 1, Image: "x", Labels: ownedBy("web", "stack-web")},
	)

	res := rig.deps.Resources(testsupport.Context(t), rig.envID, swarmStack(), stackops.Declared{Known: true})

	if res.Secrets.Error != stackops.ReadFailed || len(res.Secrets.Items) != 0 || res.Secrets.Items == nil {
		t.Errorf("secrets = %+v, want its own error and no rows", res.Secrets)
	}
	if res.Configs.Error != "" || len(res.Configs.Items) != 1 {
		t.Errorf("configs = %+v, want them read", res.Configs)
	}
	if res.Networks.Error != "" || len(res.Networks.Items) != 1 || res.Tasks.Error != "" {
		t.Errorf("networks = %+v tasks = %+v, want them read", res.Networks, res.Tasks)
	}
}

func TestResourcesOfAStandaloneStackListTheVolumesOfItsProjectOnTheHost(t *testing.T) {
	t.Parallel()
	h := hostStack(t, "demo", hostedCompose, nil)
	h.Volumes = append(h.Volumes, dockertest.FakeVolume{Name: "demo_hand", Driver: "local", Labels: map[string]string{"com.docker.compose.project": "demo"}})
	rig := hostRig(t, h)

	res := rig.deps.Resources(testsupport.Context(t), rig.envID, standaloneStack("demo"), stackops.Declared{})

	if res.Volumes.Error != "" || len(res.Volumes.Items) != 1 {
		t.Fatalf("volumes = %+v, want the one the deploy made", res.Volumes)
	}
	v := res.Volumes.Items[0]
	if v.Name != "demo_data" || v.Driver != "local" || v.Ownership != stackops.OwnerStack || v.NodeID == "" {
		t.Errorf("volume = %+v", v)
	}
}

// The Resources of a stack Coaming did not make (plan 08-09, STK-12, D-13, D-18): the same cards over what its
// labels and its services find, with the ownership badges previewing what adopting would own.

func legacyNS(ns string) map[string]string {
	return map[string]string{"com.docker.stack.namespace": ns}
}

func TestResourcesOfADiscoveredSwarmStackPreviewWhatAdoptingWouldOwn(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.TasksFollowWrites = false
		c.Tasks = []swarm.Task{
			resourcesTask("t-app-1", "svc-app", "node-1", 1, "running", "running", time.Minute),
			resourcesTask("t-blog-1", "svc-blog", "node-1", 1, "running", "running", time.Minute),
		}
		c.Networks = []dockertest.FakeNetwork{
			{ID: "n-default", Name: "legacy_default", Driver: "overlay", Scope: "swarm", Labels: legacyNS("legacy")},
			// Labelled with the namespace and attached by no service: still the stack's.
			{ID: "n-spare", Name: "legacy_spare", Driver: "overlay", Scope: "swarm", Labels: legacyNS("legacy")},
			{ID: "n-shared", Name: "shared", Driver: "overlay", Scope: "swarm"},
			{ID: "n-blog", Name: "blog_default", Driver: "overlay", Scope: "swarm", Labels: legacyNS("blog")},
			{ID: "n-free", Name: "unrelated", Driver: "overlay", Scope: "swarm"},
		}
		c.Volumes = []dockertest.FakeVolume{
			{Name: "legacy_data", Driver: "local", Labels: legacyNS("legacy")},
			{Name: "shared-vol", Driver: "local"},
			{Name: "blog_data", Driver: "local", Labels: legacyNS("blog")},
			{Name: "loose", Driver: "local"},
		}
		c.Secrets = []dockertest.FakeSecret{
			{ID: "sec-pw", Name: "legacy_pw", Labels: legacyNS("legacy")},
			{ID: "sec-ext", Name: "shared_cert"},
			{ID: "sec-free", Name: "unrelated"},
		}
		c.Configs = []dockertest.FakeConfigObject{{ID: "cfg-app", Name: "legacy_app_conf", Labels: legacyNS("legacy")}}
	},
		dockertest.FakeService{
			ID: "svc-app", Name: "legacy_app", Mode: "replicated", Running: 1, Desired: 1, Image: "x", Labels: legacyNS("legacy"),
			Networks: []string{"n-default", "shared"},
			Mounts: []dockertest.FakeServiceMount{
				{Type: "volume", Source: "legacy_data", Target: "/data"},
				{Type: "volume", Source: "shared-vol", Target: "/shared"},
				{Type: "bind", Source: "/srv/conf", Target: "/conf"},
			},
			Secrets: []dockertest.FakeServiceObject{{ID: "sec-pw", Name: "legacy_pw", File: "pw"}, {ID: "sec-ext", Name: "shared_cert", File: "cert"}},
			Configs: []dockertest.FakeServiceObject{{ID: "cfg-app", Name: "legacy_app_conf", File: "/etc/app.conf"}},
		},
		// Another stack of the swarm that uses the same external secret is not this stack's user of it.
		dockertest.FakeService{
			ID: "svc-blog", Name: "blog_app", Mode: "replicated", Running: 1, Desired: 1, Image: "y", Labels: legacyNS("blog"),
			Secrets: []dockertest.FakeServiceObject{{ID: "sec-ext", Name: "shared_cert", File: "cert"}},
		},
	)
	st := stackops.DiscoveredStack(rig.envID, "legacy", stacks.TargetSwarm)

	res := rig.deps.Resources(testsupport.Context(t), rig.envID, st, stackops.Declared{})

	if res.Tasks.Error != "" || len(res.Tasks.Items) != 1 || res.Tasks.Items[0].Task.ID != "t-app-1" || res.Tasks.Items[0].Service != "app" {
		t.Errorf("tasks = %+v, want the one task of legacy's service, not blog's", res.Tasks)
	}
	type row struct{ name, owner string }
	rows := func(names []row) string {
		out := ""
		for _, r := range names {
			out += r.name + ":" + r.owner + " "
		}
		return out
	}
	var nets []row
	for _, n := range res.Networks.Items {
		nets = append(nets, row{n.Name, n.Ownership})
	}
	if want := []row{{"legacy_default", "stack"}, {"legacy_spare", "stack"}, {"shared", "external"}}; rows(nets) != rows(want) {
		t.Errorf("networks = %s, want %s: the labelled ones Stack, the one the service attaches External", rows(nets), rows(want))
	}
	var vols []row
	for _, v := range res.Volumes.Items {
		vols = append(vols, row{v.Name, v.Ownership})
	}
	if want := []row{{"legacy_data", "stack"}, {"shared-vol", "external"}}; rows(vols) != rows(want) {
		t.Errorf("volumes = %s, want %s: a bind mount and another stack's volume are not listed", rows(vols), rows(want))
	}
	var secrets []row
	for _, o := range res.Secrets.Items {
		secrets = append(secrets, row{o.Name, o.Ownership})
	}
	if want := []row{{"legacy_pw", "external"}, {"shared_cert", "external"}}; rows(secrets) != rows(want) {
		t.Errorf("secrets = %s, want %s: always External, even one that carries the namespace label", rows(secrets), rows(want))
	}
	for _, o := range res.Secrets.Items {
		if o.Name == "shared_cert" && (len(o.UsedBy) != 1 || o.UsedBy[0].Name != "app") {
			t.Errorf("shared_cert users = %+v, want the stack's app alone", o.UsedBy)
		}
	}
	if len(res.Configs.Items) != 1 || res.Configs.Items[0].Name != "legacy_app_conf" || res.Configs.Items[0].Ownership != stackops.OwnerExternal {
		t.Errorf("configs = %+v, want legacy_app_conf, External", res.Configs.Items)
	}
}

func TestResourcesOfADiscoveredStandaloneStackPreviewWhatAdoptingWouldOwn(t *testing.T) {
	t.Parallel()
	project := func(p, service string) map[string]string {
		return map[string]string{"com.docker.compose.project": p, "com.docker.compose.service": service}
	}
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		standalone(c)
		c.TasksFollowWrites = false
		c.Containers = []dockertest.FakeContainer{
			{
				ID: "c-web", Names: []string{"/shop-web-1"}, Image: "nginx:1.27", State: "running", Labels: project("shop", "web"),
				Mounts: []string{"shop_data", "ext-vol", "/srv/conf"},
				Networks: map[string]dockertest.FakeEndpoint{
					"shop_default": {NetworkID: "n-default"}, "shared-net": {NetworkID: "n-shared"}, "bridge": {NetworkID: "n-bridge"},
				},
			},
			{ID: "c-db", Names: []string{"/shop-db-1"}, Image: "postgres:17", State: "exited", Labels: project("shop", "db")},
			{ID: "c-run", Names: []string{"/shop-run-1"}, Image: "nginx:1.27", State: "running", Labels: map[string]string{
				"com.docker.compose.project": "shop", "com.docker.compose.service": "web", "com.docker.compose.oneoff": "True",
			}},
			{ID: "c-else", Names: []string{"/elsewhere-web-1"}, Image: "nginx:1.27", State: "running", Labels: project("elsewhere", "web")},
		}
		c.Networks = []dockertest.FakeNetwork{
			{ID: "n-default", Name: "shop_default", Driver: "bridge", Scope: "local", Labels: map[string]string{"com.docker.compose.project": "shop"}},
			{ID: "n-shared", Name: "shared-net", Driver: "bridge", Scope: "local"},
			{ID: "n-bridge", Name: "bridge", Driver: "bridge", Scope: "local"},
			{ID: "n-else", Name: "elsewhere_default", Driver: "bridge", Scope: "local", Labels: map[string]string{"com.docker.compose.project": "elsewhere"}},
		}
		c.Volumes = []dockertest.FakeVolume{
			{Name: "shop_data", Driver: "local", Labels: map[string]string{"com.docker.compose.project": "shop"}},
			{Name: "ext-vol", Driver: "local"},
			{Name: "elsewhere_data", Driver: "local", Labels: map[string]string{"com.docker.compose.project": "elsewhere"}},
			{Name: "loose", Driver: "local"},
		}
	})
	st := stackops.DiscoveredStack(rig.envID, "shop", stacks.TargetStandalone)

	res := rig.deps.Resources(testsupport.Context(t), rig.envID, st, stackops.Declared{})

	var names []string
	for _, c := range res.Containers.Items {
		names = append(names, c.Name)
	}
	if res.Containers.Error != "" || !slices.Equal(names, []string{"shop-db-1", "shop-web-1"}) {
		t.Errorf("containers = %v (error %q), want the project's two, the one-off and another project's left out", names, res.Containers.Error)
	}
	got := func() []string {
		var out []string
		for _, n := range res.Networks.Items {
			out = append(out, n.Name+":"+n.Ownership)
		}
		for _, v := range res.Volumes.Items {
			out = append(out, v.Name+":"+v.Ownership)
		}
		return out
	}()
	want := []string{"shared-net:external", "shop_default:stack", "ext-vol:external", "shop_data:stack"}
	if !slices.Equal(got, want) {
		t.Errorf("networks and volumes = %v, want %v: the project's labelled ones Stack, what its containers use without the label External, the default bridge and a bind mount left out", got, want)
	}
	if res.Secrets.Error != "" || len(res.Secrets.Items) != 0 || res.Configs.Error != "" || len(res.Configs.Items) != 0 {
		t.Errorf("secrets = %+v configs = %+v, want empty cards: a compose secret is a file, and nothing declares one here", res.Secrets, res.Configs)
	}
}

func TestManagedResourcesUnchanged(t *testing.T) {
	t.Parallel()
	// A stack of Coaming's is its objects by ID: what carries its namespace without its ID is somebody else's, and
	// what a service of its own mentions without declaring it is not listed.
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.TasksFollowWrites = false
		c.Tasks = []swarm.Task{
			resourcesTask("t-mine", "svc-mine", "node-1", 1, "running", "running", time.Minute),
			resourcesTask("t-hand", "svc-hand", "node-1", 1, "running", "running", time.Minute),
		}
		c.Volumes = []dockertest.FakeVolume{
			{Name: "web_data", Driver: "local", Labels: ownedBy("web", "stack-web")},
			{Name: "web_hand", Driver: "local", Labels: legacyNS("web")},
		}
		c.Secrets = []dockertest.FakeSecret{
			{ID: "sec-mine", Name: "web_tok_1a2b3c4d", Labels: ownedBy("web", "stack-web")},
			{ID: "sec-hand", Name: "web_hand", Labels: legacyNS("web")},
		}
	},
		dockertest.FakeService{ID: "svc-mine", Name: "web_mine", Mode: "replicated", Running: 1, Desired: 1, Image: "x", Labels: ownedBy("web", "stack-web"),
			Mounts: []dockertest.FakeServiceMount{{Type: "volume", Source: "mentioned-only", Target: "/m"}}},
		dockertest.FakeService{ID: "svc-hand", Name: "web_hand", Mode: "replicated", Running: 1, Desired: 1, Image: "y", Labels: legacyNS("web")},
	)

	res := rig.deps.Resources(testsupport.Context(t), rig.envID, swarmStack(), stackops.Declared{})

	if len(res.Tasks.Items) != 1 || res.Tasks.Items[0].Task.ID != "t-mine" {
		t.Errorf("tasks = %+v, want the stack's own service's task alone", res.Tasks.Items)
	}
	if len(res.Volumes.Items) != 1 || res.Volumes.Items[0].Name != "web_data" || res.Volumes.Items[0].Ownership != stackops.OwnerStack {
		t.Errorf("volumes = %+v, want web_data, Stack: a volume a service mentions and the file does not declare is not listed", res.Volumes.Items)
	}
	if len(res.Secrets.Items) != 1 || res.Secrets.Items[0].FullName != "web_tok_1a2b3c4d" || res.Secrets.Items[0].Ownership != stackops.OwnerStack {
		t.Errorf("secrets = %+v, want the stack's own secret alone", res.Secrets.Items)
	}
}

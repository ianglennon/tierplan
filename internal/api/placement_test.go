// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
)

// Where tasks run (plan 06-11, RES-12, D-18, D-19, RESEARCH E13). The fixture is the one the
// tasks tests use: four nodes, the local socket being node-1.

type plNode struct {
	ID           string            `json:"id"`
	Hostname     string            `json:"hostname"`
	Role         string            `json:"role"`
	Leader       bool              `json:"leader"`
	Availability string            `json:"availability"`
	State        string            `json:"state"`
	Cpus         float64           `json:"cpus"`
	MemoryBytes  int64             `json:"memoryBytes"`
	Address      string            `json:"address"`
	OS           string            `json:"os"`
	Arch         string            `json:"arch"`
	Labels       map[string]string `json:"labels"`
	EngineLabels map[string]string `json:"engineLabels"`
	TargetNodeID string            `json:"targetNodeId"`
}

type plTask struct {
	ID           string `json:"id"`
	ServiceID    string `json:"serviceId"`
	Service      string `json:"service"`
	Stack        string `json:"stack"`
	Slot         int    `json:"slot"`
	NodeID       string `json:"nodeId"`
	NodeKnown    bool   `json:"nodeKnown"`
	State        string `json:"state"`
	DockerState  string `json:"dockerState"`
	DesiredState string `json:"desiredState"`
	Error        string `json:"error"`
}

type plService struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Stack       string   `json:"stack"`
	Mode        string   `json:"mode"`
	Constraints []string `json:"constraints"`
	Preferences []string `json:"preferences"`
	MaxReplicas uint64   `json:"maxReplicas"`
}

type plBody struct {
	Nodes    []plNode    `json:"nodes"`
	Tasks    []plTask    `json:"tasks"`
	Services []plService `json:"services"`
}

func plGet(t *testing.T, f *envFix, e envBody, query string) plBody {
	t.Helper()
	rec := f.get(t, e, "placement"+query)
	if rec.code != http.StatusOK {
		t.Fatalf("GET placement%s: %d %s", query, rec.code, rec.body)
	}
	return decode[plBody](t, rec)
}

func plTaskOf(t *testing.T, b plBody, id string) plTask {
	t.Helper()
	for _, tk := range b.Tasks {
		if tk.ID == id {
			return tk
		}
	}
	t.Fatalf("no task %q in %+v", id, b.Tasks)
	return plTask{}
}

func plServices() []dockertest.FakeService {
	web := webService()
	web.Constraints = []string{"node.role==worker"}
	api := dockertest.FakeService{
		ID: "svc-api", Name: "api", Mode: "replicated", Running: 1, Desired: 1,
		Labels: map[string]string{"com.docker.stack.namespace": "shop"},
	}
	solo := dockertest.FakeService{ID: "svc-solo", Name: "solo", Mode: "replicated", Running: 1, Desired: 1}
	return []dockertest.FakeService{web, api, solo}
}

func TestPlacementListsTasksByNode(t *testing.T) {
	t.Parallel()
	f, e := tkEnv(t, 5, plServices(),
		tkRunning("web-1", "svc-web", "node-3", 1, "c1", time.Minute),
		tkRunning("web-2", "svc-web", "node-2", 2, "c2", time.Minute),
		tkRunning("api-1", "svc-api", "node-1", 1, "c3", time.Minute),
		tkRunning("solo-1", "svc-solo", "node-1", 1, "c4", time.Minute),
		tkExited("web-old", "svc-web", "node-3", 1, "failed", "shutdown", 1, "boom", 10*time.Minute),
	)
	b := plGet(t, f, e, "")

	var hosts []string
	for _, n := range b.Nodes {
		hosts = append(hosts, n.Hostname)
	}
	if want := []string{"mgr-1", "wrk-away", "wrk-down", "wrk-ready"}; !slices.Equal(hosts, want) {
		t.Fatalf("nodes are %v, want %v: every node, sorted by name", hosts, want)
	}
	mgr := b.Nodes[0]
	if mgr.ID != "node-1" || mgr.Role != "manager" || !mgr.Leader || mgr.Availability != "active" || mgr.State != "ready" || mgr.TargetNodeID != e.ID {
		t.Errorf("mgr-1 = %+v, want the leader manager, ready and active, reachable through the local socket (targetNodeId %q)", mgr, e.ID)
	}
	if mgr.Labels == nil || mgr.EngineLabels == nil {
		t.Errorf("mgr-1 labels = %#v / %#v, want objects and never null", mgr.Labels, mgr.EngineLabels)
	}
	if down := b.Nodes[2]; down.State != "down" || down.TargetNodeID != "" {
		t.Errorf("wrk-down = %+v, want state down and no route to it", down)
	}

	if len(b.Tasks) != 4 {
		t.Fatalf("tasks = %+v, want the four whose desired state is running (a failed task being replaced is not one)", b.Tasks)
	}
	web1 := plTaskOf(t, b, "web-1")
	if web1.ServiceID != "svc-web" || web1.Service != "web" || web1.Stack != "shop" || web1.Slot != 1 || web1.NodeID != "node-3" || !web1.NodeKnown ||
		web1.State != "running" || web1.DesiredState != "running" || web1.Error != "" {
		t.Errorf("web-1 = %+v", web1)
	}
	if solo := plTaskOf(t, b, "solo-1"); solo.Stack != "" {
		t.Errorf("solo-1 stack = %q, want none", solo.Stack)
	}
	// A task on a node that is down cannot be believed to run (RESEARCH E13).
	if on := plTaskOf(t, b, "web-2"); on.State != "unknown" || on.DockerState != "running" {
		t.Errorf("web-2 = %+v, want state unknown with Docker's own word kept", on)
	}

	svc := map[string]plService{}
	for _, s := range b.Services {
		svc[s.Name] = s
	}
	if len(svc) != 3 || svc["web"].Stack != "shop" || svc["web"].Mode != "replicated" || !slices.Equal(svc["web"].Constraints, []string{"node.role==worker"}) {
		t.Errorf("services = %+v, want web with its stack, mode and constraints", b.Services)
	}
	if svc["solo"].Constraints == nil {
		t.Errorf("solo constraints = null, want an empty list")
	}
	for i := 1; i < len(b.Tasks); i++ {
		p, c := b.Tasks[i-1], b.Tasks[i]
		if p.Service > c.Service || p.Service == c.Service && p.Slot > c.Slot {
			t.Errorf("tasks are not sorted by service then slot: %+v", b.Tasks)
		}
	}
}

func TestPlacementNotPlacedAndRemovedNode(t *testing.T) {
	t.Parallel()
	pending := tkTask("web-pending", "svc-web", "", 3, "pending", "running", time.Minute)
	pending.Status.Err = "no suitable node (scheduling constraints not satisfied on 4 nodes)\x1b"
	f, e := tkEnv(t, 5, plServices(),
		pending,
		tkRunning("web-gone", "svc-web", "node-gone", 1, "c1", time.Minute),
	)
	b := plGet(t, f, e, "")

	un := plTaskOf(t, b, "web-pending")
	if un.NodeID != "" || un.State != "pending" || !un.NodeKnown {
		t.Errorf("task with no node = %+v, want an empty nodeId, its own state and nothing missing", un)
	}
	if un.Error != "no suitable node (scheduling constraints not satisfied on 4 nodes)" {
		t.Errorf("error = %q, want the swarm's text cleaned of control characters", un.Error)
	}
	gone := plTaskOf(t, b, "web-gone")
	if gone.NodeKnown || gone.NodeID != "node-gone" || gone.State != "unknown" {
		t.Errorf("task of a removed node = %+v, want nodeKnown false, the ID kept and state unknown", gone)
	}
	for _, n := range b.Nodes {
		if n.ID == "node-gone" {
			t.Errorf("the removed node is listed as a node: %+v", n)
		}
	}
}

func TestPlacementForOneService(t *testing.T) {
	t.Parallel()
	f, e := tkEnv(t, 5, plServices(),
		tkRunning("web-1", "svc-web", "node-3", 1, "c1", time.Minute),
		tkRunning("api-1", "svc-api", "node-1", 1, "c2", time.Minute),
	)
	b := plGet(t, f, e, "?serviceId=svc-web")
	if len(b.Tasks) != 1 || b.Tasks[0].ID != "web-1" {
		t.Errorf("tasks = %+v, want only the service's own", b.Tasks)
	}
	if len(b.Nodes) != 4 {
		t.Errorf("nodes = %d, want all four, so the empty columns still show", len(b.Nodes))
	}
	if len(b.Services) != 1 || b.Services[0].ID != "svc-web" || b.Services[0].Constraints[0] != "node.role==worker" {
		t.Errorf("services = %+v, want the one service, with its constraints", b.Services)
	}
	if byName := plGet(t, f, e, "?serviceId=web"); len(byName.Tasks) != 1 {
		t.Errorf("by name: tasks = %+v, want the service's task", byName.Tasks)
	}

	rec := f.get(t, e, "placement?serviceId=nope")
	if rec.code != http.StatusNotFound || rec.ptyp != "service_not_found" {
		t.Errorf("an unknown service = %d %s (%s), want 404 service_not_found", rec.code, rec.body, rec.ptyp)
	}
}

func TestPlacementCarriesConstraintFacts(t *testing.T) {
	t.Parallel()
	web := webService()
	web.Constraints = []string{"node.role==worker", "node.labels.zone!=a"}
	web.Preferences = []string{"node.labels.zone", "engine.labels.rack"}
	web.MaxReplicas = 2
	api := dockertest.FakeService{ID: "svc-api", Name: "api", Mode: "replicated", Running: 1, Desired: 1}
	f, e := tkEnv(t, 5, []dockertest.FakeService{web, api},
		tkRunning("web-1", "svc-web", "node-3", 1, "c1", time.Minute),
	)
	b := plGet(t, f, e, "")
	for _, n := range b.Nodes {
		if n.OS == "" || n.Arch == "" || n.Labels == nil || n.EngineLabels == nil {
			t.Errorf("node %s = %+v, want os, arch and both label maps (never null)", n.ID, n)
		}
	}
	byName := map[string]plService{}
	for _, s := range b.Services {
		byName[s.Name] = s
	}
	got := byName[web.Name]
	if !slices.Equal(got.Preferences, web.Preferences) || got.MaxReplicas != 2 || !slices.Equal(got.Constraints, web.Constraints) {
		t.Errorf("web = %+v, want its constraints, spread labels and a max of 2 per node", got)
	}
	if bare := byName["api"]; bare.Preferences == nil || len(bare.Preferences) != 0 || bare.MaxReplicas != 0 {
		t.Errorf("api = %+v, want no preferences (an empty list, not null) and no limit", bare)
	}
	if rec := f.get(t, e, "placement"); strings.Contains(string(rec.body), `"preferences":null`) {
		t.Errorf("body = %s, want no null preferences", rec.body)
	}
}

func TestPlacementWithNothingRunningListsNodesAndNoNull(t *testing.T) {
	t.Parallel()
	f, e := tkEnv(t, 5, nil)
	rec := f.get(t, e, "placement")
	if rec.code != http.StatusOK || !strings.Contains(string(rec.body), `"tasks":[]`) || !strings.Contains(string(rec.body), `"services":[]`) {
		t.Fatalf("answer = %d %s, want tasks and services as empty lists, never null", rec.code, rec.body)
	}
}

func TestPlacementIsForASwarmAndForAViewer(t *testing.T) {
	t.Parallel()
	f, e := tkEnv(t, 5, plServices(), tkRunning("t", "svc-web", "node-1", 1, "c", time.Minute))
	get := f.asRole(t, e, "pl-viewer", "viewer")
	rec := get("placement")
	if rec.Code != http.StatusOK {
		t.Fatalf("a Viewer's read = %d %s, want 200", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	solo := newEnvFix(t, dockertest.FakeConfig{Hostname: "solo", Version: "29.1.0"})
	solo.clusters = envs.NewClusters(solo.svc, solo.snaps, envs.ClusterOptions{Now: solo.clk.now})
	solo.svc.WithClusters(solo.clusters)
	solo.h = solo.build(t)
	se := solo.add(t)
	awaitOnline(t, solo.mon, se.ID)
	if rec := solo.get(t, se, "placement"); rec.code != http.StatusConflict || rec.ptyp != "not_a_swarm" {
		t.Errorf("a standalone host = %d %s (%s), want 409 not_a_swarm", rec.code, rec.body, rec.ptyp)
	}
}

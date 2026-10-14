// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/bayplan/proto/wire"
)

// The nodes of a swarm (plan 06-13, RES-05 read side, D-20 to D-23). The fixture is the
// tasks fixture's four nodes, given the facts the Nodes pages show; the local socket is node-1.

type ndItem struct {
	ID              string  `json:"id"`
	Hostname        string  `json:"hostname"`
	Role            string  `json:"role"`
	Leader          bool    `json:"leader"`
	RunsBayplan     bool    `json:"runsBayplan"`
	State           string  `json:"state"`
	Availability    string  `json:"availability"`
	Reachability    string  `json:"reachability"`
	Address         string  `json:"address"`
	EngineVersion   string  `json:"engineVersion"`
	OS              string  `json:"os"`
	Arch            string  `json:"arch"`
	Cpus            float64 `json:"cpus"`
	MemoryBytes     int64   `json:"memoryBytes"`
	TasksRunning    int     `json:"tasksRunning"`
	TasksNotRunning int     `json:"tasksNotRunning"`
	AgentReporting  bool    `json:"agentReporting"`
	TargetNodeID    string  `json:"targetNodeId"`
}

type ndCounts struct {
	Managers int `json:"managers"`
	Workers  int `json:"workers"`
	Drained  int `json:"drained"`
	Down     int `json:"down"`
}

type ndList struct {
	Items           []ndItem `json:"items"`
	Total           int      `json:"total"`
	UnfilteredTotal int      `json:"unfilteredTotal"`
	Counts          ndCounts `json:"counts"`
	TasksKnown      bool     `json:"tasksKnown"`
	NoManager       bool     `json:"noManager"`
	FetchedAt       string   `json:"fetchedAt"`
}

type ndAgent struct {
	Reporting bool   `json:"reporting"`
	Version   string `json:"version"`
	LastSeen  string `json:"lastSeen"`
}

type ndDetail struct {
	ID            string            `json:"id"`
	Version       uint64            `json:"version"`
	Hostname      string            `json:"hostname"`
	Role          string            `json:"role"`
	Leader        bool              `json:"leader"`
	RunsBayplan   bool              `json:"runsBayplan"`
	State         string            `json:"state"`
	Availability  string            `json:"availability"`
	Reachability  string            `json:"reachability"`
	Address       string            `json:"address"`
	ManagerAddr   string            `json:"managerAddr"`
	EngineVersion string            `json:"engineVersion"`
	OS            string            `json:"os"`
	Arch          string            `json:"arch"`
	Cpus          float64           `json:"cpus"`
	MemoryBytes   int64             `json:"memoryBytes"`
	Labels        map[string]string `json:"labels"`
	EngineLabels  map[string]string `json:"engineLabels"`
	CreatedAt     string            `json:"createdAt"`
	UpdatedAt     string            `json:"updatedAt"`
	Agent         ndAgent           `json:"agent"`
	TargetNodeID  string            `json:"targetNodeId"`
}

type ndTask struct {
	ID           string `json:"id"`
	ServiceID    string `json:"serviceId"`
	Service      string `json:"service"`
	Slot         int    `json:"slot"`
	NodeID       string `json:"nodeId"`
	Node         string `json:"node"`
	NodeKnown    bool   `json:"nodeKnown"`
	State        string `json:"state"`
	DockerState  string `json:"dockerState"`
	DesiredState string `json:"desiredState"`
	Error        string `json:"error"`
	UpdatedAt    string `json:"updatedAt"`
}

type ndTasks struct {
	Items  []ndTask `json:"items"`
	Total  int      `json:"total"`
	Capped bool     `json:"capped"`
}

var (
	ndCreated = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ndUpdated = time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)
)

func ndNodes() []dockertest.FakeNode {
	return []dockertest.FakeNode{
		{
			ID: "node-1", Hostname: "mgr-1", Role: "manager", Leader: true, Address: "10.0.0.1", ManagerAddr: "10.0.0.1:2377",
			EngineVersion: "29.1.0", OS: "linux", Arch: "aarch64", NanoCPUs: 4e9, MemoryBytes: 8 << 30,
			Labels: map[string]string{"zone": "a"}, EngineLabels: map[string]string{"disk": "SSD"},
			Version: 7, CreatedAt: ndCreated, UpdatedAt: ndUpdated,
		},
		{ID: "node-2", Hostname: "wrk-down", State: "down", Availability: "drain", Address: "10.0.0.2", EngineVersion: "29.1.0", OS: "linux"},
		{ID: "node-3", Hostname: "wrk-ready", Address: "10.0.0.3", EngineVersion: "29.0.0", OS: "linux", NanoCPUs: 2e9, MemoryBytes: 4 << 30},
		{ID: "node-4", Hostname: "wrk-away", State: "disconnected", Availability: "pause", Address: "10.0.0.4", EngineVersion: "29.1.0", OS: "linux"},
	}
}

// ndEnv is a local environment whose socket is node-1 of a swarm of four nodes.
func ndEnv(t *testing.T, services []dockertest.FakeService, tasks ...swarm.Task) (*envFix, envBody) {
	t.Helper()
	cfg := swarmManagerEngine(services...)
	cfg.SwarmNodeID, cfg.SwarmClusterID = "node-1", "cluster-1"
	cfg.NodeRows = ndNodes()
	cfg.Tasks = tasks
	f := newEnvFix(t, cfg)
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.h = f.build(t)
	e := f.add(t)
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	return f, e
}

func ndFixtureTasks() []swarm.Task {
	return []swarm.Task{
		tkRunning("web-1", "svc-web", "node-3", 1, "c1", time.Minute),
		tkRunning("web-2", "svc-web", "node-2", 2, "c2", time.Minute),
		tkRunning("api-1", "svc-api", "node-1", 1, "c3", time.Minute),
		tkRunning("solo-1", "svc-solo", "node-1", 1, "c4", time.Minute),
		tkExited("web-old", "svc-web", "node-3", 1, "failed", "shutdown", 1, "boom", 10*time.Minute),
	}
}

func ndList1(t *testing.T, f *envFix, e envBody, query string) ndList {
	t.Helper()
	rec := f.get(t, e, "nodes"+query)
	if rec.code != http.StatusOK {
		t.Fatalf("GET nodes%s: %d %s", query, rec.code, rec.body)
	}
	return decode[ndList](t, rec)
}

func ndHosts(items []ndItem) []string {
	var out []string
	for _, n := range items {
		out = append(out, n.Hostname)
	}
	return out
}

func TestListNodes(t *testing.T) {
	f, e := ndEnv(t, plServices(), ndFixtureTasks()...)
	b := ndList1(t, f, e, "")

	if want := []string{"mgr-1", "wrk-away", "wrk-down", "wrk-ready"}; !slices.Equal(ndHosts(b.Items), want) {
		t.Fatalf("nodes are %v, want %v: every node, sorted by host name", ndHosts(b.Items), want)
	}
	if b.Total != 4 || b.UnfilteredTotal != 4 || !b.TasksKnown || b.NoManager || b.FetchedAt == "" {
		t.Errorf("body = total %d unfiltered %d tasksKnown %v noManager %v fetchedAt %q", b.Total, b.UnfilteredTotal, b.TasksKnown, b.NoManager, b.FetchedAt)
	}
	if want := (ndCounts{Managers: 1, Workers: 3, Drained: 1, Down: 2}); b.Counts != want {
		t.Errorf("counts = %+v, want %+v (down counts down and disconnected)", b.Counts, want)
	}
	mgr := b.Items[0]
	want := ndItem{
		ID: "node-1", Hostname: "mgr-1", Role: "manager", Leader: true, RunsBayplan: true, State: "ready", Availability: "active",
		Reachability: "reachable", Address: "10.0.0.1", EngineVersion: "29.1.0", OS: "linux", Arch: "aarch64", Cpus: 4, MemoryBytes: 8 << 30,
		TasksRunning: 2, AgentReporting: true, TargetNodeID: e.ID,
	}
	if mgr != want {
		t.Errorf("mgr-1 = %+v, want %+v", mgr, want)
	}
	byHost := map[string]ndItem{}
	for _, n := range b.Items {
		byHost[n.Hostname] = n
	}
	down := byHost["wrk-down"]
	// A task on a node that is down cannot be believed to run (RESEARCH E13).
	if down.State != "down" || down.Availability != "drain" || down.Role != "worker" || down.Reachability != "" || down.Leader ||
		down.TasksRunning != 0 || down.TasksNotRunning != 1 || down.AgentReporting || down.TargetNodeID != "" || down.RunsBayplan {
		t.Errorf("wrk-down = %+v, want a drained down worker whose one task does not count as running, with no agent", down)
	}
	if ready := byHost["wrk-ready"]; ready.TasksRunning != 1 || ready.TasksNotRunning != 0 || ready.Cpus != 2 || ready.MemoryBytes != 4<<30 || ready.Arch != "x86_64" || ready.AgentReporting {
		t.Errorf("wrk-ready = %+v, want one running task (the failed one being replaced is not counted), 2 CPUs and 4 GiB", ready)
	}
	if away := byHost["wrk-away"]; away.State != "disconnected" || away.Availability != "pause" || away.TasksRunning+away.TasksNotRunning != 0 {
		t.Errorf("wrk-away = %+v", away)
	}
}

func TestListNodesFilters(t *testing.T) {
	f, e := ndEnv(t, plServices(), ndFixtureTasks()...)
	all := ndCounts{Managers: 1, Workers: 3, Drained: 1, Down: 2}
	for _, tc := range []struct {
		name, query string
		want        []string
	}{
		{"a host name", "?q=MGR", []string{"mgr-1"}},
		{"an address", "?q=10.0.0.3", []string{"wrk-ready"}},
		{"a label key", "?q=zone", []string{"mgr-1"}},
		{"an engine label value, whatever its case", "?q=ssd", []string{"mgr-1"}},
		{"nothing", "?q=nope", nil},
		{"managers", "?status=managers", []string{"mgr-1"}},
		{"workers", "?status=workers", []string{"wrk-away", "wrk-down", "wrk-ready"}},
		{"drained", "?status=drained", []string{"wrk-down"}},
		{"down", "?status=down", []string{"wrk-away", "wrk-down"}},
		{"two chips are an or", "?status=managers,drained", []string{"mgr-1", "wrk-down"}},
		{"a search and a chip are an and", "?status=workers&q=10.0.0.2", []string{"wrk-down"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := ndList1(t, f, e, tc.query)
			if got := ndHosts(b.Items); !slices.Equal(got, tc.want) {
				t.Errorf("nodes = %v, want %v", got, tc.want)
			}
			if b.Total != len(tc.want) || b.UnfilteredTotal != 4 || b.Counts != all {
				t.Errorf("total %d unfiltered %d counts %+v, want %d, 4 and the chip numbers before the search and the filter", b.Total, b.UnfilteredTotal, b.Counts, len(tc.want))
			}
		})
	}
	if rec := f.get(t, e, "nodes?status=managers,bogus"); rec.code != http.StatusUnprocessableEntity || rec.ptyp != "status_invalid" {
		t.Errorf("a bad status = %d %s (%s), want 422 status_invalid", rec.code, rec.body, rec.ptyp)
	}
	if rec := f.get(t, e, "nodes?q="+strings.Repeat("a", 129)); rec.code != http.StatusUnprocessableEntity || rec.ptyp != "query_too_long" {
		t.Errorf("a long search = %d %s (%s), want 422 query_too_long", rec.code, rec.body, rec.ptyp)
	}
}

func TestGetNodeAndInspect(t *testing.T) {
	f, e := ndEnv(t, plServices(), ndFixtureTasks()...)
	rec := f.get(t, e, "nodes/node-1")
	if rec.code != http.StatusOK {
		t.Fatalf("GET node: %d %s", rec.code, rec.body)
	}
	d := decode[ndDetail](t, rec)
	if d.ID != "node-1" || d.Version != 7 || d.Hostname != "mgr-1" || d.Role != "manager" || !d.Leader || !d.RunsBayplan || d.State != "ready" ||
		d.Availability != "active" || d.Reachability != "reachable" || d.Address != "10.0.0.1" || d.ManagerAddr != "10.0.0.1:2377" ||
		d.EngineVersion != "29.1.0" || d.OS != "linux" || d.Arch != "aarch64" || d.Cpus != 4 || d.MemoryBytes != 8<<30 || d.TargetNodeID != e.ID {
		t.Errorf("detail = %+v", d)
	}
	if d.Labels["zone"] != "a" || d.EngineLabels["disk"] != "SSD" {
		t.Errorf("labels = %v, engine labels = %v", d.Labels, d.EngineLabels)
	}
	if d.CreatedAt != "2026-01-02T03:04:05Z" || d.UpdatedAt != "2026-06-07T08:09:10Z" {
		t.Errorf("times = %q, %q; want RFC 3339 UTC", d.CreatedAt, d.UpdatedAt)
	}
	// The local socket is reached directly: it is reporting, and it has no agent version.
	if d.Agent != (ndAgent{Reporting: true}) {
		t.Errorf("agent = %+v, want reporting through the local socket with no version", d.Agent)
	}

	w := decode[ndDetail](t, f.get(t, e, "nodes/node-3"))
	if w.Role != "worker" || w.Leader || w.RunsBayplan || w.Reachability != "" || w.ManagerAddr != "" || w.Agent.Reporting || w.TargetNodeID != "" {
		t.Errorf("worker = %+v, want no manager facts, no agent and no route", w)
	}
	if w.Labels == nil || w.EngineLabels == nil {
		t.Errorf("worker labels = %#v / %#v, want objects and never null", w.Labels, w.EngineLabels)
	}
	if rec := f.get(t, e, "nodes/node-3"); strings.Contains(string(rec.body), ":null") {
		t.Errorf("body = %s, want no null", rec.body)
	}

	rec = f.get(t, e, "nodes/node-1/inspect")
	if rec.code != http.StatusOK {
		t.Fatalf("GET inspect: %d %s", rec.code, rec.body)
	}
	var doc struct {
		Document map[string]any `json:"document"`
	}
	doc = decode[struct {
		Document map[string]any `json:"document"`
	}](t, rec)
	if doc.Document["ID"] != "node-1" || doc.Document["Spec"] == nil || doc.Document["Description"] == nil {
		t.Errorf("inspect document = %v, want the manager's own", doc.Document)
	}

	for _, path := range []string{"nodes/nope", "nodes/nope/inspect", "nodes/nope/tasks"} {
		if rec := f.get(t, e, path); rec.code != http.StatusNotFound || rec.ptyp != "node_not_found" {
			t.Errorf("GET %s = %d %s (%s), want 404 node_not_found", path, rec.code, rec.body, rec.ptyp)
		}
	}
}

func TestNodeTasks(t *testing.T) {
	tasks := []swarm.Task{
		tkRunning("web-1", "svc-web", "node-3", 1, "c1", time.Minute),
		tkRunning("api-1", "svc-api", "node-3", 1, "c2", 2*time.Minute),
		tkRunning("elsewhere", "svc-web", "node-1", 2, "c3", time.Minute),
	}
	// 60 earlier tasks of a slot, each older than the one before.
	for i := range 60 {
		tasks = append(tasks, tkExited(fmt.Sprintf("old-%02d", i), "svc-web", "node-3", 1, "shutdown", "shutdown", 0, "", time.Duration(10+i)*time.Minute))
	}
	f, e := ndEnv(t, plServices(), tasks...)
	rec := f.get(t, e, "nodes/node-3/tasks")
	if rec.code != http.StatusOK {
		t.Fatalf("GET tasks: %d %s", rec.code, rec.body)
	}
	b := decode[ndTasks](t, rec)
	if len(b.Items) != 2+50 || b.Total != 62 || !b.Capped {
		t.Fatalf("items = %d total %d capped %v, want the 2 tasks meant to run plus the 50 newest others, of 62", len(b.Items), b.Total, b.Capped)
	}
	// The tasks meant to run come first, newest first; then the earlier ones, newest first.
	if b.Items[0].ID != "web-1" || b.Items[1].ID != "api-1" || b.Items[2].ID != "old-00" || b.Items[51].ID != "old-49" {
		t.Errorf("order = %s, %s, %s ... %s", b.Items[0].ID, b.Items[1].ID, b.Items[2].ID, b.Items[51].ID)
	}
	for _, it := range b.Items {
		if it.ID == "elsewhere" || it.NodeID != "node-3" {
			t.Errorf("task %+v is not of node-3", it)
		}
	}
	web := b.Items[0]
	if web.ServiceID != "svc-web" || web.Service != "web" || web.Slot != 1 || web.Node != "wrk-ready" || !web.NodeKnown ||
		web.State != "running" || web.DesiredState != "running" || web.UpdatedAt == "" {
		t.Errorf("web-1 = %+v", web)
	}

	// A node with a few tasks is not capped.
	small := decode[ndTasks](t, f.get(t, e, "nodes/node-1/tasks"))
	if len(small.Items) != 1 || small.Total != 1 || small.Capped || small.Items[0].ID != "elsewhere" {
		t.Errorf("node-1 tasks = %+v, want its one task", small)
	}
	empty := f.get(t, e, "nodes/node-4/tasks")
	if empty.code != http.StatusOK || !strings.Contains(string(empty.body), `"items":[]`) {
		t.Errorf("a node with no tasks = %d %s, want an empty list and never null", empty.code, empty.body)
	}
}

func TestNodesAreForASwarmAndForAViewer(t *testing.T) {
	f, e := ndEnv(t, plServices(), ndFixtureTasks()...)
	get := f.asRole(t, e, "nd-viewer", "viewer")
	for _, path := range []string{"nodes", "nodes/node-1", "nodes/node-1/inspect", "nodes/node-1/tasks"} {
		rec := get(path)
		if rec.Code != http.StatusOK {
			t.Errorf("a Viewer's GET %s = %d %s, want 200", path, rec.Code, rec.Body)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store", path, cc)
		}
	}

	sf := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0"})
	sf.clusters = envs.NewClusters(sf.svc, sf.snaps, envs.ClusterOptions{Now: sf.clk.now})
	sf.svc.WithClusters(sf.clusters)
	sf.h = sf.build(t)
	se := sf.add(t)
	waitFor(t, "the monitor's first probe", func() bool { return sf.mon.Status(se.ID).State == envs.StateOnline })
	for _, path := range []string{"nodes", "nodes/node-1", "nodes/node-1/inspect", "nodes/node-1/tasks"} {
		if rec := sf.get(t, se, path); rec.code != http.StatusConflict || rec.ptyp != "not_a_swarm" {
			t.Errorf("GET %s on a standalone host = %d %s (%s), want 409 not_a_swarm", path, rec.code, rec.body, rec.ptyp)
		}
	}
}

// ndSwarm is a swarm of two managers and a worker, each with an agent, and a fourth node no
// agent runs on. Bayplan's own container is on the second manager.
type ndSwarm struct {
	*agentFix
	env       tunneltest.Environment
	m1, m2, w *agentConn
}

func newNodesSwarm(t *testing.T) *ndSwarm {
	t.Helper()
	f := newAgentFix(t)
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.tclk.Now, Publisher: f.brk, Events: f.hub})
	f.svc.WithClusters(f.clusters)
	f.h = f.build(t)
	env := f.srv.NewEnvironment(t, "Swarm")
	rows := []dockertest.FakeNode{
		{ID: "node-mgr-1", Hostname: "mgr-1", Role: "manager", Leader: true, Address: "10.1.0.1"},
		{ID: "node-mgr-2", Hostname: "mgr-2", Role: "manager", Address: "10.1.0.2"},
		{ID: "node-wrk-1", Hostname: "wrk-1", Address: "10.1.0.3"},
		{ID: "node-ghost", Hostname: "ghost", State: "down", Address: "10.1.0.4"},
	}
	mgr := func(i int, containers ...dockertest.FakeContainer) (dockertest.FakeConfig, *wire.Facts) {
		name := fmt.Sprintf("mgr-%d", i)
		cfg := dockertest.FakeConfig{Hostname: name, SwarmState: "active", SwarmControl: true, NodeRows: rows, Containers: containers}
		return cfg, &wire.Facts{
			EngineID: "ENG:" + name, Hostname: name, EngineReachable: true,
			Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-" + name, ClusterID: "cluster-1", Control: true},
		}
	}
	s := &ndSwarm{agentFix: f, env: env}
	cfg, facts := mgr(1, dockertest.FakeContainer{ID: "aaaaaaaaaaaa11111111", Names: []string{"/web"}, State: "running"})
	s.m1 = f.connect(t, env, cfg, facts)
	cfg, facts = mgr(2, dockertest.FakeContainer{ID: selfID + "ffffffffffffffffffffffffffffffffffffffffffffffffffff", Names: []string{"/bayplan"}, State: "running"})
	s.m2 = f.connect(t, env, cfg, facts)
	s.w = f.connect(t, env, dockertest.FakeConfig{Hostname: "wrk-1", SwarmState: "active"},
		&wire.Facts{EngineID: "ENG:wrk-1", Hostname: "wrk-1", EngineReachable: true, Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-wrk-1"}})
	f.whenState(t, env.ID, "online")
	return s
}

func TestRunsBayplanNode(t *testing.T) {
	// On the local socket's own swarm node.
	f, e := ndEnv(t, nil)
	b := ndList1(t, f, e, "")
	var runs []string
	for _, n := range b.Items {
		if n.RunsBayplan {
			runs = append(runs, n.ID)
		}
	}
	if !slices.Equal(runs, []string{"node-1"}) {
		t.Errorf("local socket: runsBayplan on %v, want only node-1, the node the socket is", runs)
	}

	// On the node of the container that is Bayplan's own, found through the agents.
	s := newNodesSwarm(t)
	s.whenProbed(t, s.m1.id, s.m2.id, s.w.id)
	var l ndList
	waitFor(t, "the node list", func() bool {
		rec := s.get(t, s.env.ID, "nodes")
		if rec.code != http.StatusOK {
			return false
		}
		l = decode[ndList](t, rec)
		for _, n := range l.Items {
			if n.RunsBayplan {
				return true
			}
		}
		return false
	})
	runs = nil
	for _, n := range l.Items {
		if n.RunsBayplan {
			runs = append(runs, n.ID)
		}
	}
	if !slices.Equal(runs, []string{"node-mgr-2"}) {
		t.Errorf("agents: runsBayplan on %v, want only node-mgr-2, the node of Bayplan's own container", runs)
	}
	byID := map[string]ndItem{}
	for _, n := range l.Items {
		byID[n.ID] = n
	}
	for id, agent := range map[string]string{"node-mgr-1": s.m1.id, "node-mgr-2": s.m2.id, "node-wrk-1": s.w.id} {
		if n := byID[id]; !n.AgentReporting || n.TargetNodeID != agent {
			t.Errorf("%s = %+v, want an agent reporting, target %s", id, n, agent)
		}
	}
	if ghost := byID["node-ghost"]; ghost.AgentReporting || ghost.TargetNodeID != "" || ghost.RunsBayplan {
		t.Errorf("node-ghost = %+v, want no agent", ghost)
	}

	// The detail has the same answer and the agent card.
	d := decode[ndDetail](t, s.get(t, s.env.ID, "nodes/node-wrk-1"))
	if !d.Agent.Reporting || d.Agent.Version == "" || d.Agent.LastSeen == "" || d.RunsBayplan {
		t.Errorf("node-wrk-1 = %+v, want its agent's version and last seen, and not Bayplan's node", d)
	}
	if m2 := decode[ndDetail](t, s.get(t, s.env.ID, "nodes/node-mgr-2")); !m2.RunsBayplan {
		t.Errorf("node-mgr-2 detail = %+v, want runsBayplan", m2)
	}
	ghost := decode[ndDetail](t, s.get(t, s.env.ID, "nodes/node-ghost"))
	if ghost.Agent != (ndAgent{}) {
		t.Errorf("node-ghost agent = %+v, want not reporting with no version and no last seen", ghost.Agent)
	}
}

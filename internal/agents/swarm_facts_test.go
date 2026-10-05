// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build swarm

package agents_test

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/proto/wire"
)

// These checks run the product's own Docker code against a real swarm, where the
// earlier plans could only read a fake daemon: what a manager and a worker answer,
// the formats of the IDs, the scope of cluster events and what the merged feed
// makes of them.

func dockerxFor(t *testing.T, n *dockertest.Node) *dockerx.Client {
	t.Helper()
	c, err := dockerx.New(dockertest.TCPTransportFor(n.Host()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestSwarmRealEngineFacts reads a real swarm of three managers and a worker with
// dockerx directly and through the environment API.
func TestSwarmRealEngineFacts(t *testing.T) {
	f := startSwarmFix(t, 3, 1)
	worker := f.sw.Workers[0]
	f.install(t)
	f.waitSwarmOnline(t, 4)
	ctx := execCtx(t, 5*time.Minute)

	// IDs: the formats a real engine uses pass the wire protocol's check, a
	// manager's /info carries the cluster ID and a worker's does not.
	engineIDs := map[string]bool{}
	nodeIDs := map[string]string{} // hostname to swarm node ID
	var cluster string
	for _, n := range f.sw.Nodes() {
		info, err := n.Info(ctx, t)
		if err != nil {
			t.Fatal(err)
		}
		if info.Name != n.Hostname {
			t.Errorf("%s: /info name = %q, want the node's hostname", n.Hostname, info.Name)
		}
		for what, id := range map[string]string{"engine ID": info.ID, "swarm node ID": info.NodeID} {
			if !wire.ValidIdentity(id) {
				t.Errorf("%s: the %s %q fails wire.ValidIdentity", n.Hostname, what, id)
			}
		}
		t.Logf("%s: engine ID %s, node ID %s, state %s, control %v", n.Hostname, info.ID, info.NodeID, info.State, info.Control)
		engineIDs[info.ID] = true
		nodeIDs[n.Hostname] = info.NodeID
		if n.Manager {
			if !wire.ValidIdentity(info.ClusterID) {
				t.Errorf("%s: a manager's /info has no usable Swarm.Cluster.ID: %q", n.Hostname, info.ClusterID)
				continue
			}
			if cluster == "" {
				cluster = info.ClusterID
			} else if info.ClusterID != cluster {
				t.Errorf("%s: cluster ID %q, the first manager said %q", n.Hostname, info.ClusterID, cluster)
			}
		} else if info.ClusterID != "" {
			t.Errorf("%s: a worker's /info carries a cluster ID %q", n.Hostname, info.ClusterID)
		}
	}
	if len(engineIDs) != 4 {
		t.Errorf("engine IDs = %v, want four distinct ones (duplicate-host detection depends on it)", engineIDs)
	}

	// The probe types each node.
	for _, n := range f.sw.Nodes() {
		res, err := dockerxFor(t, n).Probe(ctx)
		if err != nil {
			t.Fatalf("probe %s: %v", n.Hostname, err)
		}
		want := dockerx.TypeSwarmWorker
		if n.Manager {
			want = dockerx.TypeSwarmManager
		}
		if !res.Supported || res.Type != want || res.SwarmNodeState != "active" || res.Hostname != n.Hostname {
			t.Errorf("probe of %s = %+v, want a supported %s, swarm state active", n.Hostname, res, want)
		}
	}

	// The cluster view from each manager: leader, reachability, engine versions,
	// availability and state strings, the agent service's counts and tasks.
	engine := strings.TrimSpace(sh(t, f.sw.Managers[0], "docker version --format '{{.Server.Version}}'"))
	for _, m := range f.sw.Managers {
		read, err := dockerxFor(t, m).Cluster(ctx)
		if err != nil {
			t.Fatalf("cluster read on %s: %v", m.Hostname, err)
		}
		if len(read.Nodes) != 4 {
			t.Fatalf("%s lists %d nodes, want 4: %+v", m.Hostname, len(read.Nodes), read.Nodes)
		}
		leaders := 0
		for _, n := range read.Nodes {
			var h *dockertest.Node
			for _, c := range f.sw.Nodes() {
				if c.Hostname == n.Hostname {
					h = c
				}
			}
			if h == nil {
				t.Errorf("%s lists a node %q the harness never started", m.Hostname, n.Hostname)
				continue
			}
			wantRole, wantReach := "worker", ""
			if h.Manager {
				wantRole, wantReach = "manager", "reachable"
			}
			if n.Role != wantRole || n.Availability != "active" || n.State != "ready" || n.OS != "linux" || n.Reachability != wantReach || n.EngineVersion != engine || n.ID != nodeIDs[n.Hostname] {
				t.Errorf("%s sees node %+v, want role %s, active, ready, linux, reachability %q, engine %s, ID %s", m.Hostname, n, wantRole, wantReach, engine, nodeIDs[n.Hostname])
			}
			if n.Leader {
				leaders++
			}
		}
		if leaders != 1 {
			t.Errorf("%s sees %d leaders, want exactly one", m.Hostname, leaders)
		}
		var svc *dockerx.ServiceRow
		for i := range read.Services {
			if read.Services[i].Name == agentName {
				svc = &read.Services[i]
			}
		}
		// ?status=true counts for a global service: one task per node.
		if svc == nil || svc.Mode != "global" || svc.Running != 4 || svc.Desired != 4 {
			t.Errorf("%s sees the agent service as %+v, want global with 4 of 4 running", m.Hostname, svc)
		}
		if len(read.AgentTasks) != 4 {
			t.Errorf("%s sees %d agent tasks, want 4: %+v", m.Hostname, len(read.AgentTasks), read.AgentTasks)
		}
		for _, task := range read.AgentTasks {
			if task.State != "running" || task.Error != "" {
				t.Errorf("%s sees agent task %+v, want running with no error", m.Hostname, task)
			}
		}
	}

	// A worker's answer to a cluster question is a 503 whose message the server
	// maps to not_a_manager; it still answers node-local ones.
	_, err := dockerxFor(t, worker).Cluster(ctx)
	if err == nil || envs.ReasonFor(err) != envs.ReasonNotAManager {
		t.Errorf("a worker's cluster read = %v, want an error ReasonFor maps to not_a_manager", err)
	}
	for _, path := range []string{"/nodes", "/services", "/tasks"} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+strings.TrimPrefix(worker.Host(), "tcp://")+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s on the worker: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "This node is not a swarm manager") {
			t.Errorf("worker GET %s = %d %s, want 503 with the not-a-manager message", path, resp.StatusCode, body)
		}
	}

	// The agent service is found by its label and not its name: the check the
	// stack-file install relies on, here proved on the service as installed.
	if out := strings.TrimSpace(sh(t, f.sw.Managers[0], "docker service ls --filter label=bayplan.agent=true --format '{{.Name}}'")); out != agentName {
		t.Errorf("services labelled bayplan.agent=true = %q, want %s", out, agentName)
	}

	// The environment's agent count is what the service says: running tasks.
	ps := strings.Fields(sh(t, f.sw.Managers[0], "docker service ps "+agentName+" --filter desired-state=running --format '{{.CurrentState}}'"))
	running := 0
	for _, s := range ps {
		if s == "Running" {
			running++
		}
	}
	if env, _ := f.srv.Environment(f.env.ID); env.Agents == nil || env.Agents.Connected != running || env.Agents.Expected != len(f.sw.Nodes()) {
		t.Errorf("environment agents = %+v, docker service ps shows %d running of %d nodes", env.Agents, running, len(f.sw.Nodes()))
	}
}

// TestSwarmRealEvents checks what real cluster events look like and what the
// merged feed does with them: a swarm-scoped event comes from every manager and
// shows once, a node-local one comes from its node only and names it.
func TestSwarmRealEvents(t *testing.T) {
	f := startSwarmFix(t, 3, 1)
	worker, mgr1, mgr2 := f.sw.Workers[0], f.sw.Managers[0], f.sw.Managers[1]
	dockertest.LoadImage(t, "busybox", worker, mgr2)
	f.install(t)
	f.waitSwarmOnline(t, 4)
	ctx := execCtx(t, 5*time.Minute)

	type seen struct {
		scope, typ, action, name string
	}
	streams := map[string]chan seen{}
	for _, n := range f.sw.Nodes() {
		rows, _ := dockerxFor(t, n).Events(ctx, time.Time{})
		ch := make(chan seen, 256)
		streams[n.Hostname] = ch
		go func() {
			for r := range rows {
				select {
				case ch <- seen{r.Scope, r.Type, r.Action, r.Name}:
				default:
				}
			}
		}()
	}
	// Start the environment's own feed before anything happens.
	if code, body := f.srv.Do(http.MethodGet, "/api/env/"+f.env.ID+"/events", nil); code != http.StatusOK {
		t.Fatalf("GET events = %d %s", code, body)
	}

	sh(t, mgr1, "docker network create -d overlay --attachable probe-net")
	sh(t, worker, "docker run -d --name ev-w1 busybox sleep 300")
	sh(t, mgr2, "docker run -d --name ev-m2 busybox sleep 300")

	// What each engine's own stream carried.
	collect := func(host string, want seen) bool {
		for {
			select {
			case s := <-streams[host]:
				if s == want {
					return true
				}
			case <-time.After(20 * time.Second):
				return false
			}
		}
	}
	for _, m := range f.sw.Managers {
		if !collect(m.Hostname, seen{"swarm", "network", "create", "probe-net"}) {
			t.Errorf("manager %s never reported the swarm-scoped network create", m.Hostname)
		}
	}
	if !collect(worker.Hostname, seen{"local", "container", "start", "ev-w1"}) {
		t.Errorf("the worker never reported its own container start with scope local")
	}
	if !collect(mgr2.Hostname, seen{"local", "container", "start", "ev-m2"}) {
		t.Errorf("manager 2 never reported its own container start with scope local")
	}
	// The worker has no cluster scope.
	for len(streams[worker.Hostname]) > 0 {
		if s := <-streams[worker.Hostname]; s.scope == "swarm" {
			t.Errorf("the worker's stream carried a swarm-scoped event: %+v", s)
		}
	}

	// The merged feed: one row for the network, the container rows on their nodes.
	type dto struct{ Node, Type, Action, Name string }
	var rows []dto
	f.srv.WaitFor("the merged feed to show the three events", 30*time.Second, func() (bool, string) {
		code, body := f.srv.Do(http.MethodGet, "/api/env/"+f.env.ID+"/events", nil)
		if code != http.StatusOK {
			return false, string(body)
		}
		var l struct{ Items []dto }
		_ = json.Unmarshal(body, &l)
		rows = l.Items
		has := func(node, typ, action, name string) bool {
			return slices.ContainsFunc(rows, func(r dto) bool {
				return r.Type == typ && r.Action == action && r.Name == name && (node == "" || r.Node == node)
			})
		}
		return has("", "network", "create", "probe-net") && has(worker.Hostname, "container", "start", "ev-w1") && has(mgr2.Hostname, "container", "start", "ev-m2"), string(body)
	})
	creates := 0
	for _, r := range rows {
		if r.Type == "network" && r.Action == "create" && r.Name == "probe-net" {
			creates++
		}
	}
	if creates != 1 {
		t.Errorf("the merged feed shows the swarm-scoped network create %d times, want once", creates)
	}
}

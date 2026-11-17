// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build swarm

package agents_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/server/servertest"
)

// This is spike S13 of phase 19 as a test (SWM-01, SWM-02, D-04 to D-10): a TierPlan that runs on a swarm
// worker, with no Local environment, adds its swarm through the agents and then folds its own node into
// that environment by node ID, from the Docker socket it detected. It needs a swarm of its own (it
// promotes, demotes and removes a node, which TestSwarmShared forbids), so it is a TestSwarm function.

// localHostFacts is the part of GET /api/local-host these tests read.
type localHostFacts struct {
	Detected bool `json:"detected"`
	Managed  bool `json:"managed"`
	Probe    *struct {
		Type              string   `json:"type"`
		SwarmManagerHosts []string `json:"swarmManagerHosts"`
	} `json:"probe"`
	Swarm *struct {
		EnvironmentID string `json:"environmentId"`
		FoldOffered   bool   `json:"foldOffered"`
	} `json:"swarm"`
}

// foldOfferFacts is the part of GET /api/environments/{id}/fold-offer these tests read.
type foldOfferFacts struct {
	Offered bool   `json:"offered"`
	Source  string `json:"source"`
	Role    string `json:"role"`
	Refusal string `json:"refusal"`
}

// getInto GETs a path as the admin and decodes a 200 answer into out.
func getInto(t *testing.T, srv *servertest.Server, path string, out any) {
	t.Helper()
	code, body := srv.Do(http.MethodGet, path, nil)
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, code, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("GET %s: %v: %s", path, err, body)
	}
}

// localHost reads GET /api/local-host.
func localHost(t *testing.T, srv *servertest.Server) localHostFacts {
	t.Helper()
	var h localHostFacts
	getInto(t, srv, "/api/local-host", &h)
	return h
}

// foldOffer reads GET /api/environments/{id}/fold-offer.
func foldOffer(t *testing.T, srv *servertest.Server, envID string) foldOfferFacts {
	t.Helper()
	var o foldOfferFacts
	getInto(t, srv, "/api/environments/"+envID+"/fold-offer", &o)
	return o
}

// environmentCount is how many environments the list holds.
func environmentCount(t *testing.T, srv *servertest.Server) int {
	t.Helper()
	var l struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	getInto(t, srv, "/api/environments", &l)
	return len(l.Items)
}

// stackNamed counts the stacks of an environment with this name.
func stackNamed(t *testing.T, srv *servertest.Server, envID, name string) int {
	t.Helper()
	var l struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	getInto(t, srv, "/api/env/"+envID+"/stacks", &l)
	n := 0
	for _, s := range l.Items {
		if s.Name == name {
			n++
		}
	}
	return n
}

// workerFoldFix is the swarm and the TierPlan of TestSwarmWorkerFold.
type workerFoldFix struct {
	sw     *dockertest.Swarm
	worker *dockertest.Node
	proxy  *dockertest.SwitchProxy
	srv    *servertest.Server
	env    servertest.Created
}

// foldAgentRow is a row of the agents list with the fields these tests read that servertest.Agent does not carry.
type foldAgentRow struct {
	Node     string `json:"node"`
	Role     string `json:"role"`
	NodeRole string `json:"nodeRole"`
	Standby  bool   `json:"standby"`
}

func (f *workerFoldFix) agentRows(t *testing.T) []foldAgentRow {
	t.Helper()
	var l struct {
		Items []foldAgentRow `json:"items"`
	}
	getInto(t, f.srv, "/api/env/"+f.env.ID+"/agents", &l)
	return l.Items
}

// TestSwarmWorkerFold is the S13 path on a real swarm A (2 managers, 1 worker), with no Local environment.
// The server's socket is first a worker of another swarm, B, which swarm A's environment must refuse; it is
// then switched to swarm A's worker, which folds, follows promotion and demotion, and shows as degraded
// once it is removed from the swarm.
func TestSwarmWorkerFold(t *testing.T) {
	sw := dockertest.StartSwarm(t, "", 2, 1)
	dockertest.LoadAgentImage(t, sw.Nodes()...)
	dockertest.LoadImage(t, "busybox", sw.Nodes()...)
	dumpOnFailure(t, sw.Nodes()...)
	worker := sw.Workers[0]
	mgr := sw.Managers[0]

	// Swarm B is only a place for the server's socket to point at: no agent is installed on it.
	swB := dockertest.StartSwarm(t, "", 1, 1)
	dumpOnFailure(t, swB.Nodes()...)

	proxy := dockertest.SwitchableUnixProxy(t, swB.Workers[0].Host())
	srv := servertest.Start(t, servertest.Options{Host: sw.Gateway, Env: map[string]string{"DOCKER_HOST": "unix://" + proxy.Path()}})
	f := &workerFoldFix{sw: sw, worker: worker, proxy: proxy, srv: srv, env: srv.CreateAgentEnvironment("")}

	// The swarm is added through its agents. Nothing here reads the server's own socket.
	t.Logf("swarm install on %s:\n%s", mgr.Hostname, quiet(sh(t, mgr, f.env.Install.Swarm.Command)))
	srv.WaitOnline(f.env.ID, installWait)
	srv.WaitFor("the swarm's node list to hold the 3 nodes", installWait, func() (bool, string) {
		e, ok := srv.Environment(f.env.ID)
		if !ok || e.Status.State != "online" || e.Nodes == nil || e.Nodes.Total != 3 || e.Agents == nil || e.Agents.Connected != 3 {
			return false, summarise(e)
		}
		srv.RefreshCluster(f.env.ID)
		return true, summarise(e)
	})

	// S13 row 6: the socket is a worker of swarm B. Its managers are not swarm A's, so the offer is hidden
	// with no refusal to explain, local-host names no swarm, and the fold is refused with nothing changed.
	h := localHost(t, srv)
	if !h.Detected || h.Probe == nil || h.Probe.Type != "swarm-worker" || len(h.Probe.SwarmManagerHosts) != 1 || h.Managed || h.Swarm != nil {
		t.Fatalf("local-host on swarm B's worker = %+v probe %+v swarm %+v, want a detected, unmanaged swarm-worker with its one manager and no swarm field", h, h.Probe, h.Swarm)
	}
	if o := foldOffer(t, srv, f.env.ID); o.Offered || o.Refusal != "" {
		t.Fatalf("fold-offer on swarm B's worker = %+v, want not offered and no refusal", o)
	}
	code, body := srv.Do(http.MethodPost, "/api/environments/"+f.env.ID+"/fold-local", map[string]any{})
	var problem struct {
		Type string `json:"type"`
	}
	if code != http.StatusConflict || json.Unmarshal(body, &problem) != nil || problem.Type != "fold_node_not_listed" {
		t.Fatalf("POST fold-local on swarm B's worker = %d %s, want 409 fold_node_not_listed", code, body)
	}
	if n := environmentCount(t, srv); n != 1 {
		t.Errorf("%d environments after the refused fold, want swarm A's only", n)
	}
	if row := srv.Row(f.env.ID); row.LocalNodeID.Valid {
		t.Errorf("local_node_id = %+v after the refused fold, want unset", row.LocalNodeID)
	}
	if rows := srv.AuditRows("environment.local_folded"); len(rows) != 0 {
		t.Errorf("audit environment.local_folded = %+v after the refused fold, want none", rows)
	}

	// From here on the socket is swarm A's worker.
	proxy.PointAt(worker.Host())

	// The socket is the worker's: a swarm-worker probe naming the managers, not managed, and the swarm
	// environment that this node belongs to, offered for the fold.
	h = localHost(t, srv)
	if !h.Detected || h.Probe == nil || h.Probe.Type != "swarm-worker" || len(h.Probe.SwarmManagerHosts) != 2 || h.Managed {
		t.Fatalf("local-host = %+v probe %+v, want a detected, unmanaged swarm-worker with 2 manager hosts", h, h.Probe)
	}
	for _, host := range h.Probe.SwarmManagerHosts {
		if strings.Contains(host, ":") {
			t.Errorf("manager host %q carries a port", host)
		}
	}
	srv.WaitFor("local-host to name the swarm environment with its fold offered", installWait, func() (bool, string) {
		srv.RefreshCluster(f.env.ID)
		h = localHost(t, srv)
		return h.Swarm != nil && h.Swarm.EnvironmentID == f.env.ID && h.Swarm.FoldOffered, "swarm " + toJSON(h.Swarm)
	})
	if o := foldOffer(t, srv, f.env.ID); !o.Offered || o.Source != "socket" || o.Role != "worker" {
		t.Fatalf("fold-offer = %+v, want offered from the socket as a worker", o)
	}
	if n := environmentCount(t, srv); n != 1 {
		t.Fatalf("%d environments before the fold, want the swarm's only", n)
	}

	// The fold.
	if code, body := srv.Do(http.MethodPost, "/api/environments/"+f.env.ID+"/fold-local", map[string]any{}); code != http.StatusOK {
		t.Fatalf("POST fold-local = %d %s, want 200", code, body)
	}
	rows := srv.AuditRows("environment.local_folded")
	if len(rows) != 1 || rows[0].Details["source"] != "socket" || rows[0].Details["role"] != "worker" {
		t.Fatalf("audit environment.local_folded = %+v, want one row from the socket as a worker", rows)
	}
	if h = localHost(t, srv); !h.Managed || h.Swarm != nil && h.Swarm.EnvironmentID != f.env.ID {
		t.Fatalf("local-host after the fold = %+v, want managed", h)
	}
	if n := environmentCount(t, srv); n != 1 {
		t.Errorf("%d environments after the fold, want exactly one", n)
	}

	// A stack-labelled service of the swarm is listed once, and the worker's own container once, on the worker.
	sh(t, mgr, "docker service create --detach --no-resolve-image --name s19_web --label com.docker.stack.namespace=s19 "+
		"--container-label com.docker.stack.namespace=s19 --constraint node.role==worker busybox sleep 600")
	srv.WaitFor("the stack s19 to be listed once", installWait, func() (bool, string) {
		n := stackNamed(t, srv, f.env.ID, "s19")
		return n == 1, "stacks named s19: " + strconv.Itoa(n)
	})
	srv.WaitFor("the service's task container to be listed once, on the worker", installWait, func() (bool, string) {
		list, code, body := srv.Containers(f.env.ID, "")
		n := 0
		for _, c := range list.Items {
			if strings.HasPrefix(c.Name, "s19_web.") {
				n++
				if c.Node != worker.Hostname {
					return false, "container " + c.Name + " on " + c.Node
				}
			}
		}
		return code == http.StatusOK && n == 1, string(body)
	})

	// The agent on the worker is the standby; the folded row says it is a worker.
	var sawStandby, sawLocal bool
	for _, a := range f.agentRows(t) {
		switch {
		case a.Role == "local":
			sawLocal = true
			if a.NodeRole != "worker" {
				t.Errorf("the local row's nodeRole = %q, want worker", a.NodeRole)
			}
		case a.Node == worker.Hostname:
			sawStandby = true
			if !a.Standby {
				t.Errorf("the agent on the worker is not marked standby: %+v", a)
			}
		case a.Standby:
			t.Errorf("the agent on %s is marked standby, only the folded node's is: %+v", a.Node, a)
		}
	}
	if !sawLocal || !sawStandby {
		t.Errorf("agents list: local row %v, standby agent on the worker %v, want both: %+v", sawLocal, sawStandby, f.agentRows(t))
	}
}

// toJSON is a value as one line, for a failure message.
func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

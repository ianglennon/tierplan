// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build swarm

package agents_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/server/servertest"
)

// This is the manager-hosted counterpart of TestSwarmWorkerFold (SWM-01, SWM-02, D-01, D-04, D-06, D-07,
// D-08, D-10, D-21): the host type the maintainer hit on 2026-11-15. A TierPlan whose Docker socket is a
// swarm manager and that has no Local environment refuses to add that socket as an environment of its own,
// adds the swarm through the agents, folds its own node from the socket by cluster ID and node ID, lists
// every stack once, and keeps the agent on its node as a standby. The socket never changes target, so the
// plain UnixProxy serves (the worker test needs the switchable one). It reads its facts with the helpers
// of swarm_worker_fold_test.go (localHost, foldOffer, environmentCount, stackNamed, foldAgentRow).

// TestSwarmManagerFold runs the path on a swarm of two managers and no worker: TierPlan's own manager,
// and a second manager whose agent answers cluster reads before the fold and carries half of the stack.
func TestSwarmManagerFold(t *testing.T) {
	sw := dockertest.StartSwarm(t, "", 2, 0)
	dockertest.LoadAgentImage(t, sw.Nodes()...)
	dockertest.LoadImage(t, "busybox", sw.Nodes()...)
	dumpOnFailure(t, sw.Nodes()...)
	host := sw.Managers[1] // TierPlan's own manager; the agent install runs on the other one
	other := sw.Managers[0]

	proxy := dockertest.UnixProxy(t, host.Host())
	srv := servertest.Start(t, servertest.Options{Host: sw.Gateway, Env: map[string]string{"DOCKER_HOST": "unix://" + proxy}})

	// D-01, D-21: the socket is a manager of a swarm, so local-host says so, names the managers without
	// ports and no swarm environment (there is none yet), and adding it as a Local environment is refused.
	h := localHost(t, srv)
	if !h.Detected || h.Probe == nil || h.Probe.Type != "swarm-manager" || len(h.Probe.SwarmManagerHosts) != 2 || h.Managed || h.Swarm != nil {
		t.Fatalf("local-host on a swarm manager = %+v probe %+v swarm %+v, want a detected, unmanaged swarm-manager with 2 manager hosts and no swarm field", h, h.Probe, h.Swarm)
	}
	for _, mh := range h.Probe.SwarmManagerHosts {
		if strings.Contains(mh, ":") {
			t.Errorf("manager host %q carries a port", mh)
		}
	}
	code, body := srv.Do(http.MethodPost, "/api/environments", map[string]any{"kind": "local"})
	var problem struct {
		Type string `json:"type"`
	}
	if code != http.StatusConflict || json.Unmarshal(body, &problem) != nil || problem.Type != "local_is_swarm_node" {
		t.Fatalf("POST /api/environments kind local on a swarm manager = %d %s, want 409 local_is_swarm_node", code, body)
	}
	if n := environmentCount(t, srv); n != 0 {
		t.Fatalf("%d environments after the refused local add, want none", n)
	}
	t.Logf("D-21: the manager's socket was refused as an environment; none exists")

	// The swarm is added through its agents, installed from the other manager. The node list holds both.
	env := srv.CreateAgentEnvironment("")
	t.Logf("swarm install on %s:\n%s", other.Hostname, quiet(sh(t, other, env.Install.Swarm.Command)))
	srv.WaitOnline(env.ID, installWait)
	srv.WaitFor("the swarm's node list to hold the 2 managers", installWait, func() (bool, string) {
		e, ok := srv.Environment(env.ID)
		if !ok || e.Status.State != "online" || e.Nodes == nil || e.Nodes.Total != 2 || e.Agents == nil || e.Agents.Connected != 2 {
			return false, summarise(e)
		}
		srv.RefreshCluster(env.ID)
		return true, summarise(e)
	})

	// D-04, D-06: local-host names the swarm environment this node belongs to, and the fold is offered
	// from the socket as a manager.
	srv.WaitFor("local-host to name the swarm environment with its fold offered", installWait, func() (bool, string) {
		srv.RefreshCluster(env.ID)
		h = localHost(t, srv)
		return h.Swarm != nil && h.Swarm.EnvironmentID == env.ID && h.Swarm.FoldOffered, "swarm " + toJSON(h.Swarm)
	})
	if o := foldOffer(t, srv, env.ID); !o.Offered || o.Source != "socket" || o.Role != "manager" {
		t.Fatalf("fold-offer = %+v, want offered from the socket as a manager", o)
	}
	if n := environmentCount(t, srv); n != 1 {
		t.Fatalf("%d environments before the fold, want the swarm's only", n)
	}

	// D-07: the fold.
	if code, body := srv.Do(http.MethodPost, "/api/environments/"+env.ID+"/fold-local", map[string]any{}); code != http.StatusOK {
		t.Fatalf("POST fold-local = %d %s, want 200", code, body)
	}
	rows := srv.AuditRows("environment.local_folded")
	if len(rows) != 1 || rows[0].Details["source"] != "socket" || rows[0].Details["role"] != "manager" {
		t.Fatalf("audit environment.local_folded = %+v, want one row from the socket as a manager", rows)
	}
	if h = localHost(t, srv); !h.Managed {
		t.Fatalf("local-host after the fold = %+v, want managed", h)
	}
	if row := srv.Row(env.ID); !row.LocalNodeID.Valid || row.LocalNodeID.String == "" {
		t.Errorf("local_node_id = %+v after the fold, want it set", row.LocalNodeID)
	}
	if n := environmentCount(t, srv); n != 1 {
		t.Errorf("%d environments after the fold, want exactly one", n)
	}
	t.Logf("after the fold: %s", summarise(mustEnvironment(t, srv, env.ID)))
}

// mustEnvironment is the environment as the list reports it, for a log line.
func mustEnvironment(t *testing.T, srv *servertest.Server, id string) servertest.Environment {
	t.Helper()
	e, ok := srv.Environment(id)
	if !ok {
		t.Fatalf("environment %s is not listed", id)
	}
	return e
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/tierplan/proto/wire"
)

// A folded host keeps the agent the global service runs on its node. These tests read what
// the API says about that agent and about the folded node itself (D-08, D-10, D-11, D-19).

// standbyRow is the part of a node-list row these tests read.
type standbyRow struct {
	ID       string `json:"id"`
	Node     string `json:"node"`
	Role     string `json:"role"`
	Standby  bool   `json:"standby"`
	NodeRole string `json:"nodeRole"`
}

// connectStandby connects the agent that the swarm's global service runs on the folded
// host's node: it reports the folded host's swarm node ID, a worker's facts, and the same
// host name. skew is how far its clock is ahead of the server's.
func (f *foldFix) connectStandby(t *testing.T, skew time.Duration) *agentConn {
	t.Helper()
	a := f.connectAs(t, f.env, dockertest.FakeConfig{
		Hostname: "local-1", SwarmState: "active", SwarmNodeID: "node-local", SwarmClusterID: "cluster-1",
		NodeRows: []dockertest.FakeNode{
			{ID: "node-mgr-1", Hostname: "mgr-1", Role: "manager", EngineVersion: "29.1.0", OS: "linux"},
			{ID: "node-local", Hostname: "local-1", Role: "manager", EngineVersion: "29.1.0", OS: "linux", Leader: true},
		},
	}, &wire.Facts{
		EngineID: "ENG:local-agent", Hostname: "local-1", EngineReachable: true, EngineVersion: "29.1.0",
		Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-local", ClusterID: "cluster-1"},
	}, tunneltest.Agent{ClockSkew: skew})
	awaitFirstProbe(t, f.mon, envs.AgentTarget(a.id))
	return a
}

// nodeRows reads the node list of the swarm environment.
func (f *foldFix) nodeRows(t *testing.T) []standbyRow {
	t.Helper()
	rec := f.req(t, http.MethodGet, "/api/env/"+f.env.ID+"/agents", "")
	if rec.code != http.StatusOK {
		t.Fatalf("agents: %d %s", rec.code, rec.body)
	}
	var list struct {
		Items []standbyRow `json:"items"`
	}
	if err := json.Unmarshal(rec.body, &list); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func (f *foldFix) fold(t *testing.T) {
	t.Helper()
	if rec := f.req(t, http.MethodPost, f.foldPath(), ""); rec.code != http.StatusOK {
		t.Fatalf("fold: %d %s", rec.code, rec.body)
	}
	// The fold re-registers the socket, so its row reads as unprobed until the next probe.
	awaitProbed(t, f.mon, envs.SocketTarget(f.env.ID))
}

func TestTracerTheNodeListMarksTheAgentOnTheFoldedNodeAsStandby(t *testing.T) {
	t.Parallel()
	f := newFoldFix(t, nil)
	f.fold(t)
	standby := f.connectStandby(t, 0)

	rows := f.nodeRows(t)
	if len(rows) != 3 {
		t.Fatalf("rows = %+v, want mgr-1, the agent on the folded node and the folded host", rows)
	}
	for _, r := range rows {
		switch {
		case r.ID == standby.id:
			if !r.Standby {
				t.Errorf("the agent on the folded node is %+v, want standby true", r)
			}
		case r.Role == "local":
			if r.NodeRole != "manager" || r.Standby {
				t.Errorf("the folded row is %+v, want nodeRole manager and not standby", r)
			}
		default:
			if r.Node != "mgr-1" || r.Standby {
				t.Errorf("row %+v, want mgr-1 with standby false", r)
			}
		}
	}
}

func TestTheFoldedRowOfAWorkerHostSaysWorker(t *testing.T) {
	t.Parallel()
	f := newWorkerFoldFix(t, nil)
	f.fold(t)
	rec := f.req(t, http.MethodGet, "/api/env/"+f.env.ID+"/agents", "")
	if rec.code != http.StatusOK {
		t.Fatalf("agents: %d %s", rec.code, rec.body)
	}
	var list struct {
		Items []standbyRow `json:"items"`
	}
	if err := json.Unmarshal(rec.body, &list); err != nil {
		t.Fatal(err)
	}
	var local *standbyRow
	for _, it := range list.Items {
		if it.Role == "local" {
			it := it
			local = &it
		} else if it.Standby {
			t.Errorf("%+v is marked standby, want no agent to be", it)
		}
	}
	if local == nil || local.NodeRole != "worker" {
		t.Fatalf("the folded row is %+v in %s, want nodeRole worker", local, rec.body)
	}
}

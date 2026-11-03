// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// The janitor settings of one swarm node (plan 11-23, JAN-01, D-04, D-15, D-21): a node can be switched off or keep
// more or fewer of its task containers, and everything else follows the environment. The swarm is the one of
// janitorSwarm, two managers and a worker behind real agent sessions, whose managers list the three nodes (and
// one that is down) so that the node routes know them.

// The swarm node IDs of the fixture's agents (newSwarmFixHostNet reports "node-"+hostname).
const (
	nodeMgr1 = "node-mgr-1"
	nodeMgr2 = "node-mgr-2"
	nodeWrk1 = "node-wrk-1"
	// nodeGhost is a node the managers list as down: no agent reaches it.
	nodeGhost = "node-ghost-1"
)

// nodeRows are the swarm nodes the managers list.
func nodeRows() []dockertest.FakeNode {
	return []dockertest.FakeNode{
		{ID: nodeMgr1, Hostname: "mgr-1", Role: "manager", Leader: true, Reachability: "reachable"},
		{ID: nodeMgr2, Hostname: "mgr-2", Role: "manager", Reachability: "reachable"},
		{ID: nodeWrk1, Hostname: "wrk-1"},
		{ID: nodeGhost, Hostname: "ghost-1", State: "down"},
	}
}

// nodeSwarm is janitorSwarm whose managers list nodeRows.
func nodeSwarm(t *testing.T, boxes map[string][]taskBox) *swarmFix {
	t.Helper()
	return janitorSwarm(t, boxes, func(host string, cfg *dockertest.FakeConfig) {
		if strings.HasPrefix(host, "mgr-") {
			cfg.NodeRows = nodeRows()
		}
	})
}

// twoNodeBoxes puts four exited containers of a slot of web on the worker and four of another slot on the first
// manager, all stopped two days ago: with the default keep of 2 each node has its two oldest to remove.
func twoNodeBoxes() map[string][]taskBox {
	boxes := map[string][]taskBox{"wrk-1": exitedSlot(4)}
	for i := range 4 {
		boxes["mgr-1"] = append(boxes["mgr-1"], taskBox{id: fmt.Sprintf("m-x%d", i+1), service: "web", serviceID: "svc-web", slot: "2", state: "exited", ageIdx: i, finishedAgo: 48 * time.Hour})
	}
	return boxes
}

type janitorNodeRow struct {
	NodeID    string `json:"nodeId"`
	Name      string `json:"name"`
	Reporting bool   `json:"reporting"`
	Enabled   struct {
		Value      bool `json:"value"`
		Overridden bool `json:"overridden"`
	} `json:"enabled"`
	KeepN struct {
		Value      int  `json:"value"`
		Overridden bool `json:"overridden"`
	} `json:"keepN"`
}

type janitorNodesAnswer struct {
	Nodes []janitorNodeRow `json:"nodes"`
}

func (s *swarmFix) nodePath(nodeID string) string {
	return "/api/env/" + s.env.ID + "/janitor/nodes/" + nodeID
}

func (s *swarmFix) putNode(t *testing.T, nodeID, body string) *envRec {
	t.Helper()
	return s.req(t, http.MethodPut, s.nodePath(nodeID), body)
}

func (s *swarmFix) resetNode(t *testing.T, nodeID string) *envRec {
	t.Helper()
	return s.req(t, http.MethodDelete, s.nodePath(nodeID), "")
}

type nodeAudit struct {
	TargetType, TargetID, EnvID, Result string
	Details                             map[string]string
}

// nodeAudits are the janitor.node_policy_changed rows of the audit log.
func (s *swarmFix) nodeAudits(t *testing.T) []nodeAudit {
	t.Helper()
	rows, err := s.st.Read.Query(`SELECT target_type, target_id, COALESCE(env_id, ''), result, COALESCE(details, '{}')
		FROM audit_log WHERE action = 'janitor.node_policy_changed' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []nodeAudit
	for rows.Next() {
		var r nodeAudit
		var details string
		if err := rows.Scan(&r.TargetType, &r.TargetID, &r.EnvID, &r.Result, &details); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(details), &r.Details); err != nil {
			t.Fatalf("details %q: %v", details, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// storedNodeRows is how many node rows the store holds.
func (s *swarmFix) storedNodeRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := s.st.Read.QueryRow(`SELECT COUNT(*) FROM janitor_node_policy`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// refsOf are the refs of preview items, in the preview's order.
func refsOf(items []janitorItem) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.Ref)
	}
	return out
}

// The tracer (plan 11-23, D-04): with the janitor off for the first manager, its stale task containers are
// protected (node_disabled) in the preview and no run removes them, while the worker's are still removed. Nothing
// else about the swarm changes.
func TestJanitorNodeDisabledProtects(t *testing.T) {
	t.Parallel()
	s := nodeSwarm(t, twoNodeBoxes())
	_, before := s.previewJanitor(t)
	if want := []string{"m-x1", "m-x2", "web-x1", "web-x2"}; !slices.Equal(sortedStrings(refsOf(before.Items)), want) {
		t.Fatalf("items before = %v, want the two oldest of each node %v", refsOf(before.Items), want)
	}

	rec := s.putNode(t, nodeMgr1, `{"enabled": false}`)
	if rec.code != http.StatusOK {
		t.Fatalf("PUT node: %d %s", rec.code, rec.body)
	}
	var saved janitorNodeRow
	if err := json.Unmarshal(rec.body, &saved); err != nil || saved.NodeID != nodeMgr1 || saved.Name != "mgr-1" || !saved.Reporting ||
		saved.Enabled.Value || !saved.Enabled.Overridden || saved.KeepN.Overridden || saved.KeepN.Value != 2 {
		t.Fatalf("answer = %s (%v), want mgr-1 reporting, disabled by its own setting, keep following the environment's 2", rec.body, err)
	}

	_, after := s.previewJanitor(t)
	if want := []string{"web-x1", "web-x2"}; !slices.Equal(refsOf(after.Items), want) {
		t.Fatalf("items = %v, want only the worker's %v", refsOf(after.Items), want)
	}
	var protected []string
	for _, it := range after.NotRemoved {
		if it.Class != "protected" || it.Reason.Code != "node_disabled" || it.Node != "mgr-1" || it.Reason.Service != "web" || it.Reason.Slot != "2" {
			t.Errorf("notRemoved %+v, want protected node_disabled on mgr-1 naming its service and slot", it)
		}
		protected = append(protected, it.Ref)
	}
	if want := []string{"m-x1", "m-x2"}; !slices.Equal(protected, want) {
		t.Fatalf("protected = %v, want mgr-1's two oldest %v", protected, want)
	}

	// Everything the preview lists, the protected ones too, is ticked: the protected are not removed.
	_, started := s.runJanitor(t, tick(append(slices.Clone(after.Items), after.NotRemoved...)...))
	s.awaitRun(t, started.JobID)
	if got := s.containerDeletes(); !slices.Equal(sortedStrings(got), []string{"DELETE /containers/web-x1", "DELETE /containers/web-x2"}) {
		t.Fatalf("the daemons saw %v, want only the worker's two deletes", got)
	}
	// Switched on again, the first manager's are removable at once.
	if rec := s.resetNode(t, nodeMgr1); rec.code != http.StatusOK {
		t.Fatalf("DELETE node: %d %s", rec.code, rec.body)
	}
	_, again := s.previewJanitor(t)
	if want := []string{"m-x1", "m-x2"}; !slices.Equal(refsOf(again.Items), want) {
		t.Fatalf("items after the reset = %v, want mgr-1's %v (the worker's are gone)", refsOf(again.Items), want)
	}
	rows := s.nodeAudits(t)
	if len(rows) != 2 || rows[0].Details["node"] != "mgr-1" || rows[0].Details["fields"] != "enabled" || rows[0].EnvID != s.env.ID ||
		rows[1].Details["node"] != "mgr-1" || rows[1].Details["fields"] != "enabled" {
		t.Fatalf("audit rows = %+v, want the save and the reset, each naming mgr-1 and the field", rows)
	}
}

func sortedStrings(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// D-14: the preview takes candidate node settings beside the policy, so a node can be switched off or made to
// keep fewer before anything is saved; a candidate replaces that node's saved settings and saves nothing.
func TestJanitorNodePreviewCandidate(t *testing.T) {
	t.Parallel()
	s := nodeSwarm(t, twoNodeBoxes())
	cand := func(body string) janitorPreview {
		t.Helper()
		rec, got := s.previewWith(t, body)
		if rec.code != http.StatusOK {
			t.Fatalf("preview %s: %d %s", body, rec.code, rec.body)
		}
		return got
	}
	if got := cand(`{"nodes": {"` + nodeMgr1 + `": {"enabled": false}}}`); !slices.Equal(refsOf(got.Items), []string{"web-x1", "web-x2"}) || len(got.NotRemoved) != 2 {
		t.Fatalf("a candidate that switches mgr-1 off: items %v notRemoved %d, want only the worker's two and mgr-1's two protected", refsOf(got.Items), len(got.NotRemoved))
	}
	if got := cand(`{"nodes": {"` + nodeWrk1 + `": {"keepN": 0}}}`); !slices.Equal(sortedStrings(refsOf(got.Items)), []string{"m-x1", "m-x2", "web-x1", "web-x2", "web-x3", "web-x4"}) {
		t.Fatalf("a candidate that makes the worker keep none: items %v, want all four of the worker's and mgr-1's two", refsOf(got.Items))
	}
	if got := cand(`{"nodes": {"` + nodeWrk1 + `": {"keepN": 20}}}`); !slices.Equal(refsOf(got.Items), []string{"m-x1", "m-x2"}) {
		t.Fatalf("a candidate that makes the worker keep 20: items %v, want only mgr-1's two", refsOf(got.Items))
	}
	// A candidate beside a candidate policy.
	if got := cand(`{"policy": {"keepN": 0}, "nodes": {"` + nodeWrk1 + `": {"enabled": false}}}`); !slices.Equal(sortedStrings(refsOf(got.Items)), []string{"m-x1", "m-x2", "m-x3", "m-x4"}) {
		t.Fatalf("a candidate policy that keeps none beside the worker switched off: items %v, want all four of mgr-1's", refsOf(got.Items))
	}
	if n := s.storedNodeRows(t); n != 0 {
		t.Fatalf("%d node rows stored after previews, want none: a preview saves nothing", n)
	}
	if rows := s.nodeAudits(t); len(rows) != 0 {
		t.Fatalf("audit rows = %+v after previews, want none", rows)
	}

	// A candidate replaces what the node has saved: saved off, candidate saying nothing, follows the environment.
	if rec := s.putNode(t, nodeMgr1, `{"enabled": false}`); rec.code != http.StatusOK {
		t.Fatalf("PUT node: %d %s", rec.code, rec.body)
	}
	if got := cand(`{}`); !slices.Equal(refsOf(got.Items), []string{"web-x1", "web-x2"}) {
		t.Fatalf("a preview with no candidate: items %v, want the saved setting kept (mgr-1 off)", refsOf(got.Items))
	}
	if got := cand(`{"nodes": {"` + nodeMgr1 + `": {}}}`); !slices.Equal(sortedStrings(refsOf(got.Items)), []string{"m-x1", "m-x2", "web-x1", "web-x2"}) {
		t.Fatalf("a candidate with nothing set for mgr-1: items %v, want it following the environment again", refsOf(got.Items))
	}
	// A value the save would refuse is refused.
	if rec, _ := s.previewWith(t, `{"nodes": {"`+nodeWrk1+`": {"keepN": 21}}}`); rec.code != http.StatusUnprocessableEntity {
		t.Fatalf("preview with keepN 21: %d %s, want 422", rec.code, rec.body)
	}
}

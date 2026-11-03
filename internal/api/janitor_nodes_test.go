// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/tierplan/proto/wire"
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

// policyNodes reads the nodes of the environment's policy.
func (s *swarmFix) policyNodes(t *testing.T) []janitorNodeRow {
	t.Helper()
	rec := s.req(t, http.MethodGet, "/api/env/"+s.env.ID+"/janitor/policy", "")
	if rec.code != http.StatusOK {
		t.Fatalf("policy: %d %s", rec.code, rec.body)
	}
	return decode[janitorNodesAnswer](t, rec).Nodes
}

func nodeRowOf(t *testing.T, rows []janitorNodeRow, id string) janitorNodeRow {
	t.Helper()
	for _, r := range rows {
		if r.NodeID == id {
			return r
		}
	}
	t.Fatalf("no node %s in %+v", id, rows)
	return janitorNodeRow{}
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

// A standalone host has no nodes to set.
func TestJanitorNodeNotSwarm(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine())
	awaitOnline(t, f.mon, e.ID)
	path := "/api/env/" + e.ID + "/janitor/nodes/node-1"
	if rec := f.req(t, http.MethodPut, path, `{"enabled": false}`); rec.code != http.StatusConflict || rec.ptyp != "not_swarm" {
		t.Fatalf("PUT on a standalone host: %d %s (%s), want 409 not_swarm", rec.code, rec.body, rec.ptyp)
	}
	if rec := f.req(t, http.MethodDelete, path, ""); rec.code != http.StatusConflict || rec.ptyp != "not_swarm" {
		t.Fatalf("DELETE on a standalone host: %d %s (%s), want 409 not_swarm", rec.code, rec.body, rec.ptyp)
	}
	if pol := decode[janitorNodesAnswer](t, f.req(t, http.MethodGet, "/api/env/"+e.ID+"/janitor/policy", "")); pol.Nodes == nil || len(pol.Nodes) != 0 {
		t.Fatalf("a standalone host's nodes = %v, want an empty list and not null", pol.Nodes)
	}
}

// A node ID the swarm does not have is 404 node_not_found, and nothing is written.
func TestJanitorNodeUnknown(t *testing.T) {
	t.Parallel()
	s := nodeSwarm(t, nil)
	for _, id := range []string{"node-nobody", "node-mgr-1x", "NODE-MGR-1"} {
		if rec := s.putNode(t, id, `{"enabled": false}`); rec.code != http.StatusNotFound || rec.ptyp != "node_not_found" {
			t.Errorf("PUT %s: %d %s (%s), want 404 node_not_found", id, rec.code, rec.body, rec.ptyp)
		}
		if rec := s.resetNode(t, id); rec.code != http.StatusNotFound || rec.ptyp != "node_not_found" {
			t.Errorf("DELETE %s: %d %s (%s), want 404 node_not_found", id, rec.code, rec.body, rec.ptyp)
		}
	}
	if n := s.storedNodeRows(t); n != 0 {
		t.Fatalf("%d node rows stored, want none", n)
	}
	if rows := s.nodeAudits(t); len(rows) != 0 {
		t.Fatalf("audit rows = %+v, want none", rows)
	}
}

// keepN is 0 to 20: the schema refuses anything else before the route runs.
func TestJanitorNodeRange(t *testing.T) {
	t.Parallel()
	s := nodeSwarm(t, nil)
	for _, body := range []string{`{"keepN": 21}`, `{"keepN": -1}`, `{"keepN": "many"}`, `{"enabled": "yes"}`} {
		if rec := s.putNode(t, nodeWrk1, body); rec.code != http.StatusUnprocessableEntity {
			t.Errorf("PUT %s: %d %s, want 422", body, rec.code, rec.body)
		}
	}
	for _, keep := range []int{0, 20} {
		if rec := s.putNode(t, nodeWrk1, fmt.Sprintf(`{"keepN": %d}`, keep)); rec.code != http.StatusOK {
			t.Errorf("PUT keepN %d: %d %s, want 200", keep, rec.code, rec.body)
		}
	}
	if row := nodeRowOf(t, s.policyNodes(t), nodeWrk1); !row.KeepN.Overridden || row.KeepN.Value != 20 {
		t.Errorf("worker = %+v, want the last accepted keep-N, 20", row)
	}
	if rows := s.nodeAudits(t); len(rows) != 2 {
		t.Errorf("audit rows = %+v, want one for each accepted save and none for the refused", rows)
	}
}

// Changing a node needs janitor.policy: an Operator without the extra and a Viewer get 403 and nothing is written.
func TestJanitorNodeNeedsExtra(t *testing.T) {
	t.Parallel()
	s := nodeSwarm(t, nil)
	path := s.nodePath(nodeWrk1)
	as := func(user, role string, extra bool) (cookie, csrf string) {
		s.addUser(t, user, false)
		s.addEnvRole(t, user, s.env.ID, role)
		if extra {
			if _, err := s.st.Write.Exec(`INSERT INTO assignment_extras (assignment_id, permission) VALUES (?, 'janitor.policy')`, "ra-"+user+"-"+s.env.ID); err != nil {
				t.Fatal(err)
			}
		}
		return s.signIn(t, user, false)
	}
	put := func(cookie, csrf string) *httptest.ResponseRecorder {
		return s.call(t, authReq{method: http.MethodPut, path: path, body: `{"enabled": false}`, cookie: cookie, csrf: csrf})
	}
	del := func(cookie, csrf string) *httptest.ResponseRecorder {
		return s.call(t, authReq{method: http.MethodDelete, path: path, cookie: cookie, csrf: csrf})
	}
	oc, ocsrf := as("olive", "operator", false)
	vc, vcsrf := as("vera", "viewer", false)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"an operator without the extra, PUT": put(oc, ocsrf), "an operator without the extra, DELETE": del(oc, ocsrf),
		"a viewer, PUT": put(vc, vcsrf), "a viewer, DELETE": del(vc, vcsrf),
	} {
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", name, rec.Code, rec.Body)
		}
	}
	if rec := s.call(t, authReq{method: http.MethodPut, path: path, body: `{"enabled": false}`}); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d, want 401", rec.Code)
	}
	if n := s.storedNodeRows(t); n != 0 {
		t.Fatalf("%d node rows after the refusals, want none", n)
	}
	// Reading the policy, the nodes with it, needs only the environment's read access.
	if rec := s.call(t, authReq{method: http.MethodGet, path: "/api/env/" + s.env.ID + "/janitor/policy", cookie: vc}); rec.Code != http.StatusOK {
		t.Errorf("a viewer reading the policy: %d %s, want 200", rec.Code, rec.Body)
	}
	tc, tcsrf := as("trent", "operator", true)
	if rec := put(tc, tcsrf); rec.Code != http.StatusOK {
		t.Fatalf("an operator with janitor.policy: %d %s, want 200", rec.Code, rec.Body)
	}
	if row := nodeRowOf(t, s.policyNodes(t), nodeWrk1); !row.Enabled.Overridden || row.Enabled.Value {
		t.Errorf("worker = %+v, want it switched off", row)
	}
	var actor string
	if err := s.st.Read.QueryRow(`SELECT actor_id FROM audit_log WHERE action = 'janitor.node_policy_changed'`).Scan(&actor); err != nil || actor != "id-trent" {
		t.Errorf("actor = %q (%v), want the person who saved, id-trent", actor, err)
	}
	if rec := del(tc, tcsrf); rec.Code != http.StatusOK {
		t.Errorf("the same operator resetting: %d %s, want 200", rec.Code, rec.Body)
	}
}

// Resetting a node that follows the environment, and saving what is stored, write nothing and audit nothing.
func TestJanitorNodeResetNoOp(t *testing.T) {
	t.Parallel()
	s := nodeSwarm(t, nil)
	rec := s.resetNode(t, nodeWrk1)
	if rec.code != http.StatusOK {
		t.Fatalf("DELETE a node that follows: %d %s, want 200", rec.code, rec.body)
	}
	var row janitorNodeRow
	if err := json.Unmarshal(rec.body, &row); err != nil || row.NodeID != nodeWrk1 || row.Enabled.Overridden || row.KeepN.Overridden || !row.Enabled.Value || row.KeepN.Value != 2 {
		t.Fatalf("answer = %s (%v), want the worker following the environment", rec.body, err)
	}
	if n := s.storedNodeRows(t); n != 0 {
		t.Fatalf("%d node rows, want none", n)
	}
	if rows := s.nodeAudits(t); len(rows) != 0 {
		t.Fatalf("audit rows = %+v after a reset of nothing, want none", rows)
	}
	for range 2 {
		if rec := s.putNode(t, nodeWrk1, `{"keepN": 5}`); rec.code != http.StatusOK {
			t.Fatalf("PUT: %d %s", rec.code, rec.body)
		}
	}
	if rows := s.nodeAudits(t); len(rows) != 1 || rows[0].Details["fields"] != "keepN" || rows[0].Details["node"] != "wrk-1" {
		t.Fatalf("audit rows = %+v after the same save twice, want one", rows)
	}
	// Nothing set at all is a reset, and it is audited once, naming what the node had set.
	if rec := s.putNode(t, nodeWrk1, `{"enabled": null, "keepN": null}`); rec.code != http.StatusOK {
		t.Fatalf("PUT nulls: %d %s", rec.code, rec.body)
	}
	if rec := s.resetNode(t, nodeWrk1); rec.code != http.StatusOK {
		t.Fatalf("DELETE: %d %s", rec.code, rec.body)
	}
	rows := s.nodeAudits(t)
	if len(rows) != 2 || rows[1].Details["fields"] != "keepN" || s.storedNodeRows(t) != 0 {
		t.Fatalf("audit rows = %+v rows stored %d, want the reset audited once and no row left", rows, s.storedNodeRows(t))
	}
}

// RESEARCH F12: settings are kept under the swarm node ID, so they follow the node when its agent is enrolled
// again as a new agent.
func TestJanitorNodeSurvivesReenrol(t *testing.T) {
	t.Parallel()
	s := nodeSwarm(t, map[string][]taskBox{"wrk-1": exitedSlot(4)})
	if rec := s.putNode(t, nodeWrk1, `{"enabled": false}`); rec.code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.code, rec.body)
	}
	_, first := s.previewJanitor(t)
	if len(first.Items) != 0 || len(first.NotRemoved) != 2 || first.NotRemoved[0].NodeID != s.w.id {
		t.Fatalf("before: items %v notRemoved %+v, want the worker's two protected under its agent %s", refsOf(first.Items), first.NotRemoved, s.w.id)
	}

	s.kickWorker(t)
	cfg := dockertest.FakeConfig{Hostname: "wrk-1", SwarmState: "active"}
	for _, b := range exitedSlot(4) {
		cfg.Containers = append(cfg.Containers, b.fake())
	}
	facts := &wire.Facts{EngineID: "ENG:wrk-1", Hostname: "wrk-1", EngineReachable: true, Swarm: &wire.SwarmFacts{State: "active", NodeID: nodeWrk1}}
	again := s.connectAs(t, s.env, cfg, facts, tunneltest.Agent{})
	awaitFirstProbe(t, s.mon, envs.AgentTarget(again.id))
	if again.id == s.w.id {
		t.Fatalf("the worker's agent kept its ID %s: this is not a re-enrolment", again.id)
	}

	_, after := s.previewJanitor(t)
	var onNew []janitorItem
	for _, it := range after.NotRemoved {
		if it.NodeID == again.id {
			onNew = append(onNew, it)
		}
	}
	if len(onNew) != 2 {
		t.Fatalf("notRemoved on the new agent = %+v (all %+v), want the worker's two", onNew, after.NotRemoved)
	}
	for _, it := range onNew {
		if it.Class != "protected" || it.Reason.Code != "node_disabled" {
			t.Errorf("%+v, want protected node_disabled: the setting followed the node to its new agent", it)
		}
	}
	for _, it := range after.Items {
		if it.NodeID == again.id {
			t.Errorf("%+v is removable on the re-enrolled node, want its setting kept", it)
		}
	}
	if row := nodeRowOf(t, s.policyNodes(t), nodeWrk1); !row.Enabled.Overridden || row.Enabled.Value || !row.Reporting {
		t.Errorf("worker = %+v, want it reporting through the new agent with its setting kept", row)
	}
}

// The nodes of the policy: every node the swarm lists, by name, with whether an agent reports it and where each
// setting comes from. A node nothing reaches is listed as not reporting, and can still be given settings.
func TestJanitorNodeNotReporting(t *testing.T) {
	t.Parallel()
	s := nodeSwarm(t, nil)
	rows := s.policyNodes(t)
	var names []string
	for _, r := range rows {
		names = append(names, r.Name)
	}
	if !slices.Equal(names, []string{"ghost-1", "mgr-1", "mgr-2", "wrk-1"}) {
		t.Fatalf("nodes = %v, want every node the swarm lists, by name", names)
	}
	ghost := nodeRowOf(t, rows, nodeGhost)
	if ghost.Reporting || !ghost.Enabled.Value || ghost.Enabled.Overridden || ghost.KeepN.Value != 2 || ghost.KeepN.Overridden {
		t.Errorf("the down node = %+v, want it not reporting and following the environment", ghost)
	}
	for _, id := range []string{nodeMgr1, nodeMgr2, nodeWrk1} {
		if r := nodeRowOf(t, rows, id); !r.Reporting {
			t.Errorf("%+v, want it reporting: an agent is connected on it", r)
		}
	}
	if rec := s.putNode(t, nodeGhost, `{"keepN": 7}`); rec.code != http.StatusOK {
		t.Fatalf("PUT the down node: %d %s, want 200: the swarm has it", rec.code, rec.body)
	}
	s.kickWorker(t)
	rows = s.policyNodes(t)
	if w := nodeRowOf(t, rows, nodeWrk1); w.Reporting {
		t.Errorf("worker = %+v, want it not reporting once its agent is gone", w)
	}
	if g := nodeRowOf(t, rows, nodeGhost); g.Reporting || !g.KeepN.Overridden || g.KeepN.Value != 7 {
		t.Errorf("the down node = %+v, want its own keep-N of 7", g)
	}
	// What the environment follows reaches a node that follows it.
	if rec := s.req(t, http.MethodPut, "/api/env/"+s.env.ID+"/janitor/policy", policyJSON(0, map[string]any{"keepN": 4, "enabled": false})); rec.code != http.StatusOK {
		t.Fatalf("PUT policy: %d %s", rec.code, rec.body)
	}
	rows = s.policyNodes(t)
	if m := nodeRowOf(t, rows, nodeMgr2); m.KeepN.Value != 4 || m.KeepN.Overridden || m.Enabled.Value || m.Enabled.Overridden {
		t.Errorf("mgr-2 = %+v, want it to follow the environment's keep 4 and off", m)
	}
	if g := nodeRowOf(t, rows, nodeGhost); g.KeepN.Value != 7 || !g.KeepN.Overridden {
		t.Errorf("the down node = %+v, want its own keep-N kept", g)
	}
}

// A node that sets itself on is still cleaned by a run while the environment's schedule is off, and the
// environment being off never protects a node: Clean now cleans every node that was not switched off.
func TestJanitorNodeEnvironmentOffStillCleans(t *testing.T) {
	t.Parallel()
	s := nodeSwarm(t, twoNodeBoxes())
	if rec := s.req(t, http.MethodPut, "/api/env/"+s.env.ID+"/janitor/policy", policyJSON(0, map[string]any{"enabled": false})); rec.code != http.StatusOK {
		t.Fatalf("PUT policy: %d %s", rec.code, rec.body)
	}
	if rec := s.putNode(t, nodeMgr1, `{"enabled": true}`); rec.code != http.StatusOK {
		t.Fatalf("PUT node: %d %s", rec.code, rec.body)
	}
	_, got := s.previewJanitor(t)
	if want := []string{"m-x1", "m-x2", "web-x1", "web-x2"}; !slices.Equal(sortedStrings(refsOf(got.Items)), want) {
		t.Fatalf("items = %v, want every node cleaned: the schedule being off is not a node being switched off", refsOf(got.Items))
	}
}

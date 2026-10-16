// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/bayplan/proto/wire"
)

// Drain, pause, activate, promote and demote (plan 06-15, RES-05, D-20 to D-23). Docker is a fake
// daemon behind the real monitor, aggregator and cluster reader; the router, policy, CSRF guard and
// audit writer are the real ones. The impact a dialog shows is computed by the same code the write
// runs, so these tests read the impact and then make the change.

type niTaskBody struct {
	ID        string `json:"id"`
	ServiceID string `json:"serviceId"`
	Service   string `json:"service"`
	Slot      int    `json:"slot"`
	Outcome   string `json:"outcome"`
}

type niBody struct {
	Action   string       `json:"action"`
	Hostname string       `json:"hostname"`
	Version  uint64       `json:"version"`
	Tasks    []niTaskBody `json:"tasks"`
	Managers struct {
		Total     int `json:"total"`
		Reachable int `json:"reachable"`
		After     int `json:"after"`
	} `json:"managers"`
	Warnings     []string `json:"warnings"`
	NeedsConfirm bool     `json:"needsConfirm"`
}

// naNodes is a swarm of three managers (the local socket is node-1, the leader) and three workers.
func naNodes() []dockertest.FakeNode {
	return []dockertest.FakeNode{
		{ID: "node-1", Hostname: "mgr-1", Role: "manager", Leader: true, Address: "10.0.0.1", Version: 7, OS: "linux"},
		{ID: "node-2", Hostname: "mgr-2", Role: "manager", Address: "10.0.0.2", Version: 5, OS: "linux"},
		{ID: "node-3", Hostname: "mgr-3", Role: "manager", Address: "10.0.0.3", Version: 6, OS: "linux"},
		{ID: "node-4", Hostname: "wrk-1", Address: "10.0.0.4", Version: 3, OS: "linux", Labels: map[string]string{"disk": "ssd", "zone": "a"}},
		{ID: "node-5", Hostname: "wrk-2", Address: "10.0.0.5", Version: 4, OS: "linux"},
		{ID: "node-6", Hostname: "wrk-3", Address: "10.0.0.6", Version: 2, OS: "linux", Availability: "pause"},
	}
}

func naServices() []dockertest.FakeService {
	return []dockertest.FakeService{
		{ID: "svc-web", Name: "web", Mode: "replicated", Running: 2, Desired: 2, Constraints: []string{"node.role==worker"}},
		{ID: "svc-agent", Name: "agent", Mode: "global", Running: 3, Desired: 3},
		{ID: "svc-db", Name: "db", Mode: "replicated", Running: 1, Desired: 1, Constraints: []string{"node.labels.disk==ssd"}},
	}
}

// naEnv is a local environment whose socket is node-1 of the swarm above.
func naEnv(t *testing.T, tune func(*dockertest.FakeConfig)) (*envFix, envBody) {
	t.Helper()
	cfg := swarmManagerEngine(naServices()...)
	cfg.SwarmNodeID, cfg.SwarmClusterID = "node-1", "cluster-1"
	cfg.NodeRows = naNodes()
	cfg.Tasks = []swarm.Task{
		tkRunning("web-1", "svc-web", "node-4", 1, "c1", time.Minute),
		tkRunning("db-1", "svc-db", "node-4", 1, "c2", time.Minute),
		tkRunning("agent-1", "svc-agent", "node-4", 0, "c3", time.Minute),
		tkRunning("web-2", "svc-web", "node-5", 2, "c4", time.Minute),
	}
	if tune != nil {
		tune(&cfg)
	}
	f := newEnvFix(t, cfg)
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	return f, e
}

func (f *envFix) nodeImpact(t *testing.T, e envBody, node, action string) *envRec {
	t.Helper()
	return f.get(t, e, "nodes/"+node+"/impact?action="+action)
}

func (f *envFix) setAvailability(t *testing.T, e envBody, node, body string) *envRec {
	t.Helper()
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/nodes/"+node+"/availability", body)
}

func (f *envFix) setRole(t *testing.T, e envBody, node, body string) *envRec {
	t.Helper()
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/nodes/"+node+"/role", body)
}

func (f *envFix) nodeAudits(t *testing.T) []auditRowFull {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT action, target_type, target_id, COALESCE(env_id, ''), result, COALESCE(reason, ''), COALESCE(details, '{}')
		FROM audit_log WHERE action LIKE 'node.%' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditRowFull
	for rows.Next() {
		var r auditRowFull
		var details string
		if err := rows.Scan(&r.Action, &r.TargetType, &r.TargetID, &r.EnvID, &r.Result, &r.Reason, &details); err != nil {
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

func niOf(t *testing.T, rec *envRec) niBody {
	t.Helper()
	if rec.code != http.StatusOK {
		t.Fatalf("impact: %d %s", rec.code, rec.body)
	}
	return decode[niBody](t, rec)
}

func TestNodeImpactOfADrainClassifiesTheNodesTasks(t *testing.T) {
	f, e := naEnv(t, nil)
	b := niOf(t, f.nodeImpact(t, e, "node-4", "drain"))
	if b.Action != "drain" || b.Hostname != "wrk-1" || b.NeedsConfirm || len(b.Warnings) != 0 {
		t.Fatalf("body = %+v, want a plain drain of wrk-1", b)
	}
	if b.Version != 3 {
		t.Errorf("version = %d, want the node's own, 3: the version a change sent on the strength of this must carry", b.Version)
	}
	got := map[string]string{}
	for _, tk := range b.Tasks {
		got[tk.ID] = tk.Outcome
	}
	// web may go to wrk-2, the agent is global, and nothing but wrk-1 has the ssd db wants.
	want := map[string]string{"web-1": "moves", "agent-1": "stops", "db-1": "cannot_place"}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("task %s: %q, want %q (%+v)", id, got[id], w, b.Tasks)
		}
	}
	if len(b.Tasks) != 3 {
		t.Errorf("tasks = %+v, want exactly the node's three", b.Tasks)
	}
	for _, tk := range b.Tasks {
		if tk.ID == "web-1" && (tk.Service != "web" || tk.ServiceID != "svc-web" || tk.Slot != 1) {
			t.Errorf("web-1 = %+v", tk)
		}
	}
	if b.Managers.Total != 3 || b.Managers.Reachable != 3 || b.Managers.After != 3 {
		t.Errorf("managers = %+v, want 3 of 3 and 3 after", b.Managers)
	}
	if empty := niOf(t, f.nodeImpact(t, e, "node-6", "drain")); empty.Tasks == nil || len(empty.Tasks) != 0 {
		t.Errorf("a node with no tasks: %+v, want an empty list and never null", empty.Tasks)
	}
}

func TestNodeImpactOfADemoteOfBayplansNodeNeedsTheNameAndWarnsOfAnEvenCount(t *testing.T) {
	f, e := naEnv(t, nil)
	b := niOf(t, f.nodeImpact(t, e, "node-1", "demote"))
	if !b.NeedsConfirm || !slices.Contains(b.Warnings, "runs_bayplan") || !slices.Contains(b.Warnings, "even_count") || slices.Contains(b.Warnings, "quorum_at_risk") {
		t.Errorf("body = %+v, want runs_bayplan and even_count and the typed confirm", b)
	}
	if b.Managers.After != 2 {
		t.Errorf("managers = %+v, want 2 after", b.Managers)
	}
	// Another manager, all reachable: an even count warns only.
	b = niOf(t, f.nodeImpact(t, e, "node-2", "demote"))
	if b.NeedsConfirm || !slices.Equal(b.Warnings, []string{"even_count"}) {
		t.Errorf("body = %+v, want only the even-count warning", b)
	}
}

func TestNodeImpactRefusals(t *testing.T) {
	f, e := naEnv(t, nil)
	for _, action := range []string{"", "remove", "activate", "Drain"} {
		if rec := f.nodeImpact(t, e, "node-4", action); rec.code != http.StatusUnprocessableEntity {
			t.Errorf("action %q = %d %s, want 422", action, rec.code, rec.body)
		}
	}
	if rec := f.nodeImpact(t, e, "ghost", "drain"); rec.code != http.StatusNotFound || rec.ptyp != "node_not_found" {
		t.Errorf("an unknown node = %d %s (%s), want 404 node_not_found", rec.code, rec.body, rec.ptyp)
	}
	get := f.asRole(t, e, "na-operator", "operator")
	if rec := get("nodes/node-4/impact?action=drain"); rec.Code != http.StatusForbidden {
		t.Errorf("an Operator's impact = %d %s, want 403: node changes need Admin", rec.Code, rec.Body)
	}

	sf := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0"})
	sf.clusters = envs.NewClusters(sf.svc, sf.snaps, envs.ClusterOptions{Now: sf.clk.now})
	sf.svc.WithClusters(sf.clusters)
	sf.h = sf.build(t)
	se := sf.add(t)
	awaitOnline(t, sf.mon, se.ID)
	if rec := sf.nodeImpact(t, se, "node-1", "drain"); rec.code != http.StatusConflict || rec.ptyp != "not_a_swarm" {
		t.Errorf("a standalone host = %d %s (%s), want 409 not_a_swarm", rec.code, rec.body, rec.ptyp)
	}
}

func TestSetNodeAvailabilityDrainsWithTheFullSpecAndTheVersionJustRead(t *testing.T) {
	f, e := naEnv(t, nil)
	rec := f.setAvailability(t, e, "node-4", `{"availability":"drain","version":3}`)
	if rec.code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	ups := f.fake.NodeUpdates()
	if len(ups) != 1 {
		t.Fatalf("updates = %+v, want exactly one", ups)
	}
	u := ups[0]
	if u.NodeID != "node-4" || u.Version != "3" || u.Spec.Availability != "drain" || u.Spec.Role != "worker" ||
		u.Spec.Labels["disk"] != "ssd" || u.Spec.Labels["zone"] != "a" {
		t.Errorf("update = %+v, want the whole spec with availability drain at version 3", u)
	}

	rows := f.nodeAudits(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want one", rows)
	}
	r := rows[0]
	if r.Action != audit.ActionNodeAvailabilityChanged || r.Result != string(audit.ResultSuccess) || r.TargetType != "node" || r.TargetID != "node-4" || r.EnvID != e.ID {
		t.Errorf("audit row = %+v", r)
	}
	for k, want := range map[string]string{"name": "wrk-1", "from": "active", "to": "drain", "confirmed": "false", "code": "done", "warnings": ""} {
		if got, ok := r.Details[k]; !ok || got != want {
			t.Errorf("detail %s = %q (present %v), want %q", k, got, ok, want)
		}
	}

	// The list shows it at once, and the version moved on: the same request again is stale.
	list := decode[ndList](t, f.get(t, e, "nodes"))
	for _, n := range list.Items {
		if n.ID == "node-4" && n.Availability != "drain" {
			t.Errorf("node-4 reads %q after the drain, want drain", n.Availability)
		}
	}
	again := f.setAvailability(t, e, "node-4", `{"availability":"drain","version":3}`)
	if again.code != http.StatusConflict || again.ptyp != "node_changed" {
		t.Errorf("the same version again = %d %s (%s), want 409 node_changed", again.code, again.body, again.ptyp)
	}
}

func TestSetNodeAvailabilityPauseAndActivateNeedNoName(t *testing.T) {
	f, e := naEnv(t, nil)
	// Pausing the node Bayplan runs on sends nothing away, so it needs no name.
	if rec := f.setAvailability(t, e, "node-1", `{"availability":"pause","version":7}`); rec.code != http.StatusNoContent {
		t.Fatalf("pause = %d: %s", rec.code, rec.body)
	}
	if rec := f.setAvailability(t, e, "node-6", `{"availability":"active","version":2}`); rec.code != http.StatusNoContent {
		t.Fatalf("activate = %d: %s", rec.code, rec.body)
	}
	ups := f.fake.NodeUpdates()
	if len(ups) != 2 || ups[0].Spec.Availability != "pause" || ups[1].Spec.Availability != "active" {
		t.Fatalf("updates = %+v", ups)
	}
	rows := f.nodeAudits(t)
	if len(rows) != 2 || rows[1].Details["from"] != "pause" || rows[1].Details["to"] != "active" || rows[0].Details["from"] != "active" || rows[0].Details["to"] != "pause" {
		t.Errorf("audit rows = %+v, want pause then activate with their from and to", rows)
	}
}

func TestSetNodeAvailabilityOfBayplansNodeNeedsItsNameAndThenIsNotBlocked(t *testing.T) {
	f, e := naEnv(t, nil)
	for _, confirm := range []string{"", "mgr-2", "MGR-1", "mgr-1 "} {
		body := fmt.Sprintf(`{"availability":"drain","version":7,"confirm":%q}`, confirm)
		rec := f.setAvailability(t, e, "node-1", body)
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "confirmation_required" {
			t.Errorf("confirm %q = %d %s (%s), want 422 confirmation_required", confirm, rec.code, rec.body, rec.ptyp)
		}
	}
	if ups := f.fake.NodeUpdates(); len(ups) != 0 {
		t.Fatalf("updates = %+v, want none before the name is typed", ups)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v, want none", w)
	}
	rows := f.nodeAudits(t)
	if len(rows) != 4 {
		t.Fatalf("audit rows = %+v, want one failure per refused attempt", rows)
	}
	for _, r := range rows {
		if r.Result != string(audit.ResultFailure) || r.Reason != "confirmation_required" || r.Details["confirmed"] != "false" ||
			r.Details["code"] != "confirmation_required" || !strings.Contains(r.Details["warnings"], "runs_bayplan") {
			t.Errorf("row = %+v, want a failure confirmation_required carrying runs_bayplan", r)
		}
	}

	// Bayplan never hard-blocks a change an Admin confirmed.
	rec := f.setAvailability(t, e, "node-1", `{"availability":"drain","version":7,"confirm":"mgr-1"}`)
	if rec.code != http.StatusNoContent {
		t.Fatalf("with its name: %d %s, want 204", rec.code, rec.body)
	}
	if ups := f.fake.NodeUpdates(); len(ups) != 1 || ups[0].Spec.Availability != "drain" {
		t.Errorf("updates = %+v, want the drain sent", ups)
	}
	last := f.nodeAudits(t)[4]
	if last.Result != string(audit.ResultSuccess) || last.Details["confirmed"] != "true" || last.Details["code"] != "done" || !strings.Contains(last.Details["warnings"], "runs_bayplan") {
		t.Errorf("last row = %+v, want a success with confirmed true and the warning", last)
	}
}

func TestSetNodeAvailabilityRefusals(t *testing.T) {
	f, e := naEnv(t, nil)
	for name, tc := range map[string]struct {
		node, body, ptyp string
		code             int
	}{
		"a stale version":         {"node-4", `{"availability":"drain","version":2}`, "node_changed", http.StatusConflict},
		"an unknown node":         {"ghost", `{"availability":"drain","version":2}`, "node_not_found", http.StatusNotFound},
		"an availability of down": {"node-4", `{"availability":"down","version":3}`, "", http.StatusUnprocessableEntity},
		"no availability":         {"node-4", `{"version":3}`, "", http.StatusUnprocessableEntity},
		"no version":              {"node-4", `{"availability":"drain"}`, "", http.StatusUnprocessableEntity},
		"an unknown field":        {"node-4", `{"availability":"drain","version":3,"role":"manager"}`, "", http.StatusUnprocessableEntity},
	} {
		rec := f.setAvailability(t, e, tc.node, tc.body)
		if rec.code != tc.code || (tc.ptyp != "" && rec.ptyp != tc.ptyp) {
			t.Errorf("%s = %d %s (%s), want %d %s", name, rec.code, rec.body, rec.ptyp, tc.code, tc.ptyp)
		}
	}
	if ups := f.fake.NodeUpdates(); len(ups) != 0 {
		t.Errorf("updates = %+v, want none: a stale version is never retried", ups)
	}
	rows := f.nodeAudits(t)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "node_changed" || rows[0].TargetID != "node-4" {
		t.Errorf("audit rows = %+v, want one failure node_changed, and none for a node that does not exist", rows)
	}
}

func TestSetNodeRolePromotesAndDemotes(t *testing.T) {
	f, e := naEnv(t, nil)
	if rec := f.setRole(t, e, "node-4", `{"role":"manager","version":3}`); rec.code != http.StatusNoContent {
		t.Fatalf("promote = %d: %s", rec.code, rec.body)
	}
	// Four managers now, all reachable: demoting one leaves three, which warns of nothing.
	if rec := f.setRole(t, e, "node-2", `{"role":"worker","version":5}`); rec.code != http.StatusNoContent {
		t.Fatalf("demote = %d: %s", rec.code, rec.body)
	}
	ups := f.fake.NodeUpdates()
	if len(ups) != 2 || ups[0].Spec.Role != "manager" || ups[0].Spec.Availability != "active" || ups[0].Spec.Labels["disk"] != "ssd" || ups[1].Spec.Role != "worker" {
		t.Fatalf("updates = %+v", ups)
	}
	rows := f.nodeAudits(t)
	if len(rows) != 2 || rows[0].Action != audit.ActionNodeRoleChanged || rows[0].Details["from"] != "worker" || rows[0].Details["to"] != "manager" ||
		rows[1].Details["from"] != "manager" || rows[1].Details["to"] != "worker" || rows[1].Details["confirmed"] != "false" {
		t.Errorf("audit rows = %+v", rows)
	}
}

func TestSetNodeRoleDemoteThatPutsQuorumAtRiskNeedsTheName(t *testing.T) {
	f, e := naEnv(t, func(c *dockertest.FakeConfig) {
		c.NodeRows[2].Reachability = "unreachable" // mgr-3 cannot be reached
	})
	b := niOf(t, f.nodeImpact(t, e, "node-2", "demote"))
	if !b.NeedsConfirm || !slices.Contains(b.Warnings, "quorum_at_risk") || b.Managers.Reachable != 2 {
		t.Fatalf("impact = %+v, want quorum_at_risk with 2 reachable", b)
	}
	rec := f.setRole(t, e, "node-2", `{"role":"worker","version":5}`)
	if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "confirmation_required" {
		t.Fatalf("without the name = %d %s (%s), want 422 confirmation_required", rec.code, rec.body, rec.ptyp)
	}
	if ups := f.fake.NodeUpdates(); len(ups) != 0 {
		t.Fatalf("updates = %+v, want none", ups)
	}
	rec = f.setRole(t, e, "node-2", `{"role":"worker","version":5,"confirm":"mgr-2"}`)
	if rec.code != http.StatusNoContent {
		t.Fatalf("with the name = %d %s, want 204: Bayplan warns, Docker alone refuses", rec.code, rec.body)
	}
	rows := f.nodeAudits(t)
	last := rows[len(rows)-1]
	if last.Details["confirmed"] != "true" || !strings.Contains(last.Details["warnings"], "quorum_at_risk") {
		t.Errorf("last row = %+v, want confirmed true and quorum_at_risk", last)
	}
}

func TestSetNodeRoleMapsDockersOwnRefusals(t *testing.T) {
	f, e := naEnv(t, func(c *dockertest.FakeConfig) {
		c.NodeUpdateError = "rpc error: code = FailedPrecondition desc = attempting to demote the last manager of the swarm"
	})
	rec := f.setRole(t, e, "node-2", `{"role":"worker","version":5}`)
	if rec.code != http.StatusConflict || rec.ptyp != "last_manager" {
		t.Fatalf("status = %d %s (%s), want 409 last_manager", rec.code, rec.body, rec.ptyp)
	}
	var p actionProblem
	if err := json.Unmarshal(rec.body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Detail != "You can't demote the last manager. Promote another node first." {
		t.Errorf("detail = %q", p.Detail)
	}
	rows := f.nodeAudits(t)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "last_manager" || rows[0].Details["code"] != "last_manager" {
		t.Errorf("audit rows = %+v, want one failure last_manager", rows)
	}
}

func TestSetNodeRoleKeepsAnyOtherRefusalsWords(t *testing.T) {
	f, e := naEnv(t, func(c *dockertest.FakeConfig) {
		c.NodeUpdateError = "rpc error: code = Unavailable desc = no leader elected"
	})
	rec := f.setRole(t, e, "node-4", `{"role":"manager","version":3}`)
	if rec.code != http.StatusBadGateway || rec.ptyp != "node_update_failed" || !strings.Contains(string(rec.body), "no leader elected") {
		t.Errorf("status = %d %s (%s), want 502 node_update_failed with Docker's words", rec.code, rec.body, rec.ptyp)
	}
}

func TestNodeActionsNeedAdmin(t *testing.T) {
	f, e := naEnv(t, nil)
	f.addUser(t, "na-operator", false)
	f.addEnvRole(t, "na-operator", e.ID, "operator")
	cookie, csrf := f.signIn(t, "na-operator", false)
	for _, path := range []string{"nodes/node-4/availability", "nodes/node-4/role"} {
		rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/" + path, body: `{"availability":"drain","role":"manager","version":3}`, cookie: cookie, csrf: csrf})
		if rec.Code != http.StatusForbidden {
			t.Errorf("an Operator's POST %s = %d: %s, want 403", path, rec.Code, rec.Body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
	if rows := f.nodeAudits(t); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none for a refused caller", rows)
	}

	f.addUser(t, "na-boss", false)
	f.addEnvRole(t, "na-boss", e.ID, "admin")
	cookie, csrf = f.signIn(t, "na-boss", false)
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/nodes/node-4/availability", body: `{"availability":"pause","version":3}`, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusNoContent {
		t.Errorf("an env Admin's pause = %d: %s, want 204", rec.Code, rec.Body)
	}
}

func TestNodeActionsOnAStandaloneHostAreNotASwarm(t *testing.T) {
	sf := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0"})
	sf.clusters = envs.NewClusters(sf.svc, sf.snaps, envs.ClusterOptions{Now: sf.clk.now})
	sf.svc.WithClusters(sf.clusters)
	sf.h = sf.build(t)
	se := sf.add(t)
	awaitOnline(t, sf.mon, se.ID)
	if rec := sf.setAvailability(t, se, "node-1", `{"availability":"drain","version":1}`); rec.code != http.StatusConflict || rec.ptyp != "not_a_swarm" {
		t.Errorf("availability = %d %s (%s), want 409 not_a_swarm", rec.code, rec.body, rec.ptyp)
	}
	if rec := sf.setRole(t, se, "node-1", `{"role":"worker","version":1}`); rec.code != http.StatusConflict || rec.ptyp != "not_a_swarm" {
		t.Errorf("role = %d %s (%s), want 409 not_a_swarm", rec.code, rec.body, rec.ptyp)
	}
}

// naAgentSwarm is a swarm of two managers and a worker whose nodes only some agents reach. Only
// the first manager has an agent connected, unless both is true.
func naAgentSwarm(t *testing.T, both bool) (*agentFix, tunneltest.Environment, *agentConn) {
	t.Helper()
	f := newAgentFix(t)
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.tclk.Now, Publisher: f.brk, Events: f.hub})
	f.svc.WithClusters(f.clusters)
	f.h = f.build(t)
	env := f.srv.NewEnvironment(t, "Swarm")
	rows := []dockertest.FakeNode{
		{ID: "node-mgr-1", Hostname: "mgr-1", Role: "manager", Leader: true, Address: "10.1.0.1", Version: 9},
		{ID: "node-mgr-2", Hostname: "mgr-2", Role: "manager", Address: "10.1.0.2", Version: 8},
		{ID: "node-wrk-1", Hostname: "wrk-1", Address: "10.1.0.3", Version: 4},
	}
	mgr := func(i int) (dockertest.FakeConfig, *wire.Facts) {
		name := fmt.Sprintf("mgr-%d", i)
		return dockertest.FakeConfig{Hostname: name, SwarmState: "active", SwarmControl: true, NodeRows: rows}, &wire.Facts{
			EngineID: "ENG:" + name, Hostname: name, EngineReachable: true,
			Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-" + name, ClusterID: "cluster-1", Control: true},
		}
	}
	cfg, facts := mgr(1)
	m1 := f.connect(t, env, cfg, facts)
	keys := []string{envs.AgentTarget(m1.id)}
	if both {
		cfg, facts = mgr(2)
		m2 := f.connect(t, env, cfg, facts)
		keys = append(keys, envs.AgentTarget(m2.id))
	}
	f.whenState(t, env.ID, "online")
	f.whenProbed(t, keys...)
	return f, env, m1
}

func TestNodeImpactWarnsWhenTheNodeHasTheOnlyConnectedManagerAgent(t *testing.T) {
	f, env, m1 := naAgentSwarm(t, false)
	// naAgentSwarm returned with the manager agent probed, so the impact is read once.
	rec := f.get(t, env.ID, "nodes/node-mgr-1/impact?action=drain")
	if rec.code != http.StatusOK {
		t.Fatalf("impact: %d %s", rec.code, rec.body)
	}
	b := decode[niBody](t, rec)
	if !slices.Contains(b.Warnings, "only_manager_agent") {
		t.Fatalf("impact = %+v, want it to name the only manager agent", b)
	}
	if !b.NeedsConfirm {
		t.Errorf("impact = %+v, want the typed confirm", b)
	}
	// The worker has no agent and no manager agent runs on it.
	if w := niOf(t, f.get(t, env.ID, "nodes/node-wrk-1/impact?action=drain")); w.NeedsConfirm || len(w.Warnings) != 0 {
		t.Errorf("worker impact = %+v, want no warning", w)
	}
	// The change is sent through that agent once the name is typed.
	if rec := f.req(t, http.MethodPost, "/api/env/"+env.ID+"/nodes/node-mgr-1/availability", `{"availability":"drain","version":9}`); rec.code != http.StatusUnprocessableEntity {
		t.Fatalf("without the name = %d %s, want 422", rec.code, rec.body)
	}
	if ups := m1.fake.NodeUpdates(); len(ups) != 0 {
		t.Fatalf("updates = %+v, want none before the name is typed", ups)
	}
	if rec := f.req(t, http.MethodPost, "/api/env/"+env.ID+"/nodes/node-mgr-1/availability", `{"availability":"drain","version":9,"confirm":"mgr-1"}`); rec.code != http.StatusNoContent {
		t.Fatalf("with the name = %d %s, want 204", rec.code, rec.body)
	}
	if ups := m1.fake.NodeUpdates(); len(ups) != 1 || ups[0].NodeID != "node-mgr-1" || ups[0].Spec.Availability != "drain" {
		t.Errorf("updates through the agent = %+v", ups)
	}
}

func TestNodeImpactDoesNotWarnWhenAnotherManagerAgentIsConnected(t *testing.T) {
	f, env, _ := naAgentSwarm(t, true)
	// naAgentSwarm returned with both manager agents probed, so the node list is read once.
	rec := f.get(t, env.ID, "nodes")
	if rec.code != http.StatusOK {
		t.Fatalf("nodes: %d %s", rec.code, rec.body)
	}
	reporting := 0
	for _, it := range decode[ndList](t, rec).Items {
		if it.AgentReporting {
			reporting++
		}
	}
	if reporting != 2 {
		t.Fatalf("%d nodes have a reporting agent, want both manager agents mapped: %s", reporting, rec.body)
	}
	for _, node := range []string{"node-mgr-1", "node-mgr-2"} {
		if b := niOf(t, f.get(t, env.ID, "nodes/"+node+"/impact?action=drain")); slices.Contains(b.Warnings, "only_manager_agent") || b.NeedsConfirm {
			t.Errorf("%s impact = %+v, want no only_manager_agent warning with two manager agents connected", node, b)
		}
	}
}

// Remove a node that is down (plan 06-17, RES-05, D-22): only a Down worker, by typing its name, never
// forced, and the agent of that swarm node is revoked without a block.

// nrEnv is naEnv with a worker that is down and a manager that is down.
func nrEnv(t *testing.T, tune func(*dockertest.FakeConfig)) (*envFix, envBody) {
	t.Helper()
	return naEnv(t, func(c *dockertest.FakeConfig) {
		c.NodeRows = append(c.NodeRows,
			dockertest.FakeNode{ID: "node-7", Hostname: "wrk-dead", Address: "10.0.0.7", Version: 9, OS: "linux", State: "down"},
			dockertest.FakeNode{ID: "node-8", Hostname: "mgr-dead", Role: "manager", Address: "10.0.0.8", Version: 8, OS: "linux", State: "down", Reachability: "unreachable"},
		)
		if tune != nil {
			tune(c)
		}
	})
}

func (f *envFix) removeNode(t *testing.T, e envBody, node, confirm string) *envRec {
	t.Helper()
	path := "/api/env/" + e.ID + "/nodes/" + node
	if confirm != "" {
		path += "?confirm=" + confirm
	}
	return f.req(t, http.MethodDelete, path, "")
}

func TestRemoveNode(t *testing.T) {
	f, e := nrEnv(t, nil)
	rec := f.removeNode(t, e, "node-7", "wrk-dead")
	if rec.code != http.StatusNoContent {
		t.Fatalf("status = %d %s, want 204", rec.code, rec.body)
	}
	if w := f.fake.Writes(); len(w) != 1 || w[0] != "DELETE /nodes/node-7" {
		t.Errorf("writes = %v, want exactly one DELETE of the node with no query: force is never sent", w)
	}
	if rec := f.get(t, e, "nodes/node-7"); rec.code != http.StatusNotFound || rec.ptyp != "node_not_found" {
		t.Errorf("the node after the remove = %d %s (%s), want 404 node_not_found", rec.code, rec.body, rec.ptyp)
	}
	rows := f.nodeAudits(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want one", rows)
	}
	r := rows[0]
	if r.Action != "node.removed" || r.TargetType != "node" || r.TargetID != "node-7" || r.EnvID != e.ID || r.Result != string(audit.ResultSuccess) ||
		r.Details["hostname"] != "wrk-dead" || r.Details["agent"] != "" || r.Details["code"] != "done" {
		t.Errorf("audit row = %+v, want a success naming the host and no agent", r)
	}
}

func TestRemoveNodeNeedsTheNodesNameTyped(t *testing.T) {
	f, e := nrEnv(t, nil)
	for _, confirm := range []string{"", "wrk-dea", "WRK-DEAD", "node-7"} {
		rec := f.removeNode(t, e, "node-7", confirm)
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "confirmation_required" {
			t.Errorf("confirm %q = %d %s (%s), want 422 confirmation_required", confirm, rec.code, rec.body, rec.ptyp)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none before the name is typed", w)
	}
	rows := f.nodeAudits(t)
	if len(rows) != 4 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "confirmation_required" || rows[0].Details["code"] != "confirmation_required" {
		t.Errorf("audit rows = %+v, want a failure row for each refused attempt", rows)
	}
}

func TestRemoveNodeRefusals(t *testing.T) {
	f, e := nrEnv(t, nil)
	for _, tc := range []struct {
		name, node string
		code       int
		ptyp       string
	}{
		{"a worker that is ready", "node-4", http.StatusConflict, "node_not_down"},
		{"a worker that is paused", "node-6", http.StatusConflict, "node_not_down"},
		{"a manager that is ready", "node-2", http.StatusConflict, "node_is_manager"},
		{"Bayplan's own manager", "node-1", http.StatusConflict, "node_is_manager"},
		{"a manager that is down", "node-8", http.StatusConflict, "node_is_manager"},
		{"a node the swarm does not have", "node-99", http.StatusNotFound, "node_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The name is typed correctly, so only the refusal can stop it.
			rec := f.removeNode(t, e, tc.node, "anything")
			if rec.code != tc.code || rec.ptyp != tc.ptyp {
				t.Errorf("status = %d %s (%s), want %d %s", rec.code, rec.body, rec.ptyp, tc.code, tc.ptyp)
			}
		})
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none: the server refuses before any write", w)
	}
	rows := f.nodeAudits(t)
	byNode := map[string]auditRowFull{}
	for _, r := range rows {
		byNode[r.TargetID] = r
	}
	if r := byNode["node-4"]; r.Action != "node.removed" || r.Result != string(audit.ResultFailure) || r.Reason != "node_not_down" || r.Details["hostname"] != "wrk-1" {
		t.Errorf("audit row of the ready worker = %+v, want a failure node_not_down", r)
	}
	if r := byNode["node-2"]; r.Reason != "node_is_manager" {
		t.Errorf("audit row of the manager = %+v, want a failure node_is_manager", r)
	}
	if _, ok := byNode["node-99"]; ok {
		t.Errorf("audit rows = %+v, want none for a node that does not exist", rows)
	}
}

func TestRemoveNodeMapsDockersOwnRefusals(t *testing.T) {
	// The node came back between the read and the write.
	f, e := nrEnv(t, func(c *dockertest.FakeConfig) {
		c.WriteError = "rpc error: code = FailedPrecondition desc = node node-7 is not down and can't be removed"
	})
	rec := f.removeNode(t, e, "node-7", "wrk-dead")
	if rec.code != http.StatusConflict || rec.ptyp != "node_not_down" {
		t.Fatalf("status = %d %s (%s), want 409 node_not_down", rec.code, rec.body, rec.ptyp)
	}
	rows := f.nodeAudits(t)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "node_not_down" {
		t.Errorf("audit rows = %+v, want one failure node_not_down", rows)
	}

	f, e = nrEnv(t, func(c *dockertest.FakeConfig) {
		c.WriteError = "rpc error: code = Unavailable desc = no leader elected"
	})
	rec = f.removeNode(t, e, "node-7", "wrk-dead")
	if rec.code != http.StatusBadGateway || rec.ptyp != "node_remove_failed" || !strings.Contains(string(rec.body), "no leader elected") {
		t.Errorf("status = %d %s (%s), want 502 node_remove_failed with Docker's words", rec.code, rec.body, rec.ptyp)
	}
}

func TestRemoveNodeNeedsAdmin(t *testing.T) {
	f, e := nrEnv(t, nil)
	f.addUser(t, "nr-operator", false)
	f.addEnvRole(t, "nr-operator", e.ID, "operator")
	cookie, csrf := f.signIn(t, "nr-operator", false)
	rec := f.call(t, authReq{method: http.MethodDelete, path: "/api/env/" + e.ID + "/nodes/node-7?confirm=wrk-dead", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusForbidden {
		t.Errorf("an Operator's DELETE = %d: %s, want 403", rec.Code, rec.Body)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
	if rows := f.nodeAudits(t); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none for a refused caller", rows)
	}

	f.addUser(t, "nr-boss", false)
	f.addEnvRole(t, "nr-boss", e.ID, "admin")
	cookie, csrf = f.signIn(t, "nr-boss", false)
	rec = f.call(t, authReq{method: http.MethodDelete, path: "/api/env/" + e.ID + "/nodes/node-7?confirm=wrk-dead", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusNoContent {
		t.Errorf("an env Admin's DELETE = %d: %s, want 204", rec.Code, rec.Body)
	}
}

func TestRemoveNodeOnAStandaloneHostIsNotASwarm(t *testing.T) {
	sf := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0"})
	sf.clusters = envs.NewClusters(sf.svc, sf.snaps, envs.ClusterOptions{Now: sf.clk.now})
	sf.svc.WithClusters(sf.clusters)
	sf.h = sf.build(t)
	se := sf.add(t)
	awaitOnline(t, sf.mon, se.ID)
	if rec := sf.removeNode(t, se, "node-1", "node-1"); rec.code != http.StatusConflict || rec.ptyp != "not_a_swarm" {
		t.Errorf("remove = %d %s (%s), want 409 not_a_swarm", rec.code, rec.body, rec.ptyp)
	}
}

// nrAgentSwarm is a swarm of a manager and a worker with an agent each, the worker's node down.
func nrAgentSwarm(t *testing.T) (*agentFix, tunneltest.Environment, *agentConn, *agentConn) {
	t.Helper()
	f := newAgentFix(t)
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.tclk.Now, Publisher: f.brk, Events: f.hub})
	f.svc.WithClusters(f.clusters)
	f.h = f.build(t)
	env := f.srv.NewEnvironment(t, "Swarm")
	rows := []dockertest.FakeNode{
		{ID: "node-mgr-1", Hostname: "mgr-1", Role: "manager", Leader: true, Address: "10.1.0.1", Version: 9},
		{ID: "node-wrk-1", Hostname: "wrk-1", Address: "10.1.0.3", Version: 4, State: "down"},
	}
	mgr := f.connect(t, env, dockertest.FakeConfig{Hostname: "mgr-1", SwarmState: "active", SwarmControl: true, NodeRows: rows}, &wire.Facts{
		EngineID: "ENG:mgr-1", Hostname: "mgr-1", EngineReachable: true,
		Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-mgr-1", ClusterID: "cluster-1", Control: true},
	})
	wrk := f.connect(t, env, dockertest.FakeConfig{Hostname: "wrk-1", SwarmState: "active"}, &wire.Facts{
		EngineID: "ENG:wrk-1", Hostname: "wrk-1", EngineReachable: true, Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-wrk-1"},
	})
	f.whenState(t, env.ID, "online")
	f.whenProbed(t, envs.AgentTarget(mgr.id), envs.AgentTarget(wrk.id))
	return f, env, mgr, wrk
}

func TestRemoveNodeRevokesTheAgentOfThatNodeWithoutBlockingTheHost(t *testing.T) {
	f, env, mgr, wrk := nrAgentSwarm(t)
	rec := f.req(t, http.MethodDelete, "/api/env/"+env.ID+"/nodes/node-wrk-1?confirm=wrk-1", "")
	if rec.code != http.StatusNoContent {
		t.Fatalf("status = %d %s, want 204", rec.code, rec.body)
	}
	if w := mgr.fake.Writes(); len(w) != 1 || w[0] != "DELETE /nodes/node-wrk-1" {
		t.Errorf("writes through the manager's agent = %v, want one DELETE of the node", w)
	}

	var status, reason, revokedBy string
	if err := f.st.Read.QueryRow(`SELECT status, COALESCE(revoke_reason, ''), COALESCE(revoked_by, '') FROM agents WHERE id = ?`, wrk.id).Scan(&status, &reason, &revokedBy); err != nil {
		t.Fatal(err)
	}
	if status != "revoked" || reason != "node_removed" || revokedBy == "" || revokedBy == "system:agent-reaper" {
		t.Errorf("the worker's agent = %s %q by %q, want revoked with node_removed by the admin", status, reason, revokedBy)
	}
	if err := f.st.Read.QueryRow(`SELECT status FROM agents WHERE id = ?`, mgr.id).Scan(&status); err != nil || status != "active" {
		t.Errorf("the manager's agent = %q, %v, want it left active", status, err)
	}
	var blocks int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM agent_blocks WHERE env_id = ?`, env.ID).Scan(&blocks); err != nil || blocks != 0 {
		t.Errorf("blocks = %d, %v, want none: the host must be able to rejoin", blocks, err)
	}

	var revoked int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'agent.revoked' AND target_id = ? AND actor_id = ?`, wrk.id, revokedBy).Scan(&revoked); err != nil || revoked != 1 {
		t.Errorf("agent.revoked rows by the admin = %d, %v, want one", revoked, err)
	}
	rows := f.nodeAudits(t)
	if len(rows) != 1 || rows[0].Action != "node.removed" || rows[0].Result != string(audit.ResultSuccess) || rows[0].Details["agent"] != wrk.id || rows[0].Details["hostname"] != "wrk-1" {
		t.Errorf("audit rows = %+v, want one success naming the revoked agent", rows)
	}

}

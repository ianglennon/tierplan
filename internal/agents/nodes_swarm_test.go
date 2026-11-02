// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build swarm

package agents_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
)

// nodeView is the part of a node as the nodes routes answer it that these tests read.
type nodeView struct {
	ID           string            `json:"id"`
	Version      uint64            `json:"version"`
	Hostname     string            `json:"hostname"`
	Role         string            `json:"role"`
	Leader       bool              `json:"leader"`
	State        string            `json:"state"`
	Availability string            `json:"availability"`
	Labels       map[string]string `json:"labels"`
	Agent        struct {
		Reporting bool `json:"reporting"`
	} `json:"agent"`
}

// node reads one node of the swarm by its harness node's hostname, from the nodes list and then the node route (the
// list has no version).
func (f *swarmFix) node(t *testing.T, n *dockertest.Node) nodeView {
	t.Helper()
	code, body := f.srv.Do(http.MethodGet, "/api/env/"+f.env.ID+"/nodes", nil)
	var list struct {
		Items []nodeView `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil {
		t.Fatalf("GET nodes = %d %s", code, body)
	}
	for _, it := range list.Items {
		if it.Hostname == n.Hostname {
			return f.nodeByID(t, it.ID)
		}
	}
	t.Fatalf("the nodes list has no node %s: %s", n.Hostname, body)
	return nodeView{}
}

// nodeByID reads the node route.
func (f *swarmFix) nodeByID(t *testing.T, id string) nodeView {
	t.Helper()
	code, body := f.srv.Do(http.MethodGet, "/api/env/"+f.env.ID+"/nodes/"+id, nil)
	var v nodeView
	if code != http.StatusOK || json.Unmarshal(body, &v) != nil {
		t.Fatalf("GET node %s = %d %s", id, code, body)
	}
	return v
}

// waitNode polls the node route, after reading the cluster again each time, until cond holds of the node as the
// route answers it (the list has neither the version nor the labels, so the route is read for the node it finds).
func (f *swarmFix) waitNode(t *testing.T, n *dockertest.Node, what string, d time.Duration, cond func(nodeView) bool) nodeView {
	t.Helper()
	var got nodeView
	f.srv.WaitFor("node "+n.Hostname+" "+what, d, func() (bool, string) {
		f.srv.RefreshCluster(f.env.ID)
		code, body := f.srv.Do(http.MethodGet, "/api/env/"+f.env.ID+"/nodes", nil)
		var list struct {
			Items []nodeView `json:"items"`
		}
		if code != http.StatusOK || json.Unmarshal(body, &list) != nil {
			return false, string(body)
		}
		for _, it := range list.Items {
			if it.Hostname != n.Hostname {
				continue
			}
			code, body = f.srv.Do(http.MethodGet, "/api/env/"+f.env.ID+"/nodes/"+it.ID, nil)
			var v nodeView
			if code != http.StatusOK || json.Unmarshal(body, &v) != nil {
				return false, string(body)
			}
			got = v
			b, _ := json.Marshal(v)
			return cond(v), string(b)
		}
		return false, "not listed"
	})
	return got
}

// changeNode sends a node change (the availability or the role route) with the version of a node read just
// before, and reads again and sends again when the answer is node_changed: the node object moves on its own as the
// swarm settles a change (a demotion takes the node out of the managers a moment after its spec says worker), and
// the version check that refuses a stale change is the very thing these tests show works. It gives up after five
// tries and answers the last. The body is sent with its version filled in.
func (f *swarmFix) changeNode(t *testing.T, id, route string, body map[string]any) (int, []byte) {
	t.Helper()
	for attempt := 1; ; attempt++ {
		body["version"] = f.nodeByID(t, id).Version
		code, raw := f.srv.Do(http.MethodPost, "/api/env/"+f.env.ID+"/nodes/"+id+"/"+route, body)
		if code != http.StatusConflict || problemType(raw) != "node_changed" || attempt == 5 {
			return code, raw
		}
		t.Logf("the node moved between the read and the %s change (attempt %d of 5); reading it again", route, attempt)
	}
}

// nodeAudit are the rows of an action as "result:to:code", in order.
func (f *swarmFix) nodeAudit(action string) (successes, failures []string) {
	for _, r := range f.srv.AuditRows(action) {
		line := r.Result + ":" + r.Details["to"] + ":" + r.Details["code"]
		if r.Result == "success" {
			successes = append(successes, line)
		} else {
			failures = append(failures, line)
		}
	}
	return successes, failures
}

// nodeImpact is the answer of the impact route.
type nodeImpact struct {
	Hostname string `json:"hostname"`
	Version  uint64 `json:"version"`
	Tasks    []struct {
		Service string `json:"service"`
		Outcome string `json:"outcome"`
	} `json:"tasks"`
	Managers struct {
		Total int `json:"total"`
		After int `json:"after"`
	} `json:"managers"`
	Warnings     []string `json:"warnings"`
	NeedsConfirm bool     `json:"needsConfirm"`
}

func (f *swarmFix) impact(t *testing.T, id, action string) nodeImpact {
	t.Helper()
	code, body := f.srv.Do(http.MethodGet, "/api/env/"+f.env.ID+"/nodes/"+id+"/impact?action="+action, nil)
	var in nodeImpact
	if code != http.StatusOK || json.Unmarshal(body, &in) != nil {
		t.Fatalf("GET impact of %s for %s = %d %s", id, action, code, body)
	}
	return in
}

// sleepService starts a replicated service of the harness image that does nothing, made on the manager, with the
// image resolved by no registry. It returns when every task is running.
func sleepService(t *testing.T, f *swarmFix, name string, replicas int, extra string) string {
	t.Helper()
	id := strings.TrimSpace(sh(t, f.sw.Managers[0], "docker service create --name "+name+" --replicas "+strconv.Itoa(replicas)+
		" --no-resolve-image --detach --stop-grace-period 1s "+extra+" "+dockertest.ServiceTestImage+" sleep 3600"))
	dockertest.WaitRunningTasks(t, f.sw.Managers[0], id, replicas, taskWait)
	return id
}

// runningHosts are the hostnames the running-desired tasks of a service run on, from a manager.
func runningHosts(t *testing.T, f *swarmFix, id string) []string {
	t.Helper()
	out, err := f.sw.Managers[0].Sh(execCtx(t, time.Minute), "docker service ps --filter desired-state=running --format '{{.Node}}|{{.CurrentState}}' "+id)
	if err != nil {
		return nil
	}
	var hosts []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if host, state, ok := strings.Cut(l, "|"); ok && strings.HasPrefix(state, "Running") {
			hosts = append(hosts, host)
		}
	}
	slices.Sort(hosts)
	return hosts
}

// TestSwarmNodeDrainAndRoles drains the worker of a real swarm of three managers and a worker, then demotes and promotes
// a manager, all by the routes (plan 06-37, RES-08, RES-11, RESEARCH A3, D-28): the impact of the drain says the worker's task
// moves; after the drain the service has its four tasks on the other three nodes; a change sent with the version
// that was read before another change is refused 409 node_changed, so the node object's version check is real
// (RESEARCH A3); activating the worker brings its agent back; the demote of a manager warns of the even count and
// leaves two, and the promote leaves three again. Each attempt that reached a node is audited.
func TestSwarmNodeDrainAndRoles(t *testing.T) {
	f := startSwarmFix(t, 3, 1)
	mgr, worker := f.sw.Managers[0], f.sw.Workers[0]
	dockertest.LoadImage(t, dockertest.ServiceTestImage, f.sw.Nodes()...)
	f.install(t)
	f.waitSwarmOnline(t, 4)
	base := "/api/env/" + f.env.ID

	id := sleepService(t, f, "drain-web", 4, "")
	if hosts := runningHosts(t, f, id); !slices.Contains(hosts, worker.Hostname) {
		t.Fatalf("the four tasks run on %v: the worker has none, so there is nothing to drain", hosts)
	}

	// Drain: the impact says the service's task moves and the agent's does not (a global service's task is not moved).
	w := f.node(t, worker)
	in := f.impact(t, w.ID, "drain")
	outcomes := map[string]string{}
	for _, tk := range in.Tasks {
		outcomes[tk.Service] = tk.Outcome
	}
	if outcomes["drain-web"] != "moves" || outcomes[agentName] != "stops" || in.Version != w.Version || in.Hostname != worker.Hostname {
		t.Errorf("impact of draining the worker = %+v, want drain-web to move, the agent to stop, at the node's version %d", in, w.Version)
	}
	if code, body := f.changeNode(t, w.ID, "availability", map[string]any{"availability": "drain", "confirm": worker.Hostname}); code != http.StatusNoContent {
		t.Fatalf("POST availability drain = %d %s", code, body)
	}
	dockertest.Wait(t, 3*time.Minute, "the service to run its four tasks on the three other nodes", func() (bool, string) {
		hosts := runningHosts(t, f, id)
		return len(hosts) == 4 && !slices.Contains(hosts, worker.Hostname), strings.Join(hosts, ",")
	})
	drained := f.waitNode(t, worker, "to be drained", time.Minute, func(n nodeView) bool { return n.Availability == "drain" })
	if drained.Version <= w.Version {
		t.Errorf("the node's version is %d after the drain, was %d: it must have moved", drained.Version, w.Version)
	}

	// A change that carries the version read before the drain comes back node_changed (RESEARCH A3), and writes nothing.
	stale := map[string]any{"availability": "active", "version": w.Version, "confirm": worker.Hostname}
	code, body := f.srv.Do(http.MethodPost, base+"/nodes/"+w.ID+"/availability", stale)
	if code != http.StatusConflict || problemType(body) != "node_changed" {
		t.Errorf("POST availability with a stale version = %d %s, want 409 node_changed", code, body)
	}
	if still := f.nodeByID(t, w.ID); still.Availability != "drain" {
		t.Errorf("the refused change left the node %s, want it still drained", still.Availability)
	}
	// With the version just read it is accepted, and the global agent service puts its agent back on the node.
	if code, body = f.changeNode(t, w.ID, "availability", map[string]any{"availability": "active", "confirm": worker.Hostname}); code != http.StatusNoContent {
		t.Fatalf("POST availability active = %d %s", code, body)
	}
	f.waitNode(t, worker, "to be active again", time.Minute, func(n nodeView) bool { return n.Availability == "active" })
	f.waitSwarmOnlineWithin(t, 4, recoverWait)
	// The audit log has the drain and the activation, and the refusal in between; any other failure would be a retry
	// after the node moved on its own, which is node_changed too.
	ok, failed := f.nodeAudit("node.availability_changed")
	if want := []string{"success:drain:done", "success:active:done"}; !slices.Equal(ok, want) {
		t.Errorf("successful node.availability_changed rows = %v, want %v", ok, want)
	}
	if !slices.Contains(failed, "failure:active:node_changed") || slices.ContainsFunc(failed, func(l string) bool { return !strings.HasSuffix(l, ":node_changed") }) {
		t.Errorf("failed node.availability_changed rows = %v, want only node_changed refusals, the stale one among them", failed)
	}

	// Roles: demote a manager that does not lead, then promote it again.
	leader := f.leaderNode(t)
	var target *dockertest.Node
	for _, m := range f.managersExcept(leader, mgr) {
		target = m
	}
	if target == nil {
		t.Fatal("no manager to demote besides the leader and the first")
	}
	n := f.node(t, target)
	dm := f.impact(t, n.ID, "demote")
	if dm.Managers.Total != 3 || dm.Managers.After != 2 || !slices.Contains(dm.Warnings, "even_count") {
		t.Errorf("impact of the demotion = %+v, want 3 managers now, 2 after and the even_count warning", dm)
	}
	if code, body = f.changeNode(t, n.ID, "role", map[string]any{"role": "worker", "confirm": target.Hostname}); code != http.StatusNoContent {
		t.Fatalf("POST role worker = %d %s", code, body)
	}
	demoted := f.waitNode(t, target, "to be a worker", 2*time.Minute, func(v nodeView) bool { return v.Role == "worker" })
	// The spec says worker at once; the swarm drops the node from its managers a moment later.
	dockertest.Wait(t, time.Minute, "docker node ls to show two managers after the demotion", func() (bool, string) {
		out, err := mgr.Sh(execCtx(t, time.Minute), "docker node ls --format '{{.ManagerStatus}}' | grep -c .")
		return err == nil && strings.TrimSpace(out) == "2", strings.TrimSpace(out)
	})
	pm := f.impact(t, demoted.ID, "promote")
	if pm.Managers.Total != 2 || pm.Managers.After != 3 {
		t.Errorf("impact of the promotion = %+v, want 2 managers now and 3 after", pm)
	}
	if code, body = f.changeNode(t, demoted.ID, "role", map[string]any{"role": "manager", "confirm": target.Hostname}); code != http.StatusNoContent {
		t.Fatalf("POST role manager = %d %s", code, body)
	}
	f.waitNode(t, target, "to be a manager", 2*time.Minute, func(v nodeView) bool { return v.Role == "manager" })
	if err := f.sw.WaitForQuorum(context.Background()); err != nil {
		t.Errorf("after the promotion: %v", err)
	}
	f.waitSwarmOnlineWithin(t, 4, recoverWait)
	ok, failed = f.nodeAudit("node.role_changed")
	if want := []string{"success:worker:done", "success:manager:done"}; !slices.Equal(ok, want) {
		t.Errorf("successful node.role_changed rows = %v, want %v", ok, want)
	}
	if slices.ContainsFunc(failed, func(l string) bool { return !strings.HasSuffix(l, ":node_changed") }) {
		t.Errorf("failed node.role_changed rows = %v, want none but node_changed retries", failed)
	}
}

// TestSwarmNodeRemove stops the worker of a real swarm and removes it by the route (plan 06-37, RES-08, D-28, D-10): a
// Ready manager and a Ready worker are refused (node_is_manager, node_not_down) before anything is written; once the
// engine of the stopped worker has been out of touch long enough to be Down, removing it needs its name typed and
// succeeds, never forced; its agent is revoked at once with the reason node_removed and no block, so a host that
// rejoins can enrol again; and a task of the service that ran there still names the node, which the tasks route
// shows as not known (a removed node).
func TestSwarmNodeRemove(t *testing.T) {
	f := startSwarmFix(t, 3, 1)
	mgr, worker := f.sw.Managers[0], f.sw.Workers[0]
	dockertest.LoadImage(t, dockertest.ServiceTestImage, f.sw.Nodes()...)
	f.install(t)
	f.waitSwarmOnline(t, 4)
	base := "/api/env/" + f.env.ID

	id := sleepService(t, f, "rm-web", 4, "")
	if hosts := runningHosts(t, f, id); !slices.Contains(hosts, worker.Hostname) {
		t.Fatalf("the four tasks run on %v: the worker has none", hosts)
	}
	w := f.node(t, worker)
	oldAgent, ok := f.agentOn(worker)
	if !ok || oldAgent.Status != "active" {
		t.Fatalf("the worker's agent = %+v, want active", oldAgent)
	}

	// A manager and a node that is up are refused before anything is written.
	m := f.node(t, f.sw.Managers[1])
	if code, body := f.srv.Do(http.MethodDelete, base+"/nodes/"+m.ID+"?confirm="+m.Hostname, nil); code != http.StatusConflict || problemType(body) != "node_is_manager" {
		t.Errorf("DELETE of a manager = %d %s, want 409 node_is_manager", code, body)
	}
	if code, body := f.srv.Do(http.MethodDelete, base+"/nodes/"+w.ID+"?confirm="+w.Hostname, nil); code != http.StatusConflict || problemType(body) != "node_not_down" {
		t.Errorf("DELETE of a Ready worker = %d %s, want 409 node_not_down", code, body)
	}
	if n := strings.TrimSpace(sh(t, mgr, "docker node ls -q | wc -l")); n != "4" {
		t.Fatalf("the swarm has %s nodes after the refused removals, want 4", n)
	}

	// Stop the worker and wait for the swarm to call it Down.
	if err := dockertest.StopNode(context.Background(), worker); err != nil {
		t.Fatal(err)
	}
	f.waitNode(t, worker, "to be down", 4*time.Minute, func(v nodeView) bool { return v.State == "down" })
	// Its task is replaced on the three nodes that remain.
	dockertest.Wait(t, 4*time.Minute, "the service to have four running tasks on the nodes that remain", func() (bool, string) {
		hosts := runningHosts(t, f, id)
		return len(hosts) == 4 && !slices.Contains(hosts, worker.Hostname), strings.Join(hosts, ",")
	})

	// Removal needs the name typed.
	if code, body := f.srv.Do(http.MethodDelete, base+"/nodes/"+w.ID, nil); code != http.StatusUnprocessableEntity || problemType(body) != "confirmation_required" {
		t.Errorf("DELETE of a Down worker without its name = %d %s, want 422 confirmation_required", code, body)
	}
	if code, body := f.srv.Do(http.MethodDelete, base+"/nodes/"+w.ID+"?confirm="+w.Hostname, nil); code != http.StatusNoContent {
		t.Fatalf("DELETE of the Down worker = %d %s", code, body)
	}
	if n := strings.TrimSpace(sh(t, mgr, "docker node ls -q | wc -l")); n != "3" {
		t.Errorf("the swarm has %s nodes after the removal, want 3", n)
	}

	// The agent is revoked by the removal itself, with no block.
	var got struct{ Status, Reason string }
	f.srv.WaitFor("the worker's agent to be revoked", 90*time.Second, func() (bool, string) {
		a, _ := f.agentOn(worker)
		got.Status, got.Reason = a.Status, a.RevokeReason
		return a.ID == oldAgent.ID && a.Status == "revoked", agentLines(f.srv.Agents(f.env.ID))
	})
	a, _ := f.agentOn(worker)
	if a.Blocked || a.Connected || got.Reason != "node_removed" {
		t.Errorf("the revoked agent = %+v, want not blocked, not connected, reason node_removed", a)
	}
	if blocks := f.audit(t, "agent.blocked"); len(blocks) != 0 {
		t.Errorf("agent.blocked entries = %+v, want none: removing a node blocks nothing", blocks)
	}
	removed := f.srv.AuditRows("node.removed")
	if len(removed) != 4 || removed[len(removed)-1].Result != "success" || removed[len(removed)-1].TargetID != w.ID {
		t.Errorf("node.removed rows = %+v, want the three refusals (a manager, a node that is up, no name) and the removal", removed)
	}
	f.waitSwarmOnlineWithin(t, 3, installWait)

	// A task that ran on the removed node still names it. The tasks route shows it as a node the swarm no longer lists.
	type taskItem struct {
		ID        string `json:"id"`
		NodeID    string `json:"nodeId"`
		Node      string `json:"node"`
		NodeKnown bool   `json:"nodeKnown"`
		State     string `json:"state"`
	}
	f.srv.WaitFor("a task of the service that names the removed node, shown as not known", time.Minute, func() (bool, string) {
		code, body := f.srv.Do(http.MethodGet, base+"/services/"+id+"/tasks", nil)
		var tasks struct {
			Slots []struct {
				Current taskItem   `json:"current"`
				History []taskItem `json:"history"`
			} `json:"slots"`
		}
		if code != http.StatusOK || json.Unmarshal(body, &tasks) != nil {
			return false, string(body)
		}
		var seen []string
		for _, s := range tasks.Slots {
			for _, tk := range append([]taskItem{s.Current}, s.History...) {
				if tk.NodeID == w.ID {
					seen = append(seen, tk.State)
					if !tk.NodeKnown && tk.Node == "" {
						return true, ""
					}
				}
			}
		}
		return false, "tasks naming the removed node: " + strings.Join(seen, ",")
	})
}

// sharedConstraintsMatchPlacement gives nodes labels with mixed case and a CIDR address, makes global services
// with placement constraints and compares the nodes the swarm really puts tasks on with the evaluator TierPlan shows
// placement by (plan 06-37, RES-12, RESEARCH A10): a label's value matches without regard to case, its name does not,
// a node without the label meets !=, node.ip takes a CIDR and its negation, and constraints are all required. A
// constraint that matches nothing leaves a task with no suitable node, as the evaluator says.
func sharedConstraintsMatchPlacement(t *testing.T, f *swarmFix) {
	mgr := f.sw.Managers[0]
	dockertest.LoadImage(t, dockertest.ServiceTestImage, f.sw.Nodes()...)
	f.install(t)
	f.waitSwarmOnline(t, 4)
	base := "/api/env/" + f.env.ID
	m1, m2, m3, w1 := f.sw.Managers[0], f.sw.Managers[1], f.sw.Managers[2], f.sw.Workers[0]

	// zone labels with mixed case: East on m1, east on m2, West on the worker, none on m3.
	for node, zone := range map[*dockertest.Node]string{m1: "East", m2: "east", w1: "West"} {
		v := f.node(t, node)
		code, body := f.srv.Do(http.MethodPut, base+"/nodes/"+v.ID+"/labels", map[string]any{"labels": map[string]string{"zone": zone}, "version": v.Version})
		if code != http.StatusNoContent && code != http.StatusOK {
			t.Fatalf("PUT labels of %s = %d %s", node.Hostname, code, body)
		}
	}
	// The addresses of the worker and the last manager are one /31 apart from the others.
	addr, err := netip.ParseAddr(w1.IP)
	if err != nil {
		t.Fatal(err)
	}
	cidr := netip.PrefixFrom(addr, 31).Masked().String()
	if !netip.MustParsePrefix(cidr).Contains(netip.MustParseAddr(m3.IP)) || netip.MustParsePrefix(cidr).Contains(netip.MustParseAddr(m1.IP)) {
		t.Fatalf("%s is expected to hold %s and %s but not %s", cidr, w1.IP, m3.IP, m1.IP)
	}

	for _, node := range f.sw.Nodes() {
		f.waitNode(t, node, "to show its zone label", time.Minute, func(v nodeView) bool {
			return v.Labels["zone"] != "" || node == m3
		})
	}

	cases := []struct {
		name        string
		constraints []string
		want        []*dockertest.Node
	}{
		{"label-value-case", []string{"node.labels.zone==EAST"}, []*dockertest.Node{m1, m2}},
		{"label-not-equal", []string{"node.labels.zone!=east"}, []*dockertest.Node{m3, w1}},
		{"ip-cidr", []string{"node.ip==" + cidr}, []*dockertest.Node{m3, w1}},
		{"ip-not-cidr", []string{"node.ip!=" + cidr}, []*dockertest.Node{m1, m2}},
		{"all-required", []string{"node.role==worker", "node.labels.zone==west"}, []*dockertest.Node{w1}},
		{"ip-and-label", []string{"node.ip==" + cidr, "node.labels.zone!=west"}, []*dockertest.Node{m3}},
	}
	byHost := map[string]*dockertest.Node{}
	for _, n := range f.sw.Nodes() {
		byHost[n.Hostname] = n
	}
	for _, tc := range cases {
		args := ""
		for _, c := range tc.constraints {
			args += " --constraint '" + c + "'"
		}
		id := strings.TrimSpace(sh(t, mgr, "docker service create --name cm-"+tc.name+" --mode global --no-resolve-image --detach --stop-grace-period 1s"+args+" "+dockertest.ServiceTestImage+" sleep 3600"))
		expectNames := make([]string, 0, len(tc.want))
		for _, n := range tc.want {
			expectNames = append(expectNames, n.Hostname)
		}
		slices.Sort(expectNames)

		// What the evaluator says, from the nodes TierPlan reads.
		code, body := f.srv.Do(http.MethodGet, base+"/placement?serviceId="+id, nil)
		var pl struct {
			Nodes []struct {
				ID, Hostname, Role, Address, OS, Arch string
				Labels, EngineLabels                  map[string]string
			} `json:"nodes"`
		}
		if code != http.StatusOK || json.Unmarshal(body, &pl) != nil || len(pl.Nodes) != 4 {
			t.Fatalf("%s: GET placement = %d %s", tc.name, code, body)
		}
		var evaluated []string
		for _, n := range pl.Nodes {
			ok, err := envs.MatchesAll(tc.constraints, envs.NodeFacts{
				ID: n.ID, Hostname: n.Hostname, IP: n.Address, Role: n.Role, OS: n.OS, Arch: n.Arch, Labels: n.Labels, EngineLabels: n.EngineLabels,
			})
			if err != nil {
				t.Fatalf("%s: the evaluator refused %v: %v", tc.name, tc.constraints, err)
			}
			if ok {
				evaluated = append(evaluated, n.Hostname)
			}
		}
		slices.Sort(evaluated)
		if !slices.Equal(evaluated, expectNames) {
			t.Errorf("%s: the evaluator says %v for %v, want %v", tc.name, evaluated, tc.constraints, expectNames)
		}

		// Where the swarm really put the tasks.
		dockertest.Wait(t, 3*time.Minute, tc.name+": the global service to run a task on each node that meets "+strings.Join(tc.constraints, " and "), func() (bool, string) {
			hosts := runningHosts(t, f, id)
			return slices.Equal(hosts, expectNames), strings.Join(hosts, ",")
		})
		// A global service has made all its tasks by now: nothing more arrives on a node that does not meet the constraints.
		dockertest.Wait(t, time.Minute, tc.name+": the service to hold only the tasks it should", func() (bool, string) {
			out := strings.TrimSpace(sh(t, mgr, "docker service ps --filter desired-state=running -q "+id+" | wc -l"))
			return out == strconv.Itoa(len(expectNames)), out
		})
		sh(t, mgr, "docker service rm "+id)
	}

	// A constraint nothing meets: the label's name is case sensitive, so Zone is not zone. A replicated task has no
	// suitable node, and the evaluator agrees that no node meets it.
	none := []string{"node.labels.Zone==east"}
	id := strings.TrimSpace(sh(t, mgr, "docker service create --name cm-none --replicas 1 --no-resolve-image --detach --constraint 'node.labels.Zone==east' "+dockertest.ServiceTestImage+" sleep 3600"))
	code, body := f.srv.Do(http.MethodGet, base+"/placement?serviceId="+id, nil)
	var pl struct {
		Nodes []struct {
			ID, Hostname, Role, Address, OS, Arch string
			Labels, EngineLabels                  map[string]string
		} `json:"nodes"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &pl) != nil {
		t.Fatalf("GET placement = %d %s", code, body)
	}
	for _, n := range pl.Nodes {
		if ok, _ := envs.MatchesAll(none, envs.NodeFacts{ID: n.ID, Hostname: n.Hostname, IP: n.Address, Role: n.Role, OS: n.OS, Arch: n.Arch, Labels: n.Labels, EngineLabels: n.EngineLabels}); ok {
			t.Errorf("the evaluator says %s meets %v, but no node has a label named Zone", n.Hostname, none)
		}
	}
	dockertest.Wait(t, 2*time.Minute, "the task of the unplaceable service to be pending with no suitable node", func() (bool, string) {
		out, _ := mgr.Sh(execCtx(t, time.Minute), "docker service ps --no-trunc --format '{{.CurrentState}}|{{.Error}}' "+id)
		return strings.Contains(out, "no suitable node"), strings.TrimSpace(out)
	})
}

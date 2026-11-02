// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build swarm

package agents_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/server/servertest"
	"github.com/ianglennon/tierplan/internal/tunnel"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// These suites are the real-swarm evidence for ENV-09 and ENV-05: what the
// dashboard does when a manager dies, when the quorum is lost, when an admin
// revokes a node and when a node leaves the swarm. Every wait is a poll with a
// deadline; the quorum-loss spike took about 20 s to notice (RESEARCH Pitfall 11).

const (
	// recoverWait is how long a stopped node, its swarm task and its agent get to
	// come back: an engine start, a raft election and a swarm task start in turn.
	recoverWait = 3 * time.Minute
	// failoverWait is how long the server gets to notice a dead manager.
	failoverWait = 60 * time.Second
)

// waitSwarmOnlineWithin is waitSwarmOnline with the deadline of the caller, for
// the waits that follow a node coming back.
func (f *swarmFix) waitSwarmOnlineWithin(t *testing.T, want int, d time.Duration) servertest.Environment {
	t.Helper()
	var got servertest.Environment
	f.srv.WaitFor("the swarm environment to be online with every agent", d, func() (bool, string) {
		e, ok := f.srv.Environment(f.env.ID)
		got = e
		ready := ok && e.Status.State == "online" && e.Engine.Version != "" &&
			e.Nodes != nil && e.Nodes.Ready == want && e.Nodes.Total == want &&
			e.Agents != nil && e.Agents.Connected == want && e.Agents.Expected == want
		return ready, summarise(e)
	})
	return got
}

// leaderNode is the manager `docker node ls` calls the Leader, asked of each
// manager in turn until one answers.
func (f *swarmFix) leaderNode(t *testing.T) *dockertest.Node {
	t.Helper()
	var leader *dockertest.Node
	dockertest.Wait(t, 60*time.Second, "a manager to say which node leads", func() (bool, string) {
		var seen []string
		for _, m := range f.sw.Managers {
			out, err := m.Sh(execCtx(t, time.Minute), "docker node ls --format '{{.Hostname}}|{{.ManagerStatus}}'")
			if err != nil {
				seen = append(seen, err.Error())
				continue
			}
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				host, status, _ := strings.Cut(line, "|")
				if status != "Leader" {
					continue
				}
				for _, n := range f.sw.Managers {
					if n.Hostname == host {
						leader = n
						return true, ""
					}
				}
			}
			seen = append(seen, out)
		}
		return false, strings.Join(seen, "; ")
	})
	return leader
}

// managersExcept is every manager but the ones named.
func (f *swarmFix) managersExcept(skip ...*dockertest.Node) []*dockertest.Node {
	var out []*dockertest.Node
	for _, m := range f.sw.Managers {
		if !slices.Contains(skip, m) {
			out = append(out, m)
		}
	}
	return out
}

// waitLeaderKnown waits until the server's own agent list calls the agent of node
// the leader, which is the server having read the cluster and learned who leads,
// and then reads the cluster once more. The first read of an environment goes to
// the manager with the lowest agent ID; only once the leader is known do reads go
// to it. The server does that every 15 seconds, so a test that stops the leader
// before the next timer would be stopping a manager the server never used, which
// is no failover at all. Reading again here makes the leader the manager in use.
func (f *swarmFix) waitLeaderKnown(t *testing.T, leader *dockertest.Node) {
	t.Helper()
	f.srv.WaitFor("the server to learn that "+leader.Hostname+" leads", installWait, func() (bool, string) {
		as := f.srv.Agents(f.env.ID)
		for _, a := range as {
			if a.Node == leader.Hostname && a.Role == "leader" {
				return true, ""
			}
		}
		return false, agentLines(as)
	})
	f.srv.RefreshCluster(f.env.ID)
}

// agentLines is the agent rows as short lines, for a failure message.
func agentLines(as []servertest.Agent) string {
	var out []string
	for _, a := range as {
		out = append(out, strings.Join([]string{a.ID[:8], a.Node, a.Role, a.Status, map[bool]string{true: "connected", false: "offline"}[a.Connected], map[bool]string{true: "blocked", false: ""}[a.Blocked]}, " "))
	}
	return strings.Join(out, "; ")
}

// agentOn is the agent row of a node, preferring the one that is not revoked.
func (f *swarmFix) agentOn(node *dockertest.Node) (servertest.Agent, bool) {
	var found servertest.Agent
	ok := false
	for _, a := range f.srv.Agents(f.env.ID) {
		if a.Node != node.Hostname {
			continue
		}
		if !ok || (found.Status == "revoked" && a.Status != "revoked") {
			found, ok = a, true
		}
	}
	return found, ok
}

// auditEntry is the part of an audit row these tests read.
type auditEntry struct {
	Action      string            `json:"action"`
	Actor       string            `json:"actor"`
	ActorType   string            `json:"actorType"`
	Result      string            `json:"result"`
	Reason      string            `json:"reason"`
	Target      string            `json:"target"`
	Environment *string           `json:"environment"`
	Details     map[string]string `json:"details"`
}

// audit lists the entries of one action, newest first.
func (f *swarmFix) audit(t *testing.T, action string) []auditEntry {
	t.Helper()
	code, body := f.srv.Do(http.MethodGet, "/api/audit?limit=100&action="+action, nil)
	var list struct {
		Items []auditEntry `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil {
		t.Fatalf("GET /api/audit?action=%s = %d %s", action, code, body)
	}
	return list.Items
}

// feedRow is one row of GET /api/env/{id}/events.
type feedRow struct {
	Node   string `json:"node"`
	Type   string `json:"type"`
	Action string `json:"action"`
	Name   string `json:"name"`
}

// feed is the recent events of the environment, newest first.
func (f *swarmFix) feed(t *testing.T) []feedRow {
	t.Helper()
	code, body := f.srv.Do(http.MethodGet, "/api/env/"+f.env.ID+"/events", nil)
	var list struct {
		Items []feedRow `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil {
		t.Fatalf("GET events = %d %s", code, body)
	}
	return list.Items
}

// tierplanRows are the feed rows TierPlan wrote itself with this action.
func tierplanRows(rows []feedRow, action string) []feedRow {
	var out []feedRow
	for _, r := range rows {
		if r.Type == "tierplan" && r.Action == action {
			out = append(out, r)
		}
	}
	return out
}

// waitRefusal waits for the audit log to hold an agent.enrol_refused entry whose
// reason contains this text.
func (f *swarmFix) waitRefusal(t *testing.T, reason string, d time.Duration) {
	t.Helper()
	f.srv.WaitFor("a refused attempt with reason "+reason+" in the audit log", d, func() (bool, string) {
		var seen []string
		for _, e := range f.audit(t, "agent.enrol_refused") {
			seen = append(seen, e.Reason)
			if strings.Contains(e.Reason, reason) {
				return true, ""
			}
		}
		return false, "reasons seen: " + strings.Join(seen, ", ")
	})
}

// agentContainerLog is the log of the newest agent container on a node, from
// before it is replaced.
func agentContainerLog(t *testing.T, n *dockertest.Node) string {
	t.Helper()
	out, _ := n.Sh(execCtx(t, time.Minute), "for c in $(docker ps -aq --filter label=tierplan.agent=true); do docker logs $c 2>&1; done")
	return out
}

// TestSwarmManagerFailover stops the node of the leader while the other two
// managers keep the quorum. The dashboard keeps answering with no error, the feed
// and the audit log say once that the manager is unreachable and which one took
// over, the environment is degraded while the node is away, and it is online again
// when the node is back with no second record (ENV-09, D-43, D-57).
func TestSwarmManagerFailover(t *testing.T) {
	f := startSwarmFix(t, 3, 1)
	f.install(t)
	f.waitSwarmOnline(t, 4)
	leader := f.leaderNode(t)
	f.waitLeaderKnown(t, leader)
	if _, code, body := f.srv.Summary(f.env.ID); code != http.StatusOK {
		t.Fatalf("summary before the failure = %d %s", code, body)
	}
	t.Logf("stopping the leader %s", leader.Hostname)
	ctx := execCtx(t, 15*time.Minute)

	if err := dockertest.StopNode(ctx, leader); err != nil {
		t.Fatal(err)
	}

	// From the moment the node is gone, every dashboard call answers 200: a failed
	// manager is not an error anyone sees.
	calls := 0
	wantFeed := "Manager " + leader.Hostname + " unreachable, using "
	var row feedRow
	var env servertest.Environment
	f.srv.WaitFor("the failover to be in the feed, the audit log and the status", failoverWait, func() (bool, string) {
		sum, code, body := f.srv.Summary(f.env.ID)
		calls++
		if code != http.StatusOK {
			t.Fatalf("summary call %d after the leader stopped = %d %s, want 200 and no error shown to anyone", calls, code, body)
		}
		_ = sum
		for _, r := range tierplanRows(f.feed(t), "failover") {
			if strings.HasPrefix(r.Name, wantFeed) {
				row = r
			}
		}
		e, _ := f.srv.Environment(f.env.ID)
		env = e
		ok := row.Name != "" && len(f.audit(t, "environment.failover")) > 0 && e.Status.State == "degraded" && e.HasIssue("agent_offline")
		return ok, "feed row " + row.Name + "; status " + summarise(e)
	})
	for range 10 {
		if _, code, body := f.srv.Summary(f.env.ID); code != http.StatusOK {
			t.Fatalf("summary = %d %s with one manager down", code, body)
		}
		// The dashboard answering 200 over a bounded stretch of real time with one
		// manager down is the behaviour under test; no event marks the stretch's end.
		time.Sleep(500 * time.Millisecond) //nolint:forbidigo // an observation window that is the behaviour under test
	}
	t.Logf("%d summary calls answered 200 while the failover was noticed; feed row %q on node %q", calls, row.Name, row.Node)
	t.Logf("status with the leader down: %s", summarise(env))

	// The row names the manager that took over, and it is a manager that is still up.
	using := strings.TrimPrefix(row.Name, wantFeed)
	if !slices.ContainsFunc(f.managersExcept(leader), func(n *dockertest.Node) bool { return n.Hostname == using }) {
		t.Errorf("the feed says %q took over, want one of the surviving managers %v", using, f.managersExcept(leader))
	}
	var issueNames []string
	for _, i := range env.Status.Issues {
		issueNames = append(issueNames, i.Code+"@"+i.Node)
	}
	if !slices.Contains(issueNames, "agent_offline@"+leader.Hostname) {
		t.Errorf("issues = %v, want agent_offline naming %s", issueNames, leader.Hostname)
	}
	audits := f.audit(t, "environment.failover")
	if len(audits) != 1 || audits[0].Actor != "system:agent-hub" || audits[0].Details["from"] != leader.Hostname || audits[0].Details["to"] != using {
		t.Errorf("environment.failover audit entries = %+v, want exactly one under system:agent-hub from %s to %s", audits, leader.Hostname, using)
	}
	// Node-local data of the survivors still reads, and the list names the silent node.
	list, code, body := f.srv.Containers(f.env.ID, "")
	if code != http.StatusOK || !slices.Contains(list.NotReporting, leader.Hostname) || len(list.Items) < 3 {
		t.Errorf("containers with the leader down = %d %s, want 200, at least the three survivors' agents and %s not reporting", code, body, leader.Hostname)
	}

	// Right after the election a follower can still report every node as not ready,
	// so the overview may say 0 of 4 for a moment. It settles on what is true: three
	// nodes ready of four, the dead manager's agent the one that is missing.
	f.srv.WaitFor("the overview to settle on 3 of 4 nodes ready and 3 of 4 agents", failoverWait, func() (bool, string) {
		e, ok := f.srv.Environment(f.env.ID)
		env = e
		return ok && e.Nodes != nil && e.Nodes.Ready == 3 && e.Nodes.Total == 4 &&
			e.Agents != nil && e.Agents.Connected == 3 && e.Agents.Expected == 4 && e.Status.State == "degraded", summarise(e)
	})
	t.Logf("the overview settled on %s", summarise(env))

	// The node comes back: the environment is online again, nobody is told twice.
	if err := dockertest.StartNode(ctx, leader); err != nil {
		t.Fatal(err)
	}
	f.waitSwarmOnlineWithin(t, 4, recoverWait)
	if rows := tierplanRows(f.feed(t), "failover"); len(rows) != 1 {
		t.Errorf("failover rows in the feed after the node returned = %+v, want exactly one", rows)
	}
	if audits := f.audit(t, "environment.failover"); len(audits) != 1 {
		t.Errorf("environment.failover audit entries after the node returned = %d, want exactly one", len(audits))
	}
	if got := len(f.srv.Agents(f.env.ID)); got != 4 {
		t.Errorf("agent rows after the node returned = %d, want the same four (no re-enrolment)", got)
	}
}

// TestSwarmQuorumLoss stops two of the three managers. The remaining one cannot
// elect a leader, so cluster questions fail; the environment says no_quorum, not a
// generic error, and what each running node holds is still served. Starting the
// managers again brings the environment back online (D-43, RESEARCH Pitfall 11).
func TestSwarmQuorumLoss(t *testing.T) {
	f := startSwarmFix(t, 3, 1)
	f.install(t)
	f.waitSwarmOnline(t, 4)
	leader := f.leaderNode(t)
	f.waitLeaderKnown(t, leader)

	// Keep the first manager and the worker; stop the other two, whichever leads.
	stopped := f.sw.Managers[1:]
	running := []*dockertest.Node{f.sw.Managers[0], f.sw.Workers[0]}
	ctx := execCtx(t, 20*time.Minute)
	for _, n := range stopped {
		if err := dockertest.StopNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	began := time.Now()

	var env servertest.Environment
	f.srv.WaitFor("the environment to say no_quorum", 90*time.Second, func() (bool, string) {
		e, ok := f.srv.Environment(f.env.ID)
		env = e
		return ok && e.Status.State == "degraded" && e.HasIssue("no_quorum"), summarise(e)
	})
	t.Logf("no_quorum shown %s after the managers stopped: %s", time.Since(began).Round(time.Second), summarise(env))
	if env.Status.Reason != "no_quorum" {
		t.Errorf("reason = %q, want no_quorum as the worst cause", env.Status.Reason)
	}

	// Node-local data of the running nodes still reads, and the stopped nodes are named.
	f.srv.WaitFor("the running nodes' rows to be listed", 30*time.Second, func() (bool, string) {
		list, code, body := f.srv.Containers(f.env.ID, "")
		if code != http.StatusOK {
			return false, string(body)
		}
		var nodes []string
		for _, c := range list.Items {
			nodes = append(nodes, c.Node)
		}
		want := []string{running[0].Hostname, running[1].Hostname}
		return slices.Contains(nodes, want[0]) && slices.Contains(nodes, want[1]), string(body)
	})
	list, _, _ := f.srv.Containers(f.env.ID, "")
	for _, n := range stopped {
		if !slices.Contains(list.NotReporting, n.Hostname) {
			t.Errorf("notReporting = %v, want %s named", list.NotReporting, n.Hostname)
		}
	}
	sum, code, body := f.srv.Summary(f.env.ID)
	if code != http.StatusOK {
		t.Fatalf("summary with no quorum = %d %s, want 200 from the nodes that run", code, body)
	}
	if sum.Counts.Nodes != nil || sum.Counts.Containers < 2 {
		t.Errorf("summary counts = nodes %v containers %d, want nodes unknown (no manager can say) and the running nodes' containers: %s", sum.Counts.Nodes, sum.Counts.Containers, body)
	}

	// The managers come back; the swarm elects a leader and the environment is online.
	for _, n := range stopped {
		if err := dockertest.StartNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.sw.WaitForQuorum(ctx); err != nil {
		t.Fatal(err)
	}
	f.waitSwarmOnlineWithin(t, 4, recoverWait)
	if sum, code, body := f.srv.Summary(f.env.ID); code != http.StatusOK || sum.Counts.Nodes == nil || *sum.Counts.Nodes != 4 {
		t.Errorf("summary after the quorum returned = %d %s, want 4 nodes again", code, body)
	}
}

// sharedRevokeBlocksReenrol revokes the worker's agent. The session closes at
// once; the agent is refused revoked, drops its identity and enrols again with the
// token, which is still valid, and is refused blocked and waits; after the unblock
// the host enrols again by itself as a new agent (ENV-05, D-08, D-09, D-10).
func sharedRevokeBlocksReenrol(t *testing.T, f *swarmFix) {
	worker := f.sw.Workers[0]
	f.install(t)
	f.waitSwarmOnline(t, 4)
	old, ok := f.agentOn(worker)
	if !ok || !old.Connected || old.Status != "active" {
		t.Fatalf("the worker's agent = %+v (found %v), want connected and active", old, ok)
	}
	revokePath := "/api/env/" + f.env.ID + "/agents/" + old.ID

	// Revoke: the session ends, the row says why and that it is blocked. The ordering
	// that matters is that the revoked agent has no session once the revoke has
	// committed, not how long the close takes: the hub drops the session when its
	// socket closes, and the agent's next hello is refused, so it never registers again.
	code, body := f.srv.Do(http.MethodPost, revokePath+"/revoke", map[string]string{"reason": "swarm test"})
	if code != http.StatusNoContent {
		t.Fatalf("POST revoke = %d %s", code, body)
	}
	if _, err := f.srv.Core().Tunnel().AwaitSession(testsupport.Context(t), old.ID, func(s *tunnel.Session) bool { return s == nil }); err != nil {
		t.Fatalf("the revoked agent still has a session after the revoke answered: %v", err)
	}
	f.srv.WaitFor("the revoked agent to be listed as not connected", 60*time.Second, func() (bool, string) {
		a, _ := f.agentOn(worker)
		return !a.Connected, fmt.Sprintf("%+v", a)
	})
	got, _ := f.agentOn(worker)
	if got.ID != old.ID || got.Status != "revoked" || !got.Blocked || got.RevokeReason != "swarm test" || got.RevokedAt == "" {
		t.Errorf("the worker's agent after the revoke = %+v, want revoked, blocked, reason \"swarm test\"", got)
	}
	if again, _ := f.srv.Do(http.MethodPost, revokePath+"/revoke", nil); again != http.StatusConflict {
		t.Errorf("a second revoke = %d, want 409 agent_already_revoked", again)
	}
	if row := f.srv.AgentRow(old.ID); row.RevokedBy.String != "u1" {
		t.Errorf("revoked_by = %+v, want the admin", row.RevokedBy)
	}
	t.Logf("the agent's own log after being revoked:\n%s", agentContainerLog(t, worker))

	// The kicked agent reconnects at once and says hello with its old certificate:
	// refused revoked. It has its join token (a Docker secret of the service), so it
	// drops that identity and enrols again, and the block on its host refuses that
	// with blocked (D-08, D-10).
	f.waitRefusal(t, "revoked", 90*time.Second)
	f.waitRefusal(t, "blocked", 120*time.Second)
	f.srv.WaitFor("the agent to say it was refused revoked and then blocked", 60*time.Second, func() (bool, string) {
		log := agentContainerLog(t, worker)
		return strings.Contains(log, "code=revoked") && strings.Contains(log, "code=blocked"), log
	})
	if a, _ := f.agentOn(worker); a.ID != old.ID || a.Status != "revoked" || a.Connected {
		t.Errorf("after the refused enrolment the row = %+v, want the same agent, still revoked and not connected", a)
	}
	if n := len(f.srv.Agents(f.env.ID)); n != 4 {
		t.Errorf("agent rows = %d, want 4 (a refused hello and a refused enrolment make no row)", n)
	}

	// A restarted task has no identity on its volume any more. It enrols with the
	// token, is refused blocked again and stays up waiting: no exit, so no restart
	// loop (and no tight loop of enrolments either).
	sh(t, worker, "docker rm -f $(docker ps -aq --filter label=tierplan.agent=true)")
	f.srv.WaitFor("swarm to start the agent task again", 90*time.Second, func() (bool, string) {
		out, _ := worker.Sh(execCtx(t, time.Minute), "docker ps -q --filter label=tierplan.agent=true")
		return strings.TrimSpace(out) != "", out
	})
	f.srv.WaitFor("the restarted agent to say it was refused blocked", 60*time.Second, func() (bool, string) {
		out, _ := worker.Sh(execCtx(t, time.Minute), "for c in $(docker ps -q --filter label=tierplan.agent=true); do docker logs $c 2>&1; done")
		return strings.Contains(out, "code=blocked"), out
	})
	// The blocked agent's container must stay up for a bounded window after it logged
	// code=blocked (it waits, it does not exit); no event marks the window's end.
	time.Sleep(10 * time.Second) //nolint:forbidigo // an observation window that is the behaviour under test
	if out, _ := worker.Sh(execCtx(t, time.Minute), "docker ps -q --filter label=tierplan.agent=true"); strings.TrimSpace(out) == "" {
		t.Errorf("the blocked agent's container is not running; it must wait, not exit:\n%s", agentContainerLog(t, worker))
	}
	if n := len(f.srv.Agents(f.env.ID)); n != 4 {
		t.Errorf("agent rows after a blocked enrolment = %d, want 4", n)
	}
	if a, _ := f.agentOn(worker); a.ID != old.ID || a.Connected {
		t.Errorf("the worker's row = %+v, want the old revoked agent and nothing connected", a)
	}

	// Unblock: the waiting agent enrols again at its next try (at most a minute),
	// as a new agent. The old row stays revoked.
	if code, body := f.srv.Do(http.MethodPost, revokePath+"/unblock", nil); code != http.StatusNoContent {
		t.Fatalf("POST unblock = %d %s", code, body)
	}
	if code, _ := f.srv.Do(http.MethodPost, revokePath+"/unblock", nil); code != http.StatusConflict {
		t.Errorf("a second unblock = %d, want 409 not_blocked", code)
	}
	var fresh servertest.Agent
	f.srv.WaitFor("a new active agent row for the worker", 180*time.Second, func() (bool, string) {
		for _, a := range f.srv.Agents(f.env.ID) {
			if a.Node == worker.Hostname && a.Status == "active" && a.Connected {
				fresh = a
				return true, ""
			}
		}
		return false, agentLines(f.srv.Agents(f.env.ID))
	})
	if fresh.ID == old.ID {
		t.Errorf("the host enrolled as agent %s, the revoked one", fresh.ID)
	}
	if oldNow := f.srv.AgentRow(old.ID); oldNow.Status != "revoked" {
		t.Errorf("the old agent is %q after the unblock, want it to stay revoked", oldNow.Status)
	}
	f.waitSwarmOnlineWithin(t, 4, installWait)
	if n := len(f.srv.Agents(f.env.ID)); n != 5 {
		t.Errorf("agent rows = %d, want 5 (the revoked one and the new one for the worker)", n)
	}
	for _, action := range []string{"agent.revoked", "agent.blocked", "agent.unblocked"} {
		if len(f.audit(t, action)) == 0 {
			t.Errorf("no %s entry in the audit log", action)
		}
	}
}

// TestSwarmNodeRemovalAutoRevokes makes the worker leave the swarm and removes it
// with docker node rm. The reaper revokes its agent under system:agent-reaper, with
// no block; when the host rejoins, an agent of the global service enrols again
// (D-10).
func TestSwarmNodeRemovalAutoRevokes(t *testing.T) {
	f := startSwarmFix(t, 3, 1)
	mgr, worker := f.sw.Managers[0], f.sw.Workers[0]
	f.install(t)
	f.waitSwarmOnline(t, 4)
	old, ok := f.agentOn(worker)
	if !ok || old.Status != "active" {
		t.Fatalf("the worker's agent = %+v, want active", old)
	}

	sh(t, worker, "docker swarm leave")
	// A node that has left is "down" for a few seconds before a manager lets it go.
	dockertest.Wait(t, 60*time.Second, "docker node rm to accept the worker", func() (bool, string) {
		out, err := mgr.Sh(execCtx(t, time.Minute), "docker node rm "+worker.Hostname+" 2>&1")
		return err == nil, out
	})
	removed := time.Now()

	var got servertest.Agent
	f.srv.WaitFor("the worker's agent to be revoked by the reaper", 90*time.Second, func() (bool, string) {
		a, _ := f.agentOn(worker)
		got = a
		return a.ID == old.ID && a.Status == "revoked", agentLines(f.srv.Agents(f.env.ID))
	})
	t.Logf("the reaper revoked the agent %s after docker node rm", time.Since(removed).Round(time.Second))
	if got.Blocked || got.Connected || got.RevokeReason != "left_swarm" {
		t.Errorf("the revoked agent = %+v, want not blocked, not connected, reason left_swarm", got)
	}
	row := f.srv.AgentRow(old.ID)
	if row.RevokedBy.String != "system:agent-reaper" {
		t.Errorf("revoked_by = %+v, want system:agent-reaper", row.RevokedBy)
	}
	autos := f.audit(t, "agent.auto_revoked")
	if len(autos) != 1 || autos[0].Actor != "system:agent-reaper" || autos[0].ActorType != "system" || autos[0].Target != old.ID {
		t.Errorf("agent.auto_revoked entries = %+v, want one under system:agent-reaper for %s", autos, old.ID)
	}
	if blocks := f.audit(t, "agent.blocked"); len(blocks) != 0 {
		t.Errorf("agent.blocked entries = %+v, want none: an automatic revoke blocks nothing", blocks)
	}
	// With the node gone from the list the environment expects three nodes, and has them.
	f.waitSwarmOnlineWithin(t, 3, installWait)

	// The host rejoins with a fresh join token and the global service puts an agent on
	// it. That agent finds the identity of the one that was revoked on its state
	// volume, which is on the host and which leaving the swarm does not touch. It says
	// hello with it and is refused revoked; the agent has its join token (a Docker
	// secret of the service), so it drops that identity and enrols again as a new
	// agent, with nothing removed from the host by hand (D-10).
	if out, err := worker.Sh(execCtx(t, time.Minute), "docker volume inspect "+agentName+" 2>&1"); err != nil {
		t.Fatalf("the agent state volume is gone from the worker before the rejoin: %v\n%s", err, out)
	}
	token := strings.TrimSpace(sh(t, mgr, "docker swarm join-token -q worker"))
	sh(t, worker, "docker swarm join --token "+token+" --advertise-addr "+worker.IP+" "+mgr.IP+":2377")
	var fresh servertest.Agent
	f.srv.WaitFor("a new active agent row for the rejoined worker", recoverWait, func() (bool, string) {
		for _, a := range f.srv.Agents(f.env.ID) {
			if a.Node == worker.Hostname && a.Status == "active" && a.Connected {
				fresh = a
				return true, ""
			}
		}
		return false, agentLines(f.srv.Agents(f.env.ID)) + "\nworker agent log:\n" + agentContainerLog(t, worker)
	})
	if fresh.ID == old.ID {
		t.Errorf("the rejoined host came back as %s, the revoked agent", old.ID)
	}
	f.waitSwarmOnlineWithin(t, 4, installWait)
	if n := len(f.srv.Agents(f.env.ID)); n != 5 {
		t.Errorf("agent rows = %d, want 5 (the revoked one and the new one for the worker)", n)
	}
	if blocks := f.audit(t, "agent.blocked"); len(blocks) != 0 {
		t.Errorf("agent.blocked entries after the rejoin = %+v, want none", blocks)
	}
	if oldNow := f.srv.AgentRow(old.ID); oldNow.Status != "revoked" {
		t.Errorf("the old agent is %q after the rejoin, want it to stay revoked", oldNow.Status)
	}
	// It got there by itself: the old agent's hello was refused revoked, the agent
	// says it dropped the identity, and its state volume was never removed.
	log := agentContainerLog(t, worker)
	if !strings.Contains(log, "code=revoked") || !strings.Contains(log, "will enrol again with its join token") {
		t.Errorf("the worker's agent log does not show the refusal and the new enrolment:\n%s", log)
	}
	if out, err := worker.Sh(execCtx(t, time.Minute), "docker volume inspect "+agentName+" 2>&1"); err != nil {
		t.Errorf("the agent state volume is gone from the worker: %v\n%s", err, out)
	}
}

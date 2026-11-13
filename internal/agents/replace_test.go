// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package agents_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/agents"
	"github.com/ianglennon/tierplan/internal/tunnel"
	"github.com/ianglennon/tierplan/proto/wire"
)

// Review 17 CR-02: a host whose agent gave up its identity (an agent reinstalled with
// a new install command, or one that set its identity aside after an auth_rejected)
// enrols again into the same environment while its earlier row is still active. That
// row is retired in the enrolment's transaction, so the host never has two active
// agents.

func activeAgentsOf(t *testing.T, r *rig, envID string) int {
	t.Helper()
	return count(t, r.st, `SELECT COUNT(*) FROM agents WHERE env_id = ? AND status = 'active'`, envID)
}

func TestAStandaloneHostThatEnrolsAgainRetiresItsEarlierAgentWithoutABlock(t *testing.T) {
	r := newRig(t)
	k := &kicks{st: r.st}
	n := &notices{}
	r.svc.WithKicker(k).WithNotifier(n)
	c := r.create(t, "edge")
	host := standalone("ENG:EDGE-1", "edge-1")
	first := r.enrolFacts(t, c, host)

	second := r.enrolFacts(t, c, host)
	if second.id == first.id {
		t.Fatal("the second enrolment got the first agent's ID")
	}
	if got := activeAgentsOf(t, r, c.EnvID); got != 1 {
		t.Fatalf("active agents = %d, want 1", got)
	}
	if a := r.agentRow(t, second.id); a.status != "active" {
		t.Fatalf("the new agent is %+v, want active", a)
	}
	if a := r.agentRow(t, first.id); a.status != "revoked" || a.reason != "replaced" || a.revokedBy != "system:agent-enrol" {
		t.Fatalf("the earlier agent is %+v, want revoked as replaced by system:agent-enrol", a)
	}
	if n := count(t, r.st, `SELECT COUNT(*) FROM agent_blocks WHERE env_id = ?`, c.EnvID); n != 0 {
		t.Errorf("the retire made %d block rows, want none: the host is the one enrolling", n)
	}

	rows := auditRows(t, r.st, "agent.revoked")
	if len(rows) != 1 || !strings.Contains(rows[0], `"reason":"replaced"`) || !strings.Contains(rows[0], `"replaced_by":"`+second.id+`"`) ||
		!strings.Contains(rows[0], `"node":"edge-1"`) {
		t.Fatalf("agent.revoked rows = %v, want one naming the node, the reason replaced and the new agent", rows)
	}
	var actorID, target string
	if err := r.st.Read.QueryRow(`SELECT actor_id, target_id FROM audit_log WHERE action = 'agent.revoked'`).Scan(&actorID, &target); err != nil ||
		actorID != "system:agent-enrol" || target != first.id {
		t.Errorf("the row's actor %q target %q, %v; want system:agent-enrol and the earlier agent", actorID, target, err)
	}
	for _, other := range []string{"agent.blocked", "agent.auto_revoked"} {
		if rows := auditRows(t, r.st, other); len(rows) != 0 {
			t.Errorf("%s rows = %v, want none", other, rows)
		}
	}
	if rows := auditRows(t, r.st, "agent.enrolled"); len(rows) != 2 {
		t.Errorf("agent.enrolled rows = %d, want 2", len(rows))
	}
	if len(k.agents) != 1 || k.agents[0] != first.id || k.codes[0] != tunnel.DisconnectRevoked || k.statusAtKick != "revoked" {
		t.Errorf("sessions closed = %v %v (row was %q at the time), want the earlier agent's, as revoked, after the commit", k.agents, k.codes, k.statusAtKick)
	}
	if got := n.list(); len(got) != 1 || got[0] != c.EnvID {
		t.Errorf("notified %v, want the environment once", got)
	}

	// The earlier identity is refused revoked from now on, and told why.
	_, err := r.svc.Hello(r.ctx, first.hello(first.cert, first.priv))
	if ref := refusal(t, err); ref.Code != wire.ReasonRevoked || !strings.Contains(ref.Detail, "enrolled again") {
		t.Errorf("hello of the replaced agent = %+v, want revoked saying the host enrolled again", ref)
	}
	if _, err := r.svc.Hello(r.ctx, second.hello(second.cert, second.priv)); err != nil {
		t.Errorf("hello of the new agent: %v", err)
	}
}

func TestAnEnrolmentOfAnotherHostRetiresNothing(t *testing.T) {
	r := newRig(t)
	s := newSwarmEnv(t, r, "prod")
	if got := activeAgentsOf(t, r, s.c.EnvID); got != 3 {
		t.Fatalf("active agents = %d, want the 3 of the swarm", got)
	}
	r.enrolFacts(t, s.c, worker("ENG:W3-prod", "node-w3-prod", "wrk-3"))
	if got := activeAgentsOf(t, r, s.c.EnvID); got != 4 {
		t.Fatalf("active agents = %d, want 4: a new host retires nobody", got)
	}
	if rows := auditRows(t, r.st, "agent.revoked"); len(rows) != 0 {
		t.Errorf("agent.revoked rows = %v, want none", rows)
	}
	if len(s.kicked.agents) != 0 || len(s.notified.list()) != 0 {
		t.Errorf("kicked %v and notified %v, want neither", s.kicked.agents, s.notified.list())
	}
}

func TestASwarmNodeThatEnrolsAgainRetiresOnlyTheAgentOfThatNode(t *testing.T) {
	r := newRig(t)
	s := newSwarmEnv(t, r, "prod")
	again := r.enrolFacts(t, s.c, worker("ENG:W2-prod", "node-w2-prod", "wrk-2"))
	if a := r.agentRow(t, s.w2.id); a.status != "revoked" || a.reason != "replaced" {
		t.Fatalf("the node's earlier agent is %+v, want revoked as replaced", a)
	}
	for name, id := range map[string]string{"manager": s.m.id, "first worker": s.w1.id, "new agent": again.id} {
		if a := r.agentRow(t, id); a.status != "active" {
			t.Errorf("the %s is %+v, want active", name, a)
		}
	}
	if got := activeAgentsOf(t, r, s.c.EnvID); got != 3 {
		t.Errorf("active agents = %d, want 3", got)
	}
	rows := auditRows(t, r.st, "agent.revoked")
	if len(rows) != 1 || !strings.Contains(rows[0], `"swarm_node":"node-w2-prod"`) {
		t.Errorf("agent.revoked rows = %v, want one for the node", rows)
	}
	// A revoke that the retire already made is not made twice.
	if err := r.svc.RevokeRemovedNode(r.ctx, admin, s.c.EnvID, s.w2.id, "", ""); !errors.Is(err, agents.ErrAlreadyRevoked) {
		t.Errorf("a later revoke of the replaced agent = %v, want already revoked", err)
	}
}

// The same engine on another swarm node is a host that left the swarm and joined
// again: its earlier agent belongs to a node that is gone, which the reaper revokes
// as left_swarm after two node lists. The enrolment matches the node too, so it does
// not take that decision away from the reaper.
func TestTheSameEngineOnAnotherSwarmNodeIsLeftToTheReaper(t *testing.T) {
	r := newRig(t)
	s := newSwarmEnv(t, r, "prod")
	r.enrolFacts(t, s.c, worker("ENG:W2-prod", "node-w2-rejoined", "wrk-2"))
	if a := r.agentRow(t, s.w2.id); a.status != "active" {
		t.Fatalf("the agent of the old node is %+v, want it left active for the reaper", a)
	}
	if rows := auditRows(t, r.st, "agent.revoked"); len(rows) != 0 {
		t.Errorf("agent.revoked rows = %v, want none", rows)
	}
}

// liveSet is a LiveSessions that says the agents in it are connected.
type liveSet map[string]bool

func (l liveSet) Live(agentID string) bool { return l[agentID] }

// Review 17-2 WR-02 and WR-03: an earlier agent of the host that is connected now is
// never retired. A second agent on one host (a leftover container, a cloned VM) is
// refused host_already_connected, and nothing of its enrolment is written, so the two
// do not replace each other every minute; a spoofed enrolment that claims a connected
// host's engine and node cannot take its place either.
func TestAnEnrolmentOfAHostWhoseEarlierAgentIsConnectedIsRefusedAndRetiresNothing(t *testing.T) {
	for name, f := range map[string]wire.Facts{
		"standalone": standalone("ENG:EDGE-1", "edge-1"),
		"swarm node": worker("ENG:W2-prod", "node-w2-prod", "wrk-2"),
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			k := &kicks{st: r.st}
			n := &notices{}
			live := liveSet{}
			r.svc.WithKicker(k).WithNotifier(n).WithLive(live)
			c := r.create(t, "edge")
			first := r.enrolFacts(t, c, f)
			live[first.id] = true
			agentsBefore := count(t, r.st, `SELECT COUNT(*) FROM agents WHERE env_id = ?`, c.EnvID)
			enrolledBefore := len(auditRows(t, r.st, "agent.enrolled"))

			_, err := r.enrolWith(t, c.JoinToken, f)
			ref := refusal(t, err)
			if ref.Code != wire.RefuseHostAlreadyConnected || ref.AuditReason != wire.RefuseHostAlreadyConnected ||
				!strings.Contains(ref.Detail, "connected") || !strings.Contains(ref.Detail, "remove the duplicate") {
				t.Fatalf("refusal = %+v, want host_already_connected telling the operator to remove the duplicate agent", ref)
			}
			// The cloned VM is the other usual cause: the Detail names it and the fix.
			for _, want := range []string{"cloned", "engine ID", "engine-id", "its own environment"} {
				if !strings.Contains(ref.Detail, want) {
					t.Errorf("the detail %q lacks %q, the cloned-VM case and its fix", ref.Detail, want)
				}
			}
			// The copied agent goes first: it holds the original's identity and would start with Docker.
			d := ref.Detail
			if rm, stop, inst := strings.Index(d, "first remove the copied agent"), strings.Index(d, "stop Docker"), strings.Index(d, "install the agent fresh"); rm < 0 || stop < rm || inst < stop {
				t.Errorf("the detail %q does not give the clone steps in order", d)
			}
			if got := count(t, r.st, `SELECT COUNT(*) FROM agents WHERE env_id = ?`, c.EnvID); got != agentsBefore {
				t.Errorf("agent rows = %d, want %d: a refused enrolment writes no row", got, agentsBefore)
			}
			if a := r.agentRow(t, first.id); a.status != "active" || a.reason != "" || a.revokedBy != "" {
				t.Errorf("the connected agent is %+v, want it untouched", a)
			}
			if rows := auditRows(t, r.st, "agent.revoked"); len(rows) != 0 {
				t.Errorf("agent.revoked rows = %v, want none", rows)
			}
			if got := len(auditRows(t, r.st, "agent.enrolled")); got != enrolledBefore {
				t.Errorf("agent.enrolled rows = %d, want %d", got, enrolledBefore)
			}
			if len(k.agents) != 0 || len(n.list()) != 0 {
				t.Errorf("kicked %v and notified %v, want neither", k.agents, n.list())
			}
			if _, err := r.svc.Hello(r.ctx, first.hello(first.cert, first.priv)); err != nil {
				t.Errorf("hello of the connected agent: %v", err)
			}

			// Once the earlier agent is gone, the host's next enrolment retires it as before.
			live[first.id] = false
			second := r.enrolFacts(t, c, f)
			if a := r.agentRow(t, first.id); a.status != "revoked" || a.reason != "replaced" {
				t.Errorf("the earlier agent after it went offline is %+v, want revoked as replaced", a)
			}
			if a := r.agentRow(t, second.id); a.status != "active" {
				t.Errorf("the new agent is %+v, want active", a)
			}
		})
	}
}

// An earlier agent of the host that is not connected is retired as before, also with
// a live-session check in place; another host's connected agent changes nothing.
func TestAnOfflineEarlierAgentIsStillRetiredWhenOtherAgentsAreConnected(t *testing.T) {
	r := newRig(t)
	s := newSwarmEnv(t, r, "prod")
	r.svc.WithLive(liveSet{s.m.id: true, s.w1.id: true}) // w2 is offline
	again := r.enrolFacts(t, s.c, worker("ENG:W2-prod", "node-w2-prod", "wrk-2"))
	if a := r.agentRow(t, s.w2.id); a.status != "revoked" || a.reason != "replaced" {
		t.Fatalf("the node's offline agent is %+v, want revoked as replaced", a)
	}
	for name, id := range map[string]string{"manager": s.m.id, "first worker": s.w1.id, "new agent": again.id} {
		if a := r.agentRow(t, id); a.status != "active" {
			t.Errorf("the %s is %+v, want active", name, a)
		}
	}
	if len(s.kicked.agents) != 1 || s.kicked.agents[0] != s.w2.id {
		t.Errorf("kicked %v, want only the retired agent", s.kicked.agents)
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package agents_test

import (
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ianglennon/tierplan/internal/agents"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/tunnel"
)

// Revoke on leave (plan 02-20, ENV-09, D-10, T-02-68, RESEARCH Pitfall 12): an
// agent whose node is gone from two consecutive successful node lists is revoked by
// the reaper, with no block, and a node that is only down or drained keeps its agent.

func nodeRow(id string) dockerx.NodeRow {
	return dockerx.NodeRow{ID: id, Hostname: id, Role: "worker", Availability: "active", State: "ready"}
}

func nodes(rows ...dockerx.NodeRow) dockerx.ClusterRead { return dockerx.ClusterRead{Nodes: rows} }

// swarmEnv is a swarm of one manager and two workers, enrolled.
type swarmEnv struct {
	c          agents.Created
	m, w1, w2  member
	reaper     *agents.Reaper
	kicked     *kicks
	notified   *notices
	managerRow dockerx.NodeRow
}

func newSwarmEnv(t *testing.T, r *rig, name string) *swarmEnv {
	t.Helper()
	s := &swarmEnv{c: r.create(t, name), kicked: &kicks{st: r.st}, notified: &notices{}}
	r.svc.WithKicker(s.kicked).WithNotifier(s.notified)
	s.m = r.enrolFacts(t, s.c, manager("ENG:M-"+name, "node-m-"+name, "CL-"+name, "mgr-1"))
	s.w1 = r.enrolFacts(t, s.c, worker("ENG:W1-"+name, "node-w1-"+name, "wrk-1"))
	s.w2 = r.enrolFacts(t, s.c, worker("ENG:W2-"+name, "node-w2-"+name, "wrk-2"))
	s.reaper = agents.NewReaper(r.svc)
	s.managerRow = dockerx.NodeRow{ID: "node-m-" + name, Hostname: "mgr-1", Role: "manager", Leader: true, Availability: "active", State: "ready", Reachability: "reachable"}
	return s
}

// list is a node list with the manager and the named workers.
func (s *swarmEnv) list(name string, workers ...string) dockerx.ClusterRead {
	rows := []dockerx.NodeRow{s.managerRow}
	for _, w := range workers {
		rows = append(rows, nodeRow("node-"+w+"-"+name))
	}
	return nodes(rows...)
}

type agentRow struct {
	status, revokedBy, reason string
}

func (r *rig) agentRow(t *testing.T, id string) agentRow {
	t.Helper()
	var by, reason sql.NullString
	var a agentRow
	if err := r.st.Read.QueryRow(`SELECT status, revoked_by, revoke_reason FROM agents WHERE id = ?`, id).Scan(&a.status, &by, &reason); err != nil {
		t.Fatal(err)
	}
	a.revokedBy, a.reason = by.String, reason.String
	return a
}

func TestReaperOneMissIsNotARevokeAndTwoConsecutiveMissesAreWithNoBlockAndAnAuditRow(t *testing.T) {
	r := newRig(t)
	s := newSwarmEnv(t, r, "prod")
	missing := s.list("prod", "w1") // w2 has left

	s.reaper.Observe(r.ctx, s.c.EnvID, missing, s.managerRow.ID)
	if a := r.agentRow(t, s.w2.id); a.status != "active" {
		t.Fatalf("after one miss the agent is %+v, want active", a)
	}
	if len(s.kicked.agents) != 0 {
		t.Fatalf("one miss closed sessions: %v", s.kicked.agents)
	}

	s.reaper.Observe(r.ctx, s.c.EnvID, missing, s.managerRow.ID)
	a := r.agentRow(t, s.w2.id)
	if a.status != "revoked" || a.revokedBy != "system:agent-reaper" || a.reason == "" {
		t.Fatalf("after two misses the agent is %+v, want revoked by system:agent-reaper with a reason", a)
	}
	for name, id := range map[string]string{"manager": s.m.id, "first worker": s.w1.id} {
		if a := r.agentRow(t, id); a.status != "active" {
			t.Errorf("the %s was revoked as well: %+v", name, a)
		}
	}

	if n := count(t, r.st, `SELECT COUNT(*) FROM agent_blocks WHERE env_id = ?`, s.c.EnvID); n != 0 {
		t.Errorf("the auto-revoke made %d block rows, want none: the host may rejoin", n)
	}
	if k := s.kicked; len(k.agents) != 1 || k.agents[0] != s.w2.id || k.codes[0] != tunnel.DisconnectRevoked || k.statusAtKick != "revoked" {
		t.Errorf("sessions closed = %v %v (row was %q at the time), want the revoked agent's, as revoked, after the commit", k.agents, k.codes, k.statusAtKick)
	}
	if got := s.notified.list(); len(got) != 1 || got[0] != s.c.EnvID {
		t.Errorf("notified %v, want the environment once", got)
	}

	if rows := auditRows(t, r.st, "agent.auto_revoked"); len(rows) != 1 {
		t.Fatalf("agent.auto_revoked rows = %v, want one", rows)
	}
	var actorType, actorID, target string
	if err := r.st.Read.QueryRow(`SELECT actor_type, actor_id, target_id FROM audit_log WHERE action = 'agent.auto_revoked'`).Scan(&actorType, &actorID, &target); err != nil ||
		actorType != "system" || actorID != "system:agent-reaper" || target != s.w2.id {
		t.Errorf("actor %s/%s target %s, %v; want system:agent-reaper and the agent", actorType, actorID, target, err)
	}
	for _, other := range []string{"agent.revoked", "agent.blocked"} {
		if rows := auditRows(t, r.st, other); len(rows) != 0 {
			t.Errorf("%s rows = %v, want none for an automatic revoke", other, rows)
		}
	}

	// A third list does nothing more.
	s.reaper.Observe(r.ctx, s.c.EnvID, missing, s.managerRow.ID)
	if rows := auditRows(t, r.st, "agent.auto_revoked"); len(rows) != 1 {
		t.Errorf("a third list revoked again: %v", rows)
	}
}

func TestReaperASightingBetweenTwoMissesStartsTheCountAgain(t *testing.T) {
	r := newRig(t)
	s := newSwarmEnv(t, r, "prod")
	missing, present := s.list("prod", "w1"), s.list("prod", "w1", "w2")
	for i, l := range []dockerx.ClusterRead{missing, present, missing, present, missing} {
		s.reaper.Observe(r.ctx, s.c.EnvID, l, s.managerRow.ID)
		if a := r.agentRow(t, s.w2.id); a.status != "active" {
			t.Fatalf("after list %d the agent is %+v, want active: the misses were not consecutive", i, a)
		}
	}
	s.reaper.Observe(r.ctx, s.c.EnvID, missing, s.managerRow.ID)
	if a := r.agentRow(t, s.w2.id); a.status != "revoked" {
		t.Fatalf("two misses in a row at last: %+v, want revoked", a)
	}
}

func TestReaperKeepsTheAgentOfANodeThatIsDownDrainedOrPaused(t *testing.T) {
	r := newRig(t)
	s := newSwarmEnv(t, r, "prod")
	for name, mutate := range map[string]func(*dockerx.NodeRow){
		"down":    func(n *dockerx.NodeRow) { n.State = "down" },
		"drained": func(n *dockerx.NodeRow) { n.Availability = "drain" },
		"paused":  func(n *dockerx.NodeRow) { n.Availability = "pause" },
		"unknown": func(n *dockerx.NodeRow) { n.State = "unknown" },
	} {
		w2 := nodeRow("node-w2-prod")
		mutate(&w2)
		l := nodes(s.managerRow, nodeRow("node-w1-prod"), w2)
		for range 5 {
			s.reaper.Observe(r.ctx, s.c.EnvID, l, s.managerRow.ID)
		}
		if a := r.agentRow(t, s.w2.id); a.status != "active" {
			t.Fatalf("a node that is %s lost its agent: %+v", name, a)
		}
	}
	if rows := auditRows(t, r.st, "agent.auto_revoked"); len(rows) != 0 {
		t.Fatalf("auto-revoke rows = %v, want none", rows)
	}
}

func TestReaperIgnoresAListThatDoesNotIncludeTheAnsweringManager(t *testing.T) {
	r := newRig(t)
	s := newSwarmEnv(t, r, "prod")
	bogus := nodes(nodeRow("node-w1-prod")) // the manager that answered is not in its own list
	// A forged list that leaves everyone out, however often it is sent, revokes nobody.
	for range 6 {
		s.reaper.Observe(r.ctx, s.c.EnvID, bogus, s.managerRow.ID)
	}
	for _, id := range []string{s.m.id, s.w1.id, s.w2.id} {
		if a := r.agentRow(t, id); a.status != "active" {
			t.Fatalf("a list that left out its own answerer revoked %s: %+v", id, a)
		}
	}
	// An empty list, and no answerer, are no list either.
	s.reaper.Observe(r.ctx, s.c.EnvID, dockerx.ClusterRead{}, s.managerRow.ID)
	s.reaper.Observe(r.ctx, s.c.EnvID, dockerx.ClusterRead{}, "")
	s.reaper.Observe(r.ctx, s.c.EnvID, s.list("prod", "w1"), "")
	s.reaper.Observe(r.ctx, s.c.EnvID, s.list("prod", "w1"), "")
	if a := r.agentRow(t, s.w2.id); a.status != "active" {
		t.Fatalf("lists with no answerer revoked an agent: %+v", a)
	}

	// And one that is ignored neither counts nor resets: miss, ignored, miss.
	s.reaper.Observe(r.ctx, s.c.EnvID, s.list("prod", "w1"), s.managerRow.ID)
	s.reaper.Observe(r.ctx, s.c.EnvID, bogus, s.managerRow.ID)
	s.reaper.Observe(r.ctx, s.c.EnvID, s.list("prod", "w1"), s.managerRow.ID)
	if a := r.agentRow(t, s.w2.id); a.status != "revoked" {
		t.Fatalf("two real misses around an ignored list: %+v, want revoked", a)
	}
}

func TestReaperNeverTouchesAnAdminRevokedAgentAStandaloneHostOrAnotherEnvironment(t *testing.T) {
	r := newRig(t)
	s := newSwarmEnv(t, r, "prod")
	other := newSwarmEnv(t, r, "lab")
	if err := r.svc.Revoke(r.ctx, ops, s.c.EnvID, s.w1.id, "by hand", "", ""); err != nil {
		t.Fatal(err)
	}
	blocksBefore := count(t, r.st, `SELECT COUNT(*) FROM agent_blocks WHERE env_id = ?`, s.c.EnvID)
	standaloneEnv := r.create(t, "edge")
	edge := r.enrolFacts(t, standaloneEnv, standalone("ENG:EDGE", "edge-1"))

	// prod's list names neither worker; the lab swarm is not in it at all.
	l := nodes(s.managerRow)
	for range 4 {
		s.reaper.Observe(r.ctx, s.c.EnvID, l, s.managerRow.ID)
		// A list for the standalone environment is meaningless: it has no swarm node.
		s.reaper.Observe(r.ctx, standaloneEnv.EnvID, nodes(nodeRow("node-x")), "node-x")
	}
	if a := r.agentRow(t, s.w1.id); a.revokedBy != "u-ops" || a.reason != "by hand" {
		t.Errorf("the admin's revoke was overwritten: %+v", a)
	}
	if a := r.agentRow(t, s.w2.id); a.revokedBy != "system:agent-reaper" {
		t.Errorf("the missing worker was not auto-revoked: %+v", a)
	}
	if n := count(t, r.st, `SELECT COUNT(*) FROM agent_blocks WHERE env_id = ?`, s.c.EnvID); n != blocksBefore {
		t.Errorf("block rows went from %d to %d", blocksBefore, n)
	}
	for name, id := range map[string]string{"the standalone host": edge.id, "the other swarm's manager": other.m.id, "the other swarm's worker": other.w1.id, "the other swarm's second worker": other.w2.id} {
		if a := r.agentRow(t, id); a.status != "active" {
			t.Errorf("%s was revoked: %+v", name, a)
		}
	}
	if rows := auditRows(t, r.st, "agent.auto_revoked"); len(rows) != 1 {
		t.Errorf("agent.auto_revoked rows = %v, want exactly the one worker", rows)
	}
}

func TestReaperAnAutoRevokedHostEnrolsAgainWithTheJoinToken(t *testing.T) {
	r := newRig(t)
	s := newSwarmEnv(t, r, "prod")
	missing := s.list("prod", "w1")
	s.reaper.Observe(r.ctx, s.c.EnvID, missing, s.managerRow.ID)
	s.reaper.Observe(r.ctx, s.c.EnvID, missing, s.managerRow.ID)
	if a := r.agentRow(t, s.w2.id); a.status != "revoked" {
		t.Fatalf("setup: %+v", a)
	}

	// The same host rejoins the swarm and runs the install command again.
	again := r.enrolFacts(t, s.c, worker("ENG:W2-prod", "node-w2-prod", "wrk-2"))
	if again.id == s.w2.id {
		t.Fatal("the host enrolled as the revoked agent")
	}
	if a := r.agentRow(t, again.id); a.status != "active" {
		t.Fatalf("the rejoined host is %+v, want a new active agent", a)
	}
}

func TestReaperAutoRevokeOfAnAlreadyRevokedOrUnknownAgentChangesNothing(t *testing.T) {
	r := newRig(t)
	s := newSwarmEnv(t, r, "prod")
	if err := r.svc.AutoRevoke(r.ctx, s.c.EnvID, s.w2.id); err != nil {
		t.Fatalf("AutoRevoke: %v", err)
	}
	if err := r.svc.AutoRevoke(r.ctx, s.c.EnvID, s.w2.id); !errors.Is(err, agents.ErrAlreadyRevoked) {
		t.Errorf("a second AutoRevoke = %v, want ErrAlreadyRevoked", err)
	}
	if err := r.svc.AutoRevoke(r.ctx, s.c.EnvID, "no-such-agent"); !errors.Is(err, agents.ErrAgentNotFound) {
		t.Errorf("AutoRevoke of an unknown agent = %v, want ErrAgentNotFound", err)
	}
	if err := r.svc.AutoRevoke(r.ctx, "no-such-env", s.w1.id); !errors.Is(err, agents.ErrEnvironmentNotFound) {
		t.Errorf("AutoRevoke in an unknown environment = %v, want ErrEnvironmentNotFound", err)
	}
	if rows := auditRows(t, r.st, "agent.auto_revoked"); len(rows) != 1 {
		t.Errorf("agent.auto_revoked rows = %v, want one", rows)
	}
}

func TestReaperObservingFromManyGoroutinesPassesTheRaceDetectorAndRevokesOnce(t *testing.T) {
	r := newRig(t)
	s := newSwarmEnv(t, r, "prod")
	missing := s.list("prod", "w1")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 4 {
				s.reaper.Observe(r.ctx, s.c.EnvID, missing, s.managerRow.ID)
			}
		}()
	}
	wg.Wait()
	if a := r.agentRow(t, s.w2.id); a.status != "revoked" {
		t.Fatalf("agent = %+v, want revoked", a)
	}
	if rows := auditRows(t, r.st, "agent.auto_revoked"); len(rows) != 1 {
		t.Errorf("agent.auto_revoked rows = %v, want exactly one", rows)
	}
}

// An enrolment whose node no fresh list had shown leaves the earlier row of the node active
// (Service.Enrol, unverified). The first list that shows the node retires the older row that
// is not connected: until then the oldest active row would stay the node's agent for routing.

// unverifiedDuplicate is a swarm of three whose worker 2 enrolled again with no node list held,
// so it has two active rows. The provider stays unset for the list: none is held.
func unverifiedDuplicate(t *testing.T) (*rig, *swarmEnv, member) {
	t.Helper()
	r := newRig(t)
	s := newSwarmEnv(t, r, "prod")
	r.svc.WithNodeLists(&fakeNodeLists{})
	again := r.enrolFacts(t, s.c, worker("ENG:W2-prod", "node-w2-prod", "wrk-2"))
	if a := r.agentRow(t, s.w2.id); a.status != "active" {
		t.Fatalf("the unverified enrolment retired the earlier row: %+v", a)
	}
	return r, s, again
}

func TestReaperAListThatShowsTheNodeRetiresTheOlderOfflineDuplicateAndKicksIt(t *testing.T) {
	r, s, again := unverifiedDuplicate(t)

	s.reaper.Observe(r.ctx, s.c.EnvID, s.list("prod", "w1", "w2"), s.managerRow.ID)

	a := r.agentRow(t, s.w2.id)
	if a.status != "revoked" || a.reason != "replaced" || a.revokedBy != "system:agent-reaper" {
		t.Fatalf("the older row is %+v, want revoked as replaced by system:agent-reaper", a)
	}
	if a := r.agentRow(t, again.id); a.status != "active" {
		t.Errorf("the newest row is %+v, want it active", a)
	}
	rows := auditRows(t, r.st, "agent.revoked")
	if len(rows) != 1 || !strings.Contains(rows[0], `"replaced_by":"`+again.id+`"`) || !strings.Contains(rows[0], `"source":"node_list"`) ||
		!strings.Contains(rows[0], `"reason":"replaced"`) || !strings.Contains(rows[0], `"swarm_node":"node-w2-prod"`) {
		t.Fatalf("agent.revoked rows = %v, want one naming the newest row, the reason replaced and source=node_list", rows)
	}
	if k := s.kicked; len(k.agents) != 1 || k.agents[0] != s.w2.id || k.statusAtKick != "revoked" {
		t.Errorf("sessions closed = %v (row was %q), want the older row's, after the commit", k.agents, k.statusAtKick)
	}
	if n := count(t, r.st, `SELECT COUNT(*) FROM agent_blocks WHERE env_id = ?`, s.c.EnvID); n != 0 {
		t.Errorf("the retire made %d block rows, want none", n)
	}
	if got := activeAgentsOf(t, r, s.c.EnvID); got != 3 {
		t.Errorf("active agents = %d, want 3: one per node", got)
	}
	// The node's agent for routing is the newest now.
	if id, err := r.svc.ActiveAgentOfSwarmNode(r.ctx, s.c.EnvID, "node-w2-prod"); err != nil || id != again.id {
		t.Errorf("the agent of the node is %q, %v; want the newest row %s", id, err, again.id)
	}

	// The next list finds nothing to retire.
	s.reaper.Observe(r.ctx, s.c.EnvID, s.list("prod", "w1", "w2"), s.managerRow.ID)
	if rows := auditRows(t, r.st, "agent.revoked"); len(rows) != 1 {
		t.Errorf("a second list retired again: %v", rows)
	}
}

func TestReaperAListLeavesAnOlderDuplicateThatIsConnected(t *testing.T) {
	r, s, again := unverifiedDuplicate(t)
	r.svc.WithLive(liveSet{s.w2.id: true})

	s.reaper.Observe(r.ctx, s.c.EnvID, s.list("prod", "w1", "w2"), s.managerRow.ID)

	if a := r.agentRow(t, s.w2.id); a.status != "active" {
		t.Fatalf("the connected older row is %+v, want it left", a)
	}
	if a := r.agentRow(t, again.id); a.status != "active" {
		t.Errorf("the newest row is %+v, want it active", a)
	}
	if rows := auditRows(t, r.st, "agent.revoked"); len(rows) != 0 {
		t.Errorf("agent.revoked rows = %v, want none", rows)
	}
}

func TestReaperANodeMissingFromTheListIsOnlyCountedNotRetiredAsADuplicate(t *testing.T) {
	r, s, again := unverifiedDuplicate(t)

	s.reaper.Observe(r.ctx, s.c.EnvID, s.list("prod", "w1"), s.managerRow.ID) // w2 is not listed
	for name, id := range map[string]string{"older": s.w2.id, "newest": again.id} {
		if a := r.agentRow(t, id); a.status != "active" {
			t.Errorf("after one miss the %s row is %+v, want active", name, a)
		}
	}
	if rows := auditRows(t, r.st, "agent.revoked"); len(rows) != 0 {
		t.Errorf("agent.revoked rows = %v, want none for a node the list lacks", rows)
	}
}

func TestReaperASingleRowPerNodeRetiresNothing(t *testing.T) {
	r := newRig(t)
	s := newSwarmEnv(t, r, "prod")
	s.reaper.Observe(r.ctx, s.c.EnvID, s.list("prod", "w1", "w2"), s.managerRow.ID)
	if rows := auditRows(t, r.st, "agent.revoked"); len(rows) != 0 {
		t.Errorf("agent.revoked rows = %v, want none", rows)
	}
	if got := activeAgentsOf(t, r, s.c.EnvID); got != 3 {
		t.Errorf("active agents = %d, want 3", got)
	}
}

func TestReaperAListThatNamesNoAnswererRetiresNoDuplicate(t *testing.T) {
	r, s, _ := unverifiedDuplicate(t)
	s.reaper.Observe(r.ctx, s.c.EnvID, s.list("prod", "w1", "w2"), "")
	if a := r.agentRow(t, s.w2.id); a.status != "active" {
		t.Errorf("a list with no answerer retired the older row: %+v", a)
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package agents_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/tunnel"
	"github.com/ianglennon/tierplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/tierplan/proto/wire"
)

// The swarm node an enrolling agent reports is a claim of a join-token holder. The service
// checks it against the node list a manager last gave (Service.WithNodeLists): a fresh list
// that lacks the node refuses the enrolment, a node no fresh list has shown is accepted but
// retires no earlier agent, and a listed node retires them as before (17-REVIEW-2 WR-03).

// nodeList is what a fake provider holds for one environment.
type nodeList struct {
	ids []string
	at  time.Time
	ok  bool
}

// fakeNodeLists is a NodeLists with lists a test sets, and a count of the questions asked. A
// refresh counts itself and, when next holds a list for the environment, makes it the held one.
type fakeNodeLists struct {
	mu        sync.Mutex
	lists     map[string]nodeList
	next      map[string]nodeList
	asked     int
	refreshes int
	minAge    time.Duration
}

func (f *fakeNodeLists) RefreshSwarmNodeIDs(ctx context.Context, envID string, minAge time.Duration) ([]string, time.Time, bool) {
	f.mu.Lock()
	f.refreshes++
	f.minAge = minAge
	if l, ok := f.next[envID]; ok {
		f.lists[envID] = l
	}
	if _, ok := ctx.Deadline(); !ok {
		f.minAge = -1 // a refresh without a deadline is a test failure, seen through minAge
	}
	f.mu.Unlock()
	return f.SwarmNodeIDs(envID)
}

func (f *fakeNodeLists) refreshed() (int, time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshes, f.minAge
}

func (f *fakeNodeLists) set(envID string, l nodeList) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lists == nil {
		f.lists = map[string]nodeList{}
	}
	f.lists[envID] = l
}

func (f *fakeNodeLists) SwarmNodeIDs(envID string) ([]string, time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked++
	l := f.lists[envID]
	return l.ids, l.at, l.ok
}

func (f *fakeNodeLists) questions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked
}

// freshList is a list read just now that holds the nodes of the swarm env of newSwarmEnv.
func freshList(r *rig, name string, extra ...string) nodeList {
	ids := append([]string{"node-m-" + name, "node-w1-" + name, "node-w2-" + name}, extra...)
	return nodeList{ids: ids, at: r.clock, ok: true}
}

// swarmWithProvider is a swarm of one manager and two workers, enrolled before any provider is
// set, and then a provider that holds the list given.
func swarmWithProvider(t *testing.T, l func(r *rig) nodeList) (*rig, *swarmEnv, *fakeNodeLists) {
	t.Helper()
	r := newRig(t)
	s := newSwarmEnv(t, r, "prod")
	p := &fakeNodeLists{}
	if l != nil {
		p.set(s.c.EnvID, l(r))
	}
	r.svc.WithNodeLists(p)
	return r, s, p
}

func enrolledDetail(t *testing.T, r *rig, agentID string) string {
	t.Helper()
	var d string
	if err := r.st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = 'agent.enrolled' AND target_id = ?`, agentID).Scan(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestNodeCheckAFreshListWithTheNodeEnrolsAndRetiresTheEarlierOfflineRow(t *testing.T) {
	r, s, p := swarmWithProvider(t, func(r *rig) nodeList { return freshList(r, "prod") })
	again := r.enrolFacts(t, s.c, worker("ENG:W2-prod", "node-w2-prod", "wrk-2"))

	if a := r.agentRow(t, s.w2.id); a.status != "revoked" || a.reason != "replaced" || a.revokedBy != "system:agent-enrol" {
		t.Fatalf("the earlier row of the node is %+v, want revoked as replaced by system:agent-enrol", a)
	}
	if a := r.agentRow(t, again.id); a.status != "active" {
		t.Fatalf("the new agent is %+v, want active", a)
	}
	if d := enrolledDetail(t, r, again.id); !strings.Contains(d, `"node_check":"listed"`) {
		t.Errorf("agent.enrolled detail = %s, want node_check=listed", d)
	}
	if p.questions() == 0 {
		t.Error("the provider was never asked")
	}
}

func TestNodeCheckAFreshListWithoutTheNodeRefusesAndWritesNothing(t *testing.T) {
	r, s, _ := swarmWithProvider(t, func(r *rig) nodeList { return freshList(r, "prod") })
	agentsBefore := count(t, r.st, `SELECT COUNT(*) FROM agents WHERE env_id = ?`, s.c.EnvID)
	enrolledBefore := len(auditRows(t, r.st, "agent.enrolled"))

	_, err := r.enrolWith(t, s.c.JoinToken, worker("ENG:W9-prod", "node-w9-prod", "wrk-9"))
	ref := refusal(t, err)
	if ref.Code != wire.RefuseNodeNotInSwarm || ref.AuditReason != wire.RefuseNodeNotInSwarm {
		t.Fatalf("refusal = %+v, want node_not_in_swarm with the same audit reason", ref)
	}
	for _, leak := range []string{"node-w9-prod", "wrk-9", "ENG:W9-prod"} {
		if strings.Contains(ref.Detail, leak) {
			t.Errorf("the detail %q echoes %q", ref.Detail, leak)
		}
	}
	if got := count(t, r.st, `SELECT COUNT(*) FROM agents WHERE env_id = ?`, s.c.EnvID); got != agentsBefore {
		t.Errorf("agent rows = %d, want %d: a refused enrolment writes no row", got, agentsBefore)
	}
	if got := len(auditRows(t, r.st, "agent.enrolled")); got != enrolledBefore {
		t.Errorf("agent.enrolled rows = %d, want %d", got, enrolledBefore)
	}
	if rows := auditRows(t, r.st, "agent.revoked"); len(rows) != 0 {
		t.Errorf("agent.revoked rows = %v, want none", rows)
	}
}

func TestNodeCheckARefusedClaimLeavesTheEarlierRowOfThatEnginesNodeAlone(t *testing.T) {
	// The claimed node is a real node's, with another node's engine: the list lacks the claim.
	r, s, _ := swarmWithProvider(t, func(r *rig) nodeList { return freshList(r, "prod") })
	_, err := r.enrolWith(t, s.c.JoinToken, worker("ENG:W2-prod", "node-w2-elsewhere", "wrk-2"))
	if ref := refusal(t, err); ref.Code != wire.RefuseNodeNotInSwarm {
		t.Fatalf("refusal = %+v, want node_not_in_swarm", ref)
	}
	if a := r.agentRow(t, s.w2.id); a.status != "active" {
		t.Errorf("the earlier agent is %+v, want it untouched", a)
	}
	if rows := auditRows(t, r.st, "agent.revoked"); len(rows) != 0 {
		t.Errorf("agent.revoked rows = %v, want none", rows)
	}
}

// The hub turns the refusal away and audits it as agent.enrol_refused with the reason.
func TestNodeCheckTheHubAuditsTheRefusalAsEnrolRefusedWithTheReason(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{AuditRefusals: true})
	env := srv.NewEnvironment(t, "prod")
	p := &fakeNodeLists{}
	p.set(env.ID, nodeList{ids: []string{"node-m1"}, at: srv.Clock.Now(), ok: true})
	srv.Svc.WithNodeLists(p)

	f := worker("ENG:W9", "node-w9", "wrk-9")
	c := srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: &f})
	if c.Refuse == nil || c.Refuse.Code != wire.RefuseNodeNotInSwarm {
		t.Fatalf("refuse = %+v (err %v), want node_not_in_swarm", c.Refuse, c.Err)
	}
	var reason string
	if err := srv.Store.Read.QueryRow(`SELECT reason FROM audit_log WHERE action = 'agent.enrol_refused' ORDER BY seq DESC LIMIT 1`).Scan(&reason); err != nil || reason != wire.RefuseNodeNotInSwarm {
		t.Fatalf("audit reason = %q, %v, want node_not_in_swarm", reason, err)
	}
	if n := count(t, srv.Store, `SELECT COUNT(*) FROM agents WHERE env_id = ?`, env.ID); n != 0 {
		t.Errorf("%d agent rows, want none", n)
	}
}

func TestNodeCheckAListThatIsTooOldOrNotHeldEnrolsUnverifiedAndRetiresNothing(t *testing.T) {
	for name, l := range map[string]func(r *rig) nodeList{
		"older than two minutes": func(r *rig) nodeList {
			x := freshList(r, "prod")
			x.at = r.clock.Add(-2*time.Minute - time.Second)
			x.ids = []string{"node-other"} // would refuse if it were believed
			return x
		},
		"none held": func(r *rig) nodeList { return nodeList{} },
		"not ok":    func(r *rig) nodeList { return nodeList{ids: []string{"node-w2-prod"}, at: r.clock, ok: false} },
	} {
		t.Run(name, func(t *testing.T) {
			r, s, _ := swarmWithProvider(t, l)
			again := r.enrolFacts(t, s.c, worker("ENG:W2-prod", "node-w2-prod", "wrk-2"))
			if a := r.agentRow(t, again.id); a.status != "active" {
				t.Fatalf("the new agent is %+v, want active", a)
			}
			if a := r.agentRow(t, s.w2.id); a.status != "active" {
				t.Fatalf("the earlier row is %+v, want it left active for the reaper", a)
			}
			if rows := auditRows(t, r.st, "agent.revoked"); len(rows) != 0 {
				t.Errorf("agent.revoked rows = %v, want none", rows)
			}
			if d := enrolledDetail(t, r, again.id); !strings.Contains(d, `"node_check":"unverified"`) {
				t.Errorf("agent.enrolled detail = %s, want node_check=unverified", d)
			}
			if len(s.kicked.agents) != 0 {
				t.Errorf("kicked %v, want none", s.kicked.agents)
			}
		})
	}
}

// A list exactly two minutes old still decides; one second more does not.
func TestNodeCheckAListOfExactlyTwoMinutesIsStillFresh(t *testing.T) {
	r, s, _ := swarmWithProvider(t, func(r *rig) nodeList {
		x := freshList(r, "prod")
		x.at = r.clock.Add(-2 * time.Minute)
		x.ids = []string{"node-other"}
		return x
	})
	_, err := r.enrolWith(t, s.c.JoinToken, worker("ENG:W9-prod", "node-w9-prod", "wrk-9"))
	if ref := refusal(t, err); ref.Code != wire.RefuseNodeNotInSwarm {
		t.Fatalf("refusal = %+v, want node_not_in_swarm from a list of exactly two minutes", ref)
	}
}

func TestNodeCheckAnUnverifiedEnrolmentWhoseEarlierAgentIsConnectedIsStillRefusedHostAlreadyConnected(t *testing.T) {
	r, s, _ := swarmWithProvider(t, nil) // no list is held
	r.svc.WithLive(liveSet{s.w2.id: true})
	_, err := r.enrolWith(t, s.c.JoinToken, worker("ENG:W2-prod", "node-w2-prod", "wrk-2"))
	ref := refusal(t, err)
	if ref.Code != wire.RefuseHostAlreadyConnected {
		t.Fatalf("refusal = %+v, want host_already_connected", ref)
	}
	if a := r.agentRow(t, s.w2.id); a.status != "active" {
		t.Errorf("the connected agent is %+v, want it untouched", a)
	}
}

func TestNodeCheckSwarmFactsWithAnEmptyNodeIDAreUnverifiedNeverRefusedForIt(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "prod")
	p := &fakeNodeLists{}
	p.set(c.EnvID, nodeList{ids: []string{"node-other"}, at: r.clock, ok: true})
	r.svc.WithNodeLists(p)

	m := r.enrolFacts(t, c, manager("ENG:M", "", "CL1", "mgr-1"))
	if d := enrolledDetail(t, r, m.id); !strings.Contains(d, `"node_check":"unverified"`) {
		t.Errorf("agent.enrolled detail = %s, want node_check=unverified", d)
	}
	if p.questions() != 0 {
		t.Errorf("the provider was asked %d times about a node with no ID", p.questions())
	}
}

func TestNodeCheckAStandaloneHostDoesNotConsultTheProviderAndRetiresByEngineID(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "edge")
	p := &fakeNodeLists{}
	r.svc.WithNodeLists(p)
	host := standalone("ENG:EDGE-1", "edge-1")
	first := r.enrolFacts(t, c, host)
	second := r.enrolFacts(t, c, host)

	if p.questions() != 0 {
		t.Errorf("the provider was asked %d times for a standalone host", p.questions())
	}
	if a := r.agentRow(t, first.id); a.status != "revoked" || a.reason != "replaced" {
		t.Errorf("the earlier agent is %+v, want revoked as replaced", a)
	}
	if d := enrolledDetail(t, r, second.id); strings.Contains(d, "node_check") {
		t.Errorf("agent.enrolled detail = %s, want no node_check for a standalone host", d)
	}
}

func TestNodeCheckWithoutAProviderASwarmNodeIsNotCheckedAndRetiresAsBefore(t *testing.T) {
	r := newRig(t)
	s := newSwarmEnv(t, r, "prod")
	again := r.enrolFacts(t, s.c, worker("ENG:W2-prod", "node-w2-prod", "wrk-2"))
	if a := r.agentRow(t, s.w2.id); a.status != "revoked" || a.reason != "replaced" {
		t.Fatalf("the earlier row is %+v, want revoked as replaced", a)
	}
	if d := enrolledDetail(t, r, again.id); strings.Contains(d, "node_check") {
		t.Errorf("agent.enrolled detail = %s, want no node_check without a provider", d)
	}
}

// enrolVersion enrols an agent of agentVersion that reports facts.
func (r *rig) enrolVersion(t *testing.T, token, agentVersion string, facts wire.Facts) (tunnel.EnrolResult, error) {
	t.Helper()
	req := newProof(t, "h").request(token)
	req.Facts, req.AgentVersion = facts, agentVersion
	return r.svc.Enrol(r.ctx, req)
}

// An agent from before node_not_in_swarm would take that code for a refusal that does not pass
// and exit at its first enrolment (review 261113-ota WR-04). It is sent rate_limited, which
// every agent retries without exiting, with a time to wait; the audit reason stays the same.
func TestNodeCheckAnAgentThatDoesNotKnowTheCodeIsRefusedRateLimited(t *testing.T) {
	for version, wantCode := range map[string]string{
		"0.1.0":          wire.RefuseRateLimited,
		"v0.1.0":         wire.RefuseRateLimited,
		"0.0.9":          wire.RefuseRateLimited,
		"dev (abc1234)":  wire.RefuseRateLimited, // not a release: cannot be told apart
		"":               wire.RefuseRateLimited,
		"0.1.1":          wire.RefuseNodeNotInSwarm,
		"v0.1.1-rc.1":    wire.RefuseNodeNotInSwarm,
		"0.2.0":          wire.RefuseNodeNotInSwarm,
		"v1.0.0+build.7": wire.RefuseNodeNotInSwarm,
	} {
		t.Run(version, func(t *testing.T) {
			r, s, _ := swarmWithProvider(t, func(r *rig) nodeList { return freshList(r, "prod") })
			_, err := r.enrolVersion(t, s.c.JoinToken, version, worker("ENG:W9-prod", "node-w9-prod", "wrk-9"))
			ref := refusal(t, err)
			if ref.Code != wantCode || ref.AuditReason != wire.RefuseNodeNotInSwarm {
				t.Fatalf("refusal = %+v, want code %s with the audit reason node_not_in_swarm", ref, wantCode)
			}
			wantWait := time.Duration(0)
			if wantCode == wire.RefuseRateLimited {
				wantWait = 15 * time.Second
			}
			if ref.RetryAfter != wantWait {
				t.Errorf("retry after = %v, want %v", ref.RetryAfter, wantWait)
			}
		})
	}
}

// A list that lacks the node and is a few seconds old is read again once before a refusal, so a
// node that joined since is enrolled at its first try (review 261113-ota IN-03).
func TestNodeCheckAMissOnAListSecondsOldReadsItAgainOnce(t *testing.T) {
	r, s, p := swarmWithProvider(t, func(r *rig) nodeList {
		x := freshList(r, "prod")
		x.at = r.clock.Add(-10 * time.Second)
		return x
	})
	p.mu.Lock()
	p.next = map[string]nodeList{s.c.EnvID: freshList(r, "prod", "node-w9-prod")}
	p.mu.Unlock()

	m := r.enrolFacts(t, s.c, worker("ENG:W9-prod", "node-w9-prod", "wrk-9"))
	if d := enrolledDetail(t, r, m.id); !strings.Contains(d, `"node_check":"listed"`) {
		t.Errorf("agent.enrolled detail = %s, want node_check=listed from the list read again", d)
	}
	if n, minAge := p.refreshed(); n != 1 || minAge != 5*time.Second {
		t.Errorf("refreshes = %d with a minimum age of %v, want one of 5s with a deadline", n, minAge)
	}
}

// A list read again that still lacks the node refuses, and one read in the last seconds is not
// read again: enrolments cannot make the managers read their cluster more often than that.
func TestNodeCheckAMissIsReadAgainOnlyWhenTheListIsSecondsOld(t *testing.T) {
	for name, tc := range map[string]struct {
		age           time.Duration
		wantRefreshes int
	}{
		"a list read just now":      {0, 0},
		"a list read 4 seconds ago": {4 * time.Second, 0},
		"a list read 5 seconds ago": {5 * time.Second, 1},
	} {
		t.Run(name, func(t *testing.T) {
			r, s, p := swarmWithProvider(t, func(r *rig) nodeList {
				x := freshList(r, "prod")
				x.at = r.clock.Add(-tc.age)
				return x
			})
			_, err := r.enrolWith(t, s.c.JoinToken, worker("ENG:W9-prod", "node-w9-prod", "wrk-9"))
			if ref := refusal(t, err); ref.Code != wire.RefuseNodeNotInSwarm {
				t.Fatalf("refusal = %+v, want node_not_in_swarm", ref)
			}
			if n, _ := p.refreshed(); n != tc.wantRefreshes {
				t.Errorf("refreshes = %d, want %d", n, tc.wantRefreshes)
			}
		})
	}
}

// Through the hub an older agent gets rate_limited with the time to wait in the refuse message,
// and the audit row keeps the reason node_not_in_swarm.
func TestNodeCheckTheHubTellsAnOlderAgentRateLimitedWithTheWait(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{AuditRefusals: true})
	env := srv.NewEnvironment(t, "prod")
	p := &fakeNodeLists{}
	p.set(env.ID, nodeList{ids: []string{"node-m1"}, at: srv.Clock.Now(), ok: true})
	srv.Svc.WithNodeLists(p)

	f := worker("ENG:W9", "node-w9", "wrk-9")
	c := srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: &f, Version: "0.1.0"})
	if c.Refuse == nil || c.Refuse.Code != wire.RefuseRateLimited || c.Refuse.RetryAfterMs != 15000 {
		t.Fatalf("refuse = %+v (err %v), want rate_limited with a wait of 15000 ms", c.Refuse, c.Err)
	}
	var reason string
	if err := srv.Store.Read.QueryRow(`SELECT reason FROM audit_log WHERE action = 'agent.enrol_refused' ORDER BY seq DESC LIMIT 1`).Scan(&reason); err != nil || reason != wire.RefuseNodeNotInSwarm {
		t.Fatalf("audit reason = %q, %v, want node_not_in_swarm", reason, err)
	}
}

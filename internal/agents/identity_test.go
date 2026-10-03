// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package agents_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ianglennon/bayplan/internal/agents"
	"github.com/ianglennon/bayplan/internal/tunnel"
	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/bayplan/proto/authn"
	"github.com/ianglennon/bayplan/proto/wire"
)

func standalone(engine, host string) wire.Facts {
	return wire.Facts{EngineID: engine, Hostname: host, EngineReachable: true}
}

func worker(engine, node, host string) wire.Facts {
	return wire.Facts{
		EngineID: engine, Hostname: host, EngineReachable: true,
		Swarm: &wire.SwarmFacts{State: "active", NodeID: node, ManagerIDs: []string{"mgr-node"}},
	}
}

func manager(engine, node, cluster, host string) wire.Facts {
	return wire.Facts{
		EngineID: engine, Hostname: host, EngineReachable: true,
		Swarm: &wire.SwarmFacts{State: "active", NodeID: node, ClusterID: cluster, Control: true},
	}
}

// envState is what an environment's row says about its identity.
type envState struct {
	name, envType, engine, cluster, pending string
	nameAuto                                int
	changedAt                               sql.NullInt64
}

func (r *rig) state(t *testing.T, envID string) envState {
	t.Helper()
	var s envState
	var typ, eng, cl, pend sql.NullString
	err := r.st.Read.QueryRow(`SELECT name, name_auto, env_type, engine_id, swarm_cluster_id, pending_cluster_id, cluster_changed_at FROM environments WHERE id = ?`, envID).
		Scan(&s.name, &s.nameAuto, &typ, &eng, &cl, &pend, &s.changedAt)
	if err != nil {
		t.Fatal(err)
	}
	s.envType, s.engine, s.cluster, s.pending = typ.String, eng.String, cl.String, pend.String
	return s
}

// bind sets the identity columns of an environment directly.
func (r *rig) bind(t *testing.T, envID, envType, engine, cluster string) {
	t.Helper()
	if _, err := r.st.Write.Exec(`UPDATE environments SET env_type = NULLIF(?, ''), engine_id = NULLIF(?, ''), swarm_cluster_id = NULLIF(?, '') WHERE id = ?`,
		envType, engine, cluster, envID); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) markChanged(t *testing.T, envID, pending string) {
	t.Helper()
	if _, err := r.st.Write.Exec(`UPDATE environments SET cluster_changed_at = 1, pending_cluster_id = ? WHERE id = ?`, pending, envID); err != nil {
		t.Fatal(err)
	}
}

func TestCheckIdentityDecidesTypeBindingAndNameForEachKindOfHost(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, r *rig, envID string)
		facts wire.Facts
		// want is the refusal code, or "" for an accepted host; dec is checked when accepted.
		want string
		dec  agents.Decision
	}{
		{
			name:  "an empty environment and a standalone host: standalone, bound to its engine, named by its hostname",
			facts: standalone("ENG:X", "edge-1"),
			dec:   agents.Decision{EnvType: "standalone", EngineID: "ENG:X", Name: "edge-1"},
		},
		{
			name:  "an empty environment and a worker: swarm with no cluster, and the placeholder name stays",
			facts: worker("ENG:W", "node-w", "wrk-1"),
			dec:   agents.Decision{EnvType: "swarm"},
		},
		{
			name:  "an empty environment and a manager: swarm bound to its cluster, named by its hostname and swarm",
			facts: manager("ENG:M", "node-m", "CL1", "mgr-1"),
			dec:   agents.Decision{EnvType: "swarm", ClusterID: "CL1", Name: "mgr-1 swarm"},
		},
		{
			name:  "a swarm without a cluster and a manager: the cluster is bound, the type already is",
			setup: func(t *testing.T, r *rig, id string) { r.bind(t, id, "swarm", "", "") },
			facts: manager("ENG:M", "node-m", "CL1", "mgr-1"),
			dec:   agents.Decision{ClusterID: "CL1", Name: "mgr-1 swarm"},
		},
		{
			name:  "a swarm bound to a cluster and a manager of it: accepted, nothing to bind",
			setup: func(t *testing.T, r *rig, id string) { r.bind(t, id, "swarm", "", "CL1") },
			facts: manager("ENG:M", "node-m", "CL1", "mgr-2"),
			dec:   agents.Decision{Name: "mgr-2 swarm"},
		},
		{
			name:  "a swarm bound to a cluster and a worker: accepted whatever swarm it is in",
			setup: func(t *testing.T, r *rig, id string) { r.bind(t, id, "swarm", "", "CL1") },
			facts: worker("ENG:W", "node-w", "wrk-1"),
			dec:   agents.Decision{Name: ""},
		},
		{
			name:  "a swarm bound to a cluster and a manager of another: cluster_changed",
			setup: func(t *testing.T, r *rig, id string) { r.bind(t, id, "swarm", "", "CL1") },
			facts: manager("ENG:M", "node-m", "CL2", "mgr-9"),
			want:  wire.ReasonClusterChanged,
			dec:   agents.Decision{PendingCluster: "CL2", RecordChange: true},
		},
		{
			name: "a change already recorded is not recorded again",
			setup: func(t *testing.T, r *rig, id string) {
				r.bind(t, id, "swarm", "", "CL1")
				r.markChanged(t, id, "CL3")
			},
			facts: manager("ENG:M", "node-m", "CL2", "mgr-9"),
			want:  wire.ReasonClusterChanged,
			dec:   agents.Decision{PendingCluster: "CL2", RecordChange: false},
		},
		{
			name:  "a standalone environment and a swarm host: type_mismatch",
			setup: func(t *testing.T, r *rig, id string) { r.bind(t, id, "standalone", "ENG:X", "") },
			facts: manager("ENG:X", "node-m", "CL1", "mgr-1"),
			want:  wire.RefuseTypeMismatch,
		},
		{
			name:  "a swarm environment and a standalone host: type_mismatch",
			setup: func(t *testing.T, r *rig, id string) { r.bind(t, id, "swarm", "", "CL1") },
			facts: standalone("ENG:X", "edge-1"),
			want:  wire.RefuseTypeMismatch,
		},
		{
			name:  "a standalone environment bound to one engine and a different host: host_mismatch",
			setup: func(t *testing.T, r *rig, id string) { r.bind(t, id, "standalone", "ENG:X", "") },
			facts: standalone("ENG:Y", "edge-2"),
			want:  wire.RefuseHostMismatch,
		},
		{
			name: "a standalone environment and its own host again: accepted, nothing to bind",
			setup: func(t *testing.T, r *rig, id string) {
				r.bind(t, id, "standalone", "ENG:X", "")
				// Named already by the host that bound it.
				if _, err := r.st.Write.Exec(`UPDATE environments SET name = 'edge-1', name_auto = 0 WHERE id = ?`, id); err != nil {
					t.Fatal(err)
				}
			},
			facts: standalone("ENG:X", "edge-1"),
			dec:   agents.Decision{},
		},
		{
			name:  "an engine that does not answer cannot be classified and is refused engine_unreachable",
			facts: wire.Facts{Hostname: "edge-1"},
			want:  wire.ReasonEngineUnreachable,
		},
		{
			name:  "a reachable engine with no engine ID is refused auth_rejected",
			facts: wire.Facts{Hostname: "edge-1", EngineReachable: true},
			want:  wire.ReasonAuthRejected,
		},
		{
			name:  "an engine ID outside the alphabet is refused auth_rejected before it touches SQL",
			facts: standalone("x'; DROP TABLE agents;--", "edge-1"),
			want:  wire.ReasonAuthRejected,
		},
		{
			name:  "an engine ID of 129 characters is refused auth_rejected",
			facts: standalone(strings.Repeat("a", 129), "edge-1"),
			want:  wire.ReasonAuthRejected,
		},
		{
			name:  "a cluster ID outside the alphabet is refused auth_rejected",
			facts: manager("ENG:M", "node-m", "CL 1", "mgr-1"),
			want:  wire.ReasonAuthRejected,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			c := r.create(t, "")
			if tc.setup != nil {
				tc.setup(t, r, c.EnvID)
			}
			dec, err := agents.CheckIdentity(r.ctx, r.st.RQ, c.EnvID, tc.facts)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("CheckIdentity: %v", err)
				}
			} else {
				if got := refusal(t, err).Code; got != tc.want {
					t.Fatalf("refusal = %q, want %q (%v)", got, tc.want, err)
				}
			}
			if tc.want == "" || tc.want == wire.ReasonClusterChanged {
				if dec != tc.dec {
					t.Fatalf("decision = %+v, want %+v", dec, tc.dec)
				}
			}
		})
	}
}

func TestACheckIdentityRefusalNamesTheEnvironmentItConcerns(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "lab")
	r.bind(t, c.EnvID, "standalone", "ENG:X", "")
	_, err := agents.CheckIdentity(r.ctx, r.st.RQ, c.EnvID, standalone("ENG:Y", "edge-2"))
	if got := refusal(t, err); got.Code != wire.RefuseHostMismatch || !strings.Contains(got.Detail, "lab") {
		t.Fatalf("refusal = %+v, want host_mismatch naming lab", got)
	}
}

func TestADuplicateIdentityIsRefusedNamingTheEnvironmentThatOwnsIt(t *testing.T) {
	cases := []struct {
		name  string
		first wire.Facts // enrolled into "lab"
		again wire.Facts // offered to the second environment
		note  string
	}{
		{"the engine ID of a standalone host", standalone("ENG:X", "edge-1"), standalone("ENG:X", "clone"), "a cloned VM with the same engine ID"},
		{"the engine ID of a swarm node", worker("ENG:W", "node-w", "wrk-1"), standalone("ENG:W", "wrk-1"), "a swarm node offered as a standalone host"},
		{"the swarm cluster ID", manager("ENG:M1", "node-m1", "CL1", "mgr-1"), manager("ENG:M2", "node-m2", "CL1", "mgr-2"), "a manager of a swarm that is already an environment"},
		{"the swarm node ID", worker("ENG:W", "node-w", "wrk-1"), worker("ENG:OTHER", "node-w", "wrk-1"), "a node that reports another engine ID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			lab := r.create(t, "lab")
			if _, err := r.enrolWith(t, lab.JoinToken, tc.first); err != nil {
				t.Fatalf("the first enrolment: %v", err)
			}
			other := r.create(t, "other")
			_, err := r.enrolWith(t, other.JoinToken, tc.again)
			got := refusal(t, err)
			if got.Code != wire.RefuseDuplicateEnvironment || !strings.Contains(got.Detail, "lab") {
				t.Fatalf("%s: refusal = %+v, want duplicate_environment naming lab", tc.note, got)
			}
			if n := count(t, r.st, `SELECT COUNT(*) FROM agents WHERE env_id = ?`, other.EnvID); n != 0 {
				t.Fatalf("%d agents were written for the refused host", n)
			}
		})
	}
}

func TestTheLocalSocketEnvironmentAndARevokedAgentNeverMakeAHostADuplicate(t *testing.T) {
	r := newRig(t)
	// The local environment (kind local) with an engine ID, as plan 30 will record it.
	if _, err := r.st.Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, engine_id)
		VALUES ('local-1', 'local', 'This host', '/var/run/docker.sock', 1, 1, 1, 'ENG:X')`); err != nil {
		t.Fatal(err)
	}
	c := r.create(t, "edge")
	if _, err := r.enrolWith(t, c.JoinToken, standalone("ENG:X", "edge-1")); err != nil {
		t.Fatalf("a host that is also the local socket host was refused: %v", err)
	}

	// An agent revoked in one environment no longer holds its host there.
	a := r.create(t, "a")
	res, err := r.enrolWith(t, a.JoinToken, worker("ENG:W", "node-w", "wrk-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Revoke(r.ctx, ops, a.EnvID, res.AgentID, "", "", ""); err != nil {
		t.Fatal(err)
	}
	b := r.create(t, "b")
	if _, err := r.enrolWith(t, b.JoinToken, worker("ENG:W", "node-w", "wrk-1")); err != nil {
		t.Fatalf("a host whose agent was revoked elsewhere was refused: %v", err)
	}
}

func TestEnrolBindsTheTypeTheEngineAndTheClusterAndNamesTheEnvironment(t *testing.T) {
	r := newRig(t)

	solo := r.create(t, "")
	if _, err := r.enrolWith(t, solo.JoinToken, standalone("ENG:X", "edge-1")); err != nil {
		t.Fatal(err)
	}
	if s := r.state(t, solo.EnvID); s.envType != "standalone" || s.engine != "ENG:X" || s.name != "edge-1" || s.nameAuto != 0 {
		t.Fatalf("standalone state = %+v", s)
	}

	sw := r.create(t, "")
	if _, err := r.enrolWith(t, sw.JoinToken, worker("ENG:W", "node-w", "wrk-1")); err != nil {
		t.Fatal(err)
	}
	if s := r.state(t, sw.EnvID); s.envType != "swarm" || s.cluster != "" || s.name != "New environment" || s.nameAuto != 1 {
		t.Fatalf("after a worker the state = %+v, want a swarm with no cluster still named by placeholder", s)
	}
	if _, err := r.enrolWith(t, sw.JoinToken, manager("ENG:M", "node-m", "CL1", "mgr-1")); err != nil {
		t.Fatal(err)
	}
	if s := r.state(t, sw.EnvID); s.cluster != "CL1" || s.name != "mgr-1 swarm" || s.nameAuto != 0 {
		t.Fatalf("after the first manager the state = %+v", s)
	}
	// A second manager does not rename it.
	if _, err := r.enrolWith(t, sw.JoinToken, manager("ENG:M2", "node-m2", "CL1", "mgr-2")); err != nil {
		t.Fatal(err)
	}
	if s := r.state(t, sw.EnvID); s.name != "mgr-1 swarm" {
		t.Fatalf("a second manager renamed the environment to %q", s.name)
	}
}

func TestAnEnvironmentTheAdminNamedKeepsItsNameAndAnOverlongHostnameStillFitsTheLimit(t *testing.T) {
	r := newRig(t)
	named := r.create(t, "Production")
	if _, err := r.enrolWith(t, named.JoinToken, manager("ENG:M", "node-m", "CL1", "mgr-1")); err != nil {
		t.Fatal(err)
	}
	if s := r.state(t, named.EnvID); s.name != "Production" || s.nameAuto != 0 {
		t.Fatalf("state = %+v", s)
	}

	long := r.create(t, "")
	if _, err := r.enrolWith(t, long.JoinToken, manager("ENG:M9", "node-m9", "CL9", strings.Repeat("h", 100))); err != nil {
		t.Fatal(err)
	}
	if s := r.state(t, long.EnvID); len([]rune(s.name)) > 64 || !strings.HasSuffix(s.name, " swarm") {
		t.Fatalf("name %q (%d characters), want at most 64 and ending in ' swarm'", s.name, len([]rune(s.name)))
	}
}

func TestEnrolOfAnEngineThatDoesNotAnswerIsRefusedAndLeavesNoAgent(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "")
	_, err := r.enrolWith(t, c.JoinToken, wire.Facts{Hostname: "edge-1"})
	if got := refusal(t, err).Code; got != wire.ReasonEngineUnreachable {
		t.Fatalf("code = %q, want engine_unreachable", got)
	}
	if n := count(t, r.st, `SELECT COUNT(*) FROM agents`); n != 0 {
		t.Fatalf("%d agents were written", n)
	}
	if s := r.state(t, c.EnvID); s.envType != "" || s.name != "New environment" {
		t.Fatalf("state = %+v, want the environment untouched", s)
	}
}

func TestEnrolOfADifferentTypeOrHostLeavesTheBindingAsItWas(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "lab")
	if _, err := r.enrolWith(t, c.JoinToken, standalone("ENG:X", "edge-1")); err != nil {
		t.Fatal(err)
	}
	_, err := r.enrolWith(t, c.JoinToken, manager("ENG:M", "node-m", "CL1", "mgr-1"))
	if got := refusal(t, err).Code; got != wire.RefuseTypeMismatch {
		t.Fatalf("code = %q, want type_mismatch", got)
	}
	_, err = r.enrolWith(t, c.JoinToken, standalone("ENG:Y", "edge-2"))
	if got := refusal(t, err).Code; got != wire.RefuseHostMismatch {
		t.Fatalf("code = %q, want host_mismatch", got)
	}
	if s := r.state(t, c.EnvID); s.envType != "standalone" || s.engine != "ENG:X" {
		t.Fatalf("state = %+v", s)
	}
	if n := count(t, r.st, `SELECT COUNT(*) FROM agents WHERE env_id = ?`, c.EnvID); n != 1 {
		t.Fatalf("%d agents, want the first only", n)
	}
}

func TestTwoManagersOfOneNewClusterEnrollingAtOnceBothSucceedWithOneBinding(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, f := range []wire.Facts{manager("ENG:M1", "node-m1", "CL1", "mgr-1"), manager("ENG:M2", "node-m2", "CL1", "mgr-2")} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = r.enrolWith(t, c.JoinToken, f)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("manager %d: %v", i+1, err)
		}
	}
	if s := r.state(t, c.EnvID); s.cluster != "CL1" || s.envType != "swarm" {
		t.Fatalf("state = %+v", s)
	}
	if n := count(t, r.st, `SELECT COUNT(*) FROM agents WHERE env_id = ?`, c.EnvID); n != 2 {
		t.Fatalf("%d agents, want 2", n)
	}
}

func TestTwoEnvironmentsBindingTheSameClusterAtOnceLeaveOneWinnerAndOneDuplicate(t *testing.T) {
	r := newRig(t)
	a, b := r.create(t, "a"), r.create(t, "b")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, tc := range []struct {
		tok string
		f   wire.Facts
	}{
		{a.JoinToken, manager("ENG:M1", "node-m1", "CL1", "mgr-1")},
		{b.JoinToken, manager("ENG:M2", "node-m2", "CL1", "mgr-2")},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = r.enrolWith(t, tc.tok, tc.f)
		}()
	}
	wg.Wait()
	wins, dups := 0, 0
	for _, err := range errs {
		var re *tunnel.Refusal
		switch {
		case err == nil:
			wins++
		case errors.As(err, &re) && re.Code == wire.RefuseDuplicateEnvironment:
			dups++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 || dups != 1 {
		t.Fatalf("%d winners and %d duplicates, want one of each", wins, dups)
	}
	if n := count(t, r.st, `SELECT COUNT(*) FROM environments WHERE swarm_cluster_id = 'CL1'`); n != 1 {
		t.Fatalf("%d environments bound to CL1, want 1", n)
	}
}

// notices records the environments a notification was made for.
type notices struct {
	mu  sync.Mutex
	ids []string
}

func (n *notices) Notify(envID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ids = append(n.ids, envID)
}

func (n *notices) list() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.ids...)
}

func TestAManagerOfAnotherClusterIsRefusedAtEnrolmentRecordedOnceAndNothingIsRebound(t *testing.T) {
	r := newRig(t)
	n := &notices{}
	r.svc.WithNotifier(n)
	c := r.create(t, "prod")
	if _, err := r.enrolWith(t, c.JoinToken, manager("ENG:M1", "node-m1", "CL1", "mgr-1")); err != nil {
		t.Fatal(err)
	}

	_, err := r.enrolWith(t, c.JoinToken, manager("ENG:M9", "node-m9", "CL2", "mgr-9"))
	if got := refusal(t, err).Code; got != wire.ReasonClusterChanged {
		t.Fatalf("code = %q, want cluster_changed", got)
	}
	s := r.state(t, c.EnvID)
	if s.cluster != "CL1" || s.pending != "CL2" || !s.changedAt.Valid {
		t.Fatalf("state = %+v, want CL1 still bound, CL2 pending and the change recorded", s)
	}
	first := s.changedAt.Int64
	if got := n.list(); len(got) != 1 || got[0] != c.EnvID {
		t.Fatalf("notified %v, want the environment once", got)
	}

	// The same report again, or a third cluster, changes nothing and tells nobody.
	r.clock = r.clock.Add(1e9)
	if _, err := r.enrolWith(t, c.JoinToken, manager("ENG:M9", "node-m9", "CL2", "mgr-9")); err == nil {
		t.Fatal("the second attempt was accepted")
	}
	if _, err := r.enrolWith(t, c.JoinToken, manager("ENG:M8", "node-m8", "CL3", "mgr-8")); err == nil {
		t.Fatal("the third cluster was accepted")
	}
	s = r.state(t, c.EnvID)
	if s.cluster != "CL1" || s.pending != "CL2" || s.changedAt.Int64 != first {
		t.Fatalf("state = %+v, want the first record kept", s)
	}
	if got := n.list(); len(got) != 1 {
		t.Fatalf("notified %d times, want once", len(got))
	}
	if n := count(t, r.st, `SELECT COUNT(*) FROM agents WHERE env_id = ?`, c.EnvID); n != 1 {
		t.Fatalf("%d agents, want the first manager only", n)
	}
}

func TestHelloOfAManagerOfAnotherClusterIsRefusedAndRecordedButAnyOtherHelloIsAccepted(t *testing.T) {
	r := newRig(t)
	n := &notices{}
	r.svc.WithNotifier(n)
	c := r.create(t, "prod")
	m := r.enrolFacts(t, c, manager("ENG:M1", "node-m1", "CL1", "mgr-1"))

	hello := func(f wire.Facts) (tunnel.HelloResult, error) {
		req := m.hello(m.cert, m.priv)
		req.Facts = f
		return r.svc.Hello(r.ctx, req)
	}
	if _, err := hello(manager("ENG:M1", "node-m1", "CL1", "mgr-1")); err != nil {
		t.Fatalf("the same cluster: %v", err)
	}
	if _, err := hello(wire.Facts{}); err != nil {
		t.Fatalf("an engine that did not answer: %v", err)
	}
	if _, err := hello(worker("ENG:M1", "node-m1", "mgr-1")); err != nil {
		t.Fatalf("a node that reports no cluster: %v", err)
	}
	if s := r.state(t, c.EnvID); s.changedAt.Valid {
		t.Fatalf("a change was recorded by accepted hellos: %+v", s)
	}

	_, err := hello(manager("ENG:M1", "node-m1", "CL2", "mgr-1"))
	if got := refusal(t, err).Code; got != wire.ReasonClusterChanged {
		t.Fatalf("code = %q, want cluster_changed", got)
	}
	s := r.state(t, c.EnvID)
	if s.cluster != "CL1" || s.pending != "CL2" || !s.changedAt.Valid {
		t.Fatalf("state = %+v", s)
	}
	if _, err := hello(manager("ENG:M1", "node-m1", "CL2", "mgr-1")); err == nil {
		t.Fatal("the second hello of the new cluster was accepted")
	}
	if got := n.list(); len(got) != 1 {
		t.Fatalf("notified %d times, want once", len(got))
	}
}

func TestHelloDoesNotRefuseATypeOrHostChangeBecauseThoseAreEnrolmentOnlyRefusals(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "lab")
	m := r.enrolFacts(t, c, standalone("ENG:X", "edge-1"))
	req := m.hello(m.cert, m.priv)
	req.Facts = standalone("ENG:Y", "edge-1")
	if _, err := r.svc.Hello(r.ctx, req); err != nil {
		t.Fatalf("hello after the engine ID changed: %v", err)
	}
	req.Facts = manager("ENG:X", "node-m", "CL1", "edge-1")
	if _, err := r.svc.Hello(r.ctx, req); err != nil {
		t.Fatalf("hello after the host joined a swarm: %v", err)
	}
}

// enrolFacts enrols an agent that reports facts and returns it with its keys, so a
// test can say hello for it.
func (r *rig) enrolFacts(t *testing.T, c agents.Created, f wire.Facts) member {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tr := authn.Transcript(wire.ModeEnrol, wire.ProtoVersion, make([]byte, 32), make([]byte, 32), "h", pub, nil)
	res, err := r.svc.Enrol(r.ctx, tunnel.EnrolRequest{
		Token: c.JoinToken, Pub: pub, Sig: authn.Sign(priv, tr), Transcript: tr, AgentVersion: "0.2.0", IP: "198.51.100.7", Facts: f,
	})
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	return member{id: res.AgentID, env: res.EnvID, priv: priv, pub: pub, cert: res.Cert}
}

func TestAnEnvironmentCreatedWithoutANameStoresThePlaceholderAndWaitsForAHostToNameIt(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "")
	if c.Name != "New environment" {
		t.Fatalf("created name = %q, want the placeholder", c.Name)
	}
	if s := r.state(t, c.EnvID); s.name != "New environment" || s.nameAuto != 1 {
		t.Fatalf("state = %+v", s)
	}
	if named := r.create(t, "Production"); r.state(t, named.EnvID).nameAuto != 0 {
		t.Fatal("an environment the admin named is waiting for an automatic name")
	}
}

func TestIdentityRefusalsTravelTheHandshakeWithTheirCodesAndAreAuditedByReason(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{AuditRefusals: true})
	lab, other := srv.NewEnvironment(t, "lab"), srv.NewEnvironment(t, "other")
	srv.Enrol(t, lab) // the default host of lab

	// The same host offered to another environment.
	host := wire.Facts{EngineID: "ENG:" + lab.ID, Hostname: "clone", EngineReachable: true}
	c := srv.Dial(t, tunneltest.Agent{Token: other.Token, Facts: &host})
	if c.Refuse == nil || c.Refuse.Code != wire.RefuseDuplicateEnvironment || !strings.Contains(c.Refuse.Detail, `"lab"`) {
		t.Fatalf("refuse = %+v (err %v), want duplicate_environment naming lab", c.Refuse, c.Err)
	}
	var reason string
	if err := srv.Store.Read.QueryRow(`SELECT reason FROM audit_log WHERE action = 'agent.enrol_refused' ORDER BY seq DESC LIMIT 1`).Scan(&reason); err != nil || reason != "duplicate_host" {
		t.Fatalf("audit reason = %q, %v, want duplicate_host", reason, err)
	}

	// A swarm host offered to the standalone environment, and another host too.
	swarmHost := manager("ENG:M", "node-m", "CL1", "mgr")
	if c := srv.Dial(t, tunneltest.Agent{Token: lab.Token, Facts: &swarmHost}); c.Refuse == nil || c.Refuse.Code != wire.RefuseTypeMismatch {
		t.Fatalf("refuse = %+v (err %v), want type_mismatch", c.Refuse, c.Err)
	}
	another := standalone("ENG:ELSEWHERE", "edge-9")
	if c := srv.Dial(t, tunneltest.Agent{Token: lab.Token, Facts: &another}); c.Refuse == nil || c.Refuse.Code != wire.RefuseHostMismatch {
		t.Fatalf("refuse = %+v (err %v), want host_mismatch", c.Refuse, c.Err)
	}
}

func TestAManagerOfAReinitialisedSwarmIsRefusedOnItsNextHelloAndTheEnvironmentRecordsTheNewCluster(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{AuditRefusals: true})
	env := srv.NewEnvironment(t, "prod")
	was := manager("ENG:M1", "node-m1", "CL1", "mgr-1")
	first := srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: &was})
	if first.Welcome == nil {
		t.Fatalf("enrolment refused: %+v (err %v)", first.Refuse, first.Err)
	}

	now := manager("ENG:M1", "node-m1", "CL2", "mgr-1")
	c := srv.Dial(t, tunneltest.Agent{Identity: &first.Identity, Facts: &now})
	if c.Refuse == nil || c.Refuse.Code != wire.ReasonClusterChanged {
		t.Fatalf("hello refuse = %+v (err %v), want cluster_changed", c.Refuse, c.Err)
	}
	var cluster, pending sql.NullString
	var at sql.NullInt64
	if err := srv.Store.Read.QueryRow(`SELECT swarm_cluster_id, pending_cluster_id, cluster_changed_at FROM environments WHERE id = ?`, env.ID).Scan(&cluster, &pending, &at); err != nil {
		t.Fatal(err)
	}
	if cluster.String != "CL1" || pending.String != "CL2" || !at.Valid {
		t.Fatalf("environment = %v %v %v, want CL1 bound, CL2 pending, recorded", cluster, pending, at)
	}
}

func TestAdoptClusterRebindsClearsTheFlagAuditsFromAndToAndTellsTheBroker(t *testing.T) {
	r := newRig(t)
	n := &notices{}
	r.svc.WithNotifier(n)
	c := r.create(t, "prod")
	r.bind(t, c.EnvID, "swarm", "", "CL1")
	r.markChanged(t, c.EnvID, "CL2")

	got, err := r.svc.AdoptCluster(r.ctx, ops, c.EnvID, "203.0.113.5", "test")
	if err != nil {
		t.Fatalf("AdoptCluster: %v", err)
	}
	if got.From != "CL1" || got.To != "CL2" {
		t.Fatalf("adopted = %+v, want CL1 to CL2", got)
	}
	if s := r.state(t, c.EnvID); s.cluster != "CL2" || s.pending != "" || s.changedAt.Valid {
		t.Fatalf("state = %+v, want CL2 bound and the change cleared", s)
	}
	rows := auditRows(t, r.st, "environment.cluster_adopted")
	if len(rows) != 1 || !strings.Contains(rows[0], `"from":"CL1"`) || !strings.Contains(rows[0], `"to":"CL2"`) {
		t.Fatalf("environment.cluster_adopted = %v", rows)
	}
	var actor, target string
	if err := r.st.Read.QueryRow(`SELECT actor_id, target_id FROM audit_log WHERE action = 'environment.cluster_adopted'`).Scan(&actor, &target); err != nil || actor != "u-ops" || target != c.EnvID {
		t.Fatalf("actor %q target %q, %v", actor, target, err)
	}
	if got := n.list(); len(got) != 1 || got[0] != c.EnvID {
		t.Fatalf("notified %v, want the environment once", got)
	}

	// The same manager is now accepted, and a second adopt has nothing to adopt.
	m := r.enrolFacts(t, c, manager("ENG:M2", "node-m2", "CL2", "mgr-2"))
	req := m.hello(m.cert, m.priv)
	req.Facts = manager("ENG:M2", "node-m2", "CL2", "mgr-2")
	if _, err := r.svc.Hello(r.ctx, req); err != nil {
		t.Fatalf("hello of the adopted cluster: %v", err)
	}
	if _, err := r.svc.AdoptCluster(r.ctx, ops, c.EnvID, "", ""); !errors.Is(err, agents.ErrNoClusterChange) {
		t.Fatalf("a second adopt = %v, want ErrNoClusterChange", err)
	}
}

func TestAdoptClusterWithNothingToAdoptAnUnknownEnvironmentAndAClusterAnotherEnvironmentHoldsChangesNothing(t *testing.T) {
	r := newRig(t)
	c := r.create(t, "prod")
	r.bind(t, c.EnvID, "swarm", "", "CL1")
	if _, err := r.svc.AdoptCluster(r.ctx, ops, c.EnvID, "", ""); !errors.Is(err, agents.ErrNoClusterChange) {
		t.Fatalf("no recorded change = %v, want ErrNoClusterChange", err)
	}
	if _, err := r.svc.AdoptCluster(r.ctx, ops, "0197b8a0-0000-7000-8000-000000000000", "", ""); !errors.Is(err, agents.ErrEnvironmentNotFound) {
		t.Fatalf("unknown environment = %v, want ErrEnvironmentNotFound", err)
	}

	// CL2 became another environment's while this one waited.
	other := r.create(t, "other")
	r.bind(t, other.EnvID, "swarm", "", "CL2")
	r.markChanged(t, c.EnvID, "CL2")
	if _, err := r.svc.AdoptCluster(r.ctx, ops, c.EnvID, "", ""); !errors.Is(err, agents.ErrClusterInUse) {
		t.Fatalf("a cluster another environment holds = %v, want ErrClusterInUse", err)
	}
	if s := r.state(t, c.EnvID); s.cluster != "CL1" || s.pending != "CL2" || !s.changedAt.Valid {
		t.Fatalf("state = %+v, want it as it was", s)
	}
	if n := len(auditRows(t, r.st, "environment.cluster_adopted")); n != 0 {
		t.Fatalf("%d adopt entries for refused adopts", n)
	}
}

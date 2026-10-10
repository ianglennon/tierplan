// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/agents"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/tunnel"
	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/bayplan/proto/wire"
)

// Manager choice, failover and the cluster view (plan 02-20, ENV-09, ENV-06,
// ENV-02, ENV-08, D-34, D-42, D-43, D-44, D-47). A real hub, real tunnels and a
// fake Docker daemon behind each agent, as in aggregate_test.go.

// swarmRig is a swarm environment of three managers, mgr-2 the leader, with the
// cluster reader attached.
type swarmRig struct {
	*tunRig
	env tunneltest.Environment
	c   *Clusters
	// m are the managers mgr-1, mgr-2 and mgr-3.
	m     [3]*agentConn
	nodes []dockertest.FakeNode
}

func managerNodes(extra ...dockertest.FakeNode) []dockertest.FakeNode {
	nodes := []dockertest.FakeNode{
		{ID: "node-m1", Hostname: "mgr-1", Role: "manager", EngineVersion: "29.1.0", OS: "linux"},
		{ID: "node-m2", Hostname: "mgr-2", Role: "manager", EngineVersion: "29.1.0", OS: "linux", Leader: true},
		{ID: "node-m3", Hostname: "mgr-3", Role: "manager", EngineVersion: "29.1.0", OS: "linux"},
	}
	return append(nodes, extra...)
}

func managerFacts(i int) *wire.Facts {
	return &wire.Facts{
		EngineID: fmt.Sprintf("ENG:m%d", i), Hostname: fmt.Sprintf("mgr-%d", i), EngineReachable: true,
		Swarm: &wire.SwarmFacts{State: "active", NodeID: fmt.Sprintf("node-m%d", i), ClusterID: "cluster-1", Control: true},
	}
}

func managerConfig(i int, nodes []dockertest.FakeNode, services []dockertest.FakeService) dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: fmt.Sprintf("mgr-%d", i), SwarmState: "active", SwarmControl: true,
		NodeRows: nodes, ServiceRows: services,
		Containers: []dockertest.FakeContainer{{ID: fmt.Sprintf("c%d", i), Names: []string{fmt.Sprintf("/svc-on-m%d", i)}, State: "running"}},
	}
}

func newSwarmRig(t *testing.T, nodes []dockertest.FakeNode, services []dockertest.FakeService) *swarmRig {
	t.Helper()
	return newSwarmRigWith(t, nodes, services, nil)
}

// newSwarmRigWith is newSwarmRig with a reaper watching the node lists.
func newSwarmRigWith(t *testing.T, nodes []dockertest.FakeNode, services []dockertest.FakeService, reaper NodeObserver) *swarmRig {
	t.Helper()
	r := newTunRig(t, nil)
	r.pastGrace()
	s := &swarmRig{tunRig: r, nodes: nodes}
	s.env = r.srv.NewEnvironment(t, "Swarm")
	s.c = NewClusters(r.svc, r.snaps, ClusterOptions{
		Now: r.clk.Now, Publisher: r.brk, Events: r.evs, Grace: 2 * time.Minute, Reaper: reaper,
	})
	r.svc.WithClusters(s.c)
	for i := 1; i <= 3; i++ {
		s.m[i-1] = r.connect(t, s.env, managerConfig(i, nodes, services), managerFacts(i))
	}
	r.whenOnline(t, s.env.ID)
	return s
}

// probeHost says which manager a read went to: each fake daemon has its own host name.
func probeHost(t *testing.T, cli *dockerx.Client) string {
	t.Helper()
	res, err := cli.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	return res.Hostname
}

// reconnect brings an agent back with its identity, as its own retry would.
func (r *tunRig) reconnect(t *testing.T, a *agentConn) {
	t.Helper()
	c := r.srv.Hello(t, a.conn.Identity)
	if c.Welcome == nil {
		t.Fatalf("hello refused: %+v", c.Refuse)
	}
	c.ProxyDocker(t, a.fake.SocketPath())
	control := c.OpenControl(t)
	tunneltest.SendHeartbeat(t, control, true, r.clk.Now().UnixMilli())
}

func (r *tunRig) kick(t *testing.T, a *agentConn) {
	t.Helper()
	r.srv.Hub.Kick(a.id, tunnel.DisconnectClosed)
	eventually(t, "the session to be gone", func() bool {
		return !connectedNow(r.srv.Hub, a.conn.Identity.EnvID, a.id)
	})
}

func connectedNow(h *tunnel.Hub, envID, agentID string) bool {
	for _, v := range h.Views(envID) {
		if v.AgentID == agentID {
			return v.Connected
		}
	}
	return false
}

func (r *tunRig) failovers(t *testing.T) []FailoverData {
	t.Helper()
	backlog, _, cancel := r.brk.Subscribe(context.Background())
	defer cancel()
	var out []FailoverData
	for _, ev := range backlog {
		if ev.Type == EventFailover {
			d, ok := ev.Data.(FailoverData)
			if !ok {
				t.Fatalf("a failover event carries %T", ev.Data)
			}
			out = append(out, d)
		}
	}
	return out
}

func (r *tunRig) failoverAudits(t *testing.T, envID string) int {
	return rowCount(t, r, `SELECT COUNT(*) FROM audit_log WHERE action = 'environment.failover' AND env_id = ? AND actor_id = 'system:agent-hub'`, envID)
}

func TestARefreshLearnsTheLeaderAndLaterReadsGoToIt(t *testing.T) {
	s := newSwarmRig(t, managerNodes(), nil)
	if err := s.c.Refresh(context.Background(), s.env.ID); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	st, ok := s.c.Cached(s.env.ID)
	if !ok || st.LeaderNodeID != "node-m2" || st.LeaderAgentID != s.m[1].id || len(st.Read.Nodes) != 3 {
		t.Fatalf("state = %+v (%v), want the leader node-m2 on agent %s and three nodes", st, ok, s.m[1].id)
	}
	if got := s.c.LeaderAgent(s.env.ID); got != s.m[1].id {
		t.Errorf("LeaderAgent = %q, want %q", got, s.m[1].id)
	}
	for range 3 {
		var host string
		if err := s.c.ClusterRead(context.Background(), s.env.ID, func(cli *dockerx.Client) error {
			host = probeHost(t, cli)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if host != "mgr-2" {
			t.Fatalf("the read went to %s, want the leader mgr-2", host)
		}
	}
	// The one-engine reads of the dashboard follow the same choice.
	in, err := s.svc.Input(context.Background(), s.env.ID, s.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if key, _ := s.svc.primary(in); key != AgentTarget(s.m[1].id) {
		t.Errorf("the primary target is %s, want the leader's %s", key, AgentTarget(s.m[1].id))
	}
}

func TestWhenTheLeaderDiesMidReadTheReadSucceedsOnAnotherManagerAndTheFailoverIsSaidOnce(t *testing.T) {
	s := newSwarmRig(t, managerNodes(), nil)
	ctx := context.Background()
	if err := s.c.Refresh(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}

	var attempts []string
	err := s.c.ClusterRead(ctx, s.env.ID, func(cli *dockerx.Client) error {
		attempts = append(attempts, probeHost(t, cli))
		if len(attempts) == 1 {
			s.kick(t, s.m[1]) // mgr-2's tunnel drops under the read
		}
		_, err := cli.Cluster(ctx)
		return err
	})
	if err != nil {
		t.Fatalf("the read failed although two managers were left: %v", err)
	}
	if len(attempts) != 2 || attempts[0] != "mgr-2" || (attempts[1] != "mgr-1" && attempts[1] != "mgr-3") {
		t.Fatalf("attempts = %v, want mgr-2 first and then one other manager", attempts)
	}

	fo := s.failovers(t)
	if len(fo) != 1 || fo[0].FromAgentID != s.m[1].id || fo[0].FromNode != "mgr-2" || fo[0].ToNode != attempts[1] {
		t.Fatalf("failover events = %+v, want one from mgr-2 to %s", fo, attempts[1])
	}
	msg := "Manager mgr-2 unreachable, using " + attempts[1]
	var found bool
	for _, row := range s.evs.Recent(AgentTarget(fo[0].ToAgentID), 10) {
		found = found || row.Type == "bayplan" && row.Name == msg
	}
	if !found {
		t.Errorf("the feed of the manager now in use has no bayplan row saying %q: %+v", msg, s.evs.Recent(AgentTarget(fo[0].ToAgentID), 10))
	}
	if n := s.failoverAudits(t, s.env.ID); n != 1 {
		t.Errorf("environment.failover audit rows under system:agent-hub = %d, want 1", n)
	}
	if n := rowCount(t, s.tunRig, `SELECT COUNT(*) FROM audit_log WHERE action = 'environment.failover' AND details LIKE '%mgr-2%' AND details LIKE ?`, "%"+attempts[1]+"%"); n != 1 {
		t.Errorf("the audit row does not name both managers (%d rows)", n)
	}

	// Degraded until mgr-2 is back, naming it.
	e := s.health(t, s.env.ID)
	if e.Health.State != StateDegraded || issueList(e.Health) != "agent_offline@mgr-2" {
		t.Fatalf("health = %+v (%s), want degraded with agent_offline@mgr-2", e.Health, issueList(e.Health))
	}

	// Reads keep going to the manager that took over, with no further event.
	for range 2 {
		if err := s.c.ClusterRead(ctx, s.env.ID, func(cli *dockerx.Client) error { _, err := cli.Cluster(ctx); return err }); err != nil {
			t.Fatal(err)
		}
	}
	// mgr-2 returns: the degradation clears, and nothing is said a second time.
	s.reconnect(t, s.m[1])
	eventually(t, "mgr-2 to be back and the environment online", func() bool {
		e := s.health(t, s.env.ID)
		return e.Health.State == StateOnline
	})
	if err := s.c.Refresh(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(s.failovers(t)); n != 1 {
		t.Errorf("%d failover events after the manager came back, want still 1", n)
	}
	if n := s.failoverAudits(t, s.env.ID); n != 1 {
		t.Errorf("%d failover audit rows after the manager came back, want still 1", n)
	}
}

func TestAManagerThatDisconnectedBetweenReadsIsAFailoverToo(t *testing.T) {
	s := newSwarmRig(t, managerNodes(), nil)
	ctx := context.Background()
	// The first read does not know the leader yet; the second goes to it.
	for range 2 {
		if err := s.c.Refresh(ctx, s.env.ID); err != nil {
			t.Fatal(err)
		}
	}
	if st, _ := s.c.Cached(s.env.ID); st.InUse != s.m[1].id {
		t.Fatalf("the manager in use is %s, want the leader %s", st.InUse, s.m[1].id)
	}
	s.kick(t, s.m[1])
	var host string
	if err := s.c.ClusterRead(ctx, s.env.ID, func(cli *dockerx.Client) error { host = probeHost(t, cli); return nil }); err != nil {
		t.Fatal(err)
	}
	if host == "mgr-2" || host == "" {
		t.Fatalf("the read went to %q although mgr-2 is gone", host)
	}
	fo := s.failovers(t)
	if len(fo) != 1 || fo[0].FromNode != "mgr-2" || fo[0].ToNode != host {
		t.Fatalf("failover events = %+v, want one from mgr-2 to %s", fo, host)
	}
}

func TestTwoReadsThatBothLoseTheLeaderProduceOneFailoverEvent(t *testing.T) {
	s := newSwarmRig(t, managerNodes(), nil)
	ctx := context.Background()
	if err := s.c.Refresh(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}

	var entered sync.WaitGroup
	entered.Add(2)
	proceed := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			first := true
			errs <- s.c.ClusterRead(ctx, s.env.ID, func(cli *dockerx.Client) error {
				if first {
					first = false
					entered.Done()
					<-proceed
				}
				_, err := cli.Cluster(ctx)
				return err
			})
		}()
	}
	entered.Wait()
	s.kick(t, s.m[1])
	close(proceed)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a read failed although two managers were left: %v", err)
		}
	}
	if fo := s.failovers(t); len(fo) != 1 {
		t.Errorf("failover events = %+v, want exactly one for two reads", fo)
	}
	if n := s.failoverAudits(t, s.env.ID); n != 1 {
		t.Errorf("failover audit rows = %d, want 1", n)
	}
}

func TestAReadErrorThatIsNotATunnelErrorIsNotRetried(t *testing.T) {
	s := newSwarmRig(t, managerNodes(), nil)
	ctx := context.Background()
	if err := s.c.Refresh(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("boom")
	var calls atomic.Int32
	if err := s.c.ClusterRead(ctx, s.env.ID, func(*dockerx.Client) error { calls.Add(1); return boom }); !errors.Is(err, boom) || calls.Load() != 1 {
		t.Fatalf("a plain error: err = %v after %d calls, want boom after 1", err, calls.Load())
	}

	// A 500 from the daemon is the daemon's answer, not a lost tunnel.
	for i, a := range s.m {
		cfg := managerConfig(i+1, s.nodes, nil)
		cfg.QuorumLost = true
		a.fake.Reconfigure(cfg)
	}
	calls.Store(0)
	err := s.c.ClusterRead(ctx, s.env.ID, func(cli *dockerx.Client) error { calls.Add(1); _, err := cli.Cluster(ctx); return err })
	if err == nil || ReasonFor(err) != ReasonNoQuorum || calls.Load() != 1 {
		t.Fatalf("a quorum-loss 500: err = %v (reason %q) after %d calls, want no_quorum after 1", err, ReasonFor(err), calls.Load())
	}
	if len(s.failovers(t)) != 0 {
		t.Error("a daemon error produced a failover event")
	}
}

func TestATunnelErrorIsRetriedOnceAndNoMoreAndSaysNothingWhenTheRetryFails(t *testing.T) {
	s := newSwarmRig(t, managerNodes(), nil)
	ctx := context.Background()
	if err := s.c.Refresh(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	err := s.c.ClusterRead(ctx, s.env.ID, func(*dockerx.Client) error {
		calls.Add(1)
		return fmt.Errorf("lost: %w", tunnel.ErrAgentGone)
	})
	if !errors.Is(err, tunnel.ErrAgentGone) || calls.Load() != 2 {
		t.Fatalf("err = %v after %d calls, want the tunnel error after exactly 2 (one retry, on another manager)", err, calls.Load())
	}
	if len(s.failovers(t)) != 0 || s.failoverAudits(t, s.env.ID) != 0 {
		t.Error("a retry that also failed produced a failover record")
	}
}

func TestReadOnlyClusterCallerOffersOnlyReadFunctions(t *testing.T) {
	typ := reflect.TypeOf(&Clusters{})
	bad := regexp.MustCompile(`Write|Update|Create|Remove|Mutate|Delete|Scale|Deploy`)
	var names []string
	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		names = append(names, name)
		if bad.MatchString(name) {
			t.Errorf("(*Clusters).%s sounds like a change; the cluster reader has no mutating variant (D-42)", name)
		}
	}
	if !strings.Contains(strings.Join(names, " "), "ClusterRead") {
		t.Errorf("the methods %v do not include ClusterRead: the test would prove nothing", names)
	}
}

func TestNoManagerConnectedIsNotAManager(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Workers")
	c := NewClusters(r.svc, r.snaps, ClusterOptions{Now: r.clk.Now, Publisher: r.brk, Events: r.evs})
	r.svc.WithClusters(c)
	var workers []*agentConn
	for i := 1; i <= 2; i++ {
		workers = append(workers, r.connect(t, env, dockertest.FakeConfig{
			Hostname: fmt.Sprintf("w-%d", i), SwarmState: "active", SwarmControl: false,
			Containers: []dockertest.FakeContainer{{ID: fmt.Sprintf("w%d", i), Names: []string{fmt.Sprintf("/job-%d", i)}, State: "running"}},
		}, &wire.Facts{
			EngineID: fmt.Sprintf("ENG:w%d", i), Hostname: fmt.Sprintf("w-%d", i), EngineReachable: true,
			Swarm: &wire.SwarmFacts{State: "active", NodeID: fmt.Sprintf("node-w%d", i), Control: false},
		}))
	}
	var e Environment
	// Both sessions are known to the hub before the health is read, or the second
	// worker still counts as missing.
	eventually(t, "both workers to be connected", func() bool {
		return connectedNow(r.srv.Hub, env.ID, workers[0].id) && connectedNow(r.srv.Hub, env.ID, workers[1].id)
	})
	eventually(t, "the workers to be probed", func() bool {
		e = r.health(t, env.ID)
		return e.Status.State == StateOnline
	})
	if e.Health.State != StateDegraded || e.Health.Reason != ReasonNotAManager || issueList(e.Health) != "not_a_manager@" {
		t.Fatalf("health = %+v (%s), want degraded / not_a_manager", e.Health, issueList(e.Health))
	}
	err := c.ClusterRead(context.Background(), env.ID, func(*dockerx.Client) error { return nil })
	if !errors.Is(err, ErrNoManager) || ReasonFor(err) != ReasonNotAManager {
		t.Errorf("ClusterRead = %v (reason %q), want ErrNoManager that maps to not_a_manager", err, ReasonFor(err))
	}
	// Node-local data from the workers keeps working.
	agg, _, err := r.agg.Snapshot(context.Background(), env.ID)
	if err != nil || len(agg.Containers) != 2 {
		t.Fatalf("Snapshot = %+v, %v; want both workers' containers", agg.Containers, err)
	}
}

func TestQuorumLossIsNoQuorumWhileNodeLocalDataKeepsWorking(t *testing.T) {
	s := newSwarmRig(t, managerNodes(), nil)
	ctx := context.Background()
	if err := s.c.Refresh(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}
	if e := s.health(t, s.env.ID); e.Health.State != StateOnline {
		t.Fatalf("health = %+v before the quorum was lost, want online", e.Health)
	}

	for i, a := range s.m {
		cfg := managerConfig(i+1, s.nodes, nil)
		cfg.QuorumLost = true
		a.fake.Reconfigure(cfg)
	}
	if err := s.c.Refresh(ctx, s.env.ID); err == nil || ReasonFor(err) != ReasonNoQuorum {
		t.Fatalf("Refresh = %v, want an error that maps to no_quorum", err)
	}
	e := s.health(t, s.env.ID)
	if e.Health.State != StateDegraded || e.Health.Reason != ReasonNoQuorum || issueList(e.Health) != "no_quorum@" {
		t.Fatalf("health = %+v (%s), want degraded / no_quorum", e.Health, issueList(e.Health))
	}
	if e.Overview.Nodes != nil && e.Overview.Nodes.Total != 3 {
		t.Errorf("nodes = %+v: stale cluster counts were shown while the quorum is lost", e.Overview.Nodes)
	}
	agg, _, err := s.agg.Snapshot(ctx, s.env.ID)
	if err != nil || len(agg.Containers) != 3 {
		t.Fatalf("Snapshot = %+v, %v; want every manager's node-local containers while the quorum is lost", agg.Containers, err)
	}
	if agg.Counts.Nodes != nil || agg.Counts.Services != nil {
		t.Errorf("counts = %+v, want no cluster counts without a quorum", agg.Counts)
	}

	// The quorum comes back: one successful read clears it.
	for i, a := range s.m {
		a.fake.Reconfigure(managerConfig(i+1, s.nodes, nil))
	}
	if err := s.c.Refresh(ctx, s.env.ID); err != nil {
		t.Fatalf("Refresh after recovery: %v", err)
	}
	if e := s.health(t, s.env.ID); e.Health.State != StateOnline {
		t.Fatalf("health = %+v after the quorum came back, want online", e.Health)
	}
}

func TestExpectedAgentsAreActiveReadyOrDownNodesAndTheOverviewCountsNodesAndAgents(t *testing.T) {
	nodes := managerNodes(
		dockertest.FakeNode{ID: "node-w1", Hostname: "w-1", Role: "worker", EngineVersion: "29.1.0", OS: "linux"},
		dockertest.FakeNode{ID: "node-w2", Hostname: "w-2", Role: "worker", Availability: "drain", EngineVersion: "29.1.0", OS: "linux"},
		dockertest.FakeNode{ID: "node-w3", Hostname: "w-3", Role: "worker", State: "down", EngineVersion: "29.1.0", OS: "linux"},
		dockertest.FakeNode{ID: "node-w4", Hostname: "w-4", Role: "worker", Availability: "pause", State: "down", EngineVersion: "29.1.0", OS: "linux"},
	)
	s := newSwarmRig(t, nodes, nil)
	if err := s.c.Refresh(context.Background(), s.env.ID); err != nil {
		t.Fatal(err)
	}
	e := s.health(t, s.env.ID)
	// Nodes: 3 managers + w-1 + w-2 are ready, w-3 and w-4 are down.
	if e.Overview.Nodes == nil || *e.Overview.Nodes != (NodeCounts{Ready: 5, Total: 7}) {
		t.Errorf("nodes = %+v, want 5 ready of 7", e.Overview.Nodes)
	}
	// Agents expected: active nodes that are ready or down: 3 managers, w-1 and w-3.
	if e.Overview.Agents == nil || *e.Overview.Agents != (AgentCounts{Connected: 3, Expected: 5}) {
		t.Errorf("agents = %+v, want 3 connected of 5 expected (a drained or paused node runs none)", e.Overview.Agents)
	}
	// Two expected agents have not enrolled: two issues with no node name.
	if e.Health.State != StateDegraded || issueList(e.Health) != "agent_offline@,agent_offline@" {
		t.Errorf("health = %+v (%s), want degraded with two missing agents", e.Health, issueList(e.Health))
	}
}

func TestANodeOnAnEngineBelow24IsUnsupportedForThatNodeOnly(t *testing.T) {
	// Drained, so it is not expected to run an agent: the engine is the only issue.
	nodes := managerNodes(dockertest.FakeNode{ID: "node-old", Hostname: "old-1", Role: "worker", Availability: "drain", EngineVersion: "23.0.7", OS: "linux"})
	s := newSwarmRig(t, nodes, nil)
	if err := s.c.Refresh(context.Background(), s.env.ID); err != nil {
		t.Fatal(err)
	}
	e := s.health(t, s.env.ID)
	if e.Health.State != StateDegraded || e.Health.Reason != ReasonUnsupportedEngine || issueList(e.Health) != "unsupported_engine@old-1" {
		t.Fatalf("health = %+v (%s), want degraded / unsupported_engine@old-1 and nothing refused as a whole", e.Health, issueList(e.Health))
	}
	if _, _, err := s.agg.Snapshot(context.Background(), s.env.ID); err != nil {
		t.Errorf("the environment cannot be read although one node is old: %v", err)
	}
}

func TestFailingServicesFillTheOverviewWithTheGraceFromTheClusterOptions(t *testing.T) {
	var s *swarmRig
	// The clock is the rig's; the services are updated relative to it.
	old := time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC)
	services := []dockertest.FakeService{
		{ID: "s1", Name: "web", Mode: "replicated", Running: 2, Desired: 3, UpdatedAt: old},
		{ID: "s2", Name: "db", Mode: "replicated", Running: 1, Desired: 1, UpdatedAt: old},
		{ID: "s3", Name: "cron", Mode: "replicated-job", Running: 0, Desired: 1, UpdatedAt: old},
		{ID: "s4", Name: "api", Mode: "global", Running: 3, Desired: 3, UpdatedAt: old},
		{ID: "s5", Name: "new", Mode: "replicated", Running: 0, Desired: 1, UpdatedAt: time.Date(2026, 3, 1, 12, 1, 30, 0, time.UTC)},
	}
	s = newSwarmRig(t, managerNodes(), services)
	if err := s.c.Refresh(context.Background(), s.env.ID); err != nil {
		t.Fatal(err)
	}
	e := s.health(t, s.env.ID)
	// web is failing; "new" was updated 30 s before the rig's clock, so it is inside the grace.
	if e.Overview.Failing == nil || *e.Overview.Failing != (FailingCounts{Failing: 1, Total: 4}) {
		t.Fatalf("failing = %+v, want 1 of 4 (the job is not counted, the new service is inside the grace)", e.Overview.Failing)
	}
}

func TestAStandaloneHostCountsItsFailingContainersFromTheirRestartPolicies(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	c := NewClusters(r.svc, r.snaps, ClusterOptions{Now: r.clk.Now, Publisher: r.brk, Events: r.evs})
	r.svc.WithClusters(c)
	env := r.srv.NewEnvironment(t, "Standalone")
	edge := r.connect(t, env, dockertest.FakeConfig{
		Hostname: "edge-1",
		Containers: []dockertest.FakeContainer{
			{ID: "a", Names: []string{"/loops"}, State: "restarting", Status: "Restarting (1) 3 seconds ago"},
			{ID: "b", Names: []string{"/fine"}, State: "running", Status: "Up 2 hours"},
			{ID: "c", Names: []string{"/stopped-by-hand"}, State: "exited", Status: "Exited (0) 1 hour ago"},
			{ID: "d", Names: []string{"/oneoff"}, State: "exited", Status: "Exited (1) 1 hour ago"},
		},
		RestartPolicies: map[string]string{"a": "always", "b": "unless-stopped", "c": "always", "d": "no"},
	}, nil)
	r.whenOnline(t, env.ID)
	if err := c.Refresh(context.Background(), env.ID); err != nil {
		t.Fatal(err)
	}
	e := r.health(t, env.ID)
	if e.Overview.Failing == nil || *e.Overview.Failing != (FailingCounts{Failing: 1, Total: 3}) {
		t.Fatalf("failing = %+v, want 1 of 3 containers that have a restart policy", e.Overview.Failing)
	}
	// Policies are read once: a second refresh inspects nothing new.
	n := len(inspectRequests(edge))
	if err := c.Refresh(context.Background(), env.ID); err != nil {
		t.Fatal(err)
	}
	if got := len(inspectRequests(edge)); got != n {
		t.Errorf("the second refresh inspected %d more containers, want none (cached by ID)", got-n)
	}
}

// inspectRequests are the container inspects the standalone host's daemon served.
func inspectRequests(a *agentConn) []string {
	var out []string
	for _, p := range a.fake.Requests() {
		if strings.Contains(p, "/containers/") && strings.HasSuffix(p, "/json") && !strings.HasSuffix(p, "/containers/json") {
			out = append(out, p)
		}
	}
	return out
}

func TestALocalSwarmManagerCountsItsNodesAndFailingServices(t *testing.T) {
	local := dockertest.NewFake(t, managerConfig(1, managerNodes(), []dockertest.FakeService{
		{ID: "s1", Name: "web", Mode: "replicated", Running: 0, Desired: 2, UpdatedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)},
	}))
	r := newTunRig(t, local)
	r.pastGrace()
	c := NewClusters(r.svc, r.snaps, ClusterOptions{Now: r.clk.Now, Publisher: r.brk, Events: r.evs})
	r.svc.WithClusters(c)
	env, err := r.svc.AddLocal(context.Background(), admin, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	r.whenOnline(t, env.ID)
	if err := c.Refresh(context.Background(), env.ID); err != nil {
		t.Fatal(err)
	}
	e := r.health(t, env.ID)
	if e.Overview.Nodes == nil || *e.Overview.Nodes != (NodeCounts{Ready: 3, Total: 3}) {
		t.Errorf("nodes = %+v, want 3 of 3 from the local manager", e.Overview.Nodes)
	}
	if e.Overview.Failing == nil || *e.Overview.Failing != (FailingCounts{Failing: 1, Total: 1}) {
		t.Errorf("failing = %+v, want 1 of 1", e.Overview.Failing)
	}
}

func TestRunRefreshesEnvironmentsOnAnIntervalAndStopsWhenItsContextEnds(t *testing.T) {
	s := newSwarmRig(t, managerNodes(), nil)
	c := NewClusters(s.svc, s.snaps, ClusterOptions{Now: s.clk.Now, Publisher: s.brk, Events: s.evs, Interval: 10 * time.Millisecond})
	s.svc.WithClusters(c)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	eventually(t, "the background refresh to read the cluster", func() bool {
		st, ok := c.Cached(s.env.ID)
		return ok && len(st.Read.Nodes) == 3
	})
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}

func TestForgetDropsTheStateOfARemovedEnvironment(t *testing.T) {
	s := newSwarmRig(t, managerNodes(), nil)
	if err := s.c.Refresh(context.Background(), s.env.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.c.Cached(s.env.ID); !ok {
		t.Fatal("nothing was cached")
	}
	s.c.Forget(s.env.ID)
	if _, ok := s.c.Cached(s.env.ID); ok {
		t.Error("the state of a forgotten environment is still cached")
	}
}

// observed is a NodeObserver that records every node list it was shown.
type observed struct {
	mu    sync.Mutex
	calls []observation
}

type observation struct {
	envID, answerer string
	nodes           []string
}

func (o *observed) Observe(_ context.Context, envID string, list dockerx.ClusterRead, answeringNodeID string) {
	var ids []string
	for _, n := range list.Nodes {
		ids = append(ids, n.ID)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, observation{envID: envID, answerer: answeringNodeID, nodes: ids})
}

func (o *observed) list() []observation {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]observation(nil), o.calls...)
}

func TestEverySuccessfulNodeListIsShownToTheReaperWithTheNodeOfTheManagerThatAnswered(t *testing.T) {
	obs := &observed{}
	s := newSwarmRigWith(t, managerNodes(), nil, obs)
	ctx := context.Background()
	for range 2 {
		if err := s.c.Refresh(ctx, s.env.ID); err != nil {
			t.Fatal(err)
		}
	}
	got := obs.list()
	if len(got) != 2 {
		t.Fatalf("the reaper saw %d lists, want 2: %+v", len(got), got)
	}
	for _, o := range got {
		if o.envID != s.env.ID || strings.Join(o.nodes, ",") != "node-m1,node-m2,node-m3" {
			t.Errorf("observation = %+v, want this environment's three nodes", o)
		}
	}
	// The second read went to the leader, whose node answered.
	if got[1].answerer != "node-m2" {
		t.Errorf("the second list was answered by %q, want the leader's node-m2", got[1].answerer)
	}
}

func TestAFailedNodeListAndALoneWorkerAreNotShownToTheReaper(t *testing.T) {
	obs := &observed{}
	s := newSwarmRigWith(t, managerNodes(), nil, obs)
	ctx := context.Background()
	if err := s.c.Refresh(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}
	for i, a := range s.m {
		cfg := managerConfig(i+1, s.nodes, nil)
		cfg.QuorumLost = true
		a.fake.Reconfigure(cfg)
	}
	if err := s.c.Refresh(ctx, s.env.ID); err == nil {
		t.Fatal("a read without quorum succeeded")
	}
	if n := len(obs.list()); n != 1 {
		t.Fatalf("the reaper saw %d lists, want only the one that succeeded", n)
	}
}

func TestLocalSwarmListsAreNotShownToTheReaperBecauseTheyHaveNoAgents(t *testing.T) {
	obs := &observed{}
	local := dockertest.NewFake(t, managerConfig(1, managerNodes(), nil))
	r := newTunRig(t, local)
	r.pastGrace()
	c := NewClusters(r.svc, r.snaps, ClusterOptions{Now: r.clk.Now, Publisher: r.brk, Events: r.evs, Reaper: obs})
	r.svc.WithClusters(c)
	env, err := r.svc.AddLocal(context.Background(), admin, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	r.whenOnline(t, env.ID)
	if err := c.Refresh(context.Background(), env.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(obs.list()); n != 0 {
		t.Errorf("the reaper saw %d lists of the local socket's swarm", n)
	}
}

func TestAWorkerRemovedFromTheSwarmLosesItsAgentAfterTwoReadsAndItsSessionIsClosed(t *testing.T) {
	var s *swarmRig
	// The real reaper over the same store as the tunnel's agents service.
	reaper := &lateReaper{}
	s = newSwarmRigWith(t, managerNodes(), nil, reaper)
	reaper.set(agents.NewReaper(s.srv.Svc))

	// A worker that is enrolled and connected, and has been removed from the swarm:
	// the managers' node list does not name it.
	w := s.connect(t, s.env, dockertest.FakeConfig{Hostname: "gone-1", SwarmState: "active"}, &wire.Facts{
		EngineID: "ENG:gone-1", Hostname: "gone-1", EngineReachable: true,
		Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-gone-1", Control: false},
	})
	eventually(t, "the worker's session", func() bool { return s.srv.Hub.Session(w.id) != nil })
	sess := s.srv.Hub.Session(w.id)
	ctx := context.Background()

	if err := s.c.Refresh(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}
	if n := rowCount(t, s.tunRig, `SELECT COUNT(*) FROM agents WHERE id = ? AND status = 'active'`, w.id); n != 1 {
		t.Fatal("the worker was revoked after one node list")
	}
	if err := s.c.Refresh(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}
	if n := rowCount(t, s.tunRig, `SELECT COUNT(*) FROM agents WHERE id = ? AND status = 'revoked' AND revoked_by = 'system:agent-reaper'`, w.id); n != 1 {
		t.Fatal("the worker was not revoked by the reaper after two node lists")
	}
	select {
	case <-sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the revoked agent's session is still open")
	}
	if n := rowCount(t, s.tunRig, `SELECT COUNT(*) FROM audit_log WHERE action = 'agent.auto_revoked' AND env_id = ? AND actor_id = 'system:agent-reaper'`, s.env.ID); n != 1 {
		t.Errorf("agent.auto_revoked rows = %d, want 1", n)
	}
	if n := rowCount(t, s.tunRig, `SELECT COUNT(*) FROM agent_blocks WHERE env_id = ?`, s.env.ID); n != 0 {
		t.Errorf("%d block rows, want none", n)
	}
	// The three managers are untouched and the environment is healthy again.
	if n := rowCount(t, s.tunRig, `SELECT COUNT(*) FROM agents WHERE env_id = ? AND status = 'active'`, s.env.ID); n != 3 {
		t.Errorf("%d active agents left, want the three managers", n)
	}
	eventually(t, "the environment to be online without the removed worker", func() bool {
		return s.health(t, s.env.ID).Health.State == StateOnline
	})
}

// lateReaper lets a test build the reaper after the rig that needs it as an option.
type lateReaper struct {
	mu sync.Mutex
	r  NodeObserver
}

func (l *lateReaper) set(r NodeObserver) {
	l.mu.Lock()
	l.r = r
	l.mu.Unlock()
}

func (l *lateReaper) Observe(ctx context.Context, envID string, list dockerx.ClusterRead, answering string) {
	l.mu.Lock()
	r := l.r
	l.mu.Unlock()
	if r != nil {
		r.Observe(ctx, envID, list, answering)
	}
}

// The service references of the last read are kept, so the networks list can count the
// services that mention a network without asking a manager (plan 04-13 task 2, D-41).
func TestARefreshKeepsTheServiceReferencesOfTheManagersAnswer(t *testing.T) {
	services := []dockertest.FakeService{
		{ID: "s1", Name: "web", Mode: "replicated", Running: 1, Desired: 1, Networks: []string{"ovl-id"},
			Mounts: []dockertest.FakeServiceMount{{Type: "volume", Source: "data"}}},
		{ID: "s2", Name: "idle", Mode: "replicated", Running: 0, Desired: 0, Networks: []string{"ovl-id"}},
	}
	s := newSwarmRig(t, managerNodes(), services)
	if err := s.c.Refresh(context.Background(), s.env.ID); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	st, ok := s.c.Cached(s.env.ID)
	if !ok || len(st.ServiceRefs) != 2 {
		t.Fatalf("state = %+v (%v), want two service references", st, ok)
	}
	if !reflect.DeepEqual(st.ServiceRefs, st.Read.ServiceRefs) {
		t.Errorf("ServiceRefs = %+v, want the read's %+v", st.ServiceRefs, st.Read.ServiceRefs)
	}
	var idle dockerx.ServiceRef
	for _, r := range st.ServiceRefs {
		if r.Name == "idle" {
			idle = r
		}
	}
	if len(idle.Networks) != 1 || idle.Networks[0] != "ovl-id" {
		t.Errorf("idle = %+v, want the network it mentions with no task running", idle)
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	swarmapi "github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/agents"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/tunnel"
	"github.com/ianglennon/tierplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/tierplan/proto/testsupport"
	"github.com/ianglennon/tierplan/proto/wire"
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
	s := newSwarmRigBare(t, nodes, reaper)
	for i := 1; i <= 3; i++ {
		s.m[i-1] = s.connect(t, s.env, managerConfig(i, nodes, services), managerFacts(i))
	}
	s.whenOnline(t, s.env.ID)
	return s
}

// newSwarmRigBare is the swarm rig before any manager has connected.
func newSwarmRigBare(t *testing.T, nodes []dockertest.FakeNode, reaper NodeObserver) *swarmRig {
	t.Helper()
	r := newTunRig(t, nil)
	r.pastGrace()
	s := &swarmRig{tunRig: r, nodes: nodes}
	s.env = r.srv.NewEnvironment(t, "Swarm")
	s.c = NewClusters(r.svc, r.snaps, ClusterOptions{
		Now: r.clk.Now, Publisher: r.brk, Events: r.evs, Grace: 2 * time.Minute, Reaper: reaper,
	})
	r.svc.WithClusters(s.c)
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
	awaitProbed(t, r.mon, AgentTarget(a.id))
}

func (r *tunRig) kick(t *testing.T, a *agentConn) {
	t.Helper()
	r.srv.Hub.Kick(a.id, tunnel.DisconnectClosed)
	r.awaitDisconnected(t, a.conn.Identity.EnvID, a.id)
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

// TestAnEnvironmentReadsOnlineBeforeTheLeaderHasAnsweredAndReadsGoToTheLeaderOnlyOnceItHas
// pins the readiness rule the swarm rigs follow: an environment reads online as soon as
// one target has probed, but a cluster read offers only managers whose monitor holds an
// answering client. The leader's first probe is held, so the race is not left to timing.
func TestAnEnvironmentReadsOnlineBeforeTheLeaderHasAnsweredAndReadsGoToTheLeaderOnlyOnceItHas(t *testing.T) {
	s := newSwarmRigBare(t, managerNodes(), nil)
	ctx := context.Background()
	// The held probe must not hit the monitor's own timeout however long the test is starved.
	s.mon.probeTimeout = 10 * time.Minute
	hold := make(chan struct{})
	released := false
	release := func() {
		if !released {
			released = true
			close(hold)
		}
	}
	t.Cleanup(release)
	nodes := managerNodes()
	s.m[0] = s.connect(t, s.env, managerConfig(1, nodes, nil), managerFacts(1))
	leaderCfg := managerConfig(2, nodes, nil)
	leaderCfg.ProbeHold = hold
	s.m[1] = s.dial(t, s.env, leaderCfg, managerFacts(2))
	s.m[2] = s.connect(t, s.env, managerConfig(3, nodes, nil), managerFacts(3))

	s.whenOnline(t, s.env.ID)
	if st := s.mon.Status(AgentTarget(s.m[1].id)); st.State != StateUnknown {
		t.Fatalf("the leader's target reads %s although its probe is held, want unknown", st.State)
	}
	if err := s.c.Refresh(ctx, s.env.ID); err != nil {
		t.Fatalf("Refresh with the leader still unprobed: %v", err)
	}
	read := func() string {
		var host string
		if err := s.c.ClusterRead(ctx, s.env.ID, func(cli *dockerx.Client) error {
			host = probeHost(t, cli)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return host
	}
	if host := read(); host != "mgr-1" && host != "mgr-3" {
		t.Fatalf("the read went to %q while the leader's client is not ready, want mgr-1 or mgr-3", host)
	}

	release()
	if st := awaitProbed(t, s.mon, AgentTarget(s.m[1].id)); st.State != StateOnline {
		t.Fatalf("the leader's target reads %s after its probe was released, want online", st.State)
	}
	if host := read(); host != "mgr-2" {
		t.Fatalf("the read went to %q once the leader had answered, want the leader mgr-2", host)
	}
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
		found = found || row.Type == "tierplan" && row.Name == msg
	}
	if !found {
		t.Errorf("the feed of the manager now in use has no tierplan row saying %q: %+v", msg, s.evs.Recent(AgentTarget(fo[0].ToAgentID), 10))
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
	s.awaitHealth(t, s.env.ID, "mgr-2 to be back and the environment online", func(e Environment) bool {
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

// StreamOnManager (plan 06-29, D-37) is a read for a stream the caller keeps open for as
// long as it likes: it goes to the manager a cluster read would choose, runs once and never
// moves to another, because a stream that dropped has already shown part of its answer.
func TestStreamOnManagerRunsOnceOnTheLeaderAndNeverFailsOverToAnotherManager(t *testing.T) {
	s := newSwarmRig(t, managerNodes(), nil)
	ctx := context.Background()
	if err := s.c.Refresh(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}

	var host string
	var calls atomic.Int32
	if err := s.c.StreamOnManager(ctx, s.env.ID, func(cli *dockerx.Client) error {
		calls.Add(1)
		host = probeHost(t, cli)
		return nil
	}); err != nil || calls.Load() != 1 || host != "mgr-2" {
		t.Fatalf("StreamOnManager = %v after %d calls on %q, want one call on the leader mgr-2", err, calls.Load(), host)
	}

	// A plain error comes back as it is.
	boom := errors.New("boom")
	calls.Store(0)
	if err := s.c.StreamOnManager(ctx, s.env.ID, func(*dockerx.Client) error { calls.Add(1); return boom }); !errors.Is(err, boom) || calls.Load() != 1 {
		t.Fatalf("a plain error: %v after %d calls, want boom after 1", err, calls.Load())
	}

	// A tunnel error is not retried either: ClusterRead would run again on another manager.
	calls.Store(0)
	err := s.c.StreamOnManager(ctx, s.env.ID, func(*dockerx.Client) error {
		calls.Add(1)
		return fmt.Errorf("lost: %w", tunnel.ErrAgentGone)
	})
	if !errors.Is(err, tunnel.ErrAgentGone) || calls.Load() != 1 {
		t.Fatalf("a tunnel error: %v after %d calls, want the tunnel error after exactly 1", err, calls.Load())
	}
	if len(s.failovers(t)) != 0 || s.failoverAudits(t, s.env.ID) != 0 {
		t.Error("a stream that was not retried produced a failover record")
	}
}

func TestStreamOnManagerRunsOnALocalSwarmManagersSocket(t *testing.T) {
	local := dockertest.NewFake(t, managerConfig(1, managerNodes(), nil))
	r := newTunRig(t, local)
	r.pastGrace()
	c := NewClusters(r.svc, r.snaps, ClusterOptions{Now: r.clk.Now, Publisher: r.brk, Events: r.evs})
	r.svc.WithClusters(c)
	env, err := r.svc.AddLocal(context.Background(), admin, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	r.whenOnline(t, env.ID)
	calls := 0
	var host string
	err = c.StreamOnManager(context.Background(), env.ID, func(cli *dockerx.Client) error {
		calls++
		host = probeHost(t, cli)
		return nil
	})
	if err != nil || calls != 1 || host != "mgr-1" {
		t.Fatalf("StreamOnManager = %v after %d calls on %q, want one call on the local socket's engine", err, calls, host)
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
	r.awaitSessions(t, workers[0].id, workers[1].id)
	e = r.awaitHealth(t, env.ID, "the workers to be probed", func(e Environment) bool {
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

func TestClusterReadRunsOnALocalSwarmManager(t *testing.T) {
	local := dockertest.NewFake(t, managerConfig(1, managerNodes(), []dockertest.FakeService{
		{ID: "s1", Name: "web", Mode: "replicated", Running: 2, Desired: 2},
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
	calls := 0
	var host string
	err = c.ClusterRead(context.Background(), env.ID, func(cli *dockerx.Client) error {
		calls++
		host = probeHost(t, cli)
		return nil
	})
	if err != nil || calls != 1 || host != "mgr-1" {
		t.Fatalf("ClusterRead = %v after %d calls on %q, want one call on the local socket's engine", err, calls, host)
	}
	// An error of fn comes back as it is and is not run again: there is no other manager.
	boom := errors.New("boom")
	calls = 0
	if err := c.ClusterRead(context.Background(), env.ID, func(*dockerx.Client) error { calls++; return boom }); !errors.Is(err, boom) || calls != 1 {
		t.Errorf("ClusterRead = %v after %d calls, want boom after one", err, calls)
	}
}

func TestRunRefreshesEnvironmentsOnAnIntervalAndStopsWhenItsContextEnds(t *testing.T) {
	s := newSwarmRig(t, managerNodes(), nil)
	// The observer is shown every node list the background refresh has read, after the read has
	// been cached, which is the signal that the refresh has done its work.
	listed := make(chan struct{}, 1)
	c := NewClusters(s.svc, s.snaps, ClusterOptions{
		Now: s.clk.Now, Publisher: s.brk, Events: s.evs, Interval: 10 * time.Millisecond,
		Reaper: nodeListSignal(func() {
			select {
			case listed <- struct{}{}:
			default:
			}
		}),
	})
	s.svc.WithClusters(c)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	testsupport.Await(t, "the background refresh to read the cluster", listed)
	if st, ok := c.Cached(s.env.ID); !ok || len(st.Read.Nodes) != 3 {
		t.Errorf("after the background refresh the cached state is %+v (known %v), want the three nodes", st.Read.Nodes, ok)
	}
	cancel()
	select {
	case <-done:
	case <-testsupport.Context(t).Done():
		t.Fatal("Run did not return after its context ended")
	}
}

func TestSwarmNodeIDsAreTheCachedNodeListAndAbsentWithoutOneOrWithoutAQuorum(t *testing.T) {
	// No manager has answered yet: nothing is held.
	bare := newSwarmRigBare(t, managerNodes(), nil)
	if ids, at, ok := bare.c.SwarmNodeIDs(bare.env.ID); ok || ids != nil || !at.IsZero() {
		t.Fatalf("before any read: %v %v %v, want no list", ids, at, ok)
	}

	s := newSwarmRig(t, managerNodes(), nil)
	ctx := context.Background()
	if err := s.c.Refresh(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}
	ids, at, ok := s.c.SwarmNodeIDs(s.env.ID)
	st, _ := s.c.Cached(s.env.ID)
	slices.Sort(ids)
	if !ok || !slices.Equal(ids, []string{"node-m1", "node-m2", "node-m3"}) || at.IsZero() || !at.Equal(st.FetchedAt) {
		t.Fatalf("after a read: %v %v %v, want the three nodes and the time of the read (%v)", ids, at, ok, st.FetchedAt)
	}
	if _, _, ok := s.c.SwarmNodeIDs("no-such-environment"); ok {
		t.Error("an unknown environment has a node list")
	}

	// A list that leaves out the manager that answered it is not believed, as the reaper
	// does not believe it (T-02-68): it decides no enrolment, although the read succeeded.
	answerless := []dockertest.FakeNode{{ID: "node-w1", Hostname: "wrk-1", Role: "worker", EngineVersion: "29.1.0", OS: "linux"}}
	for i, a := range s.m {
		a.fake.Reconfigure(managerConfig(i+1, answerless, nil))
	}
	if err := s.c.Refresh(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.c.Cached(s.env.ID); !st.Known() || len(st.Read.Nodes) != 1 || st.AnsweredBy == "" {
		t.Fatalf("the read of a list without its answerer: %+v, want it held, with its answerer", st)
	}
	if ids, _, ok := s.c.SwarmNodeIDs(s.env.ID); ok || ids != nil {
		t.Errorf("a list that leaves out its answerer: %v %v, want no list", ids, ok)
	}
	for i, a := range s.m {
		a.fake.Reconfigure(managerConfig(i+1, s.nodes, nil))
	}
	if err := s.c.Refresh(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.c.SwarmNodeIDs(s.env.ID); !ok {
		t.Fatal("the list names its answerer again, want it believed")
	}

	// A quorum that is lost makes the held list no basis for a refusal.
	for i, a := range s.m {
		cfg := managerConfig(i+1, s.nodes, nil)
		cfg.QuorumLost = true
		a.fake.Reconfigure(cfg)
	}
	if err := s.c.Refresh(ctx, s.env.ID); err == nil || ReasonFor(err) != ReasonNoQuorum {
		t.Fatalf("Refresh = %v, want no_quorum", err)
	}
	if ids, _, ok := s.c.SwarmNodeIDs(s.env.ID); ok || ids != nil {
		t.Errorf("while the quorum is lost: %v %v, want no list", ids, ok)
	}
}

// An enrolment whose node the held list lacks has the cluster read again, at most once per
// minAge and never while a read is running, so a node that just joined is found (review
// 261113-ota IN-03).
func TestRefreshSwarmNodeIDsReadsAgainOnlyWhenTheLastAttemptIsOldEnough(t *testing.T) {
	if _, _, ok := newSwarmRigBare(t, managerNodes(), nil).c.RefreshSwarmNodeIDs(context.Background(), "no-such-environment", 5*time.Second); ok {
		t.Fatal("an environment never read has a list")
	}
	s := newSwarmRig(t, managerNodes(), nil)
	ctx := context.Background()
	if err := s.c.Refresh(ctx, s.env.ID); err != nil {
		t.Fatal(err)
	}
	// A node joins the swarm after the read.
	joined := managerNodes(dockertest.FakeNode{ID: "node-w9", Hostname: "wrk-9", Role: "worker", EngineVersion: "29.1.0", OS: "linux"})
	for i, a := range s.m {
		a.fake.Reconfigure(managerConfig(i+1, joined, nil))
	}

	// The last attempt is newer than minAge: no read, the held list answers.
	ids, _, ok := s.c.RefreshSwarmNodeIDs(ctx, s.env.ID, time.Hour)
	if !ok || slices.Contains(ids, "node-w9") {
		t.Fatalf("within minAge: %v %v, want the held list without the new node", ids, ok)
	}
	// A read is running: no second one, and no wait for it.
	e, _ := s.c.lookup(s.env.ID)
	e.refresh.Lock()
	ids, _, _ = s.c.RefreshSwarmNodeIDs(ctx, s.env.ID, 0)
	e.refresh.Unlock()
	if slices.Contains(ids, "node-w9") {
		t.Fatalf("while a read runs: %v, want the held list", ids)
	}
	// Old enough: one read, and the new node is in the list.
	before, _ := s.c.Cached(s.env.ID)
	s.clk.Advance(5 * time.Second)
	ids, at, ok := s.c.RefreshSwarmNodeIDs(ctx, s.env.ID, 5*time.Second)
	if !ok || !slices.Contains(ids, "node-w9") || !at.After(before.FetchedAt) {
		t.Fatalf("after minAge: %v %v %v, want a new read that holds the new node", ids, at, ok)
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
	s.awaitSessions(t, w.id)
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
	case <-testsupport.Context(t).Done():
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
	s.awaitHealth(t, s.env.ID, "the environment to be online without the removed worker", func(e Environment) bool {
		return e.Health.State == StateOnline
	})
}

// nodeListSignal is a NodeObserver that only says a node list was read.
type nodeListSignal func()

func (f nodeListSignal) Observe(context.Context, string, dockerx.ClusterRead, string) { f() }

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

// Restart-loop detection in the cluster read (plan 06-34, RES-13, D-14). The clock is the
// rig's; nothing waits for a real window.

// loopRig is a swarm rig with the service web and a way to change the task history every
// manager reports. The cluster reader runs on its own clock, which the test moves by whole
// windows: the tunnel hub's clock stays put, because a heartbeat that is minutes old
// would end the agents' sessions.
type loopRig struct {
	*swarmRig
	services []dockertest.FakeService
	clock    *tunneltest.Clock
}

func newLoopRig(t *testing.T) *loopRig {
	t.Helper()
	services := []dockertest.FakeService{{ID: "svc-web", Name: "web", Mode: "replicated", Running: 1, Desired: 1}}
	l := &loopRig{swarmRig: newSwarmRig(t, managerNodes(), services), services: services}
	l.clock = tunneltest.NewClock(l.clk.Now())
	l.c = NewClusters(l.svc, l.snaps, ClusterOptions{
		Now: l.clock.Now, Publisher: l.brk, Events: l.evs, Grace: 2 * time.Minute,
	})
	l.svc.WithClusters(l.c)
	return l
}

// history gives every manager the failed tasks of web that failed the given durations
// before now, newest first by argument order, and a history limit.
func (l *loopRig) history(limit int, ago ...time.Duration) {
	now := l.clock.Now()
	var tasks []swarmapi.Task
	for i, d := range ago {
		tk := swarmapi.Task{ID: fmt.Sprintf("task-%d-%d", i, d/time.Second), ServiceID: "svc-web", NodeID: "node-m1", Slot: 1, DesiredState: swarmapi.TaskStateShutdown}
		tk.Status = swarmapi.TaskStatus{State: swarmapi.TaskStateFailed, Timestamp: now.Add(-d), Err: fmt.Sprintf("task: non-zero exit (%d)", i+1)}
		tasks = append(tasks, tk)
	}
	for i, a := range l.m {
		cfg := managerConfig(i+1, l.nodes, l.services)
		cfg.Tasks, cfg.SwarmClusterID, cfg.HistoryLimit = tasks, "cluster-1", limit
		a.fake.Reconfigure(cfg)
	}
}

func (l *loopRig) refresh(t *testing.T) ClusterState {
	t.Helper()
	if err := l.c.Refresh(context.Background(), l.env.ID); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	st, ok := l.c.Cached(l.env.ID)
	if !ok {
		t.Fatal("no cluster state after a refresh")
	}
	return st
}

func TestAClusterReadFlagsARestartLoopAndTheFlagOutlivesDockersHistoryUntilAQuietWindow(t *testing.T) {
	l := newLoopRig(t)
	l.history(8, time.Minute, 2*time.Minute, 3*time.Minute)
	st := l.refresh(t)
	loop, ok := st.RestartLoops["svc-web"]
	if !ok || loop.Count != 3 || loop.Name != "web" || loop.LatestError != "task: non-zero exit (1)" || len(st.RestartLoops) != 1 {
		t.Fatalf("restart loops = %+v, want web with 3 failures and the newest task's error", st.RestartLoops)
	}
	if st.HistoryLimit != 8 {
		t.Errorf("history limit = %d, want the cluster's 8", st.HistoryLimit)
	}

	// Docker prunes its history; the three failures are still inside the window.
	l.history(8)
	l.clock.Advance(30 * time.Second)
	if st := l.refresh(t); st.RestartLoops["svc-web"].Count != 3 {
		t.Fatalf("after Docker pruned its history = %+v, want the 3 seen failures kept", st.RestartLoops)
	}

	// A full window with nothing new clears it.
	l.clock.Advance(DefaultRestartLoopWindow)
	if st := l.refresh(t); len(st.RestartLoops) != 0 {
		t.Fatalf("a quiet window later = %+v, want cleared", st.RestartLoops)
	}
}

func TestARestartLoopUsesTheEnvironmentsThresholdAndWindow(t *testing.T) {
	l := newLoopRig(t)
	l.history(5, time.Minute, 2*time.Minute)
	if st := l.refresh(t); len(st.RestartLoops) != 0 {
		t.Fatalf("two failures flagged %+v at the default threshold of 3", st.RestartLoops)
	}
	if _, err := l.st.Write.Exec(`UPDATE environments SET restart_loop_failures = 2 WHERE id = ?`, l.env.ID); err != nil {
		t.Fatal(err)
	}
	if st := l.refresh(t); st.RestartLoops["svc-web"].Count != 2 {
		t.Fatalf("with a threshold of 2 = %+v, want a loop of 2", st.RestartLoops)
	}
	// Tighten the window to one minute: the failures are two and three minutes old.
	l.history(5, 2*time.Minute, 3*time.Minute)
	if _, err := l.st.Write.Exec(`UPDATE environments SET restart_loop_window_minutes = 1 WHERE id = ?`, l.env.ID); err != nil {
		t.Fatal(err)
	}
	l.clock.Advance(time.Minute)
	if st := l.refresh(t); len(st.RestartLoops) != 0 {
		t.Fatalf("with a window of one minute = %+v, want none", st.RestartLoops)
	}
}

func TestALostQuorumHidesTheRestartLoopsAndTheyReturnWhenItComesBack(t *testing.T) {
	l := newLoopRig(t)
	l.history(5, time.Minute, 2*time.Minute, 3*time.Minute)
	if st := l.refresh(t); len(st.RestartLoops) != 1 {
		t.Fatalf("not flagged before the quorum loss: %+v", st.RestartLoops)
	}
	for i, a := range l.m {
		cfg := managerConfig(i+1, l.nodes, l.services)
		cfg.QuorumLost = true
		a.fake.Reconfigure(cfg)
	}
	_ = l.c.Refresh(context.Background(), l.env.ID)
	if st, _ := l.c.Cached(l.env.ID); st.RestartLoops != nil {
		t.Fatalf("restart loops while the quorum is lost = %+v, want none shown", st.RestartLoops)
	}
	l.history(5)
	l.clock.Advance(15 * time.Second)
	if st := l.refresh(t); st.RestartLoops["svc-web"].Count != 3 {
		t.Fatalf("after the quorum came back = %+v, want the loop kept in memory", st.RestartLoops)
	}
}

// Mutes in the cluster read (plan 06-35, RES-13, D-16).

func (l *loopRig) mute(t *testing.T, serviceID string) {
	t.Helper()
	if err := l.st.Q.InsertRestartLoopMute(context.Background(), sqlc.InsertRestartLoopMuteParams{
		EnvID: l.env.ID, Kind: "service", TargetID: serviceID, Name: serviceID, MutedBy: "user-1", MutedAt: l.clock.Now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
}

func (l *loopRig) muteRows(t *testing.T) []string {
	t.Helper()
	rows, err := l.st.RQ.ListRestartLoopMutes(context.Background(), l.env.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.TargetID)
	}
	return ids
}

func TestAMuteRowMarksTheFlaggedServiceMutedAndNothingElseAboutItChanges(t *testing.T) {
	l := newLoopRig(t)
	l.history(8, time.Minute, 2*time.Minute, 3*time.Minute)
	l.mute(t, "svc-web")
	st := l.refresh(t)
	loop, ok := st.RestartLoops["svc-web"]
	if !ok || !loop.Muted || loop.Count != 3 {
		t.Fatalf("restart loops = %+v, want web still flagged with its 3 failures, and muted", st.RestartLoops)
	}
	if !st.MutedServices["svc-web"] {
		t.Errorf("muted services = %v, want web", st.MutedServices)
	}
}

func TestAMutedServiceStaysMutedWhileItIsNotLoopingAndTheMuteIsKept(t *testing.T) {
	l := newLoopRig(t)
	l.history(8)
	l.mute(t, "svc-web")
	st := l.refresh(t)
	if len(st.RestartLoops) != 0 || !st.MutedServices["svc-web"] {
		t.Fatalf("loops = %+v, muted = %v, want no loop and web still muted", st.RestartLoops, st.MutedServices)
	}
	if got := l.muteRows(t); len(got) != 1 {
		t.Errorf("mute rows = %v, want the mute kept for when the loop returns", got)
	}
}

func TestASuccessfulReadDeletesTheMutesOfServicesThatNoLongerExist(t *testing.T) {
	l := newLoopRig(t)
	l.history(8)
	l.mute(t, "svc-web")
	l.mute(t, "svc-gone")
	st := l.refresh(t)
	if got := l.muteRows(t); len(got) != 1 || got[0] != "svc-web" {
		t.Fatalf("mute rows = %v, want only web's", got)
	}
	if st.MutedServices["svc-gone"] {
		t.Errorf("muted services = %v, want the gone service dropped", st.MutedServices)
	}
}

func TestSettingAMuteUpdatesWhatIsServedAtOnceAndLeavesTheCopiesAlreadyHandedOutAlone(t *testing.T) {
	l := newLoopRig(t)
	l.history(8, time.Minute, 2*time.Minute, 3*time.Minute)
	before := l.refresh(t)
	if before.RestartLoops["svc-web"].Muted {
		t.Fatal("web muted before anyone muted it")
	}
	l.c.SetRestartLoopMute(l.env.ID, "svc-web", true)
	after, _ := l.c.Cached(l.env.ID)
	if !after.RestartLoops["svc-web"].Muted || !after.MutedServices["svc-web"] {
		t.Fatalf("after a mute = %+v / %v, want web muted without waiting for the next read", after.RestartLoops, after.MutedServices)
	}
	if before.RestartLoops["svc-web"].Muted {
		t.Error("a state handed out earlier changed: the maps are shared, a mute must replace them")
	}
	l.c.SetRestartLoopMute(l.env.ID, "svc-web", false)
	back, _ := l.c.Cached(l.env.ID)
	if back.RestartLoops["svc-web"].Muted || back.MutedServices["svc-web"] {
		t.Errorf("after an unmute = %+v / %v, want web not muted", back.RestartLoops, back.MutedServices)
	}
}

// Restart-loop detection of standalone containers in the cluster read (plan 06-36, RES-13,
// D-15, D-16). The die events are the ones the environment's feed holds, put there with
// Inject; the cluster reader runs on its own clock, which the test moves.

// die puts a die event of the container name into the feed of the target, dated ago before
// the reader's clock. The feed starts asynchronously, so it retries until the feed exists.
func dieIn(t *testing.T, hub *Hub, target string, now time.Time, id, name, containerID string, ago time.Duration, exit string) {
	t.Helper()
	at := now.Add(-ago)
	ev := dockerx.EventRow{
		ID: id, Time: at, TimeNano: at.UnixNano(), Type: "container", Action: "die", ActorID: containerID, Name: name,
		Attributes: map[string]string{"name": name, "exitCode": exit},
	}
	if err := hub.Await(testsupport.Context(t), func() bool { return hub.Inject(target, ev) }); err != nil {
		t.Fatalf("the target's event feed never opened: %v", err)
	}
}

// writesTo are the paths a daemon served that change a container.
func writesTo(a *agentConn) []string {
	var out []string
	for _, p := range a.fake.Requests() {
		for _, verb := range []string{"/restart", "/stop", "/kill", "/pause", "/rename", "/update"} {
			if strings.HasSuffix(p, verb) {
				out = append(out, p)
			}
		}
	}
	return out
}

// standaloneLoopRig is a standalone host with a crash-looping container, a calm one and
// the task container of a swarm service, all with a restart policy of always.
type standaloneLoopRig struct {
	*tunRig
	c     *Clusters
	clock *tunneltest.Clock
	env   tunneltest.Environment
	edge  *agentConn
	key   string
}

func newStandaloneLoopRig(t *testing.T) *standaloneLoopRig {
	t.Helper()
	r := newTunRig(t, nil)
	r.pastGrace()
	g := &standaloneLoopRig{tunRig: r, clock: tunneltest.NewClock(r.clk.Now())}
	g.c = NewClusters(r.svc, r.snaps, ClusterOptions{Now: g.clock.Now, Publisher: r.brk, Events: r.evs})
	r.svc.WithClusters(g.c)
	g.env = r.srv.NewEnvironment(t, "Standalone")
	g.edge = r.connect(t, g.env, dockertest.FakeConfig{
		Hostname: "edge-1",
		Containers: []dockertest.FakeContainer{
			{ID: "ctr-crashy", Names: []string{"/crashy"}, State: "restarting", Status: "Restarting (3) 3 seconds ago"},
			{ID: "ctr-calm", Names: []string{"/calm"}, State: "running", Status: "Up 2 hours"},
			{ID: "ctr-task", Names: []string{"/web.1.abcdef"}, State: "restarting", Status: "Restarting (1) 3 seconds ago",
				Labels: map[string]string{"com.docker.swarm.task.id": "abcdef"}},
		},
		RestartPolicies: map[string]string{"ctr-crashy": "always", "ctr-calm": "always", "ctr-task": "always"},
	}, nil)
	r.whenOnline(t, g.env.ID)
	g.key = AgentTarget(g.edge.id)
	return g
}

func (g *standaloneLoopRig) refresh(t *testing.T) ClusterState {
	t.Helper()
	if err := g.c.Refresh(context.Background(), g.env.ID); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	st, ok := g.c.Cached(g.env.ID)
	if !ok {
		t.Fatal("no cluster state after a refresh")
	}
	return st
}

func (g *standaloneLoopRig) dies(t *testing.T, prefix, name, containerID string, n int, exit string) {
	t.Helper()
	for i := range n {
		dieIn(t, g.evs, g.key, g.clock.Now(), fmt.Sprintf("%s-%d", prefix, i), name, containerID, time.Duration(i+1)*time.Minute, exit)
	}
}

func TestAStandaloneHostFlagsAContainerThatKeepsDyingAndNeverATaskContainerOrACalmOne(t *testing.T) {
	g := newStandaloneLoopRig(t)
	g.dies(t, "crashy", "crashy", "ctr-crashy", 3, "3")
	g.dies(t, "calm", "calm", "ctr-calm", 3, "0")
	g.dies(t, "task", "web.1.abcdef", "ctr-task", 4, "1")
	st := g.refresh(t)
	key := ContainerLoopKey(g.edge.id, "crashy")
	loop, ok := st.ContainerLoops[key]
	if !ok || len(st.ContainerLoops) != 1 {
		t.Fatalf("container loops = %+v, want crashy only", st.ContainerLoops)
	}
	if loop.Count != 3 || loop.LastExitCode != "3" || loop.ContainerID != "ctr-crashy" || loop.NodeID != g.edge.id || loop.Window != DefaultRestartLoopWindow {
		t.Errorf("loop = %+v, want 3 deaths, exit code 3, on this node, in the default window", loop)
	}
	if st.RestartLoops != nil {
		t.Errorf("service loops = %+v, want none on a standalone host", st.RestartLoops)
	}
	if bad := writesTo(g.edge); len(bad) != 0 {
		t.Errorf("detection changed containers: %v", bad)
	}
}

func TestAContainerLoopIsKeptAcrossReadsAndClearsAfterAQuietWindow(t *testing.T) {
	g := newStandaloneLoopRig(t)
	g.dies(t, "crashy", "crashy", "ctr-crashy", 3, "3")
	if st := g.refresh(t); len(st.ContainerLoops) != 1 {
		t.Fatalf("container loops = %+v, want the loop", st.ContainerLoops)
	}
	g.clock.Advance(30 * time.Second)
	if st := g.refresh(t); st.ContainerLoops[ContainerLoopKey(g.edge.id, "crashy")].Count == 0 {
		t.Fatalf("container loops = %+v, want the same loop on the next read", st.ContainerLoops)
	}
	g.clock.Advance(DefaultRestartLoopWindow + time.Minute)
	if st := g.refresh(t); len(st.ContainerLoops) != 0 {
		t.Fatalf("a quiet window later = %+v, want cleared", st.ContainerLoops)
	}
}

func TestAContainerLoopUsesTheEnvironmentsThresholdAndNeverInspectsTwice(t *testing.T) {
	g := newStandaloneLoopRig(t)
	g.dies(t, "crashy", "crashy", "ctr-crashy", 2, "3")
	if st := g.refresh(t); len(st.ContainerLoops) != 0 {
		t.Fatalf("two deaths flagged %+v at the default threshold of 3", st.ContainerLoops)
	}
	if _, err := g.st.Write.Exec(`UPDATE environments SET restart_loop_failures = 2 WHERE id = ?`, g.env.ID); err != nil {
		t.Fatal(err)
	}
	if st := g.refresh(t); st.ContainerLoops[ContainerLoopKey(g.edge.id, "crashy")].Count != 2 {
		t.Fatalf("with a threshold of 2 = %+v, want a loop of 2", st.ContainerLoops)
	}
	n := len(inspectRequests(g.edge))
	g.refresh(t)
	if got := len(inspectRequests(g.edge)); got != n {
		t.Errorf("a third read inspected %d more containers, want none: the policy is remembered by container ID", got-n)
	}
}

func TestASwarmEnvironmentFlagsAStandaloneContainerOnANodeAndInspectsOnlyThatOne(t *testing.T) {
	l := newLoopRig(t)
	cfg := managerConfig(1, l.nodes, l.services)
	cfg.Containers = []dockertest.FakeContainer{
		{ID: "ctr-crashy", Names: []string{"/crashy"}, State: "restarting", Status: "Restarting (2) 3 seconds ago"},
		{ID: "ctr-calm", Names: []string{"/calm"}, State: "running", Status: "Up 2 hours"},
		{ID: "ctr-task", Names: []string{"/web.1.abcdef"}, State: "restarting", Labels: map[string]string{"com.docker.swarm.task.id": "abcdef"}},
	}
	cfg.RestartPolicies = map[string]string{"ctr-crashy": "unless-stopped", "ctr-calm": "always", "ctr-task": "always"}
	l.m[0].fake.Reconfigure(cfg)
	target := AgentTarget(l.m[0].id)
	now := l.clock.Now()
	for i := range 3 {
		dieIn(t, l.evs, target, now, fmt.Sprintf("x%d", i), "crashy", "ctr-crashy", time.Duration(i+1)*time.Minute, "2")
		dieIn(t, l.evs, target, now, fmt.Sprintf("t%d", i), "web.1.abcdef", "ctr-task", time.Duration(i+1)*time.Minute, "1")
		dieIn(t, l.evs, target, now, fmt.Sprintf("c%d", i), "calm", "ctr-calm", time.Duration(i+1)*time.Minute, "0")
	}
	st := l.refresh(t)
	key := ContainerLoopKey(l.m[0].id, "crashy")
	if loop, ok := st.ContainerLoops[key]; !ok || len(st.ContainerLoops) != 1 || loop.Count != 3 || loop.LastExitCode != "2" {
		t.Fatalf("container loops = %+v, want crashy on mgr-1 only", st.ContainerLoops)
	}
	inspected := inspectRequests(l.m[0])
	if len(inspected) != 1 || !strings.Contains(inspected[0], "ctr-crashy") {
		t.Errorf("inspects = %v, want only the container over the threshold", inspected)
	}
	if bad := writesTo(l.m[0]); len(bad) != 0 {
		t.Errorf("detection changed containers: %v", bad)
	}
}

func TestAContainerLoopSurvivesALostQuorumBecauseItNeedsNoCluster(t *testing.T) {
	l := newLoopRig(t)
	target := AgentTarget(l.m[0].id)
	now := l.clock.Now()
	cfg := managerConfig(1, l.nodes, l.services)
	cfg.Containers = []dockertest.FakeContainer{{ID: "ctr-crashy", Names: []string{"/crashy"}, State: "restarting"}}
	cfg.RestartPolicies = map[string]string{"ctr-crashy": "always"}
	cfg.QuorumLost = true
	l.m[0].fake.Reconfigure(cfg)
	for i := range 3 {
		dieIn(t, l.evs, target, now, fmt.Sprintf("x%d", i), "crashy", "ctr-crashy", time.Duration(i+1)*time.Minute, "2")
	}
	_ = l.c.Refresh(context.Background(), l.env.ID)
	st, _ := l.c.Cached(l.env.ID)
	if len(st.ContainerLoops) != 1 {
		t.Fatalf("container loops = %+v, want the loop even though the swarm has no quorum", st.ContainerLoops)
	}
}

// Mutes of containers in the cluster read (plan 06-36, D-16, A12).

func (g *standaloneLoopRig) mute(t *testing.T, key string) {
	t.Helper()
	if err := g.st.Q.InsertRestartLoopMute(context.Background(), sqlc.InsertRestartLoopMuteParams{
		EnvID: g.env.ID, Kind: "container", TargetID: key, Name: key, MutedBy: "user-1", MutedAt: g.clock.Now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAContainerMuteMarksTheFlaggedContainerMutedAndIsKeptWhileItIsQuiet(t *testing.T) {
	g := newStandaloneLoopRig(t)
	g.dies(t, "crashy", "crashy", "ctr-crashy", 3, "3")
	key, quiet := ContainerLoopKey(g.edge.id, "crashy"), ContainerLoopKey(g.edge.id, "gone-for-now")
	g.mute(t, key)
	g.mute(t, quiet)
	st := g.refresh(t)
	if loop := st.ContainerLoops[key]; !loop.Muted || loop.Count != 3 {
		t.Fatalf("container loops = %+v, want crashy still flagged and muted", st.ContainerLoops)
	}
	if !st.MutedContainers[key] || !st.MutedContainers[quiet] {
		t.Errorf("muted containers = %v, want both: a container recreated under its name stays muted", st.MutedContainers)
	}
	rows, err := g.st.RQ.ListRestartLoopMutes(context.Background(), g.env.ID)
	if err != nil || len(rows) != 2 {
		t.Errorf("mute rows = %+v (%v), want both kept", rows, err)
	}
	if len(st.MutedServices) != 0 {
		t.Errorf("muted services = %v, want a container mute to leave the services alone", st.MutedServices)
	}
}

func TestSettingAContainerMuteUpdatesWhatIsServedAtOnceAndLeavesTheCopiesAlreadyHandedOutAlone(t *testing.T) {
	g := newStandaloneLoopRig(t)
	g.dies(t, "crashy", "crashy", "ctr-crashy", 3, "3")
	key := ContainerLoopKey(g.edge.id, "crashy")
	before := g.refresh(t)
	if before.ContainerLoops[key].Muted {
		t.Fatal("crashy muted before anyone muted it")
	}
	g.c.SetContainerRestartLoopMute(g.env.ID, key, true)
	after, _ := g.c.Cached(g.env.ID)
	if !after.ContainerLoops[key].Muted || !after.MutedContainers[key] {
		t.Fatalf("after a mute = %+v / %v, want crashy muted without waiting for the next read", after.ContainerLoops, after.MutedContainers)
	}
	if before.ContainerLoops[key].Muted {
		t.Error("a state handed out earlier changed: the maps are shared, a mute must replace them")
	}
	g.c.SetContainerRestartLoopMute(g.env.ID, key, false)
	back, _ := g.c.Cached(g.env.ID)
	if back.ContainerLoops[key].Muted || back.MutedContainers[key] {
		t.Errorf("after an unmute = %+v / %v, want crashy not muted", back.ContainerLoops, back.MutedContainers)
	}
}

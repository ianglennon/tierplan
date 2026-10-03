// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/tunnel"
	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/bayplan/internal/vault"
	"github.com/ianglennon/bayplan/proto/wire"
)

// Agent targets feed the rollup and one aggregator serves every read (plan
// 02-11, ENV-06, ENV-08, D-33, D-41, D-47). The agents are real sessions of the
// real hub (tunneltest) answering from fake daemons, and the registry, monitor,
// snapshot cache and events hub are the production ones, on a clock the test moves.

type tunRig struct {
	srv   *tunneltest.Server
	st    *store.Store
	mon   *Monitor
	snaps *Snapshots
	evs   *Hub
	svc   *Service
	agg   *Aggregator
	brk   *Broker
	clk   *tunneltest.Clock
	start time.Time
}

// newTunRig builds the registry over the same store the tunnel's agents service
// writes. The server "started" at the clock's first reading. local is the daemon
// the local socket environment would use; nil means none.
func newTunRig(t *testing.T, local *dockertest.Fake) *tunRig {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	clk := tunneltest.NewClock(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	st := storetest.New(t)
	vlt, err := vault.LoadOrCreate("", filepath.Dir(st.Path), log)
	if err != nil {
		t.Fatal(err)
	}
	aud := audit.NewWriter(st, vlt, log, clk.Now)
	mon := NewMonitor(log, clk.Now, nil)
	dockerHost := ""
	if local != nil {
		dockerHost = "unix://" + local.SocketPath()
	}
	svc := NewService(st, aud, mon, dockerHost, "self-id", clk.Now)
	snaps := NewSnapshots(mon, 0, clk.Now)
	evs := NewHub(mon, snaps, log, clk.Now)
	r := &tunRig{st: st, mon: mon, snaps: snaps, evs: evs, svc: svc, clk: clk, start: clk.Now()}
	r.agg = NewAggregator(svc, snaps, evs, clk.Now)
	r.brk = NewBroker(svc, clk.Now)
	// Registered after Start's own cleanup would be wrong: these must run first, so
	// they are registered before the server.
	t.Cleanup(func() {
		r.brk.Close()
		evs.Close()
		mon.StopAll()
	})
	r.srv = tunneltest.Start(t, tunneltest.Options{
		Clock: clk, Store: st, Observer: svc, Notifier: r.brk,
		Configure: func(h *tunnel.Hub) *tunnel.Hub {
			return h.WithTiming(10*time.Second, 30*time.Second).WithWatchInterval(5 * time.Millisecond)
		},
	})
	svc.WithAgents(r.srv.Hub).WithHealth(HealthConfig{ServerStart: r.start})
	return r
}

// pastGrace moves the clock beyond the reconnect grace, before any agent connects
// (a jump with a session open would let the watchdog close it).
func (r *tunRig) pastGrace() { r.clk.Advance(2 * time.Minute) }

// agentConn is a connected agent with a fake daemon behind its tunnel.
type agentConn struct {
	conn    *tunneltest.Conn
	fake    *dockertest.Fake
	control interface{ Write([]byte) (int, error) }
	id      string
}

// connect enrols a new agent in env, serves its Docker streams from a fake
// daemon, and sends its first heartbeat.
func (r *tunRig) connect(t *testing.T, env tunneltest.Environment, cfg dockertest.FakeConfig, facts *wire.Facts) *agentConn {
	t.Helper()
	fake := dockertest.NewFake(t, cfg)
	if facts == nil {
		facts = &wire.Facts{EngineID: "ENG:" + cfg.Hostname, Hostname: cfg.Hostname, EngineReachable: true}
	}
	c := r.srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: facts})
	if c.Welcome == nil {
		t.Fatalf("enrolment refused: %+v", c.Refuse)
	}
	c.ProxyDocker(t, fake.SocketPath())
	control := c.OpenControl(t)
	tunneltest.SendHeartbeat(t, control, true, r.clk.Now().UnixMilli())
	return &agentConn{conn: c, fake: fake, control: control, id: c.Identity.AgentID}
}

func (r *tunRig) health(t *testing.T, envID string) Environment {
	t.Helper()
	e, err := r.svc.Get(context.Background(), envID)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// whenOnline waits until the environment is online and its target has probed.
func (r *tunRig) whenOnline(t *testing.T, envID string) Environment {
	t.Helper()
	var e Environment
	eventually(t, "the environment to be online with a probed target", func() bool {
		e = r.health(t, envID)
		return e.Health.State == StateOnline && e.Status.State == StateOnline
	})
	return e
}

func TestAStandaloneAgentEnvironmentIsPendingThenOnlineWithTheEngineFromItsTargetsProbe(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Remote")
	if e := r.health(t, env.ID); e.Health.State != StatePending || e.Status.State != StateUnknown {
		t.Fatalf("a new environment is %+v / %+v, want pending with nothing monitored", e.Health, e.Status)
	}

	a := r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1", Version: "28.4.0"}, nil)
	e := r.whenOnline(t, env.ID)
	if e.Status.Probe.EngineVersion != "28.4.0" || e.Status.Probe.Hostname != "edge-1" {
		t.Fatalf("engine = %+v, want the version and host the agent's daemon reported", e.Status.Probe)
	}
	if e.Health.Reason != "" || len(e.Health.Issues) != 0 {
		t.Fatalf("health = %+v, want plain online", e.Health)
	}
	// Monitoring is by agent: nothing is keyed by the environment, which has no socket.
	if st := r.mon.Status(env.ID); st.State != StateUnknown {
		t.Errorf("a monitor exists under the environment ID: %+v", st)
	}
	if st := r.mon.Status(AgentTarget(a.id)); st.State != StateOnline {
		t.Errorf("the agent's target is %+v, want online", st)
	}
}

func TestAfterAHeartbeatTimeoutTheEnvironmentIsOnlineWhileConnectedThenOfflineWithReasonTimeoutSinceTheDisconnect(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Remote")
	a := r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1"}, nil)
	r.srv.Events.WaitLen(t, 1)
	s := r.srv.Hub.Session(a.id)
	eventually(t, "the heartbeat", func() bool { return !s.Status().LastHeartbeat.IsZero() })
	r.whenOnline(t, env.ID)

	// Silent for 29 s: still connected, so still online.
	r.clk.Advance(29 * time.Second)
	time.Sleep(60 * time.Millisecond)
	if e := r.health(t, env.ID); e.Health.State != StateOnline {
		t.Fatalf("after 29 s of silence: %+v, want online", e.Health)
	}
	// Past the window the hub closes the session with the reason timeout.
	r.clk.Advance(2 * time.Second)
	closedAt := r.clk.Now()
	r.srv.Events.WaitLen(t, 2)
	e := r.health(t, env.ID)
	if e.Health.State != StateOffline || e.Health.Reason != ReasonTimeout || !e.Health.Since.Equal(closedAt) {
		t.Fatalf("health = %+v, want offline / timeout since the disconnect at %v", e.Health, closedAt)
	}
	if e.Status.State != StateUnknown {
		t.Errorf("the target of a gone agent still reports %+v", e.Status)
	}
}

func TestADropShorterThanTheWindowChangesNothingAndALongerOneIsOffline(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Remote")
	a := r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1"}, nil)
	r.srv.Events.WaitLen(t, 1)
	s := r.srv.Hub.Session(a.id)
	eventually(t, "the heartbeat", func() bool { return !s.Status().LastHeartbeat.IsZero() })
	r.whenOnline(t, env.ID)
	beat := s.Status().LastHeartbeat

	r.clk.Advance(5 * time.Second)
	droppedAt := r.clk.Now()
	_ = a.conn.WS.CloseNow()
	r.srv.Events.WaitLen(t, 2)

	// 29.999 s after the last heartbeat the agent still counts as present.
	r.clk.Advance(beat.Add(30*time.Second - time.Millisecond).Sub(r.clk.Now()))
	if e := r.health(t, env.ID); e.Health.State != StateOnline {
		t.Fatalf("29.999 s after the last heartbeat: %+v, want online", e.Health)
	}
	r.clk.Advance(time.Millisecond)
	e := r.health(t, env.ID)
	if e.Health.State != StateOffline || e.Health.Reason != ReasonAgentOffline || !e.Health.Since.Equal(droppedAt) {
		t.Fatalf("30 s after the last heartbeat: %+v, want offline / agent_offline since %v", e.Health, droppedAt)
	}
}

func TestAnEnvironmentWhoseAgentsHaveNotReconnectedAfterAServerStartIsReconnectingForTheGrace(t *testing.T) {
	r := newTunRig(t, nil)
	env := r.srv.NewEnvironment(t, "Remote")
	// The agent enrolled in an earlier run of the server: a row, no live session.
	a := r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1"}, nil)
	r.srv.Events.WaitLen(t, 1)
	_ = a.conn.WS.CloseNow()
	r.srv.Events.WaitLen(t, 2)
	r.srv.Hub.Forget(env.ID) // what a restart does to the hub's memory

	r.clk.Advance(60*time.Second - time.Millisecond)
	if e := r.health(t, env.ID); e.Health.State != StateReconnecting || !e.Health.Since.Equal(r.start) {
		t.Fatalf("at 59.999 s: %+v, want reconnecting since the server start", e.Health)
	}
	r.clk.Advance(time.Millisecond)
	if e := r.health(t, env.ID); e.Health.State != StateOffline || e.Health.Reason != ReasonAgentOffline {
		t.Fatalf("at 60 s: %+v, want offline / agent_offline", e.Health)
	}
}

func TestARevokedEnvironmentIsRevokedAndAnEnvironmentIsNeverDeletedForWaiting(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	waiting := r.srv.NewEnvironment(t, "Waiting")
	r.clk.Advance(24 * 365 * time.Hour) // a year with no agent
	if e := r.health(t, waiting.ID); e.Health.State != StatePending {
		t.Fatalf("a year later: %+v, want it still pending", e.Health)
	}
	if list, err := r.svc.List(context.Background()); err != nil || len(list) != 1 {
		t.Fatalf("List = %d environments, %v; want the pending one kept", len(list), err)
	}

	env := r.srv.NewEnvironment(t, "Remote")
	a := r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1"}, nil)
	r.srv.Events.WaitLen(t, 1)
	_ = a.conn.WS.CloseNow()
	r.srv.Events.WaitLen(t, 2)
	revokedAt := r.clk.Now()
	r.srv.Exec(t, `UPDATE agents SET status = 'revoked', revoked_at = ? WHERE id = ?`, revokedAt.UnixNano(), a.id)
	e := r.health(t, env.ID)
	if e.Health.State != StateRevoked || e.Health.Reason != ReasonRevoked || !e.Health.Since.Equal(revokedAt) {
		t.Fatalf("health = %+v, want revoked since %v", e.Health, revokedAt)
	}
}

func TestTheAggregatorReadsALocalEnvironmentThroughItsOwnKey(t *testing.T) {
	local := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "this-host", Containers: []dockertest.FakeContainer{{ID: "l1", Names: []string{"/on-this-host"}, State: "running"}}})
	r := newTunRig(t, local)
	e, err := r.svc.AddLocal(context.Background(), admin, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	agg, h, err := r.agg.Snapshot(context.Background(), e.ID)
	if err != nil || h.State != StateOnline {
		t.Fatalf("Snapshot = %v, %+v", err, h)
	}
	if len(agg.Containers) != 1 || agg.Containers[0].Name != "on-this-host" || agg.Target.Probe.Hostname != "this-host" {
		t.Fatalf("aggregate = %+v, want the local daemon's container", agg)
	}
}

func TestTheAggregatorReadsAStandaloneEnvironmentThroughItsAgentsTarget(t *testing.T) {
	local := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "this-host", Containers: []dockertest.FakeContainer{{ID: "l1", Names: []string{"/on-this-host"}, State: "running"}}})
	r := newTunRig(t, local)
	r.pastGrace()
	if _, err := r.svc.AddLocal(context.Background(), admin, "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	env := r.srv.NewEnvironment(t, "Remote")
	a := r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1", Containers: []dockertest.FakeContainer{
		{ID: "a1", Names: []string{"/web"}, State: "running"}, {ID: "a2", Names: []string{"/db"}, State: "exited"},
	}}, nil)
	r.whenOnline(t, env.ID)

	agg, h, err := r.agg.Snapshot(context.Background(), env.ID)
	if err != nil || h.State != StateOnline {
		t.Fatalf("Snapshot = %v, %+v", err, h)
	}
	if len(agg.Containers) != 2 || agg.Containers[0].Name != "db" || agg.Counts.Containers != 2 || agg.Counts.ContainersRunning != 1 {
		t.Fatalf("aggregate = %+v, want the two containers of the agent's daemon", agg.Snapshot)
	}
	if agg.Target.Probe.Hostname != "edge-1" || len(agg.NotReporting) != 0 {
		t.Fatalf("target = %+v, not reporting %v", agg.Target.Probe, agg.NotReporting)
	}
	if len(agg.Nodes) != 1 || agg.Nodes[0].ID != a.id || agg.Nodes[0].Name != "edge-1" {
		t.Fatalf("nodes = %+v, want the one agent", agg.Nodes)
	}
}

func TestTheAggregatorAnswersErrNotOnlineWithTheRolledUpHealthWhenNothingCanBeRead(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Remote")

	_, h, err := r.agg.Snapshot(context.Background(), env.ID)
	var no ErrNotOnline
	if !errors.As(err, &no) || no.Health.State != StatePending || h.State != StatePending {
		t.Fatalf("pending: %v / %+v, want ErrNotOnline carrying pending", err, h)
	}

	a := r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1"}, nil)
	r.whenOnline(t, env.ID)
	r.srv.Events.WaitLen(t, 1)
	_ = a.conn.WS.CloseNow()
	r.srv.Events.WaitLen(t, 2)
	r.clk.Advance(31 * time.Second)
	_, h, err = r.agg.Snapshot(context.Background(), env.ID)
	if !errors.As(err, &no) || no.Health.State != StateOffline || no.Health.Reason != ReasonAgentOffline || h.State != StateOffline {
		t.Fatalf("offline: %v / %+v, want ErrNotOnline carrying offline / agent_offline", err, h)
	}

	if _, _, err := r.agg.Snapshot(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown environment: %v, want ErrNotFound", err)
	}
}

func TestAnAgentThatIsPresentButBetweenSessionsIsReconnectingNotOnlineWithNothingToRead(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Remote")
	a := r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1"}, nil)
	r.whenOnline(t, env.ID)
	r.srv.Events.WaitLen(t, 1)
	_ = a.conn.WS.CloseNow()
	r.srv.Events.WaitLen(t, 2)

	// Inside the window the environment is not shown as offline, but there is no
	// session to read through, and the read says so.
	if e := r.health(t, env.ID); e.Health.State != StateOnline {
		t.Fatalf("inside the window: %+v, want online", e.Health)
	}
	_, _, err := r.agg.Snapshot(context.Background(), env.ID)
	var no ErrNotOnline
	if !errors.As(err, &no) || no.Health.State != StateReconnecting || no.Health.Reason != ReasonAgentOffline {
		t.Fatalf("Snapshot = %v, want ErrNotOnline reconnecting / agent_offline", err)
	}
}

func TestTheAggregatorServesTheEventsOfTheAgentsTarget(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Remote")
	a := r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1"}, nil)
	r.whenOnline(t, env.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	backlog, ch, stop := r.agg.Events(ctx, env.ID)
	defer stop()
	a.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: "abc123", Attributes: map[string]string{"name": "web"}})

	got := false
	for _, ev := range backlog {
		got = got || (ev.Action == "start" && ev.Name == "web")
	}
	for !got {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("the subscription ended before the event arrived")
			}
			got = ev.Action == "start" && ev.Name == "web"
		case <-ctx.Done():
			t.Fatal("the event published on the agent's daemon never arrived")
		}
	}
	eventually(t, "the event in the recent list", func() bool {
		for _, ev := range r.agg.Recent(context.Background(), env.ID, 10) {
			if ev.Action == "start" && ev.Name == "web" {
				return true
			}
		}
		return false
	})
}

func TestARetryNowOnAnAgentEnvironmentProbesItsTargets(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Remote")
	a := r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1"}, nil)
	r.whenOnline(t, env.ID)
	pings := func() int {
		n := 0
		for _, p := range a.fake.Requests() {
			if p == "/_ping" {
				n++
			}
		}
		return n
	}
	before := pings()
	if _, err := r.svc.Reconnect(context.Background(), env.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a second probe of the agent's daemon", func() bool { return pings() > before })
}

func TestRemovingAnAgentEnvironmentMakesTheHubForgetItsAgents(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Remote")
	r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1"}, nil)
	r.whenOnline(t, env.ID)
	if len(r.srv.Hub.Views(env.ID)) != 1 {
		t.Fatal("the hub has no view of the agent")
	}
	if err := r.svc.Remove(context.Background(), admin, env.ID, "203.0.113.5", "test"); err != nil {
		t.Fatal(err)
	}
	if got := r.srv.Hub.Views(env.ID); len(got) != 0 {
		t.Fatalf("after Remove the hub still remembers %+v", got)
	}
}

func TestTwentyGoroutinesConnectingDisconnectingAndReadingStatusPassTheRaceDetector(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Remote")
	const agents = 10
	conns := make([]*agentConn, agents)
	for i := range conns {
		conns[i] = r.connect(t, env, dockertest.FakeConfig{Hostname: "edge"}, nil)
	}
	r.srv.Events.WaitLen(t, agents)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var reconnecting, readers sync.WaitGroup
	for i := range agents {
		// Each of ten goroutines drops its agent and brings it back with hello.
		reconnecting.Go(func() {
			id := conns[i].id
			for range 3 {
				prev := r.srv.Hub.Session(id)
				_ = conns[i].conn.WS.CloseNow()
				again := r.srv.Hello(t, conns[i].conn.Identity)
				if again.Welcome == nil {
					t.Errorf("hello refused: %+v (err %v)", again.Refuse, again.Err)
					return
				}
				again.ProxyDocker(t, conns[i].fake.SocketPath())
				conns[i].conn = again
				// The hub registers a session just after it sends the welcome. Closing
				// the connection before that would race two handshakes of one agent,
				// which is a client behaviour this test is not about.
				deadline := time.Now().Add(5 * time.Second)
				for s := r.srv.Hub.Session(id); s == nil || s == prev; s = r.srv.Hub.Session(id) {
					if time.Now().After(deadline) {
						t.Errorf("the new session of %s was never registered", id)
						return
					}
					time.Sleep(time.Millisecond)
				}
			}
		})
		// Each of ten others reads the environment's status and the aggregate.
		readers.Go(func() {
			for ctx.Err() == nil {
				e, err := r.svc.Get(ctx, env.ID)
				if err != nil {
					if ctx.Err() == nil {
						t.Errorf("Get: %v", err)
					}
					return
				}
				_ = e.Health
				_, _, _ = r.agg.Snapshot(ctx, env.ID)
				if _, err := r.svc.List(ctx); err != nil {
					if ctx.Err() == nil {
						t.Errorf("List: %v", err)
					}
					return
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
	reconnecting.Wait()
	cancel()
	readers.Wait()
	eventually(t, "every agent connected again", func() bool {
		live := 0
		for _, v := range r.srv.Hub.Views(env.ID) {
			if v.Connected {
				live++
			}
		}
		return live == agents
	})

	if e := r.health(t, env.ID); e.Health.State != StateOnline && e.Health.State != StateDegraded {
		t.Fatalf("health = %+v after the agents came back", e.Health)
	}
}

func TestEveryVariantReadsThroughTheAggregator(t *testing.T) {
	local := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "this-host", Containers: []dockertest.FakeContainer{{ID: "l1", Names: []string{"/local-1"}, State: "running"}}})
	r := newTunRig(t, local)
	r.pastGrace()
	localEnv, err := r.svc.AddLocal(context.Background(), admin, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}

	standaloneEnv := r.srv.NewEnvironment(t, "Standalone")
	r.connect(t, standaloneEnv, dockertest.FakeConfig{Hostname: "standalone-1", Containers: []dockertest.FakeContainer{{ID: "s1", Names: []string{"/standalone-c"}, State: "running"}}}, nil)

	swarmEnv := r.srv.NewEnvironment(t, "Swarm")
	manager := &wire.Facts{
		EngineID: "ENG:m1", Hostname: "manager-1", EngineReachable: true,
		Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-m1", ClusterID: "cluster-1", Control: true},
	}
	r.connect(t, swarmEnv, dockertest.FakeConfig{
		Hostname: "manager-1", SwarmState: "active", SwarmControl: true, Nodes: 1, Services: 2,
		Containers: []dockertest.FakeContainer{{ID: "w1", Names: []string{"/swarm-c"}, State: "running"}},
	}, manager)

	for _, tc := range []struct {
		name, envID, container, host string
	}{
		{"local", localEnv.ID, "local-1", "this-host"},
		{"standalone", standaloneEnv.ID, "standalone-c", "standalone-1"},
		{"swarm", swarmEnv.ID, "swarm-c", "manager-1"},
	} {
		r.whenOnline(t, tc.envID)
		// The summary read: through the aggregator, which picks the target.
		agg, h, err := r.agg.Snapshot(context.Background(), tc.envID)
		if err != nil || h.State != StateOnline {
			t.Errorf("%s: Snapshot = %v, %+v", tc.name, err, h)
			continue
		}
		if len(agg.Containers) != 1 || agg.Containers[0].Name != tc.container || agg.Target.Probe.Hostname != tc.host {
			t.Errorf("%s: aggregate = %+v / target %+v, want container %s from %s", tc.name, agg.Snapshot, agg.Target.Probe.Hostname, tc.container, tc.host)
		}
		// The status rollup: the same environment through the registry.
		e, err := r.svc.Get(context.Background(), tc.envID)
		if err != nil || e.Health.State != StateOnline || e.Health.State != h.State {
			t.Errorf("%s: Get = %v, %+v; want the same online health as the aggregator's %+v", tc.name, err, e.Health, h)
		}
	}
	// A swarm manager's snapshot carries the cluster counts, and no more is
	// fanned out until plan 21: the one manager's snapshot is the whole answer.
	agg, _, err := r.agg.Snapshot(context.Background(), swarmEnv.ID)
	if err != nil || agg.Counts.Nodes == nil || *agg.Counts.Nodes != 1 || agg.Counts.Services == nil || *agg.Counts.Services != 2 {
		t.Fatalf("swarm counts = %+v, %v; want 1 node and 2 services from the manager", agg.Counts, err)
	}
}

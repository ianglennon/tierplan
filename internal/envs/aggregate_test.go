// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
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

// dial enrols a new agent in env, serves its Docker streams from a fake daemon, and
// sends its first heartbeat. It does not wait for the agent's target to probe; a test
// that observes the state before the first probe, or holds a probe on purpose, uses it.
func (r *tunRig) dial(t *testing.T, env tunneltest.Environment, cfg dockertest.FakeConfig, facts *wire.Facts) *agentConn {
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

// connect is dial followed by waiting until the agent's target has finished its first
// probe. An environment reads online as soon as one target has probed, but a cluster
// read offers only managers whose monitor already holds an answering client, so a rig
// is ready when every connected agent's target has probed, not when the environment
// reads online.
func (r *tunRig) connect(t *testing.T, env tunneltest.Environment, cfg dockertest.FakeConfig, facts *wire.Facts) *agentConn {
	t.Helper()
	a := r.dial(t, env, cfg, facts)
	awaitProbed(t, r.mon, AgentTarget(a.id))
	return a
}

// awaitProbed waits until the monitored target key has finished its first probe,
// whatever it found. The deadline only guards against a hang.
func awaitProbed(t *testing.T, mon *Monitor, key string) Status {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := mon.AwaitStatus(ctx, key, func(s Status) bool { return s.State != StateUnknown })
	if err != nil {
		t.Fatalf("the first probe of %s never finished: last status %+v: %v", key, st, err)
	}
	return st
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
		t.Fatalf("aggregate = %+v, want the two containers of the agent's daemon", agg.Containers)
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
	if err := r.svc.Remove(context.Background(), admin, env.ID, "203.0.113.5", "test", false); err != nil {
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
			t.Errorf("%s: aggregate = %+v / target %+v, want container %s from %s", tc.name, agg.Containers, agg.Target.Probe.Hostname, tc.container, tc.host)
		}
		// The status rollup: the same environment through the registry.
		e, err := r.svc.Get(context.Background(), tc.envID)
		if err != nil || e.Health.State != StateOnline || e.Health.State != h.State {
			t.Errorf("%s: Get = %v, %+v; want the same online health as the aggregator's %+v", tc.name, err, e.Health, h)
		}
	}
	// A swarm manager's snapshot carries the cluster counts.
	agg, _, err := r.agg.Snapshot(context.Background(), swarmEnv.ID)
	if err != nil || agg.Counts.Nodes == nil || *agg.Counts.Nodes != 1 || agg.Counts.Services == nil || *agg.Counts.Services != 2 {
		t.Fatalf("swarm counts = %+v, %v; want 1 node and 2 services from the manager", agg.Counts, err)
	}
}

func rowCount(t *testing.T, r *tunRig, query string, args ...any) int {
	t.Helper()
	var n int
	if err := r.st.Read.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRemovingAnAgentEnvironmentRevokesAndDisconnectsEveryAgentAndDeletesWhatItOwned(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Remote")
	// Nodes of one swarm: a standalone environment has one host only.
	node := func(name string) *wire.Facts {
		return &wire.Facts{EngineID: "ENG:" + name, Hostname: name, EngineReachable: true, Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-" + name}}
	}
	one := r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1"}, node("edge-1"))
	two := r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-2"}, node("edge-2"))
	r.whenOnline(t, env.ID)
	// A third agent that was revoked earlier leaves block rows behind.
	three := r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-3"}, node("edge-3"))
	if err := r.srv.Svc.Revoke(context.Background(), admin, env.ID, three.id, "old", "", ""); err != nil {
		t.Fatal(err)
	}
	if rowCount(t, r, `SELECT COUNT(*) FROM agent_blocks WHERE env_id = ?`, env.ID) == 0 {
		t.Fatal("the revoke left no block rows to cascade")
	}
	s1, s2 := r.srv.Hub.Session(one.id), r.srv.Hub.Session(two.id)
	if s1 == nil || s2 == nil {
		t.Fatal("the agents have no sessions")
	}

	if err := r.svc.Remove(context.Background(), admin, env.ID, "203.0.113.5", "test", false); err != nil {
		t.Fatal(err)
	}

	for name, s := range map[string]*tunnel.Session{"first": s1, "second": s2} {
		select {
		case <-s.Done():
		case <-time.After(time.Second):
			t.Errorf("the %s agent's session is still open a second after the environment was removed", name)
		}
	}
	for _, table := range []string{"agents", "agent_blocks", "env_cas", "env_join_tokens"} {
		if n := rowCount(t, r, `SELECT COUNT(*) FROM `+table+` WHERE env_id = ?`, env.ID); n != 0 {
			t.Errorf("%s still has %d rows for the removed environment", table, n)
		}
	}
	if n := rowCount(t, r, `SELECT COUNT(*) FROM environments WHERE id = ?`, env.ID); n != 0 {
		t.Error("the environment row is still there")
	}
	// Two agents were still active: each is audited as revoked, with the reason.
	if n := rowCount(t, r, `SELECT COUNT(*) FROM audit_log WHERE action = 'agent.revoked' AND env_id = ? AND details LIKE '%environment_removed%'`, env.ID); n != 2 {
		t.Errorf("agent.revoked rows for the removal = %d, want 2", n)
	}
	if n := rowCount(t, r, `SELECT COUNT(*) FROM audit_log WHERE action = 'environment.removed' AND env_id = ?`, env.ID); n != 1 {
		t.Errorf("environment.removed rows = %d", n)
	}
	// Neither agent can say hello again: the environment does not exist.
	for _, a := range []*agentConn{one, two} {
		c := r.srv.Hello(t, a.conn.Identity)
		if c.Refuse == nil || c.Refuse.Code != wire.ReasonAuthRejected {
			t.Errorf("hello after removal = %+v (err %v), want auth_rejected", c.Refuse, c.Err)
		}
	}
}

// The swarm fan-out (plan 02-21, ENV-02, ENV-06, D-46): every present agent is read
// in parallel through the per-target snapshot cache, node-local rows keep their node,
// cluster-scoped data comes from the manager in use, and a node that did not answer
// is named instead of silently missing.

// fanRig is a swarm of three managers (mgr-1 to mgr-3, mgr-2 leads) and one worker,
// wrk-1, with a short per-node timeout so a slow node costs a fraction of a second.
type fanRig struct {
	*swarmRig
	w *agentConn
}

func workerFacts(name string) *wire.Facts {
	return &wire.Facts{
		EngineID: "ENG:" + name, Hostname: name, EngineReachable: true,
		Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-" + name, Control: false},
	}
}

func fakeContainer(id, name string) dockertest.FakeContainer {
	return dockertest.FakeContainer{ID: id, Names: []string{"/" + name}, State: "running"}
}

// newFanRig enrols the managers and the worker. cfg may change each node's daemon
// (i is 1 to 3 for the managers and 4 for the worker) before it is started.
func newFanRig(t *testing.T, extraNodes []dockertest.FakeNode, cfg func(i int, c *dockertest.FakeConfig)) *fanRig {
	t.Helper()
	r := newTunRig(t, nil)
	r.pastGrace()
	s := &swarmRig{tunRig: r, nodes: managerNodes(append([]dockertest.FakeNode{
		{ID: "node-wrk-1", Hostname: "wrk-1", Role: "worker", EngineVersion: "29.1.0", OS: "linux"},
	}, extraNodes...)...)}
	s.env = r.srv.NewEnvironment(t, "Swarm")
	s.c = NewClusters(r.svc, r.snaps, ClusterOptions{Now: r.clk.Now, Publisher: r.brk, Events: r.evs, Grace: 2 * time.Minute})
	r.svc.WithClusters(s.c)
	r.agg.WithNodeTimeout(300 * time.Millisecond)
	services := []dockertest.FakeService{{ID: "svc-1", Name: "web", Mode: "replicated", Running: 1, Desired: 1}, {ID: "svc-2", Name: "db", Mode: "global", Running: 4, Desired: 4}}
	for i := 1; i <= 3; i++ {
		c := managerConfig(i, s.nodes, services)
		if cfg != nil {
			cfg(i, &c)
		}
		s.m[i-1] = r.connect(t, s.env, c, managerFacts(i))
	}
	wc := dockertest.FakeConfig{Hostname: "wrk-1", SwarmState: "active", Containers: []dockertest.FakeContainer{fakeContainer("w1", "svc-on-wrk-1")}}
	if cfg != nil {
		cfg(4, &wc)
	}
	f := &fanRig{swarmRig: s, w: r.connect(t, s.env, wc, workerFacts("wrk-1"))}
	r.whenOnline(t, s.env.ID)
	// The node list and the leader are known before anything is read.
	if err := s.c.Refresh(context.Background(), s.env.ID); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fanRig) read(t *testing.T) Aggregate {
	t.Helper()
	agg, _, err := f.agg.Snapshot(context.Background(), f.env.ID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return agg
}

func containerNodes(rows []ContainerRow) []string {
	var out []string
	for _, c := range rows {
		out = append(out, c.Node+"/"+c.Name)
	}
	return out
}

func TestAggregateSwarmReadsEveryNodeAndGivesEachRowItsNode(t *testing.T) {
	f := newFanRig(t, nil, func(i int, c *dockertest.FakeConfig) {
		c.Containers = []dockertest.FakeContainer{fakeContainer(fmt.Sprintf("c%d", i), "web")}
		c.Images = []dockertest.FakeImage{{ID: fmt.Sprintf("sha256:%064d", 1), RepoTags: []string{"nginx:1"}, Size: 10}}
		c.Volumes = []dockertest.FakeVolume{{Name: "data", Driver: "local"}}
	})
	agg := f.read(t)

	// Two nodes each run a container called web: both rows stay, told apart by node.
	if got := strings.Join(containerNodes(agg.Containers), " "); got != "mgr-1/web mgr-2/web mgr-3/web wrk-1/web" {
		t.Fatalf("containers = %s, want one web per node, by name then node", got)
	}
	if len(agg.Images) != 4 || len(agg.Volumes) != 4 {
		t.Fatalf("images %d, volumes %d, want one of each per node (duplicates across nodes are kept)", len(agg.Images), len(agg.Volumes))
	}
	nodes := map[string]bool{}
	for _, v := range agg.Volumes {
		nodes[v.Node] = true
	}
	if len(nodes) != 4 {
		t.Errorf("volume nodes = %v, want four different nodes", nodes)
	}
	if len(agg.NotReporting) != 0 || agg.NotReporting == nil {
		t.Errorf("notReporting = %#v, want an empty list, not nil", agg.NotReporting)
	}
	// Totals are the sums, each equal to its list; the breakdown has every node.
	c := agg.Counts
	if c.Containers != 4 || c.ContainersRunning != 4 || c.Images != 4 || c.Volumes != 4 || c.Networks != len(agg.Networks) {
		t.Errorf("counts = %+v, want the cluster totals", c)
	}
	if len(agg.ByNode) != 4 || agg.ByNode["wrk-1"].Containers != 1 || agg.ByNode["mgr-2"].Images != 1 {
		t.Errorf("byNode = %+v, want each node's own counts", agg.ByNode)
	}
	// Nodes and services are the cluster's, from the manager.
	if c.Nodes == nil || *c.Nodes != 4 || c.Services == nil || *c.Services != 2 {
		t.Errorf("nodes/services = %v/%v, want 4 and 2 from the manager's cluster read", c.Nodes, c.Services)
	}
	if len(agg.Nodes) != 4 {
		t.Errorf("nodes = %+v, want the four that answered", agg.Nodes)
	}
}

func TestAggregateSwarmScopedNetworksComeFromTheManagerInUseOnceAndLocalOnesFromEachNode(t *testing.T) {
	f := newFanRig(t, nil, func(i int, c *dockertest.FakeConfig) {
		nets := []dockertest.FakeNetwork{
			{ID: "ovl-1", Name: "ovl", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.1.0/24"}},
			{ID: fmt.Sprintf("br-%d", i), Name: "bridge", Driver: "bridge", Scope: "local"},
		}
		if i == 1 {
			// A manager that is not the one in use: what only it lists is not shown.
			nets = append(nets, dockertest.FakeNetwork{ID: "stale", Name: "only-on-mgr-1", Driver: "overlay", Scope: "swarm"})
		}
		c.Networks = nets
	})
	agg := f.read(t)

	var cluster, local []string
	for _, n := range agg.Networks {
		if n.Scope == "swarm" {
			cluster = append(cluster, n.Node+"/"+n.Name)
		} else {
			local = append(local, n.Node+"/"+n.Name)
		}
	}
	if got := strings.Join(cluster, " "); got != "Cluster/ovl" {
		t.Errorf("swarm networks = %q, want the overlay once, labelled Cluster, from the manager in use", got)
	}
	if got := strings.Join(local, " "); got != "mgr-1/bridge mgr-2/bridge mgr-3/bridge wrk-1/bridge" {
		t.Errorf("local networks = %q, want a bridge per node", got)
	}
	if agg.Counts.Networks != 5 {
		t.Errorf("network total = %d, want 5 (the list's length)", agg.Counts.Networks)
	}
	if agg.ByNode["mgr-1"].Networks != 1 {
		t.Errorf("mgr-1 networks = %d, want its one local network", agg.ByNode["mgr-1"].Networks)
	}
}

func TestAggregateSlowNodeIsNamedAndTheRestAreNotHeldUp(t *testing.T) {
	f := newFanRig(t, nil, func(i int, c *dockertest.FakeConfig) {
		if i == 4 {
			c.ListDelay = 3 * time.Second
		}
	})
	start := time.Now()
	agg := f.read(t)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the read took %v, want about the per-node timeout, not the slow node's delay", took)
	}
	if !reflect.DeepEqual(agg.NotReporting, []string{"wrk-1"}) {
		t.Errorf("notReporting = %v, want the slow node by name", agg.NotReporting)
	}
	if got := strings.Join(containerNodes(agg.Containers), " "); got != "mgr-1/svc-on-m1 mgr-2/svc-on-m2 mgr-3/svc-on-m3" {
		t.Errorf("containers = %s, want the three managers' rows and none of the slow node's", got)
	}
	if agg.Counts.Containers != 3 || len(agg.ByNode) != 3 {
		t.Errorf("counts = %+v byNode = %v, want only the nodes that answered", agg.Counts, agg.ByNode)
	}
}

func TestAggregateNodeWithNoAgentIsNamedWhetherItsAgentDroppedOrWasNeverEnrolled(t *testing.T) {
	// wrk-2 is in the swarm and never had an agent; wrk-1's agent drops.
	f := newFanRig(t, []dockertest.FakeNode{{ID: "node-wrk-2", Hostname: "wrk-2", Role: "worker", EngineVersion: "29.1.0", OS: "linux"}}, nil)
	f.kick(t, f.w)
	if err := f.c.Refresh(context.Background(), f.env.ID); err != nil {
		t.Fatal(err)
	}
	agg := f.read(t)
	if !reflect.DeepEqual(agg.NotReporting, []string{"wrk-1", "wrk-2"}) {
		t.Errorf("notReporting = %v, want the dropped agent's node and the never-enrolled one, by name", agg.NotReporting)
	}
	if agg.Counts.Containers != 3 {
		t.Errorf("containers = %d, want the three managers' only", agg.Counts.Containers)
	}
}

func TestAggregateNodeThatIsDrainedAndHasNoAgentIsNotExpectedToReport(t *testing.T) {
	f := newFanRig(t, []dockertest.FakeNode{{ID: "node-wrk-3", Hostname: "wrk-3", Role: "worker", Availability: "drain", EngineVersion: "29.1.0", OS: "linux"}}, nil)
	if agg := f.read(t); len(agg.NotReporting) != 0 {
		t.Errorf("notReporting = %v, want none: a drained node gets no agent task", agg.NotReporting)
	}
}

func TestAggregateWhenNoNodeReportsTheListsAreEmptyAndEveryNodeIsNamed(t *testing.T) {
	f := newFanRig(t, nil, nil)
	for _, a := range append(f.m[:], f.w) {
		f.kick(t, a)
	}
	// Still inside the offline window: the environment is not offline, but nothing answers.
	agg := f.read(t)
	if want := []string{"mgr-1", "mgr-2", "mgr-3", "wrk-1"}; !reflect.DeepEqual(agg.NotReporting, want) {
		t.Errorf("notReporting = %v, want %v", agg.NotReporting, want)
	}
	if len(agg.Containers)+len(agg.Images)+len(agg.Volumes)+len(agg.Networks) != 0 || agg.Counts.Containers != 0 {
		t.Errorf("rows %+v, want empty lists", agg)
	}
	if agg.Containers == nil || agg.Networks == nil {
		t.Error("an empty list is nil: the API would answer null")
	}
	if agg.Counts.Nodes != nil || agg.Counts.Services != nil {
		t.Errorf("counts = %+v, want no cluster counts without a manager", agg.Counts)
	}
}

func TestAggregateReadsTwentyNodesInParallelNotOneAfterAnother(t *testing.T) {
	// 20 workers behind a 200 ms list each: serial reads would take 4 s, parallel ones
	// about 400 ms (the limit is 16 at once).
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Many")
	r.agg.WithNodeTimeout(5 * time.Second)
	for i := 1; i <= 20; i++ {
		name := fmt.Sprintf("w-%02d", i)
		r.connect(t, env, dockertest.FakeConfig{
			Hostname: name, SwarmState: "active", ListDelay: 200 * time.Millisecond,
			Containers: []dockertest.FakeContainer{fakeContainer("c"+name, "job-"+name)},
		}, workerFacts(name))
	}
	r.whenOnline(t, env.ID)
	start := time.Now()
	agg, _, err := r.agg.Snapshot(context.Background(), env.ID)
	took := time.Since(start)
	if err != nil || len(agg.Containers) != 20 || len(agg.NotReporting) != 0 {
		t.Fatalf("Snapshot = %d containers, notReporting %v, %v; want all 20 nodes", len(agg.Containers), agg.NotReporting, err)
	}
	if took > 2*time.Second {
		t.Errorf("the read took %v: the nodes were read one after another", took)
	}
}

func TestAggregateOfAStandaloneHostIsOneNodeAndNamesNobodyMissing(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Remote")
	r.connect(t, env, dockertest.FakeConfig{
		Hostname: "edge-1", Containers: []dockertest.FakeContainer{fakeContainer("a1", "web")},
		Networks: []dockertest.FakeNetwork{{ID: "n1", Name: "bridge", Driver: "bridge", Scope: "local"}},
	}, nil)
	r.whenOnline(t, env.ID)
	agg, _, err := r.agg.Snapshot(context.Background(), env.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(containerNodes(agg.Containers), " "); got != "edge-1/web" || agg.Networks[0].Node != "edge-1" {
		t.Errorf("rows = %s / network node %q, want everything on the one host", got, agg.Networks[0].Node)
	}
	if len(agg.ByNode) != 1 || agg.ByNode["edge-1"].Containers != 1 || agg.NotReporting == nil || len(agg.NotReporting) != 0 {
		t.Errorf("byNode = %+v notReporting = %#v, want one entry and nobody missing", agg.ByNode, agg.NotReporting)
	}
}

func TestAggregateOfTheLocalSocketIsOneNodeNamedAfterTheEngine(t *testing.T) {
	local := dockertest.NewFake(t, dockertest.FakeConfig{
		Hostname: "this-host", Containers: []dockertest.FakeContainer{fakeContainer("l1", "local-1")},
	})
	r := newTunRig(t, local)
	r.pastGrace()
	e, err := r.svc.AddLocal(context.Background(), admin, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	r.whenOnline(t, e.ID)
	agg, _, err := r.agg.Snapshot(context.Background(), e.ID)
	if err != nil || len(agg.Containers) != 1 || agg.Containers[0].Node != "this-host" || agg.ByNode["this-host"].Containers != 1 {
		t.Fatalf("aggregate = %+v, %v; want the one container on this-host", agg, err)
	}
}

func TestAggregateNodeRefCarriesTheHostsReadOnlyFlag(t *testing.T) {
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Locked")
	r.connect(t, env, dockertest.FakeConfig{Hostname: "ro-1"}, &wire.Facts{
		EngineID: "ENG:ro", Hostname: "ro-1", EngineReachable: true, Policy: wire.PolicyFacts{ReadOnly: true},
	})
	r.whenOnline(t, env.ID)
	agg, _, err := r.agg.Snapshot(context.Background(), env.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(agg.Nodes) != 1 || !agg.Nodes[0].ReadOnly {
		t.Errorf("nodes = %+v, want the one node read-only", agg.Nodes)
	}
}

func TestAggregateNodeRefOfTheLocalSocketIsNeverReadOnly(t *testing.T) {
	local := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "this-host"})
	r := newTunRig(t, local)
	r.pastGrace()
	e, err := r.svc.AddLocal(context.Background(), admin, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	r.whenOnline(t, e.ID)
	agg, _, err := r.agg.Snapshot(context.Background(), e.ID)
	if err != nil || len(agg.Nodes) != 1 || agg.Nodes[0].ReadOnly {
		t.Fatalf("nodes = %+v, %v; want the local socket writable", agg.Nodes, err)
	}
}

// The node chooser of a pull needs each node's architecture and, for a node that is not
// answering, an ID to name it by (plan 05-03, IMG-02, D-25).

func TestAggregateNodeRefCarriesTheArchitectureTheWayARegistryNamesIt(t *testing.T) {
	f := newFanRig(t, nil, nil)
	agg := f.read(t)
	if len(agg.Nodes) != 4 {
		t.Fatalf("nodes = %+v, want four", agg.Nodes)
	}
	for _, n := range agg.Nodes {
		if n.Arch != "amd64" {
			t.Errorf("node %s has arch %q, want amd64", n.Name, n.Arch)
		}
	}

	local := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "this-host"})
	r := newTunRig(t, local)
	r.pastGrace()
	e, err := r.svc.AddLocal(context.Background(), admin, "", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	r.whenOnline(t, e.ID)
	one, _, err := r.agg.Snapshot(context.Background(), e.ID)
	if err != nil || len(one.Nodes) != 1 || one.Nodes[0].Arch != "amd64" {
		t.Fatalf("nodes = %+v, %v; want the local socket's node as amd64", one.Nodes, err)
	}
}

func TestAggregateUnreachableNodesCarryTheIDsAWriteRoutesBy(t *testing.T) {
	// wrk-2 is in the swarm and never had an agent; wrk-1's agent drops.
	f := newFanRig(t, []dockertest.FakeNode{{ID: "node-wrk-2", Hostname: "wrk-2", Role: "worker", EngineVersion: "29.1.0", OS: "linux"}}, nil)
	f.kick(t, f.w)
	if err := f.c.Refresh(context.Background(), f.env.ID); err != nil {
		t.Fatal(err)
	}
	agg := f.read(t)
	if len(agg.Unreachable) != 2 {
		t.Fatalf("unreachable = %+v, want the two nodes notReporting names", agg.Unreachable)
	}
	dropped, never := agg.Unreachable[0], agg.Unreachable[1]
	if dropped.Name != "wrk-1" || dropped.ID != f.w.id || dropped.Role != "worker" {
		t.Errorf("the dropped node = %+v, want wrk-1 by its agent's ID %q", dropped, f.w.id)
	}
	if never.Name != "wrk-2" || never.ID != "node-wrk-2" || never.Role != "worker" {
		t.Errorf("the node that never had an agent = %+v, want wrk-2 by its swarm node ID", never)
	}
	for _, n := range agg.Nodes {
		if n.ID == dropped.ID || n.ID == never.ID {
			t.Errorf("node %+v is both reporting and unreachable", n)
		}
	}
}

func TestAggregateOfAnEnvironmentWhoseNodesAllAnswerHasNoUnreachableNode(t *testing.T) {
	f := newFanRig(t, nil, nil)
	if agg := f.read(t); len(agg.Unreachable) != 0 {
		t.Errorf("unreachable = %+v, want none", agg.Unreachable)
	}
}

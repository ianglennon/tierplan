// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package tunnel_test

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/tunnel"
	"github.com/ianglennon/tierplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/tierplan/proto/wire"
)

// What the hub remembers about each agent for the health rollup (plan 02-11,
// D-41): connected now, last sign of life, and how the last session ended.

func viewOf(t *testing.T, srv *tunneltest.Server, envID, agentID string) tunnel.AgentLive {
	t.Helper()
	for _, v := range srv.Hub.Views(envID) {
		if v.AgentID == agentID {
			return v
		}
	}
	t.Fatalf("no view of agent %s in %s: %+v", agentID, envID, srv.Hub.Views(envID))
	return tunnel.AgentLive{}
}

func TestViewsOfAFreshConnectionAreConnectedAndHeardAtTheConnect(t *testing.T) {
	srv := timed(t, nil)
	env := srv.NewEnvironment(t, "Production")
	srv.Clock.Advance(time.Minute)
	c := srv.Enrol(t, env)
	srv.Events.WaitLen(t, 1)

	v := viewOf(t, srv, env.ID, c.Identity.AgentID)
	if !v.Connected || v.EnvID != env.ID || !v.ConnectedAt.Equal(srv.Clock.Now()) || !v.LastSeen.Equal(v.ConnectedAt) {
		t.Fatalf("view = %+v, want connected, at the connect time %v", v, srv.Clock.Now())
	}
	if !v.EngineReachable {
		t.Error("before the first heartbeat nothing says the engine is unreachable, so it counts as reachable")
	}
	if !v.Ended.IsZero() || v.LastCode != "" {
		t.Errorf("view = %+v, want no end yet", v)
	}
}

func TestViewsFollowHeartbeatsAndTheEngineFlag(t *testing.T) {
	srv := timed(t, nil)
	env := srv.NewEnvironment(t, "Production")
	c := srv.Enrol(t, env)
	srv.Events.WaitLen(t, 1)
	s := srv.Hub.Session(c.Identity.AgentID)
	control := c.OpenControl(t)
	srv.Clock.Advance(7 * time.Second)
	at := srv.Clock.Now()
	tunneltest.SendHeartbeat(t, control, false, 1)
	// The view of a live session is read from the session's status, so the status
	// showing the heartbeat is the view showing it.
	awaitStatus(t, s, "the heartbeat to show in the status", func(st tunnel.SessionStatus) bool { return st.LastHeartbeat.Equal(at) })
	if v := viewOf(t, srv, env.ID, c.Identity.AgentID); !v.LastSeen.Equal(at) {
		t.Fatalf("view = %+v, want it last heard at %v", v, at)
	}
	if v := viewOf(t, srv, env.ID, c.Identity.AgentID); v.EngineReachable {
		t.Fatalf("view = %+v, want the engine reported unreachable", v)
	}
}

func TestViewsRememberATimeoutWithItsTimeAndTheLastHeartbeat(t *testing.T) {
	k := newTicks()
	srv := timed(t, k.option)
	env := srv.NewEnvironment(t, "Production")
	c := srv.Enrol(t, env)
	srv.Events.WaitLen(t, 1)
	s := srv.Hub.Session(c.Identity.AgentID)
	control := c.OpenControl(t)
	tunneltest.SendHeartbeat(t, control, true, 1)
	beat := awaitStatus(t, s, "the heartbeat", func(st tunnel.SessionStatus) bool { return !st.LastHeartbeat.IsZero() }).LastHeartbeat

	srv.Clock.Advance(31 * time.Second)
	k.look(t, s)
	srv.Events.WaitLen(t, 2)
	v := viewOf(t, srv, env.ID, c.Identity.AgentID)
	if v.Connected || v.LastCode != tunnel.DisconnectTimeout || !v.Ended.Equal(srv.Clock.Now()) || !v.LastSeen.Equal(beat) {
		t.Fatalf("view = %+v, want disconnected by timeout at %v, last heard %v", v, srv.Clock.Now(), beat)
	}
}

func TestViewsRememberACleanDropAndAReconnectIsConnectedAgain(t *testing.T) {
	srv := timed(t, nil)
	env := srv.NewEnvironment(t, "Production")
	c := srv.Enrol(t, env)
	srv.Events.WaitLen(t, 1)
	srv.Clock.Advance(5 * time.Second)
	_ = c.WS.CloseNow()
	srv.Events.WaitLen(t, 2)
	v := viewOf(t, srv, env.ID, c.Identity.AgentID)
	if v.Connected || v.LastCode != tunnel.DisconnectClosed || !v.Ended.Equal(srv.Clock.Now()) {
		t.Fatalf("view = %+v, want disconnected, closed, ended now", v)
	}

	srv.Clock.Advance(5 * time.Second)
	again := srv.Hello(t, c.Identity)
	if again.Welcome == nil {
		t.Fatalf("hello refused: %+v", again.Refuse)
	}
	srv.Events.WaitLen(t, 3)
	v = viewOf(t, srv, env.ID, c.Identity.AgentID)
	if !v.Connected || !v.ConnectedAt.Equal(srv.Clock.Now()) {
		t.Fatalf("view = %+v, want connected again at %v", v, srv.Clock.Now())
	}
	if n := len(srv.Hub.Views(env.ID)); n != 1 {
		t.Fatalf("%d views for one agent", n)
	}
}

func TestViewsCarryTheClockOffsetMeasuredAtTheHandshake(t *testing.T) {
	srv := timed(t, nil)
	env := srv.NewEnvironment(t, "Production")
	c := srv.Dial(t, tunneltest.Agent{Token: env.Token, ClockSkew: 45 * time.Second})
	if c.Welcome == nil {
		t.Fatalf("refused: %+v", c.Refuse)
	}
	srv.Events.WaitLen(t, 1)
	v := viewOf(t, srv, env.ID, c.Identity.AgentID)
	if !v.ClockWarning || v.ClockOffset < 44*time.Second || v.ClockOffset > 46*time.Second {
		t.Fatalf("view = %+v, want an offset of about 45 s with the warning set", v)
	}
}

func TestViewsAreScopedToTheEnvironmentOrderedByAgentAndForgottenOnRequest(t *testing.T) {
	srv := timed(t, nil)
	one, two := srv.NewEnvironment(t, "One"), srv.NewEnvironment(t, "Two")
	var ids []string
	// Three hosts of one swarm: a standalone environment has one host only, and an enrolment
	// from a host that already has an agent here retires that agent (review 17 CR-02).
	for i := range 3 {
		c := enrolNode(t, srv, one, fmt.Sprintf("node-%d", i))
		ids = append(ids, c.Identity.AgentID)
		srv.Events.WaitLen(t, i+1)
	}
	other := srv.Enrol(t, two)
	srv.Events.WaitLen(t, 4)

	views := srv.Hub.Views(one.ID)
	if len(views) != 3 {
		t.Fatalf("%d views, want 3", len(views))
	}
	for i := 1; i < len(views); i++ {
		if views[i-1].AgentID >= views[i].AgentID {
			t.Fatalf("views not ordered by agent ID: %+v", views)
		}
	}
	sort.Strings(ids)
	for i, v := range views {
		if v.AgentID == other.Identity.AgentID || v.EnvID != one.ID || v.AgentID != ids[i] {
			t.Fatalf("view %+v is not the %d-th agent of the environment (%v)", v, i, ids)
		}
	}
	if got := srv.Hub.Views("unknown"); len(got) != 0 {
		t.Fatalf("views of an unknown environment: %+v", got)
	}

	srv.Hub.Forget(one.ID)
	if got := srv.Hub.Views(one.ID); len(got) != 0 {
		t.Fatalf("views after Forget: %+v", got)
	}
	if got := srv.Hub.Views(two.ID); len(got) != 1 {
		t.Fatalf("Forget of one environment touched another: %+v", got)
	}
}

// The version an agent reported is remembered with the rest, so the overview can show
// it after the agent has gone (plan 02-12).
func TestViewsRememberTheVersionTheAgentReportedAfterItLeaves(t *testing.T) {
	srv := timed(t, nil)
	env := srv.NewEnvironment(t, "Production")
	c := srv.Dial(t, tunneltest.Agent{Token: env.Token, Version: "0.2.7"})
	if c.Welcome == nil {
		t.Fatalf("enrolment refused: %+v", c.Refuse)
	}
	srv.Events.WaitLen(t, 1)
	if v := viewOf(t, srv, env.ID, c.Identity.AgentID); v.Version != "0.2.7" {
		t.Fatalf("version = %q while connected, want 0.2.7", v.Version)
	}
	_ = c.WS.CloseNow()
	srv.Events.WaitLen(t, 2)
	if v := viewOf(t, srv, env.ID, c.Identity.AgentID); v.Version != "0.2.7" || v.Connected {
		t.Fatalf("view = %+v after the agent left, want it disconnected and still 0.2.7", v)
	}
}

// enrolNode enrols an agent in env that reports itself as the swarm node name, so several
// agents of one environment are several hosts. tunneltest.Enrol reports the same host each
// time, and an enrolment from a host that already has an agent in the environment retires
// that agent (review 17 CR-02).
func enrolNode(t *testing.T, srv *tunneltest.Server, env tunneltest.Environment, name string) *tunneltest.Conn {
	t.Helper()
	c := srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: &wire.Facts{
		EngineID: "ENG:" + name, Hostname: name, EngineReachable: true,
		Swarm: &wire.SwarmFacts{State: "active", NodeID: name},
	}})
	if c.Welcome == nil {
		t.Fatalf("enrol of %s was refused: %+v (err %v)", name, c.Refuse, c.Err)
	}
	return c
}

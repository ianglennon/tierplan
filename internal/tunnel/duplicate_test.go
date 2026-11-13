// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package tunnel_test

import (
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/tunnel"
	"github.com/ianglennon/tierplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/tierplan/proto/wire"
)

// Review 17-2 WR-02: a second agent on a host whose agent is connected now is refused
// host_already_connected, with no row written and the connected agent left alone.
// Once the connected agent is gone the host's next enrolment retires it as replaced.
func TestASecondAgentOfAConnectedHostIsRefusedAndTheFirstKeepsItsSession(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{AuditRefusals: true})
	env := srv.NewEnvironment(t, "Production")
	first := srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: swarmFacts("worker-1")})
	if first.Welcome == nil {
		t.Fatalf("enrolment refused: %+v", first.Refuse)
	}
	agentID := first.Identity.AgentID
	s := awaitRegistered(t, srv.Hub, agentID)
	if !srv.Hub.Live(agentID) {
		t.Fatal("Live = false for an agent whose session is registered")
	}

	refusedWith(t, srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: swarmFacts("worker-1")}), wire.RefuseHostAlreadyConnected)
	if got := scalar(t, srv, `SELECT COUNT(*) FROM agents WHERE env_id = ?`, env.ID); got != 1 {
		t.Errorf("agent rows = %d, want 1: the refused enrolment writes none", got)
	}
	if got := scalar(t, srv, `SELECT COUNT(*) FROM agents WHERE id = ? AND status = 'active'`, agentID); got != 1 {
		t.Error("the connected agent is no longer active")
	}
	if got := scalar(t, srv, `SELECT COUNT(*) FROM audit_log WHERE action = 'agent.enrol_refused' AND reason = ?`, wire.RefuseHostAlreadyConnected); got != 1 {
		t.Errorf("agent.enrol_refused rows with the reason host_already_connected = %d, want 1", got)
	}
	if srv.Hub.Session(agentID) != s {
		t.Error("the connected agent's session was replaced or closed")
	}

	// The first agent stops: its host may enrol again, which retires it.
	_ = first.WS.CloseNow()
	awaitGone(t, srv.Hub, agentID, "the hub to forget the stopped agent's session")
	if srv.Hub.Live(agentID) {
		t.Fatal("Live = true for an agent with no session")
	}
	if again := srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: swarmFacts("worker-1")}); again.Welcome == nil {
		t.Fatalf("the enrolment after the first agent stopped was refused: %+v", again.Refuse)
	}
	if got := scalar(t, srv, `SELECT COUNT(*) FROM agents WHERE id = ? AND status = 'revoked' AND revoke_reason = 'replaced'`, agentID); got != 1 {
		t.Error("the stopped agent was not retired as replaced")
	}
}

// A session that has gone silent past the offline window is not live, even before the
// watchdog closes it, so it does not block its host's enrolment.
func TestASilentSessionIsNotLive(t *testing.T) {
	ticks := make(chan time.Time) // the watchdog never ticks, so only Live's own check counts
	srv := tunneltest.Start(t, tunneltest.Options{Configure: func(h *tunnel.Hub) *tunnel.Hub { return h.WithWatchTicks(ticks) }})
	env := srv.NewEnvironment(t, "Production")
	c := srv.Enrol(t, env)
	awaitRegistered(t, srv.Hub, c.Identity.AgentID)
	if !srv.Hub.Live(c.Identity.AgentID) {
		t.Fatal("Live = false for a session that has just connected")
	}
	srv.Clock.Advance(tunnel.DefaultOfflineAfter + time.Second)
	if srv.Hub.Live(c.Identity.AgentID) {
		t.Error("Live = true for a session silent for longer than the offline window")
	}
	if srv.Hub.Session(c.Identity.AgentID) == nil {
		t.Error("the session was removed; this test is about one that is still registered")
	}
}

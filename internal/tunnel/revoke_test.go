// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package tunnel_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/tunnel"
	"github.com/ianglennon/tierplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/tierplan/proto/testsupport"
	"github.com/ianglennon/tierplan/proto/wire"
)

var revoker = audit.Actor{Type: audit.ActorUser, ID: "u-admin", Name: "admin"}

func swarmFacts(name string) *wire.Facts {
	return &wire.Facts{
		EngineID: "ENG:" + name, Hostname: name, EngineReachable: true,
		Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-" + name},
	}
}

func scalar(t *testing.T, srv *tunneltest.Server, query string, args ...any) int {
	t.Helper()
	var n int
	if err := srv.Store.Read.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// awaitGone waits until the hub has no live session for agentID. It wakes on each
// registration and removal and never polls.
func awaitGone(t *testing.T, h *tunnel.Hub, agentID, what string) {
	t.Helper()
	if _, err := h.AwaitSession(testsupport.Context(t), agentID, func(s *tunnel.Session) bool { return s == nil }); err != nil {
		t.Fatalf("waiting for %s: %v", what, err)
	}
}

// awaitRegistered waits until the hub has a live session for agentID and returns it.
func awaitRegistered(t *testing.T, h *tunnel.Hub, agentID string) *tunnel.Session {
	t.Helper()
	s, err := h.AwaitSession(testsupport.Context(t), agentID, func(s *tunnel.Session) bool { return s != nil })
	if err != nil {
		t.Fatalf("waiting for the session of %s to be registered: %v", agentID, err)
	}
	return s
}

func refusedWith(t *testing.T, c *tunneltest.Conn, code string) {
	t.Helper()
	if c.Welcome != nil {
		t.Fatalf("the handshake was welcomed, want a refusal with %s", code)
	}
	if c.Refuse == nil || c.Refuse.Code != code {
		t.Fatalf("refusal = %+v (err %v), want %s", c.Refuse, c.Err, code)
	}
}

func TestRevokeClosesTheSessionWithinASecondFailsACallInFlightAndKeepsTheHostOut(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{AuditRefusals: true})
	env := srv.NewEnvironment(t, "Production")
	c := srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: swarmFacts("worker-1")})
	if c.Welcome == nil {
		t.Fatalf("enrolment refused: %+v", c.Refuse)
	}
	srv.Events.WaitLen(t, 1)
	agentID := c.Identity.AgentID
	s := srv.Hub.Session(agentID)
	requests := holdOpenStreams(t, c)

	errc := make(chan error, 1)
	go func() {
		resp, err := httpOver(s).Get("http://docker/containers/json")
		if err == nil {
			_ = resp.Body.Close()
		}
		errc <- err
	}()
	testsupport.Await(t, "the request to reach the agent's stream", requests)

	if err := srv.Svc.Revoke(context.Background(), revoker, env.ID, agentID, "decommissioned", "203.0.113.5", "test"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	// "At once" is proved by awaiting the close on the shared deadline. Nothing but
	// the revoke's kick can end this session in that time: the silence watchdog runs
	// on the fake clock, which does not move, and the multiplexer's ping and write
	// timeouts are a minute in tunneltest (WithMuxTimeouts).
	testsupport.Await(t, "the session to end", s.Done())
	if err := testsupport.Await(t, "the call in flight to fail", errc); !errors.Is(err, tunnel.ErrAgentGone) {
		t.Fatalf("the call in flight ended with %v, want an error wrapping ErrAgentGone", err)
	}
	// The hub forgets the session just after it ends, so wait rather than look once.
	awaitGone(t, srv.Hub, agentID, "the hub to forget the revoked agent's session")
	log := srv.Events.WaitLen(t, 2)
	if want := "disconnected:" + agentID + ":revoked"; log[1] != want {
		t.Errorf("lifecycle = %v, want %q second", log, want)
	}
	if got := scalar(t, srv, `SELECT COUNT(*) FROM audit_log WHERE action = 'agent.revoked'`); got != 1 {
		t.Errorf("agent.revoked rows = %d", got)
	}
	if got := scalar(t, srv, `SELECT COUNT(*) FROM audit_log WHERE action = 'agent.blocked'`); got != 2 {
		t.Errorf("agent.blocked rows = %d, want the engine ID and the swarm node ID", got)
	}

	// The revoked agent's own certificate is refused on every later hello.
	for range 2 {
		refusedWith(t, srv.Hello(t, c.Identity), wire.ReasonRevoked)
	}

	// The host cannot come back with the join token while it is blocked.
	refusedWith(t, srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: swarmFacts("worker-1")}), wire.ReasonBlocked)
	// Another host with the same token is unaffected.
	other := srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: swarmFacts("worker-2")})
	if other.Welcome == nil {
		t.Fatalf("another host was refused: %+v", other.Refuse)
	}
}

func TestUnblockLetsTheHostEnrolAgainAsANewAgentAndTheOldOneStaysRevoked(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{})
	env := srv.NewEnvironment(t, "Production")
	c := srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: swarmFacts("worker-1")})
	if c.Welcome == nil {
		t.Fatalf("enrolment refused: %+v", c.Refuse)
	}
	srv.Events.WaitLen(t, 1)
	old := c.Identity.AgentID
	if err := srv.Svc.Revoke(context.Background(), revoker, env.ID, old, "", "", ""); err != nil {
		t.Fatal(err)
	}
	refusedWith(t, srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: swarmFacts("worker-1")}), wire.ReasonBlocked)

	if err := srv.Svc.Unblock(context.Background(), revoker, env.ID, old, "", ""); err != nil {
		t.Fatalf("Unblock: %v", err)
	}
	again := srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: swarmFacts("worker-1")})
	if again.Welcome == nil {
		t.Fatalf("enrolment after Unblock was refused: %+v", again.Refuse)
	}
	if again.Identity.AgentID == old {
		t.Error("the host got its old agent back")
	}
	// The old certificate is still no good.
	refusedWith(t, srv.Hello(t, c.Identity), wire.ReasonRevoked)
}

// gate lets a test hold one hello between its verification and its registration.
type gate struct {
	tunnel.Verifier
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gate) Hello(ctx context.Context, req tunnel.HelloRequest) (tunnel.HelloResult, error) {
	res, err := g.Verifier.Hello(ctx, req)
	if err == nil {
		g.once.Do(func() {
			close(g.reached)
			<-g.release
		})
	}
	return res, err
}

func TestARevokeBetweenTheHelloCheckAndTheRegistrationLeavesNoSessionBehind(t *testing.T) {
	g := &gate{reached: make(chan struct{}), release: make(chan struct{})}
	srv := tunneltest.Start(t, tunneltest.Options{Verifier: func(real tunnel.Verifier) tunnel.Verifier {
		g.Verifier = real
		return g
	}})
	env := srv.NewEnvironment(t, "Production")
	// The enrolment is a handshake too; only hellos are held.
	first := srv.Enrol(t, env)
	srv.Events.WaitLen(t, 1)
	id := first.Identity
	_ = first.WS.CloseNow()
	srv.Events.WaitLen(t, 2)

	helloc := make(chan *tunneltest.Conn, 1)
	go func() { helloc <- srv.Hello(t, id) }()
	testsupport.Await(t, "the hello to pass verification", g.reached)
	// The hello has passed the status check and is about to register: revoke now,
	// completely, kick included, then let it go on.
	if err := srv.Svc.Revoke(context.Background(), revoker, env.ID, id.AgentID, "", "", ""); err != nil {
		t.Fatal(err)
	}
	close(g.release)

	c := testsupport.Await(t, "the late hello to be answered", helloc)
	if c.Welcome == nil {
		t.Fatalf("hello refused before the registration: %+v (err %v)", c.Refuse, c.Err)
	}
	awaitGone(t, srv.Hub, id.AgentID, "the late session to be closed")
	rctx := testsupport.Context(t)
	// A read that ends because the deadline passed says the server never closed.
	if _, _, err := c.WS.Read(rctx); err == nil || rctx.Err() != nil {
		t.Fatalf("the connection of a revoked agent is still open and readable (err %v)", err)
	}
	// The server closes the connection only after the registration was refused (the
	// kick follows the errNoLongerActive answer), so by the time the read failed
	// the registry has had its last word.
	if srv.Hub.Session(id.AgentID) != nil {
		t.Fatal("a session for a revoked agent stayed registered")
	}
}

func TestRevokeRacingAHelloFiftyTimesNeverLeavesASessionForTheRevokedAgent(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{})
	env := srv.NewEnvironment(t, "Production")
	for i := range 50 {
		first := srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: swarmFacts("node-" + string(rune('a'+i%26)) + string(rune('a'+i/26)))})
		if first.Welcome == nil {
			t.Fatalf("round %d: enrolment refused: %+v", i, first.Refuse)
		}
		id := first.Identity
		awaitRegistered(t, srv.Hub, id.AgentID)
		_ = first.WS.CloseNow()
		awaitGone(t, srv.Hub, id.AgentID, "the first session to end")

		var wg sync.WaitGroup
		var hello *tunneltest.Conn
		start := make(chan struct{})
		wg.Go(func() {
			<-start
			hello = srv.Hello(t, id)
		})
		wg.Go(func() {
			<-start
			if err := srv.Svc.Revoke(context.Background(), revoker, env.ID, id.AgentID, "", "", ""); err != nil {
				t.Errorf("round %d: Revoke: %v", i, err)
			}
		})
		close(start)
		wg.Wait()

		if hello.Welcome != nil {
			// Welcomed before the revoke landed: the server must close it.
			rctx := testsupport.Context(t)
			// A read that ends because the deadline passed says the server never closed.
			if _, _, err := hello.WS.Read(rctx); err == nil || rctx.Err() != nil {
				t.Fatalf("round %d: the welcomed connection of a revoked agent stayed open (err %v)", i, err)
			}
		} else if hello.Refuse == nil || hello.Refuse.Code != wire.ReasonRevoked {
			t.Fatalf("round %d: hello = %+v (err %v), want a welcome that is then closed or a revoked refusal", i, hello.Refuse, hello.Err)
		}
		// A refused handshake keeps its slot until the agent finishes the close, and
		// all of these come from one address, which may hold 16 at once (T-02-24): an
		// agent that has been refused lets go, so the test does too.
		_ = hello.WS.CloseNow()
		// The close frame reaches the agent a moment before the hub has unregistered
		// the session, so wait for the unregistration. A session that was never
		// closed would stay, and the wait would fail.
		awaitGone(t, srv.Hub, id.AgentID, fmt.Sprintf("round %d: the session of the revoked agent to be unregistered", i))
	}
}

func TestKickEnvClosesEverySessionOfOneEnvironmentAndNoOtherAndKickOfNobodyIsHarmless(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{})
	one, two := srv.NewEnvironment(t, "One"), srv.NewEnvironment(t, "Two")
	a, b := srv.Enrol(t, one), srv.Enrol(t, one)
	other := srv.Enrol(t, two)
	srv.Events.WaitLen(t, 3)

	srv.Hub.Kick("no-such-agent", "revoked")
	srv.Hub.KickEnv("no-such-environment", "removed")
	srv.Hub.KickEnv(one.ID, "removed")
	awaitGone(t, srv.Hub, a.Identity.AgentID, "the first session of the environment to close")
	awaitGone(t, srv.Hub, b.Identity.AgentID, "the second session of the environment to close")
	if s := srv.Hub.Session(other.Identity.AgentID); s == nil || isDone(s) {
		t.Error("the other environment's session was closed")
	}
}

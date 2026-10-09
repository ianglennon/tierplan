// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package tunnel_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/tunnel"
	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/bayplan/proto/wire"
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
	holdOpenStreams(t, c)

	errc := make(chan error, 1)
	go func() {
		resp, err := httpOver(s).Get("http://docker/containers/json")
		if err == nil {
			_ = resp.Body.Close()
		}
		errc <- err
	}()
	time.Sleep(100 * time.Millisecond) // the request is now blocked on its stream

	start := time.Now()
	if err := srv.Svc.Revoke(context.Background(), revoker, env.ID, agentID, "decommissioned", "203.0.113.5", "test"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	waitFor(t, "the session to end", func() bool { return isDone(s) })
	if took := time.Since(start); took > time.Second {
		t.Errorf("the session took %v to close after the revoke", took)
	}
	select {
	case err := <-errc:
		if !errors.Is(err, tunnel.ErrAgentGone) {
			t.Fatalf("the call in flight ended with %v, want an error wrapping ErrAgentGone", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the call in flight is still hanging")
	}
	// The hub forgets the session just after it ends, so wait rather than look once.
	waitFor(t, "the hub to forget the revoked agent's session", func() bool { return srv.Hub.Session(agentID) == nil })
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
	select {
	case <-g.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the hello never passed verification")
	}
	// The hello has passed the status check and is about to register: revoke now,
	// completely, kick included, then let it go on.
	if err := srv.Svc.Revoke(context.Background(), revoker, env.ID, id.AgentID, "", "", ""); err != nil {
		t.Fatal(err)
	}
	close(g.release)

	c := <-helloc
	if c.Welcome == nil {
		t.Fatalf("hello refused before the registration: %+v (err %v)", c.Refuse, c.Err)
	}
	waitFor(t, "the late session to be closed", func() bool { return srv.Hub.Session(id.AgentID) == nil })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := c.WS.Read(ctx); err == nil {
		t.Fatal("the connection of a revoked agent is still open and readable")
	}
	time.Sleep(50 * time.Millisecond)
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
		_ = first.WS.CloseNow()
		waitFor(t, "the first session to end", func() bool { return srv.Hub.Session(id.AgentID) == nil })

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
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if _, _, err := hello.WS.Read(ctx); err == nil {
				cancel()
				t.Fatalf("round %d: the welcomed connection of a revoked agent stayed open", i)
			}
			cancel()
		} else if hello.Refuse == nil || hello.Refuse.Code != wire.ReasonRevoked {
			t.Fatalf("round %d: hello = %+v (err %v), want a welcome that is then closed or a revoked refusal", i, hello.Refuse, hello.Err)
		}
		// A refused handshake keeps its slot until the agent finishes the close, and
		// all of these come from one address, which may hold 16 at once (T-02-24): an
		// agent that has been refused lets go, so the test does too.
		_ = hello.WS.CloseNow()
		// The close frame reaches the agent a moment before the hub has unregistered
		// the session, so the registry is given a short while to settle. A session
		// that was never closed would stay.
		deadline := time.Now().Add(2 * time.Second)
		for srv.Hub.Session(id.AgentID) != nil {
			if time.Now().After(deadline) {
				t.Fatalf("round %d: a session for the revoked agent is still registered", i)
			}
			time.Sleep(time.Millisecond)
		}
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
	waitFor(t, "both sessions of the environment to close", func() bool {
		return srv.Hub.Session(a.Identity.AgentID) == nil && srv.Hub.Session(b.Identity.AgentID) == nil
	})
	if s := srv.Hub.Session(other.Identity.AgentID); s == nil || isDone(s) {
		t.Error("the other environment's session was closed")
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package tunnel_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/bayplan/proto/wire"
)

// enrolled enrols one agent, lets its connection go and waits for the hub to
// notice, so a test starts from an agent that is known but not connected.
func enrolled(t *testing.T, srv *tunneltest.Server, env tunneltest.Environment) tunneltest.Identity {
	t.Helper()
	c := srv.Enrol(t, env)
	srv.Events.WaitLen(t, 1)
	_ = c.WS.CloseNow()
	srv.Events.WaitLen(t, 2)
	return c.Identity
}

func TestHelloWithAValidCertificateAndSignatureRegistersASessionAndIsWelcomed(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{})
	env := srv.NewEnvironment(t, "Production")
	id := enrolled(t, srv, env)

	c := srv.Hello(t, id)
	if c.Welcome == nil {
		t.Fatalf("hello was refused: %+v (err %v)", c.Refuse, c.Err)
	}
	if c.Welcome.AgentID != id.AgentID || c.Welcome.EnvID != env.ID || len(c.Welcome.Cert) != 0 {
		t.Fatalf("welcome = %+v; want the agent and environment IDs and no new certificate", c.Welcome)
	}
	if c.Welcome.HeartbeatMs != wire.DefaultHeartbeatMs {
		t.Errorf("heartbeatMs = %d", c.Welcome.HeartbeatMs)
	}
	log := srv.Events.WaitLen(t, 3)
	if log[2] != "connected:"+id.AgentID {
		t.Fatalf("lifecycle = %v", log)
	}
	if s := srv.Hub.Session(id.AgentID); s == nil || s.EnvID != env.ID {
		t.Fatalf("the hub has no session for the agent: %+v", s)
	}
}

func TestHelloRefusesWhatItShouldWithTheSpecificCode(t *testing.T) {
	type tweak func(t *testing.T, srv *tunneltest.Server, env tunneltest.Environment, id tunneltest.Identity) tunneltest.Agent
	other := func(t *testing.T, srv *tunneltest.Server) tunneltest.Environment {
		return srv.NewEnvironment(t, "Staging")
	}
	for _, tc := range []struct {
		name string
		do   tweak
		code string
	}{
		{"an unknown environment", func(t *testing.T, srv *tunneltest.Server, _ tunneltest.Environment, id tunneltest.Identity) tunneltest.Agent {
			cert := tunneltest.ForeignCert(t, uuid.NewString(), id.AgentID, id.Public(), srv.Clock.Now())
			return tunneltest.Agent{Identity: &id, Cert: cert}
		}, wire.ReasonAuthRejected},
		{"a certificate signed by another environment's CA", func(t *testing.T, srv *tunneltest.Server, env tunneltest.Environment, id tunneltest.Identity) tunneltest.Agent {
			cert := tunneltest.ForeignCert(t, env.ID, id.AgentID, id.Public(), srv.Clock.Now())
			return tunneltest.Agent{Identity: &id, Cert: cert}
		}, wire.ReasonAuthRejected},
		{"a certificate of an agent of another environment", func(t *testing.T, srv *tunneltest.Server, _ tunneltest.Environment, _ tunneltest.Identity) tunneltest.Agent {
			// A real agent of environment B presents its own certificate and key
			// but claims the ID of an agent of A: it is still B's agent, and B's
			// CA is the one that vouches, so this must be refused by the row check.
			b := srv.Enrol(t, other(t, srv))
			srv.Events.WaitLen(t, 3)
			id := b.Identity
			id.AgentID = uuid.NewString()
			return tunneltest.Agent{Identity: &id}
		}, wire.ReasonAuthRejected},
		{"a wrong signature", func(t *testing.T, _ *tunneltest.Server, _ tunneltest.Environment, id tunneltest.Identity) tunneltest.Agent {
			_, other := tunneltest.NewKey(t)
			return tunneltest.Agent{Identity: &id, SigningKey: other}
		}, wire.ReasonAuthRejected},
		{"a signature for a different audience", func(_ *testing.T, _ *tunneltest.Server, _ tunneltest.Environment, id tunneltest.Identity) tunneltest.Agent {
			return tunneltest.Agent{Identity: &id, SignedAud: "evil.example"}
		}, wire.ReasonAuthRejected},
		{"an audience other than this server's", func(_ *testing.T, _ *tunneltest.Server, _ tunneltest.Environment, id tunneltest.Identity) tunneltest.Agent {
			return tunneltest.Agent{Identity: &id, Aud: "evil.example"}
		}, wire.ReasonAuthRejected},
		{"a public key that differs from the stored row", func(t *testing.T, srv *tunneltest.Server, _ tunneltest.Environment, id tunneltest.Identity) tunneltest.Agent {
			pub, _ := tunneltest.NewKey(t)
			srv.Exec(t, `UPDATE agents SET public_key = ? WHERE id = ?`, []byte(pub), id.AgentID)
			return tunneltest.Agent{Identity: &id}
		}, wire.ReasonAuthRejected},
		{"an agent row that no longer exists", func(t *testing.T, srv *tunneltest.Server, _ tunneltest.Environment, id tunneltest.Identity) tunneltest.Agent {
			srv.Exec(t, `DELETE FROM agents WHERE id = ?`, id.AgentID)
			return tunneltest.Agent{Identity: &id}
		}, wire.ReasonAuthRejected},
		{"a certificate past its NotAfter at the server clock", func(_ *testing.T, srv *tunneltest.Server, _ tunneltest.Environment, id tunneltest.Identity) tunneltest.Agent {
			srv.Clock.Advance(31 * 24 * time.Hour)
			return tunneltest.Agent{Identity: &id}
		}, wire.ReasonCertExpired},
		{"an agent that was revoked", func(t *testing.T, srv *tunneltest.Server, _ tunneltest.Environment, id tunneltest.Identity) tunneltest.Agent {
			srv.Exec(t, `UPDATE agents SET status = 'revoked', revoked_at = 1 WHERE id = ?`, id.AgentID)
			return tunneltest.Agent{Identity: &id}
		}, wire.ReasonRevoked},
		{"a garbage certificate", func(_ *testing.T, _ *tunneltest.Server, _ tunneltest.Environment, id tunneltest.Identity) tunneltest.Agent {
			return tunneltest.Agent{Identity: &id, Cert: []byte("not a certificate")}
		}, wire.ReasonAuthRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := tunneltest.Start(t, tunneltest.Options{})
			env := srv.NewEnvironment(t, "Production")
			id := enrolled(t, srv, env)

			a := tc.do(t, srv, env, id)
			before := len(srv.Events.Log())
			c := srv.Dial(t, a)
			if c.Refuse == nil {
				t.Fatalf("hello was not refused: welcome %+v", c.Welcome)
			}
			if c.Refuse.Code != tc.code {
				t.Errorf("refuse code = %q (%s), want %q", c.Refuse.Code, c.Refuse.Detail, tc.code)
			}
			_, _, err := c.WS.Read(context.Background())
			if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Errorf("close status %v, want policy violation (%v)", websocket.CloseStatus(err), err)
			}
			if got := len(srv.Events.Log()); got != before {
				t.Errorf("the observer heard of a refused agent: %v", srv.Events.Log())
			}
		})
	}
}

// An unknown environment and a certificate from the wrong CA must be the same
// answer, so an environment ID cannot be probed.
func TestHelloAnswersAnUnknownEnvironmentExactlyAsABadCertificate(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{})
	env := srv.NewEnvironment(t, "Production")
	id := enrolled(t, srv, env)

	unknown := srv.Dial(t, tunneltest.Agent{Identity: &id, Cert: tunneltest.ForeignCert(t, uuid.NewString(), id.AgentID, id.Public(), srv.Clock.Now())})
	wrongCA := srv.Dial(t, tunneltest.Agent{Identity: &id, Cert: tunneltest.ForeignCert(t, env.ID, id.AgentID, id.Public(), srv.Clock.Now())})
	if unknown.Refuse == nil || wrongCA.Refuse == nil {
		t.Fatalf("not refused: %+v %+v", unknown.Refuse, wrongCA.Refuse)
	}
	if *unknown.Refuse != *wrongCA.Refuse {
		t.Fatalf("an unknown environment is told %+v but a bad certificate %+v", *unknown.Refuse, *wrongCA.Refuse)
	}
}

func TestTwoHellosForTheSameAgentReplaceTheOldSessionWithTheNewOne(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{})
	env := srv.NewEnvironment(t, "Production")
	first := srv.Enrol(t, env)
	id := first.Identity
	srv.Events.WaitLen(t, 1)
	oldSess := srv.Hub.Session(id.AgentID)

	second := srv.Hello(t, id)
	if second.Welcome == nil {
		t.Fatalf("the second hello was refused: %+v", second.Refuse)
	}
	log := srv.Events.WaitLen(t, 3)
	want := []string{"connected:" + id.AgentID, "disconnected:" + id.AgentID + ":replaced", "connected:" + id.AgentID}
	for i := range want {
		if log[i] != want[i] {
			t.Fatalf("lifecycle = %v, want %v", log, want)
		}
	}
	newSess := srv.Hub.Session(id.AgentID)
	if newSess == nil || newSess == oldSess {
		t.Fatal("the hub still holds the old session")
	}
	select {
	case <-oldSess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the old session was not closed")
	}
	select {
	case <-newSess.Done():
		t.Fatal("the new session ended with the old one")
	default:
	}
	// The old connection really is dead: the agent end can read nothing more.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := first.WS.Read(ctx); err == nil {
		t.Error("the replaced session's WebSocket is still open")
	}
	if n := len(srv.Events.Log()); n != 3 {
		t.Errorf("the replaced session was reported again: %v", srv.Events.Log())
	}
}

func TestSimultaneousHellosForOneAgentLeaveExactlyOneSession(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{})
	env := srv.NewEnvironment(t, "Production")
	id := enrolled(t, srv, env)

	const n = 8
	var wg sync.WaitGroup
	conns := make([]*tunneltest.Conn, n)
	for i := range n {
		wg.Go(func() { conns[i] = srv.Hello(t, id) })
	}
	wg.Wait()
	for i, c := range conns {
		if c.Welcome == nil {
			t.Fatalf("hello %d was refused: %+v", i, c.Refuse)
		}
	}
	// 2 events from the enrolment, n connects and n-1 replacements.
	log := srv.Events.WaitLen(t, 2+n+(n-1))

	// Per agent the lifecycle alternates: never a disconnect before its connect,
	// never two live sessions at once.
	live := 0
	for i, e := range log[2:] {
		switch {
		case e == "connected:"+id.AgentID:
			live++
		case e == "disconnected:"+id.AgentID+":replaced":
			live--
		default:
			t.Fatalf("unexpected lifecycle event %q at %d in %v", e, i, log)
		}
		if live < 0 || live > 1 {
			t.Fatalf("%d live sessions after %q: %v", live, e, log)
		}
	}
	if live != 1 {
		t.Fatalf("%d sessions left, want exactly one: %v", live, log)
	}
	if got := srv.Hub.Sessions(); len(got) != 1 {
		t.Fatalf("hub holds %d sessions", len(got))
	}
}

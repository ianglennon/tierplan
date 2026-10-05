// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package tunnel_test

import (
	"bytes"
	"context"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/tunnel"
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
		{"an agent that claims an ID other than its certificate's", func(t *testing.T, srv *tunneltest.Server, _ tunneltest.Environment, _ tunneltest.Identity) tunneltest.Agent {
			// A real agent of another environment presents its own certificate and
			// key but claims a different agent ID: the certificate names who it is.
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

// T-02-21 (review WR-B-01). A hello whose certificate the environment's CA never
// signed used to be answered by what the certificate's dates said: a forged leaf
// that named a real environment and had a NotAfter in the past was told
// cert_expired and "enrol the agent again", while the same leaf naming an unknown
// environment was told auth_rejected. That was an environment-existence oracle for
// anyone who could reach the server. Every certificate the CA did not sign must get
// the same answer whatever its dates, and an agent the CA really issued for must
// still be told it expired and be able to enrol again.
func TestHelloAnswersAForgedExpiredCertificateExactlyAsAnUnknownEnvironment(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{})
	env := srv.NewEnvironment(t, "Production")
	id := enrolled(t, srv, env)

	past := srv.Clock.Now().Add(-60 * 24 * time.Hour)
	forgedExpiredReal := srv.Dial(t, tunneltest.Agent{Identity: &id, Cert: tunneltest.ForeignCert(t, env.ID, id.AgentID, id.Public(), past)})
	forgedExpiredUnknown := srv.Dial(t, tunneltest.Agent{Identity: &id, Cert: tunneltest.ForeignCert(t, uuid.NewString(), id.AgentID, id.Public(), past)})
	forgedCurrentReal := srv.Dial(t, tunneltest.Agent{Identity: &id, Cert: tunneltest.ForeignCert(t, env.ID, id.AgentID, id.Public(), srv.Clock.Now())})

	for name, c := range map[string]*tunneltest.Conn{
		"an expired forgery naming a real environment":     forgedExpiredReal,
		"an expired forgery naming an unknown environment": forgedExpiredUnknown,
		"a current forgery naming a real environment":      forgedCurrentReal,
	} {
		if c.Refuse == nil {
			t.Fatalf("%s was not refused: welcome %+v", name, c.Welcome)
		}
		if c.Refuse.Code != wire.ReasonAuthRejected {
			t.Errorf("%s: code = %q (%s), want %q", name, c.Refuse.Code, c.Refuse.Detail, wire.ReasonAuthRejected)
		}
		if strings.Contains(c.Refuse.Detail, env.ID) {
			t.Errorf("%s: the refusal names the environment: %q", name, c.Refuse.Detail)
		}
	}
	if *forgedExpiredReal.Refuse != *forgedExpiredUnknown.Refuse {
		t.Fatalf("an expired forgery for a real environment is told %+v but for an unknown one %+v", *forgedExpiredReal.Refuse, *forgedExpiredUnknown.Refuse)
	}
	if *forgedExpiredReal.Refuse != *forgedCurrentReal.Refuse {
		t.Fatalf("an expired forgery is told %+v but a current one %+v", *forgedExpiredReal.Refuse, *forgedCurrentReal.Refuse)
	}

	// The agent the CA really issued for is still told, and can recover.
	srv.Clock.Advance(31 * 24 * time.Hour)
	expired := srv.Hello(t, id)
	if expired.Refuse == nil || expired.Refuse.Code != wire.ReasonCertExpired {
		t.Fatalf("a genuine expired certificate: refuse %+v welcome %+v, want %q", expired.Refuse, expired.Welcome, wire.ReasonCertExpired)
	}
	renewed := srv.Enrol(t, env)
	if renewed.Welcome == nil || len(renewed.Identity.Cert) == 0 {
		t.Fatalf("the join token did not enrol the agent again: %+v", renewed.Welcome)
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

// The older of two overlapping handshakes of one agent may finish last. The agent
// has moved on to the newer connection, so the older one must not replace it with a
// session nobody is on the other end of (plan 02-11 hub race).
func TestAnOlderHandshakeThatFinishesLastDoesNotReplaceTheNewerSession(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{})
	env := srv.NewEnvironment(t, "Production")
	id := enrolled(t, srv, env)
	base := len(srv.Events.WaitLen(t, 2))

	older := srv.Begin(t, tunneltest.Agent{Identity: &id})
	newer := srv.Begin(t, tunneltest.Agent{Identity: &id})
	winner := newer.Finish()
	if winner.Welcome == nil {
		t.Fatalf("the newer hello was refused: %+v", winner.Refuse)
	}
	srv.Events.WaitLen(t, base+1)
	live := srv.Hub.Session(id.AgentID)
	if live == nil {
		t.Fatal("the newer hello did not register")
	}

	late := older.Finish()
	if late.Welcome == nil {
		t.Fatalf("the older hello was refused: %+v", late.Refuse)
	}
	// The server turns the older connection away after its welcome.
	select {
	case <-late.Mux.CloseChan():
	case <-time.After(5 * time.Second):
		t.Fatal("the older connection was not closed")
	}
	if got := srv.Hub.Session(id.AgentID); got != live {
		t.Fatalf("the hub holds %p after the older hello finished, want the newer session %p", got, live)
	}
	select {
	case <-live.Done():
		t.Fatal("the newer session ended")
	default:
	}
	if winner.Mux.IsClosed() {
		t.Error("the newer connection was closed")
	}
	if log := srv.Events.Log(); len(log) != base+1 {
		t.Errorf("the turned-away connection was reported: %v", log)
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
	// Every hello ends up either replaced by a newer one or turned away because a
	// newer one registered first; exactly one connection is left, and it is open.
	// The agent end of each connection sees the other n-1 close.
	waitFor := time.Now().Add(10 * time.Second)
	var survivors []*tunneltest.Conn
	for {
		survivors = survivors[:0]
		for _, c := range conns {
			if !c.Mux.IsClosed() {
				survivors = append(survivors, c)
			}
		}
		if len(survivors) == 1 {
			break
		}
		if time.Now().After(waitFor) {
			t.Fatalf("%d connections are still open, want exactly one", len(survivors))
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A replaced session is closed a moment before it is reported, so the log may
	// still be one event short of what the connections show.
	var log []string
	liveIn := func(log []string) int {
		live := 0
		for _, e := range log[2:] {
			switch e {
			case "connected:" + id.AgentID:
				live++
			case "disconnected:" + id.AgentID + ":replaced":
				live--
			}
		}
		return live
	}
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if log = srv.Events.Log(); len(log) >= 3 && liveIn(log) == 1 {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("the lifecycle never settled on one session: %v", log)
		}
	}

	// Per agent the lifecycle alternates: never a disconnect before its connect,
	// never two live sessions at once.
	live := 0
	for i, e := range log[2:] {
		switch e {
		case "connected:" + id.AgentID:
			live++
		case "disconnected:" + id.AgentID + ":replaced":
			live--
		default:
			t.Fatalf("unexpected lifecycle event %q at %d in %v", e, i, log)
		}
		if live < 0 || live > 1 {
			t.Fatalf("%d live sessions after %q: %v", live, e, log)
		}
	}
	if got := srv.Hub.Sessions(); len(got) != 1 {
		t.Fatalf("hub holds %d sessions", len(got))
	}
}

// trustLoopback puts the hub behind the client-address middleware with
// 127.0.0.0/8 as a trusted proxy, so the test client counts as a TLS-terminating
// proxy in front of Bayplan.
func trustLoopback(h http.Handler) http.Handler {
	return httpx.ClientIPMiddleware([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})(h)
}

func versioned(v string) tunneltest.Options {
	return tunneltest.Options{Configure: func(h *tunnel.Hub) *tunnel.Hub { return h.WithServerVersion(v) }}
}

func TestCompatibleWindowIsEnforcedByTheHandshakeInBothModes(t *testing.T) {
	srv := tunneltest.Start(t, versioned("v0.3.1"))
	env := srv.NewEnvironment(t, "Production")
	for _, tc := range []struct {
		agent string
		ok    bool
		say   string
	}{
		{"0.3.0", true, ""},
		{"v0.2.7", true, ""},
		{"0.1.9", false, "too old"},
		{"0.4.0", false, "too new"},
	} {
		c := srv.Dial(t, tunneltest.Agent{Token: env.Token, Version: tc.agent})
		if tc.ok {
			if c.Welcome == nil {
				t.Errorf("agent %s was refused: %+v", tc.agent, c.Refuse)
			}
			continue
		}
		if c.Refuse == nil || c.Refuse.Code != wire.ReasonVersionIncompatible || !strings.Contains(c.Refuse.Detail, tc.say) {
			t.Errorf("agent %s got refusal %+v (welcomed: %v), want version_incompatible saying %q", tc.agent, c.Refuse, c.Welcome != nil, tc.say)
		}
	}

	// A hello is held to the same window.
	id := enrolled(t, srv, env)
	c := srv.Dial(t, tunneltest.Agent{Identity: &id, Version: "0.4.0"})
	if c.Refuse == nil || c.Refuse.Code != wire.ReasonVersionIncompatible {
		t.Fatalf("hello of a too-new agent got %+v, want version_incompatible", c.Refuse)
	}
}

func TestCompatibleDevAndEdgeServersAcceptAnyAgentOfTheSameProtocol(t *testing.T) {
	for _, server := range []string{"dev (abc123)", "edge-20261003-abc1234", ""} {
		srv := tunneltest.Start(t, versioned(server))
		env := srv.NewEnvironment(t, "Production")
		if c := srv.Dial(t, tunneltest.Agent{Token: env.Token, Version: "0.0.1"}); c.Welcome == nil {
			t.Errorf("server %q refused an old agent: %+v", server, c.Refuse)
		}
		if c := srv.Dial(t, tunneltest.Agent{Token: env.Token, Proto: 2}); c.Refuse == nil || c.Refuse.Code != wire.ReasonVersionIncompatible {
			t.Errorf("server %q accepted protocol 2: %+v", server, c)
		}
	}
}

func TestCompatibleRefusalCarriesTheUpdateHintWhenOneIsSet(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{Configure: func(h *tunnel.Hub) *tunnel.Hub {
		return h.WithServerVersion("v0.3.1").WithUpdateHint(func(tooOld bool, _ string) string {
			if tooOld {
				return "run: docker pull bayplan-agent"
			}
			return ""
		})
	}})
	env := srv.NewEnvironment(t, "Production")
	c := srv.Dial(t, tunneltest.Agent{Token: env.Token, Version: "0.1.0"})
	if c.Refuse == nil || !strings.Contains(c.Refuse.Detail, "run: docker pull bayplan-agent") {
		t.Fatalf("refusal = %+v, want the update command in the detail", c.Refuse)
	}
}

// The hint names the agent's environment type, taken from the facts it reported:
// a swarm whose state is active, else a standalone host (D-25).
func TestUpdateHintIsGivenTheEnvironmentTypeFromTheAgentsFacts(t *testing.T) {
	var got []string
	srv := tunneltest.Start(t, tunneltest.Options{Configure: func(h *tunnel.Hub) *tunnel.Hub {
		return h.WithServerVersion("v0.3.1").WithUpdateHint(func(tooOld bool, envType string) string {
			got = append(got, envType)
			return "update this " + envType
		})
	}})
	env := srv.NewEnvironment(t, "Production")
	for _, tc := range []struct {
		name  string
		facts *wire.Facts
		want  string
	}{
		{"swarm manager", &wire.Facts{EngineReachable: true, Swarm: &wire.SwarmFacts{State: "active", Control: true, NodeID: "n1", ClusterID: "c1"}}, "swarm"},
		{"swarm worker", &wire.Facts{EngineReachable: true, Swarm: &wire.SwarmFacts{State: "active", NodeID: "n2"}}, "swarm"},
		{"standalone", &wire.Facts{EngineReachable: true}, "standalone"},
		{"inactive swarm", &wire.Facts{EngineReachable: true, Swarm: &wire.SwarmFacts{State: "inactive"}}, "standalone"},
	} {
		got = nil
		c := srv.Dial(t, tunneltest.Agent{Token: env.Token, Version: "0.1.0", Facts: tc.facts})
		if c.Refuse == nil || !strings.Contains(c.Refuse.Detail, "update this "+tc.want) {
			t.Errorf("%s: refusal = %+v, want the hint for %q in the detail", tc.name, c.Refuse, tc.want)
		}
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s: the hint was asked for %v, want [%s]", tc.name, got, tc.want)
		}
	}
}

func TestClockSkewBeyondFiveMinutesIsRefusedWithTheOffset(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{})
	env := srv.NewEnvironment(t, "Production")
	for _, skew := range []time.Duration{6 * time.Minute, -6 * time.Minute} {
		c := srv.Dial(t, tunneltest.Agent{Token: env.Token, ClockSkew: skew})
		if c.Refuse == nil || c.Refuse.Code != wire.ReasonClockSkew {
			t.Fatalf("an agent %v off was answered %+v, want clock_skew", skew, c.Refuse)
		}
		if !strings.Contains(c.Refuse.Detail, "6m") {
			t.Errorf("detail %q does not carry the offset", c.Refuse.Detail)
		}
	}
	// A refused enrolment leaves no agent behind.
	var n int
	if err := srv.Store.Read.QueryRow(`SELECT COUNT(*) FROM agents`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d agents after refused enrolments (%v), want none", n, err)
	}

	// The same applies to a hello.
	id := enrolled(t, srv, env)
	c := srv.Dial(t, tunneltest.Agent{Identity: &id, ClockSkew: 6 * time.Minute})
	if c.Refuse == nil || c.Refuse.Code != wire.ReasonClockSkew {
		t.Fatalf("hello with a skewed clock got %+v, want clock_skew", c.Refuse)
	}
}

func TestClockSkewOfFortySecondsIsAcceptedWithAWarningAndTwentyWithout(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{})
	env := srv.NewEnvironment(t, "Production")
	for _, tc := range []struct {
		skew time.Duration
		warn bool
	}{
		{40 * time.Second, true},
		{-40 * time.Second, true},
		{20 * time.Second, false},
		{0, false},
	} {
		c := srv.Dial(t, tunneltest.Agent{Token: env.Token, ClockSkew: tc.skew})
		if c.Welcome == nil {
			t.Fatalf("an agent %v off was refused: %+v", tc.skew, c.Refuse)
		}
		// The session is registered just after the welcome is sent.
		var s *tunnel.Session
		waitFor(t, "the session to be registered", func() bool { s = srv.Hub.Session(c.Welcome.AgentID); return s != nil })
		st := s.Status()
		// The agent's ts is whole milliseconds, so the offset is exact to a millisecond.
		if d := st.ClockOffset - tc.skew; d < -time.Millisecond || d > time.Millisecond || st.ClockWarning != tc.warn {
			t.Errorf("skew %v: session says offset %v warning %v, want about %v and %v", tc.skew, st.ClockOffset, st.ClockWarning, tc.skew, tc.warn)
		}
	}
}

func TestExporterBindingIsCheckedWhenBayplanTerminatesTLSItself(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{})
	env := srv.NewEnvironment(t, "Production")

	if c := srv.Dial(t, tunneltest.Agent{Token: env.Token}); c.Welcome == nil {
		t.Fatalf("a hello bound to its own TLS session was refused: %+v", c.Refuse)
	}
	for name, a := range map[string]tunneltest.Agent{
		"an exporter value from another session": {Token: env.Token, EKM: bytes.Repeat([]byte{7}, 32)},
		"no exporter value":                      {Token: env.Token, NoEKM: true},
	} {
		c := srv.Dial(t, a)
		if c.Refuse == nil || c.Refuse.Code != wire.ReasonAuthRejected {
			t.Errorf("%s: got %+v, want auth_rejected", name, c.Refuse)
		}
	}
}

func TestExporterBindingIsNotComparedBehindATrustedProxy(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{Wrap: trustLoopback})
	env := srv.NewEnvironment(t, "Production")
	for name, a := range map[string]tunneltest.Agent{
		"no exporter value":      {Token: env.Token, NoEKM: true},
		"a different exporter":   {Token: env.Token, EKM: bytes.Repeat([]byte{7}, 32)},
		"the exporter as it was": {Token: env.Token},
	} {
		if c := srv.Dial(t, a); c.Welcome == nil {
			t.Errorf("%s: refused behind a trusted proxy: %+v", name, c.Refuse)
		}
	}
}

func TestWelcomeCarriesTheServersCurrentPins(t *testing.T) {
	pins := []string{"sha256/AAAA"}
	srv := tunneltest.Start(t, tunneltest.Options{Configure: func(h *tunnel.Hub) *tunnel.Hub {
		return h.WithPins(func() []string { return pins })
	}})
	env := srv.NewEnvironment(t, "Production")

	c := srv.Dial(t, tunneltest.Agent{Token: env.Token})
	if c.Welcome == nil {
		t.Fatalf("refused: %+v", c.Refuse)
	}
	if got := c.Welcome.Pins; len(got) != 1 || got[0] != "sha256/AAAA" {
		t.Fatalf("welcome pins = %v, want [sha256/AAAA]", got)
	}

	// The function is read on every handshake, so a change applies to the next one.
	pins = []string{"sha256/BBBB", "sha256/AAAA"}
	c = srv.Dial(t, tunneltest.Agent{Token: env.Token})
	if c.Welcome == nil || len(c.Welcome.Pins) != 2 || c.Welcome.Pins[0] != "sha256/BBBB" {
		t.Fatalf("welcome = %+v, want both pins", c.Welcome)
	}
}

func TestWelcomeHasNoPinsWhenNoneAreConfigured(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{})
	env := srv.NewEnvironment(t, "Production")
	c := srv.Dial(t, tunneltest.Agent{Token: env.Token})
	if c.Welcome == nil || len(c.Welcome.Pins) != 0 {
		t.Fatalf("welcome = %+v, want no pins", c.Welcome)
	}
}

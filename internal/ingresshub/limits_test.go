// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/ingresshub/ingresstest"
	"github.com/ianglennon/tierplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/tierplan/proto/ingresswire"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// held is a client that upgraded and has not answered the challenge: its handshake holds a slot until it sends
// its auth or goes away.
type held struct {
	ws    *websocket.Conn
	first any
}

// dialHeld upgrades, reads the first message and does nothing more. The first message is the challenge of an
// admitted handshake, or the refusal of one that was not.
func dialHeld(t *testing.T, srv *ingresstest.Server) *held {
	t.Helper()
	ctx := testsupport.Context(t)
	ws, _, err := websocket.Dial(ctx, srv.URL(), &websocket.DialOptions{ //nolint:bodyclose // a WebSocket upgrade response has no body to close
		HTTPClient: srv.Client(), Subprotocols: []string{ingresswire.Subprotocol},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read the first message: %v", err)
	}
	msg, err := ingresswire.ParseServerMessage(data)
	if err != nil {
		t.Fatalf("the first message is not a server message: %s: %v", data, err)
	}
	return &held{ws: ws, first: msg}
}

// refusedBy returns the refusal c ended with, after reading the close that follows it. The server holds its
// handshake slot until the node has answered that close, so a test that makes many refused attempts in a row
// reads it; otherwise the slots, not the bucket under test, would run out.
func refusedBy(t *testing.T, c *ingresstest.Conn) *ingresswire.Refuse {
	t.Helper()
	if c.Refuse == nil {
		return nil
	}
	c.AwaitClose(t)
	return c.Refuse
}

func awaitInFlight(t *testing.T, srv *ingresstest.Server, want int) {
	t.Helper()
	if n, err := srv.Hub.AwaitHandshakes(testsupport.Context(t), func(n int) bool { return n == want }); err != nil {
		t.Fatalf("handshakes in flight = %d, want %d: %v", n, want, err)
	}
}

// requireRateLimited fails unless m is a rate_limited refusal that tells the node to wait retryAfterMs.
func requireRateLimited(t *testing.T, m any, retryAfterMs int) {
	t.Helper()
	r, ok := m.(ingresswire.Refuse)
	if !ok {
		t.Fatalf("message = %T, want a refusal", m)
	}
	if r.Code != ingresswire.RefuseRateLimited || r.RetryAfterMs != retryAfterMs {
		t.Errorf("refusal = %q retryAfterMs %d, want %q and %d", r.Code, r.RetryAfterMs, ingresswire.RefuseRateLimited, retryAfterMs)
	}
}

// TestLimitsPerAddressInFlight: one address holds at most eight handshakes at once. The ninth is refused
// rate_limited with retryAfterMs 1000, and audited; another address is admitted; a slot given back admits one.
func TestLimitsPerAddressInFlight(t *testing.T) {
	// The held handshakes stay silent on purpose, so the first-frame timer, a real one (3 s by default), is
	// lengthened to the suite's hang bound, so a slow runner does not see a held slot expire by itself (WR-C-1).
	// The frozen 10 s ingresswire.HandshakeTimeout still bounds a handshake and is now the timer that applies.
	srv := ingresstest.Start(t, ingresstest.Options{AuditRefusals: true, FirstFrame: testsupport.Deadline})

	var open []*held
	for range 8 {
		h := dialHeld(t, srv)
		if _, ok := h.first.(ingresswire.Challenge); !ok {
			t.Fatalf("one of the first eight handshakes was not admitted: %T", h.first)
		}
		open = append(open, h)
	}
	awaitInFlight(t, srv, 8)

	requireRateLimited(t, dialHeld(t, srv).first, 1000)
	rows := refusedRows(t, srv)
	if len(rows) != 1 || rows[0].Reason != ingresswire.RefuseRateLimited || rows[0].ActorType != "anonymous" {
		t.Errorf("audit rows = %+v, want one anonymous rate_limited refusal", rows)
	}

	release, refuse := srv.Hub.GateAdmit("192.0.2.77")
	if refuse != nil {
		t.Fatalf("another address was refused: %+v", refuse)
	}
	release()

	// Giving one slot back admits one handshake.
	_ = open[0].ws.CloseNow()
	awaitInFlight(t, srv, 7)
	if _, ok := dialHeld(t, srv).first.(ingresswire.Challenge); !ok {
		t.Error("a handshake was refused although the address had a free slot")
	}
}

// TestLimitsGlobalInFlight: thirty-two handshakes in flight over many addresses fill the global slots, and the
// next is refused; releasing one admits one.
func TestLimitsGlobalInFlight(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	var releases []func()
	for i := range 32 {
		release, refuse := srv.Hub.GateAdmit(fmt.Sprintf("192.0.2.%d", i+1))
		if refuse != nil {
			t.Fatalf("handshake %d of 32 was refused: %+v", i+1, refuse)
		}
		releases = append(releases, release)
	}
	awaitInFlight(t, srv, 32)

	if release, refuse := srv.Hub.GateAdmit("198.51.100.1"); refuse == nil {
		release()
		t.Fatal("a handshake was admitted with every global slot taken")
	} else {
		requireRateLimited(t, *refuse, 1000)
	}

	releases[0]()
	awaitInFlight(t, srv, 31)
	release, refuse := srv.Hub.GateAdmit("198.51.100.1")
	if refuse != nil {
		t.Fatalf("a handshake was refused after a slot was released: %+v", refuse)
	}
	release()
}

// TestLimitsConnectBucket: an address may start sixty-four handshakes in one instant and earns one more every
// 100 ms.
func TestLimitsConnectBucket(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	const ip = "192.0.2.10"
	for i := range 64 {
		release, refuse := srv.Hub.GateAdmit(ip)
		if refuse != nil {
			t.Fatalf("attempt %d of 64 in the same instant was refused: %+v", i+1, refuse)
		}
		release()
	}
	if release, refuse := srv.Hub.GateAdmit(ip); refuse == nil {
		release()
		t.Fatal("the sixty-fifth attempt in the same instant was admitted")
	} else {
		requireRateLimited(t, *refuse, 1000)
	}

	srv.Clock.Advance(100 * time.Millisecond)
	release, refuse := srv.Hub.GateAdmit(ip)
	if refuse != nil {
		t.Fatalf("an attempt 100 ms later was refused: %+v", refuse)
	}
	release()
	if release, refuse := srv.Hub.GateAdmit(ip); refuse == nil {
		release()
		t.Error("two attempts were admitted after one refill")
	}
}

// TestLimitsEnrolBucket: ten enrolments from one address are judged by the service, the eleventh is refused
// rate_limited with retryAfterMs 30000 before the service sees its token, and one more is judged after 6 s.
// Connect-mode attempts do not spend the bucket.
func TestLimitsEnrolBucket(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{AuditRefusals: true})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")

	// Twelve reconnects of a node the server has never seen: each is refused by the service, none is limited,
	// and none touches the enrolment bucket.
	for range 12 {
		id := ingresstest.NewIdentity()
		id.NodeID = uuid.NewString()
		c := ingresstest.DialNode(t, srv.URL(), srv.Client(), srv.Audience(), id, ingresstest.Dial{})
		if r := refusedBy(t, c); r == nil || r.Code == ingresswire.RefuseRateLimited {
			t.Fatalf("a connect attempt was not judged by the service: refuse %+v, err %v", r, c.Err)
		}
	}

	// Ten enrolments with a token that is well formed and wrong: the service looks it up and refuses it.
	wrong := "tpit_" + env.ID + "_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	for i := range 10 {
		c := ingresstest.DialNode(t, srv.URL(), srv.Client(), srv.Audience(), ingresstest.NewIdentity(), ingresstest.Dial{Token: wrong})
		if r := refusedBy(t, c); r == nil || r.Code != ingresswire.RefuseAuthRejected {
			t.Fatalf("enrolment %d of 10 was not judged by the service: refuse %+v, err %v", i+1, r, c.Err)
		}
	}

	// The eleventh carries the valid token, so only the bucket can have turned it away.
	c := ingresstest.DialNode(t, srv.URL(), srv.Client(), srv.Audience(), ingresstest.NewIdentity(),
		ingresstest.Dial{Token: env.Token, Name: "node-1", SwarmNodeID: "n1"})
	r := refusedBy(t, c)
	if r == nil {
		t.Fatalf("the eleventh enrolment was not refused: welcome %+v, err %v", c.Welcome, c.Err)
	}
	if r.Code != ingresswire.RefuseRateLimited || r.RetryAfterMs != 30000 {
		t.Errorf("refusal = %q retryAfterMs %d, want %q and 30000", r.Code, r.RetryAfterMs, ingresswire.RefuseRateLimited)
	}
	if n := nodeCount(t, srv); n != 0 {
		t.Errorf("%d node rows after refused enrolments, want 0", n)
	}

	// Every enrolment refusal is audited. The ten judged ones carry the service's reason; the eleventh carries
	// rate_limited, which the service never writes, so it was refused before its token was looked at.
	var enrolRows []refusedRow
	for _, r := range refusedRows(t, srv) {
		var d map[string]string
		if err := json.Unmarshal([]byte(r.Details), &d); err != nil {
			t.Fatalf("refusal details %q: %v", r.Details, err)
		}
		if d["mode"] == ingresswire.ModeEnrol {
			enrolRows = append(enrolRows, r)
		}
	}
	if len(enrolRows) != 11 {
		t.Fatalf("%d audited enrolment refusals, want 11", len(enrolRows))
	}
	for i, r := range enrolRows[:10] {
		if r.Reason == ingresswire.RefuseRateLimited {
			t.Errorf("refusal %d was rate limited, want the service's reason", i+1)
		}
	}
	if got := enrolRows[10].Reason; got != ingresswire.RefuseRateLimited {
		t.Errorf("eleventh refusal reason = %q, want %q", got, ingresswire.RefuseRateLimited)
	}

	// Six seconds earn one more attempt, and the token was never spent.
	srv.Clock.Advance(6 * time.Second)
	srv.Enrol(t, ingresstest.NewIdentity(), ingresstest.Dial{Token: env.Token, Name: "node-1", SwarmNodeID: "n1"})
}

// TestLimitsIPv6Slash64IsOneAddress: every address of one /64 shares a budget, and another /64 has its own.
func TestLimitsIPv6Slash64IsOneAddress(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	for range 8 {
		if _, refuse := srv.Hub.GateAdmit("2001:db8::1"); refuse != nil {
			t.Fatalf("a handshake within the per-address budget was refused: %+v", refuse)
		}
	}
	if release, refuse := srv.Hub.GateAdmit("2001:db8::2"); refuse == nil {
		release()
		t.Fatal("a second address of the same /64 got a budget of its own")
	}
	release, refuse := srv.Hub.GateAdmit("2001:db8:0:1::1")
	if refuse != nil {
		t.Fatalf("an address of another /64 was refused: %+v", refuse)
	}
	release()
}

// TestLimitsIdleEntriesExpire: an address idle for ten minutes is forgotten, so the tables cannot grow with
// every address that ever called, and an exhausted address starts again.
func TestLimitsIdleEntriesExpire(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	const ip = "192.0.2.20"
	for range 64 {
		release, _ := srv.Hub.GateAdmit(ip)
		release()
	}
	if _, refuse := srv.Hub.GateAdmit(ip); refuse == nil {
		t.Fatal("the address was not exhausted")
	}
	if connect, _ := srv.Hub.GateEntries(); connect != 1 {
		t.Fatalf("the connect bucket tracks %d addresses, want 1", connect)
	}

	srv.Clock.Advance(10 * time.Minute)
	// A different address is what makes the table look for idle entries.
	release, refuse := srv.Hub.GateAdmit("192.0.2.21")
	if refuse != nil {
		t.Fatalf("a new address was refused: %+v", refuse)
	}
	release()
	if connect, _ := srv.Hub.GateEntries(); connect != 1 {
		t.Errorf("the connect bucket tracks %d addresses after ten idle minutes, want only the new one", connect)
	}
	release, refuse = srv.Hub.GateAdmit(ip)
	if refuse != nil {
		t.Fatalf("an exhausted address was still refused after ten minutes: %+v", refuse)
	}
	release()
}

// TestLimitsFirstFrameTimeout: a client that upgrades and never sends its auth is dropped after the first-frame
// timeout, and its slot is given back.
func TestLimitsFirstFrameTimeout(t *testing.T) {
	// wall-clock: the hub's first-frame timeout is a real timer; a silent client is dropped after it however
	// short it is, so a short value cannot fail a correct run.
	srv := ingresstest.Start(t, ingresstest.Options{FirstFrame: 50 * time.Millisecond})
	h := dialHeld(t, srv)
	if _, ok := h.first.(ingresswire.Challenge); !ok {
		t.Fatalf("the handshake was not admitted: %T", h.first)
	}
	// The slot is given back without the client doing anything more.
	awaitInFlight(t, srv, 0)
	if _, _, err := h.ws.Read(testsupport.Context(t)); err == nil {
		t.Error("the connection of a client that never sent its auth is still open")
	}
}

// TestLimitsAreSeparateFromTheAgentHub: the ingress gate is its own. An address that has used up its ingress
// connect budget is still admitted by the agent hub.
func TestLimitsAreSeparateFromTheAgentHub(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	agents := tunneltest.Start(t, tunneltest.Options{})

	// The test client's address, as the hub sees it.
	const ip = "127.0.0.1"
	for range 64 {
		release, refuse := srv.Hub.GateAdmit(ip)
		if refuse != nil {
			t.Fatalf("could not spend the ingress budget: %+v", refuse)
		}
		release()
	}
	requireRateLimited(t, dialHeld(t, srv).first, 1000)

	// Begin fails the test unless the agent hub answers the connection with its challenge.
	agents.Begin(t, tunneltest.Agent{})
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/ianglennon/tierplan/internal/ingresshub/ingresstest"
	"github.com/ianglennon/tierplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/tierplan/proto/authn"
	"github.com/ianglennon/tierplan/proto/ingresswire"
	"github.com/ianglennon/tierplan/proto/testsupport"
	"github.com/ianglennon/tierplan/proto/wire"
)

// The agent endpoint and the ingress endpoint are one server speaking two protocols with two kinds of
// credential. These tests run the real hub of each (ingresstest and tunneltest, neither edited) and prove that
// nothing made for one is accepted by the other: not a signature, not a join token, not a subprotocol (D-07).

// ccAgentRows is how many agents the tunneltest server's database holds.
func ccAgentRows(t *testing.T, tun *tunneltest.Server) int {
	t.Helper()
	var n int
	if err := tun.Store.Read.QueryRow(`SELECT COUNT(*) FROM agents`).Scan(&n); err != nil {
		t.Fatalf("count the agents: %v", err)
	}
	return n
}

// ccIngressEnrolAtAgent makes the ingress node's enrolment by hand at the agent endpoint: it dials
// /agent/connect with the agent subprotocol, reads the agent challenge and answers with an enrol-mode auth that
// carries token and a fresh key. The EKM sent and signed is the export of ekmLabel, and the signature is made
// over transcript, so a test chooses exactly what is wrong. It returns what the server answered.
func ccIngressEnrolAtAgent(t *testing.T, tun *tunneltest.Server, token, ekmLabel string,
	transcript func(mode string, proto int, serverNonce, nodeNonce []byte, aud string, pub, ekm []byte) []byte) any {
	t.Helper()
	ctx := testsupport.Context(t)
	ws, resp, err := websocket.Dial(ctx, tun.URL(), &websocket.DialOptions{ //nolint:bodyclose // a WebSocket upgrade response has no body to close
		HTTPClient: tun.HTTP.Client(), Subprotocols: []string{wire.Subprotocol},
	})
	if err != nil {
		t.Fatalf("dial the agent endpoint: %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read the agent challenge: %v", err)
	}
	var ch wire.Challenge
	if err := json.Unmarshal(data, &ch); err != nil || ch.T != wire.TypeChallenge {
		t.Fatalf("challenge = %s, %v", data, err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("read random bytes: %v", err)
	}
	ekm := authn.EKMFor(resp.TLS, ekmLabel)
	if ekm == nil {
		t.Fatalf("the TLS session exported no keying material for %q", ekmLabel)
	}
	a := wire.Auth{
		T: wire.TypeAuth, Mode: wire.ModeEnrol, Proto: wire.ProtoVersion, AgentVersion: "0.2.0", Aud: tun.Audience(),
		Nonce: nonce, TS: tun.Clock.Now().UnixMilli(), EKM: ekm, Pub: pub, Token: token,
		Facts: wire.Facts{EngineID: "ENG:cross", Hostname: "cross-host", EngineReachable: true},
	}
	a.Sig = authn.Sign(priv, transcript(wire.ModeEnrol, wire.ProtoVersion, ch.Nonce, nonce, a.Aud, pub, ekm))
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("encode the auth: %v", err)
	}
	if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write the auth: %v", err)
	}
	_, data, err = ws.Read(ctx)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	var head struct{ T string }
	if err := json.Unmarshal(data, &head); err != nil {
		t.Fatalf("answer %s: %v", data, err)
	}
	switch head.T {
	case wire.TypeRefuse:
		var r wire.Refuse
		_ = json.Unmarshal(data, &r)
		return r
	case wire.TypeWelcome:
		var w wire.Welcome
		_ = json.Unmarshal(data, &w)
		return w
	}
	t.Fatalf("unexpected answer %s", data)
	return nil
}

// TestCrossProtocolAgentSignatureAtIngress: an auth signed over the agent's transcript is refused auth_rejected at
// /ingress/connect, in enrol mode (no row is written) and in connect mode (the node's own key does not help).
func TestCrossProtocolAgentSignatureAtIngress(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")

	enrol := ingresstest.DialNode(t, srv.URL(), srv.Client(), srv.Audience(), ingresstest.NewIdentity(),
		ingresstest.Dial{Mode: ingresswire.ModeEnrol, Token: env.Token, Name: "node-n1", SwarmNodeID: "n1", AgentTranscript: true})
	if enrol.Welcome != nil || enrol.Refuse == nil || enrol.Refuse.Code != ingresswire.RefuseAuthRejected {
		t.Fatalf("enrol with an agent signature: welcome %+v refuse %+v err %v, want refuse %q", enrol.Welcome, enrol.Refuse, enrol.Err, ingresswire.RefuseAuthRejected)
	}
	if n := nodeCount(t, srv); n != 0 {
		t.Errorf("%d ingress_nodes rows after the refused enrolment, want 0", n)
	}

	// The same token and key, signed for the ingress channel, enrol: the agent signature was the only fault.
	id := ingresstest.NewIdentity()
	c := srv.Enrol(t, id, ingresstest.Dial{Token: env.Token, Name: "node-n1", SwarmNodeID: "n1"})
	if c.Welcome == nil {
		t.Fatal("the control enrolment was not welcomed")
	}

	conn := ingresstest.DialNode(t, srv.URL(), srv.Client(), srv.Audience(), id,
		ingresstest.Dial{Mode: ingresswire.ModeConnect, AgentTranscript: true})
	if conn.Welcome != nil || conn.Refuse == nil || conn.Refuse.Code != ingresswire.RefuseAuthRejected {
		t.Fatalf("connect with an agent signature: welcome %+v refuse %+v err %v, want refuse %q", conn.Welcome, conn.Refuse, conn.Err, ingresswire.RefuseAuthRejected)
	}
	// And the control for connect: the node's key signing the ingress transcript is welcomed.
	if again := srv.Connect(t, id, ingresstest.Dial{}); again.Welcome == nil {
		t.Error("the control connect was not welcomed")
	}
}

// TestCrossProtocolIngressSignatureAtAgent: an ingress-signed auth sent to the agent endpoint, with a valid agent
// join token, is refused auth_rejected and no agent row appears. It is tried twice: with only the signature's
// domain wrong, and as a complete ingress handshake (the ingress exporter label's value sent and signed).
func TestCrossProtocolIngressSignatureAtAgent(t *testing.T) {
	cases := map[string]string{
		"agent exporter value, ingress transcript":   authn.EKMLabel,
		"ingress exporter value, ingress transcript": authn.IngressEKMLabel,
	}
	for name, label := range cases {
		t.Run(name, func(t *testing.T) {
			tun := tunneltest.Start(t, tunneltest.Options{})
			env := tun.NewEnvironment(t, "cross")
			got := ccIngressEnrolAtAgent(t, tun, env.Token, label, authn.IngressTranscript)
			r, ok := got.(wire.Refuse)
			if !ok || r.Code != wire.ReasonAuthRejected {
				t.Fatalf("answer = %+v, want refuse %q", got, wire.ReasonAuthRejected)
			}
			if n := ccAgentRows(t, tun); n != 0 {
				t.Errorf("%d agent rows after the refused enrolment, want 0", n)
			}
			// The token was not spent: an agent that signs the agent transcript enrols with it.
			if c := tun.Enrol(t, env); c.Welcome == nil {
				t.Error("the control enrolment with the same token was not welcomed")
			}
		})
	}
}

// TestCrossProtocolAgentTokenAtIngressNamesTheKind: an agent join token (tpjt_) at ingress enrolment is refused
// wrong_token_kind, and the detail names the ingress join token the person needs.
func TestCrossProtocolAgentTokenAtIngressNamesTheKind(t *testing.T) {
	tun := tunneltest.Start(t, tunneltest.Options{})
	agentEnv := tun.NewEnvironment(t, "cross")
	if !strings.HasPrefix(agentEnv.Token, "tpjt_") {
		t.Fatalf("the agent environment's token %q is not a tpjt_ token", agentEnv.Token[:min(len(agentEnv.Token), 5)])
	}

	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")
	c := ingresstest.DialNode(t, srv.URL(), srv.Client(), srv.Audience(), ingresstest.NewIdentity(),
		ingresstest.Dial{Mode: ingresswire.ModeEnrol, WireToken: agentEnv.Token, Name: "node-n1", SwarmNodeID: "n1"})
	if c.Welcome != nil || c.Refuse == nil || c.Refuse.Code != ingresswire.RefuseWrongTokenKind {
		t.Fatalf("agent token at ingress: welcome %+v refuse %+v err %v, want refuse %q", c.Welcome, c.Refuse, c.Err, ingresswire.RefuseWrongTokenKind)
	}
	if !strings.Contains(c.Refuse.Detail, "ingress join token") {
		t.Errorf("the detail %q does not name the ingress join token", c.Refuse.Detail)
	}
	if n := nodeCount(t, srv); n != 0 {
		t.Errorf("%d ingress_nodes rows, want 0", n)
	}
}

// TestCrossProtocolIngressTokenAtAgent: an ingress join token (tpit_) through the real agent endpoint is refused
// auth_rejected, the one answer every bad credential gets there, and no agent row is written.
func TestCrossProtocolIngressTokenAtAgent(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	if !strings.HasPrefix(env.Token, "tpit_") {
		t.Fatalf("the ingress environment's token %q is not a tpit_ token", env.Token[:min(len(env.Token), 5)])
	}

	tun := tunneltest.Start(t, tunneltest.Options{})
	c := tun.Dial(t, tunneltest.Agent{Token: env.Token})
	if c.Welcome != nil || c.Refuse == nil || c.Refuse.Code != wire.ReasonAuthRejected {
		t.Fatalf("ingress token at agent: welcome %+v refuse %+v err %v, want refuse %q", c.Welcome, c.Refuse, c.Err, wire.ReasonAuthRejected)
	}
	if n := ccAgentRows(t, tun); n != 0 {
		t.Errorf("%d agent rows, want 0", n)
	}
}

// TestCrossProtocolSubprotocolsBothWays: each endpoint accepts the upgrade and then closes 1008, naming the
// subprotocol it requires, when the client offers the other endpoint's subprotocol.
func TestCrossProtocolSubprotocolsBothWays(t *testing.T) {
	t.Run("ingress subprotocol at the agent endpoint", func(t *testing.T) {
		tun := tunneltest.Start(t, tunneltest.Options{})
		ccExpectClose(t, tun.URL(), tun.HTTP.Client(), ingresswire.Subprotocol, wire.Subprotocol)
		if n := ccAgentRows(t, tun); n != 0 {
			t.Errorf("%d agent rows, want 0", n)
		}
	})
	t.Run("agent subprotocol at the ingress endpoint", func(t *testing.T) {
		srv := ingresstest.Start(t, ingresstest.Options{})
		ccExpectClose(t, srv.URL(), srv.Client(), wire.Subprotocol, ingresswire.Subprotocol)
		if n := nodeCount(t, srv); n != 0 {
			t.Errorf("%d ingress_nodes rows, want 0", n)
		}
	})
}

// ccExpectClose dials url offering only offered and requires the server to close with 1008 and a reason that
// names the subprotocol required. The server accepts the upgrade first, so the close is read from the first
// message the client waits for.
func ccExpectClose(t *testing.T, url string, client *http.Client, offered, required string) {
	t.Helper()
	ctx := testsupport.Context(t)
	ws, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{ //nolint:bodyclose // a WebSocket upgrade response has no body to close
		HTTPClient: client, Subprotocols: []string{offered},
	})
	if err != nil {
		t.Fatalf("dial offering %q: %v", offered, err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != "" {
		t.Errorf("the server agreed to the subprotocol %q", got)
	}
	_, data, err := ws.Read(ctx)
	if err == nil {
		t.Fatalf("the server sent a message to a client of the wrong subprotocol: %s", data)
	}
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("the connection ended without a close frame: %v", err)
	}
	if ce.Code != websocket.StatusPolicyViolation || !strings.Contains(ce.Reason, required) {
		t.Errorf("closed with %d %q, want 1008 naming %q", ce.Code, ce.Reason, required)
	}
}

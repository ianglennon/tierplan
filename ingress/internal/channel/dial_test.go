// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package channel

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ianglennon/tierplan/proto/authn"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/ingresswire"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

const testToken = "tpit_env1_0123456789abcdef0123456789abcdef"

// testNow is the fixed clock every dial in this file stamps into its auth message.
var testNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func fixedNow() time.Time { return testNow }

// script says how the test server answers one connection.
type script struct {
	// welcome is what the server sends when it does not refuse.
	welcome ingresswire.Welcome
	// refuse, when set, is sent instead of a welcome.
	refuse *ingresswire.Refuse
	// beforeHello, when set, is closed by the node's callback; the server reads
	// hello only after it.
	beforeHello <-chan struct{}
}

// saw is what the test server observed of one connection.
type saw struct {
	Auth ingresswire.Auth
	// SigOK is whether the signature verifies, with the server's own rebuild of
	// the transcript from its nonce, the node's nonce, the audience, the key and
	// the exporter of the server side of the TLS session.
	SigOK bool
	// AudOK is whether the audience is the host[:port] the server listens on.
	AudOK bool
	// EKMOK is whether the exporter the node sent equals the server's own.
	EKMOK bool
	// Hello is the hello the server read, nil when none arrived.
	Hello *ingresswire.Hello
	// HelloErr is why no hello arrived: the connection closed.
	HelloErr error
}

// testServer is a TLS WebSocket server speaking tierplan.ingress.v1 from the
// server's side, using the same proto functions the real server does.
type testServer struct {
	ts  *httptest.Server
	saw chan saw
}

func newTestServer(t *testing.T, sc script) *testServer {
	t.Helper()
	s := &testServer{saw: make(chan saw, 1)}
	s.ts = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.serve(t, sc, w, r) }))
	s.ts.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.ts.StartTLS()
	t.Cleanup(s.ts.Close)
	return s
}

// url is the server's ingress address, wss.
func (s *testServer) url() string {
	return "wss" + strings.TrimPrefix(s.ts.URL, "https") + "/ingress/connect"
}

// pin is the SPKI pin of the server's own (self-signed) certificate.
func (s *testServer) pin() string { return authn.SPKIPin(s.ts.Certificate()) }

// next is what the server saw, once the connection has got that far.
func (s *testServer) next(t *testing.T) saw {
	t.Helper()
	return testsupport.Await(t, "the server's report of the connection", s.saw)
}

func (s *testServer) serve(t *testing.T, sc script, w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{ingresswire.Subprotocol}})
	if err != nil {
		t.Errorf("test server: accept: %v", err)
		return
	}
	defer func() { _ = c.CloseNow() }()
	ctx := r.Context()

	nonce, err := ingresswire.NewNonce()
	if err != nil {
		t.Errorf("test server: nonce: %v", err)
		return
	}
	send := func(v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			t.Errorf("test server: encode: %v", err)
			return false
		}
		return c.Write(ctx, websocket.MessageText, b) == nil
	}
	if !send(ingresswire.Challenge{T: ingresswire.TypeChallenge, Proto: ingresswire.ProtoVersion, Nonce: nonce, TS: testNow.Unix()}) {
		return
	}

	var got saw
	_, data, err := c.Read(ctx)
	if err != nil {
		got.HelloErr = err
		s.saw <- got
		return
	}
	if err := json.Unmarshal(data, &got.Auth); err != nil {
		t.Errorf("test server: the auth message is not JSON: %v", err)
		return
	}
	a := got.Auth
	aud, _ := authn.Audience(s.url())
	ekm := authn.EKMFor(r.TLS, authn.IngressEKMLabel)
	got.AudOK = a.Aud == aud
	got.EKMOK = authn.EKMEqual(a.EKM, ekm)
	got.SigOK = authn.Verify(a.Pub, authn.IngressTranscript(a.Mode, a.Proto, nonce, a.Nonce, a.Aud, a.Pub, ekm), a.Sig)

	if sc.refuse != nil {
		send(*sc.refuse)
		s.saw <- got
		drain(ctx, c)
		return
	}
	if !send(sc.welcome) {
		return
	}
	if sc.beforeHello != nil {
		select {
		case <-sc.beforeHello:
		case <-ctx.Done():
			return
		}
	}
	_, data, err = c.Read(ctx)
	if err != nil {
		got.HelloErr = err
		s.saw <- got
		return
	}
	msg, err := ingresswire.ParseNodeMessage(data)
	if h, ok := msg.(ingresswire.Hello); err == nil && ok {
		got.Hello = &h
	} else {
		t.Errorf("test server: the node's first message after the welcome is not a hello: %s", data)
	}
	s.saw <- got
	drain(ctx, c)
}

// drain reads until the node closes, which is how the node ends a session.
func drain(ctx context.Context, c *websocket.Conn) {
	for {
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
	}
}

func testWelcome(instance string) ingresswire.Welcome {
	return ingresswire.Welcome{
		T: ingresswire.TypeWelcome, NodeID: "n-1", EnvID: "e-1", InstanceID: instance,
		HealthMs: 30000, ServerTime: testNow.Unix(),
	}
}

func testKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	k, err := ingresswire.NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func tokenFunc(token string) func() (string, error) {
	return func() (string, error) { return token, nil }
}

// TestEnrolPersistsBeforeHello is the tracer: a node enrols over a real TLS
// WebSocket, its identity is handed to the caller, and only then does the server
// read hello (D-23, D-38).
func TestEnrolPersistsBeforeHello(t *testing.T) {
	released := make(chan struct{})
	s := newTestServer(t, script{welcome: testWelcome("i-1"), beforeHello: released})

	var enrolled []ingresswire.Welcome
	o := Options{
		URL: s.url(), InstallPins: []string{s.pin()}, Version: "v0.2.0-test", Now: fixedNow,
		AppliedID: func() string { return "b-1" },
		OnEnrolled: func(w ingresswire.Welcome) error {
			enrolled = append(enrolled, w)
			close(released)
			return nil
		},
	}
	conn, err := Dial(testsupport.Context(t), o, Credentials{
		Key: testKey(t), Name: "node-a", SwarmNodeID: "sw-1", Token: tokenFunc(testToken),
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close("test done") })

	if len(enrolled) != 1 || enrolled[0].NodeID != "n-1" || enrolled[0].InstanceID != "i-1" {
		t.Fatalf("OnEnrolled calls = %+v, want one with node n-1 and instance i-1", enrolled)
	}
	if w := conn.Welcome(); w.NodeID != "n-1" || w.EnvID != "e-1" || w.HealthMs != 30000 {
		t.Errorf("Welcome() = %+v", w)
	}
	if got := conn.InstanceID(); got != "i-1" {
		t.Errorf("InstanceID() = %q, want the welcome's i-1 to persist", got)
	}

	got := s.next(t)
	a := got.Auth
	if a.Mode != ingresswire.ModeEnrol || a.Token != testToken || a.Name != "node-a" || a.SwarmNodeID != "sw-1" {
		t.Errorf("auth = mode %q name %q swarm %q (token match %v), want an enrol with the token and claims", a.Mode, a.Name, a.SwarmNodeID, a.Token == testToken)
	}
	if !got.SigOK || !got.AudOK || !got.EKMOK {
		t.Errorf("signature ok %v, audience ok %v, exporter ok %v, want all three", got.SigOK, got.AudOK, got.EKMOK)
	}
	if got.Hello == nil {
		t.Fatalf("the server read no hello: %v", got.HelloErr)
	}
	if got.Hello.Version != "v0.2.0-test" || got.Hello.AppliedID != "b-1" || !slices.Equal(got.Hello.Schemas, ingressbundle.SupportedSchemas()) {
		t.Errorf("hello = %+v, want the version, applied ID b-1 and schemas %v", *got.Hello, ingressbundle.SupportedSchemas())
	}
}

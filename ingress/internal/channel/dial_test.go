// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package channel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
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

const testToken = "tpit_env1_0123456789abcdef0123456789abcdef" //nolint:gosec // a fixed fake join token for tests

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
	// session, when set, runs after the server has read hello and replaces the
	// plain drain: it is the server's half of a bundle session. The server then
	// reports nothing on saw, so a test that uses it does not call next.
	session func(ctx context.Context, c *websocket.Conn)
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
	if sc.session != nil {
		sc.session(ctx, c)
		return
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

// connectCreds are the credentials of a node that enrolled before: it has an ID
// and the instance it pinned, and its token function must never be called.
func connectCreds(t *testing.T, key ed25519.PrivateKey) Credentials {
	t.Helper()
	return Credentials{
		Key: key, NodeID: "n-1", InstanceID: "i-1",
		Token: func() (string, error) {
			t.Error("the join token was read in connect mode (D-22)")
			return "", errors.New("must not be called")
		},
	}
}

func TestConnectModeNeverReadsToken(t *testing.T) {
	s := newTestServer(t, script{welcome: testWelcome("i-1")})
	enrolledCalls := 0
	o := Options{
		URL: s.url(), InstallPins: []string{s.pin()}, Version: "v0.2.0-test", Now: fixedNow,
		OnEnrolled: func(ingresswire.Welcome) error { enrolledCalls++; return nil },
	}
	conn, err := Dial(testsupport.Context(t), o, connectCreds(t, testKey(t)))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close("test done") })

	got := s.next(t)
	if got.Auth.Mode != ingresswire.ModeConnect || got.Auth.NodeID != "n-1" || got.Auth.Token != "" {
		t.Errorf("auth = mode %q node %q token set %v, want connect, n-1 and no token", got.Auth.Mode, got.Auth.NodeID, got.Auth.Token != "")
	}
	if !got.SigOK || !got.AudOK || !got.EKMOK {
		t.Errorf("signature over the connect transcript ok %v, audience ok %v, exporter ok %v, want all three", got.SigOK, got.AudOK, got.EKMOK)
	}
	if got.Hello == nil {
		t.Fatalf("the server read no hello: %v", got.HelloErr)
	}
	if enrolledCalls != 0 {
		t.Errorf("OnEnrolled was called %d times in connect mode", enrolledCalls)
	}
	if conn.InstanceID() != "i-1" {
		t.Errorf("InstanceID() = %q, want the pinned i-1", conn.InstanceID())
	}
}

func TestRefusalCarriesRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		refuse ingresswire.Refuse
		want   time.Duration
	}{
		{ingresswire.Refuse{T: ingresswire.TypeRefuse, Code: ingresswire.RefuseRateLimited, Detail: "slow down", RetryAfterMs: 1000}, time.Second},
		{ingresswire.Refuse{T: ingresswire.TypeRefuse, Code: ingresswire.RefuseNodeRevoked}, 0},
	} {
		t.Run(tc.refuse.Code, func(t *testing.T) {
			s := newTestServer(t, script{refuse: &tc.refuse})
			o := Options{URL: s.url(), InstallPins: []string{s.pin()}, Now: fixedNow}
			_, err := Dial(testsupport.Context(t), o, Credentials{Key: testKey(t), Token: tokenFunc(testToken)})
			var re *RefusedError
			if !errors.As(err, &re) {
				t.Fatalf("Dial error = %v, want a *RefusedError", err)
			}
			if re.Code != tc.refuse.Code || re.Detail != tc.refuse.Detail || re.RetryAfter != tc.want {
				t.Errorf("RefusedError = %+v, want code %q detail %q retry-after %v", *re, tc.refuse.Code, tc.refuse.Detail, tc.want)
			}
			if !strings.Contains(re.Error(), tc.refuse.Code) {
				t.Errorf("Error() = %q, want it to name the code", re.Error())
			}
			if got := s.next(t); got.Hello != nil {
				t.Error("a hello was sent after a refusal")
			}
		})
	}
}

func TestWelcomeInstanceMismatch(t *testing.T) {
	s := newTestServer(t, script{welcome: testWelcome("i-2")})
	o := Options{URL: s.url(), InstallPins: []string{s.pin()}, Now: fixedNow}
	_, err := Dial(testsupport.Context(t), o, connectCreds(t, testKey(t))) // pinned to i-1
	if !errors.Is(err, ingresswire.ErrInstanceMismatch) {
		t.Fatalf("Dial error = %v, want ErrInstanceMismatch", err)
	}
	got := s.next(t)
	if got.Hello != nil || got.HelloErr == nil {
		t.Errorf("hello = %+v, read error = %v, want the connection closed with no hello", got.Hello, got.HelloErr)
	}
}

func TestOnEnrolledErrorStopsBeforeHello(t *testing.T) {
	s := newTestServer(t, script{welcome: testWelcome("i-1")})
	saveErr := errors.New("disk full")
	o := Options{
		URL: s.url(), InstallPins: []string{s.pin()}, Now: fixedNow,
		OnEnrolled: func(ingresswire.Welcome) error { return saveErr },
	}
	_, err := Dial(testsupport.Context(t), o, Credentials{Key: testKey(t), Token: tokenFunc(testToken)})
	if !errors.Is(err, saveErr) {
		t.Fatalf("Dial error = %v, want the callback's error wrapped", err)
	}
	got := s.next(t)
	if got.Hello != nil || got.HelloErr == nil {
		t.Errorf("hello = %+v, read error = %v, want the connection closed with no hello", got.Hello, got.HelloErr)
	}
}

// refusingTransport fails the test when it is used at all.
type refusingTransport struct{ t *testing.T }

func (r refusingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.t.Error("Dial made an HTTP request for a URL that is not wss")
	return nil, errors.New("must not be called")
}

func TestDialRefusesPlainWS(t *testing.T) {
	lc := net.ListenConfig{}
	ln, err := lc.Listen(testsupport.Context(t), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan int, 1)
	go func() {
		n := 0
		for {
			c, err := ln.Accept()
			if err != nil {
				accepted <- n
				return
			}
			n++
			_ = c.Close()
		}
	}()

	for name, o := range map[string]Options{
		"default client":     {URL: "ws://" + ln.Addr().String() + "/ingress/connect"},
		"transport spy":      {URL: "ws://" + ln.Addr().String() + "/ingress/connect", HTTPClient: &http.Client{Transport: refusingTransport{t}}},
		"https is not wss":   {URL: "https://" + ln.Addr().String() + "/ingress/connect"},
		"no scheme":          {URL: ln.Addr().String() + "/ingress/connect"},
		"empty":              {URL: ""},
		"wss without a host": {URL: "wss:///ingress/connect"},
	} {
		_, err := Dial(testsupport.Context(t), o, Credentials{Key: testKey(t), Token: tokenFunc(testToken)})
		if err == nil {
			t.Errorf("%s: Dial succeeded for %q", name, o.URL)
		}
	}
	// Every attempt above returned before this point, so a connection it made is
	// already in the listener's queue.
	_ = ln.Close()
	if n := testsupport.Await(t, "the listener's accept count", accepted); n != 0 {
		t.Errorf("%d connections reached the listener, want none", n)
	}
}

func TestDialErrorsHoldNoToken(t *testing.T) {
	key := testKey(t)
	agentToken := ingresswire.AgentTokenPrefix + "agent-secret-value-0123456789"
	forbidden := []string{testToken, agentToken, "agent-secret-value"}
	for _, raw := range [][]byte{key, key.Seed()} {
		forbidden = append(forbidden,
			base64.StdEncoding.EncodeToString(raw), base64.URLEncoding.EncodeToString(raw),
			base64.RawStdEncoding.EncodeToString(raw), base64.RawURLEncoding.EncodeToString(raw),
			hex.EncodeToString(raw), strings.ToUpper(hex.EncodeToString(raw)))
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cases := []struct {
		name  string
		sc    script
		mod   func(s *testServer, o *Options, cr *Credentials)
		works bool
	}{
		{name: "refused", sc: script{refuse: &ingresswire.Refuse{T: ingresswire.TypeRefuse, Code: ingresswire.RefuseAuthRejected, Detail: "bad credential"}}},
		{name: "instance mismatch", sc: script{welcome: testWelcome("i-2")}, mod: func(_ *testServer, _ *Options, cr *Credentials) { cr.InstanceID = "i-1" }},
		{name: "enrolment not saved", sc: script{welcome: testWelcome("i-1")}, mod: func(_ *testServer, o *Options, _ *Credentials) {
			o.OnEnrolled = func(ingresswire.Welcome) error { return errors.New("disk full") }
		}},
		{name: "untrusted server", sc: script{welcome: testWelcome("i-1")}, mod: func(_ *testServer, o *Options, _ *Credentials) { o.InstallPins = nil }},
		{name: "plain ws", mod: func(_ *testServer, o *Options, _ *Credentials) { o.URL = "ws://127.0.0.1:1/ingress/connect" }},
		{name: "token unavailable", sc: script{welcome: testWelcome("i-1")}, mod: func(_ *testServer, _ *Options, cr *Credentials) {
			cr.Token = func() (string, error) { return "", errors.New("the token file is missing") }
		}},
		{name: "agent token", sc: script{welcome: testWelcome("i-1")}, mod: func(_ *testServer, _ *Options, cr *Credentials) { cr.Token = tokenFunc(agentToken) }},
		{name: "welcomed", sc: script{welcome: testWelcome("i-1")}, works: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, tc.sc)
			o := Options{URL: s.url(), InstallPins: []string{s.pin()}, Version: "v0.2.0-test", Now: fixedNow, Log: logger}
			cr := Credentials{Key: key, Name: "node-a", Token: tokenFunc(testToken)}
			if tc.mod != nil {
				tc.mod(s, &o, &cr)
			}
			conn, err := Dial(testsupport.Context(t), o, cr)
			switch {
			case tc.works && err != nil:
				t.Fatalf("Dial: %v", err)
			case tc.works:
				_ = conn.Close("test done")
			case err == nil:
				t.Fatal("Dial succeeded, want a failure")
			default:
				for _, f := range forbidden {
					if strings.Contains(err.Error(), f) {
						t.Errorf("the error %q holds a secret (%d bytes of %q...)", err, len(f), f[:min(len(f), 6)])
					}
				}
			}
		})
	}
	if logs.Len() == 0 {
		t.Error("the captured log is empty, so the check of the log proves nothing")
	}
	for _, f := range forbidden {
		if strings.Contains(logs.String(), f) {
			t.Errorf("the log holds a secret (%d bytes of %q...)", len(f), f[:min(len(f), 6)])
		}
	}
}

func TestNodeName(t *testing.T) {
	host := func(name string, err error) func() (string, error) {
		return func() (string, error) { return name, err }
	}
	for _, tc := range []struct {
		name     string
		hostname func() (string, error)
		want     string
	}{
		{"hostname wins", host("edge-1", nil), "edge-1"},
		{"an error falls back to the node ID", host("", errors.New("no hostname")), "node-id-7"},
		{"an empty hostname falls back to the node ID", host("", nil), "node-id-7"},
		{"no hostname function", nil, "node-id-7"},
		{"a long hostname is cut to the limit", host(strings.Repeat("h", 200), nil), strings.Repeat("h", ingresswire.MaxNameLen)},
		{"a cut never splits a character", host(strings.Repeat("h", ingresswire.MaxNameLen-1)+"\u00e9", nil), strings.Repeat("h", ingresswire.MaxNameLen-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := NodeName(tc.hostname, "node-id-7")
			if got != tc.want {
				t.Errorf("NodeName = %q, want %q", got, tc.want)
			}
			if len(got) > ingresswire.MaxNameLen {
				t.Errorf("len = %d, want at most %d", len(got), ingresswire.MaxNameLen)
			}
		})
	}
	if got := NodeName(host(strings.Repeat("h", 200), nil), ""); len(got) != 128 {
		t.Errorf("a 200-byte hostname gave %d bytes, want 128", len(got))
	}
}

// TestDefaultHandshakeHasADeadline is WR-A-01: with no HandshakeContext, Dial bounds the handshake by
// ingresswire.HandshakeTimeout, so the supervisor's loop context (which has no deadline) cannot let one
// stalled server park the reconnect loop for good.
func TestDefaultHandshakeHasADeadline(t *testing.T) {
	hctx, cancel := Options{}.handshakeContext(testsupport.Context(t))
	defer cancel()
	dl, ok := hctx.Deadline()
	if !ok {
		t.Fatal("the default handshake context has no deadline")
	}
	if left := time.Until(dl); left <= 0 || left > ingresswire.HandshakeTimeout {
		t.Fatalf("the default handshake deadline is %v away, want within (0, %v]", left, ingresswire.HandshakeTimeout)
	}
}

// stallingServer is a TLS server that accepts the connection and then never answers. With upgrade set it
// completes the WebSocket upgrade and sends no challenge; without it, it never answers the upgrade request
// at all. It signals stalled once the node is waiting on it.
func stallingServer(t *testing.T, upgrade bool) (url, pin string, stalled <-chan struct{}) {
	t.Helper()
	sig := make(chan struct{}, 1)
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if upgrade {
			c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{ingresswire.Subprotocol}})
			if err != nil {
				t.Errorf("stalling server: accept: %v", err)
				return
			}
			defer func() { _ = c.CloseNow() }()
		}
		sig <- struct{}{}
		<-r.Context().Done()
	}))
	ts.Config.ErrorLog = log.New(io.Discard, "", 0)
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return "wss" + strings.TrimPrefix(ts.URL, "https") + "/ingress/connect", authn.SPKIPin(ts.Certificate()), sig
}

// TestDialGivesUpOnAStalledServer is WR-A-01: a server that accepts the TCP and TLS connection and then
// answers neither the upgrade nor the challenge ends Dial when the handshake context ends. The test ends
// that context itself once the server is stalled, so no clock is waited on; before the fix Dial used the
// caller's context for every step and blocked until the test's own deadline.
func TestDialGivesUpOnAStalledServer(t *testing.T) {
	for name, upgrade := range map[string]bool{"never answers the upgrade": false, "never sends the challenge": true} {
		t.Run(name, func(t *testing.T) {
			url, pin, stalled := stallingServer(t, upgrade)
			expire := make(chan context.CancelFunc, 1)
			o := Options{
				URL: url, InstallPins: []string{pin}, Now: fixedNow,
				HandshakeContext: func(ctx context.Context) (context.Context, context.CancelFunc) {
					hctx, cancel := context.WithCancel(ctx)
					expire <- cancel
					return hctx, cancel
				},
			}
			go func() {
				cancel := <-expire
				<-stalled
				cancel()
			}()
			conn, err := Dial(testsupport.Context(t), o, Credentials{Key: testKey(t), Token: tokenFunc(testToken)})
			if err == nil {
				_ = conn.Close("unexpected")
				t.Fatal("Dial succeeded against a server that never answers")
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Dial error = %v, want the handshake context's cancellation", err)
			}
		})
	}
}

// TestDialRefusesAForeignSubprotocol is WR-A-01: a peer that completes the upgrade without selecting
// tierplan.ingress.v1 is not treated as a protocol participant, so no auth message goes to it.
func TestDialRefusesAForeignSubprotocol(t *testing.T) {
	got := make(chan bool, 1)
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil) // selects no subprotocol
		if err != nil {
			t.Errorf("foreign server: accept: %v", err)
			return
		}
		defer func() { _ = c.CloseNow() }()
		// The node must close without sending anything: the first read ends with its close.
		_, _, err = c.Read(r.Context())
		got <- err != nil
	}))
	ts.Config.ErrorLog = log.New(io.Discard, "", 0)
	ts.StartTLS()
	t.Cleanup(ts.Close)

	o := Options{
		URL: "wss" + strings.TrimPrefix(ts.URL, "https") + "/ingress/connect", InstallPins: []string{authn.SPKIPin(ts.Certificate())}, Now: fixedNow,
	}
	_, err := Dial(testsupport.Context(t), o, Credentials{Key: testKey(t), Token: tokenFunc(testToken)})
	if err == nil || !strings.Contains(err.Error(), ingresswire.Subprotocol) {
		t.Fatalf("Dial error = %v, want a refusal naming %s", err, ingresswire.Subprotocol)
	}
	if !testsupport.Await(t, "the foreign server's first read", got) {
		t.Error("the node sent a message to a server that did not speak the protocol")
	}
}

// TestRefusalRetryAfterCannotOverflow is IN-A-01: a retryAfterMs so large that ms*1e6 wraps a Duration to a
// small positive value (here 448us) is clamped before the multiplication, to MaxRetryAfter.
func TestRefusalRetryAfterCannotOverflow(t *testing.T) {
	const wraps = 18446744073710 // times 1e6 is 2^64 + 448384, a small positive Duration once wrapped
	ms := int64(wraps)
	if wrapped := time.Duration(ms) * time.Millisecond; wrapped <= 0 || wrapped >= time.Second {
		t.Fatalf("the test value no longer wraps to a small positive Duration (%v)", wrapped)
	}
	s := newTestServer(t, script{refuse: &ingresswire.Refuse{T: ingresswire.TypeRefuse, Code: ingresswire.RefuseRateLimited, RetryAfterMs: wraps}})
	o := Options{URL: s.url(), InstallPins: []string{s.pin()}, Now: fixedNow}
	_, err := Dial(testsupport.Context(t), o, Credentials{Key: testKey(t), Token: tokenFunc(testToken)})
	var re *RefusedError
	if !errors.As(err, &re) {
		t.Fatalf("Dial error = %v, want a *RefusedError", err)
	}
	if re.RetryAfter != MaxRetryAfter {
		t.Errorf("RetryAfter = %v, want the clamp %v", re.RetryAfter, MaxRetryAfter)
	}
	s.next(t)
}

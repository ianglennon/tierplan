// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package tunnel_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/ianglennon/bayplan/internal/tunnel"
	"github.com/ianglennon/bayplan/proto/authn"
	"github.com/ianglennon/bayplan/proto/wire"
)

// fakeVerifier stands in for the agents service.
type fakeVerifier struct {
	mu      sync.Mutex
	err     error
	agentID string
	last    tunnel.EnrolRequest
	calls   int
	// renewErr is what Renew returns when set, and renews the agents it was asked for.
	renewErr error
	renews   []string
}

func (f *fakeVerifier) Enrol(_ context.Context, req tunnel.EnrolRequest) (tunnel.EnrolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = req
	if f.err != nil {
		return tunnel.EnrolResult{}, f.err
	}
	id := f.agentID
	if id == "" {
		id = "agent-1"
	}
	return tunnel.EnrolResult{AgentID: id, EnvID: "env-1", Cert: []byte("cert-der")}, nil
}

func (f *fakeVerifier) Hello(context.Context, tunnel.HelloRequest) (tunnel.HelloResult, error) {
	return tunnel.HelloResult{}, &tunnel.Refusal{Code: wire.ReasonAuthRejected, Detail: "the fake verifier does not say hello"}
}

// Renew answers a renewal with a fixed certificate, or with the verifier's error.
func (f *fakeVerifier) Renew(_ context.Context, agentID, _ string) (tunnel.RenewResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renews = append(f.renews, agentID)
	if f.renewErr != nil {
		return tunnel.RenewResult{}, f.renewErr
	}
	return tunnel.RenewResult{Cert: []byte("renewed-cert-der"), NotAfter: time.Unix(1_900_000_000, 0).UTC()}, nil
}

// StillActive says every agent is still active: the fake never revokes.
func (f *fakeVerifier) StillActive(string) bool { return true }

func (f *fakeVerifier) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// observer records the session lifecycle in order.
type observer struct {
	mu     sync.Mutex
	events []string
	conn   chan *tunnel.Session
}

func newObserver() *observer { return &observer{conn: make(chan *tunnel.Session, 4)} }

func (o *observer) Connected(s *tunnel.Session) {
	o.mu.Lock()
	o.events = append(o.events, "connected:"+s.AgentID)
	o.mu.Unlock()
	o.conn <- s
}

func (o *observer) Disconnected(s *tunnel.Session, code string) {
	o.mu.Lock()
	o.events = append(o.events, "disconnected:"+s.AgentID+":"+code)
	o.mu.Unlock()
}

func (o *observer) log() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.events...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type fixture struct {
	hub *tunnel.Hub
	v   *fakeVerifier
	o   *observer
	ts  *httptest.Server
}

// newFixture serves a hub. tlsOn picks HTTPS or plain HTTP, and publicURL is what
// the hub believes Bayplan's public URL is: a function of the server's address.
func newFixture(t *testing.T, tlsOn bool, publicURL func(addr string) string) *fixture {
	t.Helper()
	return newConfiguredFixture(t, tlsOn, publicURL, slog.New(slog.DiscardHandler), nil)
}

// newConfiguredFixture is newFixture with the hub's logger chosen by the test and
// a configure function applied to the hub before it serves, for the options that
// must be set before the hub serves (WithAllowInsecure).
func newConfiguredFixture(t *testing.T, tlsOn bool, publicURL func(addr string) string, log *slog.Logger, configure func(*tunnel.Hub) *tunnel.Hub) *fixture {
	t.Helper()
	f := &fixture{v: &fakeVerifier{}, o: newObserver()}
	f.ts = httptest.NewUnstartedServer(nil)
	addr := f.ts.Listener.Addr().String()
	f.hub = tunnel.NewHub(f.v, f.o, log, nil).WithPublicURL(func() string { return publicURL(addr) })
	if configure != nil {
		f.hub = configure(f.hub)
	}
	f.ts.Config.Handler = f.hub.Handler()
	if tlsOn {
		f.ts.StartTLS()
	} else {
		f.ts.Start()
	}
	t.Cleanup(func() {
		f.hub.Close()
		f.ts.CloseClientConnections()
		f.ts.Close()
	})
	return f
}

func (f *fixture) wsURL() string {
	return "ws" + strings.TrimPrefix(f.ts.URL, "http") + "/agent/connect"
}

// hs describes the agent side of a handshake, with a knob for each way a
// handshake can go wrong.
type hs struct {
	aud       string // empty: the address the agent dialed
	proto     int    // zero: wire.ProtoVersion
	mode      string // empty: enrol
	ekm       []byte // nil: the TLS exporter of the dial response
	noEKM     bool
	binary    bool
	subproto  string // empty: wire.Subprotocol
	pubLen    int
	hostBytes int
	// noMux leaves the WebSocket unwrapped after the welcome, so a test can read
	// the server's close frame itself: a yamux session would own the reader.
	noMux bool
}

type agentConn struct {
	ws      *websocket.Conn
	resp    *http.Response
	welcome *wire.Welcome
	refuse  *wire.Refuse
	mux     *yamux.Session
	// err is the error that ended the exchange when the server closed the
	// connection instead of answering.
	err error
}

// dial opens the WebSocket and runs the agent side of the handshake. It returns
// after the server's welcome or refusal.
func (f *fixture) dial(t *testing.T, h hs) *agentConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	sub := h.subproto
	if sub == "" {
		sub = wire.Subprotocol
	}
	ws, resp, err := websocket.Dial(ctx, f.wsURL(), &websocket.DialOptions{ //nolint:bodyclose // a WebSocket upgrade response has no body to close
		HTTPClient: f.ts.Client(), Subprotocols: []string{sub},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	ac := &agentConn{ws: ws, resp: resp}

	_, data, err := ws.Read(ctx)
	if err != nil {
		if h.subproto != "" {
			ac.err = err // the server closed a connection that did not speak the subprotocol
			return ac
		}
		t.Fatalf("read the challenge: %v", err)
	}
	var ch wire.Challenge
	if err := json.Unmarshal(data, &ch); err != nil || ch.T != wire.TypeChallenge || len(ch.Nonce) != 32 {
		t.Fatalf("challenge = %s, %v", data, err)
	}

	aud := h.aud
	if aud == "" {
		aud = f.ts.Listener.Addr().String()
	}
	pubLen := h.pubLen
	if pubLen == 0 {
		pubLen = ed25519.PublicKeySize
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubBytes := []byte(pub)
	if pubLen != ed25519.PublicKeySize {
		pubBytes = pubBytes[:pubLen]
	}
	nonce := make([]byte, 32)
	_, _ = rand.Read(nonce)
	ekm := h.ekm
	if ekm == nil && !h.noEKM {
		ekm = authn.EKM(resp.TLS)
	}
	proto := h.proto
	if proto == 0 {
		proto = wire.ProtoVersion
	}
	mode := h.mode
	if mode == "" {
		mode = wire.ModeEnrol
	}
	tr := authn.Transcript(mode, proto, ch.Nonce, nonce, aud, pubBytes, ekm)
	auth := wire.Auth{
		T: wire.TypeAuth, Mode: mode, Proto: proto, AgentVersion: "0.2.0", Aud: aud, Nonce: nonce, TS: time.Now().UnixMilli(),
		Pub: pubBytes, Token: "bpjt_test", EKM: ekm, Sig: authn.Sign(priv, tr),
		Facts: wire.Facts{Hostname: strings.Repeat("h", h.hostBytes), EngineReachable: true},
	}
	b, _ := json.Marshal(auth)
	typ := websocket.MessageText
	if h.binary {
		typ = websocket.MessageBinary
	}
	if err := ws.Write(ctx, typ, b); err != nil {
		t.Fatalf("send auth: %v", err)
	}

	_, data, err = ws.Read(ctx)
	if err != nil {
		return ac // closed without an answer
	}
	var head struct{ T string }
	_ = json.Unmarshal(data, &head)
	switch head.T {
	case wire.TypeWelcome:
		var w wire.Welcome
		_ = json.Unmarshal(data, &w)
		ac.welcome = &w
		if h.noMux {
			break
		}
		conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
		muxCfg := yamux.DefaultConfig()
		muxCfg.LogOutput = io.Discard
		mux, err := yamux.Server(conn, muxCfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = mux.Close() })
		ac.mux = mux
	case wire.TypeRefuse:
		var r wire.Refuse
		_ = json.Unmarshal(data, &r)
		ac.refuse = &r
	default:
		t.Fatalf("unexpected answer %s", data)
	}
	return ac
}

func sameAddr(scheme string) func(string) string {
	return func(addr string) string { return scheme + "://" + addr }
}

func TestAHandshakeWelcomesTheAgentAndTheServerOpensStreamsToIt(t *testing.T) {
	f := newFixture(t, true, sameAddr("https"))
	ac := f.dial(t, hs{hostBytes: 5})
	if ac.welcome == nil || ac.welcome.AgentID != "agent-1" || ac.welcome.EnvID != "env-1" || string(ac.welcome.Cert) != "cert-der" {
		t.Fatalf("welcome = %+v refuse = %+v", ac.welcome, ac.refuse)
	}
	if ac.welcome.HeartbeatMs != wire.DefaultHeartbeatMs {
		t.Errorf("heartbeat %d", ac.welcome.HeartbeatMs)
	}
	if ac.resp.Header.Get("Sec-WebSocket-Protocol") != wire.Subprotocol {
		t.Errorf("the server did not select the subprotocol: %v", ac.resp.Header)
	}

	// What the verifier saw: the transcript the handler built, never one the agent chose.
	if got := f.v.last; got.Token != "bpjt_test" || len(got.Transcript) != 32 || got.AgentVersion != "0.2.0" || got.Facts.Hostname != "hhhhh" {
		t.Errorf("verifier saw %+v", got)
	}
	if !authn.Verify(f.v.last.Pub, f.v.last.Transcript, f.v.last.Sig) {
		t.Error("the proof of possession does not verify over the transcript the handler built")
	}

	sess := <-f.o.conn
	if sess.AgentID != "agent-1" || sess.EnvID != "env-1" || f.hub.Session("agent-1") != sess {
		t.Fatalf("session = %+v", sess)
	}
	if got := sess.Transport().Describe(); got != "tunnel://agent-1" {
		t.Errorf("Describe = %q", got)
	}

	// The server opens a stream; the first byte on it is the Docker HTTP kind.
	served := make(chan byte, 1)
	go func() {
		st, err := ac.mux.Accept()
		if err != nil {
			return
		}
		defer func() { _ = st.Close() }()
		var kind [1]byte
		_, _ = io.ReadFull(st, kind[:])
		served <- kind[0]
		_, _ = st.Write([]byte("pong"))
	}()
	conn, err := sess.Transport().DialContext(context.Background(), "tcp", "ignored:1")
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	defer func() { _ = conn.Close() }()
	select {
	case kind := <-served:
		if kind != wire.StreamKindDockerHTTP {
			t.Fatalf("the first byte of the stream is %#x, want %#x", kind, wire.StreamKindDockerHTTP)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the agent never saw the stream")
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "pong" {
		t.Fatalf("read the agent's answer: %q, %v", buf, err)
	}

	// The agent goes away: the observer hears of it with a D-40 code, and a dial
	// now fails at once with ErrAgentGone instead of hanging (D-42).
	_ = ac.ws.CloseNow()
	waitFor(t, "the disconnect", func() bool { return len(f.o.log()) == 2 })
	if got := f.o.log(); got[0] != "connected:agent-1" || got[1] != "disconnected:agent-1:"+tunnel.DisconnectClosed {
		t.Fatalf("lifecycle = %v", got)
	}
	if f.hub.Session("agent-1") != nil {
		t.Error("the session is still registered after it ended")
	}
	start := time.Now()
	if _, err := sess.Transport().DialContext(context.Background(), "tcp", "x"); !errors.Is(err, tunnel.ErrAgentGone) {
		t.Fatalf("DialContext on a dead session = %v, want ErrAgentGone", err)
	}
	if time.Since(start) > time.Second {
		t.Error("a dial on a dead session did not fail at once")
	}
}

func TestADialAlreadyCancelledNeverOpensAStream(t *testing.T) {
	f := newFixture(t, true, sameAddr("https"))
	f.dial(t, hs{})
	sess := <-f.o.conn
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sess.Transport().DialContext(ctx, "tcp", "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, err := (tunnel.StreamTransport{}).DialContext(context.Background(), "tcp", "x"); !errors.Is(err, tunnel.ErrAgentGone) {
		t.Fatalf("a transport with no session = %v, want ErrAgentGone", err)
	}
}

func TestTheHandshakeRefusesWhatItShould(t *testing.T) {
	for _, tc := range []struct {
		name     string
		h        hs
		verifier error
		wantCode string
		wantText string
	}{
		{"a different audience", hs{aud: "evil.example"}, nil, wire.ReasonAuthRejected, "different host"},
		{"another protocol version", hs{proto: 2}, nil, wire.ReasonVersionIncompatible, "protocol"},
		{"hello mode without a certificate", hs{mode: wire.ModeHello}, nil, wire.ReasonAuthRejected, "malformed"},
		{"a short key", hs{pubLen: 10}, nil, wire.ReasonAuthRejected, "malformed"},
		{"a binary frame", hs{binary: true}, nil, wire.ReasonAuthRejected, "text frame"},
		{"a binding that does not match this TLS connection", hs{ekm: make([]byte, 32)}, nil, wire.ReasonAuthRejected, "BAYPLAN_TRUSTED_PROXIES"},
		{"no binding at all over TLS", hs{noEKM: true}, nil, wire.ReasonAuthRejected, "BAYPLAN_TRUSTED_PROXIES"},
		{"a refusal from the verifier", hs{}, &tunnel.RefuseError{Code: wire.RefuseDuplicateEnvironment, Detail: "already in Production"}, wire.RefuseDuplicateEnvironment, "already in Production"},
		{"a server fault in the verifier", hs{}, errors.New("database is locked"), wire.RefuseInternal, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, true, sameAddr("https"))
			f.v.err = tc.verifier
			ac := f.dial(t, tc.h)
			if ac.refuse == nil {
				t.Fatalf("not refused: welcome %+v", ac.welcome)
			}
			if ac.refuse.Code != tc.wantCode || !strings.Contains(ac.refuse.Detail, tc.wantText) {
				t.Errorf("refuse = %+v, want code %s and detail containing %q", ac.refuse, tc.wantCode, tc.wantText)
			}
			if strings.Contains(ac.refuse.Detail, "database is locked") {
				t.Error("a server fault leaked into the refusal sent to the agent")
			}
			// The connection is closed with policy-violation status and the code.
			_, _, err := ac.ws.Read(context.Background())
			if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Errorf("close status %v, want policy violation (%v)", websocket.CloseStatus(err), err)
			}
			if len(f.o.log()) != 0 {
				t.Errorf("the observer heard of a refused agent: %v", f.o.log())
			}
		})
	}
}

func TestTheVerifierIsNotAskedWhenTheHandshakeIsMalformed(t *testing.T) {
	f := newFixture(t, true, sameAddr("https"))
	f.dial(t, hs{aud: "evil.example"})
	f.dial(t, hs{proto: 9})
	if n := f.v.callCount(); n != 0 {
		t.Fatalf("the verifier was asked %d times about handshakes that failed the cheap checks", n)
	}
}

func TestARequestThatIsNotAnAgentIsTurnedAwayBeforeTheUpgrade(t *testing.T) {
	f := newFixture(t, true, sameAddr("https"))
	get := func(headers map[string]string) int {
		req, _ := http.NewRequest(http.MethodGet, f.ts.URL+"/agent/connect", nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := f.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	upgrade := map[string]string{
		"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13",
		"Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ==", "Sec-WebSocket-Protocol": wire.Subprotocol,
	}
	if got := get(nil); got != http.StatusUpgradeRequired {
		t.Errorf("a plain GET answered %d, want 426", got)
	}
	withOrigin := map[string]string{"Origin": "https://evil.example"}
	for k, v := range upgrade {
		withOrigin[k] = v
	}
	if got := get(withOrigin); got != http.StatusForbidden {
		t.Errorf("a request with an Origin answered %d, want 403", got)
	}
	if f.v.callCount() != 0 {
		t.Error("the verifier was asked about a browser request")
	}
}

func TestPlainTransportIsRefusedWhenThePublicURLIsHTTPS(t *testing.T) {
	f := newFixture(t, false, sameAddr("https"))
	_, resp, err := websocket.Dial(context.Background(), f.wsURL(), &websocket.DialOptions{Subprotocols: []string{wire.Subprotocol}}) //nolint:bodyclose // coder/websocket reads and closes the body of a refused upgrade itself
	if err == nil {
		t.Fatal("a plain ws:// connection was accepted although the public URL is https")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("response = %v, want 403", resp)
	}
}

// The httptest address is 127.0.0.1, so an http base on it is plain http on this
// machine and needs no switch.
func TestPlainTransportIsAcceptedWhenTheAgentAddressIsPlainHTTPOnThisMachine(t *testing.T) {
	f := newFixture(t, false, sameAddr("http"))
	ac := f.dial(t, hs{noEKM: true})
	if ac.welcome == nil {
		t.Fatalf("refused: %+v", ac.refuse)
	}
}

// A plain http agent address for another host does not make plain transport
// acceptable; only the development switch does (G-02-R4-3, T-02-139).
func TestPlainTransportIsRefusedWhenTheAgentAddressIsPlainHTTPForAnotherHost(t *testing.T) {
	log, rec := newRecordingLogger()
	f := newConfiguredFixture(t, false, func(string) string { return "http://10.0.0.5:7480" }, log, nil)
	_, resp, err := websocket.Dial(context.Background(), f.wsURL(), &websocket.DialOptions{Subprotocols: []string{wire.Subprotocol}}) //nolint:bodyclose // coder/websocket reads and closes the body of a refused upgrade itself
	if err == nil {
		t.Fatal("a plain ws:// connection was accepted for an agent address on another host")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("response = %v, want 403", resp)
	}
	if f.v.callCount() != 0 {
		t.Error("the verifier was asked about a refused connection")
	}
	if got := rec.containing("BAYPLAN_AGENT_ALLOW_INSECURE"); len(got) != 1 || got[0].level != slog.LevelWarn {
		t.Errorf("records naming the switch = %+v, want exactly one Warn", got)
	}
}

func TestPlainTransportToAnotherHostIsAcceptedWithTheSwitch(t *testing.T) {
	f := newConfiguredFixture(t, false, func(string) string { return "http://10.0.0.5:7480" }, slog.New(slog.DiscardHandler),
		func(h *tunnel.Hub) *tunnel.Hub { return h.WithAllowInsecure(true) })
	ac := f.dial(t, hs{aud: "10.0.0.5:7480", noEKM: true})
	if ac.welcome == nil {
		t.Fatalf("refused: %+v", ac.refuse)
	}
}

func TestPlainAddressRefusedOnlyForPlainAddressesToAnotherHostWithoutTheSwitch(t *testing.T) {
	for _, c := range []struct {
		raw   string
		allow bool
		want  bool
	}{
		{"http://10.0.0.5:7480", false, true},
		{"http://10.0.0.5:7480", true, false},
		{"ws://10.0.0.5:7480/agent/connect", false, true},
		{"HTTP://Bayplan.Example", false, true},
		{"http://localhost.example", false, true},
		{"http://127.0.0.1:7480", false, false},
		{"ws://[::1]:7480/agent/connect", false, false},
		{"http://localhost:5173", false, false},
		{"https://bayplan.example", false, false},
		{"wss://bayplan.example/agent/connect", false, false},
		{"", false, false},
		{"http://[::1", false, false}, // url.Parse refuses it
	} {
		if got := tunnel.PlainAddressRefused(c.raw, c.allow); got != c.want {
			t.Errorf("PlainAddressRefused(%q, %v) = %v, want %v", c.raw, c.allow, got, c.want)
		}
	}
}

func TestWithNoPublicURLNoAgentIsAccepted(t *testing.T) {
	f := newFixture(t, true, func(string) string { return "" })
	_, resp, err := websocket.Dial(context.Background(), f.wsURL(), &websocket.DialOptions{HTTPClient: f.ts.Client(), Subprotocols: []string{wire.Subprotocol}}) //nolint:bodyclose // coder/websocket reads and closes the body of a refused upgrade itself
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, resp = %v; want 503", err, resp)
	}
}

func TestAConnectionWithoutTheSubprotocolIsClosed(t *testing.T) {
	f := newFixture(t, true, sameAddr("https"))
	ac := f.dial(t, hs{subproto: "something.else"})
	if websocket.CloseStatus(ac.err) != websocket.StatusPolicyViolation {
		t.Fatalf("close status %v, want policy violation (%v)", websocket.CloseStatus(ac.err), ac.err)
	}
	if f.v.callCount() != 0 {
		t.Error("the verifier was asked about a connection that did not speak the subprotocol")
	}
}

func TestAnOversizedHandshakeMessageClosesTheConnection(t *testing.T) {
	f := newFixture(t, true, sameAddr("https"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, f.wsURL(), &websocket.DialOptions{HTTPClient: f.ts.Client(), Subprotocols: []string{wire.Subprotocol}}) //nolint:bodyclose // coder/websocket reads and closes the body of a refused upgrade itself
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	if _, _, err := ws.Read(ctx); err != nil {
		t.Fatal(err)
	}
	ws.SetReadLimit(-1)
	big := make([]byte, wire.MaxHandshakeBytes*4)
	for i := range big {
		big[i] = 'a'
	}
	_ = ws.Write(ctx, websocket.MessageText, big)
	_, _, err = ws.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("close status %v, want message too big (%v)", websocket.CloseStatus(err), err)
	}
	if f.v.callCount() != 0 {
		t.Error("the verifier was asked about an oversized message")
	}
}

func TestANewSessionOfTheSameAgentReplacesTheOldOne(t *testing.T) {
	f := newFixture(t, true, sameAddr("https"))
	first := f.dial(t, hs{})
	oldSess := <-f.o.conn
	second := f.dial(t, hs{})
	newSess := <-f.o.conn
	if oldSess == newSess || f.hub.Session("agent-1") != newSess {
		t.Fatal("the new session did not replace the old one")
	}
	waitFor(t, "the old session to end", func() bool {
		select {
		case <-oldSess.Done():
			return true
		default:
			return false
		}
	})
	waitFor(t, "the old session's disconnect", func() bool { return len(f.o.log()) == 3 })
	// The old session is reported gone, with the reason replaced, before the new
	// one is reported connected, and the hub still has the new one.
	if got := f.o.log(); got[0] != "connected:agent-1" || got[1] != "disconnected:agent-1:"+tunnel.DisconnectReplaced || got[2] != "connected:agent-1" {
		t.Fatalf("lifecycle = %v", got)
	}
	if f.hub.Session("agent-1") != newSess {
		t.Fatal("the old session's end removed the new one")
	}
	select {
	case <-newSess.Done():
		t.Fatal("the new session ended with the old one")
	default:
	}
	_ = first
	_ = second
}

func TestClosingTheHubEndsEverySessionAndRefusesNewOnes(t *testing.T) {
	f := newFixture(t, true, sameAddr("https"))
	ac := f.dial(t, hs{noMux: true})
	sess := <-f.o.conn

	done := make(chan struct{})
	go func() { f.hub.Close(); close(done) }()

	// The agent is told with a close frame (going away), so it reconnects with backoff.
	_, _, err := ac.ws.Read(context.Background())
	if websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("close status %v, want going away (%v)", websocket.CloseStatus(err), err)
	}
	_ = ac.ws.CloseNow()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Hub.Close did not return")
	}
	select {
	case <-sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not end")
	}
	if _, err := sess.Transport().DialContext(context.Background(), "tcp", "x"); !errors.Is(err, tunnel.ErrAgentGone) {
		t.Fatalf("dial after Close = %v, want ErrAgentGone", err)
	}
	_, resp, err := websocket.Dial(context.Background(), f.wsURL(), &websocket.DialOptions{HTTPClient: f.ts.Client(), Subprotocols: []string{wire.Subprotocol}}) //nolint:bodyclose // coder/websocket reads and closes the body of a refused upgrade itself
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a connection after Close: err %v resp %v, want 503", err, resp)
	}
	f.hub.Close() // safe to call again
}

func TestSessionsAreListedByAgentID(t *testing.T) {
	f := newFixture(t, true, sameAddr("https"))
	f.v.agentID = "b"
	f.dial(t, hs{})
	<-f.o.conn
	f.v.agentID = "a"
	f.dial(t, hs{})
	<-f.o.conn
	got := f.hub.Sessions()
	if len(got) != 2 || got[0].AgentID != "a" || got[1].AgentID != "b" {
		t.Fatalf("Sessions = %v", got)
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressrender

import (
	"bufio"
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ianglennon/tierplan/internal/ingressrender/traefiktest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Spike S9, short form (Phase 28, SELF-02, SELF-05, D-10, D-14, D-15, A-03): TierPlan's own route goes
// through a servers transport that verifies the backend (serverName and rootCAs naming TierPlan's
// self-signed certificate, which has no CA bit), over HTTP/2, and the event feed and the shell
// WebSocket must stay up through it. The backend here is the stub TierPlan of traefiktest, started with
// the real keepalive periods shortened. The 20-minute idle hold through the real virtual IP is 28-15.
const (
	// s9Front is the name TierPlan's route answers to.
	s9Front = "app.example.org"
	// s9SelfName is the name on the backend's certificate, which the transport's serverName must carry.
	s9SelfName = "tierplan.example.org"
	// s9RouterRule is the rule of the self route: the whole host except the two connect paths, which
	// the proxy must never carry (D-10).
	s9RouterRule = "Host(`" + s9Front + "`) && !PathPrefix(`/agent/connect`) && !PathPrefix(`/ingress/connect`)"
	// s9SelfCert is where the stub's certificate is written under certs/, and the path rootCAs names.
	s9SelfCert = traefiktest.NodeRoot + "/certs/self.crt"
)

// s9Doc is a dynamic file with one router, s9, on websecure with TLS, to the service s9-self, which is
// the stub over the transport t-self with the given serverName. rootCAs is omitted when withRoots is
// false.
func s9Doc(serverName string, withRoots bool) string {
	transport := "    t-self:\n      serverName: " + serverName + "\n"
	if withRoots {
		transport += "      rootCAs:\n        - " + s9SelfCert + "\n"
	}
	return s1ReloadOptions +
		"  certificates:\n    - certFile: " + traefiktest.NodeRoot + "/certs/front.crt\n      keyFile: " + traefiktest.NodeRoot + "/certs/front.key\n" +
		"http:\n  routers:\n" +
		s1Router("s9", s9RouterRule, "s9-self") +
		"  services:\n    s9-self:\n      loadBalancer:\n        serversTransport: t-self\n        servers:\n          - url: https://" +
		traefiktest.StubAlias + ":" + traefiktest.StubPort + "\n" +
		"  serversTransports:\n" + transport
}

// s9Site is one Traefik in front of a stub TierPlan.
type s9Site struct {
	*s1Site
	stub *traefiktest.Stub
	// selfCert is the stub's certificate, which is also its own root.
	selfCert []byte
}

// startS9 starts the stub with the given keepalives, a Traefik with the s9 route and the CA that
// signed the front certificate. serverName and withRoots shape the transport.
func startS9(t *testing.T, stub traefiktest.StubOptions, serverName string, withRoots bool) *s9Site {
	t.Helper()
	env := traefiktest.NewEnv(t)
	ca := traefiktest.NewCA(t)
	frontChain, frontKey := ca.Leaf(t, s9Front)
	selfCert, selfKey := traefiktest.SelfSignedLeaf(t, s9SelfName)
	stub.CertPEM, stub.KeyPEM = selfCert, selfKey
	st := traefiktest.StartSelfStub(t, env, stub)
	env.Apply(t, append(s1CertFiles("front", frontChain, frontKey),
		traefiktest.FileSpec{Path: "certs/self.crt", Data: selfCert},
		s1Dynamic(s9Doc(serverName, withRoots))))
	site := &s1Site{env: env, ca: ca, tr: env.StartTraefik(t, traefiktest.TraefikOptions{Static: Static()})}
	return &s9Site{s1Site: site, stub: st, selfCert: selfCert}
}

// TestS9VerifiedBackendOverTSelf is the tracer: a request for TierPlan's host reaches the stub through
// Traefik over a transport that verifies it, and the stub says the request arrived on HTTP/2, so h2 was
// negotiated to the backend.
func TestS9VerifiedBackendOverTSelf(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	s := startS9(t, traefiktest.StubOptions{}, s9SelfName, true)
	s.awaitStatus(t, s9Front, "/", http.StatusOK)

	status, body, _ := s.tr.Get(t, s.ca.Pool(), s9Front, "/proto", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /proto answered %d, want 200", status)
	}
	if got := strings.TrimSpace(body); got != "HTTP/2.0" {
		t.Errorf("the stub saw the protocol %q, want HTTP/2.0: HTTP/2 was not negotiated to the backend", got)
	}
}

// TestS9EventFeedIsNotBuffered proves the event feed streams through the route with no buffering
// middleware configured (D-14): the stub writes a comment line every second and each reaches the client
// about a second after the one before it. Lines held back by a buffer would arrive in a bunch after one
// long wait, so the gap before the first line is bounded too, from the moment the request is sent.
func TestS9EventFeedIsNotBuffered(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	const lines = 5
	const bound = 2 * time.Second
	s := startS9(t, traefiktest.StubOptions{SSEEvery: time.Second}, s9SelfName, true)
	s.awaitStatus(t, s9Front, "/", http.StatusOK)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+s9Front+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	prev := time.Now() // wall-clock: the arrival times of the lines are the measurement; no wait depends on them
	resp, err := s.tr.StreamClient(s.ca.Pool(), s9Front).Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("GET /events answered %d with Content-Type %q, want 200 text/event-stream", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	br := bufio.NewReader(resp.Body)
	for i := range lines {
		var line string
		for line == "" {
			l, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("read line %d of the feed: %v", i+1, err)
			}
			line = strings.TrimSpace(l)
		}
		now := time.Now() // wall-clock: see above
		gap := now.Sub(prev)
		prev = now
		t.Logf("line %d %q arrived %s after the one before", i+1, line, gap.Round(time.Millisecond))
		if line != ": ping" {
			t.Errorf("line %d is %q, want the comment line %q", i+1, line, ": ping")
		}
		if gap > bound {
			t.Errorf("line %d arrived %s after the one before, want within %s: the feed is being buffered", i+1, gap, bound)
		}
	}
}

// TestS9ShellWebSocketThroughTraefik proves a WebSocket dialled through the route completes, echoes, and
// stays open while the stub pings it (D-14). The proxy must answer the stub's pings with the client's
// pongs, so the connection is read in the background the whole time and the stub's own log is the
// count of pongs it got back.
func TestS9ShellWebSocketThroughTraefik(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	const pings = 3
	s := startS9(t, traefiktest.StubOptions{PingEvery: time.Second}, s9SelfName, true)
	s.awaitStatus(t, s9Front, "/", http.StatusOK)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "wss://"+s9Front+"/ws", &websocket.DialOptions{HTTPClient: s.tr.StreamClient(s.ca.Pool(), s9Front)})
	if err != nil {
		t.Fatalf("dial wss://%s/ws: %v", s9Front, err)
	}
	defer func() { _ = conn.CloseNow() }()

	// One reader answers the stub's pings and hands the echoes to the test.
	msgs := make(chan string, 8)
	readErr := make(chan error, 1)
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				readErr <- err
				return
			}
			msgs <- string(data)
		}
	}()
	echo := func(text string) {
		t.Helper()
		if err := conn.Write(ctx, websocket.MessageText, []byte(text)); err != nil {
			t.Fatalf("write %q: %v", text, err)
		}
		if got := testsupport.Await(t, "the echo of "+text, msgs); got != text {
			t.Fatalf("the stub echoed %q, want %q", got, text)
		}
	}
	echo("hello")

	testsupport.Poll(t, testsupport.Deadline, "the stub to log three pongs", func() (bool, string) { // testsupport: external, the stub pings on its own schedule
		n := strings.Count(s.stub.Logs(t), "ws pong ")
		return n >= pings, "pongs logged: " + strconv.Itoa(n)
	})
	select {
	case err := <-readErr:
		t.Fatalf("the connection closed while the stub was pinging it: %v", err)
	default:
	}
	echo("again")
}

// TestS9ConnectPathsAre404 proves the two connect paths are not carried by the route (SELF-05, D-10): a
// request for either answers 404 from the proxy although the stub answers 200 on both when reached
// directly, and the rest of the host is served.
func TestS9ConnectPathsAre404(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	s := startS9(t, traefiktest.StubOptions{}, s9SelfName, true)
	// Before the route loads every path is 404, so the 404s below mean something only after a 200.
	s.awaitStatus(t, s9Front, "/", http.StatusOK)

	for _, p := range []string{"/agent/connect", "/ingress/connect", "/agent/connect/x", "/ingress/connect/x"} {
		status, err := s.statusOf(s9Front, p)
		if err != nil || status != http.StatusNotFound {
			t.Errorf("GET %s through the route answered %d (error %v), want 404", p, status, err)
		}
	}
	for _, p := range []string{"/agent/connect", "/ingress/connect"} {
		status, body := s.stub.Get(t, s.selfCert, p)
		if status != http.StatusOK || body != "reached" {
			t.Errorf("GET %s straight to the stub answered %d %q, want 200 reached: the 404 above would not prove the path is excluded", p, status, body)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+s9Front+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := s.tr.StreamClient(s.ca.Pool(), s9Front).Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /events answered %d, want 200", resp.StatusCode)
	}
}

// s9Refused starts a route whose transport must fail the verification and checks that the proxy answers
// a server error and says why in its log. The status is not pinned to one code (A-03): that Traefik
// answers 500 here and not 502 is an observation of this release, not a contract.
func s9Refused(t *testing.T, serverName string, withRoots bool) {
	t.Helper()
	s := startS9(t, traefiktest.StubOptions{}, serverName, withRoots)
	// Before the route loads the answer is a 404 or a handshake failure, so wait for a server error.
	var status int
	testsupport.Poll(t, testsupport.Deadline, "the route to answer a server error", func() (bool, string) { // testsupport: external, Traefik reloads on its own schedule
		st, err := s.statusOf(s9Front, "/")
		if err != nil {
			return false, err.Error()
		}
		status = st
		return st >= http.StatusInternalServerError, "status " + strconv.Itoa(st)
	})
	if status >= 200 && status < 300 {
		t.Fatalf("the route answered %d: the backend was reached without a verified certificate", status)
	}
	testsupport.Poll(t, testsupport.Deadline, "Traefik's log to hold an x509 line", func() (bool, string) { // testsupport: external, a real container's log
		return strings.Contains(s.tr.Logs(t), "x509"), "no x509 line yet"
	})
	t.Logf("a transport with serverName %q and roots %v answered %d", serverName, withRoots, status)
}

// TestS9WrongServerNameIsRefused is the first negative control (A-03, T-28-03): the backend's
// certificate is trusted but is not for the name the transport names, so a request is a server error.
// It shows that the positive test cannot pass with verification off.
func TestS9WrongServerNameIsRefused(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	s9Refused(t, "wrong.example.org", true)
}

// TestS9NoRootCAsIsRefused is the second: the transport names the right name but no root, so the
// backend's self-signed certificate is not trusted.
func TestS9NoRootCAsIsRefused(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	s9Refused(t, s9SelfName, false)
}

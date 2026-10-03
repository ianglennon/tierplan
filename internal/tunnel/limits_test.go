// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package tunnel_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/tunnel"
	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/bayplan/proto/wire"
)

// The hardening of /agent/connect and of the session (plan 02-07, RESEARCH
// Pattern 1, Pattern 2, Pitfall 7, Pitfall 8, D-13).

// auditSink records the entries written to it.
type auditSink struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (a *auditSink) Record(_ context.Context, e audit.Entry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
	return nil
}

func (a *auditSink) all() []audit.Entry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]audit.Entry(nil), a.entries...)
}

// plainServer serves a hub over plain HTTP, optionally behind the client-address
// middleware with 127.0.0.0/8 trusted as a proxy, and returns it with its
// fake verifier.
func plainServer(t *testing.T, publicScheme string, trustLoopback bool, configure func(*tunnel.Hub) *tunnel.Hub) (*httptest.Server, *fakeVerifier) {
	t.Helper()
	v := &fakeVerifier{}
	ts := httptest.NewUnstartedServer(nil)
	addr := ts.Listener.Addr().String()
	hub := tunnel.NewHub(v, newObserver(), slog.New(slog.DiscardHandler), nil).
		WithPublicURL(func() string { return publicScheme + "://" + addr })
	if configure != nil {
		hub = configure(hub)
	}
	h := hub.Handler()
	if trustLoopback {
		h = httpx.ClientIPMiddleware([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})(h)
	} else {
		h = httpx.ClientIPMiddleware(nil)(h)
	}
	ts.Config.Handler = h
	ts.Start()
	t.Cleanup(func() {
		hub.Close()
		ts.CloseClientConnections()
		ts.Close()
	})
	return ts, v
}

// upgradeRequest sends an upgrade request and returns the status and, when the
// upgrade was not accepted, the start of the body.
func upgradeRequest(t *testing.T, url string, headers map[string]string) (status int, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url+"/agent/connect", nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13",
		"Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ==", "Sec-WebSocket-Protocol": wire.Subprotocol,
	} {
		req.Header.Set(k, v)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// An accepted upgrade has no body to read: the connection is the WebSocket.
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusSwitchingProtocols {
		return resp.StatusCode, ""
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	return resp.StatusCode, string(b)
}

func TestOriginHeaderIsRefusedBeforeTheUpgradeAndAudited(t *testing.T) {
	sink := &auditSink{}
	ts, v := plainServer(t, "http", false, func(h *tunnel.Hub) *tunnel.Hub { return h.WithRefusalAudit(sink) })
	if status, _ := upgradeRequest(t, ts.URL, map[string]string{"Origin": "https://evil.example"}); status != http.StatusForbidden {
		t.Fatalf("a request with an Origin answered %d, want 403", status)
	}
	if v.callCount() != 0 {
		t.Error("the verifier was asked about a browser request")
	}
	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("%d audit entries, want 1: %+v", len(got), got)
	}
	e := got[0]
	if e.Action != audit.ActionAgentEnrolRefused || e.Actor.Type != audit.ActorAnonymous || e.IP != "127.0.0.1" || e.Details["code"] != "origin" {
		t.Errorf("entry = %+v, want an anonymous agent.enrol_refused from 127.0.0.1 with code origin", e)
	}
}

func TestAnUpgradeFailureRunsTheHookWithThePeerAndAnswers426(t *testing.T) {
	var mu sync.Mutex
	var peers []string
	ts, _ := plainServer(t, "http", false, func(h *tunnel.Hub) *tunnel.Hub {
		return h.OnUpgradeFailure(func(peer string) {
			mu.Lock()
			peers = append(peers, peer)
			mu.Unlock()
		})
	})
	resp, err := http.Get(ts.URL + "/agent/connect")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("status %d, want 426", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(peers) != 1 || peers[0] != "127.0.0.1" {
		t.Fatalf("hook saw %v, want [127.0.0.1]", peers)
	}
}

func TestInsecureTransportIsRefusedUnlessTrustedProxyOrAllowedByTheOperator(t *testing.T) {
	cases := []struct {
		name      string
		trusted   bool
		headers   map[string]string
		configure func(*tunnel.Hub) *tunnel.Hub
		want      int
	}{
		{name: "an untrusted peer over plain HTTP", want: http.StatusForbidden},
		{name: "an untrusted peer claiming https in a header", headers: map[string]string{"X-Forwarded-Proto": "https"}, want: http.StatusForbidden},
		{name: "a trusted proxy that does not say https", trusted: true, want: http.StatusForbidden},
		{name: "a trusted proxy saying https", trusted: true, headers: map[string]string{"X-Forwarded-Proto": "https"}, want: http.StatusSwitchingProtocols},
		{name: "the operator allowed plain transport", configure: func(h *tunnel.Hub) *tunnel.Hub { return h.WithAllowInsecure(true) }, want: http.StatusSwitchingProtocols},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &auditSink{}
			ts, _ := plainServer(t, "https", tc.trusted, func(h *tunnel.Hub) *tunnel.Hub {
				h = h.WithRefusalAudit(sink)
				if tc.configure != nil {
					h = tc.configure(h)
				}
				return h
			})
			status, body := upgradeRequest(t, ts.URL, tc.headers)
			if status != tc.want {
				t.Fatalf("status %d, want %d (%s)", status, tc.want, body)
			}
			if tc.want == http.StatusForbidden {
				if !strings.Contains(body, wire.RefuseInsecureTransport) {
					t.Errorf("body %q does not say %s", body, wire.RefuseInsecureTransport)
				}
				if got := sink.all(); len(got) != 1 || got[0].Details["code"] != wire.RefuseInsecureTransport {
					t.Errorf("audit = %+v, want one entry with code insecure_transport", got)
				}
			}
		})
	}
}

// firstMessage dials the agent endpoint of ts and returns the first message the
// server sends: a challenge when the attempt is admitted, a refusal when it is
// not.
func firstMessage(t *testing.T, ctx context.Context, srv *tunneltest.Server) (*websocket.Conn, wire.Refuse, string) {
	t.Helper()
	ws, _, err := websocket.Dial(ctx, srv.URL(), &websocket.DialOptions{ //nolint:bodyclose // a WebSocket upgrade response has no body to close
		HTTPClient: srv.HTTP.Client(), Subprotocols: []string{wire.Subprotocol},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_, data, err := ws.Read(ctx)
	if err != nil {
		_ = ws.CloseNow()
		t.Fatalf("read the first message: %v", err)
	}
	var head struct{ T string }
	_ = json.Unmarshal(data, &head)
	var ref wire.Refuse
	if head.T == wire.TypeRefuse {
		_ = json.Unmarshal(data, &ref)
	}
	return ws, ref, head.T
}

func TestConcurrentHandshakeCapRefusesThe65thWithRateLimitedAndARetryAfter(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{Configure: func(h *tunnel.Hub) *tunnel.Hub { return h.WithConnectLimit(1000) }})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	held := make([]*websocket.Conn, 0, 64)
	for i := range 64 {
		ws, _, typ := firstMessage(t, ctx, srv)
		if typ != wire.TypeChallenge {
			t.Fatalf("handshake %d got %q, want a challenge", i+1, typ)
		}
		held = append(held, ws)
	}
	t.Cleanup(func() {
		for _, ws := range held {
			_ = ws.CloseNow()
		}
	})

	ws, ref, typ := firstMessage(t, ctx, srv)
	defer func() { _ = ws.CloseNow() }()
	if typ != wire.TypeRefuse || ref.Code != wire.RefuseRateLimited || ref.RetryAfterMs <= 0 {
		t.Fatalf("the 65th handshake got %q %+v, want a refusal rate_limited with retryAfterMs", typ, ref)
	}

	// Letting one go frees its slot.
	_ = held[0].CloseNow()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ws2, _, typ := firstMessage(t, ctx, srv)
		_ = ws2.CloseNow()
		if typ == wire.TypeChallenge {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the slot of a closed handshake was never freed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRateLimitAdmitsTheBurstFromOneAddressAndRefusesTheRest(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{}) // the default burst, 200, on a clock that does not move
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	var admitted, limited int
	for range 260 {
		ws, ref, typ := firstMessage(t, ctx, srv)
		_ = ws.CloseNow()
		switch typ {
		case wire.TypeChallenge:
			admitted++
		case wire.TypeRefuse:
			if ref.Code != wire.RefuseRateLimited || ref.RetryAfterMs <= 0 {
				t.Fatalf("refusal = %+v, want rate_limited with retryAfterMs", ref)
			}
			limited++
		default:
			t.Fatalf("unexpected first message %q", typ)
		}
	}
	if admitted != 200 || limited != 60 {
		t.Fatalf("admitted %d and limited %d of 260 attempts from one address, want 200 and 60", admitted, limited)
	}
}

func TestAuditThrottleKeepsFiveHundredRefusalsToTheAllowanceAndOneSummary(t *testing.T) {
	sink := &auditSink{}
	throttle := auth.NewAuditThrottle(sink, auth.NewIPLimiter(time.Hour, 20, time.Hour, nil), slog.New(slog.DiscardHandler), nil)
	ts, _ := plainServer(t, "https", false, func(h *tunnel.Hub) *tunnel.Hub {
		return h.WithConnectLimit(100000).WithRefusalAudit(throttle)
	})
	for range 500 {
		if status, _ := upgradeRequest(t, ts.URL, map[string]string{"Origin": "https://evil.example"}); status != http.StatusForbidden {
			t.Fatalf("status %d, want 403", status)
		}
	}
	if n := len(sink.all()); n != 20 {
		t.Fatalf("%d rows written for 500 refusals before the flush, want the allowance of 20", n)
	}
	if err := throttle.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := sink.all()
	if len(all) != 21 {
		t.Fatalf("%d rows after the flush, want 21", len(all))
	}
	last := all[20]
	if last.Action != audit.ActionAgentEnrolRefused || last.Details["suppressed"] != "480" {
		t.Errorf("summary = %+v, want agent.enrol_refused with suppressed=480", last)
	}
}

func TestStreamCapAllowsSixtyFourStreamsAndRefusesThe65thWithoutDroppingTheSession(t *testing.T) {
	srv := timed(t, nil)
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	srv.Events.WaitLen(t, 1)
	s := srv.Hub.Session(c.Identity.AgentID)
	holdOpenStreams(t, c)

	conns := make([]io.Closer, 0, 64)
	for i := range 64 {
		conn, err := s.Transport().DialContext(context.Background(), "tcp", "x")
		if err != nil {
			t.Fatalf("stream %d: %v", i+1, err)
		}
		conns = append(conns, conn)
	}
	_, err := s.Transport().DialContext(context.Background(), "tcp", "x")
	if !errors.Is(err, tunnel.ErrBusy) {
		t.Fatalf("the 65th stream = %v, want ErrBusy", err)
	}
	if errors.Is(err, tunnel.ErrAgentGone) {
		t.Error("ErrBusy must not look like ErrAgentGone: the caller would mark the agent offline")
	}
	if isDone(s) {
		t.Fatal("the session ended")
	}
	_ = conns[0].Close()
	conn, err := s.Transport().DialContext(context.Background(), "tcp", "x")
	if err != nil {
		t.Fatalf("a stream after one was closed = %v, want a free slot", err)
	}
	_ = conn.Close()
	for _, c := range conns[1:] {
		_ = c.Close()
	}
}

func TestTooLargeResponseFailsTypedWhileEventsAreExemptAndTheSessionStaysUsable(t *testing.T) {
	if tunnel.MaxResponseBytes != 64<<20 {
		t.Fatalf("MaxResponseBytes = %d, want 64 MiB", tunnel.MaxResponseBytes)
	}
	const limit = 1 << 20
	srv := timed(t, func(h *tunnel.Hub) *tunnel.Hub { return h.WithResponseLimit(limit) })
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	srv.Events.WaitLen(t, 1)
	s := srv.Hub.Session(c.Identity.AgentID)

	big := strings.Repeat("x", 2*limit)
	go func() {
		for {
			st, err := c.Mux.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = st.Close() }()
				br := bufio.NewReader(st)
				if k, err := br.ReadByte(); err != nil || k != wire.StreamKindDockerHTTP {
					return
				}
				for {
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}
					_ = req.Body.Close()
					body := "ok"
					if req.URL.Path == "/containers/json" || strings.HasSuffix(req.URL.Path, "/events") {
						body = big
					}
					if _, err := io.WriteString(st, "HTTP/1.1 200 OK\r\nContent-Length: "+itoa(len(body))+"\r\n\r\n"+body); err != nil {
						return
					}
				}
			}()
		}
	}()

	hc := httpOver(s)
	resp, err := hc.Get("http://docker/containers/json")
	if err != nil {
		t.Fatalf("the headers of the large answer: %v", err)
	}
	_, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !errors.Is(err, tunnel.ErrTooLarge) {
		t.Fatalf("reading a body of %d bytes with a limit of %d = %v, want ErrTooLarge", len(big), limit, err)
	}
	if errors.Is(err, tunnel.ErrAgentGone) {
		t.Error("ErrTooLarge must not look like ErrAgentGone")
	}

	// The session is still good for a small answer, and the events path is not capped.
	resp, err = hc.Get("http://docker/_ping")
	if err != nil {
		t.Fatalf("a small request after the large one: %v", err)
	}
	if b, _ := io.ReadAll(resp.Body); string(b) != "ok" {
		t.Errorf("small answer = %q", b)
	}
	_ = resp.Body.Close()
	resp, err = hc.Get("http://docker/v1.47/events?since=1")
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(resp.Body); err != nil || len(b) != len(big) {
		t.Errorf("events read %d bytes, err %v; want all %d", len(b), err, len(big))
	}
	_ = resp.Body.Close()
	if isDone(s) {
		t.Fatal("the session ended")
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

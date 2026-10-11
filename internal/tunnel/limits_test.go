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
	"strconv"
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
	return loggedPlainServer(t, publicScheme, trustLoopback, slog.New(slog.DiscardHandler), nil, configure)
}

// loggedPlainServer is plainServer with the hub's logger and clock chosen by the
// test, for a test that reads what the hub wrote or moves time.
func loggedPlainServer(t *testing.T, publicScheme string, trustLoopback bool, log *slog.Logger, now func() time.Time, configure func(*tunnel.Hub) *tunnel.Hub) (*httptest.Server, *fakeVerifier) {
	t.Helper()
	v := &fakeVerifier{}
	ts := httptest.NewUnstartedServer(nil)
	addr := ts.Listener.Addr().String()
	hub := tunnel.NewHub(v, newObserver(), log, now).
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

// logRecord is one record a recordingHandler kept.
type logRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

// recordingHandler keeps every record it is given, safe for concurrent use. The
// attributes of a handler made with WithAttrs and of a group are kept flat.
type recordingHandler struct {
	mu      *sync.Mutex
	records *[]logRecord
	attrs   map[string]string
}

func newRecordingLogger() (*slog.Logger, *recordingHandler) {
	h := &recordingHandler{mu: &sync.Mutex{}, records: &[]logRecord{}, attrs: map[string]string{}}
	return slog.New(h), h
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	rec := logRecord{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	for k, v := range h.attrs {
		rec.attrs[k] = v
	}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	*h.records = append(*h.records, rec)
	h.mu.Unlock()
	return nil
}

func (h *recordingHandler) WithAttrs(as []slog.Attr) slog.Handler {
	n := &recordingHandler{mu: h.mu, records: h.records, attrs: map[string]string{}}
	for k, v := range h.attrs {
		n.attrs[k] = v
	}
	for _, a := range as {
		n.attrs[a.Key] = a.Value.String()
	}
	return n
}

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

// first is the first of rs, or the zero record, so a failure message shows one
// record and not a hundred.
func first(rs []logRecord) logRecord {
	if len(rs) == 0 {
		return logRecord{}
	}
	return rs[0]
}

// containing returns the records whose message contains substr.
func (h *recordingHandler) containing(substr string) []logRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []logRecord
	for _, r := range *h.records {
		if strings.Contains(r.msg, substr) {
			out = append(out, r)
		}
	}
	return out
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

func TestAnUpgradeFailureIsRecordedByTheProxyObserverAndAnOriginRefusalIsNot(t *testing.T) {
	obs := httpx.NewProxyObserver(func() string { return "" }, nil)
	ts, _ := plainServer(t, "http", false, func(h *tunnel.Hub) *tunnel.Hub { return h.OnUpgradeFailure(obs.RecordUpgradeFailure) })

	if status, _ := upgradeRequest(t, ts.URL, map[string]string{"Origin": "https://evil.example"}); status != http.StatusForbidden {
		t.Fatalf("a browser request answered %d, want 403", status)
	}
	if got := obs.Signals(); len(got) != 0 {
		t.Fatalf("a browser's request to the agent endpoint recorded %+v, want nothing: it is not a proxy fault", got)
	}

	resp, err := http.Get(ts.URL + "/agent/connect")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("status %d, want 426", resp.StatusCode)
	}
	got := obs.Signals()
	if len(got) != 1 || got[0].Kind != httpx.SignalWebSocketFailing || got[0].Peer != "127.0.0.1" {
		t.Fatalf("signals = %+v, want one websocket_failing from 127.0.0.1", got)
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

// firstMessageFrom is firstMessage for a caller that says, in X-Forwarded-For,
// which address it comes from. The header counts only behind a client-address
// middleware that trusts the peer (trustLoopback).
func firstMessageFrom(t *testing.T, ctx context.Context, srv *tunneltest.Server, xff string) (*websocket.Conn, wire.Refuse, string) {
	t.Helper()
	opts := &websocket.DialOptions{HTTPClient: srv.HTTP.Client(), Subprotocols: []string{wire.Subprotocol}}
	if xff != "" {
		opts.HTTPHeader = http.Header{"X-Forwarded-For": []string{xff}}
	}
	ws, _, err := websocket.Dial(ctx, srv.URL(), opts) //nolint:bodyclose // a WebSocket upgrade response has no body to close
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

// TestOneAddressCannotHoldEveryHandshakeSlot is the regression for T-02-24 and
// WR-B-02. A client that upgrades, reads the challenge and never answers holds one
// of the 64 handshake slots; before the per-address cap one address could hold
// them all, and every enrolled agent's reconnect was refused rate_limited for as
// long as it kept going.
func TestOneAddressCannotHoldEveryHandshakeSlot(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{
		Wrap:      trustLoopback,
		Configure: func(h *tunnel.Hub) *tunnel.Hub { return h.WithConnectLimit(1000).WithFirstFrameTimeout(time.Minute) },
	})
	env := srv.NewEnvironment(t, "Production")
	id := enrolled(t, srv, env)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var challenged, refused int
	for i := range 64 {
		ws, ref, typ := firstMessageFrom(t, ctx, srv, "198.51.100.7")
		switch typ {
		case wire.TypeChallenge:
			challenged++
			t.Cleanup(func() { _ = ws.CloseNow() }) // silent: it never answers, it holds its slot
		case wire.TypeRefuse:
			refused++
			_ = ws.CloseNow()
			if ref.Code != wire.RefuseRateLimited || ref.RetryAfterMs <= 0 {
				t.Errorf("flood handshake %d refused as %+v, want rate_limited with retryAfterMs", i+1, ref)
			}
		default:
			_ = ws.CloseNow()
			t.Fatalf("flood handshake %d got %q", i+1, typ)
		}
	}
	if challenged != 16 || refused != 48 {
		t.Errorf("a silent flood from one address got %d challenges and %d refusals of 64, want 16 and 48", challenged, refused)
	}

	// The enrolled agent comes from another address and is not the flood's problem.
	c := srv.Hello(t, id)
	if c.Welcome == nil {
		t.Fatalf("the enrolled agent was not welcomed during the flood: %+v (err %v)", c.Refuse, c.Err)
	}
	waitFor(t, "the enrolled agent's session", func() bool { return srv.Hub.Session(id.AgentID) != nil })
}

// holdFrom opens a handshake that says it comes from xff and does not answer the
// challenge, so it holds its slot until the test ends or closes it. It returns the
// connection and what the server sent first. A test that holds handshakes while
// it opens more sets WithFirstFrameTimeout(time.Minute), so the first-frame
// deadline cannot let the held ones go before the test has looked.
func holdFrom(t *testing.T, ctx context.Context, srv *tunneltest.Server, xff string) (*websocket.Conn, wire.Refuse, string) {
	t.Helper()
	ws, ref, typ := firstMessageFrom(t, ctx, srv, xff)
	t.Cleanup(func() { _ = ws.CloseNow() })
	return ws, ref, typ
}

// The 64 slots are the global bound: with 16 held from each of four addresses
// they are all taken, and the per-address cap is not what refuses the 65th.
func TestConcurrentHandshakeCapRefusesThe65thWithRateLimitedAndARetryAfter(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{
		Wrap:      trustLoopback,
		Configure: func(h *tunnel.Hub) *tunnel.Hub { return h.WithConnectLimit(1000).WithFirstFrameTimeout(time.Minute) },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	held := make([]*websocket.Conn, 0, 64)
	for _, addr := range []string{"198.51.100.1", "198.51.100.2", "198.51.100.3", "198.51.100.4"} {
		for i := range 16 {
			ws, _, typ := holdFrom(t, ctx, srv, addr)
			if typ != wire.TypeChallenge {
				t.Fatalf("handshake %d from %s got %q, want a challenge", i+1, addr, typ)
			}
			held = append(held, ws)
		}
	}

	ws, ref, typ := firstMessageFrom(t, ctx, srv, "198.51.100.5")
	defer func() { _ = ws.CloseNow() }()
	if typ != wire.TypeRefuse || ref.Code != wire.RefuseRateLimited || ref.RetryAfterMs <= 0 {
		t.Fatalf("the 65th handshake got %q %+v, want a refusal rate_limited with retryAfterMs", typ, ref)
	}

	// Letting one go frees its slot.
	_ = held[0].CloseNow()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ws2, _, typ := firstMessageFrom(t, ctx, srv, "198.51.100.5")
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

// One IPv6 /64 is one client for the cap, as it is for the attempt budget, and an
// IPv4-mapped IPv6 address is its IPv4 address, so rotating addresses inside a
// block a provider hands to one customer buys no more slots (T-02-24).
func TestTheHandshakeCapPerAddressCountsAnIPv6SlashSixtyFourAsOneAddress(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{
		Wrap:      trustLoopback,
		Configure: func(h *tunnel.Hub) *tunnel.Hub { return h.WithConnectLimit(1000).WithFirstFrameTimeout(time.Minute) },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	held := make([]*websocket.Conn, 0, 16)
	for i := range 16 {
		ws, _, typ := holdFrom(t, ctx, srv, "2001:db8:1:2::"+strconv.Itoa(i+1))
		if typ != wire.TypeChallenge {
			t.Fatalf("handshake %d from the /64 got %q, want a challenge", i+1, typ)
		}
		held = append(held, ws)
	}
	if _, ref, typ := holdFrom(t, ctx, srv, "2001:db8:1:2::ffff"); typ != wire.TypeRefuse || ref.Code != wire.RefuseRateLimited || ref.RetryAfterMs <= 0 {
		t.Fatalf("a 17th handshake from the same /64 got %q %+v, want rate_limited with retryAfterMs", typ, ref)
	}
	if _, _, typ := holdFrom(t, ctx, srv, "2001:db8:1:3::1"); typ != wire.TypeChallenge {
		t.Fatalf("a handshake from another /64 got %q, want a challenge", typ)
	}

	// Closing a held handshake gives its address's count back.
	_ = held[0].CloseNow()
	waitFor(t, "the /64 to get a slot back", func() bool {
		ws, _, typ := firstMessageFrom(t, ctx, srv, "2001:db8:1:2::ffff")
		_ = ws.CloseNow()
		return typ == wire.TypeChallenge
	})

	// An IPv4-mapped address and its IPv4 address share one count.
	for i := range 16 {
		addr := "198.51.100.9"
		if i%2 == 0 {
			addr = "::ffff:198.51.100.9"
		}
		if _, _, typ := holdFrom(t, ctx, srv, addr); typ != wire.TypeChallenge {
			t.Fatalf("handshake %d from 198.51.100.9 got %q, want a challenge", i+1, typ)
		}
	}
	for _, addr := range []string{"198.51.100.9", "::ffff:198.51.100.9"} {
		if _, ref, typ := holdFrom(t, ctx, srv, addr); typ != wire.TypeRefuse || ref.Code != wire.RefuseRateLimited {
			t.Fatalf("a 17th handshake from %s got %q %+v, want rate_limited: the mapped and plain forms are one address", addr, typ, ref)
		}
	}
}

// X-Forwarded-For names the client only when the peer is a trusted proxy (D-51,
// T-02-126). From any other peer it is just a header, so sixteen different values
// are still one address, the peer's.
func TestAForwardedForHeaderFromAnUntrustedPeerDoesNotBuyMoreHandshakes(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{
		Wrap: func(next http.Handler) http.Handler { return httpx.ClientIPMiddleware(nil)(next) },
		Configure: func(h *tunnel.Hub) *tunnel.Hub {
			return h.WithConnectLimit(1000).WithFirstFrameTimeout(time.Minute)
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for i := range 16 {
		if _, _, typ := holdFrom(t, ctx, srv, "198.51.100."+strconv.Itoa(i+1)); typ != wire.TypeChallenge {
			t.Fatalf("handshake %d got %q, want a challenge", i+1, typ)
		}
	}
	if _, ref, typ := holdFrom(t, ctx, srv, "198.51.100.200"); typ != wire.TypeRefuse || ref.Code != wire.RefuseRateLimited || ref.RetryAfterMs <= 0 {
		t.Fatalf("a 17th handshake with yet another X-Forwarded-For got %q %+v, want rate_limited: the header of an untrusted peer counts for nothing", typ, ref)
	}
}

// A client that reads the challenge and says nothing is let go when the first-frame
// deadline passes, long before the 10 s the whole handshake may take (T-02-123).
func TestASilentHandshakeIsDroppedAtTheFirstFrameDeadline(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ws, _, typ := firstMessageFrom(t, ctx, srv, "")
	defer func() { _ = ws.CloseNow() }()
	if typ != wire.TypeChallenge {
		t.Fatalf("got %q, want a challenge", typ)
	}
	start := time.Now()
	_, _, err := ws.Read(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("the server sent something to a client that said nothing")
	}
	if ctx.Err() != nil {
		t.Fatalf("the server was still holding the connection after %s", elapsed.Round(100*time.Millisecond))
	}
	if elapsed < 2500*time.Millisecond || elapsed > 6*time.Second {
		t.Fatalf("a silent handshake was dropped %s after the challenge, want between 2.5 s and 6 s", elapsed.Round(10*time.Millisecond))
	}
}

// Dropping the silent handshakes gives the address its slots back: sixteen silent
// ones fill the address, and a few seconds later it can start another.
func TestTheFirstFrameDeadlineGivesTheAddressItsSlotsBack(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{
		Wrap:      trustLoopback,
		Configure: func(h *tunnel.Hub) *tunnel.Hub { return h.WithConnectLimit(1000) },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for i := range 16 {
		if _, _, typ := holdFrom(t, ctx, srv, "198.51.100.7"); typ != wire.TypeChallenge {
			t.Fatalf("silent handshake %d got %q, want a challenge", i+1, typ)
		}
	}
	if _, ref, typ := holdFrom(t, ctx, srv, "198.51.100.7"); typ != wire.TypeRefuse || ref.Code != wire.RefuseRateLimited {
		t.Fatalf("a 17th handshake got %q %+v, want rate_limited", typ, ref)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		ws, _, typ := firstMessageFrom(t, ctx, srv, "198.51.100.7")
		_ = ws.CloseNow()
		if typ == wire.TypeChallenge {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the silent handshakes still held every slot of the address 8 s after their challenges")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// The deadline is for the agent's first and only message before the welcome. An
// agent that is a little slow still gets in; one that is slower than the deadline
// does not, and the server has closed the connection by then.
func TestAnAgentThatAnswersWithinTheDeadlineIsWelcomedAndOneThatDoesNotIsDropped(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{})

	t.Run("answers after 1 s", func(t *testing.T) {
		env := srv.NewEnvironment(t, "Prompt")
		p := srv.Begin(t, tunneltest.Agent{Token: env.Token})
		time.Sleep(time.Second)
		if c := p.Finish(); c.Welcome == nil {
			t.Fatalf("an agent that answered after 1 s was not welcomed: %+v (err %v)", c.Refuse, c.Err)
		}
	})
	t.Run("answers after 4 s", func(t *testing.T) {
		env := srv.NewEnvironment(t, "Slow")
		p := srv.Begin(t, tunneltest.Agent{Token: env.Token})
		time.Sleep(4 * time.Second)
		if c := p.Finish(); c.Welcome != nil {
			t.Fatal("an agent that answered 4 s after the challenge was welcomed, want the connection closed at the deadline")
		}
	})
}

func TestWithFirstFrameTimeoutReplacesTheDeadlineOnlyWhenPositive(t *testing.T) {
	// dropped reports how long a silent handshake lasts, or ok false when it is
	// still open after limit.
	dropped := func(t *testing.T, d time.Duration, limit time.Duration) (time.Duration, bool) {
		t.Helper()
		srv := tunneltest.Start(t, tunneltest.Options{Configure: func(h *tunnel.Hub) *tunnel.Hub { return h.WithFirstFrameTimeout(d) }})
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		defer cancel()
		ws, _, typ := firstMessageFrom(t, ctx, srv, "")
		defer func() { _ = ws.CloseNow() }()
		if typ != wire.TypeChallenge {
			t.Fatalf("got %q, want a challenge", typ)
		}
		start := time.Now()
		_, _, err := ws.Read(ctx)
		if err == nil {
			t.Fatal("the server sent something to a client that said nothing")
		}
		return time.Since(start), ctx.Err() == nil
	}

	if elapsed, ok := dropped(t, 300*time.Millisecond, 2*time.Second); !ok || elapsed > 2*time.Second {
		t.Errorf("with a deadline of 300 ms a silent handshake lasted %s (dropped %v), want it dropped quickly", elapsed, ok)
	}
	for _, d := range []time.Duration{0, -time.Second} {
		if elapsed, ok := dropped(t, d, 1500*time.Millisecond); ok {
			t.Errorf("with a deadline of %s a silent handshake was dropped after %s, want the 3 s default", d, elapsed)
		}
	}
}

// getFrom is a plain GET of the agent endpoint that says, in X-Forwarded-For, which
// address it comes from.
func getFrom(t *testing.T, url, xff string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url+"/agent/connect", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-For", xff)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// These lines are written before a request is admitted, so a caller could
// otherwise fill the log with them for the price of a GET (IN-B-02, T-02-124). The
// proxy observer, which reads the same requests as a misconfiguration signal, must
// still hear every one (D-53).
func TestPreAdmissionWarningsAreWrittenOncePerAddressPerMinute(t *testing.T) {
	log, rec := newRecordingLogger()
	clock := tunneltest.NewClock(time.Now())
	var mu sync.Mutex
	hooked := 0
	ts, _ := loggedPlainServer(t, "http", true, log, clock.Now, func(h *tunnel.Hub) *tunnel.Hub {
		return h.OnUpgradeFailure(func(string) {
			mu.Lock()
			hooked++
			mu.Unlock()
		})
	})
	const notUpgrade = "without a WebSocket upgrade"

	for range 50 {
		if status := getFrom(t, ts.URL, "198.51.100.7"); status != http.StatusUpgradeRequired {
			t.Fatalf("status %d, want 426", status)
		}
	}
	if got := rec.containing(notUpgrade); len(got) != 1 || got[0].level != slog.LevelWarn || got[0].attrs["ip"] != "198.51.100.7" {
		t.Fatalf("50 requests from one address wrote %d records (first: %+v), want exactly one WARN naming 198.51.100.7", len(got), first(got))
	}
	mu.Lock()
	if hooked != 50 {
		t.Errorf("the upgrade-failure hook ran %d times for 50 requests, want 50: the proxy observer must hear every one", hooked)
	}
	mu.Unlock()

	if getFrom(t, ts.URL, "198.51.100.8"); len(rec.containing(notUpgrade)) != 2 {
		t.Errorf("a second address wrote %d records in all, want its own record (2)", len(rec.containing(notUpgrade)))
	}

	clock.Advance(61 * time.Second)
	getFrom(t, ts.URL, "198.51.100.7")
	if got := len(rec.containing(notUpgrade)); got != 3 {
		t.Errorf("a request 61 s later left %d records in all, want one more (3)", got)
	}
}

func TestPlainTransportWarningsAreWrittenOncePerAddressPerMinute(t *testing.T) {
	t.Run("accepted over plain HTTP", func(t *testing.T) {
		log, rec := newRecordingLogger()
		ts, _ := loggedPlainServer(t, "http", false, log, nil, nil)
		for range 20 {
			if status, _ := upgradeRequest(t, ts.URL, nil); status != http.StatusSwitchingProtocols {
				t.Fatalf("status %d, want 101", status)
			}
		}
		if got := rec.containing("connected over plain HTTP"); len(got) != 1 || got[0].level != slog.LevelWarn {
			t.Fatalf("20 plain upgrades wrote %d records (first: %+v), want exactly one WARN", len(got), first(got))
		}
	})
	t.Run("refused over plain HTTP", func(t *testing.T) {
		log, rec := newRecordingLogger()
		ts, _ := loggedPlainServer(t, "https", false, log, nil, nil)
		for range 20 {
			status, body := upgradeRequest(t, ts.URL, nil)
			if status != http.StatusForbidden || !strings.Contains(body, wire.RefuseInsecureTransport) {
				t.Fatalf("status %d (%s), want 403 insecure_transport", status, body)
			}
		}
		if got := rec.containing("did not arrive over TLS"); len(got) != 1 || got[0].level != slog.LevelWarn {
			t.Fatalf("20 refused plain upgrades wrote %d records (first: %+v), want exactly one WARN", len(got), first(got))
		}
	})
}

// What an agent says about itself is untrusted input and goes into a log line
// before anything about it is believed (T-02-125).
func TestHandshakeLogLinesClipTheAgentsAudienceAndMode(t *testing.T) {
	log, rec := newRecordingLogger()
	srv := tunneltest.Start(t, tunneltest.Options{Log: log})
	env := srv.NewEnvironment(t, "Production")

	c := srv.Dial(t, tunneltest.Agent{Token: env.Token, Aud: strings.Repeat("a", 4000)})
	if c.Refuse == nil || c.Refuse.Code != wire.ReasonAuthRejected {
		t.Fatalf("a handshake with a 4000-byte audience: %+v (err %v), want auth_rejected", c.Refuse, c.Err)
	}
	got := rec.containing("addressed to a different host")
	if len(got) != 1 {
		t.Fatalf("%d wrong-audience records, want 1", len(got))
	}
	if n := len(got[0].attrs["audience"]); n == 0 || n > 256 {
		t.Errorf("the audience in the log line is %d bytes, want 1 to 256", n)
	}

	c = srv.Dial(t, tunneltest.Agent{Token: env.Token, Mode: strings.Repeat("m", 4000)})
	if c.Refuse == nil {
		t.Fatalf("a handshake with a 4000-byte mode: %+v (err %v), want a refusal", c.Refuse, c.Err)
	}
	got = rec.containing("agent handshake refused")
	if len(got) == 0 {
		t.Fatal("no \"agent handshake refused\" record")
	}
	for _, r := range got {
		if n := len(r.attrs["mode"]); n > 16 {
			t.Errorf("the mode in the log line is %d bytes, want at most 16", n)
		}
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

// denied writes one denied line on the agent's control stream.
func sendDenied(t *testing.T, control io.Writer, method, path, reason string) {
	t.Helper()
	b, _ := json.Marshal(wire.Denied{T: wire.TypeDenied, Method: method, Path: path, Reason: reason})
	if _, err := control.Write(append(b, '\n')); err != nil {
		t.Fatalf("send a denied message: %v", err)
	}
}

func waitEntries(t *testing.T, sink *auditSink, n int) []audit.Entry {
	t.Helper()
	var got []audit.Entry
	waitFor(t, "audit entries", func() bool { got = sink.all(); return len(got) >= n })
	return got
}

func TestADeniedMessageIsAuditedUnderTheAgentHubAndRemembered(t *testing.T) {
	sink := &auditSink{}
	srv := timed(t, func(h *tunnel.Hub) *tunnel.Hub { return h.WithAudit(sink) })
	env := srv.NewEnvironment(t, "Production")
	c := srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: &wire.Facts{EngineID: "ENG:node-1", Hostname: "node-1", EngineReachable: true}})
	if c.Welcome == nil {
		t.Fatalf("refused: %+v", c.Refuse)
	}
	srv.Events.WaitLen(t, 1)
	control := c.OpenControl(t)

	long := "/" + strings.Repeat("p", 1000)
	sendDenied(t, control, "POST", "/containers/create", wire.DeniedReadOnly)
	got := waitEntries(t, sink, 1)
	e := got[0]
	if e.Action != audit.ActionAgentPolicyDenied || e.Actor.Type != audit.ActorSystem || e.Actor.ID != "system:agent-hub" {
		t.Errorf("entry = %+v, want agent.policy_denied under system:agent-hub", e)
	}
	if e.TargetID != c.Identity.AgentID || e.EnvID != env.ID || e.Result != audit.ResultDenied {
		t.Errorf("entry target %q env %q result %q, want the agent, its environment and denied", e.TargetID, e.EnvID, e.Result)
	}
	for k, want := range map[string]string{"node": "node-1", "method": "POST", "endpoint": "/containers/create", "reason": wire.DeniedReadOnly} {
		if e.Details[k] != want {
			t.Errorf("details[%s] = %q, want %q (%v)", k, e.Details[k], want, e.Details)
		}
	}
	for k := range e.Details {
		for _, bad := range []string{"token", "key", "secret"} {
			if strings.Contains(strings.ToLower(k), bad) {
				t.Errorf("detail key %q names a secret", k)
			}
		}
	}

	s := srv.Hub.Session(c.Identity.AgentID)
	waitFor(t, "the last denial on the session", func() bool { return s.Status().LastDenial != nil })
	if d := s.Status().LastDenial; d.Method != "POST" || d.Endpoint != "/containers/create" || d.Reason != wire.DeniedReadOnly || d.At.IsZero() {
		t.Errorf("last denial = %+v", d)
	}

	// What the agent sends is not trusted to be short.
	sendDenied(t, control, strings.Repeat("M", 100), long, strings.Repeat("r", 100))
	got = waitEntries(t, sink, 2)
	e = got[1]
	if len(e.Details["endpoint"]) > 256 || len(e.Details["method"]) > 16 || len(e.Details["reason"]) > 32 {
		t.Errorf("uncapped details: endpoint %d, method %d, reason %d bytes", len(e.Details["endpoint"]), len(e.Details["method"]), len(e.Details["reason"]))
	}
	if isDone(s) {
		t.Error("a denied message ended the session")
	}
}

// An authenticated agent is not trusted to behave: a flood of denied messages must
// not become a flood of writes on the single database writer (Pitfall 8).
func TestAFloodOfDeniedMessagesCostsAtMostTheBurstOfAuditRowsThenOneSummary(t *testing.T) {
	sink := &auditSink{}
	srv := timed(t, func(h *tunnel.Hub) *tunnel.Hub { return h.WithAudit(sink) })
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	srv.Events.WaitLen(t, 1)
	control := c.OpenControl(t)
	s := srv.Hub.Session(c.Identity.AgentID)

	for range 100 {
		sendDenied(t, control, "GET", "/swarm", wire.DeniedAllowlist)
	}
	// Heartbeat after them: heartbeats and denials share one reader, so when it has
	// seen the heartbeat it has handled every denial before it.
	tunneltest.SendHeartbeat(t, control, true, 1)
	waitFor(t, "the reader to catch up", func() bool { return !s.Status().LastHeartbeat.IsZero() })
	if n := len(sink.all()); n != 20 {
		t.Fatalf("%d audit rows for 100 denials, want the burst of 20", n)
	}

	srv.Clock.Advance(2 * time.Second)
	sendDenied(t, control, "GET", "/swarm", wire.DeniedAllowlist)
	got := waitEntries(t, sink, 21)
	if got[20].Details["suppressed"] != "80" {
		t.Errorf("the next row says suppressed=%q, want 80", got[20].Details["suppressed"])
	}
}

// The kind of a stream is decided from the request line the server wrote (plan 04-03,
// T-04-14), with or without the API version prefix, and a piece that is not the start
// of a request is not classified at all.
func TestStartsStreamRequestClassifiesByTheRequestLine(t *testing.T) {
	line := func(s string) []byte { return []byte(s + " HTTP/1.1\r\nHost: docker\r\n\r\n") }
	for _, tc := range []struct {
		name string
		in   []byte
		kind string
		ok   bool
	}{
		{"events, versioned", line("GET /v1.47/events?since=1"), "events", true},
		{"events, plain", line("GET /events"), "events", true},
		{"logs, versioned and followed", line("GET /v1.47/containers/abc/logs?follow=1&stdout=1"), "logs", true},
		{"logs, plain", line("GET /containers/abc/logs"), "logs", true},
		{"exec start, versioned", line("POST /v1.47/exec/abc/start"), "exec", true},
		{"exec start, plain", line("POST /exec/abc/start"), "exec", true},
		{"list", line("GET /containers/json"), "other", true},
		{"versioned list", line("GET /v1.47/containers/json?all=1"), "other", true},
		{"exec create is an ordinary call", line("POST /v1.47/containers/abc/exec"), "other", true},
		{"exec inspect is an ordinary call", line("GET /v1.47/exec/abc/json"), "other", true},
		{"logs by POST is not a log follow", line("POST /v1.47/containers/abc/logs"), "other", true},
		{"exec start by GET is not an exec", line("GET /v1.47/exec/abc/start"), "other", true},
		{"a path that only ends like logs", line("GET /v1.47/containers/logs"), "other", true},
		{"a nested path that ends in logs", line("GET /v1.47/containers/abc/def/logs"), "other", true},
		{"a query that names logs", line("GET /containers/json?x=/containers/abc/logs"), "other", true},
		{"a partial first write", []byte("GET /v1.47/containers/abc/log"), "other", false},
		{"the next piece of a body", []byte("{\"AttachStdin\":true}"), "other", false},
		{"the stream kind byte", []byte{wire.StreamKindDockerHTTP}, "other", false},
		{"empty", nil, "other", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, ok := tunnel.ClassifyStreamRequest(tc.in)
			if kind != tc.kind || ok != tc.ok {
				t.Fatalf("classified %q as %q, %v; want %q, %v", tc.in, kind, ok, tc.kind, tc.ok)
			}
		})
	}
}

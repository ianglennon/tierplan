// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub_test

import (
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/ingresshub/ingresstest"
	"github.com/ianglennon/tierplan/proto/ingresswire"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// holder blocks one registration between the welcome and the registration of the session (Options.Hold), so a
// test decides what happens in that window. It is armed for one registration at a time.
type holder struct {
	armed   atomic.Bool
	entered chan string
	release chan struct{}
	once    sync.Once
}

func newHolder() *holder {
	return &holder{entered: make(chan string, 1), release: make(chan struct{})}
}

func (h *holder) hold(nodeID string) {
	if h.armed.CompareAndSwap(true, false) {
		h.entered <- nodeID
		<-h.release
	}
}

// arm makes the next registration wait. Release lets it, and every later one, through.
func (h *holder) arm() { h.armed.Store(true) }

func (h *holder) Release() { h.once.Do(func() { close(h.release) }) }

// startHeld starts a server whose registrations can be held. The hold is released when the test ends, before
// the server is shut down.
func startHeld(t *testing.T) (*ingresstest.Server, *holder) {
	t.Helper()
	h := newHolder()
	srv := ingresstest.Start(t, ingresstest.Options{Hold: h.hold})
	t.Cleanup(h.Release)
	return srv, h
}

// lcEnrol enrols a new node into env, which lists swarmID, and waits for its session to be registered.
func lcEnrol(t *testing.T, srv *ingresstest.Server, env ingresstest.Environment, swarmID string) (*ingresstest.Identity, *ingresstest.Conn, *ingresshub.Session) {
	t.Helper()
	id := ingresstest.NewIdentity()
	c := srv.Enrol(t, id, ingresstest.Dial{Token: env.Token, Name: "node-" + swarmID, SwarmNodeID: swarmID})
	return id, c, lcAwaitRegistered(t, srv, id.NodeID)
}

func lcAwaitRegistered(t *testing.T, srv *ingresstest.Server, nodeID string) *ingresshub.Session {
	t.Helper()
	s, err := srv.Hub.AwaitSession(testsupport.Context(t), nodeID, func(s *ingresshub.Session) bool { return s != nil })
	if err != nil {
		t.Fatalf("node %s was never registered: %v", nodeID, err)
	}
	return s
}

func lcAwaitGone(t *testing.T, srv *ingresstest.Server, nodeID string) {
	t.Helper()
	if _, err := srv.Hub.AwaitSession(testsupport.Context(t), nodeID, func(s *ingresshub.Session) bool { return s == nil }); err != nil {
		t.Fatalf("the session of node %s is still registered: %v", nodeID, err)
	}
}

// requireClosed reads until c ends and fails unless the server closed it with code and reason.
func requireClosed(t *testing.T, c *ingresstest.Conn, code websocket.StatusCode, reason string) {
	t.Helper()
	gotCode, gotReason := c.AwaitClose(t)
	if gotCode != code || gotReason != reason {
		t.Errorf("connection closed with %d %q, want %d %q", gotCode, gotReason, code, reason)
	}
}

func lcAdmin() audit.Actor { return audit.Actor{Type: audit.ActorUser, ID: "u-admin", Name: "admin"} }

// TestLifecycleNewestConnectionWins: a node that connects twice keeps the second session. The first is closed
// with the reason replaced, and only the second is registered.
func TestLifecycleNewestConnectionWins(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")
	want := ingresstest.MakeBundle(t, 1, "a.example.com")
	srv.Bundles.Set(env.ID, want)
	id, first, firstSession := lcEnrol(t, srv, env, "n1")

	second := srv.Connect(t, id, ingresstest.Dial{})
	requireClosed(t, first, websocket.StatusGoingAway, ingresswire.CloseReplaced)

	secondSession, err := srv.Hub.AwaitSession(testsupport.Context(t), id.NodeID, func(s *ingresshub.Session) bool { return s != nil && s != firstSession })
	if err != nil {
		t.Fatalf("the second connection was never registered: %v", err)
	}
	if got := firstSession.Reason(); got != ingresswire.CloseReplaced {
		t.Errorf("first session reason = %q, want %q", got, ingresswire.CloseReplaced)
	}
	if n := len(srv.Hub.Sessions()); n != 1 {
		t.Errorf("%d sessions registered, want 1", n)
	}
	second.Hello(t, ingresstest.CurrentSchemas(), "")
	if got := second.NextBundle(t); got.ID != want.ID {
		t.Errorf("bundle ID on the second connection = %q, want %q", got.ID, want.ID)
	}
	if secondSession.Reason() != "" {
		t.Errorf("the live session already has the reason %q", secondSession.Reason())
	}
}

// TestLifecycleOlderHandshakeFinishingLastIsDropped: of two handshakes of one node, the one accepted first
// finishes last. It must not replace the session of the later one; it is closed replaced instead.
func TestLifecycleOlderHandshakeFinishingLastIsDropped(t *testing.T) {
	srv, hold := startHeld(t)
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")
	id, c0, _ := lcEnrol(t, srv, env, "n1")
	_ = c0.WS.Close(websocket.StatusNormalClosure, "")
	lcAwaitGone(t, srv, id.NodeID)

	// The earlier-accepted handshake is welcomed and then held before it registers.
	hold.arm()
	older := srv.Connect(t, id, ingresstest.Dial{})
	testsupport.Await(t, "the older handshake to reach the hold", hold.entered)

	// The later one runs through and registers.
	newer := srv.Connect(t, id, ingresstest.Dial{})
	newerSession := lcAwaitRegistered(t, srv, id.NodeID)

	hold.Release()
	requireClosed(t, older, websocket.StatusGoingAway, ingresswire.CloseReplaced)

	if got := srv.Hub.Session(id.NodeID); got != newerSession {
		t.Errorf("the registered session is not the later connection's")
	}
	if n := len(srv.Hub.Sessions()); n != 1 {
		t.Errorf("%d sessions registered, want 1", n)
	}
	if newerSession.Reason() != "" {
		t.Errorf("the later session was ended: %q", newerSession.Reason())
	}
	_ = newer
}

// TestLifecycleRevokeDuringConnectLeavesNoSession: a revoke that commits while a connect is between its
// verification and its registration must not leave a registered session (T-22-36).
func TestLifecycleRevokeDuringConnectLeavesNoSession(t *testing.T) {
	srv, hold := startHeld(t)
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")
	id, c0, _ := lcEnrol(t, srv, env, "n1")
	_ = c0.WS.Close(websocket.StatusNormalClosure, "")
	lcAwaitGone(t, srv, id.NodeID)

	hold.arm()
	c := srv.Connect(t, id, ingresstest.Dial{})
	testsupport.Await(t, "the connect to reach the hold", hold.entered)

	// The revoke commits, and calls Kick, which finds no session yet.
	if _, err := srv.Service.Revoke(testsupport.Context(t), lcAdmin(), env.ID, id.NodeID, ingresshub.RevokeOptions{Reason: "lost"}, "203.0.113.9", "test"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	hold.Release()

	requireClosed(t, c, websocket.StatusPolicyViolation, ingresswire.CloseRevoked)
	lcAwaitGone(t, srv, id.NodeID)
	if n := len(srv.Hub.Sessions()); n != 0 {
		t.Errorf("%d sessions registered after a revoke, want 0", n)
	}
	if srv.Hub.Live(id.NodeID) {
		t.Error("a revoked node is live")
	}
}

// TestLifecycleKickClosesAtOnce: Kick ends one node's session with 1008 and the code as the reason, and
// leaves the others registered.
func TestLifecycleKickClosesAtOnce(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1", "n2")
	a, ca, sa := lcEnrol(t, srv, env, "n1")
	b, _, sb := lcEnrol(t, srv, env, "n2")

	srv.Hub.Kick(a.NodeID, ingresswire.CloseRevoked)
	requireClosed(t, ca, websocket.StatusPolicyViolation, ingresswire.CloseRevoked)
	lcAwaitGone(t, srv, a.NodeID)

	if got := sa.Reason(); got != ingresswire.CloseRevoked {
		t.Errorf("kicked session reason = %q, want %q", got, ingresswire.CloseRevoked)
	}
	if got := srv.Hub.Session(b.NodeID); got != sb {
		t.Error("the other node's session is no longer registered")
	}
	if sb.Reason() != "" {
		t.Errorf("the other node's session was ended: %q", sb.Reason())
	}
	// A kick of a node with no session does nothing.
	srv.Hub.Kick("no-such-node", ingresswire.CloseRevoked)
}

// TestLifecycleKickEnvClosesOnlyThatEnvironment: KickEnv closes every session of one environment and none of
// another.
func TestLifecycleKickEnvClosesOnlyThatEnvironment(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env1 := srv.NewEnvironment(t, "swarm")
	env2 := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env1.ID, "n1", "n2")
	srv.Nodes.Set(env2.ID, "n3")
	a1, c1, _ := lcEnrol(t, srv, env1, "n1")
	a2, c2, _ := lcEnrol(t, srv, env1, "n2")
	b, _, sb := lcEnrol(t, srv, env2, "n3")

	srv.Hub.KickEnv(env1.ID, ingresswire.CloseRemoved)
	requireClosed(t, c1, websocket.StatusPolicyViolation, ingresswire.CloseRemoved)
	requireClosed(t, c2, websocket.StatusPolicyViolation, ingresswire.CloseRemoved)
	lcAwaitGone(t, srv, a1.NodeID)
	lcAwaitGone(t, srv, a2.NodeID)

	if got := srv.Hub.Session(b.NodeID); got != sb || sb.Reason() != "" {
		t.Errorf("the other environment's session was disturbed: registered %v, reason %q", got == sb, sb.Reason())
	}
}

// TestLifecycleCloseEndsEverySession: Close ends every session with the reason shutdown, and a new connection
// is refused with 503.
func TestLifecycleCloseEndsEverySession(t *testing.T) {
	srv := ingresstest.Start(t, ingresstest.Options{})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1", "n2")
	_, c1, s1 := lcEnrol(t, srv, env, "n1")
	_, c2, s2 := lcEnrol(t, srv, env, "n2")

	// Close waits for the nodes to answer the close frame, so the nodes read while it runs.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		srv.Hub.Close()
	}()
	requireClosed(t, c1, websocket.StatusGoingAway, ingresswire.CloseShutdown)
	requireClosed(t, c2, websocket.StatusGoingAway, ingresswire.CloseShutdown)
	testsupport.Await(t, "Hub.Close to return", closed)

	for _, s := range []*ingresshub.Session{s1, s2} {
		if got := s.Reason(); got != ingresswire.CloseShutdown {
			t.Errorf("session reason = %q, want %q", got, ingresswire.CloseShutdown)
		}
	}
	refused := ingresstest.DialNode(t, srv.URL(), srv.Client(), srv.Audience(), ingresstest.NewIdentity(), ingresstest.Dial{Token: env.Token})
	if refused.Status != 503 {
		t.Errorf("a dial after Close got status %d (%v), want 503", refused.Status, refused.Err)
	}
}

// tick delivers one tick to the watchdog. The channel is unbuffered, so the send completes once the watchdog
// has taken the tick.
func tick(t *testing.T, ticks chan<- time.Time, what string) {
	t.Helper()
	select {
	case ticks <- time.Time{}:
	case <-testsupport.Context(t).Done():
		t.Fatalf("the watchdog never took %s", what)
	}
}

// TestLifecycleWatchdogClosesASilentSession: on a tick, a session silent for longer than the offline window is
// closed with the reason timeout, and one that sent a message within the window is left alone. The first
// message after the welcome is the only one the node sends here, so it stands for the health message of 22-16.
func TestLifecycleWatchdogClosesASilentSession(t *testing.T) {
	clock := ingresstest.NewClock(time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	ticks := make(chan time.Time)
	srv := ingresstest.Start(t, ingresstest.Options{Clock: clock, WatchTicks: ticks})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1")
	srv.Bundles.Set(env.ID, ingresstest.MakeBundle(t, 1, "a.example.com"))
	_, c, s := lcEnrol(t, srv, env, "n1")

	// Sixty seconds in, the node speaks; the answer proves the hub has read it.
	clock.Advance(60 * time.Second)
	c.Hello(t, ingresstest.CurrentSchemas(), "")
	c.NextBundle(t)

	// Another sixty seconds: 120 s since it connected, 60 s since it spoke. Within the window. The second send
	// completes only once the watchdog is back waiting, so the first tick has been evaluated by then; a
	// watchdog that had closed the session on the first tick would not take the second.
	clock.Advance(60 * time.Second)
	tick(t, ticks, "the first tick")
	tick(t, ticks, "the second tick")
	if s.Reason() != "" {
		t.Fatalf("a session that spoke within the offline window was ended: %q", s.Reason())
	}

	// Forty more seconds: 100 s of silence, over the 90 s window. The second tick may still be evaluating, and
	// if it sees the new time it closes the session by itself, which is the outcome being waited for; so the
	// node reads while the tick is sent, and the third tick is not required.
	ended := make(chan error, 1)
	rctx := testsupport.Context(t)
	go func() {
		for {
			if _, _, err := c.WS.Read(rctx); err != nil {
				ended <- err
				return
			}
		}
	}()
	clock.Advance(40 * time.Second)
	var closeErr error
	select {
	case ticks <- time.Time{}:
		closeErr = testsupport.Await(t, "the node to see its connection closed", ended)
	case closeErr = <-ended:
	}
	var ce websocket.CloseError
	if !errors.As(closeErr, &ce) || ce.Code != websocket.StatusPolicyViolation || ce.Reason != ingresswire.CloseTimeout {
		t.Errorf("connection ended with %v, want a close with %d %q", closeErr, websocket.StatusPolicyViolation, ingresswire.CloseTimeout)
	}
	testsupport.Await(t, "the session to end", s.Done())
	if got := s.Reason(); got != ingresswire.CloseTimeout {
		t.Errorf("session reason = %q, want %q", got, ingresswire.CloseTimeout)
	}
}

// TestLifecycleLiveAndDialHosts: a session is live within the offline window of its last message, and
// DialHosts lists the host each live session dialed.
func TestLifecycleLiveAndDialHosts(t *testing.T) {
	clock := ingresstest.NewClock(time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
	// Nothing is ever sent on ticks, so the watchdog never looks: this test moves the clock past the window
	// without wanting the session closed.
	srv := ingresstest.Start(t, ingresstest.Options{Clock: clock, WatchTicks: make(chan time.Time)})
	env := srv.NewEnvironment(t, "swarm")
	srv.Nodes.Set(env.ID, "n1", "n2")
	srv.Bundles.Set(env.ID, ingresstest.MakeBundle(t, 1, "a.example.com"))
	a, ca, _ := lcEnrol(t, srv, env, "n1")
	_, _, _ = lcEnrol(t, srv, env, "n2")

	if !srv.Hub.Live(a.NodeID) {
		t.Error("a fresh session is not live")
	}
	if srv.Hub.Live("no-such-node") {
		t.Error("a node with no session is live")
	}
	if got, want := srv.Hub.DialHosts(), []string{"127.0.0.1"}; !slices.Equal(got, want) {
		t.Errorf("DialHosts = %v, want %v (once, though two sessions dialed it)", got, want)
	}

	clock.Advance(ingresshub.DefaultOfflineAfter + time.Second)
	if srv.Hub.Live(a.NodeID) {
		t.Error("a session silent past the window is live")
	}
	if got := srv.Hub.DialHosts(); len(got) != 0 {
		t.Errorf("DialHosts = %v after the window passed, want none", got)
	}

	// A message makes it live again.
	ca.Hello(t, ingresstest.CurrentSchemas(), "")
	ca.NextBundle(t)
	if !srv.Hub.Live(a.NodeID) {
		t.Error("a session that just spoke is not live")
	}
	if got := srv.Hub.DialHosts(); len(got) != 1 {
		t.Errorf("DialHosts = %v, want the host of the one live session", got)
	}
}

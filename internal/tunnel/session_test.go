// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package tunnel_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/tunnel"
	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/bayplan/proto/wire"
)

// timed starts a server whose hub heartbeats every 10 s, calls a session offline
// after 30 s without one and looks for silent sessions every few milliseconds,
// on a clock the test moves.
func timed(t *testing.T, configure func(*tunnel.Hub) *tunnel.Hub) *tunneltest.Server {
	t.Helper()
	return tunneltest.Start(t, tunneltest.Options{Configure: func(h *tunnel.Hub) *tunnel.Hub {
		h = h.WithTiming(10*time.Second, 30*time.Second).WithWatchInterval(5 * time.Millisecond)
		if configure != nil {
			h = configure(h)
		}
		return h
	}})
}

func isDone(s *tunnel.Session) bool {
	select {
	case <-s.Done():
		return true
	default:
		return false
	}
}

// stays asserts that the session is still open after the hub has had many
// chances to look at it.
func stays(t *testing.T, s *tunnel.Session, why string) {
	t.Helper()
	time.Sleep(60 * time.Millisecond) // a dozen watch intervals
	if isDone(s) {
		t.Fatalf("the session was closed: %s", why)
	}
}

func TestTimeoutClosesASessionThatHasBeenSilentForTheOfflineWindow(t *testing.T) {
	srv := timed(t, nil)
	env := srv.NewEnvironment(t, "Production")
	c := srv.Enrol(t, env)
	srv.Events.WaitLen(t, 1)
	s := srv.Hub.Session(c.Identity.AgentID)
	control := c.OpenControl(t)

	tunneltest.SendHeartbeat(t, control, true, 1)
	waitFor(t, "the heartbeat", func() bool { return !s.Status().LastHeartbeat.IsZero() })

	// A heartbeat at 29 s of a 30 s window keeps the session open.
	srv.Clock.Advance(29 * time.Second)
	stays(t, s, "29 s of silence is inside the 30 s window")
	tunneltest.SendHeartbeat(t, control, true, 2)
	waitFor(t, "the second heartbeat", func() bool { return s.Status().LastHeartbeat.Equal(srv.Clock.Now()) })
	srv.Clock.Advance(29 * time.Second)
	stays(t, s, "the heartbeat 29 s ago reset the window")

	// Past the window the session is closed, with the reason timeout.
	srv.Clock.Advance(2 * time.Second)
	waitFor(t, "the session to be closed", func() bool { return isDone(s) })
	log := srv.Events.WaitLen(t, 2)
	if want := "disconnected:" + c.Identity.AgentID + ":" + tunnel.DisconnectTimeout; log[1] != want {
		t.Fatalf("lifecycle = %v, want %q", log, want)
	}
}

func TestASessionThatNeverHeartbeatsIsClosedToo(t *testing.T) {
	srv := timed(t, nil)
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	srv.Events.WaitLen(t, 1)
	s := srv.Hub.Session(c.Identity.AgentID)
	srv.Clock.Advance(31 * time.Second)
	waitFor(t, "the silent session to be closed", func() bool { return isDone(s) })
	log := srv.Events.WaitLen(t, 2)
	if log[1] != "disconnected:"+c.Identity.AgentID+":timeout" {
		t.Fatalf("lifecycle = %v", log)
	}
}

func TestStatusReportsTheLastHeartbeatFromMemory(t *testing.T) {
	srv := timed(t, nil)
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	srv.Events.WaitLen(t, 1)
	s := srv.Hub.Session(c.Identity.AgentID)
	if st := s.Status(); !st.LastHeartbeat.IsZero() || st.EngineReachable {
		t.Fatalf("status before any heartbeat = %+v", st)
	}
	control := c.OpenControl(t)
	srv.Clock.Advance(3 * time.Second)
	at := srv.Clock.Now()
	tunneltest.SendHeartbeat(t, control, true, 1_700_000_000_123)
	waitFor(t, "the heartbeat", func() bool { return s.Status().ClockMs == 1_700_000_000_123 })
	if st := s.Status(); !st.LastHeartbeat.Equal(at) || !st.EngineReachable {
		t.Fatalf("status = %+v, want the server's time %v and an engine that is reachable", st, at)
	}
	tunneltest.SendHeartbeat(t, control, false, 1_700_000_000_456)
	waitFor(t, "the engine to be reported unreachable", func() bool { return !s.Status().EngineReachable })
	if got := s.Status().ClockMs; got != 1_700_000_000_456 {
		t.Errorf("clock = %d", got)
	}
}

func TestUnknownControlMessagesAreIgnoredAndAnOverlongLineEndsTheSession(t *testing.T) {
	srv := timed(t, nil)
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	srv.Events.WaitLen(t, 1)
	s := srv.Hub.Session(c.Identity.AgentID)
	control := c.OpenControl(t)

	for _, line := range []string{`{"t":"from-the-future","x":1}`, "this is not json", `{}`, ""} {
		if _, err := io.WriteString(control, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	tunneltest.SendHeartbeat(t, control, true, 7)
	waitFor(t, "the heartbeat after the unknown messages", func() bool { return s.Status().ClockMs == 7 })
	stays(t, s, "an unknown control message is not an error")

	// A line over 64 KiB is a misbehaving agent: the session ends.
	_, _ = io.WriteString(control, strings.Repeat("x", wire.MaxControlLine+10)+"\n")
	waitFor(t, "the session to end", func() bool { return isDone(s) })
}

// slowDocker answers a request only when told to, so a request can be held open.
func holdOpenStreams(t *testing.T, c *tunneltest.Conn) {
	t.Helper()
	go func() {
		for {
			st, err := c.Mux.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, st) }() // reads the request, never answers
		}
	}()
}

func httpOver(s *tunnel.Session) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: s.Transport().DialContext}}
}

func TestFailFastABlockedRequestEndsWithErrAgentGoneWithinASecondOfTheDrop(t *testing.T) {
	for name, closeIt := range map[string]func(srv *tunneltest.Server, c *tunneltest.Conn){
		"the agent drops the connection": func(_ *tunneltest.Server, c *tunneltest.Conn) { _ = c.WS.CloseNow() },
		"the server shuts down":          func(srv *tunneltest.Server, _ *tunneltest.Conn) { srv.Hub.Close() },
	} {
		t.Run(name, func(t *testing.T) {
			srv := timed(t, nil)
			c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
			srv.Events.WaitLen(t, 1)
			s := srv.Hub.Session(c.Identity.AgentID)
			holdOpenStreams(t, c)

			errc := make(chan error, 1)
			go func() {
				resp, err := httpOver(s).Get("http://docker/containers/json")
				if err == nil {
					_ = resp.Body.Close()
				}
				errc <- err
			}()
			time.Sleep(100 * time.Millisecond) // the request is now blocked on its stream
			closed := time.Now()
			closeIt(srv, c)
			select {
			case err := <-errc:
				if !errors.Is(err, tunnel.ErrAgentGone) {
					t.Fatalf("the blocked request ended with %v, want an error wrapping ErrAgentGone", err)
				}
				if took := time.Since(closed); took > time.Second {
					t.Errorf("the blocked request took %v to fail", took)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the blocked request is still hanging")
			}
			if _, err := s.Transport().DialContext(context.Background(), "tcp", "x"); !errors.Is(err, tunnel.ErrAgentGone) {
				t.Errorf("a dial after the drop = %v, want ErrAgentGone", err)
			}
		})
	}
}

func TestFailFastTwentyConcurrentDialsDuringTheCloseAllReturn(t *testing.T) {
	srv := timed(t, nil)
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	srv.Events.WaitLen(t, 1)
	s := srv.Hub.Session(c.Identity.AgentID)
	holdOpenStreams(t, c)

	const n = 20
	var started, finished sync.WaitGroup
	var wrong atomic.Int32
	started.Add(n)
	for range n {
		finished.Go(func() {
			started.Done()
			started.Wait()
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				conn, err := s.Transport().DialContext(context.Background(), "tcp", "x")
				if err != nil {
					if !errors.Is(err, tunnel.ErrAgentGone) {
						wrong.Add(1)
					}
					return
				}
				_ = conn.Close()
			}
			wrong.Add(1) // never failed
		})
	}
	started.Wait()
	time.Sleep(5 * time.Millisecond)
	_ = c.WS.CloseNow()

	done := make(chan struct{})
	go func() { finished.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a dial is still hanging after the close")
	}
	if w := wrong.Load(); w != 0 {
		t.Fatalf("%d dials did not fail with ErrAgentGone", w)
	}
}

func TestFailFastReadsAndWritesOnAStreamOfADeadSessionWrapErrAgentGone(t *testing.T) {
	srv := timed(t, nil)
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	srv.Events.WaitLen(t, 1)
	s := srv.Hub.Session(c.Identity.AgentID)
	holdOpenStreams(t, c)
	conn, err := s.Transport().DialContext(context.Background(), "tcp", "x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	_ = c.WS.CloseNow()
	waitFor(t, "the session to end", func() bool { return isDone(s) })
	if _, err := conn.Read(make([]byte, 8)); !errors.Is(err, tunnel.ErrAgentGone) {
		t.Errorf("Read on a stream of a dead session = %v, want ErrAgentGone", err)
	}
	if _, err := conn.Write([]byte("x")); !errors.Is(err, tunnel.ErrAgentGone) {
		t.Errorf("Write on a stream of a dead session = %v, want ErrAgentGone", err)
	}
}

func TestAReadDeadlineOnALiveSessionIsNotAnAgentGone(t *testing.T) {
	srv := timed(t, nil)
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	srv.Events.WaitLen(t, 1)
	s := srv.Hub.Session(c.Identity.AgentID)
	holdOpenStreams(t, c)
	conn, err := s.Transport().DialContext(context.Background(), "tcp", "x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	_, err = conn.Read(make([]byte, 8))
	var ne net.Error
	if errors.Is(err, tunnel.ErrAgentGone) || !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("Read past its deadline = %v, want a timeout and not ErrAgentGone", err)
	}
}

func TestHeartbeatsAndTheConnectAreReportedToTheLastSeenRecorder(t *testing.T) {
	rec := &countingRecorder{}
	srv := timed(t, func(h *tunnel.Hub) *tunnel.Hub { return h.WithLastSeen(rec) })
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	srv.Events.WaitLen(t, 1)
	control := c.OpenControl(t)
	for range 3 {
		tunneltest.SendHeartbeat(t, control, true, 1)
	}
	waitFor(t, "the recorder to hear of three heartbeats", func() bool { return rec.count(c.Identity.AgentID) >= 3 })
}

type countingRecorder struct {
	mu sync.Mutex
	n  map[string]int
}

func (r *countingRecorder) TouchLastSeen(_ context.Context, agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == nil {
		r.n = map[string]int{}
	}
	r.n[agentID]++
}

func (r *countingRecorder) count(agentID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n[agentID]
}

func TestTheWelcomeCarriesTheConfiguredHeartbeatInterval(t *testing.T) {
	srv := tunneltest.Start(t, tunneltest.Options{Configure: func(h *tunnel.Hub) *tunnel.Hub {
		return h.WithTiming(4*time.Second, 12*time.Second)
	}})
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	if c.Welcome == nil || c.Welcome.HeartbeatMs != 4000 {
		t.Fatalf("welcome = %+v, want heartbeatMs 4000", c.Welcome)
	}
}

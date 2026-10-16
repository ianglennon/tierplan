// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package tunnel_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
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
	"github.com/ianglennon/bayplan/proto/testsupport"
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

// ticks is a watchdog the test drives: handed to a hub through option, it is the
// only thing that makes the hub look for silent sessions, and each value sent on
// it is one look. A nil ticks leaves the hub on its own ticker.
type ticks chan time.Time

func newTicks() ticks { return make(ticks) }

// option is a hub option that makes k the watchdog's tick source.
func (k ticks) option(h *tunnel.Hub) *tunnel.Hub { return h.WithWatchTicks(k) }

// look has the watchdog look at the session twice. The channel is unbuffered, so a
// send that completes means the watchdog took the tick and the second one
// completing means it finished evaluating the first. It returns early when the
// session ends, which nothing takes ticks for any more.
func (k ticks) look(t testing.TB, s *tunnel.Session) {
	t.Helper()
	ctx := testsupport.Context(t)
	for range 2 {
		select {
		case k <- time.Time{}:
		case <-s.Done():
			return
		case <-ctx.Done():
			t.Fatal("the watchdog took no tick")
		}
	}
}

// stays asserts that the session is still open after the watchdog has looked at
// it twice, on the clock as the test left it.
func stays(t *testing.T, k ticks, s *tunnel.Session, why string) {
	t.Helper()
	k.look(t, s)
	if isDone(s) {
		t.Fatalf("the session was closed: %s", why)
	}
}

// staysAfterAHeartbeat asserts that the session is still open once the server has
// read a heartbeat the agent sent after the thing under test. The control stream
// is read in order, so the heartbeat being read means everything sent before it
// was read first, and a session that something closed in between ends the wait
// with ErrSessionEnded.
func staysAfterAHeartbeat(t *testing.T, s *tunnel.Session, control io.Writer, clockMs int64, why string) {
	t.Helper()
	tunneltest.SendHeartbeat(t, control, true, clockMs)
	if _, err := s.AwaitStatus(testsupport.Context(t), func(st tunnel.SessionStatus) bool { return st.ClockMs == clockMs }); err != nil {
		t.Fatalf("the session did not stay open: %s (%v)", why, err)
	}
	if isDone(s) {
		t.Fatalf("the session was closed: %s", why)
	}
}

// awaitStatus waits until the session's status satisfies cond.
func awaitStatus(t *testing.T, s *tunnel.Session, what string, cond func(tunnel.SessionStatus) bool) tunnel.SessionStatus {
	t.Helper()
	st, err := s.AwaitStatus(testsupport.Context(t), cond)
	if err != nil {
		t.Fatalf("waiting for %s: %v (last status %+v)", what, err, st)
	}
	return st
}

func TestTimeoutClosesASessionThatHasBeenSilentForTheOfflineWindow(t *testing.T) {
	k := newTicks()
	srv := timed(t, k.option)
	env := srv.NewEnvironment(t, "Production")
	c := srv.Enrol(t, env)
	srv.Events.WaitLen(t, 1)
	s := srv.Hub.Session(c.Identity.AgentID)
	control := c.OpenControl(t)

	tunneltest.SendHeartbeat(t, control, true, 1)
	awaitStatus(t, s, "the heartbeat", func(st tunnel.SessionStatus) bool { return !st.LastHeartbeat.IsZero() })

	// A heartbeat at 29 s of a 30 s window keeps the session open.
	srv.Clock.Advance(29 * time.Second)
	stays(t, k, s, "29 s of silence is inside the 30 s window")
	tunneltest.SendHeartbeat(t, control, true, 2)
	awaitStatus(t, s, "the second heartbeat", func(st tunnel.SessionStatus) bool { return st.LastHeartbeat.Equal(srv.Clock.Now()) })
	srv.Clock.Advance(29 * time.Second)
	stays(t, k, s, "the heartbeat 29 s ago reset the window")

	// Past the window the session is closed, with the reason timeout.
	srv.Clock.Advance(2 * time.Second)
	k.look(t, s)
	testsupport.Await(t, "the session to be closed", s.Done())
	log := srv.Events.WaitLen(t, 2)
	if want := "disconnected:" + c.Identity.AgentID + ":" + tunnel.DisconnectTimeout; log[1] != want {
		t.Fatalf("lifecycle = %v, want %q", log, want)
	}
}

func TestTheWatchdogClosesASessionOnlyAfterItHasBeenSilentForTheOfflineWindow(t *testing.T) {
	k := newTicks()
	srv := timed(t, k.option)
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	srv.Events.WaitLen(t, 1)
	s := srv.Hub.Session(c.Identity.AgentID)
	control := c.OpenControl(t)
	tunneltest.SendHeartbeat(t, control, true, 1)
	awaitStatus(t, s, "the heartbeat", func(st tunnel.SessionStatus) bool { return st.ClockMs == 1 })

	// Exactly the window of silence is not more than the window.
	srv.Clock.Advance(30 * time.Second)
	stays(t, k, s, "30 s of silence in a 30 s window is not past it")

	// A second more is, and the very next look closes the session.
	srv.Clock.Advance(time.Second)
	k.look(t, s)
	testsupport.Await(t, "the watchdog to close the silent session", s.Done())
	if got := s.Reason(); got != tunnel.DisconnectTimeout {
		t.Fatalf("the session ended with %q, want %q", got, tunnel.DisconnectTimeout)
	}
}

func TestASessionThatNeverHeartbeatsIsClosedToo(t *testing.T) {
	k := newTicks()
	srv := timed(t, k.option)
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	srv.Events.WaitLen(t, 1)
	s := srv.Hub.Session(c.Identity.AgentID)
	srv.Clock.Advance(31 * time.Second)
	k.look(t, s)
	testsupport.Await(t, "the silent session to be closed", s.Done())
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
	awaitStatus(t, s, "the heartbeat", func(st tunnel.SessionStatus) bool { return st.ClockMs == 1_700_000_000_123 })
	if st := s.Status(); !st.LastHeartbeat.Equal(at) || !st.EngineReachable {
		t.Fatalf("status = %+v, want the server's time %v and an engine that is reachable", st, at)
	}
	tunneltest.SendHeartbeat(t, control, false, 1_700_000_000_456)
	awaitStatus(t, s, "the engine to be reported unreachable", func(st tunnel.SessionStatus) bool { return !st.EngineReachable })
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
	awaitStatus(t, s, "the heartbeat after the unknown messages", func(st tunnel.SessionStatus) bool { return st.ClockMs == 7 })
	// The control stream is read in order, so the heartbeat being read means the
	// unknown messages before it were read and ignored.
	if isDone(s) {
		t.Fatal("the session was closed: an unknown control message is not an error")
	}

	// A line over 64 KiB is a misbehaving agent: the session ends.
	_, _ = io.WriteString(control, strings.Repeat("x", wire.MaxControlLine+10)+"\n")
	testsupport.Await(t, "the session to end", s.Done())
}

// holdOpenStreams reads every stream the server opens and never answers, so a
// request can be held open. The channel it returns receives once for each stream
// on which the agent has read the first byte of a request, so a test knows the
// request is blocked on its stream and not still on its way.
func holdOpenStreams(t *testing.T, c *tunneltest.Conn) <-chan struct{} {
	t.Helper()
	requests := make(chan struct{}, 64)
	go func() {
		for {
			st, err := c.Mux.Accept()
			if err != nil {
				return
			}
			go func() {
				var first [1]byte
				if _, err := io.ReadFull(st, first[:]); err != nil {
					return
				}
				select {
				case requests <- struct{}{}:
				default:
				}
				_, _ = io.Copy(io.Discard, st) // reads the rest of the request, never answers
			}()
		}
	}()
	return requests
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
			requests := holdOpenStreams(t, c)

			errc := make(chan error, 1)
			go func() {
				resp, err := httpOver(s).Get("http://docker/containers/json")
				if err == nil {
					_ = resp.Body.Close()
				}
				errc <- err
			}()
			testsupport.Await(t, "the request to reach the agent's stream", requests)
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
	// dialling is closed by the first dial that succeeds, so the close below lands
	// while dials are demonstrably in progress.
	dialling := make(chan struct{})
	var firstDial sync.Once
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
				firstDial.Do(func() { close(dialling) })
				_ = conn.Close()
			}
			wrong.Add(1) // never failed
		})
	}
	started.Wait()
	testsupport.Await(t, "a dial to succeed before the close", dialling)
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
	testsupport.Await(t, "the session to end", s.Done())
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
	for range 3 {
		testsupport.Await(t, "the recorder to be touched", rec.heard())
	}
	if got := rec.count(c.Identity.AgentID); got < 3 {
		t.Fatalf("the recorder heard of %d heartbeats, want at least 3", got)
	}
}

// countingRecorder counts the touches per agent and sends on a channel for each.
type countingRecorder struct {
	mu    sync.Mutex
	n     map[string]int
	touch chan struct{}
}

func (r *countingRecorder) touches() chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.touch == nil {
		r.touch = make(chan struct{}, 64)
	}
	return r.touch
}

// heard receives once for each touch.
func (r *countingRecorder) heard() <-chan struct{} { return r.touches() }

func (r *countingRecorder) TouchLastSeen(_ context.Context, agentID string) {
	ch := r.touches()
	r.mu.Lock()
	if r.n == nil {
		r.n = map[string]int{}
	}
	r.n[agentID]++
	r.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
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

// --- renewal over the control stream (D-04) ---

const day = 24 * time.Hour

// quiet starts a server whose sessions are never closed for silence however far the
// test moves the clock, so a test can jump to day 20 and ask for a renewal.
func quiet(t *testing.T) *tunneltest.Server {
	t.Helper()
	return tunneltest.Start(t, tunneltest.Options{Configure: func(h *tunnel.Hub) *tunnel.Hub {
		return h.WithTiming(10*time.Second, 1000*day)
	}})
}

// controlLines reads the lines the server writes back on the agent's control stream.
type controlLines struct {
	t     *testing.T
	lines chan []byte
}

func readControlLines(t *testing.T, control io.Reader) *controlLines {
	t.Helper()
	c := &controlLines{t: t, lines: make(chan []byte, 64)}
	go func() {
		sc := bufio.NewScanner(control)
		sc.Buffer(make([]byte, 0, 4096), wire.MaxControlLine)
		for sc.Scan() {
			c.lines <- append([]byte(nil), sc.Bytes()...)
		}
		close(c.lines)
	}()
	return c
}

// next returns the next line, or fails the test when none arrives.
func (c *controlLines) next() []byte {
	c.t.Helper()
	select {
	case l, ok := <-c.lines:
		if !ok {
			c.t.Fatal("the control stream ended before the server answered")
		}
		return l
	case <-time.After(5 * time.Second):
		c.t.Fatal("the server wrote nothing on the control stream")
		return nil
	}
}

// none asserts that nothing arrives for a while.
func (c *controlLines) none(why string) {
	c.t.Helper()
	select {
	case l := <-c.lines:
		c.t.Fatalf("the server wrote %s on the control stream: %s", l, why)
	case <-time.After(100 * time.Millisecond):
	}
}

func askToRenew(t *testing.T, control io.Writer) {
	t.Helper()
	if _, err := io.WriteString(control, `{"t":"renew"}`+"\n"); err != nil {
		t.Fatalf("send renew: %v", err)
	}
}

func TestARenewOnTheControlStreamIsAnsweredWithAFreshCertificateForTheSameKey(t *testing.T) {
	srv := quiet(t)
	env := srv.NewEnvironment(t, "Production")
	c := srv.Enrol(t, env)
	srv.Events.WaitLen(t, 1)
	control := c.OpenControl(t)
	answers := readControlLines(t, control)
	var oldSerial string
	if err := srv.Store.Read.QueryRow(`SELECT cert_serial FROM agents WHERE id = ?`, c.Identity.AgentID).Scan(&oldSerial); err != nil {
		t.Fatal(err)
	}

	srv.Clock.Advance(20 * day)
	askToRenew(t, control)
	var got wire.Renewed
	if err := json.Unmarshal(answers.next(), &got); err != nil || got.T != wire.TypeRenewed {
		t.Fatalf("answer = %+v, %v; want a renewed message", got, err)
	}
	leaf, err := x509.ParseCertificate(got.Cert)
	if err != nil {
		t.Fatalf("the renewed certificate: %v", err)
	}
	if pub, ok := leaf.PublicKey.(ed25519.PublicKey); !ok || !bytes.Equal(pub, c.Identity.Public()) {
		t.Fatal("the renewed certificate is for another key")
	}
	// A certificate keeps whole seconds; the clock of the test has more.
	if want := srv.Clock.Now().Add(30 * day).Truncate(time.Second); !leaf.NotAfter.Equal(want) {
		t.Errorf("NotAfter = %v, want 30 days from now (%v)", leaf.NotAfter, want)
	}
	var serial string
	var notAfter int64
	if err := srv.Store.Read.QueryRow(`SELECT cert_serial, cert_not_after FROM agents WHERE id = ?`, c.Identity.AgentID).Scan(&serial, &notAfter); err != nil {
		t.Fatal(err)
	}
	if d := time.Duration(notAfter - leaf.NotAfter.UnixNano()); serial == oldSerial || d < 0 || d >= time.Second {
		t.Errorf("the agent row says serial %s expiring %d; the certificate is new and expires %d", serial, notAfter, leaf.NotAfter.UnixNano())
	}
	staysAfterAHeartbeat(t, srv.Hub.Session(c.Identity.AgentID), control, 901, "a renewal keeps the session")
}

func TestARenewFromAnAgentThatIsNotDueIsRefusedAndTheSessionStays(t *testing.T) {
	srv := quiet(t)
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	srv.Events.WaitLen(t, 1)
	control := c.OpenControl(t)
	answers := readControlLines(t, control)

	askToRenew(t, control)
	var got wire.RenewRefused
	if err := json.Unmarshal(answers.next(), &got); err != nil || got.T != wire.TypeRenewRefused || got.Code != wire.RenewNotDue {
		t.Fatalf("answer = %+v, %v; want renew_refused not_due", got, err)
	}
	staysAfterAHeartbeat(t, srv.Hub.Session(c.Identity.AgentID), control, 902, "an early renew is refused, not punished")
}

func TestARenewFromAnAgentRevokedMeanwhileIsRefusedAndTheSessionClosed(t *testing.T) {
	srv := quiet(t)
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	srv.Events.WaitLen(t, 1)
	s := srv.Hub.Session(c.Identity.AgentID)
	control := c.OpenControl(t)
	answers := readControlLines(t, control)

	// The revoke has committed and its kick has not reached the session yet.
	srv.Exec(t, `UPDATE agents SET status = 'revoked' WHERE id = ?`, c.Identity.AgentID)
	srv.Clock.Advance(20 * day)
	// The refusal may or may not outrun the close, and the close may even land before
	// the renew is written: what matters is that it is not a renewal.
	// A failed send means the stream is already closing; the wait below still
	// requires the session to end, and to end as revoked.
	if _, err := io.WriteString(control, `{"t":"renew"}`+"\n"); err == nil {
		select {
		case line, ok := <-answers.lines:
			if ok {
				var got wire.RenewRefused
				if err := json.Unmarshal(line, &got); err != nil || got.T != wire.TypeRenewRefused || got.Code != wire.ReasonRevoked {
					t.Fatalf("answer = %s, want renew_refused revoked", line)
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the server neither answered nor closed the session")
		}
	}
	testsupport.Await(t, "the session to be closed", s.Done())
	if got := s.Reason(); got != tunnel.DisconnectRevoked {
		t.Errorf("the session ended with %q, want %q", got, tunnel.DisconnectRevoked)
	}
	var n int
	if err := srv.Store.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'agent.cert_renewed'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d agent.cert_renewed rows for a revoked agent (%v)", n, err)
	}
}

func TestABurstOfRenewsMakesOneCertificateAndOneAuditRow(t *testing.T) {
	srv := quiet(t)
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	srv.Events.WaitLen(t, 1)
	control := c.OpenControl(t)
	answers := readControlLines(t, control)

	srv.Clock.Advance(20 * day)
	for range 20 {
		askToRenew(t, control)
	}
	answers.next()
	// The control stream is read in order: once a heartbeat sent after the twenty
	// renews has been read, all twenty were dispatched, and the count below is of
	// what they made.
	tunneltest.SendHeartbeat(t, control, true, 903)
	awaitStatus(t, srv.Hub.Session(c.Identity.AgentID), "the heartbeat after the renews", func(st tunnel.SessionStatus) bool { return st.ClockMs == 903 })
	var n int
	if err := srv.Store.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'agent.cert_renewed'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d agent.cert_renewed rows after twenty renews, want 1 (%v)", n, err)
	}
}

func TestARenewFromAnAgentWhoseVerifierFailsIsAnsweredInternalAndRetryable(t *testing.T) {
	f := newFixture(t, true, func(addr string) string { return "https://" + addr })
	f.v.renewErr = errors.New("the database is busy")
	c := f.dial(t, hs{})
	if c.welcome == nil {
		t.Fatalf("no welcome: %+v", c)
	}
	if _, err := f.hub.AwaitSession(testsupport.Context(t), c.welcome.AgentID, func(s *tunnel.Session) bool { return s != nil }); err != nil {
		t.Fatalf("waiting for the session: %v", err)
	}
	st, err := c.mux.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write([]byte{wire.StreamKindControl}); err != nil {
		t.Fatal(err)
	}
	answers := readControlLines(t, st)
	askToRenew(t, st)
	var got wire.RenewRefused
	if err := json.Unmarshal(answers.next(), &got); err != nil || got.T != wire.TypeRenewRefused || got.Code != wire.RenewInternal {
		t.Fatalf("answer = %+v, %v; want renew_refused internal_error", got, err)
	}
}

// --- addresses and pins pushed to connected agents (D-19, D-06) ---

const (
	endpointOld = "wss://old.example.com/agent/connect"
	endpointNew = "wss://new.example.com/bayplan/agent/connect"
	testPin     = "sha256/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
)

// withEndpoint starts a server whose hub tells agents to dial the endpoint in *cur,
// which a test changes while agents are connected.
func withEndpoint(t *testing.T, cur *atomic.Value) *tunneltest.Server {
	t.Helper()
	return tunneltest.Start(t, tunneltest.Options{Configure: func(h *tunnel.Hub) *tunnel.Hub {
		return h.WithTiming(10*time.Second, 1000*day).
			WithEndpoint(func() string { s, _ := cur.Load().(string); return s }).
			WithPins(func() []string { return []string{testPin} })
	}})
}

func readURLUpdate(t *testing.T, lines *controlLines) wire.URLUpdate {
	t.Helper()
	var u wire.URLUpdate
	if err := json.Unmarshal(lines.next(), &u); err != nil || u.T != wire.TypeURLs {
		t.Fatalf("message = %+v, %v; want a urls message", u, err)
	}
	return u
}

func TestTheWelcomeCarriesTheEndpointAgentsAreToldToDial(t *testing.T) {
	var cur atomic.Value
	cur.Store(endpointOld)
	srv := withEndpoint(t, &cur)
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	if got := c.Welcome.URLs; len(got) != 1 || got[0] != endpointOld {
		t.Fatalf("welcome URLs = %v, want [%s]", got, endpointOld)
	}
	if got := c.Welcome.Pins; len(got) != 1 || got[0] != testPin {
		t.Errorf("welcome pins = %v, want [%s]", got, testPin)
	}
}

func TestPushURLsWritesTheNewAddressFirstAndThePinsToEveryConnectedAgent(t *testing.T) {
	var cur atomic.Value
	cur.Store(endpointOld)
	srv := withEndpoint(t, &cur)
	var streams []*controlLines
	for _, name := range []string{"A", "B", "C"} {
		c := srv.Enrol(t, srv.NewEnvironment(t, name))
		streams = append(streams, readControlLines(t, c.OpenControl(t)))
	}
	srv.Events.WaitLen(t, 3)
	for _, s := range streams {
		s.none("nothing is pushed until an address changes")
	}

	cur.Store(endpointNew)
	if n := srv.Hub.PushURLs([]string{endpointNew}, srv.Hub.Pins()); n != 3 {
		t.Fatalf("PushURLs wrote to %d sessions, want 3", n)
	}
	for i, s := range streams {
		u := readURLUpdate(t, s)
		if len(u.URLs) != 1 || u.URLs[0] != endpointNew || len(u.Pins) != 1 || u.Pins[0] != testPin {
			t.Errorf("agent %d was sent %+v", i, u)
		}
	}
}

func TestPushURLsSkipsASessionWithNoControlStreamAndNeverFails(t *testing.T) {
	var cur atomic.Value
	cur.Store(endpointOld)
	srv := withEndpoint(t, &cur)
	srv.Enrol(t, srv.NewEnvironment(t, "NoControl"))
	srv.Events.WaitLen(t, 1)
	if n := srv.Hub.PushURLs([]string{endpointNew}, nil); n != 0 {
		t.Fatalf("PushURLs wrote to %d sessions that never opened a control stream", n)
	}
}

func TestPushURLsWithNoAddressesWritesNothing(t *testing.T) {
	var cur atomic.Value
	cur.Store(endpointOld)
	srv := withEndpoint(t, &cur)
	c := srv.Enrol(t, srv.NewEnvironment(t, "A"))
	lines := readControlLines(t, c.OpenControl(t))
	srv.Events.WaitLen(t, 1)
	if n := srv.Hub.PushURLs(nil, []string{testPin}); n != 0 {
		t.Fatalf("PushURLs wrote to %d sessions with no address to give", n)
	}
	lines.none("an empty address list is not a message")
}

func TestAnAgentThatOpensItsControlStreamAfterTheAddressChangedIsToldAtOnce(t *testing.T) {
	var cur atomic.Value
	cur.Store(endpointOld)
	srv := withEndpoint(t, &cur)
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production")) // welcomed with the old address
	srv.Events.WaitLen(t, 1)

	cur.Store(endpointNew) // changed after the welcome, before the control stream
	lines := readControlLines(t, c.OpenControl(t))
	u := readURLUpdate(t, lines)
	if len(u.URLs) != 1 || u.URLs[0] != endpointNew {
		t.Fatalf("the late control stream was sent %+v, want the new address", u)
	}
}

func TestAnAgentWelcomedWithTheCurrentAddressIsNotToldAgainWhenItOpensItsControlStream(t *testing.T) {
	var cur atomic.Value
	cur.Store(endpointOld)
	srv := withEndpoint(t, &cur)
	c := srv.Enrol(t, srv.NewEnvironment(t, "Production"))
	srv.Events.WaitLen(t, 1)
	lines := readControlLines(t, c.OpenControl(t))
	lines.none("the welcome already carried the current address")
}

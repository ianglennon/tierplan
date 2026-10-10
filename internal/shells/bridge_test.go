// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package shells

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// The bridge between a WebSocket and a session's process, over a real HTTP server and
// the library the server uses on both ends. The process is a fake the test drives.

type rig struct {
	reg    *Registry
	sess   *Session
	exec   *fakeExec
	client *websocket.Conn
	reason chan string
	cancel context.CancelFunc
}

// newRig starts a bridge for a claimed session and dials it. ctl tunes the bridge.
func newRig(t *testing.T, ctl Controls, exec Exec) *rig {
	t.Helper()
	// A test that gives the bridge a clock gives the registry the same one.
	r := &rig{reg: NewRegistry(ctl.Clock), reason: make(chan string, 1)}
	if fe, ok := exec.(*fakeExec); ok {
		r.exec = fe
	}
	r.sess = r.reg.Create(newSession("ann"), exec, 0)
	if _, err := r.reg.Claim(r.sess.ID, "user:ann", "sign-in-ann"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	t.Cleanup(cancel)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c, err := websocket.Accept(w, req, nil)
		if err != nil {
			return
		}
		reason := Bridge(ctx, c, r.sess, ctl)
		r.reg.End(r.sess.ID, reason)
		r.reason <- reason
	}))
	t.Cleanup(srv.Close)
	dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dcancel()
	c, _, err := websocket.Dial(dctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil) //nolint:bodyclose // a WebSocket upgrade response has no body to close
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	r.client = c
	t.Cleanup(func() { _ = c.CloseNow() })
	return r
}

// gotReason waits for Bridge to return and gives its reason.
func (r *rig) gotReason(t *testing.T) string {
	t.Helper()
	select {
	case reason := <-r.reason:
		return reason
	case <-time.After(15 * time.Second):
		t.Fatal("Bridge did not return")
		return ""
	}
}

// readUntil reads binary frames until they hold want.
func (r *rig) readUntil(t *testing.T, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var got bytes.Buffer
	for !strings.Contains(got.String(), want) {
		typ, data, err := r.client.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %q: %v (got %q)", want, err, got.String())
		}
		if typ == websocket.MessageBinary {
			got.Write(data)
		}
	}
}

// control reads frames until a text frame arrives, and decodes it.
func (r *rig) control(t *testing.T) Control {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		typ, data, err := r.client.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for a control frame: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var c Control
		if err := json.Unmarshal(data, &c); err != nil {
			t.Fatalf("control frame %q: %v", data, err)
		}
		return c
	}
}

// drain reads until the socket ends, so the client answers the server's close frame as
// a browser does; a client that does not holds the server's close for five seconds.
func (r *rig) drain() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := r.client.Read(ctx); err != nil {
			return
		}
	}
}

func (r *rig) send(t *testing.T, typ websocket.MessageType, data string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.client.Write(ctx, typ, []byte(data)); err != nil {
		t.Fatalf("send %q: %v", data, err)
	}
}

// waitFor polls until cond holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOutputReachesTheBrowserAsBinaryAndInputReachesTheProcess(t *testing.T) {
	x := newFakeExec()
	r := newRig(t, Controls{}, x)

	go x.Print([]byte("prompt$ "))
	r.readUntil(t, "prompt$ ")
	r.send(t, websocket.MessageBinary, "ls\n")
	r.send(t, websocket.MessageBinary, "pwd\n")
	waitFor(t, "the input to reach the process", func() bool { return x.written() == "ls\npwd\n" })
}

func TestTheProcessEndingSendsTheExitCodeAndClosesTheSocketNormally(t *testing.T) {
	x := newFakeExec()
	r := newRig(t, Controls{}, x)
	go x.Print([]byte("bye\r\n"))
	r.readUntil(t, "bye")

	x.End(2)
	c := r.control(t)
	if c.Type != "exit" || c.Code == nil || *c.Code != 2 {
		t.Fatalf("frame = %+v, want exit with code 2", c)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := r.client.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Errorf("the socket ended with %v, want a normal close", err)
	}
	if got := r.gotReason(t); got != ReasonExited {
		t.Errorf("reason = %q, want exited", got)
	}
	if x.closeCount() != 1 {
		t.Errorf("the process was closed %d times, want once", x.closeCount())
	}
}

func TestAnExitCodeOfZeroIsSentAsZero(t *testing.T) {
	x := newFakeExec()
	r := newRig(t, Controls{}, x)
	x.End(0)
	c := r.control(t)
	if c.Type != "exit" || c.Code == nil || *c.Code != 0 {
		t.Fatalf("frame = %+v, want exit with code 0 present", c)
	}
}

func TestTheBrowserClosingFirstEndsTheProcessWithReasonDisconnected(t *testing.T) {
	x := newFakeExec()
	r := newRig(t, Controls{}, x)
	go x.Print([]byte("up"))
	r.readUntil(t, "up")

	_ = r.client.CloseNow()
	if got := r.gotReason(t); got != ReasonDisconnected {
		t.Fatalf("reason = %q, want disconnected", got)
	}
	if x.closeCount() != 1 {
		t.Errorf("the process was closed %d times, want once", x.closeCount())
	}
	res := r.sess.Result()
	if !res.StillRunning || res.ExitCode != nil {
		t.Errorf("result = %+v, want still running: the process was not seen to end (D-30)", res)
	}
}

func TestACancelledContextEndsTheBridgeAndTellsTheBrowser(t *testing.T) {
	x := newFakeExec()
	r := newRig(t, Controls{}, x)
	go x.Print([]byte("up"))
	r.readUntil(t, "up")

	r.cancel()
	if got := r.gotReason(t); got != ReasonEnded {
		t.Fatalf("reason = %q, want ended", got)
	}
	if x.closeCount() != 1 {
		t.Errorf("the process was closed %d times, want once", x.closeCount())
	}
}

func TestASessionEndedFromOutsideTellsTheBrowserItsReason(t *testing.T) {
	for _, reason := range []string{"server_shutdown", "ended_by_admin"} {
		t.Run(reason, func(t *testing.T) {
			x := newFakeExec()
			r := newRig(t, Controls{}, x)
			go x.Print([]byte("up"))
			r.readUntil(t, "up")

			if _, ok := r.reg.End(r.sess.ID, reason); !ok {
				t.Fatal("End reported false")
			}
			c := r.control(t)
			if c.Type != "closed" || c.Reason != reason {
				t.Fatalf("frame = %+v, want closed with reason %q", c, reason)
			}
			r.drain()
			if got := r.gotReason(t); got != reason {
				t.Errorf("Bridge returned %q, want %q", got, reason)
			}
		})
	}
}

func TestAnUnknownTextFrameAndAMalformedResizeAreIgnored(t *testing.T) {
	x := newFakeExec()
	r := newRig(t, Controls{}, x)

	for _, frame := range []string{
		`{"type":"hello"}`,
		`not json at all`,
		`{"type":"resize","cols":0,"rows":24}`,
		`{"type":"resize","cols":80,"rows":0}`,
		`{"type":"resize","cols":5000,"rows":24}`,
		`{"type":"resize","cols":80,"rows":1001}`,
		`{"type":"resize","cols":-5,"rows":24}`,
		`{"type":"resize"}`,
	} {
		r.send(t, websocket.MessageText, frame)
	}
	// A good one after them: it is the only one to reach the process, and the session
	// is alive to take it.
	r.send(t, websocket.MessageText, `{"type":"resize","cols":100,"rows":30}`)
	waitFor(t, "the good resize", func() bool { return len(x.sizes()) == 1 })
	if got := x.sizes(); got[0] != [2]uint{100, 30} {
		t.Errorf("resizes = %v, want only 100x30", got)
	}
	r.send(t, websocket.MessageBinary, "still alive\n")
	waitFor(t, "input after the ignored frames", func() bool { return x.written() == "still alive\n" })
}

func TestAMissingExecutableIsReportedAsNoShell(t *testing.T) {
	x := newFakeExec()
	x.waitErr = dockerx.ErrNoShell
	r := newRig(t, Controls{}, x)
	x.End(127)
	c := r.control(t)
	if c.Type != "closed" || c.Reason != ReasonNoShell {
		t.Fatalf("frame = %+v, want closed with reason no_shell", c)
	}
	r.drain()
	if got := r.gotReason(t); got != ReasonNoShell {
		t.Errorf("reason = %q, want no_shell", got)
	}
}

// floodExec prints endlessly and counts what the bridge took from it.
type floodExec struct {
	taken  atomic.Int64
	closed atomic.Bool
	done   chan struct{}
	once   sync.Once
}

func newFloodExec() *floodExec { return &floodExec{done: make(chan struct{})} }

func (f *floodExec) Read(p []byte) (int, error) {
	if f.closed.Load() {
		return 0, errors.New("closed")
	}
	for i := range p {
		p[i] = 'x'
	}
	f.taken.Add(int64(len(p)))
	return len(p), nil
}
func (f *floodExec) Write(p []byte) (int, error) { return len(p), nil }
func (f *floodExec) Close() error {
	f.closed.Store(true)
	f.once.Do(func() { close(f.done) })
	return nil
}
func (f *floodExec) Resize(context.Context, uint, uint) error { return nil }
func (f *floodExec) Wait(context.Context) (dockerx.ExecState, error) {
	return dockerx.ExecState{Running: true}, nil
}
func (f *floodExec) Inspect(context.Context) (dockerx.ExecState, error) {
	return dockerx.ExecState{Running: true}, nil
}

func TestABrowserThatStopsReadingBlocksTheShellAndTheWriteTimeoutEndsTheSession(t *testing.T) {
	x := newFloodExec()
	r := newRig(t, Controls{WriteTimeout: 300 * time.Millisecond}, x)

	// The client never reads. The shell would print without end; what the bridge takes
	// from it is only what fits in the buffers between them, never a growing queue.
	if got := r.gotReason(t); got != ReasonDisconnected {
		t.Fatalf("reason = %q, want disconnected", got)
	}
	const bound = 64 << 20
	if taken := x.taken.Load(); taken > bound {
		t.Errorf("the bridge took %d bytes from a shell nobody was reading, want fewer than %d", taken, bound)
	}
	select {
	case <-x.done:
	default:
		t.Error("the process was not closed")
	}
}

func TestAnAnsweredPingKeepsASessionAndAnUnansweredOneEndsIt(t *testing.T) {
	x := newFakeExec()
	r := newRig(t, Controls{PingInterval: 40 * time.Millisecond, WriteTimeout: 200 * time.Millisecond}, x)

	// While the client reads, its library answers every ping: the session lives well
	// past several intervals.
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			if _, _, err := r.client.Read(ctx); err != nil {
				return
			}
		}
	}()
	select {
	case reason := <-r.reason:
		t.Fatalf("the session ended (%q) while the browser was answering pings", reason)
	case <-time.After(400 * time.Millisecond):
	}
	cancel()
	wg.Wait()

	// The client reads no more, so the next ping goes unanswered.
	if got := r.gotReason(t); got != ReasonDisconnected {
		t.Fatalf("reason = %q, want disconnected", got)
	}
}

// idleRig is a rig on a clock the test advances by hand, with a bridge that checks
// every few milliseconds of real time and an idle limit the test can change. A
// goroutine reads the socket the whole time, so a test can look for a frame, or for
// the absence of one, without a read timeout closing the connection.
type idleRig struct {
	*rig
	clk    *fakeClock
	limit  atomic.Int64 // nanoseconds
	frames chan Control

	mu     sync.Mutex
	output bytes.Buffer
}

func newIdleRig(t *testing.T, limit time.Duration) *idleRig {
	t.Helper()
	clk := newFakeClock()
	ir := &idleRig{clk: clk, frames: make(chan Control, 16)}
	ir.limit.Store(int64(limit))
	ir.rig = newRig(t, Controls{
		Clock: clk, TickInterval: 5 * time.Millisecond,
		Idle: func() time.Duration { return time.Duration(ir.limit.Load()) },
	}, newFakeExec())
	go func() {
		for {
			typ, data, err := ir.client.Read(context.Background())
			if err != nil {
				close(ir.frames)
				return
			}
			if typ == websocket.MessageBinary {
				ir.mu.Lock()
				ir.output.Write(data)
				ir.mu.Unlock()
				continue
			}
			var c Control
			if json.Unmarshal(data, &c) == nil {
				ir.frames <- c
			}
		}
	}()
	return ir
}

func (ir *idleRig) printed() string {
	ir.mu.Lock()
	defer ir.mu.Unlock()
	return ir.output.String()
}

// next waits for the next control frame.
func (ir *idleRig) next(t *testing.T) Control {
	t.Helper()
	select {
	case c, ok := <-ir.frames:
		if !ok {
			t.Fatal("the socket ended while a control frame was awaited")
		}
		return c
	case <-time.After(10 * time.Second):
		t.Fatal("no control frame arrived")
		return Control{}
	}
}

// none fails the test if a control frame arrives within d of real time.
func (ir *idleRig) none(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case c := <-ir.frames:
		t.Fatalf("an unexpected control frame: %+v", c)
	case <-time.After(d):
	}
}

func TestAShellIdleForTheLimitIsWarnedAMinuteBeforeAndThenClosedWithReasonIdle(t *testing.T) {
	ir := newIdleRig(t, 6*time.Minute)

	ir.clk.Advance(4*time.Minute + 59*time.Second)
	ir.none(t, 80*time.Millisecond)

	ir.clk.Advance(time.Second) // five minutes of silence: one minute left
	if c := ir.next(t); c.Type != "idle_warning" || c.Seconds != 60 {
		t.Fatalf("frame = %+v, want idle_warning with 60 seconds", c)
	}
	ir.none(t, 80*time.Millisecond) // the warning is sent once for a stretch of silence
	ir.clk.Advance(time.Minute)
	if c := ir.next(t); c.Type != "closed" || c.Reason != ReasonIdle || c.Minutes != 6 {
		t.Fatalf("frame = %+v, want closed with reason idle and the limit, 6 minutes", c)
	}
	if got := ir.gotReason(t); got != ReasonIdle {
		t.Errorf("reason = %q, want idle", got)
	}
	if ir.reg.CountForUser("user:ann") != 0 || ir.sess.Reason() != ReasonIdle {
		t.Errorf("the session is still held or ended for %q, want idle", ir.sess.Reason())
	}
	if ir.exec.closeCount() != 1 {
		t.Errorf("the process was closed %d times, want once", ir.exec.closeCount())
	}
}

func TestAKeystrokeAfterTheWarningStartsTheSilenceAfresh(t *testing.T) {
	ir := newIdleRig(t, 6*time.Minute)
	ir.clk.Advance(5 * time.Minute)
	if c := ir.next(t); c.Type != "idle_warning" {
		t.Fatalf("frame = %+v, want idle_warning", c)
	}
	ir.send(t, websocket.MessageBinary, "x")
	waitFor(t, "the keystroke to reach the process", func() bool { return ir.exec.written() == "x" })
	ir.clk.Advance(4*time.Minute + 59*time.Second) // nearly a stretch of silence again
	ir.none(t, 150*time.Millisecond)               // not closed, and no warning yet
	ir.clk.Advance(time.Second)
	if c := ir.next(t); c.Type != "idle_warning" {
		t.Fatalf("frame = %+v, want a second idle_warning after more silence", c)
	}
	ir.clk.Advance(time.Minute)
	if c := ir.next(t); c.Type != "closed" || c.Reason != ReasonIdle {
		t.Fatalf("frame = %+v, want closed idle", c)
	}
}

func TestOutputFromTheShellAlsoStartsTheSilenceAfresh(t *testing.T) {
	ir := newIdleRig(t, 6*time.Minute)
	ir.clk.Advance(5*time.Minute + 30*time.Second)
	if c := ir.next(t); c.Type != "idle_warning" {
		t.Fatalf("frame = %+v, want idle_warning", c)
	}
	go ir.exec.Print([]byte("tick\r\n"))
	waitFor(t, "the output", func() bool { return strings.Contains(ir.printed(), "tick") })
	ir.clk.Advance(4*time.Minute + 59*time.Second)
	ir.none(t, 150*time.Millisecond) // five minutes since the printing would be a warning
	ir.clk.Advance(time.Second)
	if c := ir.next(t); c.Type != "idle_warning" {
		t.Fatalf("frame = %+v, want a new idle_warning rather than a close", c)
	}
	ir.clk.Advance(time.Minute)
	if c := ir.next(t); c.Type != "closed" || c.Reason != ReasonIdle {
		t.Fatalf("frame = %+v, want closed idle", c)
	}
}

func TestTheIdleLimitIsReadOnEveryTickNotWhenTheShellOpened(t *testing.T) {
	ir := newIdleRig(t, 6*time.Minute)
	ir.clk.Advance(5 * time.Minute)
	if c := ir.next(t); c.Type != "idle_warning" {
		t.Fatalf("frame = %+v, want idle_warning", c)
	}
	ir.limit.Store(int64(60 * time.Minute)) // an admin raises it
	ir.clk.Advance(2 * time.Minute)
	ir.none(t, 150*time.Millisecond)
	ir.limit.Store(int64(6 * time.Minute)) // and lowers it below the 7 minutes of silence so far
	if c := ir.next(t); c.Type != "closed" || c.Reason != ReasonIdle {
		t.Fatalf("frame = %+v, want closed idle at the lowered limit", c)
	}
}

func TestAShellWithNoIdleLimitNeverGoesIdle(t *testing.T) {
	clk := newFakeClock()
	ir := &idleRig{clk: clk, frames: make(chan Control, 4)}
	ir.rig = newRig(t, Controls{Clock: clk, TickInterval: 5 * time.Millisecond}, newFakeExec())
	go func() {
		for {
			typ, data, err := ir.client.Read(context.Background())
			if err != nil {
				return
			}
			if typ == websocket.MessageText {
				var c Control
				if json.Unmarshal(data, &c) == nil {
					ir.frames <- c
				}
			}
		}
	}()
	clk.Advance(1000 * time.Hour)
	ir.none(t, 100*time.Millisecond)
}

func TestAShellWhoseUserLosesAccessIsClosedWithReasonAccessLostWithinATickAndKeepsNothingOpen(t *testing.T) {
	var allowed atomic.Bool
	allowed.Store(true)
	x := newFakeExec()
	r := newRig(t, Controls{TickInterval: 20 * time.Millisecond, StillAllowed: allowed.Load}, x)
	go x.Print([]byte("prompt$ "))
	r.readUntil(t, "prompt$ ")
	time.Sleep(100 * time.Millisecond) // several ticks while still allowed; a close now would be the frame read next

	allowed.Store(false)
	start := time.Now()
	c := r.control(t)
	if c.Type != "closed" || c.Reason != ReasonAccessLost {
		t.Fatalf("frame = %+v, want closed with reason access_lost", c)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the shell took %v to close after the access went", took)
	}
	r.drain()
	if got := r.gotReason(t); got != ReasonAccessLost {
		t.Errorf("reason = %q, want access_lost", got)
	}
	if r.sess.Reason() != ReasonAccessLost || x.closeCount() != 1 {
		t.Errorf("session ended for %q with %d closes of the process, want access_lost and one", r.sess.Reason(), x.closeCount())
	}
}

func TestAnAdminEndingTheSessionAndTheServersShutdownTellTheBrowserWhichItWas(t *testing.T) {
	for _, reason := range []string{ReasonEndedByAdmin, ReasonServerShutdown} {
		x := newFakeExec()
		r := newRig(t, Controls{}, x)
		go x.Print([]byte("up\r\n"))
		r.readUntil(t, "up")
		if reason == ReasonServerShutdown {
			r.reg.CloseAll(reason)
		} else {
			r.reg.End(r.sess.ID, reason)
		}
		c := r.control(t)
		if c.Type != "closed" || c.Reason != reason {
			t.Errorf("%s: frame = %+v, want closed with that reason", reason, c)
		}
		r.drain()
		if got := r.gotReason(t); got != reason {
			t.Errorf("%s: Bridge returned %q", reason, got)
		}
	}
}

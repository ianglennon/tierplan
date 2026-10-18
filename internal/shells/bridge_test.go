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
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The bridge between a WebSocket and a session's process, over a real HTTP server and
// the library the server uses on both ends. The process is a fake the test drives.

type rig struct {
	t      *testing.T
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
	return newRigDialing(t, ctl, exec, nil)
}

// newRigDialing is newRig with the options the browser's side dials with, for a test
// that watches what the server sends it.
func newRigDialing(t *testing.T, ctl Controls, exec Exec, dial *websocket.DialOptions) *rig {
	t.Helper()
	// A test that gives the bridge a clock gives the registry the same one.
	r := &rig{t: t, reg: NewRegistry(ctl.Clock), reason: make(chan string, 1)}
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
	c, _, err := websocket.Dial(testsupport.Context(t), "ws"+strings.TrimPrefix(srv.URL, "http"), dial) //nolint:bodyclose // a WebSocket upgrade response has no body to close
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
	return testsupport.Await(t, "Bridge to return", r.reason)
}

// readUntil reads binary frames until they hold want.
func (r *rig) readUntil(t *testing.T, want string) {
	t.Helper()
	ctx := testsupport.Context(t)
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
	ctx := testsupport.Context(t)
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
	ctx := testsupport.Context(r.t)
	for {
		if _, _, err := r.client.Read(ctx); err != nil {
			return
		}
	}
}

func (r *rig) send(t *testing.T, typ websocket.MessageType, data string) {
	t.Helper()
	ctx := testsupport.Context(t)
	if err := r.client.Write(ctx, typ, []byte(data)); err != nil {
		t.Fatalf("send %q: %v", data, err)
	}
}

func TestOutputReachesTheBrowserAsBinaryAndInputReachesTheProcess(t *testing.T) {
	x := newFakeExec()
	r := newRig(t, Controls{}, x)

	go x.Print([]byte("prompt$ "))
	r.readUntil(t, "prompt$ ")
	r.send(t, websocket.MessageBinary, "ls\n")
	r.send(t, websocket.MessageBinary, "pwd\n")
	x.await(t, "the input to reach the process", func() bool { return x.written() == "ls\npwd\n" })
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
	_, _, err := r.client.Read(testsupport.Context(t))
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
	x.await(t, "the good resize", func() bool { return len(x.sizes()) == 1 })
	if got := x.sizes(); got[0] != [2]uint{100, 30} {
		t.Errorf("resizes = %v, want only 100x30", got)
	}
	r.send(t, websocket.MessageBinary, "still alive\n")
	x.await(t, "input after the ignored frames", func() bool { return x.written() == "still alive\n" })
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
	// wall-clock: the write timeout is the behaviour under test (a client that stops reading ends the session once it passes); the test waits for the end on gotReason, so a longer window only delays it and a correct run cannot fail on it
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
	// The browser's side decides whether each ping is answered, and counts them. The
	// pinger sends one ping at a time and waits for its pong before the next, so
	// seeing the fifth ping proves the first four were answered and the session lived
	// through them: the pings are the clock, not a stretch of real time. The write
	// timeout is what an unanswered ping is given, so it is generous: the answers of
	// the first half must not be late on a loaded machine.
	var answer atomic.Bool
	answer.Store(true)
	pings := make(chan struct{}, 64)
	x := newFakeExec()
	// wall-clock: the write timeout is the behaviour under test (an unanswered ping ends the session after it), so the second half of the test lasts as long as it does; 2 s is long for the in-process pongs of the first half
	r := newRigDialing(t, Controls{PingInterval: 20 * time.Millisecond, WriteTimeout: 2 * time.Second}, x, &websocket.DialOptions{
		OnPingReceived: func(context.Context, []byte) bool {
			select {
			case pings <- struct{}{}:
			default:
			}
			return answer.Load()
		},
	})

	// The client reads for the whole test, which is what lets its library see, and
	// answer, the pings.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			if _, _, err := r.client.Read(ctx); err != nil {
				return
			}
		}
	}()

	for range 5 {
		testsupport.Await(t, "a ping to reach the browser", pings)
	}
	select {
	case reason := <-r.reason:
		t.Fatalf("the session ended (%q) while the browser was answering pings", reason)
	default:
	}

	// From here the browser reads on but no longer answers: the next ping goes
	// unanswered, and after the write timeout the bridge takes the browser for gone.
	answer.Store(false)
	if got := r.gotReason(t); got != ReasonDisconnected {
		t.Fatalf("reason = %q, want disconnected", got)
	}
}

// idleRig is a rig on a clock the test advances by hand, with a bridge whose checks
// the test turns by hand too (Controls.Ticks), and an idle limit the test can change.
// A goroutine reads the socket the whole time, so a test can look for a frame, or for
// the absence of one, without a read timeout closing the connection.
type idleRig struct {
	*rig
	clk     *fakeClock
	limit   atomic.Int64 // nanoseconds
	frames  chan Control
	ticks   chan time.Time
	checked chan struct{}

	mu     sync.Mutex
	output bytes.Buffer
	// outSig is signalled (without blocking) after each chunk of output reaches the client.
	outSig chan struct{}
}

func newIdleRig(t *testing.T, limit time.Duration) *idleRig {
	t.Helper()
	ir := startIdleRig(t, true)
	ir.limit.Store(int64(limit))
	return ir
}

// startIdleRig starts the rig; withIdle false gives the bridge no idle limit at all.
func startIdleRig(t *testing.T, withIdle bool) *idleRig {
	t.Helper()
	clk := newFakeClock()
	ir := &idleRig{clk: clk, frames: make(chan Control, 16), outSig: make(chan struct{}, 1), ticks: make(chan time.Time), checked: make(chan struct{}, 1)}
	ctl := Controls{Clock: clk, Ticks: ir.ticks, Checked: func() { ir.checked <- struct{}{} }}
	if withIdle {
		ctl.Idle = func() time.Duration { return time.Duration(ir.limit.Load()) }
	}
	ir.rig = newRig(t, ctl, newFakeExec())
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
				select {
				case ir.outSig <- struct{}{}:
				default:
				}
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

// awaitOutput waits until cond holds, re-checking it after each chunk of output that reaches
// the client.
func (ir *idleRig) awaitOutput(t *testing.T, what string, cond func() bool) {
	t.Helper()
	ctx := testsupport.Context(t)
	for !cond() {
		select {
		case <-ir.outSig:
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// tick turns the bridge's checks once and returns when that check is over (Controls.Checked),
// so the test may then move the clock or change the limit. A tick being taken only proves the
// check is starting: a test that moved the clock straight after it raced the check, which could
// read the new time, close the shell and leave the next tick untaken (CI run 39125458463).
func (ir *idleRig) tick(t *testing.T) {
	t.Helper()
	select {
	case ir.ticks <- time.Time{}:
	case <-testsupport.Context(t).Done():
		t.Fatal("the bridge took no tick")
	}
	select {
	case <-ir.checked:
	case <-testsupport.Context(t).Done():
		t.Fatal("the bridge's check did not finish")
	}
}

// advance moves the clock and has the bridge look at it.
func (ir *idleRig) advance(t *testing.T, d time.Duration) {
	t.Helper()
	ir.clk.Advance(d)
	ir.tick(t)
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
	case <-testsupport.Context(t).Done():
		t.Fatal("no control frame arrived")
		return Control{}
	}
}

// none fails the test if the last advance made the bridge send a control frame. It
// takes one more tick, which is taken only once the check of the advance is over and
// its frame, if any, is written; then the browser pings the server and waits for the
// pong. The pong is written after that frame on the same connection, and the browser's
// reading goroutine handles frames in order, so when the pong is in, a frame the check
// sent is in ir.frames already. There is no window to wait through.
func (ir *idleRig) none(t *testing.T) {
	t.Helper()
	ir.tick(t)
	if err := ir.client.Ping(testsupport.Context(t)); err != nil {
		t.Fatalf("ping the server: %v", err)
	}
	select {
	case c, ok := <-ir.frames:
		if !ok {
			t.Fatal("the socket ended while no control frame was expected")
		}
		t.Fatalf("an unexpected control frame: %+v", c)
	default:
	}
}

func TestAShellIdleForTheLimitIsWarnedAMinuteBeforeAndThenClosedWithReasonIdle(t *testing.T) {
	ir := newIdleRig(t, 6*time.Minute)

	ir.advance(t, 4*time.Minute+59*time.Second)
	ir.none(t)

	ir.advance(t, time.Second) // five minutes of silence: one minute left
	if c := ir.next(t); c.Type != "idle_warning" || c.Seconds != 60 {
		t.Fatalf("frame = %+v, want idle_warning with 60 seconds", c)
	}
	ir.none(t) // the warning is sent once for a stretch of silence
	ir.advance(t, time.Minute)
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
	ir.advance(t, 5*time.Minute)
	if c := ir.next(t); c.Type != "idle_warning" {
		t.Fatalf("frame = %+v, want idle_warning", c)
	}
	ir.send(t, websocket.MessageBinary, "x")
	ir.exec.await(t, "the keystroke to reach the process", func() bool { return ir.exec.written() == "x" })
	ir.advance(t, 4*time.Minute+59*time.Second) // nearly a stretch of silence again
	ir.none(t)                                  // not closed, and no warning yet
	ir.advance(t, time.Second)
	if c := ir.next(t); c.Type != "idle_warning" {
		t.Fatalf("frame = %+v, want a second idle_warning after more silence", c)
	}
	ir.advance(t, time.Minute)
	if c := ir.next(t); c.Type != "closed" || c.Reason != ReasonIdle {
		t.Fatalf("frame = %+v, want closed idle", c)
	}
}

func TestOutputFromTheShellAlsoStartsTheSilenceAfresh(t *testing.T) {
	ir := newIdleRig(t, 6*time.Minute)
	ir.advance(t, 5*time.Minute+30*time.Second)
	if c := ir.next(t); c.Type != "idle_warning" {
		t.Fatalf("frame = %+v, want idle_warning", c)
	}
	go ir.exec.Print([]byte("tick\r\n"))
	ir.awaitOutput(t, "the output", func() bool { return strings.Contains(ir.printed(), "tick") })
	ir.advance(t, 4*time.Minute+59*time.Second)
	ir.none(t) // five minutes since the printing would be a warning
	ir.advance(t, time.Second)
	if c := ir.next(t); c.Type != "idle_warning" {
		t.Fatalf("frame = %+v, want a new idle_warning rather than a close", c)
	}
	ir.advance(t, time.Minute)
	if c := ir.next(t); c.Type != "closed" || c.Reason != ReasonIdle {
		t.Fatalf("frame = %+v, want closed idle", c)
	}
}

func TestTheIdleLimitIsReadOnEveryTickNotWhenTheShellOpened(t *testing.T) {
	ir := newIdleRig(t, 6*time.Minute)
	ir.advance(t, 5*time.Minute)
	if c := ir.next(t); c.Type != "idle_warning" {
		t.Fatalf("frame = %+v, want idle_warning", c)
	}
	ir.limit.Store(int64(60 * time.Minute)) // an admin raises it
	ir.advance(t, 2*time.Minute)
	ir.none(t)
	ir.limit.Store(int64(6 * time.Minute)) // and lowers it below the 7 minutes of silence so far
	ir.tick(t)                             // the next check reads the new limit
	if c := ir.next(t); c.Type != "closed" || c.Reason != ReasonIdle {
		t.Fatalf("frame = %+v, want closed idle at the lowered limit", c)
	}
}

func TestAShellWithNoIdleLimitNeverGoesIdle(t *testing.T) {
	ir := startIdleRig(t, false)
	ir.advance(t, 1000*time.Hour)
	ir.none(t)
}

func TestAShellWhoseUserLosesAccessIsClosedWithReasonAccessLostWithinATickAndKeepsNothingOpen(t *testing.T) {
	var allowed atomic.Bool
	allowed.Store(true)
	x := newFakeExec()
	// The test sends the ticks, and StillAllowed reports each check it answers, so the test
	// knows a check is over before it changes the answer.
	ticks := make(chan time.Time)
	checked := make(chan struct{}, 16)
	ctx := testsupport.Context(t)
	tick := func() {
		t.Helper()
		select {
		case ticks <- time.Time{}:
		case <-ctx.Done():
			t.Fatal("the bridge took no tick")
		}
	}
	r := newRig(t, Controls{Ticks: ticks, StillAllowed: func() bool {
		ok := allowed.Load()
		checked <- struct{}{}
		return ok
	}}, x)
	go x.Print([]byte("prompt$ "))
	r.readUntil(t, "prompt$ ")
	// Several ticks while still allowed: each is checked, and none closes the shell, so a
	// close from here on is the frame read next.
	for range 3 {
		tick()
		select {
		case <-checked:
		case <-ctx.Done():
			t.Fatal("the bridge did not check the access on a tick")
		}
	}

	allowed.Store(false)
	tick() // the first tick after the access went closes the shell
	c := r.control(t)
	if c.Type != "closed" || c.Reason != ReasonAccessLost {
		t.Fatalf("frame = %+v, want closed with reason access_lost", c)
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

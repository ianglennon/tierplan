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
	r := &rig{reg: NewRegistry(nil), reason: make(chan string, 1)}
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
	c, resp, err := websocket.Dial(dctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = resp
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

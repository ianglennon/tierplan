// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package channel

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ianglennon/tierplan/proto/ingresswire"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// sessionConn dials a test server that runs session as its half of a bundle
// session, and returns the node's connection.
func sessionConn(t *testing.T, welcome ingresswire.Welcome, session func(ctx context.Context, c *websocket.Conn)) *Conn {
	t.Helper()
	s := newTestServer(t, script{welcome: welcome, session: session})
	o := Options{URL: s.url(), InstallPins: []string{s.pin()}, Version: "v0.2.0-test", Now: fixedNow}
	conn, err := Dial(testsupport.Context(t), o, connectCreds(t, testKey(t)))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return conn
}

// nodeMessages reads n text messages the node sent, parsed with the server's own
// parser, and reports them on out. A node message that is not one of the four
// types ends the read.
func nodeMessages(ctx context.Context, c *websocket.Conn, n int, out chan<- any) {
	for range n {
		_, data, err := c.Read(ctx)
		if err != nil {
			out <- err
			return
		}
		msg, err := ingresswire.ParseNodeMessage(data)
		if err != nil {
			out <- err
			return
		}
		out <- msg
	}
}

// noHealth is a health function that is never asked in a test that fires no tick.
func noHealth(context.Context) (bool, string) { return false, "" }

// TestServeAnswersAppliedAndRejected is the tracer: bundle frames arrive over the
// real handshake and the server reads applied, then rejected (D-15).
func TestServeAnswersAppliedAndRejected(t *testing.T) {
	frames := [][]byte{[]byte("frame-one\x00\x01"), []byte("frame-two")}
	got := make(chan any, 4)
	conn := sessionConn(t, testWelcome("i-1"), func(ctx context.Context, c *websocket.Conn) {
		for _, f := range frames {
			if err := c.Write(ctx, websocket.MessageBinary, f); err != nil {
				got <- err
				return
			}
		}
		nodeMessages(ctx, c, 2, got)
		_ = c.Close(websocket.StatusNormalClosure, "done")
	})

	type call struct {
		frame      []byte
		instanceID string
	}
	var calls []call
	answers := map[string][2]string{"frame-one\x00\x01": {"id-1", ""}, "frame-two": {"id-2", "key_mismatch"}}
	err := conn.Serve(testsupport.Context(t), SessionOptions{
		Handle: func(_ context.Context, frame []byte, instanceID string) (string, string) {
			calls = append(calls, call{bytes.Clone(frame), instanceID})
			a := answers[string(frame)]
			return a[0], a[1]
		},
		Health: noHealth,
	})
	var ce *ClosedError
	if !errors.As(err, &ce) {
		t.Fatalf("Serve error = %v, want a *ClosedError", err)
	}

	if len(calls) != 2 {
		t.Fatalf("handler calls = %d, want 2", len(calls))
	}
	for i, c := range calls {
		if !bytes.Equal(c.frame, frames[i]) || c.instanceID != "i-1" {
			t.Errorf("call %d = frame %q instance %q, want the frame unchanged and instance i-1", i, c.frame, c.instanceID)
		}
	}
	first := testsupport.Await(t, "the first answer", got)
	second := testsupport.Await(t, "the second answer", got)
	if a, ok := first.(ingresswire.Applied); !ok || a.ID != "id-1" || a.T != ingresswire.TypeApplied {
		t.Errorf("first answer = %#v, want applied id-1", first)
	}
	if r, ok := second.(ingresswire.Rejected); !ok || r.ID != "id-2" || r.Reason != "key_mismatch" || r.T != ingresswire.TypeRejected {
		t.Errorf("second answer = %#v, want rejected id-2 key_mismatch", second)
	}
}

func TestServeCutsLongReason(t *testing.T) {
	got := make(chan any, 2)
	conn := sessionConn(t, testWelcome("i-1"), func(ctx context.Context, c *websocket.Conn) {
		if err := c.Write(ctx, websocket.MessageBinary, []byte("frame")); err != nil {
			got <- err
			return
		}
		nodeMessages(ctx, c, 1, got)
		_ = c.Close(websocket.StatusNormalClosure, "done")
	})
	long := strings.Repeat("x", ingresswire.MaxReasonLen-1) + "éé" // the cut falls inside a character
	_ = conn.Serve(testsupport.Context(t), SessionOptions{
		Handle: func(context.Context, []byte, string) (string, string) { return "id-1", long },
		Health: noHealth,
	})
	r, ok := testsupport.Await(t, "the rejected answer", got).(ingresswire.Rejected)
	if !ok {
		t.Fatal("the server did not read a rejected message")
	}
	if len(r.Reason) > ingresswire.MaxReasonLen || r.Reason != strings.Repeat("x", ingresswire.MaxReasonLen-1) {
		t.Errorf("reason is %d bytes, want it cut to the %d-byte limit without splitting a character", len(r.Reason), ingresswire.MaxReasonLen)
	}
}

func TestServeReturnsClosedError(t *testing.T) {
	conn := sessionConn(t, testWelcome("i-1"), func(_ context.Context, c *websocket.Conn) {
		_ = c.Close(websocket.StatusNormalClosure, "bye")
	})
	err := conn.Serve(testsupport.Context(t), SessionOptions{
		Handle: func(context.Context, []byte, string) (string, string) { return "", "unexpected" },
		Health: noHealth,
	})
	var ce *ClosedError
	if !errors.As(err, &ce) {
		t.Fatalf("Serve error = %v, want a *ClosedError", err)
	}
	if ce.Reason != "bye" || ce.Status != int(websocket.StatusNormalClosure) {
		t.Errorf("ClosedError = %+v, want status 1000 and reason bye", *ce)
	}
	if !strings.Contains(ce.Error(), "bye") {
		t.Errorf("Error() = %q, want the reason in it", ce.Error())
	}
}

// TestServeTextFrameIsProtocolError: a text message after the welcome ends the
// session with a protocol error (D-13).
func TestServeTextFrameIsProtocolError(t *testing.T) {
	closed := make(chan string, 1)
	conn := sessionConn(t, testWelcome("i-1"), func(ctx context.Context, c *websocket.Conn) {
		if err := c.Write(ctx, websocket.MessageText, []byte(`{"t":"welcome"}`)); err != nil {
			closed <- err.Error()
			return
		}
		_, _, err := c.Read(ctx)
		var ce websocket.CloseError
		if errors.As(err, &ce) {
			closed <- ce.Reason
			return
		}
		closed <- "no close: " + err.Error()
	})
	err := conn.Serve(testsupport.Context(t), SessionOptions{
		Handle: func(context.Context, []byte, string) (string, string) {
			t.Error("the handler was given a text frame")
			return "", ""
		},
		Health: noHealth,
	})
	if err == nil || errors.As(err, new(*ClosedError)) {
		t.Fatalf("Serve error = %v, want a protocol error that is not a server close", err)
	}
	if reason := testsupport.Await(t, "the server's view of the close", closed); reason != ingresswire.CloseProtocolError {
		t.Errorf("the server saw close reason %q, want %q", reason, ingresswire.CloseProtocolError)
	}
}

// TestServeNeedsHandlerAndHealth: a session without them is a programming error,
// reported before anything is sent.
func TestServeNeedsHandlerAndHealth(t *testing.T) {
	conn := sessionConn(t, testWelcome("i-1"), func(ctx context.Context, c *websocket.Conn) {
		_, _, _ = c.Read(ctx)
	})
	t.Cleanup(func() { _ = conn.Close("test done") })
	if err := conn.Serve(testsupport.Context(t), SessionOptions{}); err == nil {
		t.Error("Serve with no handler succeeded")
	}
}

// fakeTicker is a SessionOptions.Ticker the test fires itself.
type fakeTicker struct {
	fire    chan time.Time
	asked   chan time.Duration
	stopped chan struct{}
}

func newFakeTicker() *fakeTicker {
	return &fakeTicker{fire: make(chan time.Time), asked: make(chan time.Duration, 4), stopped: make(chan struct{}, 4)}
}

func (f *fakeTicker) Ticker(d time.Duration) (<-chan time.Time, func()) {
	f.asked <- d
	return f.fire, func() { f.stopped <- struct{}{} }
}

// tick makes one health tick happen and returns when the session has taken it.
func (f *fakeTicker) tick(t *testing.T) {
	t.Helper()
	ctx := testsupport.Context(t)
	select {
	case f.fire <- time.Time{}:
	case <-ctx.Done():
		t.Fatal("the session never took the tick")
	}
}

// serveAsync runs Serve on its own goroutine and reports its error.
func serveAsync(ctx context.Context, conn *Conn, o SessionOptions) <-chan error {
	done := make(chan error, 1)
	go func() { done <- conn.Serve(ctx, o) }()
	return done
}

// serveHealth plays a server that reads n health messages and then closes.
func serveHealth(n int, out chan<- any) func(ctx context.Context, c *websocket.Conn) {
	return func(ctx context.Context, c *websocket.Conn) {
		nodeMessages(ctx, c, n, out)
		_ = c.Close(websocket.StatusNormalClosure, "done")
	}
}

func TestHealthOnWelcomeInterval(t *testing.T) {
	got := make(chan any, 4)
	conn := sessionConn(t, testWelcome("i-1"), serveHealth(2, got)) // healthMs 30000
	f := newFakeTicker()
	state := struct {
		ok      bool
		applied string
	}{true, "b-1"}
	done := serveAsync(testsupport.Context(t), conn, SessionOptions{
		Handle: func(context.Context, []byte, string) (string, string) { return "", "" },
		Health: func(context.Context) (bool, string) { return state.ok, state.applied },
		Ticker: f.Ticker,
	})
	if d := testsupport.Await(t, "the ticker request", f.asked); d != 30*time.Second {
		t.Errorf("ticker interval = %v, want the welcome's 30s", d)
	}

	f.tick(t)
	first := testsupport.Await(t, "the first health message", got)
	state.ok, state.applied = false, "b-2" // changed between ticks, read at the next one
	f.tick(t)
	second := testsupport.Await(t, "the second health message", got)

	if h, ok := first.(ingresswire.Health); !ok || h.T != ingresswire.TypeHealth || !h.TraefikOK || h.AppliedID != "b-1" {
		t.Errorf("first = %#v, want health traefikOk true appliedId b-1", first)
	}
	if h, ok := second.(ingresswire.Health); !ok || h.TraefikOK || h.AppliedID != "b-2" {
		t.Errorf("second = %#v, want health traefikOk false appliedId b-2", second)
	}
	if err := testsupport.Await(t, "Serve to return", done); !errors.As(err, new(*ClosedError)) {
		t.Errorf("Serve error = %v, want a *ClosedError", err)
	}
}

func TestHealthDefaultInterval(t *testing.T) {
	w := testWelcome("i-1")
	w.HealthMs = 0
	conn := sessionConn(t, w, serveHealth(0, make(chan any, 1)))
	f := newFakeTicker()
	_ = conn.Serve(testsupport.Context(t), SessionOptions{
		Handle: func(context.Context, []byte, string) (string, string) { return "", "" },
		Health: noHealth,
		Ticker: f.Ticker,
	})
	if d := testsupport.Await(t, "the ticker request", f.asked); d != ingresswire.HealthInterval {
		t.Errorf("ticker interval = %v, want ingresswire.HealthInterval %v", d, ingresswire.HealthInterval)
	}
}

func TestCloseClassification(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session func(ctx context.Context, c *websocket.Conn)
		status  int
		reason  string // empty: the error must not be a *ClosedError
	}{
		{"revoked", func(_ context.Context, c *websocket.Conn) {
			_ = c.Close(websocket.StatusPolicyViolation, ingresswire.CloseRevoked)
		}, int(websocket.StatusPolicyViolation), ingresswire.CloseRevoked},
		{"replaced", func(_ context.Context, c *websocket.Conn) {
			_ = c.Close(websocket.StatusPolicyViolation, ingresswire.CloseReplaced)
		}, int(websocket.StatusPolicyViolation), ingresswire.CloseReplaced},
		{"dropped connection", func(_ context.Context, c *websocket.Conn) { _ = c.CloseNow() }, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := sessionConn(t, testWelcome("i-1"), tc.session)
			err := conn.Serve(testsupport.Context(t), SessionOptions{
				Handle: func(context.Context, []byte, string) (string, string) { return "", "" },
				Health: noHealth,
			})
			var ce *ClosedError
			switch {
			case tc.reason == "" && errors.As(err, &ce):
				t.Errorf("Serve error = %v, want a network error that is not a *ClosedError", err)
			case tc.reason == "" && err == nil:
				t.Error("Serve returned no error for a dropped connection")
			case tc.reason != "" && !errors.As(err, &ce):
				t.Errorf("Serve error = %v, want a *ClosedError", err)
			case tc.reason != "" && (ce.Reason != tc.reason || ce.Status != tc.status):
				t.Errorf("ClosedError = %+v, want status %d and reason %q", *ce, tc.status, tc.reason)
			}
		})
	}
}

func TestTickerStopped(t *testing.T) {
	conn := sessionConn(t, testWelcome("i-1"), serveHealth(0, make(chan any, 1)))
	f := newFakeTicker()
	_ = conn.Serve(testsupport.Context(t), SessionOptions{
		Handle: func(context.Context, []byte, string) (string, string) { return "", "" },
		Health: noHealth,
		Ticker: f.Ticker,
	})
	select {
	case <-f.stopped:
	default:
		t.Error("the ticker's stop function was not called by the time Serve returned")
	}
}

// TestHealthIntervalIsBounded is IN-A-01: a welcome's healthMs is kept between a second and an hour, and a
// value that would overflow a Duration is capped instead of wrapping.
func TestHealthIntervalIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name     string
		healthMs int
		want     time.Duration
	}{
		{"none", 0, ingresswire.HealthInterval},
		{"negative", -5, ingresswire.HealthInterval},
		{"a millisecond", 1, time.Second},
		{"just under the floor", 999, time.Second},
		{"the floor", 1000, time.Second},
		{"ordinary", 45000, 45 * time.Second},
		{"the cap", 3_600_000, time.Hour},
		{"over the cap", 3_600_001, time.Hour},
		{"overflowing", 18446744073710, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := healthInterval(tc.healthMs); got != tc.want {
				t.Errorf("healthInterval(%d) = %v, want %v", tc.healthMs, got, tc.want)
			}
		})
	}

	// Serve uses it: a welcome that says healthMs 1 asks the ticker for a second.
	w := testWelcome("i-1")
	w.HealthMs = 1
	conn := sessionConn(t, w, serveHealth(0, make(chan any, 1)))
	f := newFakeTicker()
	_ = conn.Serve(testsupport.Context(t), SessionOptions{
		Handle: func(context.Context, []byte, string) (string, string) { return "", "" },
		Health: noHealth,
		Ticker: f.Ticker,
	})
	if d := testsupport.Await(t, "the ticker request", f.asked); d != time.Second {
		t.Errorf("ticker interval for healthMs 1 = %v, want the floor 1s", d)
	}
}

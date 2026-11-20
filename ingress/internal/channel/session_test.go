// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package channel

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

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
	first, second := <-got, <-got
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
	r, ok := (<-got).(ingresswire.Rejected)
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

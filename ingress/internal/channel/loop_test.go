// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package channel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ianglennon/tierplan/proto/ingresswire"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// clockReq is one wait the loop asked the fake clock for.
type clockReq struct {
	d    time.Duration
	fire func()
}

// fakeClock records the waits the loop asks for. An auto clock fires every wait at
// once, so the loop runs through its attempts without waiting; a manual one waits
// for the test to fire the request it read.
type fakeClock struct {
	auto bool
	reqs chan clockReq
}

func newFakeClock(auto bool) *fakeClock {
	return &fakeClock{auto: auto, reqs: make(chan clockReq, 512)}
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	if c.auto {
		ch <- time.Time{}
	}
	c.reqs <- clockReq{d: d, fire: func() { ch <- time.Time{} }}
	return ch
}

// delays reads the next n waits the loop asked for.
func (c *fakeClock) delays(t *testing.T, n int) []time.Duration {
	t.Helper()
	out := make([]time.Duration, 0, n)
	for i := range n {
		out = append(out, testsupport.Await(t, fmt.Sprintf("wait %d of the loop", i+1), c.reqs).d)
	}
	return out
}

// lineWriter turns each log record into one string on a channel. slog's text
// handler writes one record per Write.
type lineWriter chan string

func (w lineWriter) Write(p []byte) (int, error) {
	w <- strings.TrimSpace(string(p))
	return len(p), nil
}

// newLoopLog is a logger whose lines the test reads as they are written, with the
// time dropped so equal events give equal lines.
func newLoopLog() (*slog.Logger, lineWriter) {
	lines := make(lineWriter, 4096)
	h := slog.NewTextHandler(lines, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}})
	return slog.New(h), lines
}

// dialStep is one scripted result of a dial attempt.
type dialStep func(ctx context.Context) (*Conn, error)

func failWith(err error) dialStep {
	return func(context.Context) (*Conn, error) { return nil, err }
}

// scriptedDial plays the steps in order, then blocks until the context ends. calls
// counts the attempts; reachedEnd is closed when the steps ran out.
func scriptedDial(steps []dialStep) (dial func(ctx context.Context) (*Conn, error), calls *atomic.Int32, reachedEnd <-chan struct{}) {
	calls = new(atomic.Int32)
	end := make(chan struct{})
	dial = func(ctx context.Context) (*Conn, error) {
		n := int(calls.Add(1))
		if n <= len(steps) {
			return steps[n-1](ctx)
		}
		if n == len(steps)+1 {
			close(end)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return dial, calls, end
}

// startLoop runs Run on its own goroutine and reports its result.
func startLoop(ctx context.Context, o LoopOptions) <-chan error {
	done := make(chan error, 1)
	go func() { done <- Run(ctx, o) }()
	return done
}

// stopLoop cancels the loop and checks that it returns nil.
func stopLoop(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	if err := testsupport.Await(t, "Run to return", done); err != nil {
		t.Errorf("Run returned %v, want nil when its context ends", err)
	}
}

// loopSession is the session the loop serves when a dial step returns a real
// connection: nothing it is given is used.
var loopSession = SessionOptions{
	Handle: func(context.Context, []byte, string) (string, string) { return "", "" },
	Health: noHealth,
}

func loopOptions(dial func(context.Context) (*Conn, error), clock *fakeClock, log *slog.Logger) LoopOptions {
	return LoopOptions{Dial: dial, Session: loopSession, Clock: clock, Rand: func() float64 { return 0.5 }, Log: log}
}

// realSession is a dial step that connects to a real TLS test server whose
// session ends at once with the given close.
func realSession(t *testing.T, status websocket.StatusCode, reason string) dialStep {
	t.Helper()
	s := newTestServer(t, script{welcome: testWelcome("i-1"), session: func(_ context.Context, c *websocket.Conn) {
		_ = c.Close(status, reason)
	}})
	creds := connectCreds(t, testKey(t))
	return func(ctx context.Context) (*Conn, error) {
		return Dial(ctx, Options{URL: s.url(), InstallPins: []string{s.pin()}, Now: fixedNow}, creds)
	}
}

func TestLoopBacksOffOnFailure(t *testing.T) {
	steps := make([]dialStep, 9)
	for i := range steps {
		steps[i] = failWith(errors.New("connection refused"))
	}
	dial, _, reached := scriptedDial(steps)
	clock := newFakeClock(true)
	ctx, cancel := context.WithCancel(testsupport.Context(t))
	done := startLoop(ctx, loopOptions(dial, clock, nil))
	testsupport.Await(t, "the loop to use up the failures", reached)

	got := clock.delays(t, len(steps))
	step := BackoffMin
	for i, d := range got {
		if d < step/2 || d > step {
			t.Errorf("delay %d = %v, want within [%v, %v]", i+1, d, step/2, step)
		}
		if d > BackoffMax {
			t.Errorf("delay %d = %v, exceeds the %v cap", i+1, d, BackoffMax)
		}
		step = min(step*2, BackoffMax)
	}
	if last := got[len(got)-1]; last != 45*time.Second { // the jitter source is fixed at 0.5 of the upper half
		t.Errorf("last delay = %v, want 45s (the 60s step, jittered)", last)
	}
	stopLoop(t, cancel, done)
}

func TestLoopRetryAfterWins(t *testing.T) {
	steps := make([]dialStep, 0, 7)
	for range 5 {
		steps = append(steps, failWith(errors.New("connection refused")))
	}
	rate := func(after time.Duration) dialStep {
		return failWith(&RefusedError{Code: ingresswire.RefuseRateLimited, RetryAfter: after})
	}
	steps = append(steps, rate(time.Second), rate(10*time.Minute))
	dial, _, reached := scriptedDial(steps)
	clock := newFakeClock(true)
	ctx, cancel := context.WithCancel(testsupport.Context(t))
	done := startLoop(ctx, loopOptions(dial, clock, nil))
	testsupport.Await(t, "the loop to use up the failures", reached)

	got := clock.delays(t, 7)
	if got[5] != time.Second {
		t.Errorf("delay at failure 6 = %v, want exactly the 1s hint (the computed delay was %v)", got[5], 24*time.Second)
	}
	if got[6] != MaxRetryAfter {
		t.Errorf("delay at failure 7 = %v, want the hint capped at %v", got[6], MaxRetryAfter)
	}
	stopLoop(t, cancel, done)
}

func TestLoopAdminRefusalWaitsCap(t *testing.T) {
	for _, code := range []string{
		ingresswire.RefuseAuthRejected, ingresswire.RefuseWrongTokenKind,
		ingresswire.RefuseNodeAlreadyEnrolled, ingresswire.RefuseInsecureTransport,
	} {
		for _, hint := range []time.Duration{0, 2 * time.Second} {
			t.Run(fmt.Sprintf("%s/hint %v", code, hint), func(t *testing.T) {
				dial, _, reached := scriptedDial([]dialStep{failWith(&RefusedError{Code: code, Detail: "no", RetryAfter: hint})})
				clock := newFakeClock(true)
				log, lines := newLoopLog()
				ctx, cancel := context.WithCancel(testsupport.Context(t))
				done := startLoop(ctx, loopOptions(dial, clock, log))
				testsupport.Await(t, "the loop to retry", reached)

				want := BackoffMax
				if hint > 0 {
					want = hint // a server hint still wins
				}
				if got := clock.delays(t, 1)[0]; got != want {
					t.Errorf("delay = %v, want %v", got, want)
				}
				line := testsupport.Await(t, "the refusal's log line", lines)
				if !strings.Contains(line, code) || !strings.Contains(line, "administrator") {
					t.Errorf("log line %q, want the code %q and the word administrator", line, code)
				}
				stopLoop(t, cancel, done)
			})
		}
	}
}

func TestLoopInstanceMismatchWaitsCap(t *testing.T) {
	dial, _, reached := scriptedDial([]dialStep{failWith(fmt.Errorf("ingress channel: the welcome: %w", ingresswire.ErrInstanceMismatch))})
	clock := newFakeClock(true)
	log, lines := newLoopLog()
	ctx, cancel := context.WithCancel(testsupport.Context(t))
	done := startLoop(ctx, loopOptions(dial, clock, log))
	testsupport.Await(t, "the loop to retry", reached)

	if got := clock.delays(t, 1)[0]; got != BackoffMax {
		t.Errorf("delay = %v, want %v", got, BackoffMax)
	}
	line := testsupport.Await(t, "the log line", lines)
	if !strings.HasPrefix(line, "level=ERROR") || !strings.Contains(line, "misdirected TierPlan") {
		t.Errorf("log line %q, want an error naming a misdirected TierPlan", line)
	}
	stopLoop(t, cancel, done)
}

func TestLoopRevokedStopsDialing(t *testing.T) {
	for name, step := range map[string]func(t *testing.T) dialStep{
		"refused revoked": func(*testing.T) dialStep {
			return failWith(&RefusedError{Code: ingresswire.RefuseRevoked})
		},
		"refused node_revoked": func(*testing.T) dialStep {
			return failWith(&RefusedError{Code: ingresswire.RefuseNodeRevoked, RetryAfter: time.Second})
		},
		"session closed revoked": func(t *testing.T) dialStep {
			return realSession(t, websocket.StatusPolicyViolation, ingresswire.CloseRevoked)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dial, calls, _ := scriptedDial([]dialStep{step(t)})
			clock := newFakeClock(false)
			log, lines := newLoopLog()
			ctx, cancel := context.WithCancel(testsupport.Context(t))
			done := startLoop(ctx, loopOptions(dial, clock, log))

			first := testsupport.Await(t, "the first revoked line", lines)
			if !strings.HasPrefix(first, "level=ERROR") || !strings.Contains(first, "ingress node revoked: no longer connecting to TierPlan; serving the cached bundle until the service is removed") {
				t.Fatalf("first line = %q, want the revoked error", first)
			}
			for i := range 2 {
				req := testsupport.Await(t, "the next wait", clock.reqs)
				if req.d != RevokedLogInterval {
					t.Fatalf("wait %d = %v, want %v", i+1, req.d, RevokedLogInterval)
				}
				req.fire()
				if again := testsupport.Await(t, "a repeated revoked line", lines); again != first {
					t.Errorf("repeat %d = %q, want the same line %q", i+1, again, first)
				}
			}
			stopLoop(t, cancel, done)

			if n := calls.Load(); n != 1 {
				t.Errorf("Dial was called %d times, want 1: a revoked node dials no more", n)
			}
			select {
			case extra := <-lines:
				t.Errorf("an extra log line %q: want one line now and one per fired interval", extra)
			default:
			}
		})
	}
}

func TestLoopSessionResetsFailures(t *testing.T) {
	steps := make([]dialStep, 0, 5)
	for range 4 {
		steps = append(steps, failWith(errors.New("connection refused")))
	}
	steps = append(steps, realSession(t, websocket.StatusNormalClosure, "bye"))
	dial, _, reached := scriptedDial(steps)
	clock := newFakeClock(true)
	ctx, cancel := context.WithCancel(testsupport.Context(t))
	done := startLoop(ctx, loopOptions(dial, clock, nil))
	testsupport.Await(t, "the loop to use up the steps", reached)

	got := clock.delays(t, 5)
	if got[3] != 6*time.Second {
		t.Errorf("delay after the fourth failure = %v, want 6s (the 8s step, jittered)", got[3])
	}
	if got[4] != 750*time.Millisecond {
		t.Errorf("delay after a session that was welcomed = %v, want 750ms: the count restarted at the 1s step", got[4])
	}
	stopLoop(t, cancel, done)
}

func TestLoopNeverExitsWhileContextLive(t *testing.T) {
	steps := make([]dialStep, 100)
	for i := range steps {
		steps[i] = failWith(errors.New("connection refused"))
	}
	dial, calls, reached := scriptedDial(steps)
	clock := newFakeClock(true)
	ctx, cancel := context.WithCancel(testsupport.Context(t))
	done := startLoop(ctx, loopOptions(dial, clock, nil))
	testsupport.Await(t, "the loop to use up the failures", reached)

	select {
	case err := <-done:
		t.Fatalf("Run returned %v after 100 failures with a live context", err)
	default:
	}
	if n := calls.Load(); n != 101 {
		t.Errorf("Dial was called %d times, want 101 (100 failures and the one in flight)", n)
	}
	for i, d := range clock.delays(t, 100) {
		if d > BackoffMax {
			t.Fatalf("delay %d = %v, exceeds the %v cap", i+1, d, BackoffMax)
		}
	}
	stopLoop(t, cancel, done)
}

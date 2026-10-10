// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package shells

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// fakeClock is a clock the test advances by hand; timers fire inside Advance.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
	fired   bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !t.stopped && !t.fired
		t.stopped = true
		return was
	}
}

// Advance moves the clock on and runs, in order and on this goroutine, the timers
// that fall due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*fakeTimer
	for _, t := range c.timers {
		if !t.stopped && !t.fired && !t.at.After(c.now) {
			t.fired = true
			due = append(due, t)
		}
	}
	c.mu.Unlock()
	for _, t := range due {
		t.f()
	}
}

// fakeExec is a process the test drives: bytes it prints are fed with Print, bytes
// written to it are kept, and End makes it exit.
type fakeExec struct {
	outR *io.PipeReader
	outW *io.PipeWriter

	mu      sync.Mutex
	got     bytes.Buffer
	resizes [][2]uint
	closes  int
	state   dockerx.ExecState
	waitErr error
}

func newFakeExec() *fakeExec {
	r, w := io.Pipe()
	return &fakeExec{outR: r, outW: w, state: dockerx.ExecState{Running: true}}
}

func (f *fakeExec) Read(p []byte) (int, error) { return f.outR.Read(p) }

func (f *fakeExec) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got.Write(p)
}

func (f *fakeExec) Close() error {
	f.mu.Lock()
	f.closes++
	f.mu.Unlock()
	_ = f.outR.CloseWithError(errors.New("closed"))
	return nil
}

func (f *fakeExec) Resize(_ context.Context, cols, rows uint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, [2]uint{cols, rows})
	return nil
}

func (f *fakeExec) Wait(context.Context) (dockerx.ExecState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.waitErr
}

func (f *fakeExec) Inspect(context.Context) (dockerx.ExecState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, nil
}

// Print makes the process print p; it blocks until the reader has taken it.
func (f *fakeExec) Print(p []byte) { _, _ = f.outW.Write(p) }

// End makes the process exit with code and its output end.
func (f *fakeExec) End(code int) {
	f.mu.Lock()
	f.state = dockerx.ExecState{ExitCode: code}
	f.mu.Unlock()
	_ = f.outW.Close()
}

func (f *fakeExec) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closes
}

func newSession(user string) *Session {
	return &Session{UserID: "user:" + user, AuthSession: "sign-in-" + user, EnvID: "env1", NodeID: "n1", ContainerID: "c1", ContainerName: "web"}
}

func TestACreatedSessionHasAnUnguessableIDAndIsClaimedByItsOwnerOnce(t *testing.T) {
	clk := newFakeClock()
	r := NewRegistry(clk)
	x := newFakeExec()
	s := r.Create(newSession("ann"), x, 0)
	if s == nil || len(s.ID) < 32 {
		t.Fatalf("session = %+v, want an ID of at least 32 characters", s)
	}
	other := r.Create(newSession("ann"), newFakeExec(), 0)
	if other.ID == s.ID {
		t.Fatal("two sessions share an ID")
	}
	if got := r.List(Filter{UserID: "user:ann"}); len(got) != 2 || got[0].State != StatePending {
		t.Fatalf("list = %+v, want two pending sessions", got)
	}

	if err := r.Check(s.ID, "env1", "user:ann", "sign-in-ann"); err != nil {
		t.Fatalf("Check: %v", err)
	}
	got, err := r.Claim(s.ID, "user:ann", "sign-in-ann")
	if err != nil || got != s {
		t.Fatalf("Claim = %v, %v", got, err)
	}
	if _, err := r.Claim(s.ID, "user:ann", "sign-in-ann"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a second claim = %v, want ErrNotFound: a session is single use", err)
	}
	if err := r.Check(s.ID, "env1", "user:ann", "sign-in-ann"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Check after a claim = %v, want ErrNotFound", err)
	}
	attached := 0
	for _, in := range r.List(Filter{UserID: "user:ann"}) {
		if in.ID == s.ID && in.State == StateAttached {
			attached++
		}
	}
	if attached != 1 {
		t.Errorf("the claimed session is not listed as attached: %+v", r.List(Filter{}))
	}
}

func TestAClaimByAnotherUserAnotherSignInOrAnotherEnvironmentIsTheSameNotFound(t *testing.T) {
	r := NewRegistry(newFakeClock())
	s := r.Create(newSession("ann"), newFakeExec(), 0)

	for name, c := range map[string]struct{ env, user, auth string }{
		"another user":        {"env1", "user:bob", "sign-in-ann"},
		"another sign-in":     {"env1", "user:ann", "sign-in-other"},
		"another environment": {"env2", "user:ann", "sign-in-ann"},
	} {
		if err := r.Check(s.ID, c.env, c.user, c.auth); !errors.Is(err, ErrNotFound) {
			t.Errorf("Check by %s = %v, want ErrNotFound", name, err)
		}
	}
	if _, err := r.Claim(s.ID, "user:bob", "sign-in-ann"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Claim by another user = %v, want ErrNotFound", err)
	}
	if _, err := r.Claim(s.ID, "user:ann", "sign-in-other"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Claim from another sign-in = %v, want ErrNotFound", err)
	}
	if _, err := r.Claim("nope", "user:ann", "sign-in-ann"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Claim of an unknown ID = %v, want ErrNotFound", err)
	}
	// The refused attempts used nothing up.
	if _, err := r.Claim(s.ID, "user:ann", "sign-in-ann"); err != nil {
		t.Errorf("the owner's claim after refused ones = %v", err)
	}
}

func TestEndClosesTheProcessReportsHowItEndedAndTellsTheHookOnce(t *testing.T) {
	clk := newFakeClock()
	r := NewRegistry(clk)
	var mu sync.Mutex
	type ended struct {
		id, reason string
		res        EndResult
	}
	var calls []ended
	r.OnEnd(func(s *Session, reason string, res EndResult) {
		mu.Lock()
		calls = append(calls, ended{s.ID, reason, res})
		mu.Unlock()
	})
	x := newFakeExec()
	s := r.Create(newSession("ann"), x, 0)
	if _, err := r.Claim(s.ID, "user:ann", "sign-in-ann"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(90 * time.Second)
	s.NoteExit(2)

	res, ok := r.End(s.ID, "exited")
	if !ok || res.Duration != 90*time.Second || res.ExitCode == nil || *res.ExitCode != 2 || res.StillRunning {
		t.Fatalf("End = %+v, %v, want 90 s and exit code 2", res, ok)
	}
	if x.closeCount() != 1 {
		t.Errorf("the process was closed %d times, want once", x.closeCount())
	}
	select {
	case <-s.Done():
	default:
		t.Error("Done is not closed after End")
	}
	if s.Reason() != "exited" {
		t.Errorf("Reason = %q", s.Reason())
	}
	if got := r.List(Filter{}); len(got) != 0 {
		t.Errorf("an ended session is still listed: %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0].id != s.ID || calls[0].reason != "exited" {
		t.Errorf("hook calls = %+v, want one", calls)
	}
}

func TestASessionEndedWithoutAnExitCodeIsStillRunning(t *testing.T) {
	r := NewRegistry(newFakeClock())
	s := r.Create(newSession("ann"), newFakeExec(), 0)
	res, ok := r.End(s.ID, "disconnected")
	if !ok || res.ExitCode != nil || !res.StillRunning {
		t.Fatalf("End = %+v, %v, want still running with no exit code (D-30)", res, ok)
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package shells

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/proto/testsupport"
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
	// onClose, when set, runs inside Close, before it returns.
	onClose func()
	// changed is signalled (without blocking) after each write, resize, print and end, so a
	// test waits on what the process saw instead of polling for it.
	changed chan struct{}
}

func newFakeExec() *fakeExec {
	r, w := io.Pipe()
	return &fakeExec{outR: r, outW: w, state: dockerx.ExecState{Running: true}, changed: make(chan struct{}, 1)}
}

func (f *fakeExec) Read(p []byte) (int, error) { return f.outR.Read(p) }

func (f *fakeExec) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	defer f.notify()
	return f.got.Write(p)
}

// notify wakes a test that is waiting in await.
func (f *fakeExec) notify() {
	select {
	case f.changed <- struct{}{}:
	default: // a wake-up is already waiting
	}
}

// await waits until cond holds, re-checking it after each write, resize, print or end of
// the process; the test's deadline (testsupport.Context) only guards against a hang.
func (f *fakeExec) await(t testing.TB, what string, cond func() bool) {
	t.Helper()
	ctx := testsupport.Context(t)
	for !cond() {
		select {
		case <-f.changed:
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func (f *fakeExec) Close() error {
	f.mu.Lock()
	f.closes++
	hook := f.onClose
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	_ = f.outR.CloseWithError(errors.New("closed"))
	return nil
}

func (f *fakeExec) Resize(_ context.Context, cols, rows uint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, [2]uint{cols, rows})
	f.notify()
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
func (f *fakeExec) Print(p []byte) {
	_, _ = f.outW.Write(p)
	f.notify()
}

// End makes the process exit with code and its output end.
func (f *fakeExec) End(code int) {
	f.mu.Lock()
	f.state = dockerx.ExecState{ExitCode: code}
	f.mu.Unlock()
	_ = f.outW.Close()
	f.notify()
}

func (f *fakeExec) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closes
}

func (f *fakeExec) written() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got.String()
}

func (f *fakeExec) sizes() [][2]uint {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]uint(nil), f.resizes...)
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

func TestAPendingSessionExpiresAtThirtySecondsAndItsProcessIsClosedOnce(t *testing.T) {
	clk := newFakeClock()
	r := NewRegistry(clk)
	var reasons []string
	r.OnEnd(func(_ *Session, reason string, _ EndResult) { reasons = append(reasons, reason) })
	x := newFakeExec()
	s := r.Create(newSession("ann"), x, 0)

	clk.Advance(29 * time.Second)
	if got := r.List(Filter{}); len(got) != 1 || x.closeCount() != 0 {
		t.Fatalf("at 29 s: %d sessions, process closed %d times, want the session still waiting", len(got), x.closeCount())
	}
	clk.Advance(2 * time.Second)
	if got := r.List(Filter{}); len(got) != 0 {
		t.Fatalf("at 31 s the session is still listed: %+v", got)
	}
	if x.closeCount() != 1 {
		t.Errorf("the process was closed %d times, want exactly once", x.closeCount())
	}
	if s.Reason() != "expired" || len(reasons) != 1 || reasons[0] != "expired" {
		t.Errorf("reason %q, hook reasons %v, want one expired", s.Reason(), reasons)
	}
	if _, err := r.Claim(s.ID, "user:ann", "sign-in-ann"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Claim after expiry = %v, want ErrNotFound", err)
	}
	if err := r.Check(s.ID, "env1", "user:ann", "sign-in-ann"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Check after expiry = %v, want ErrNotFound", err)
	}
}

func TestAClaimedSessionNoLongerExpires(t *testing.T) {
	clk := newFakeClock()
	r := NewRegistry(clk)
	x := newFakeExec()
	s := r.Create(newSession("ann"), x, 0)
	if _, err := r.Claim(s.ID, "user:ann", "sign-in-ann"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(10 * time.Minute)
	if got := r.List(Filter{}); len(got) != 1 || x.closeCount() != 0 {
		t.Fatalf("an attached session was ended by the pending clock: %+v, closed %d times", got, x.closeCount())
	}
}

func TestEndAnEndedSessionReturnsFalseAndDoesNothingMore(t *testing.T) {
	r := NewRegistry(newFakeClock())
	calls := 0
	r.OnEnd(func(*Session, string, EndResult) { calls++ })
	x := newFakeExec()
	s := r.Create(newSession("ann"), x, 0)

	if _, ok := r.End(s.ID, "disconnected"); !ok {
		t.Fatal("the first End reported false")
	}
	if _, ok := r.End(s.ID, "server_shutdown"); ok {
		t.Error("the second End reported true")
	}
	if s.Reason() != "disconnected" {
		t.Errorf("Reason = %q, want the first reason only", s.Reason())
	}
	if x.closeCount() != 1 || calls != 1 {
		t.Errorf("closed %d times, hook called %d times, want once each", x.closeCount(), calls)
	}
	if _, ok := r.End("never-existed", "x"); ok {
		t.Error("End of an unknown ID reported true")
	}
}

func TestCloseAllEndsPendingAndAttachedSessionsWithTheReasonAndRefusesNewOnes(t *testing.T) {
	r := NewRegistry(newFakeClock())
	pending, attached := newFakeExec(), newFakeExec()
	p := r.Create(newSession("ann"), pending, 0)
	a := r.Create(newSession("bob"), attached, 0)
	if _, err := r.Claim(a.ID, "user:bob", "sign-in-bob"); err != nil {
		t.Fatal(err)
	}

	if n := r.CloseAll("server_shutdown"); n != 2 {
		t.Fatalf("CloseAll ended %d sessions, want 2", n)
	}
	for name, s := range map[string]*Session{"pending": p, "attached": a} {
		if s.Reason() != "server_shutdown" {
			t.Errorf("%s session reason = %q", name, s.Reason())
		}
		select {
		case <-s.Done():
		default:
			t.Errorf("%s session is not done", name)
		}
	}
	if pending.closeCount() != 1 || attached.closeCount() != 1 {
		t.Errorf("processes closed %d and %d times, want once each", pending.closeCount(), attached.closeCount())
	}
	late := newFakeExec()
	if got := r.Create(newSession("ann"), late, 0); got != nil || late.closeCount() != 1 {
		t.Errorf("a session created while the server stops: %v, process closed %d times, want nil and a closed process", got, late.closeCount())
	}
}

// The HTTP server runs its shutdown hooks on goroutines it does not wait for, so
// an end can still be auditing when the server's Close calls CloseAll and then
// closes the database. CloseAll must wait for that end.
func TestCloseAllWaitsForAnEndStartedElsewhereToFinishItsAudit(t *testing.T) {
	r := NewRegistry(newFakeClock())
	s := r.Create(newSession("ann"), newFakeExec(), 0)
	entered, release := make(chan struct{}), make(chan struct{})
	r.OnEnd(func(*Session, string, EndResult) {
		close(entered)
		<-release
	})

	go r.End(s.ID, "server_shutdown")
	testsupport.Await(t, "the end to start auditing", entered)
	returned := make(chan struct{})
	go func() {
		r.CloseAll("server_shutdown")
		close(returned)
	}()
	// The registry has no signal for "CloseAll is now waiting": it blocks on a
	// condition variable with nothing observable around it, so there is nothing to
	// fence this absence on. The window can only miss a CloseAll that returns late; a
	// correct one cannot return before release, so it never fails a correct run.
	select {
	case <-returned:
		t.Fatal("CloseAll returned while another end was still auditing")
	case <-time.After(100 * time.Millisecond): // wall-clock: CloseAll cannot return before release on a correct registry, so this window can only false-pass, never fail a correct run
	}
	close(release)
	testsupport.Await(t, "CloseAll to return after the end finished", returned)
}

// A bridge whose read fails the moment the process closes must already be able to see
// that the session was ended from outside, and why, or it reads the failure as the
// process having exited and sends an exit code the process never gave.
func TestEndMarksTheSessionDoneBeforeItClosesTheProcess(t *testing.T) {
	r := NewRegistry(newFakeClock())
	x := newFakeExec()
	s := r.Create(newSession("ann"), x, 0)
	var doneAtClose bool
	var reasonAtClose string
	x.onClose = func() {
		select {
		case <-s.Done():
			doneAtClose = true
		default:
		}
		reasonAtClose = s.Reason()
	}

	r.End(s.ID, "server_shutdown")
	if !doneAtClose || reasonAtClose != "server_shutdown" {
		t.Errorf("at the moment the process was closed Done was closed = %v and the reason was %q, want true and server_shutdown", doneAtClose, reasonAtClose)
	}
}

func TestCountForUserCountsPendingAndAttachedSessionsAcrossEnvironmentsOfThatUserOnly(t *testing.T) {
	r := NewRegistry(newFakeClock())
	mk := func(user, env string) *Session {
		s := newSession(user)
		s.EnvID = env
		return r.Create(s, newFakeExec(), 0)
	}
	a1 := mk("ann", "env1")
	mk("ann", "env2")
	mk("bob", "env1")
	if _, err := r.Claim(a1.ID, "user:ann", "sign-in-ann"); err != nil {
		t.Fatal(err)
	}
	if got := r.CountForUser("user:ann"); got != 2 {
		t.Errorf("ann holds %d, want 2: one attached and one pending, in two environments", got)
	}
	if got := r.CountForUser("user:bob"); got != 1 {
		t.Errorf("bob holds %d, want 1", got)
	}
	if got := r.CountForUser("user:cy"); got != 0 {
		t.Errorf("cy holds %d, want 0", got)
	}
	r.End(a1.ID, "exited")
	if got := r.CountForUser("user:ann"); got != 1 {
		t.Errorf("ann holds %d after one ended, want 1", got)
	}
}

func TestCreateLimitedRefusesAtTheCapClosesTheProcessAndRegistersNothing(t *testing.T) {
	r := NewRegistry(newFakeClock())
	for range 2 {
		if s, err := r.CreateLimited(newSession("ann"), newFakeExec(), 0, 2); s == nil || err != nil {
			t.Fatalf("under the cap: %v, %v", s, err)
		}
	}
	x := newFakeExec()
	s, err := r.CreateLimited(newSession("ann"), x, 0, 2)
	if s != nil || !errors.Is(err, ErrLimit) {
		t.Fatalf("at the cap: %v, %v, want ErrLimit and no session", s, err)
	}
	if x.closeCount() != 1 {
		t.Errorf("the refused process was closed %d times, want once", x.closeCount())
	}
	if got := r.CountForUser("user:ann"); got != 2 {
		t.Errorf("ann holds %d, want the 2 from before", got)
	}
	// Another person is not held to ann's count, and a cap of zero means no cap.
	if s, err := r.CreateLimited(newSession("bob"), newFakeExec(), 0, 2); s == nil || err != nil {
		t.Errorf("bob: %v, %v", s, err)
	}
	if s, err := r.CreateLimited(newSession("ann"), newFakeExec(), 0, 0); s == nil || err != nil {
		t.Errorf("no cap: %v, %v", s, err)
	}
}

func TestOpensAtTheSameTimeCannotSlipUnderTheCap(t *testing.T) {
	r := NewRegistry(newFakeClock())
	const limit, tries = 3, 40
	var wg sync.WaitGroup
	var made, refused atomic.Int32
	for range tries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := r.CreateLimited(newSession("ann"), newFakeExec(), 0, limit)
			switch {
			case s != nil:
				made.Add(1)
			case errors.Is(err, ErrLimit):
				refused.Add(1)
			}
		}()
	}
	wg.Wait()
	if made.Load() != limit || refused.Load() != tries-limit || r.CountForUser("user:ann") != limit {
		t.Fatalf("made %d, refused %d, held %d; want %d, %d, %d", made.Load(), refused.Load(), r.CountForUser("user:ann"), limit, tries-limit, limit)
	}
}

func TestGetAndListNameTheOwnerAndGetIsGoneOnceTheSessionEnds(t *testing.T) {
	r := NewRegistry(newFakeClock())
	s := newSession("ann")
	s.Actor = audit.Actor{Type: audit.ActorUser, ID: "ann", Name: "ann"}
	s.EnvID, s.ContainerName = "env1", "web"
	s = r.Create(s, newFakeExec(), 0)

	in, ok := r.Get(s.ID)
	if !ok || in.Owner != "ann" || in.ContainerName != "web" || in.EnvID != "env1" || in.State != StatePending {
		t.Fatalf("Get = %+v, %v, want ann's pending shell in web", in, ok)
	}
	if got := r.List(Filter{EnvID: "env1"}); len(got) != 1 || got[0].Owner != "ann" {
		t.Errorf("List = %+v, want the one shell with its owner named", got)
	}
	if _, ok := r.Get("nope"); ok {
		t.Error("Get found a session that was never made")
	}
	r.End(s.ID, ReasonEndedByAdmin)
	if _, ok := r.Get(s.ID); ok {
		t.Error("Get still finds a session that ended")
	}
	if got := r.List(Filter{ContainerID: s.ContainerID}); len(got) != 0 {
		t.Errorf("List = %+v after the end, want none", got)
	}
}

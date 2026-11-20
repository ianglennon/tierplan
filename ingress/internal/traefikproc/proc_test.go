// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package traefikproc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// helperEnv marks the re-executed test binary that stands in for Traefik.
const helperEnv = "TRAEFIKPROC_HELPER"

// helperReport is one JSON line the stand-in writes: first its environment and arguments, then, once
// SIGTERM arrives, the word "terminated".
type helperReport struct {
	Env    []string `json:"env,omitempty"`
	Args   []string `json:"args,omitempty"`
	Signal string   `json:"signal,omitempty"`
}

// TestHelperProcess is the stand-in for Traefik. Without the marker it does nothing, so a normal run passes
// it. It registers for SIGTERM before it reports anything, so a SIGTERM the test sends after reading the
// first line is never lost.
func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(helperReport{Env: os.Environ(), Args: os.Args})
	<-term
	_ = enc.Encode(helperReport{Signal: "terminated"})
	os.Exit(0)
}

// helperArgs runs the helper case of this test binary. "--" ends the test binary's own flags, so what
// follows (Traefik's real arguments) reaches the helper untouched.
func helperArgs(rest ...string) []string {
	return append([]string{"-test.run=^TestHelperProcess$", "--"}, rest...)
}

// reports returns a file to use as the child's stdout and a channel of the lines the child writes to it.
// The parent's copy of the write end must be closed once the child holds its own, or the reader never ends.
func reports(t *testing.T) (*os.File, <-chan helperReport) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	out := make(chan helperReport, 8)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			var h helperReport
			if json.Unmarshal(sc.Bytes(), &h) == nil {
				out <- h
			}
		}
	}()
	return w, out
}

// countingStart is the real starter that counts how many children it started.
func countingStart(n *atomic.Int32) starter {
	return func(binary string, args, env []string, stdout, stderr io.Writer) (process, error) {
		n.Add(1)
		return realStart(binary, args, env, stdout, stderr)
	}
}

func TestEnvIsOnlyPathAndArgsAreTheStaticConfig(t *testing.T) {
	if got, want := Env(), []string{"PATH=/usr/local/bin:/usr/bin:/bin"}; !slices.Equal(got, want) {
		t.Fatalf("Env() = %q, want %q", got, want)
	}
	if got, want := DefaultArgs(), []string{"--configFile=/var/lib/tierplan-ingress/static/traefik.yml"}; !slices.Equal(got, want) {
		t.Fatalf("DefaultArgs() = %q, want %q", got, want)
	}
	if ingressbundle.NodeRoot != "/var/lib/tierplan-ingress" {
		t.Fatalf("NodeRoot = %q: the argument above no longer names the static file", ingressbundle.NodeRoot)
	}
	p := New(Options{})
	if !slices.Equal(p.o.Env, Env()) || !slices.Equal(p.o.Args, DefaultArgs()) || p.o.Binary != Binary {
		t.Fatalf("New did not default the binary, arguments and environment: %+v", p.o)
	}
}

// The child starts with exactly the minimal environment (the helper's own marker aside) and Traefik's
// arguments, and reports both from its own side.
func TestStartMinimalEnvAndArgs(t *testing.T) {
	out, lines := reports(t)
	var starts atomic.Int32
	p := New(Options{
		Binary: os.Args[0],
		Args:   helperArgs(DefaultArgs()...),
		Env:    append(Env(), helperEnv+"=1"),
		Stdout: out,
		start:  countingStart(&starts),
	})
	t.Cleanup(func() { _ = p.Stop() })
	if err := p.Start(testsupport.Context(t)); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	got := testsupport.Await(t, "the child's report", lines)
	if want := []string{"PATH=/usr/local/bin:/usr/bin:/bin", helperEnv + "=1"}; !slices.Equal(got.Env, want) {
		t.Fatalf("child environment = %q, want exactly %q", got.Env, want)
	}
	if last := got.Args[len(got.Args)-1]; last != "--configFile=/var/lib/tierplan-ingress/static/traefik.yml" {
		t.Fatalf("child's last argument = %q, want the static config", last)
	}
	if !p.Running() {
		t.Fatal("Running() is false after Start")
	}
	if p.PID() <= 0 || p.PID() == os.Getpid() {
		t.Fatalf("PID() = %d, want the child's pid", p.PID())
	}
}

// Stop forwards SIGTERM, the child exits on it, Stop returns, and nothing restarts the child.
func TestStopForwardsSIGTERM(t *testing.T) {
	out, lines := reports(t)
	var starts atomic.Int32
	p := New(Options{
		Binary: os.Args[0],
		Args:   helperArgs(DefaultArgs()...),
		Env:    append(Env(), helperEnv+"=1"),
		Stdout: out,
		start:  countingStart(&starts),
	})
	t.Cleanup(func() { _ = p.Stop() })
	if err := p.Start(testsupport.Context(t)); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	testsupport.Await(t, "the child to be ready", lines)

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := testsupport.Await(t, "the child's SIGTERM report", lines); got.Signal != "terminated" {
		t.Fatalf("child report after Stop = %+v, want it to have seen SIGTERM", got)
	}
	if p.Running() {
		t.Fatal("Running() is true after Stop")
	}
	if n := starts.Load(); n != 1 {
		t.Fatalf("the child was started %d times, want once: a stop must not restart it", n)
	}
}

// world is everything a timing test controls: a clock that records the delay it is asked for and fires only
// when the test says so, a reaper whose wait4 returns the exits the test queues, and a starter that hands
// out fake children. No real process, no real time.
type world struct {
	t      *testing.T
	timers chan timerReq
	starts chan *fakeProc
	sigchl chan os.Signal
	log    *syncBuffer

	mu      sync.Mutex
	exits   map[int][]syscall.WaitStatus
	nextPid int
	offset  time.Duration
	failing bool
}

type timerReq struct {
	d    time.Duration
	fire chan time.Time
}

type fakeProc struct {
	pid  int
	sigs chan os.Signal
}

func (f *fakeProc) Pid() int { return f.pid }
func (f *fakeProc) Signal(s os.Signal) error {
	f.sigs <- s
	return nil
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newWorld(t *testing.T) *world {
	t.Helper()
	return &world{
		t:       t,
		timers:  make(chan timerReq, 64),
		starts:  make(chan *fakeProc, 64),
		sigchl:  make(chan os.Signal, 64),
		log:     &syncBuffer{},
		exits:   map[int][]syscall.WaitStatus{},
		nextPid: 100,
	}
}

// After records the requested delay and returns a channel the test fires.
func (w *world) After(d time.Duration) <-chan time.Time {
	fire := make(chan time.Time, 1)
	w.timers <- timerReq{d: d, fire: fire}
	return fire
}

func (w *world) start(string, []string, []string, io.Writer, io.Writer) (process, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failing {
		return nil, errors.New("exec format error")
	}
	w.nextPid++
	f := &fakeProc{pid: w.nextPid, sigs: make(chan os.Signal, 8)}
	w.starts <- f
	return f, nil
}

func (w *world) wait4(pid int, ws *syscall.WaitStatus, _ int, _ *syscall.Rusage) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	q := w.exits[pid]
	if len(q) == 0 {
		return 0, nil
	}
	*ws = q[0]
	w.exits[pid] = q[1:]
	return pid, nil
}

func (w *world) now() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return time.Unix(1_000_000, 0).Add(w.offset)
}

// advance moves the clock the supervisor measures a run with.
func (w *world) advance(d time.Duration) {
	w.mu.Lock()
	w.offset += d
	w.mu.Unlock()
}

// exit makes f exit with the given code and raises the SIGCHLD the reaper waits for.
func (w *world) exit(f *fakeProc, code int) {
	w.mu.Lock()
	w.exits[f.pid] = append(w.exits[f.pid], syscall.WaitStatus(code<<8))
	w.mu.Unlock()
	w.sigchl <- syscall.SIGCHLD
}

// proc starts a Proc on the world and returns it with its first child.
func (w *world) proc() (*Proc, *fakeProc) {
	w.t.Helper()
	p := New(Options{
		Binary: "traefik",
		Clock:  w,
		Log:    slog.New(slog.NewTextHandler(w.log, nil)),
		start:  w.start,
		reap:   newReaper(w.wait4, false),
		sigs:   w.sigchl,
		now:    w.now,
	})
	if err := p.Start(testsupport.Context(w.t)); err != nil {
		w.t.Fatal(err)
	}
	return p, testsupport.Await(w.t, "the first child to start", w.starts)
}

func (w *world) noStart() {
	w.t.Helper()
	select {
	case f := <-w.starts:
		w.t.Fatalf("a child (pid %d) was started when none should have been", f.pid)
	default:
	}
}

// delayStep is the unjittered restart step after the nth consecutive failure.
func delayStep(failures int) time.Duration {
	step := RestartMin
	for i := 1; i < failures && step < RestartMax; i++ {
		step *= 2
	}
	return min(step, RestartMax)
}

// wantDelay fails unless d is in the jittered upper half of step: never below half of it, never above it, and
// so never above RestartMax.
func wantDelay(t *testing.T, d, step time.Duration) {
	t.Helper()
	if d < step/2 || d > step || d > RestartMax {
		t.Fatalf("restart delay = %v, want between %v and %v (and at most %v)", d, step/2, step, RestartMax)
	}
}

// A child that dies is not restarted before the delay fires, is restarted after, and the delay grows with each
// consecutive failure up to RestartMax and never beyond it.
func TestRestartBackoff(t *testing.T) {
	w := newWorld(t)
	p, child := w.proc()
	for failures := 1; failures <= 8; failures++ {
		w.exit(child, 1)
		req := testsupport.Await(t, "a restart delay", w.timers)
		wantDelay(t, req.d, delayStep(failures))
		w.noStart()
		if p.Running() {
			t.Fatal("Running() is true while the child is down")
		}
		req.fire <- time.Time{}
		child = testsupport.Await(t, "the restarted child", w.starts)
	}
	if got := delayStep(8); got != RestartMax {
		t.Fatalf("the test never reached the ceiling: step %v, want %v", got, RestartMax)
	}
}

// A child that ran at least RestartMax before it died starts the count again.
func TestRestartCountResets(t *testing.T) {
	w := newWorld(t)
	_, child := w.proc()
	for failures := 1; failures <= 2; failures++ {
		w.exit(child, 1)
		req := testsupport.Await(t, "a restart delay", w.timers)
		wantDelay(t, req.d, delayStep(failures))
		req.fire <- time.Time{}
		child = testsupport.Await(t, "the restarted child", w.starts)
	}
	w.advance(RestartMax)
	w.exit(child, 1)
	req := testsupport.Await(t, "a restart delay", w.timers)
	wantDelay(t, req.d, delayStep(1))
}

// An exit is logged once, at warn level, with its status and the delay to the restart.
func TestExitLoggedOnceAtWarn(t *testing.T) {
	w := newWorld(t)
	_, child := w.proc()
	w.exit(child, 3)
	testsupport.Await(t, "a restart delay", w.timers)
	var lines []string
	for _, l := range strings.Split(w.log.String(), "\n") {
		if strings.Contains(l, "traefik exited") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("%d lines say the child exited, want one: %q", len(lines), w.log.String())
	}
	for _, want := range []string{"level=WARN", "exit status 3", "restart_in="} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("exit line %q lacks %q", lines[0], want)
		}
	}
}

// A start that fails after the first run is logged and retried after a delay, not fatal.
func TestFailedRestartIsRetried(t *testing.T) {
	w := newWorld(t)
	_, child := w.proc()
	w.mu.Lock()
	w.failing = true
	w.mu.Unlock()
	w.exit(child, 1)
	req := testsupport.Await(t, "the first restart delay", w.timers)
	req.fire <- time.Time{}
	req = testsupport.Await(t, "the delay after the failed start", w.timers)
	wantDelay(t, req.d, delayStep(2))
	w.mu.Lock()
	w.failing = false
	w.mu.Unlock()
	req.fire <- time.Time{}
	testsupport.Await(t, "the child, once starting works", w.starts)
}

// Stop on a child that ignores SIGTERM asks the clock for exactly SupervisorStopWait, sends SIGKILL only when
// that fires, and returns once the exit is in.
func TestStopKillsAfterWait(t *testing.T) {
	w := newWorld(t)
	p, child := w.proc()
	stopped := make(chan error, 1)
	go func() { stopped <- p.Stop() }()

	if got := testsupport.Await(t, "SIGTERM", child.sigs); got != syscall.SIGTERM {
		t.Fatalf("first signal = %v, want SIGTERM", got)
	}
	req := testsupport.Await(t, "the stop window", w.timers)
	if req.d != ingressbundle.SupervisorStopWait {
		t.Fatalf("stop window = %v, want %v", req.d, ingressbundle.SupervisorStopWait)
	}
	select {
	case s := <-child.sigs:
		t.Fatalf("%v was sent before the stop window ended", s)
	default:
	}
	req.fire <- time.Time{}
	if got := testsupport.Await(t, "SIGKILL", child.sigs); got != syscall.SIGKILL {
		t.Fatalf("second signal = %v, want SIGKILL", got)
	}
	w.exit(child, 0)
	if err := testsupport.Await(t, "Stop to return", stopped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// A child that exits on SIGTERM is never sent SIGKILL.
func TestStopNoKillWhenExited(t *testing.T) {
	w := newWorld(t)
	p, child := w.proc()
	stopped := make(chan error, 1)
	go func() { stopped <- p.Stop() }()

	testsupport.Await(t, "SIGTERM", child.sigs)
	w.exit(child, 0)
	if err := testsupport.Await(t, "Stop to return", stopped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case s := <-child.sigs:
		t.Fatalf("%v was sent to a child that had exited", s)
	default:
	}
}

// An exit during a stop is not restarted, and neither is a stop during the restart delay.
func TestNoRestartDuringStop(t *testing.T) {
	w := newWorld(t)
	p, child := w.proc()
	stopped := make(chan error, 1)
	go func() { stopped <- p.Stop() }()
	testsupport.Await(t, "SIGTERM", child.sigs)
	w.exit(child, 0)
	if err := testsupport.Await(t, "Stop to return", stopped); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	w.noStart()
	if p.Running() {
		t.Fatal("Running() is true after Stop")
	}

	w2 := newWorld(t)
	p2, child2 := w2.proc()
	w2.exit(child2, 1)
	testsupport.Await(t, "a restart delay", w2.timers)
	if err := p2.Stop(); err != nil {
		t.Fatalf("Stop during the restart delay: %v", err)
	}
	w2.noStart()
}

func TestStopBeforeStartIsNil(t *testing.T) {
	if err := New(Options{}).Stop(); err != nil {
		t.Fatalf("Stop on a Proc that never started = %v, want nil", err)
	}
}

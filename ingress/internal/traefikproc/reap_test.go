// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package traefikproc

import (
	"errors"
	"sync"
	"syscall"
	"testing"

	"github.com/ianglennon/tierplan/proto/testsupport"
)

const traefikPid = 77

// scriptedWait is a wait4 that returns a fixed list of results and records every call.
type scriptedWait struct {
	mu      sync.Mutex
	results []waitResult
	calls   []waitCall
}

type waitResult struct {
	pid int
	ws  syscall.WaitStatus
	err error
}

type waitCall struct{ pid, options int }

func (s *scriptedWait) wait4(pid int, ws *syscall.WaitStatus, options int, _ *syscall.Rusage) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, waitCall{pid, options})
	if len(s.results) == 0 {
		return 0, nil
	}
	r := s.results[0]
	s.results = s.results[1:]
	*ws = r.ws
	return r.pid, r.err
}

func (s *scriptedWait) recorded() []waitCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]waitCall(nil), s.calls...)
}

func (w *watch) exited() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

func watchTraefik(t *testing.T, r *reaper) *watch {
	t.Helper()
	_, w, err := r.spawn(func() (process, error) { return &fakeProc{pid: traefikPid}, nil })
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// As PID 1 the reaper collects an orphan it was handed, delivers the Traefik status once and keeps going
// until wait4 has nothing left.
func TestReapAsPID1(t *testing.T) {
	sw := &scriptedWait{results: []waitResult{
		{pid: 4242, ws: syscall.WaitStatus(9 << 8)},
		{pid: traefikPid, ws: syscall.WaitStatus(5 << 8)},
		{pid: 0},
	}}
	r := newReaper(sw.wait4, true)
	w := watchTraefik(t, r)
	r.drain()

	calls := sw.recorded()
	if len(calls) != 3 {
		t.Fatalf("wait4 was called %d times, want 3 (orphan, Traefik, nothing left): %+v", len(calls), calls)
	}
	for _, c := range calls {
		if c.pid != -1 || c.options != syscall.WNOHANG {
			t.Fatalf("PID 1 wait4 call = %+v, want pid -1 with WNOHANG", c)
		}
	}
	testsupport.Await(t, "the Traefik exit", w.done)
	if w.status.ExitStatus() != 5 {
		t.Fatalf("delivered status = %d, want Traefik's 5, not the orphan's 9", w.status.ExitStatus())
	}
	r.drain() // nothing left: must not deliver a second time or panic on a second close
}

// An orphan's status alone is not Traefik's exit.
func TestReapDiscardsOrphans(t *testing.T) {
	sw := &scriptedWait{results: []waitResult{{pid: 4242}, {pid: 4243}, {pid: 0}}}
	r := newReaper(sw.wait4, true)
	w := watchTraefik(t, r)
	r.drain()
	if w.exited() {
		t.Fatal("an orphan's exit was delivered as Traefik's")
	}
	if n := len(sw.recorded()); n != 3 {
		t.Fatalf("wait4 was called %d times, want 3", n)
	}
}

// With no children at all (ECHILD) the drain ends and delivers nothing.
func TestReapPID1StopsOnECHILD(t *testing.T) {
	sw := &scriptedWait{results: []waitResult{{pid: -1, err: syscall.ECHILD}}}
	r := newReaper(sw.wait4, true)
	w := watchTraefik(t, r)
	r.drain()
	if n := len(sw.recorded()); n != 1 {
		t.Fatalf("wait4 was called %d times after ECHILD, want 1", n)
	}
	if w.exited() {
		t.Fatal("ECHILD delivered an exit")
	}
}

// Not as PID 1 the reaper waits on the Traefik pid only: it must never take a status that is someone else's.
func TestReapNotPID1WaitsOnTraefikOnly(t *testing.T) {
	sw := &scriptedWait{results: []waitResult{{pid: 0}, {pid: traefikPid, ws: syscall.WaitStatus(2 << 8)}}}
	r := newReaper(sw.wait4, false)
	w := watchTraefik(t, r)
	r.drain()
	if w.exited() {
		t.Fatal("an exit was delivered while wait4 reported none")
	}
	r.drain()
	testsupport.Await(t, "the Traefik exit", w.done)
	r.drain() // the watch is cleared, so there is nothing to wait on
	calls := sw.recorded()
	if len(calls) != 2 {
		t.Fatalf("wait4 was called %d times, want 2: %+v", len(calls), calls)
	}
	for _, c := range calls {
		if c.pid != traefikPid || c.options != syscall.WNOHANG {
			t.Fatalf("wait4 call = %+v, want only the Traefik pid %d with WNOHANG", c, traefikPid)
		}
	}
}

// A child that is already gone when the non-PID-1 reaper asks is reported as exited with no status, so the
// supervisor restarts it instead of waiting on a pid that will never answer.
func TestReapNotPID1ECHILDIsAnExit(t *testing.T) {
	sw := &scriptedWait{results: []waitResult{{pid: -1, err: syscall.ECHILD}}}
	r := newReaper(sw.wait4, false)
	w := watchTraefik(t, r)
	r.drain()
	testsupport.Await(t, "the Traefik exit", w.done)
	if !w.lost {
		t.Fatal("an exit without a status was not marked lost")
	}
}

// Once the reaper has recorded the exit, a signal is refused: the pid may belong to another process by now.
func TestNoSignalAfterExit(t *testing.T) {
	w := newWorld(t)
	p, child := w.proc()
	if err := p.signal(syscall.SIGHUP); err != nil {
		t.Fatalf("signal to a running child: %v", err)
	}
	if got := testsupport.Await(t, "the signal", child.sigs); got != syscall.SIGHUP {
		t.Fatalf("child got %v, want SIGHUP", got)
	}
	w.exit(child, 1)
	testsupport.Await(t, "the restart delay, which follows the recorded exit", w.timers)
	if err := p.signal(syscall.SIGTERM); !errors.Is(err, errExited) {
		t.Fatalf("signal after the exit = %v, want errExited", err)
	}
	select {
	case s := <-child.sigs:
		t.Fatalf("%v reached a child whose exit was recorded", s)
	default:
	}
}

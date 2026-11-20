// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package traefikproc

import (
	"context"
	"errors"
	"os"
	"sync"
	"syscall"
)

// watch is the exit of one child. done is closed exactly once, when the reaper has the child's status;
// status and lost are valid after that.
type watch struct {
	pid    int
	done   chan struct{}
	status syscall.WaitStatus
	// lost means the child was gone before its status could be read (wait4 said ECHILD), so status is empty.
	lost bool
}

// reaper owns every wait in the process (D-39). As PID 1 it is also the parent of every orphan in the
// container, so after each SIGCHLD it calls wait4(-1, WNOHANG) until nothing is left to collect, hands the
// Traefik child's status to the watch and throws every other status away. Not as PID 1 it waits on the
// Traefik pid only, so it never takes a status that belongs to someone else.
//
// One mutex covers both the wait4 calls and the signals sent to the child. A signal therefore either goes
// out before the child is reaped or is refused after, and never reaches a pid that has been reused.
type reaper struct {
	wait4  func(pid int, ws *syscall.WaitStatus, options int, ru *syscall.Rusage) (int, error)
	isPID1 bool

	mu  sync.Mutex
	cur *watch
}

func newReaper(wait4 func(int, *syscall.WaitStatus, int, *syscall.Rusage) (int, error), isPID1 bool) *reaper {
	return &reaper{wait4: wait4, isPID1: isPID1}
}

// spawn starts a child and registers it as the one to watch, as one step under the lock. A child that
// exits before the registration cannot be reaped as an orphan and lost: the drain waits for the lock.
func (r *reaper) spawn(start func() (process, error)) (process, *watch, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	proc, err := start()
	if err != nil {
		return nil, nil, err
	}
	w := &watch{pid: proc.Pid(), done: make(chan struct{})}
	r.cur = w
	return proc, w, nil
}

// signal sends sig to p unless w's exit has been recorded.
func (r *reaper) signal(w *watch, p process, sig os.Signal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-w.done:
		return errExited
	default:
	}
	return p.Signal(sig)
}

// loop drains after every signal on sigs until ctx ends.
func (r *reaper) loop(ctx context.Context, sigs <-chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-sigs:
			r.drain()
		}
	}
}

// drain collects every child that has exited, without blocking.
func (r *reaper) drain() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		target := -1
		if !r.isPID1 {
			if r.cur == nil {
				return
			}
			target = r.cur.pid
		}
		var ws syscall.WaitStatus
		pid, err := r.wait4(target, &ws, syscall.WNOHANG, nil)
		switch {
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.ECHILD):
			if !r.isPID1 && r.cur != nil {
				r.cur.lost = true
				r.finish()
			}
			return
		case err != nil || pid <= 0:
			return
		}
		if r.cur != nil && r.cur.pid == pid {
			r.cur.status = ws
			r.finish()
		}
		// Any other pid is an orphan that was reparented to PID 1: collected, and nothing else is done.
	}
}

// finish delivers the watched child's exit and stops watching it. The caller holds the lock.
func (r *reaper) finish() {
	close(r.cur.done)
	r.cur = nil
}

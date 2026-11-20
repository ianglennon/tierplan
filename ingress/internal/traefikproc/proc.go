// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package traefikproc runs Traefik as a child of the supervisor, which is PID 1 in the image (D-20,
// D-39). One reaper owns every wait: nothing here calls cmd.Wait, because a second waiter would race the
// reaper for the child's status and could signal a pid that has been reused.
package traefikproc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// Binary is where the image puts Traefik.
const Binary = "/usr/local/bin/traefik"

// The restart delay bounds (D-20): a crashed Traefik comes back after a delay that doubles from RestartMin
// to RestartMax.
const (
	RestartMin = time.Second
	RestartMax = 30 * time.Second
)

const minimalPath = "PATH=/usr/local/bin:/usr/bin:/bin"

// DefaultArgs is Traefik's command line: its static configuration, which the supervisor writes (D-20).
func DefaultArgs() []string {
	return []string{"--configFile=" + ingressbundle.NodeRoot + "/static/traefik.yml"}
}

// Env is the whole environment Traefik gets. The supervisor's own environment may hold the join token, and
// Traefik reads every TRAEFIK_* variable as configuration, so it is handed PATH and nothing else (D-39).
func Env() []string { return []string{minimalPath} }

// Clock is the time source for the restart delay and the stop window.
type Clock interface {
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// process is what the supervisor needs of a child: its pid and a way to signal it. Nothing waits on it; the
// reaper does.
type process interface {
	Pid() int
	Signal(os.Signal) error
}

// starter starts a child and returns it without waiting on it.
type starter func(binary string, args, env []string, stdout, stderr io.Writer) (process, error)

// osProc is a child started by os/exec. cmd.Wait is never called on it: the reaper owns the wait.
type osProc struct{ p *os.Process }

func (o osProc) Pid() int                 { return o.p.Pid }
func (o osProc) Signal(s os.Signal) error { return o.p.Signal(s) }

// Release frees the handle os.Process holds for the child (a pidfd on Linux). It is called once the reaper
// has recorded the exit, never before.
func (o osProc) Release() error { return o.p.Release() }

// realStart starts the child with exactly the environment it is given and only Start: the exit status is
// the reaper's. Stdout and Stderr are the supervisor's own files by default, which exec hands to the child
// as they are, so no copy goroutine runs.
func realStart(binary string, args, env []string, stdout, stderr io.Writer) (process, error) {
	cmd := exec.Command(binary, args...) //nolint:gosec // the binary is the image's Traefik, never request data
	// A nil Env would make exec pass the parent's whole environment on; an empty non-nil one passes nothing.
	cmd.Env = append([]string{}, env...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return osProc{p: cmd.Process}, nil
}

// Options configure New. The zero value runs the image's Traefik with its defaults.
type Options struct {
	// Binary is the executable. Empty means Binary.
	Binary string
	// Args are its arguments. Nil means DefaultArgs.
	Args []string
	// Env is its whole environment. Nil means Env.
	Env []string
	// Stdout and Stderr receive the child's output. Nil means the supervisor's own.
	Stdout, Stderr io.Writer
	// Clock times the restart delay and the stop window. Nil means the real clock.
	Clock Clock
	// Log receives the exit and restart lines. Nil means slog.Default.
	Log *slog.Logger
	// Rand feeds the restart jitter and returns a value in [0, 1). Nil means the process's random source.
	Rand func() float64

	// The rest are seams for the tests; nil means the real one.
	start starter
	reap  *reaper
	sigs  <-chan os.Signal
	now   func() time.Time
}

// Proc is the supervised Traefik child. Start it once and Stop it once; between the two it restarts a child
// that exits.
type Proc struct {
	o Options

	mu          sync.Mutex
	cur         *child
	supervising bool

	stopOnce sync.Once
	stopCh   chan struct{}
	done     chan struct{}
	stopErr  error
}

// child is one run of Traefik.
type child struct {
	proc      process
	w         *watch
	startedAt time.Time
}

// errExited is returned for a signal that was refused because the reaper has recorded the child's exit: the
// pid may already belong to another process.
var errExited = errors.New("traefikproc: the child has exited")

// New returns a Proc with the defaults filled in. It starts nothing.
func New(o Options) *Proc {
	if o.Binary == "" {
		o.Binary = Binary
	}
	if o.Args == nil {
		o.Args = DefaultArgs()
	}
	if o.Env == nil {
		o.Env = Env()
	}
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	if o.Clock == nil {
		o.Clock = realClock{}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.start == nil {
		o.start = realStart
	}
	if o.reap == nil {
		o.reap = newReaper(syscall.Wait4, os.Getpid() == 1)
	}
	if o.now == nil {
		o.now = time.Now
	}
	return &Proc{o: o, stopCh: make(chan struct{}), done: make(chan struct{})}
}

// Start starts the child and returns once it is running. It fails if the child cannot be started; after
// that the child is supervised in the background until Stop (or ctx ends, which is a Stop).
func (p *Proc) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.supervising {
		p.mu.Unlock()
		return errors.New("traefikproc: already started")
	}
	p.mu.Unlock()

	// Registered before the first start, so no SIGCHLD can be missed; the drain after the start covers a
	// child that exited before anything was listening.
	sigs := p.o.sigs
	stopNotify := func() {}
	if sigs == nil {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGCHLD)
		sigs = ch
		stopNotify = func() { signal.Stop(ch) }
	}
	// The reaper outlives ctx: a stop that ctx's end starts still needs the child's exit collected.
	rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	go p.o.reap.loop(rctx, sigs)

	c, err := p.spawn()
	if err != nil {
		cancel()
		stopNotify()
		return err
	}
	p.mu.Lock()
	p.supervising = true
	p.mu.Unlock()
	go func() {
		defer close(p.done)
		defer stopNotify()
		defer cancel()
		p.supervise(ctx, c)
	}()
	return nil
}

// spawn starts one child and registers it with the reaper.
func (p *Proc) spawn() (*child, error) {
	proc, w, err := p.o.reap.spawn(func() (process, error) {
		return p.o.start(p.o.Binary, p.o.Args, p.o.Env, p.o.Stdout, p.o.Stderr)
	})
	if err != nil {
		return nil, fmt.Errorf("start traefik: %w", err)
	}
	c := &child{proc: proc, w: w, startedAt: p.o.now()}
	p.mu.Lock()
	p.cur = c
	p.mu.Unlock()
	p.o.reap.drain()
	return c, nil
}

// supervise follows the child until a stop is requested.
func (p *Proc) supervise(ctx context.Context, c *child) {
	select {
	case <-c.w.done:
		release(c)
	case <-p.stopCh:
		p.stopErr = p.terminate(c)
	case <-ctx.Done():
		p.stopErr = p.terminate(c)
	}
}

// terminate is the D-21 stop: SIGTERM, then up to SupervisorStopWait for the exit, then SIGKILL.
func (p *Proc) terminate(c *child) error {
	defer release(c)
	if err := p.o.reap.signal(c.w, c.proc, syscall.SIGTERM); err != nil {
		if errors.Is(err, errExited) {
			return nil
		}
		p.o.Log.Warn("traefik did not take SIGTERM", "error", err)
	}
	select {
	case <-c.w.done:
		return nil
	case <-p.o.Clock.After(ingressbundle.SupervisorStopWait):
	}
	p.o.Log.Warn("traefik ignored SIGTERM, killing it", "after", ingressbundle.SupervisorStopWait)
	if err := p.o.reap.signal(c.w, c.proc, syscall.SIGKILL); err != nil && !errors.Is(err, errExited) {
		return fmt.Errorf("kill traefik: %w", err)
	}
	<-c.w.done
	return nil
}

// release frees what the child's handle holds, once its exit has been recorded.
func release(c *child) {
	select {
	case <-c.w.done:
	default:
		return
	}
	if r, ok := c.proc.(interface{ Release() error }); ok {
		_ = r.Release()
	}
}

// Stop asks the child to stop and returns once it has. A Stop on a Proc that never started returns nil.
func (p *Proc) Stop() error {
	p.mu.Lock()
	supervising := p.supervising
	p.mu.Unlock()
	if !supervising {
		return nil
	}
	p.stopOnce.Do(func() { close(p.stopCh) })
	<-p.done
	return p.stopErr
}

// signal sends sig to the current child unless the reaper has recorded its exit.
func (p *Proc) signal(sig os.Signal) error {
	p.mu.Lock()
	c := p.cur
	p.mu.Unlock()
	if c == nil {
		return errExited
	}
	return p.o.reap.signal(c.w, c.proc, sig)
}

// Running reports whether a child is running.
func (p *Proc) Running() bool { return p.PID() != 0 }

// PID is the pid of the running child, or 0 when none is.
func (p *Proc) PID() int {
	p.mu.Lock()
	c := p.cur
	p.mu.Unlock()
	if c == nil {
		return 0
	}
	select {
	case <-c.w.done:
		return 0
	default:
		return c.w.pid
	}
}

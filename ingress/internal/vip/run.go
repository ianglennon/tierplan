// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// KeepalivedBinary is where the image puts keepalived.
const KeepalivedBinary = "/usr/sbin/keepalived"

// KeepalivedArgs is keepalived's command line. -n keeps it in the foreground as the helper's child, -l logs to
// the console and -D logs in detail. Never the flag that keeps the address on exit: a stop must release the address, so the
// holder sends its priority-0 advert and the others take over without waiting for the advert timeout (D-15,
// T-26-30).
func KeepalivedArgs() []string { return []string{"-n", "-l", "-D", "-f", ConfPath} }

// The helper's times.
const (
	// StopWait is how long the helper waits for keepalived after forwarding SIGTERM before it kills it. It is
	// below the service's 10 s stop grace period (26-16), so the helper's own STOP line is written before
	// Docker would kill the container.
	StopWait = 8 * time.Second
	// fifoRetryEvery and fifoRetryFor bound the wait for keepalived to create its notify FIFO.
	fifoRetryEvery = 100 * time.Millisecond
	fifoRetryFor   = 10 * time.Second
)

// Process is a started child. Wait is called once, from one goroutine.
type Process interface {
	Pid() int
	Signal(os.Signal) error
	Wait() error
}

// execProc is a child started by os/exec.
type execProc struct{ cmd *exec.Cmd }

func (p execProc) Pid() int                 { return p.cmd.Process.Pid }
func (p execProc) Signal(s os.Signal) error { return p.cmd.Process.Signal(s) }
func (p execProc) Wait() error              { return p.cmd.Wait() }

// ExecStart starts name with args and gives its standard output and error to the two writers. The child gets
// the helper's environment: the vrrp_script runs `tierplan-ingress probe` as keepalived's child and reads
// the probe port from it, and the environment holds only the six TIERPLAN_VIP_* values (no credential: the
// key is a file keepalived reads, A-03).
func ExecStart(name string, args []string, stdout, stderr io.Writer) (Process, error) {
	cmd := exec.Command(name, args...) //nolint:gosec // the binary is the image's keepalived, never request data
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// A grandchild that keeps the output pipes open after keepalived has exited must not hold Wait forever.
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return execProc{cmd: cmd}, nil
}

// openFIFO opens keepalived's notify FIFO for reading. It is opened read-write so the open never blocks for
// a writer and a read never sees end of file when keepalived restarts; a missing FIFO is an error, never
// created here, because keepalived makes it.
func openFIFO(path string) (io.ReadCloser, error) {
	return os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // the fixed FIFOPath
}

// RunDeps are the effects Run uses. A zero field means the real one.
type RunDeps struct {
	Getenv     func(string) string
	Stdout     io.Writer
	Stderr     io.Writer
	Interfaces Lister
	// Start starts keepalived.
	Start func(name string, args []string, stdout, stderr io.Writer) (Process, error)
	Clock Clock
	// WriteFile writes the rendered configuration.
	WriteFile func(path string, data []byte, perm os.FileMode) error
	// OpenFIFO opens the notify FIFO.
	OpenFIFO func(path string) (io.ReadCloser, error)
}

func (d RunDeps) withDefaults() RunDeps {
	if d.Getenv == nil {
		d.Getenv = os.Getenv
	}
	if d.Stdout == nil {
		d.Stdout = os.Stdout
	}
	if d.Stderr == nil {
		d.Stderr = os.Stderr
	}
	if d.Interfaces == nil {
		d.Interfaces = SystemInterfaces
	}
	if d.Start == nil {
		d.Start = ExecStart
	}
	if d.Clock == nil {
		d.Clock = RealClock{}
	}
	if d.WriteFile == nil {
		d.WriteFile = os.WriteFile
	}
	if d.OpenFIFO == nil {
		d.OpenFIFO = openFIFO
	}
	return d
}

// Run is the keepalived image's PID 1 (D-15): it reads the configuration, finds the interface, writes the
// keepalived configuration, runs keepalived as its child and reports every state change it reads from the
// notify FIFO as one JSON line on d.Stdout. It returns the process's exit status: 0 after a stop that ctx's end
// asked for, 2 for a configuration error and 1 for anything else, which is a task swarm restarts.
func Run(ctx context.Context, d RunDeps) int {
	d = d.withDefaults()
	cfg, err := Load(d.Getenv)
	if err != nil {
		fmt.Fprintln(d.Stderr, "tierplan-ingress: vip-entrypoint:", err)
		return 2
	}
	em := NewEmitter(d.Stdout, cfg, d.Clock)

	iface, ok := awaitInterface(ctx, d, cfg, em)
	if !ok {
		// Stopped while there was no interface: keepalived never ran, so there is nothing to wait for.
		em.Emit(ingressbundle.VIPEvent{Event: ingressbundle.VIPEventState, State: ingressbundle.VIPStateStop})
		return 0
	}
	stale := heldOn(d.Interfaces, iface, cfg.Address.Addr())

	if err := d.WriteFile(ConfPath, Render(cfg, iface), 0o600); err != nil {
		fmt.Fprintln(d.Stderr, "tierplan-ingress: vip-entrypoint: write the keepalived configuration:", err)
		return 1
	}
	em.Emit(ingressbundle.VIPEvent{Event: ingressbundle.VIPEventStart, Iface: iface, Stale: stale})

	// keepalived's own output is consumed here and never copied on: only the check's failure becomes a fact,
	// and only closed-vocabulary lines are ever written (T-26-29). One writer for both streams, so the child
	// gets one pipe and the lines arrive in the order keepalived wrote them.
	facts := &probeFacts{}
	sink := facts.writer()
	proc, err := d.Start(KeepalivedBinary, KeepalivedArgs(), sink, sink)
	if err != nil {
		fmt.Fprintln(d.Stderr, "tierplan-ingress: vip-entrypoint: start keepalived:", err)
		return 1
	}
	exited := make(chan error, 1)
	go func() { exited <- proc.Wait() }()

	emitStop := func() {
		if em.LastState() != ingressbundle.VIPStateStop {
			em.Emit(ingressbundle.VIPEvent{Event: ingressbundle.VIPEventState, State: ingressbundle.VIPStateStop})
		}
	}
	fifo, err := openFIFOWithRetry(ctx, d, exited)
	switch {
	case errors.Is(err, errChildExited):
		fmt.Fprintln(d.Stderr, "tierplan-ingress: vip-entrypoint:", err)
		emitStop()
		return 1
	case ctx.Err() != nil:
		stop(d, proc, exited)
		emitStop()
		return 0
	case err != nil:
		stop(d, proc, exited)
		fmt.Fprintln(d.Stderr, "tierplan-ingress: vip-entrypoint:", err)
		emitStop()
		return 1
	}
	// The reader turns FIFO lines into a channel; the reporter turns them into events. Closing the FIFO ends
	// the reader, which ends the reporter, so both are finished before the final STOP line is written.
	lines := make(chan string)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer close(lines)
		readLines(fifo, maxFIFOLine, func(line string) { lines <- line })
	}()
	reporterDone := make(chan struct{})
	go func() {
		defer close(reporterDone)
		reportStates(d.Clock, em, facts, lines)
	}()

	code := 0
	select {
	case <-ctx.Done():
		stop(d, proc, exited)
	case werr := <-exited:
		if ctx.Err() == nil {
			fmt.Fprintln(d.Stderr, "tierplan-ingress: vip-entrypoint: keepalived exited:", exitText(werr))
			code = 1
		}
	}
	_ = fifo.Close()
	<-readerDone
	<-reporterDone
	emitStop()
	return code
}

// awaitInterface returns the one interface of the virtual address's network. While there is none or more than
// one it reports the fault once (again only when it changes), starts nothing and looks again every
// DiscoveryRescan: a node that cannot hold the address says so and stays up (D-09, Pitfall 15). When the
// interface appears it writes the fault again with active false, then returns. It returns false when ctx ends
// first.
func awaitInterface(ctx context.Context, d RunDeps, cfg Config, em *Emitter) (string, bool) {
	var shown *DiscoverError
	var shownKey string
	for {
		iface, err := Discover(d.Interfaces, cfg.Address)
		if err == nil {
			if shown != nil {
				em.Emit(ingressbundle.VIPEvent{Event: ingressbundle.VIPEventFault, Reason: shown.Reason, Ifaces: shown.Seen})
			}
			return iface, true
		}
		// A failure of the interface list itself leaves the node with no interface it can name.
		fault := &DiscoverError{Reason: ingressbundle.VIPReasonNoInterface}
		_ = errors.As(err, &fault)
		if key := fault.Reason + "|" + strings.Join(fault.Seen, ","); shown == nil || key != shownKey {
			// Only a line that was written counts as shown, so a dropped one is tried again at the next scan.
			if em.Emit(ingressbundle.VIPEvent{Event: ingressbundle.VIPEventFault, Reason: fault.Reason, Ifaces: fault.Seen, Active: true}) {
				shown, shownKey = fault, key
			}
		}
		select {
		case <-ctx.Done():
			return "", false
		case <-d.Clock.After(DiscoveryRescan):
		}
	}
}

// reportStates turns the notify FIFO's lines into state events until lines is closed. A FAULT is the one state
// with a reason: probe_failing when keepalived's log said the check failed since the node last was BACKUP or
// MASTER (D-12). The log can be read after the state it explains, so a FAULT with no failure seen yet is held
// for up to FaultReasonGrace; any later line, the failure line or the grace ending releases it, in order.
func reportStates(clock Clock, em *Emitter, facts *probeFacts, lines <-chan string) {
	var held bool
	var timer <-chan time.Time
	var failed <-chan struct{}
	faultEvent := func() ingressbundle.VIPEvent {
		ev := ingressbundle.VIPEvent{Event: ingressbundle.VIPEventState, State: ingressbundle.VIPStateFault}
		if facts.failing() {
			ev.Reason = ingressbundle.VIPReasonProbeFailing
		}
		return ev
	}
	release := func() {
		if held {
			em.Emit(faultEvent())
			held, timer, failed = false, nil, nil
		}
	}
	for {
		select {
		case line, open := <-lines:
			if !open {
				release()
				return
			}
			state, ok := parseFIFOLine(line)
			if !ok {
				continue
			}
			release()
			switch state {
			case ingressbundle.VIPStateFault:
				failed = facts.wait()
				if facts.failing() {
					em.Emit(faultEvent())
					failed = nil
					continue
				}
				held, timer = true, clock.After(FaultReasonGrace)
			case ingressbundle.VIPStateBackup, ingressbundle.VIPStateMaster:
				facts.reset()
				em.Emit(ingressbundle.VIPEvent{Event: ingressbundle.VIPEventState, State: state})
			default:
				em.Emit(ingressbundle.VIPEvent{Event: ingressbundle.VIPEventState, State: state})
			}
		case <-failed:
			release()
		case <-timer:
			release()
		}
	}
}

// heldOn reports whether the interface named iface holds addr now.
func heldOn(list Lister, iface string, addr netip.Addr) bool {
	ifaces, err := list()
	if err != nil {
		return false
	}
	return slices.ContainsFunc(ifaces, func(i Interface) bool {
		return i.Name == iface && slices.ContainsFunc(i.Addrs, func(p netip.Prefix) bool { return p.Addr() == addr })
	})
}

// errChildExited is what openFIFOWithRetry returns when keepalived exited before it made its FIFO.
var errChildExited = errors.New("keepalived exited before its notify FIFO existed")

// openFIFOWithRetry opens the notify FIFO, waiting for keepalived to create it, for at most fifoRetryFor.
func openFIFOWithRetry(ctx context.Context, d RunDeps, exited <-chan error) (io.ReadCloser, error) {
	for tries := 0; ; tries++ {
		f, err := d.OpenFIFO(FIFOPath)
		if err == nil {
			return f, nil
		}
		if time.Duration(tries)*fifoRetryEvery >= fifoRetryFor {
			return nil, errors.New("keepalived did not create its notify FIFO")
		}
		select {
		case <-d.Clock.After(fifoRetryEvery):
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-exited:
			return nil, errChildExited
		}
	}
}

// stop forwards SIGTERM to keepalived, so the holder sends its priority-0 advert and drops the address, and
// waits for it for at most StopWait before it kills it.
func stop(d RunDeps, proc Process, exited <-chan error) {
	_ = proc.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-d.Clock.After(StopWait):
		_ = proc.Signal(syscall.SIGKILL)
		<-exited
	}
}

// exitText says how a child ended without quoting anything it wrote.
func exitText(err error) string {
	var ee *exec.ExitError
	switch {
	case err == nil:
		return "exit status 0"
	case errors.As(err, &ee):
		return ee.ProcessState.String()
	}
	return "wait failed"
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

const testNodeID = "abcdefghijklmnopqrstuvwxy"

// testEnv is the environment of a node whose virtual address is 192.168.1.50/24.
func testEnv(k string) string {
	switch k {
	case ingressbundle.EnvVIPAddress:
		return "192.168.1.50/24"
	case ingressbundle.EnvVIPRouterID:
		return "77"
	case ingressbundle.EnvVIPProbePort:
		return "7483"
	case ingressbundle.EnvVIPKeyFile:
		return "/run/secrets/tierplan-keepalived-key"
	case ingressbundle.EnvVIPNodeID:
		return testNodeID
	case ingressbundle.EnvVIPNodeName:
		return "node-a"
	}
	return ""
}

// oneLAN is the node's interfaces: one LAN interface in the virtual address's network, and the loopback.
func oneLAN() ([]Interface, error) {
	return []Interface{
		{Name: "lo", Addrs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/8")}},
		{Name: "eth0", Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.1.10/24")}},
	}, nil
}

// syncBuf is a buffer a test reads while Run writes it, with a way to wait for lines.
type syncBuf struct {
	mu   sync.Mutex
	b    bytes.Buffer
	ping chan struct{}
}

func newSyncBuf() *syncBuf { return &syncBuf{ping: make(chan struct{}, 1)} }

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	n, err := s.b.Write(p)
	s.mu.Unlock()
	select {
	case s.ping <- struct{}{}:
	default:
	}
	return n, err
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuf) lines() []string {
	out := strings.TrimSuffix(s.String(), "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// waitLines blocks until n lines are written. The bound only turns a hang into a failure.
func (s *syncBuf) waitLines(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.After(20 * time.Second) // wall-clock: failure bound for a child process that never answers
	for {
		if l := s.lines(); len(l) >= n {
			return l
		}
		select {
		case <-s.ping:
		case <-deadline:
			t.Fatalf("waited for %d lines, got %q", n, s.String())
		}
	}
}

func parseAll(t *testing.T, lines []string) []ingressbundle.VIPEvent {
	t.Helper()
	out := make([]ingressbundle.VIPEvent, len(lines))
	for i, l := range lines {
		ev, err := ingressbundle.ParseVIPLine([]byte(l))
		if err != nil {
			t.Fatalf("line %d %q: %v", i, l, err)
		}
		out[i] = ev
	}
	return out
}

// The fake keepalived is the test binary run again (the standard helper-process pattern), so Run starts, signals
// and waits for a real child through the real ExecStart. The mode and the FIFO path travel in the environment.
const (
	envFake     = "TIERPLAN_TEST_FAKE_KEEPALIVED"
	envFakeFIFO = "TIERPLAN_TEST_FAKE_FIFO"
	// envFakeMark is a file the fake creates when it gets SIGTERM, so a test can tell the signal was forwarded.
	envFakeMark = "TIERPLAN_TEST_FAKE_MARK"
)

func fifoLine(state string) string { return fmt.Sprintf("INSTANCE %q %s 100\n", InstanceName, state) }

// TestHelperFakeKeepalived is not a test: it is the fake child. The mode is one of
//
//	lines   write FAULT, BACKUP and MASTER to the FIFO, then wait for SIGTERM, write STOP and exit 0
//	probe   write the script-failure line to stderr and FAULT to the FIFO, then as lines
//	fault   write FAULT to the FIFO only, then as lines
//	exit1   write BACKUP, then exit 1 on SIGUSR1
//	quiet   write nothing, then exit 0 on SIGTERM
//	deaf    write nothing and ignore SIGTERM, so only SIGKILL ends it
func TestHelperFakeKeepalived(t *testing.T) {
	mode := os.Getenv(envFake)
	if mode == "" {
		t.Skip("only runs as the fake keepalived child")
	}
	f, err := os.OpenFile(os.Getenv(envFakeFIFO), os.O_RDWR, 0) //nolint:gosec // G703: the path is this test's own fixture, set by the parent test
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake keepalived:", err)
		os.Exit(3)
	}
	sigs := make(chan os.Signal, 1)
	if mode == "deaf" {
		signal.Ignore(syscall.SIGTERM)
	}
	// SIGUSR2 is the statistics request (A-04): the fake takes it and writes nothing, as a keepalived with
	// nothing to report would.
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGUSR1, syscall.SIGUSR2)
	write := func(s string) { _, _ = f.WriteString(s) }
	switch mode {
	case "lines":
		write(fifoLine("FAULT") + fifoLine("BACKUP") + fifoLine("MASTER"))
	case "probe":
		fmt.Fprintln(os.Stderr, "Sat Nov 24 13:20:00 2026: VRRP_Script(chk_proxy) failed (exited with status 1)")
		write(fifoLine("FAULT"))
	case "fault":
		write(fifoLine("FAULT"))
	case "exit1":
		write(fifoLine("BACKUP"))
	}
	for s := range sigs {
		if s == syscall.SIGUSR1 {
			os.Exit(1)
		}
		if s == syscall.SIGUSR2 {
			continue
		}
		if mode == "deaf" {
			continue
		}
		if mark := os.Getenv(envFakeMark); mark != "" {
			_ = os.WriteFile(mark, []byte("TERM"), 0o600) //nolint:gosec // G703: the path is this test's own fixture, set by the parent test
		}
		write(fifoLine("STOP"))
		os.Exit(0)
	}
}

// rig is one Run over a fake keepalived.
type rig struct {
	t     *testing.T
	out   *syncBuf
	errw  *syncBuf
	clock *fakeClock
	deps  RunDeps
	conf  map[string][]byte
	// confMu guards conf: the statistics poll and the address check run beside Run's own writes.
	confMu sync.Mutex
	proc   chan Process // the started child, once Start was called
	fifo   string
	mark   string
}

// newRig makes the FIFO keepalived would make and a RunDeps that starts the fake child in mode.
func newRig(t *testing.T, mode string) *rig {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "keepalived.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	mark := filepath.Join(t.TempDir(), "sigterm.mark")
	t.Setenv(envFake, mode)
	t.Setenv(envFakeFIFO, fifo)
	t.Setenv(envFakeMark, mark)
	r := &rig{t: t, out: newSyncBuf(), errw: newSyncBuf(), clock: newFakeClock(), conf: map[string][]byte{}, proc: make(chan Process, 4), fifo: fifo, mark: mark}
	r.deps = RunDeps{
		// No test touches a real socket or the real flag path: the segment is quiet (nobody answers for the
		// address) unless a test says otherwise.
		Prober:     &fakeProber{script: []answer{{}}},
		FlagPath:   filepath.Join(t.TempDir(), "address-in-use"),
		Getenv:     testEnv,
		Stdout:     r.out,
		Stderr:     r.errw,
		Interfaces: oneLAN,
		Clock:      r.clock,
		WriteFile: func(path string, data []byte, perm os.FileMode) error {
			if perm != 0o600 {
				t.Errorf("configuration written with mode %o, want 600", perm)
			}
			r.confMu.Lock()
			defer r.confMu.Unlock()
			r.conf[path] = data
			return nil
		},
		// The files the helper writes are held in conf, so a test sees the flag without a real file system.
		RemoveFile: func(path string) error {
			r.confMu.Lock()
			defer r.confMu.Unlock()
			if _, ok := r.conf[path]; !ok {
				return os.ErrNotExist
			}
			delete(r.conf, path)
			return nil
		},
		OpenFIFO: func(string) (io.ReadCloser, error) { return openFIFO(fifo) },
		Start: func(_ string, _ []string, stdout, stderr io.Writer) (Process, error) {
			p, err := ExecStart(os.Args[0], []string{"-test.run=^TestHelperFakeKeepalived$"}, stdout, stderr)
			if err == nil {
				r.proc <- p
			}
			return p, err
		},
	}
	return r
}

// hasFile reports whether the helper has written path and not removed it.
func (r *rig) hasFile(path string) bool {
	r.confMu.Lock()
	defer r.confMu.Unlock()
	_, ok := r.conf[path]
	return ok
}

// run starts Run in a goroutine and returns the channel its exit status arrives on.
func (r *rig) run(ctx context.Context) <-chan int {
	done := make(chan int, 1)
	go func() { done <- Run(ctx, r.deps) }()
	return done
}

func waitCode(t *testing.T, done <-chan int) int {
	t.Helper()
	select {
	case c := <-done:
		return c
	case <-time.After(20 * time.Second): // wall-clock: failure bound for a Run that never returns
		t.Fatal("Run did not return")
		return -1
	}
}

// fakeClock is a clock a test moves by hand: After returns a channel that fires when Advance has moved time
// past it.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	timers  []fakeTimer
	changed chan struct{}
}

type fakeTimer struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC), changed: make(chan struct{}, 1)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, fakeTimer{at: c.now.Add(d), ch: ch})
	select {
	case c.changed <- struct{}{}:
	default:
	}
	return ch
}

// Advance moves time forward and fires every timer that is due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.timers[:0]
	for _, tm := range c.timers {
		if !tm.at.After(c.now) {
			tm.ch <- c.now
		} else {
			kept = append(kept, tm)
		}
	}
	c.timers = kept
}

func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// waitPending blocks until at least n timers wait on the clock: the moment Run is parked on it.
func (c *fakeClock) waitPending(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(20 * time.Second) // wall-clock: failure bound
	for c.pending() < n {
		select {
		case <-c.changed:
		case <-deadline:
			t.Fatalf("waited for %d timers on the clock, have %d", n, c.pending())
		}
	}
}

// waitTimer blocks until a timer of exactly d waits on the clock. The statistics poll keeps a timer of its own
// on the clock, so a test that means one particular wait names it instead of counting timers.
func (c *fakeClock) waitTimer(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.After(20 * time.Second) // wall-clock: failure bound
	for {
		c.mu.Lock()
		found := false
		for _, tm := range c.timers {
			if tm.at.Sub(c.now) == d {
				found = true
			}
		}
		c.mu.Unlock()
		if found {
			return
		}
		select {
		case <-c.changed:
		case <-deadline:
			t.Fatalf("waited for a timer of %s on the clock", d)
		}
	}
}

func TestVIPEntrypointWritesStateLines(t *testing.T) {
	r := newRig(t, "lines")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := r.run(ctx)

	events := parseAll(t, r.out.waitLines(t, 4))
	cancel()
	if code := waitCode(t, done); code != 0 {
		t.Fatalf("Run = %d, want 0 after a stop (stderr %q)", code, r.errw.String())
	}

	want := []struct{ event, state string }{
		{ingressbundle.VIPEventStart, ""},
		{ingressbundle.VIPEventState, ingressbundle.VIPStateFault},
		{ingressbundle.VIPEventState, ingressbundle.VIPStateBackup},
		{ingressbundle.VIPEventState, ingressbundle.VIPStateMaster},
	}
	for i, w := range want {
		ev := events[i]
		if ev.Event != w.event || ev.State != w.state {
			t.Errorf("line %d = %s %s, want %s %s", i, ev.Event, ev.State, w.event, w.state)
		}
		if ev.Seq != uint64(i+1) {
			t.Errorf("line %d seq = %d, want %d", i, ev.Seq, i+1)
		}
		if ev.Node != testNodeID || ev.Name != "node-a" || ev.Router != 77 {
			t.Errorf("line %d identity = %q %q %d", i, ev.Node, ev.Name, ev.Router)
		}
	}
	if events[0].Iface != "eth0" || events[0].Stale {
		t.Errorf("start = iface %q stale %v, want eth0 and not stale", events[0].Iface, events[0].Stale)
	}
	conf := string(r.conf[ConfPath])
	if !strings.Contains(conf, "interface eth0") || !strings.Contains(conf, "virtual_router_id 77") {
		t.Errorf("configuration written to %s = %q", ConfPath, conf)
	}
}

func stateOf(ev ingressbundle.VIPEvent) string { return ev.Event + "/" + ev.State }

func TestVIPEntrypointSignals(t *testing.T) {
	t.Run("a stop is forwarded to keepalived, waited for and reported once", func(t *testing.T) {
		r := newRig(t, "lines")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := r.run(ctx)
		r.out.waitLines(t, 4)
		cancel()
		if code := waitCode(t, done); code != 0 {
			t.Fatalf("Run = %d, want 0 (stderr %q)", code, r.errw.String())
		}
		// Run returns only after the child exited, so the mark proves the SIGTERM reached it first.
		if got, err := os.ReadFile(r.mark); err != nil || string(got) != "TERM" {
			t.Fatalf("the child did not get SIGTERM: mark %q, %v", got, err)
		}
		events := parseAll(t, r.out.lines())
		last := events[len(events)-1]
		if last.Event != ingressbundle.VIPEventState || last.State != ingressbundle.VIPStateStop {
			t.Fatalf("last line = %+v, want state STOP", last)
		}
		stops := 0
		for i, ev := range events {
			if ev.State == ingressbundle.VIPStateStop {
				stops++
			}
			if ev.Seq != uint64(i+1) {
				t.Errorf("line %d seq = %d, want %d", i, ev.Seq, i+1)
			}
		}
		if stops != 1 {
			t.Errorf("%d STOP lines, want 1", stops)
		}
	})

	t.Run("a keepalived that ignores SIGTERM is killed after the stop window", func(t *testing.T) {
		r := newRig(t, "deaf")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := r.run(ctx)
		r.out.waitLines(t, 1)
		// The stop must find keepalived running: before it starts, a stop ends Run without starting it.
		select {
		case <-r.proc:
		case <-time.After(20 * time.Second): // wall-clock: failure bound
			t.Fatal("keepalived was not started")
		}
		cancel()
		r.clock.waitTimer(t, StopWait)
		r.clock.Advance(StopWait)
		if code := waitCode(t, done); code != 0 {
			t.Fatalf("Run = %d, want 0 after a stop (stderr %q)", code, r.errw.String())
		}
		events := parseAll(t, r.out.lines())
		if last := events[len(events)-1]; last.State != ingressbundle.VIPStateStop {
			t.Fatalf("last line = %+v, want state STOP", last)
		}
	})

	t.Run("a keepalived that exits on its own makes the helper exit non-zero", func(t *testing.T) {
		r := newRig(t, "exit1")
		done := r.run(context.Background())
		r.out.waitLines(t, 2)
		p := <-r.proc
		if err := p.Signal(syscall.SIGUSR1); err != nil {
			t.Fatalf("signal the fake: %v", err)
		}
		if code := waitCode(t, done); code != 1 {
			t.Fatalf("Run = %d, want 1 so swarm restarts the task", code)
		}
		events := parseAll(t, r.out.lines())
		if last := events[len(events)-1]; stateOf(last) != "state/STOP" {
			t.Fatalf("last line = %+v, want state STOP", last)
		}
		if !strings.Contains(r.errw.String(), "keepalived exited") {
			t.Errorf("stderr = %q, want it to say keepalived exited", r.errw.String())
		}
	})
}

// swappable is an interface list a test changes while Run polls it.
type swappable struct {
	mu   sync.Mutex
	list []Interface
}

func (s *swappable) set(l ...Interface) {
	s.mu.Lock()
	s.list = l
	s.mu.Unlock()
}

func (s *swappable) get() ([]Interface, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Interface(nil), s.list...), nil
}

func nic(name, addr string) Interface {
	return Interface{Name: name, Addrs: []netip.Prefix{netip.MustParsePrefix(addr)}}
}

func TestVIPEntrypointDiscoveryFault(t *testing.T) {
	r := newRig(t, "quiet")
	ifs := &swappable{}
	ifs.set(nic("lo", "127.0.0.1/8"), nic("eth9", "10.0.0.5/24"))
	r.deps.Interfaces = ifs.get
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := r.run(ctx)

	// rescan moves the clock one rescan interval and waits until Run has scanned and is parked again, so a
	// line that was not written is known not to be coming.
	rescan := func() {
		t.Helper()
		r.clock.waitPending(t, 1)
		r.clock.Advance(DiscoveryRescan)
		r.clock.waitPending(t, 1)
	}

	first := parseAll(t, r.out.waitLines(t, 1))[0]
	if first.Event != ingressbundle.VIPEventFault || first.Reason != ingressbundle.VIPReasonNoInterface || !first.Active ||
		len(first.Ifaces) != 1 || first.Ifaces[0] != "eth9" {
		t.Fatalf("first line = %+v, want an active no_interface fault naming eth9", first)
	}
	if len(r.proc) != 0 {
		t.Fatal("keepalived was started without an interface")
	}

	rescan()
	if n := len(r.out.lines()); n != 1 {
		t.Fatalf("%d lines after a rescan that changed nothing, want 1", n)
	}

	// Two interfaces in the network: a different fault, written once.
	ifs.set(nic("lo", "127.0.0.1/8"), nic("eth0", "192.168.1.10/24"), nic("eth1", "192.168.1.11/24"))
	rescan()
	second := parseAll(t, r.out.waitLines(t, 2))[1]
	if second.Reason != ingressbundle.VIPReasonAmbiguousInterface || !second.Active || len(second.Ifaces) != 2 {
		t.Fatalf("second line = %+v, want an active ambiguous_interface fault naming both", second)
	}
	rescan()
	if n := len(r.out.lines()); n != 2 {
		t.Fatalf("%d lines after a second identical fault, want 2", n)
	}
	if len(r.proc) != 0 {
		t.Fatal("keepalived was started with two interfaces")
	}

	// One interface appears: the fault clears, then start, and keepalived runs.
	ifs.set(nic("lo", "127.0.0.1/8"), nic("eth0", "192.168.1.10/24"))
	r.clock.Advance(DiscoveryRescan)
	events := parseAll(t, r.out.waitLines(t, 4))
	cleared, start := events[2], events[3]
	if cleared.Event != ingressbundle.VIPEventFault || cleared.Active || cleared.Reason != ingressbundle.VIPReasonAmbiguousInterface {
		t.Fatalf("third line = %+v, want the ambiguous_interface fault with active false", cleared)
	}
	if start.Event != ingressbundle.VIPEventStart || start.Iface != "eth0" {
		t.Fatalf("fourth line = %+v, want start on eth0", start)
	}
	<-r.proc

	cancel()
	if code := waitCode(t, done); code != 0 {
		t.Fatalf("Run = %d, want 0 (stderr %q)", code, r.errw.String())
	}
}

func TestVIPEntrypointDiscoveryFaultStopsOnSignal(t *testing.T) {
	r := newRig(t, "quiet")
	r.deps.Interfaces = func() ([]Interface, error) { return []Interface{nic("lo", "127.0.0.1/8")}, nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := r.run(ctx)
	r.out.waitLines(t, 1)
	cancel()
	if code := waitCode(t, done); code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}
	events := parseAll(t, r.out.lines())
	if last := events[len(events)-1]; stateOf(last) != "state/STOP" {
		t.Fatalf("last line = %+v, want state STOP", last)
	}
	if len(r.proc) != 0 {
		t.Fatal("keepalived was started")
	}
}

func TestVIPEntrypointFaultReason(t *testing.T) {
	t.Run("a failing check before FAULT is the reason", func(t *testing.T) {
		r := newRig(t, "probe")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := r.run(ctx)
		ev := parseAll(t, r.out.waitLines(t, 2))[1]
		if ev.State != ingressbundle.VIPStateFault || ev.Reason != ingressbundle.VIPReasonProbeFailing {
			t.Fatalf("line = %+v, want FAULT with probe_failing", ev)
		}
		cancel()
		waitCode(t, done)
		// keepalived's text reached no line.
		if strings.Contains(r.out.String(), "VRRP_Script") || strings.Contains(r.out.String(), "exited with status") {
			t.Fatalf("keepalived's own text is in the report: %q", r.out.String())
		}
	})

	t.Run("FAULT with no failing check has no reason", func(t *testing.T) {
		r := newRig(t, "fault")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := r.run(ctx)
		// The helper holds a FAULT for a short grace in case the check's line is still on its way.
		r.clock.waitTimer(t, FaultReasonGrace)
		r.clock.Advance(FaultReasonGrace)
		ev := parseAll(t, r.out.waitLines(t, 2))[1]
		if ev.State != ingressbundle.VIPStateFault || ev.Reason != "" {
			t.Fatalf("line = %+v, want FAULT with no reason", ev)
		}
		cancel()
		waitCode(t, done)
	})
}

func TestProbeFactsScanner(t *testing.T) {
	var f probeFacts
	w := f.writer()
	write := func(s string) {
		t.Helper()
		if n, err := w.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	write("Nov 24 VRRP_Script(chk_proxy) succeeded\n")
	if f.failing() {
		t.Fatal("failing after a success")
	}
	// A line that arrives in two writes counts once it is complete.
	write("Nov 24 VRRP_Script(chk_pr")
	if f.failing() {
		t.Fatal("failing on half a line")
	}
	write("oxy) failed (exited with status 1)\nunrelated\n")
	if !f.failing() {
		t.Fatal("not failing after the check failed")
	}
	write("VRRP_Script(chk_proxy) succeeded\n")
	if f.failing() {
		t.Fatal("still failing after the check succeeded")
	}
	write("VRRP_Script(chk_proxy) timed out\n")
	if !f.failing() {
		t.Fatal("a timed-out check is a failing check")
	}
	f.reset()
	if f.failing() {
		t.Fatal("still failing after a reset")
	}
	// Another script's lines and a line that is far too long change nothing, and Write never blocks the child.
	write("VRRP_Script(other) failed (exited with status 1)\n")
	write(strings.Repeat("x", 100000) + "VRRP_Script(chk_proxy) failed\n")
	if f.failing() {
		t.Fatal("an overlong line or another script's line made the check fail")
	}
}

func TestRenderNamesTheScriptTheScannerWatches(t *testing.T) {
	cfg, err := Load(testEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(Render(cfg, "eth0")), "vrrp_script "+scriptName+" {") {
		t.Fatalf("Render does not define the vrrp_script %q the stderr scanner watches", scriptName)
	}
}

func TestVIPEntrypointRemovesStaleElsewhere(t *testing.T) {
	type removal struct {
		iface string
		p     netip.Prefix
	}
	run := func(t *testing.T, ifs []Interface, removeErr error) (start ingressbundle.VIPEvent, removed []removal, stderr string) {
		t.Helper()
		r := newRig(t, "quiet")
		r.deps.Interfaces = func() ([]Interface, error) { return ifs, nil }
		var mu sync.Mutex
		r.deps.RemoveAddr = func(iface string, p netip.Prefix) error {
			mu.Lock()
			defer mu.Unlock()
			removed = append(removed, removal{iface, p})
			return removeErr
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := r.run(ctx)
		start = parseAll(t, r.out.waitLines(t, 1))[0]
		cancel()
		waitCode(t, done)
		mu.Lock()
		defer mu.Unlock()
		return start, removed, r.errw.String()
	}

	t.Run("the address on another interface is removed before keepalived starts and reported", func(t *testing.T) {
		start, removed, _ := run(t, []Interface{
			nic("lo", "127.0.0.1/8"),
			nic("eth0", "192.168.1.10/24"),
			nic("eth1", "192.168.1.50/24"),
			nic("docker0", "192.168.1.50/24"), // never a keepalived interface: left alone
		}, nil)
		if start.Event != ingressbundle.VIPEventStart || start.Iface != "eth0" || !start.Stale {
			t.Fatalf("start = %+v, want eth0 and stale", start)
		}
		if len(removed) != 1 || removed[0].iface != "eth1" || removed[0].p != netip.MustParsePrefix("192.168.1.50/24") {
			t.Fatalf("removals = %+v, want only the VIP on eth1 with its own prefix length", removed)
		}
	})

	t.Run("the address on the chosen interface is reported and left to keepalived", func(t *testing.T) {
		start, removed, _ := run(t, []Interface{
			{Name: "eth0", Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.1.10/24"), netip.MustParsePrefix("192.168.1.50/24")}},
		}, nil)
		if !start.Stale || len(removed) != 0 {
			t.Fatalf("stale = %v, removals = %+v, want stale and no removal (A-02: keepalived clears the same interface)", start.Stale, removed)
		}
	})

	t.Run("no stale address, no removal", func(t *testing.T) {
		start, removed, _ := run(t, []Interface{nic("eth0", "192.168.1.10/24")}, nil)
		if start.Stale || len(removed) != 0 {
			t.Fatalf("stale = %v, removals = %+v", start.Stale, removed)
		}
	})

	t.Run("a failed removal is one stderr line and the start goes on", func(t *testing.T) {
		start, removed, stderr := run(t, []Interface{nic("eth0", "192.168.1.10/24"), nic("eth1", "192.168.1.50/24")}, syscall.EPERM)
		if start.Event != ingressbundle.VIPEventStart || start.Stale || len(removed) != 1 {
			t.Fatalf("start = %+v, removals = %+v", start, removed)
		}
		if !strings.Contains(stderr, "eth1") || strings.Count(stderr, "\n") != 1 {
			t.Fatalf("stderr = %q, want one line naming eth1", stderr)
		}
	})
}

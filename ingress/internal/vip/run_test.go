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
)

func fifoLine(state string) string { return fmt.Sprintf("INSTANCE %q %s 100\n", InstanceName, state) }

// TestHelperFakeKeepalived is not a test: it is the fake child. The mode is one of
//
//	lines   write FAULT, BACKUP and MASTER to the FIFO, then wait for SIGTERM, write STOP and exit 0
//	probe   write the script-failure line to stderr and FAULT to the FIFO, then as lines
//	fault   write FAULT to the FIFO only, then as lines
//	exit1   write BACKUP, then exit 1 on SIGUSR1
//	quiet   write nothing, then exit 0 on SIGTERM
func TestHelperFakeKeepalived(t *testing.T) {
	mode := os.Getenv(envFake)
	if mode == "" {
		t.Skip("only runs as the fake keepalived child")
	}
	f, err := os.OpenFile(os.Getenv(envFakeFIFO), os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake keepalived:", err)
		os.Exit(3)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGUSR1)
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
	proc  chan Process // the started child, once Start was called
	fifo  string
}

// newRig makes the FIFO keepalived would make and a RunDeps that starts the fake child in mode.
func newRig(t *testing.T, mode string) *rig {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "keepalived.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	t.Setenv(envFake, mode)
	t.Setenv(envFakeFIFO, fifo)
	r := &rig{t: t, out: newSyncBuf(), errw: newSyncBuf(), clock: newFakeClock(), conf: map[string][]byte{}, proc: make(chan Process, 4), fifo: fifo}
	r.deps = RunDeps{
		Getenv:     testEnv,
		Stdout:     r.out,
		Stderr:     r.errw,
		Interfaces: oneLAN,
		Clock:      r.clock,
		WriteFile: func(path string, data []byte, perm os.FileMode) error {
			if perm != 0o600 {
				t.Errorf("configuration written with mode %o, want 600", perm)
			}
			r.conf[path] = data
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

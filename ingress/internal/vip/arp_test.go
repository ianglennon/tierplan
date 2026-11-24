// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// answer is one scripted result of a check: whether the ARP probe was answered and which router IDs the
// listener heard.
type answer struct {
	arp     bool
	adverts []int
	arpErr  error
	vrrpErr error
}

// fakeProber answers from a script and records what it was asked. The last answer repeats.
type fakeProber struct {
	mu       sync.Mutex
	script   []answer
	calls    int
	listens  int
	onARP    func() // runs inside each ARPProbe
	lastIf   string
	lastAddr netip.Addr
	lastN    int
	lastID   int
	lastFor  time.Duration
}

func (f *fakeProber) next() answer {
	i := min(f.calls-1, len(f.script)-1)
	return f.script[i]
}

func (f *fakeProber) ARPProbe(_ context.Context, iface string, addr netip.Addr, n int) (bool, error) {
	f.mu.Lock()
	f.calls++
	f.lastIf, f.lastAddr, f.lastN = iface, addr, n
	a := f.next()
	hook := f.onARP
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return a.arp, a.arpErr
}

func (f *fakeProber) VRRPListen(_ context.Context, iface string, routerID int, d time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listens++
	f.lastIf, f.lastID, f.lastFor = iface, routerID, d
	a := f.next()
	return slices.Contains(a.adverts, routerID), a.vrrpErr
}

// checkerRig is an addressChecker over a temporary flag path, real file effects and a clock that does not wait.
type checkerRig struct {
	t    *testing.T
	c    *addressChecker
	out  *syncBuf
	errw *syncBuf
	p    *fakeProber
	flag string
	clk  *instantClock
}

func newCheckerRig(t *testing.T, script ...answer) *checkerRig {
	t.Helper()
	cfg, err := Load(testEnv)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := &checkerRig{t: t, out: newSyncBuf(), errw: newSyncBuf(), p: &fakeProber{script: script}, clk: &instantClock{now: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}}
	r.flag = filepath.Join(t.TempDir(), "tierplan-vip", "address-in-use")
	d := RunDeps{Stdout: r.out, Stderr: r.errw, Clock: r.clk, Prober: r.p, FlagPath: r.flag}.withDefaults()
	r.c = newAddressChecker(d, cfg, "eth0", NewEmitter(r.out, cfg, r.clk))
	return r
}

func (r *checkerRig) flagExists() bool {
	_, err := os.Stat(r.flag)
	return err == nil
}

func (r *checkerRig) events() []ingressbundle.VIPEvent { return parseAll(r.t, r.out.lines()) }

func TestAddressCheckDecision(t *testing.T) {
	// The pure decision.
	for _, tc := range []struct {
		answered, seen bool
		want           string
	}{
		{false, false, addrFree},
		{false, true, addrFree},
		{true, true, addrPeer},
		{true, false, addrInUse},
	} {
		if got := decide(tc.answered, tc.seen); got != tc.want {
			t.Errorf("decide(%v, %v) = %q, want %q", tc.answered, tc.seen, got, tc.want)
		}
	}

	// The check, over the fake prober: the router ID of testEnv is 77.
	cases := []struct {
		name       string
		a          answer
		wantFlag   bool
		wantListen bool
	}{
		{"no ARP reply is free", answer{}, false, false},
		{"a reply and an advert with our router ID is a peer", answer{arp: true, adverts: []int{77}}, false, true},
		{"a reply and no advert is in use", answer{arp: true}, true, true},
		{"a reply and only adverts of another router ID is in use", answer{arp: true, adverts: []int{51, 78}}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newCheckerRig(t, tc.a)
			r.c.check(context.Background())
			if r.flagExists() != tc.wantFlag || r.c.inUse != tc.wantFlag {
				t.Fatalf("flag exists %v, inUse %v, want %v", r.flagExists(), r.c.inUse, tc.wantFlag)
			}
			if (r.p.listens > 0) != tc.wantListen {
				t.Fatalf("listened %d times, want listening %v", r.p.listens, tc.wantListen)
			}
			if r.p.lastIf != "eth0" || r.p.lastAddr != netip.MustParseAddr("192.168.1.50") || r.p.lastN != 3 {
				t.Fatalf("probe asked %q %s %d, want eth0 192.168.1.50 3", r.p.lastIf, r.p.lastAddr, r.p.lastN)
			}
			if tc.wantListen && (r.p.lastID != 77 || r.p.lastFor != 3*time.Second) {
				t.Fatalf("listener asked router %d for %s, want 77 for 3s", r.p.lastID, r.p.lastFor)
			}
			evs := r.events()
			switch {
			case tc.wantFlag:
				if len(evs) != 1 || evs[0].Event != ingressbundle.VIPEventFault || evs[0].Reason != ingressbundle.VIPReasonAddressInUse || !evs[0].Active {
					t.Fatalf("events = %+v, want one active address_in_use fault", evs)
				}
			case len(evs) != 0:
				t.Fatalf("events = %+v, want none", evs)
			}
		})
	}
}

func TestAddressCheckRecheck(t *testing.T) {
	t.Run("in use writes the flag and the fault once, a later free check removes both", func(t *testing.T) {
		r := newCheckerRig(t,
			answer{arp: true}, // the first check
			answer{arp: true}, // still in use at the first re-check
			answer{},          // gone at the second
		)
		r.c.check(context.Background())
		if !r.flagExists() {
			t.Fatal("no flag after an in-use check")
		}
		if info, err := os.Stat(r.flag); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("flag mode = %v, %v", info, err)
		}
		before := r.clk.Now()
		r.c.recheck(context.Background())
		if r.flagExists() || r.c.inUse {
			t.Fatalf("flag exists %v inUse %v after the address was free", r.flagExists(), r.c.inUse)
		}
		if r.p.calls != 3 {
			t.Fatalf("%d checks, want 3", r.p.calls)
		}
		if waited := r.clk.Now().Sub(before); waited != 2*AddressRecheck {
			t.Fatalf("waited %s between the checks, want two of %s", waited, AddressRecheck)
		}
		evs := r.events()
		if len(evs) != 2 {
			t.Fatalf("events = %+v, want the fault and the cleared fault", evs)
		}
		if !evs[0].Active || evs[1].Active || evs[1].Event != ingressbundle.VIPEventFault || evs[1].Reason != ingressbundle.VIPReasonAddressInUse {
			t.Fatalf("events = %+v", evs)
		}
	})

	t.Run("a peer result never creates the flag and clears an existing one", func(t *testing.T) {
		r := newCheckerRig(t, answer{arp: true, adverts: []int{77}})
		r.c.check(context.Background())
		if r.flagExists() || len(r.out.lines()) != 0 {
			t.Fatalf("a peer made a flag (%v) or lines %q", r.flagExists(), r.out.lines())
		}
		r = newCheckerRig(t, answer{arp: true}, answer{arp: true, adverts: []int{77}})
		r.c.check(context.Background())
		r.c.recheck(context.Background())
		if r.flagExists() || len(r.events()) != 2 {
			t.Fatalf("flag %v events %+v", r.flagExists(), r.events())
		}
	})

	t.Run("a re-check does not run when nothing is held", func(t *testing.T) {
		r := newCheckerRig(t, answer{})
		r.c.recheck(context.Background())
		if r.p.calls != 0 {
			t.Fatalf("%d checks with no flag", r.p.calls)
		}
	})

	t.Run("a stop ends the re-check and leaves the flag", func(t *testing.T) {
		r := newCheckerRig(t, answer{arp: true})
		r.c.check(context.Background())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r.c.recheck(ctx)
		if !r.flagExists() || !r.c.inUse {
			t.Fatal("the flag went with the stop")
		}
	})

	t.Run("a stale flag from a previous run is removed at the start", func(t *testing.T) {
		r := newCheckerRig(t, answer{})
		if err := os.MkdirAll(filepath.Dir(r.flag), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(r.flag, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		r.c.clearStale()
		if r.flagExists() {
			t.Fatal("the stale flag is still there")
		}
		r.c.clearStale() // nothing to remove is not an error
		if r.errw.String() != "" {
			t.Fatalf("stderr = %q", r.errw.String())
		}
	})

	t.Run("a check that cannot finish changes nothing and shows its error once", func(t *testing.T) {
		r := newCheckerRig(t, answer{arpErr: errProbeSocket})
		for range 3 {
			r.c.check(context.Background())
		}
		if r.flagExists() || len(r.out.lines()) != 0 {
			t.Fatalf("a failed check changed the node: flag %v lines %q", r.flagExists(), r.out.lines())
		}
		if n := len(r.errw.lines()); n != 1 || !strings.Contains(r.errw.String(), errProbeSocket.Error()) {
			t.Fatalf("stderr = %q, want one line naming the step", r.errw.String())
		}
		// A listener failure after a reply is the same: no flag.
		r = newCheckerRig(t, answer{arp: true, vrrpErr: errProbeGroup})
		r.c.check(context.Background())
		if r.flagExists() {
			t.Fatal("a listener that failed still held the node out")
		}
	})

	t.Run("a stop during the check is not an error", func(t *testing.T) {
		r := newCheckerRig(t, answer{arpErr: context.Canceled})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r.c.check(ctx)
		if r.errw.String() != "" || r.flagExists() {
			t.Fatalf("stderr %q flag %v", r.errw.String(), r.flagExists())
		}
	})

	t.Run("a flag that cannot be written is not reported as held", func(t *testing.T) {
		r := newCheckerRig(t, answer{arp: true})
		r.c.d.WriteFile = func(string, []byte, os.FileMode) error { return errors.New("disk full") }
		r.c.check(context.Background())
		if r.c.inUse || len(r.out.lines()) != 0 {
			t.Fatalf("inUse %v lines %q", r.c.inUse, r.out.lines())
		}
		if strings.Contains(r.errw.String(), "disk full") {
			t.Fatalf("the system's text reached stderr: %q", r.errw.String())
		}
	})

	t.Run("a flag that cannot be removed keeps the node held and writes no clearing line", func(t *testing.T) {
		r := newCheckerRig(t, answer{arp: true}, answer{})
		r.c.check(context.Background())
		r.c.d.RemoveFile = func(string) error { return errors.New("read-only") }
		r.c.check(context.Background())
		if !r.c.inUse || len(r.events()) != 1 {
			t.Fatalf("inUse %v events %+v", r.c.inUse, r.events())
		}
	})

	t.Run("a flag already gone when it is removed counts as removed", func(t *testing.T) {
		r := newCheckerRig(t, answer{arp: true}, answer{})
		r.c.check(context.Background())
		if err := os.Remove(r.flag); err != nil {
			t.Fatal(err)
		}
		r.c.check(context.Background())
		if r.c.inUse || len(r.events()) != 2 {
			t.Fatalf("inUse %v events %+v", r.c.inUse, r.events())
		}
		if _, err := os.Stat(r.flag); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("flag: %v", err)
		}
	})
}

// The frames and packets the real prober sends and reads, built without a socket.

var (
	testMAC  = net.HardwareAddr{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}
	otherMAC = net.HardwareAddr{0xde, 0x4e, 0xdf, 0xc5, 0x6a, 0xe3}
	testVIP  = netip.MustParseAddr("192.168.1.50")
)

// arpFrame is an ARP packet from senderMAC claiming senderIP with the opcode.
func arpFrame(op byte, senderMAC net.HardwareAddr, senderIP netip.Addr) []byte {
	f := make([]byte, 60)
	copy(f[0:6], testMAC)
	copy(f[6:12], senderMAC)
	f[12], f[13] = 0x08, 0x06
	f[15], f[16], f[18], f[19], f[21] = 1, 0x08, 6, 4, op
	copy(f[22:28], senderMAC)
	a := senderIP.As4()
	copy(f[28:32], a[:])
	return f
}

func TestARPProbeFrame(t *testing.T) {
	f := arpProbeFrame(testMAC, testVIP)
	if len(f) != 60 {
		t.Fatalf("frame is %d bytes, want 60", len(f))
	}
	want := map[string][]byte{
		"destination":     {0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		"source":          testMAC,
		"ethertype":       {0x08, 0x06},
		"hardware type":   {0, 1},
		"protocol type":   {0x08, 0x00},
		"lengths":         {6, 4},
		"opcode":          {0, 1},
		"sender hardware": testMAC,
		"sender address":  {0, 0, 0, 0},
		"target hardware": {0, 0, 0, 0, 0, 0},
		"target address":  {192, 168, 1, 50},
	}
	got := map[string][]byte{
		"destination": f[0:6], "source": f[6:12], "ethertype": f[12:14], "hardware type": f[14:16],
		"protocol type": f[16:18], "lengths": f[18:20], "opcode": f[20:22], "sender hardware": f[22:28],
		"sender address": f[28:32], "target hardware": f[32:38], "target address": f[38:42],
	}
	for k, w := range want {
		if !slices.Equal(got[k], w) {
			t.Errorf("%s = % x, want % x", k, got[k], w)
		}
	}
	// A frame of the same probe is never read as a claim: the sender address is 0.0.0.0.
	if claimsAddress(f, testVIP, otherMAC) {
		t.Error("our own probe reads as a claim")
	}
}

func TestClaimsAddress(t *testing.T) {
	other := netip.MustParseAddr("192.168.1.99")
	cases := []struct {
		name  string
		frame []byte
		want  bool
	}{
		{"a reply from another machine", arpFrame(2, otherMAC, testVIP), true},
		{"the holder's announcement", arpFrame(1, otherMAC, testVIP), true},
		{"a reply from our own interface", arpFrame(2, testMAC, testVIP), false},
		{"a reply for another address", arpFrame(2, otherMAC, other), false},
		{"another opcode", arpFrame(3, otherMAC, testVIP), false},
		{"too short", arpFrame(2, otherMAC, testVIP)[:41], false},
		{"not ARP", func() []byte { f := arpFrame(2, otherMAC, testVIP); f[13] = 0x00; return f }(), false},
		{"not Ethernet", func() []byte { f := arpFrame(2, otherMAC, testVIP); f[15] = 6; return f }(), false},
		{"not IPv4", func() []byte { f := arpFrame(2, otherMAC, testVIP); f[16] = 0x86; return f }(), false},
		{"empty", nil, false},
	}
	for _, tc := range cases {
		if got := claimsAddress(tc.frame, testVIP, testMAC); got != tc.want {
			t.Errorf("%s: claimsAddress = %v, want %v", tc.name, got, tc.want)
		}
	}
	if claimsAddress(arpFrame(2, otherMAC, testVIP), netip.MustParseAddr("fe80::1"), testMAC) {
		t.Error("an IPv6 address matched an ARP frame")
	}
}

// vrrpPacket is a raw IPv4 packet of protocol 112 with a VRRP header of version and type, router ID and priority.
func vrrpPacket(ihlWords byte, proto byte, versionType byte, routerID byte) []byte {
	ihl := int(ihlWords) * 4
	p := make([]byte, ihl+8)
	p[0] = 4<<4 | ihlWords
	p[9] = proto
	p[ihl], p[ihl+1], p[ihl+2] = versionType, routerID, 100
	return p
}

func TestVRRPAdvertRouter(t *testing.T) {
	cases := []struct {
		name   string
		pkt    []byte
		wantID int
		wantOK bool
	}{
		{"version 2 advert", vrrpPacket(5, 112, 0x21, 77), 77, true},
		{"version 3 advert", vrrpPacket(5, 112, 0x31, 51), 51, true},
		{"header with options", vrrpPacket(7, 112, 0x21, 9), 9, true},
		{"another protocol", vrrpPacket(5, 6, 0x21, 77), 0, false},
		{"version 1", vrrpPacket(5, 112, 0x11, 77), 0, false},
		{"not an advert", vrrpPacket(5, 112, 0x22, 77), 0, false},
		{"not IPv4", func() []byte { p := vrrpPacket(5, 112, 0x21, 77); p[0] = 6<<4 | 5; return p }(), 0, false},
		{"header length below 20", vrrpPacket(4, 112, 0x21, 77), 0, false},
		{"cut after the IP header", vrrpPacket(5, 112, 0x21, 77)[:22], 0, false},
		{"empty", nil, 0, false},
	}
	for _, tc := range cases {
		id, ok := vrrpAdvertRouter(tc.pkt)
		if id != tc.wantID || ok != tc.wantOK {
			t.Errorf("%s: = %d, %v, want %d, %v", tc.name, id, ok, tc.wantID, tc.wantOK)
		}
	}
}

// TestVIPEntrypointChecksAddressFirst is A-05: Run asks the prober before it starts keepalived, and starts
// keepalived whatever the answer, because the flag and not a missing keepalived is what keeps the node out.
func TestVIPEntrypointChecksAddressFirst(t *testing.T) {
	for _, tc := range []struct {
		name     string
		a        answer
		wantFlag bool
	}{
		{"the segment is quiet", answer{}, false},
		{"another machine holds the address", answer{arp: true}, true},
		{"a peer holds the address", answer{arp: true, adverts: []int{77}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "lines")
			p := &fakeProber{script: []answer{tc.a}}
			var startedAtCheck = -1
			p.onARP = func() { startedAtCheck = len(r.proc) }
			r.deps.Prober = p
			flag := r.deps.FlagPath
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := r.run(ctx)

			// start, the fault when there is one, then the three states of the fake keepalived.
			want := 4
			if tc.wantFlag {
				want = 5
			}
			events := parseAll(t, r.out.waitLines(t, want))
			if startedAtCheck != 0 {
				t.Fatalf("keepalived had been started (%d) when the prober was asked, want the check first", startedAtCheck)
			}
			select {
			case <-r.proc:
			case <-time.After(20 * time.Second): // wall-clock: failure bound
				t.Fatal("keepalived was not started")
			}
			if p.calls != 1 {
				t.Fatalf("the prober was asked %d times at the start, want 1", p.calls)
			}
			if r.hasFile(flag) != tc.wantFlag {
				t.Fatalf("flag exists %v, want %v", r.hasFile(flag), tc.wantFlag)
			}
			// The fault follows the start line and precedes everything keepalived reports.
			if events[0].Event != ingressbundle.VIPEventStart {
				t.Fatalf("first line = %+v, want the start", events[0])
			}
			if tc.wantFlag {
				if ev := events[1]; ev.Event != ingressbundle.VIPEventFault || ev.Reason != ingressbundle.VIPReasonAddressInUse || !ev.Active {
					t.Fatalf("second line = %+v, want the address_in_use fault", ev)
				}
			}
			cancel()
			if code := waitCode(t, done); code != 0 {
				t.Fatalf("Run = %d, want 0 (stderr %q)", code, r.errw.String())
			}
		})
	}

	t.Run("a stop during the check ends Run without starting keepalived", func(t *testing.T) {
		r := newRig(t, "lines")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p := &fakeProber{script: []answer{{}}}
		p.onARP = cancel
		r.deps.Prober = p
		if code := waitCode(t, r.run(ctx)); code != 0 {
			t.Fatalf("Run = %d, want 0", code)
		}
		if len(r.proc) != 0 {
			t.Fatal("keepalived was started after a stop that came during the check")
		}
		evs := parseAll(t, r.out.lines())
		if last := evs[len(evs)-1]; last.State != ingressbundle.VIPStateStop {
			t.Fatalf("last line = %+v, want STOP", last)
		}
	})

	t.Run("while the address is in use the helper looks again and clears the fault", func(t *testing.T) {
		r := newRig(t, "lines")
		p := &fakeProber{script: []answer{{arp: true}, {}}}
		r.deps.Prober = p
		flag := r.deps.FlagPath
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := r.run(ctx)
		r.out.waitLines(t, 5)
		if !r.hasFile(flag) {
			t.Fatal("no flag while the address is in use")
		}
		r.clock.waitTimer(t, AddressRecheck)
		r.clock.Advance(AddressRecheck)
		lines := parseAll(t, r.out.waitLines(t, 6))
		cancel()
		if code := waitCode(t, done); code != 0 {
			t.Fatalf("Run = %d, want 0 (stderr %q)", code, r.errw.String())
		}
		var cleared bool
		for _, ev := range lines {
			if ev.Event == ingressbundle.VIPEventFault && ev.Reason == ingressbundle.VIPReasonAddressInUse && !ev.Active {
				cleared = true
			}
		}
		if !cleared {
			t.Fatalf("no cleared fault in %+v", lines)
		}
		if r.hasFile(flag) {
			t.Fatal("the flag is still there after the address was free")
		}
	})
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/proto/testsupport"
)

// sniffRig is a sniffing listener on a loopback port whose peek timers the test fires by hand.
type sniffRig struct {
	l      *sniffListener
	rt     *httpsRuntime
	timers *manualTimers
	addr   string
}

func newSniffRig(t *testing.T, maxPending int, tlsLive bool) *sniffRig {
	t.Helper()
	rt := newHTTPSRuntime()
	if tlsLive {
		cert, _ := genCert(t, "")
		rt.Install(tlsKindSupplied, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil })
	}
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	timers := newManualTimers()
	l := newSniffListenerCap(base, rt, timers.after, maxPending)
	t.Cleanup(func() { _ = l.Close() })
	return &sniffRig{l: l, rt: rt, timers: timers, addr: base.Addr().String()}
}

func (r *sniffRig) dial(t *testing.T) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

type acceptResult struct {
	conn net.Conn
	err  error
}

// acceptAsync runs one Accept and reports what it returned.
func acceptAsync(l net.Listener) <-chan acceptResult {
	ch := make(chan acceptResult, 1)
	go func() {
		c, err := l.Accept()
		ch <- acceptResult{c, err}
	}()
	return ch
}

func accept(t *testing.T, l net.Listener) net.Conn {
	t.Helper()
	r := testsupport.Await(t, "the listener to hand over a connection", acceptAsync(l))
	if r.err != nil {
		t.Fatalf("Accept: %v", r.err)
	}
	t.Cleanup(func() { _ = r.conn.Close() })
	return r.conn
}

// readErr reads from c until it fails and returns the error. Data counts as a failure of the test.
func readErr(t *testing.T, c net.Conn) error {
	t.Helper()
	ch := make(chan error, 1)
	go func() {
		var b [1]byte
		n, err := c.Read(b[:])
		if err == nil {
			err = errors.New("unexpected data: " + string(b[:n]))
		}
		ch <- err
	}()
	return testsupport.Await(t, "the connection to be closed", ch)
}

func TestSniffClassifiesByFirstByte(t *testing.T) {
	t.Run("a handshake byte with TLS live is a TLS connection", func(t *testing.T) {
		r := newSniffRig(t, maxPendingSniffs, true)
		if _, err := r.dial(t).Write([]byte{tlsHandshakeRecord}); err != nil {
			t.Fatal(err)
		}
		if _, ok := accept(t, r.l).(*tls.Conn); !ok {
			t.Fatal("a ClientHello with TLS live was not handed over as a *tls.Conn")
		}
	})
	t.Run("a handshake byte with TLS removed is plain", func(t *testing.T) {
		r := newSniffRig(t, maxPendingSniffs, true)
		r.rt.Remove()
		if _, err := r.dial(t).Write([]byte{tlsHandshakeRecord}); err != nil {
			t.Fatal(err)
		}
		if _, ok := accept(t, r.l).(*prefixConn); !ok {
			t.Fatal("a ClientHello with TLS not live was not handed over as a plain connection")
		}
	})
	t.Run("any other byte is plain and replayed", func(t *testing.T) {
		r := newSniffRig(t, maxPendingSniffs, true)
		if _, err := r.dial(t).Write([]byte("GET")); err != nil {
			t.Fatal(err)
		}
		c := accept(t, r.l)
		pc, ok := c.(*prefixConn)
		if !ok {
			t.Fatalf("a request line was handed over as %T, want *prefixConn", c)
		}
		var first [8]byte
		n, err := pc.Read(first[:])
		if err != nil || n != 1 || first[0] != 'G' {
			t.Fatalf("first Read = %q, %v; want the peeked byte alone", first[:n], err)
		}
		rest := make([]byte, 2)
		if _, err := io.ReadFull(pc, rest); err != nil || string(rest) != "ET" {
			t.Fatalf("the rest = %q, %v; want ET", rest, err)
		}
	})
}

func TestSniffClosesASilentConnectionWhenItsTimerFires(t *testing.T) {
	r := newSniffRig(t, maxPendingSniffs, true)
	silent := r.dial(t)
	if d := r.timers.WaitArmed(t); d != sniffTimeout {
		t.Fatalf("the peek timer was armed for %v, want %v", d, sniffTimeout)
	}
	accepted := acceptAsync(r.l)
	r.timers.FireAll()
	if err := readErr(t, silent); !errors.Is(err, io.EOF) {
		t.Fatalf("the silent client got %v, want EOF", err)
	}
	// The silent connection never reached Accept: the next one served is the one that spoke.
	if _, err := r.dial(t).Write([]byte("G")); err != nil {
		t.Fatal(err)
	}
	got := testsupport.Await(t, "the listener to hand over the connection that spoke", accepted)
	if got.err != nil {
		t.Fatal(got.err)
	}
	t.Cleanup(func() { _ = got.conn.Close() })
	var b [1]byte
	if _, err := io.ReadFull(got.conn, b[:]); err != nil || b[0] != 'G' {
		t.Fatalf("Accept returned a connection that read %q, %v; want the one that sent G", b[:], err)
	}
}

func TestSniffHandsOverAConnectionWhileAnEarlierOneIsSilent(t *testing.T) {
	r := newSniffRig(t, maxPendingSniffs, true)
	_ = r.dial(t) // A: silent
	r.timers.WaitArmed(t)
	if _, err := r.dial(t).Write([]byte("B")); err != nil { // B: speaks
		t.Fatal(err)
	}
	c := accept(t, r.l)
	var b [1]byte
	if _, err := io.ReadFull(c, b[:]); err != nil || b[0] != 'B' {
		t.Fatalf("Accept returned a connection that read %q, %v; want B's", b[:], err)
	}
	if n := r.timers.Pending(); n != 1 {
		t.Fatalf("%d timers are armed, want only the silent connection's", n)
	}
}

// capRig is a sniffing listener over a scripted base, so each connection reports the peer address the
// test gives it. Every real test client is on loopback, which the cap exempts, so the cap itself can
// only be reached with a made-up peer. The connections are in-memory pipes: no socket, no port, and
// closing one end is seen at the other at once.
type capRig struct {
	l      *sniffListener
	base   *scriptedListener
	timers *manualTimers
}

// loopbackPeer is a loopback peer address. publicAt gives a public one (RFC 5737 documentation range).
const loopbackPeer = "127.0.0.1:40001"

func newCapRig(t *testing.T, maxPending int) *capRig {
	t.Helper()
	base := newScriptedListener()
	timers := newManualTimers()
	l := newSniffListenerCap(base, newHTTPSRuntime(), timers.after, maxPending)
	t.Cleanup(func() { _ = l.Close() })
	return &capRig{l: l, base: base, timers: timers}
}

// connect hands the listener one connection from peer and waits until it is pending (its peek timer
// is armed). It returns the client end.
func (r *capRig) connect(t *testing.T, peer string) net.Conn {
	t.Helper()
	ap, err := netip.ParseAddrPort(peer)
	if err != nil {
		t.Fatal(err)
	}
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	r.base.push(t, acceptResult{conn: addrConn{Conn: server, remote: net.TCPAddrFromAddrPort(ap)}})
	if d := r.timers.WaitArmed(t); d != sniffTimeout {
		t.Fatalf("the peek timer was armed for %v, want %v", d, sniffTimeout)
	}
	return client
}

// publicAt is a public peer address with its own port, so a test can tell connections apart.
func publicAt(port int) string { return "203.0.113.7:" + strconv.Itoa(port) }

// speak sends one byte from client and returns the connection the listener hands over for it.
func (r *capRig) speak(t *testing.T, client net.Conn, b byte) net.Conn {
	t.Helper()
	accepted := acceptAsync(r.l)
	go func() { _, _ = client.Write([]byte{b}) }()
	got := testsupport.Await(t, "the listener to hand over the connection that spoke", accepted)
	if got.err != nil {
		t.Fatalf("Accept: %v", got.err)
	}
	t.Cleanup(func() { _ = got.conn.Close() })
	var first [1]byte
	if _, err := io.ReadFull(got.conn, first[:]); err != nil || first[0] != b {
		t.Fatalf("Accept returned a connection that read %q, %v; want the one that sent %q", first[:], err, b)
	}
	return got.conn
}

func TestSniffFullCapEvictsTheOldestPendingConnectionAndServesTheNewOne(t *testing.T) {
	r := newCapRig(t, 2)
	oldest := r.connect(t, publicAt(40001))
	second := r.connect(t, publicAt(40002))
	if n := r.l.pendingCount(); n != 2 {
		t.Fatalf("%d connections pending, want the two under the cap", n)
	}

	newest := r.connect(t, publicAt(40003))
	// The oldest silent connection is the one closed; the newcomer is not dropped.
	if err := readErr(t, oldest); !errors.Is(err, io.EOF) {
		t.Fatalf("the oldest pending connection got %v, want it closed to make room", err)
	}
	if n := r.l.pendingCount(); n != 2 {
		t.Fatalf("%d connections pending after the eviction, want the cap of 2", n)
	}
	r.speak(t, newest, 'N')
	if n := r.l.pendingCount(); n != 1 {
		t.Fatalf("%d connections pending after the newcomer spoke, want only the second", n)
	}

	// The second connection was spared, so it still works when it speaks.
	r.speak(t, second, 'S')
	if n := r.l.pendingCount(); n != 0 {
		t.Fatalf("%d connections pending, want 0", n)
	}
}

func TestSniffEvictsInArrivalOrder(t *testing.T) {
	r := newCapRig(t, 2)
	first := r.connect(t, publicAt(40001))
	second := r.connect(t, publicAt(40002))
	third := r.connect(t, publicAt(40003))
	if err := readErr(t, first); !errors.Is(err, io.EOF) {
		t.Fatalf("the first connection got %v, want it evicted", err)
	}
	fourth := r.connect(t, publicAt(40004))
	if err := readErr(t, second); !errors.Is(err, io.EOF) {
		t.Fatalf("the second connection got %v, want it evicted next", err)
	}
	// A connection that spoke has left the line: it is not evicted later in place of a silent one.
	r.speak(t, third, 'T')
	fifth := r.connect(t, publicAt(40005))
	r.speak(t, fourth, 'F')
	r.speak(t, fifth, 'V')
}

func TestSniffFullCapStillAdmitsLoopbackPeers(t *testing.T) {
	r := newCapRig(t, 2)
	a := r.connect(t, publicAt(40001))
	b := r.connect(t, publicAt(40002))

	// The health probe dials 127.0.0.1: it is admitted over the full cap, nobody is evicted for it,
	// and it is served.
	probe := r.connect(t, loopbackPeer)
	if n := r.l.pendingCount(); n != 3 {
		t.Fatalf("%d connections pending, want the two public ones and the loopback one", n)
	}
	r.speak(t, probe, 'P')

	// Loopback connections do not use up the cap either: more of them leave the public ones alone.
	for range 3 {
		r.connect(t, loopbackPeer)
	}
	if n := r.l.pendingCount(); n != 5 {
		t.Fatalf("%d connections pending, want the two public ones and three loopback ones", n)
	}
	r.speak(t, a, 'A')
	r.speak(t, b, 'B')
}

func TestSniffLoopbackPeersDoNotCrowdOutOthers(t *testing.T) {
	r := newCapRig(t, 2)
	for range 4 {
		r.connect(t, loopbackPeer)
	}
	// Four loopback connections wait, and a public one is still admitted without evicting any of them.
	public := r.connect(t, publicAt(40001))
	if n := r.l.pendingCount(); n != 5 {
		t.Fatalf("%d connections pending, want all five", n)
	}
	r.speak(t, public, 'P')
}

func TestSniffCapFreesWhenTheTimersFire(t *testing.T) {
	r := newCapRig(t, 2)
	a := r.connect(t, publicAt(40001))
	b := r.connect(t, publicAt(40002))
	r.timers.FireAll()
	for _, c := range []net.Conn{a, b} {
		if err := readErr(t, c); !errors.Is(err, io.EOF) {
			t.Fatalf("a silent connection got %v, want EOF", err)
		}
	}
	if n := r.l.pendingCount(); n != 0 {
		t.Fatalf("%d connections pending after the timers fired, want 0", n)
	}
	// Room again: the next connection is taken and served, with nothing evicted.
	r.speak(t, r.connect(t, publicAt(40003)), 'N')
}

func TestSniffCloseClosesPendingAndAcceptReturnsErrClosed(t *testing.T) {
	r := newSniffRig(t, maxPendingSniffs, true)
	silent := r.dial(t)
	r.timers.WaitArmed(t)
	accepted := acceptAsync(r.l)

	if err := r.l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := readErr(t, silent); !errors.Is(err, io.EOF) {
		t.Fatalf("a pending connection got %v after Close, want EOF", err)
	}
	got := testsupport.Await(t, "Accept to return after Close", accepted)
	if !errors.Is(got.err, net.ErrClosed) {
		t.Fatalf("Accept after Close returned %v, want net.ErrClosed", got.err)
	}
	if _, err := r.l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("a later Accept returned %v, want net.ErrClosed", err)
	}
	if err := r.l.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
}

// scriptedListener returns what the test pushes, one result per Accept, so the accept loop is driven
// step by step.
type scriptedListener struct {
	steps chan acceptResult
	done  chan struct{}
	once  chan struct{}
}

func newScriptedListener() *scriptedListener {
	return &scriptedListener{steps: make(chan acceptResult), done: make(chan struct{}), once: make(chan struct{}, 1)}
}

func (s *scriptedListener) Accept() (net.Conn, error) {
	select {
	case r := <-s.steps:
		return r.conn, r.err
	case <-s.done:
		return nil, net.ErrClosed
	}
}

func (s *scriptedListener) Close() error {
	select {
	case s.once <- struct{}{}:
		close(s.done)
	default:
	}
	return nil
}

func (s *scriptedListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func (s *scriptedListener) push(t *testing.T, r acceptResult) {
	t.Helper()
	select {
	case s.steps <- r:
	case <-testsupport.Context(t).Done():
		t.Fatal("the accept loop stopped asking for connections")
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestSniffForwardsAPermanentAcceptErrorAndRetriesATemporaryOne(t *testing.T) {
	base := newScriptedListener()
	timers := newManualTimers()
	l := newSniffListenerCap(base, newHTTPSRuntime(), timers.after, maxPendingSniffs)
	t.Cleanup(func() { _ = l.Close() })
	accepted := acceptAsync(l)

	// A temporary error is retried after a backoff that doubles from 5 ms.
	base.push(t, acceptResult{err: timeoutError{}})
	if d := timers.WaitArmed(t); d != 5*time.Millisecond {
		t.Fatalf("first backoff = %v, want 5ms", d)
	}
	timers.FireAll()
	base.push(t, acceptResult{err: &net.OpError{Op: "accept", Err: os.NewSyscallError("accept", syscall.EMFILE)}})
	if d := timers.WaitArmed(t); d != 10*time.Millisecond {
		t.Fatalf("second backoff = %v, want 10ms", d)
	}
	timers.FireAll()

	// A connection after the errors is served, and the backoff starts over.
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	base.push(t, acceptResult{conn: server})
	go func() { _, _ = client.Write([]byte("G")) }()
	got := testsupport.Await(t, "the listener to hand over the connection that followed the errors", accepted)
	if got.err != nil {
		t.Fatalf("Accept: %v", got.err)
	}
	t.Cleanup(func() { _ = got.conn.Close() })
	if d := timers.WaitArmed(t); d != sniffTimeout {
		t.Fatalf("the connection's peek timer = %v, want %v", d, sniffTimeout)
	}
	base.push(t, acceptResult{err: syscall.ECONNABORTED})
	if d := timers.WaitArmed(t); d != 5*time.Millisecond {
		t.Fatalf("backoff after a success = %v, want it reset to 5ms", d)
	}
	timers.FireAll()

	// A permanent error is forwarded to Accept, and stays the answer.
	boom := errors.New("the listener is broken")
	next := acceptAsync(l)
	base.push(t, acceptResult{err: boom})
	if r := testsupport.Await(t, "Accept to forward the permanent error", next); !errors.Is(r.err, boom) {
		t.Fatalf("Accept returned %v, want %v", r.err, boom)
	}
	if _, err := l.Accept(); !errors.Is(err, boom) {
		t.Fatalf("a later Accept returned %v, want the same error", err)
	}
}

func TestPrefixConnForwardsCloseWrite(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	accepted := acceptAsync(base)
	client, err := net.Dial("tcp", base.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	got := testsupport.Await(t, "the loopback connection", accepted)
	if got.err != nil {
		t.Fatal(got.err)
	}
	t.Cleanup(func() { _ = got.conn.Close() })

	var conn net.Conn = &prefixConn{Conn: got.conn}
	cw, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("the plain-branch connection hides CloseWrite from net/http")
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := readErr(t, client); !errors.Is(err, io.EOF) {
		t.Fatalf("the peer got %v after CloseWrite, want EOF", err)
	}
	// Only the write side is closed: the connection still reads.
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := io.ReadFull(conn, b[:]); err != nil || b[0] != 'x' {
		t.Fatalf("read after CloseWrite = %q, %v; want x", b[:], err)
	}
}

func TestServeShutdownClosesPendingSniffs(t *testing.T) {
	cfg := testConfig(t, map[string]string{"TIERPLAN_DATA_DIR": t.TempDir() + "/data"})
	srv, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	silent, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.serve(ctx, ln) }()
	cancel()
	if err := testsupport.Await(t, "serve to return after the context ended", done); err != nil {
		t.Fatalf("serve: %v", err)
	}
	// The client sent nothing, so its connection was waiting for a first byte. It is closed, whether
	// the listener had taken it yet or not.
	if err := readErr(t, silent); err == nil {
		t.Fatal("the silent connection was left open")
	}
}

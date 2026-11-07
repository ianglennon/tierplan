// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/proto/testsupport"
)

// stepClock is a clock the test moves. After arms a timer and announces it, so a test that moves the
// clock knows the loop under test is waiting on it and never sleeps to find out. It is the janitor
// scheduler's schedClock, kept here because that one lives in another package's tests.
type stepClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []stepTimer
	armed  chan struct{}
}

type stepTimer struct {
	at time.Time
	ch chan time.Time
}

func newStepClock(now time.Time) *stepClock {
	return &stepClock{now: now, armed: make(chan struct{}, 4096)}
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, stepTimer{at: c.now.Add(d), ch: ch})
	c.armed <- struct{}{}
	return ch
}

// Advance moves the clock on by d and fires every timer that is due.
func (c *stepClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.timers[:0]
	for _, t := range c.timers {
		if !t.at.After(c.now) {
			t.ch <- c.now
			continue
		}
		kept = append(kept, t)
	}
	c.timers = kept
}

// WaitArmed waits for the next timer to be armed. A hang fails with a deadline naming what was awaited.
func (c *stepClock) WaitArmed(t *testing.T) {
	t.Helper()
	testsupport.Await(t, "the loop under test to arm its next timer", c.armed)
}

// Tick waits for a timer to be armed and then moves the clock on by d.
func (c *stepClock) Tick(t *testing.T, d time.Duration) {
	t.Helper()
	c.WaitArmed(t)
	c.Advance(d)
}

// genCert makes a certificate of its own, with its own key, that a client can verify with a pool
// and ServerName "localhost". commonName is added to the names, so two certificates differ in more
// than their serial number. Plans 05-08 reuse it.
func genCert(t *testing.T, commonName string) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	want := names{dns: []string{"localhost"}, ips: []string{"127.0.0.1", "::1"}}
	if commonName != "" && commonName != "localhost" {
		want.dns = sortedKeys(map[string]bool{"localhost": true, commonName: true})
	}
	der, err := signCertificate(key, want, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf
}

// manualTimers stands in for time.AfterFunc under the sniffing listener: nothing waits on real time.
// Each armed timer is recorded and announced, so a test that fires them knows the listener has armed
// them and never sleeps to find out. Its after method matches afterFunc.
type manualTimers struct {
	mu     sync.Mutex
	timers []*manualTimer
	armed  chan time.Duration
}

type manualTimer struct {
	d       time.Duration
	f       func()
	stopped bool
	fired   bool
}

func newManualTimers() *manualTimers {
	return &manualTimers{armed: make(chan time.Duration, 4096)}
}

// after arms a timer. The stop func it returns behaves as time.Timer.Stop does: true when it stopped
// the timer before it fired, false when the timer had already fired or was stopped before.
func (m *manualTimers) after(d time.Duration, f func()) func() bool {
	t := &manualTimer{d: d, f: f}
	m.mu.Lock()
	m.timers = append(m.timers, t)
	m.mu.Unlock()
	m.armed <- d
	return func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		if t.stopped || t.fired {
			return false
		}
		t.stopped = true
		return true
	}
}

// WaitArmed waits for the next timer to be armed and returns its duration.
func (m *manualTimers) WaitArmed(t *testing.T) time.Duration {
	t.Helper()
	return testsupport.Await(t, "the listener to arm a timer", m.armed)
}

// Pending is how many timers are armed and neither stopped nor fired.
func (m *manualTimers) Pending() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, t := range m.timers {
		if !t.stopped && !t.fired {
			n++
		}
	}
	return n
}

// FireAll runs the callback of every timer that is neither stopped nor fired.
func (m *manualTimers) FireAll() {
	m.mu.Lock()
	var due []*manualTimer
	for _, t := range m.timers {
		if !t.stopped && !t.fired {
			t.fired = true
			due = append(due, t)
		}
	}
	m.mu.Unlock()
	for _, t := range due {
		t.f()
	}
}

// fakeAddrListener wraps a listener so every connection it accepts reports a chosen peer address.
// Every Go test client is on loopback, so without it the gate's non-loopback branches could only be
// reached by calling the handler directly, never over a real socket through the sniffing listener.
type fakeAddrListener struct {
	net.Listener
	remote net.Addr
}

// defaultFakePeer is a documentation-range address (RFC 5737): public, never loopback.
const defaultFakePeer = "203.0.113.9:40000"

// newFakeAddrListener reports peer (default defaultFakePeer) as the remote address of every
// connection accepted from ln.
func newFakeAddrListener(t *testing.T, ln net.Listener, peer string) fakeAddrListener {
	t.Helper()
	if peer == "" {
		peer = defaultFakePeer
	}
	ap, err := netip.ParseAddrPort(peer)
	if err != nil {
		t.Fatal(err)
	}
	return fakeAddrListener{Listener: ln, remote: net.TCPAddrFromAddrPort(ap)}
}

func (l fakeAddrListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return addrConn{Conn: c, remote: l.remote}, nil
}

type addrConn struct {
	net.Conn
	remote net.Addr
}

func (c addrConn) RemoteAddr() net.Addr { return c.remote }

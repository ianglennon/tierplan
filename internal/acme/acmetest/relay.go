// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package acmetest

import (
	"context"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Relay is a TCP port reserved before the server it forwards to exists. Pebble has to be told the port
// it validates HTTP-01 challenges on when it starts, and the TierPlan server only learns its own port
// while it is being built, so the test reserves a port here, hands it to Pebble, and points the relay at
// the server once that is listening.
type Relay struct {
	ln     net.Listener
	target atomic.Pointer[string]

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// dialTimeout bounds one connection to the target; a target on the same machine answers at once, so
// the bound only decides how long a broken setup takes to show.
const dialTimeout = 10 * time.Second

// NewRelay binds 0.0.0.0:0 and forwards what it accepts, until the test ends. The port is on every
// interface because the connection comes from a container, through the engine's host gateway.
func NewRelay(t testing.TB) *Relay {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:0") //nolint:gosec // a container reaches it through the host gateway, for this test only
	if err != nil {
		t.Fatalf("acmetest: reserve the relay port: %v", err)
	}
	r := &Relay{ln: ln, conns: map[net.Conn]struct{}{}}
	r.wg.Add(1)
	go r.accept()
	t.Cleanup(r.close)
	return r
}

// Port is the port the relay listens on.
func (r *Relay) Port() int {
	addr, ok := r.ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0
	}
	return addr.Port
}

// SetTarget sets the address new connections are forwarded to, such as "127.0.0.1:7480". A connection
// that arrives before a target is set is closed.
func (r *Relay) SetTarget(addr string) { r.target.Store(&addr) }

func (r *Relay) accept() {
	defer r.wg.Done()
	for {
		conn, err := r.ln.Accept()
		if err != nil {
			return // the listener was closed
		}
		if !r.track(conn) {
			_ = conn.Close()
			return
		}
		r.wg.Add(1)
		go r.forward(conn)
	}
}

// track records conn so close can end it, and reports false once the relay is closed.
func (r *Relay) track(conn net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	r.conns[conn] = struct{}{}
	return true
}

func (r *Relay) untrack(conn net.Conn) {
	r.mu.Lock()
	delete(r.conns, conn)
	r.mu.Unlock()
	_ = conn.Close()
}

func (r *Relay) forward(in net.Conn) {
	defer r.wg.Done()
	defer r.untrack(in)
	target := r.target.Load()
	if target == nil {
		return
	}
	dialer := net.Dialer{Timeout: dialTimeout}
	out, err := dialer.DialContext(context.Background(), "tcp", *target)
	if err != nil {
		return
	}
	if !r.track(out) {
		_ = out.Close()
		return
	}
	defer r.untrack(out)

	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		// A half-close lets the other side finish reading what is in flight.
		if tcp, ok := dst.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(out, in)
	go pipe(in, out)
	<-done
	<-done
}

func (r *Relay) close() {
	r.mu.Lock()
	r.closed = true
	for c := range r.conns {
		_ = c.Close()
	}
	r.mu.Unlock()
	_ = r.ln.Close()
	r.wg.Wait()
}

// String is the relay's port, for messages.
func (r *Relay) String() string { return "relay :" + strconv.Itoa(r.Port()) }

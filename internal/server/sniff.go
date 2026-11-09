// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"bufio"
	"bytes"
	"container/list"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"time"
)

// sniffTimeout is how long a connection may stay silent before the listener closes it. It matches
// readHeaderTimeout, which a plain connection already gets once it is served, so a browser's
// speculative connection is not cut earlier than before.
const sniffTimeout = 10 * time.Second

// maxPendingSniffs is how many connections may wait for their first byte at once. Each costs one
// goroutine and a few KiB. When the limit is reached the oldest waiting connection is closed to make
// room, so a flood of silent sockets cannot lock everyone else out. A loopback peer is never counted
// against the limit and never closed for it: the container's own health probe dials 127.0.0.1.
const maxPendingSniffs = 1024

// acceptBackoffMin and acceptBackoffMax bound the wait after a temporary Accept error (a client that
// aborted, or the process out of file descriptors). The wait doubles from the first to the second.
const (
	acceptBackoffMin = 5 * time.Millisecond
	acceptBackoffMax = time.Second
)

// tlsHandshakeRecord is the first byte of a TLS ClientHello (the handshake record type).
const tlsHandshakeRecord = 0x16

// afterFunc arms a timer that runs f after d and returns a func that stops it. The stop func reports
// true when it stopped the timer before it fired, as time.Timer.Stop does. Production uses
// time.AfterFunc; a test passes a timer it fires by hand.
type afterFunc func(d time.Duration, f func()) (stop func() bool)

func realAfter(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

// prefixConn is a plain connection whose first byte was read to decide which branch it is on. Read
// gives that byte back before anything else.
type prefixConn struct {
	net.Conn
	b   byte
	has bool
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if c.has && len(p) > 0 {
		p[0], c.has = c.b, false
		return 1, nil
	}
	return c.Conn.Read(p)
}

// CloseWrite half-closes the connection. net/http calls it, through an interface check, when it ends
// a connection after an error response, so the client can read the response before the close.
// Embedding net.Conn hides *net.TCPConn's method, so it is forwarded here.
func (c *prefixConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

var _ interface{ CloseWrite() error } = (*prefixConn)(nil)

// sniffListener is the one listener every mode serves through. It tells TLS from plain HTTP on each
// connection by the first byte. A TLS connection is handed to the TLS server; a plain one is read
// only far enough to see its request head, and is handed on only for an ACME challenge. Any other
// plain connection is closed with no bytes written.
//
// Accept never reads: a goroutine per connection waits for the first byte under a timer, so one
// silent client cannot hold up the rest.
type sniffListener struct {
	base  net.Listener
	rt    *httpsRuntime
	after afterFunc
	ready chan net.Conn

	// done is closed by Close. failed is closed, with err set first, when the base listener
	// returns an error that is not a Close.
	done      chan struct{}
	closeOnce sync.Once
	failed    chan struct{}
	failOnce  sync.Once
	err       error

	// pending holds every connection waiting for its first byte. The value is the connection's
	// place in order, or nil for a loopback peer, which is outside the limit. order holds the
	// others, oldest first, so a full limit can close the oldest.
	mu         sync.Mutex
	pending    map[net.Conn]*list.Element
	order      *list.List
	closed     bool
	maxPending int
	wg         sync.WaitGroup
}

// newSniffListener serves base through rt. A nil after means real timers.
func newSniffListener(base net.Listener, rt *httpsRuntime, after afterFunc) *sniffListener {
	return newSniffListenerCap(base, rt, after, maxPendingSniffs)
}

// newSniffListenerCap is newSniffListener with its own limit on the connections waiting for a first
// byte, so a test can use a small one.
func newSniffListenerCap(base net.Listener, rt *httpsRuntime, after afterFunc, maxPending int) *sniffListener {
	if after == nil {
		after = realAfter
	}
	l := &sniffListener{
		base:       base,
		rt:         rt,
		after:      after,
		ready:      make(chan net.Conn),
		done:       make(chan struct{}),
		failed:     make(chan struct{}),
		pending:    make(map[net.Conn]*list.Element),
		order:      list.New(),
		maxPending: maxPending,
	}
	l.wg.Add(1)
	go l.acceptLoop()
	return l
}

// isTemporaryAcceptError reports an Accept error that clears by itself: a timeout, a connection the
// client aborted, or the process or the system out of file descriptors.
func isTemporaryAcceptError(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE)
}

// acceptLoop only accepts; it never reads, so a client that sends nothing cannot hold up the next.
func (l *sniffListener) acceptLoop() {
	defer l.wg.Done()
	backoff := acceptBackoffMin
	for {
		c, err := l.base.Accept()
		if err != nil {
			select {
			case <-l.done:
				return
			default:
			}
			if !isTemporaryAcceptError(err) {
				l.fail(err)
				return
			}
			if !l.sleep(backoff) {
				return
			}
			backoff = min(backoff*2, acceptBackoffMax)
			continue
		}
		backoff = acceptBackoffMin
		// No per-address limit: Docker's proxy and swarm ingress make every client one address.
		admitted, evicted := l.register(c)
		if evicted != nil {
			_ = evicted.Close()
		}
		if !admitted {
			_ = c.Close()
			continue
		}
		l.wg.Add(1)
		go l.classify(c)
	}
}

// sleep waits d through the injected timer. It reports false when the listener was closed first.
func (l *sniffListener) sleep(d time.Duration) bool {
	woke := make(chan struct{})
	stop := l.after(d, func() { close(woke) })
	select {
	case <-woke:
		return true
	case <-l.done:
		stop()
		return false
	}
}

func (l *sniffListener) fail(err error) {
	l.failOnce.Do(func() {
		l.err = err
		close(l.failed)
	})
}

// register adds c to the connections waiting for their first byte. It reports false when c must be
// closed instead, which happens only when the listener is closed. When the limit is full and c is
// not a loopback peer, the oldest waiting connection is removed to make room and returned; the
// caller closes it. A loopback peer is always admitted and is not counted against the limit.
func (l *sniffListener) register(c net.Conn) (admitted bool, evicted net.Conn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false, nil
	}
	if isLoopbackConn(c) {
		l.pending[c] = nil
		return true, nil
	}
	if l.order.Len() >= l.maxPending {
		oldest := l.order.Front()
		if v, ok := l.order.Remove(oldest).(net.Conn); ok {
			evicted = v
			delete(l.pending, v)
		}
	}
	l.pending[c] = l.order.PushBack(c)
	return true, evicted
}

// isLoopbackPeer reports whether remoteAddr, the TCP peer, is a loopback address.
// It is judged from the connection, never from a header.
func isLoopbackPeer(remoteAddr string) bool {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return false
	}
	return ap.Addr().Unmap().IsLoopback()
}

// isLoopbackConn reports whether c's TCP peer is a loopback address.
func isLoopbackConn(c net.Conn) bool {
	a := c.RemoteAddr()
	return a != nil && isLoopbackPeer(a.String())
}

func (l *sniffListener) forget(c net.Conn) {
	l.mu.Lock()
	if e, ok := l.pending[c]; ok {
		if e != nil {
			l.order.Remove(e)
		}
		delete(l.pending, c)
	}
	l.mu.Unlock()
}

// pendingCount is how many connections are waiting for their first byte.
func (l *sniffListener) pendingCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.pending)
}

// classify reads the first byte of c and hands it on as a TLS connection, or reads a plain request
// head and hands the connection on only when that head is an ACME challenge. Everything else is
// closed with nothing written: no 400, no redirect, no error page (D-01). The connection stays in the
// pending set, under the same timer, until it has been classified (Pitfalls 1, 6 and 7).
func (l *sniffListener) classify(c net.Conn) {
	defer l.wg.Done()
	// The timer leaves the pending set before it closes, so the count is already down when the
	// client sees the close.
	stop := l.after(sniffTimeout, func() {
		l.forget(c)
		_ = c.Close()
	})
	// drop closes c without writing, leaving the pending set exactly once.
	drop := func() {
		stop()
		l.forget(c)
		_ = c.Close()
	}
	var b [1]byte
	_, err := io.ReadFull(c, b[:])
	if err != nil {
		drop()
		return
	}
	if b[0] == tlsHandshakeRecord {
		// stop runs before the connection is handed on, so the timer can never close a connection
		// that is serving a request (an events stream outlives it).
		if !stop() {
			l.forget(c)
			_ = c.Close()
			return
		}
		// It has spoken, so it is no longer waiting: leave the set before anything else, so a full
		// limit can never close a connection that is already being handed on.
		l.forget(c)
		l.deliver(tls.Server(&prefixConn{Conn: c, b: b[0], has: true}, l.rt.ServerConfig()))
		return
	}
	// Plain: read the request head ourselves, so net/http never gets the chance to write a
	// 400, 431 or 501 to a connection that is not a challenge.
	rec := &recorder{c: c}
	rec.buf.WriteByte(b[0])
	br := bufio.NewReader(io.MultiReader(bytes.NewReader(b[:]), io.LimitReader(rec, plainHeadLimit-1)))
	req, err := http.ReadRequest(br)
	if err != nil || !strings.HasPrefix(req.URL.Path, acmeChallengePrefix) {
		drop()
		return
	}
	if !stop() {
		l.forget(c)
		_ = c.Close()
		return
	}
	l.forget(c)
	l.deliver(newReplayConn(c, rec.buf.Bytes()))
}

// deliver queues a classified connection for Accept, or closes it when the listener is closed first.
func (l *sniffListener) deliver(out net.Conn) {
	select {
	case l.ready <- out:
	case <-l.done:
		_ = out.Close()
	}
}

// Accept returns the next classified connection.
func (l *sniffListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ready:
		return c, nil
	case <-l.failed:
		return nil, l.err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close stops accepting, closes every connection still waiting for its first byte and waits for the
// goroutines of the listener. It is safe to call more than once.
func (l *sniffListener) Close() error {
	var err error
	l.closeOnce.Do(func() {
		close(l.done)
		err = l.base.Close()
		l.mu.Lock()
		l.closed = true
		for c := range l.pending {
			_ = c.Close()
		}
		l.mu.Unlock()
		l.wg.Wait()
	})
	return err
}

// Addr is the address of the base listener.
func (l *sniffListener) Addr() net.Addr { return l.base.Addr() }

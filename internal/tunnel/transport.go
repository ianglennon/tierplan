// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hashicorp/yamux"

	"github.com/ianglennon/tierplan/internal/engine"
	"github.com/ianglennon/tierplan/proto/wire"
)

// StreamTransport carries Docker HTTP to one agent: every dial opens a stream in
// the agent's session and writes the stream kind first. It is an
// engine.Transport, so dockerx runs over it unchanged; the network and address
// the HTTP client passes are ignored, as for the local socket.
type StreamTransport struct {
	s *Session
}

var _ engine.Transport = StreamTransport{}

// DialContext opens a stream to the agent. A closed session answers
// ErrAgentGone at once, so a request never waits on a connection that is dead
// (D-42).
func (t StreamTransport) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	if t.s == nil {
		return nil, fmt.Errorf("%w: no session", ErrAgentGone)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if t.s.isGone() {
		return nil, fmt.Errorf("open a stream to agent %s: %w", t.s.AgentID, ErrAgentGone)
	}
	// A slot is taken before the stream is opened and returned when the
	// connection is closed. A full session says so at once instead of queueing.
	if !t.s.acquire() {
		return nil, fmt.Errorf("agent %s: %w (%d requests are in flight)", t.s.AgentID, ErrBusy, MaxSessionStreams)
	}
	st, err := t.s.ym.OpenStream()
	if err != nil {
		t.s.releaseStream()
		return nil, t.wrap(err)
	}
	conn := &streamConn{Conn: st, s: t.s}
	if _, err := conn.Write([]byte{wire.StreamKindDockerHTTP}); err != nil {
		_ = conn.Close()
		return nil, t.wrap(err)
	}
	return conn, nil
}

// wrap marks a dead-session error as ErrAgentGone and keeps any other error as
// it is. It is for the errors of opening a stream and writing its first byte.
func (t StreamTransport) wrap(err error) error {
	if t.s.gone(err) || fromConnection(err) {
		return fmt.Errorf("open a stream to agent %s: %w: %w", t.s.AgentID, ErrAgentGone, err)
	}
	return fmt.Errorf("open a stream to agent %s: %w", t.s.AgentID, err)
}

// muxErrors are the errors yamux makes itself. Opening a stream or writing to one
// fails with one of these, or with the error the connection under the session gave
// when yamux wrote to it.
var muxErrors = []error{
	yamux.ErrSessionShutdown, yamux.ErrStreamsExhausted, yamux.ErrRemoteGoAway,
	yamux.ErrStreamClosed, yamux.ErrConnectionReset, yamux.ErrConnectionWriteTimeout,
	yamux.ErrKeepAliveTimeout, yamux.ErrTimeout, yamux.ErrInvalidVersion,
	yamux.ErrInvalidMsgType, yamux.ErrDuplicateStream, yamux.ErrRecvWindowExceeded,
	yamux.ErrUnexpectedFlag,
}

// fromConnection reports whether an error of opening or first writing a stream came
// from the connection under the session rather than from yamux. Such an error means
// the agent is gone even when the session does not say so yet. yamux hands the
// connection's write error to the writer before it closes the session, so for a moment
// a dial holds a raw socket error while the session still reads as open (CI run
// 39162361452: one of twenty dials during a close was not ErrAgentGone). yamux always
// ends the session after such an error. It is not used for reads, where io.EOF ends
// a body normally.
func fromConnection(err error) bool {
	if err == nil || errors.Is(err, ErrAgentGone) || errors.Is(err, ErrBusy) {
		return false
	}
	for _, own := range muxErrors {
		if errors.Is(err, own) {
			return false
		}
	}
	return true
}

// streamConn is a Docker stream whose errors say so when the agent is gone. A
// request blocked on a read when the tunnel drops is woken by the multiplexer
// with a bare EOF or a closed-stream error; wrapped in ErrAgentGone it is a typed
// reason a caller can tell from a normal end of body, and can retry (a read) or
// must not (a mutation) as D-42 requires.
type streamConn struct {
	net.Conn
	s *Session

	// read counts the bytes received for the exchange in progress, and is reset
	// when the next request is written, so a keep-alive connection is capped per
	// answer and not for its lifetime. uncapped is set for the events feed, a log
	// follow and an exec, which are meant to outlast any cap.
	read     atomic.Int64
	uncapped atomic.Bool
	tooLarge atomic.Bool
	released sync.Once

	// hijacked is set, once and for good, when the answer to an exec start is the
	// upgrade (101): from then on the connection carries a person's keystrokes, not
	// HTTP, and Write no longer looks for a request line, so a keystroke frame that
	// reads like one cannot release the slot or put the cap back (T-04-13).
	// awaiting is set when an exec start is written and cleared when the start of the
	// answer is known; sniff holds the first bytes of that answer (under mu).
	hijacked atomic.Bool
	awaiting atomic.Bool
	sniff    []byte

	// mu guards holding: whether this connection holds one of the session's
	// interactive slots, which it takes when a log follow or an exec starts and
	// gives back when a request of another kind follows on the connection or the
	// connection closes, exactly once either way.
	mu      sync.Mutex
	holding bool
	closed  bool
}

func (c *streamConn) Read(p []byte) (int, error) {
	if c.tooLarge.Load() {
		return 0, c.errTooLarge()
	}
	n, err := c.Conn.Read(p)
	if n > 0 && c.awaiting.Load() {
		c.sniffUpgrade(p[:n])
	}
	if n > 0 && !c.uncapped.Load() && c.s.maxResponse > 0 && c.read.Add(int64(n)) > c.s.maxResponse {
		// Close the stream so the agent stops sending. The session carries on.
		c.tooLarge.Store(true)
		_ = c.Conn.Close()
		return 0, c.errTooLarge()
	}
	return n, c.s.mark(err)
}

func (c *streamConn) Write(p []byte) (int, error) {
	// Each request starts a new exchange. The events feed, a log follow and an exec
	// are the answers that are meant to be endless; the last two also take a slot of
	// the session's interactive budget, and a full budget fails the write with
	// ErrBusy before anything of the request is sent.
	//
	// Once an exec start has been answered with the upgrade the connection is a shell
	// and carries raw bytes, so nothing written to it is classified again: a keystroke
	// frame that looks like a request line must not release the slot or cap the
	// stream. A start that was not upgraded leaves the connection speaking HTTP.
	if !c.hijacked.Load() {
		if kind, ok := startsStreamRequest(p); ok {
			if err := c.enter(kind); err != nil {
				return 0, err
			}
			c.read.Store(0)
			c.uncapped.Store(kind.uncapped())
			if kind == streamExec {
				c.mu.Lock()
				c.sniff = c.sniff[:0]
				c.mu.Unlock()
				c.awaiting.Store(true)
			}
		}
	}
	n, err := c.Conn.Write(p)
	return n, c.s.mark(err)
}

// upgradeLine is how the answer to an upgraded exec start begins.
const upgradeLine = "HTTP/1.1 101"

// sniffUpgrade looks at the start of the answer to an exec start, which arrives in
// pieces of any size, and marks the connection hijacked when it is the upgrade. It
// stops looking at the first byte that cannot be the upgrade.
func (c *streamConn) sniffUpgrade(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.awaiting.Load() {
		return
	}
	c.sniff = append(c.sniff, b[:min(len(b), len(upgradeLine)-len(c.sniff))]...)
	switch {
	case !strings.HasPrefix(upgradeLine, string(c.sniff)):
		c.awaiting.Store(false)
		c.sniff = nil
	case len(c.sniff) == len(upgradeLine):
		c.hijacked.Store(true)
		c.awaiting.Store(false)
		c.sniff = nil
	}
}

// enter moves the connection's interactive slot to what the next request needs.
func (c *streamConn) enter(kind streamKind) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case kind.interactive() && !c.holding:
		if c.closed {
			return net.ErrClosed
		}
		if !c.s.acquireInteractive() {
			return fmt.Errorf("agent %s: %w (%d log follows and shells are open)", c.s.AgentID, ErrBusy, MaxInteractiveStreams)
		}
		c.holding = true
	case !kind.interactive() && c.holding:
		c.holding = false
		c.s.releaseInteractive()
	}
	return nil
}

// Close closes the stream and frees its slots in the session's budgets.
func (c *streamConn) Close() error {
	err := c.Conn.Close()
	c.mu.Lock()
	c.closed = true
	if c.holding {
		c.holding = false
		c.s.releaseInteractive()
	}
	c.mu.Unlock()
	c.released.Do(c.s.releaseStream)
	return err
}

func (c *streamConn) errTooLarge() error {
	return fmt.Errorf("agent %s: %w (more than %d bytes)", c.s.AgentID, ErrTooLarge, c.s.maxResponse)
}

// mark wraps err in ErrAgentGone when the session is gone, and returns it
// unchanged otherwise.
func (s *Session) mark(err error) error {
	if err == nil || !s.gone(err) {
		return err
	}
	return fmt.Errorf("agent %s: %w: %w", s.AgentID, ErrAgentGone, err)
}

// gone reports whether err means the agent's session is closed or closing. A
// deadline that passed on a live session does not: it is the caller's own limit.
func (s *Session) gone(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() && !s.isGone() {
		return false
	}
	return s.isGone() || errors.Is(err, yamux.ErrSessionShutdown) || errors.Is(err, net.ErrClosed)
}

// isGone is true as soon as the session has started to close, before the
// goroutine that closes done has run.
func (s *Session) isGone() bool {
	select {
	case <-s.done:
		return true
	default:
		return s.ym.IsClosed()
	}
}

// Describe names the agent for logs and errors.
func (t StreamTransport) Describe() string {
	if t.s == nil {
		return "tunnel://"
	}
	return "tunnel://" + t.s.AgentID
}

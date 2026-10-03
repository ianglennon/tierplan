// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/hashicorp/yamux"

	"github.com/ianglennon/bayplan/internal/engine"
	"github.com/ianglennon/bayplan/proto/wire"
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
	st, err := t.s.ym.OpenStream()
	if err != nil {
		return nil, t.wrap(err)
	}
	conn := streamConn{Conn: st, s: t.s}
	if _, err := conn.Write([]byte{wire.StreamKindDockerHTTP}); err != nil {
		_ = st.Close()
		return nil, t.wrap(err)
	}
	return conn, nil
}

// wrap marks a dead-session error as ErrAgentGone and keeps any other error as
// it is.
func (t StreamTransport) wrap(err error) error {
	if t.s.gone(err) {
		return fmt.Errorf("open a stream to agent %s: %w: %w", t.s.AgentID, ErrAgentGone, err)
	}
	return fmt.Errorf("open a stream to agent %s: %w", t.s.AgentID, err)
}

// streamConn is a Docker stream whose errors say so when the agent is gone. A
// request blocked on a read when the tunnel drops is woken by the multiplexer
// with a bare EOF or a closed-stream error; wrapped in ErrAgentGone it is a typed
// reason a caller can tell from a normal end of body, and can retry (a read) or
// must not (a mutation) as D-42 requires.
type streamConn struct {
	net.Conn
	s *Session
}

func (c streamConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	return n, c.s.mark(err)
}

func (c streamConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	return n, c.s.mark(err)
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

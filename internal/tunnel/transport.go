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
	select {
	case <-t.s.done:
		return nil, fmt.Errorf("open a stream to agent %s: %w", t.s.AgentID, ErrAgentGone)
	default:
	}
	st, err := t.s.ym.OpenStream()
	if err != nil {
		return nil, t.wrap(err)
	}
	if _, err := st.Write([]byte{wire.StreamKindDockerHTTP}); err != nil {
		_ = st.Close()
		return nil, t.wrap(err)
	}
	return st, nil
}

// wrap marks a dead-session error as ErrAgentGone and keeps any other error as
// it is.
func (t StreamTransport) wrap(err error) error {
	if errors.Is(err, yamux.ErrSessionShutdown) || errors.Is(err, yamux.ErrConnectionReset) || t.s.ym.IsClosed() {
		return fmt.Errorf("open a stream to agent %s: %w", t.s.AgentID, errors.Join(ErrAgentGone, err))
	}
	return fmt.Errorf("open a stream to agent %s: %w", t.s.AgentID, err)
}

// Describe names the agent for logs and errors.
func (t StreamTransport) Describe() string {
	if t.s == nil {
		return "tunnel://"
	}
	return "tunnel://" + t.s.AgentID
}

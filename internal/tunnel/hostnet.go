// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ianglennon/tierplan/proto/wire"
)

// ErrHostNetUnsupported means the agent did not report that it can read its host's
// routes (wire.Facts.HostNet is not "ok"). No stream is opened to such an agent: an
// agent older than Phase 10 would log an unknown stream kind for it.
var ErrHostNetUnsupported = errors.New("the agent cannot report its host's routes")

// hostNetTimeout is how long a host-net exchange may take when the caller's context
// sets no deadline of its own.
const hostNetTimeout = 5 * time.Second

// HostNet asks the agent for its host's IPv4 routes: one stream of kind
// wire.StreamKindHostNet, answered with one document. It asks only when the agent
// reported hostNet "ok" in its handshake. The answer is read through a limit and
// is untrusted: the caller validates every field.
func (s *Session) HostNet(ctx context.Context) (wire.HostNetDoc, error) {
	var doc wire.HostNetDoc
	if s == nil {
		return doc, fmt.Errorf("%w: no session", ErrAgentGone)
	}
	if s.Facts.HostNet != wire.HostNetOK {
		return doc, fmt.Errorf("agent %s: %w", s.AgentID, ErrHostNetUnsupported)
	}
	if err := ctx.Err(); err != nil {
		return doc, err
	}
	t := StreamTransport{s: s}
	if s.isGone() {
		return doc, fmt.Errorf("open a stream to agent %s: %w", s.AgentID, ErrAgentGone)
	}
	if !s.acquire() {
		return doc, fmt.Errorf("agent %s: %w (%d requests are in flight)", s.AgentID, ErrBusy, MaxSessionStreams)
	}
	defer s.releaseStream()
	st, err := s.ym.OpenStream()
	if err != nil {
		return doc, t.wrap(err)
	}
	defer func() { _ = st.Close() }()

	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(hostNetTimeout)
	}
	_ = st.SetDeadline(deadline)
	// A cancelled context ends the read at once instead of at the deadline.
	stop := context.AfterFunc(ctx, func() { _ = st.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	if _, err := st.Write([]byte{wire.StreamKindHostNet}); err != nil {
		return doc, t.wrap(err)
	}
	body, err := io.ReadAll(io.LimitReader(st, wire.MaxHostNetBytes+1))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return doc, cerr
		}
		return doc, s.mark(fmt.Errorf("read the routes of agent %s: %w", s.AgentID, err))
	}
	if len(body) > wire.MaxHostNetBytes {
		return doc, fmt.Errorf("agent %s: %w (more than %d bytes of routes)", s.AgentID, ErrTooLarge, wire.MaxHostNetBytes)
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return wire.HostNetDoc{}, fmt.Errorf("agent %s sent routes that are not a document: %w", s.AgentID, err)
	}
	return doc, nil
}

// HostNet asks the live session of agentID for its host's routes. ErrAgentGone when
// the agent has none.
func (h *Hub) HostNet(ctx context.Context, agentID string) (wire.HostNetDoc, error) {
	s := h.Session(agentID)
	if s == nil {
		return wire.HostNetDoc{}, fmt.Errorf("agent %s: %w", agentID, ErrAgentGone)
	}
	return s.HostNet(ctx)
}

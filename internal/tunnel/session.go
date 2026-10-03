// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package tunnel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/ianglennon/bayplan/proto/wire"
)

const (
	// DefaultOfflineAfter is how long a session may be silent before it is closed
	// (D-41).
	DefaultOfflineAfter = 30 * time.Second

	// kindTimeout bounds the wait for the kind byte of an agent-opened stream.
	kindTimeout = 10 * time.Second
	// maxAgentStreams caps the agent-opened streams being looked at at once. Only
	// one is ever needed, the control stream; the rest is room for a late one.
	maxAgentStreams = 8
	// touchTimeout bounds one last-seen write.
	touchTimeout = 5 * time.Second
)

// LastSeenRecorder is told about every sign of life from an agent, so that the
// database can keep a "last seen" without a write per heartbeat: the hub calls
// TouchLastSeen on every heartbeat and the recorder decides how often to write
// (agents.Service.TouchLastSeen writes at most once a minute per agent).
type LastSeenRecorder interface {
	TouchLastSeen(ctx context.Context, agentID string)
}

// SessionStatus is what the agent last reported, held in memory only: heartbeats
// every few seconds must not become database writes (RESEARCH Pitfall 8).
type SessionStatus struct {
	// LastHeartbeat is when the server last received a heartbeat, on its own
	// clock. It is zero until the first one.
	LastHeartbeat time.Time
	// EngineReachable is what the last heartbeat said about the Docker engine.
	EngineReachable bool
	// ClockMs is the agent's clock in the last heartbeat, in Unix milliseconds.
	ClockMs int64
	// ClockOffset is how far the agent's clock is ahead of the server's (negative
	// when behind), measured in the handshake, and ClockWarning is set when it is
	// beyond ClockWarnOffset. They hold for the whole session (D-45).
	ClockOffset  time.Duration
	ClockWarning bool
}

// Status is the last heartbeat. It reads memory and never the database.
func (s *Session) Status() SessionStatus {
	s.hbMu.Lock()
	defer s.hbMu.Unlock()
	return s.status
}

// lastSignOfLife is when the session last proved it was alive: its last
// heartbeat, or when it connected if it has sent none yet, so a new session has
// the whole window to send its first.
func (s *Session) lastSignOfLife() time.Time {
	s.hbMu.Lock()
	defer s.hbMu.Unlock()
	if s.status.LastHeartbeat.After(s.ConnectedAt) {
		return s.status.LastHeartbeat
	}
	return s.ConnectedAt
}

// WithTiming sets how often agents are told to heartbeat (the welcome carries
// it) and how long a session may be silent before it is closed with the reason
// timeout. A zero or negative value keeps the default. Call it before the hub
// serves.
func (h *Hub) WithTiming(heartbeat, offlineAfter time.Duration) *Hub {
	if heartbeat > 0 {
		h.heartbeat = heartbeat
	}
	if offlineAfter > 0 {
		h.offlineAfter = offlineAfter
	}
	return h
}

// WithWatchInterval sets how often the hub looks for silent sessions. The
// default is a sixth of the offline window, so a silent session is closed within
// a sixth of the window after it should be. Tests shorten it. Call it before the
// hub serves.
func (h *Hub) WithWatchInterval(d time.Duration) *Hub {
	if d > 0 {
		h.watchEvery = d
	}
	return h
}

// WithLastSeen sets where heartbeats and connects are recorded. Nil records
// nothing. Call it before the hub serves.
func (h *Hub) WithLastSeen(r LastSeenRecorder) *Hub {
	h.lastSeen = r
	return h
}

func (h *Hub) watchInterval() time.Duration {
	if h.watchEvery > 0 {
		return h.watchEvery
	}
	return max(h.offlineAfter/6, 10*time.Millisecond)
}

// runSession starts what a registered session needs: the loop that accepts the
// agent's streams (the control stream among them) and the watchdog that closes a
// session that has gone quiet. Both end with the session or ctx.
func (h *Hub) runSession(ctx context.Context, s *Session) {
	go h.acceptAgentStreams(ctx, s)
	go h.watch(ctx, s)
}

// watch closes a session whose last sign of life is older than the offline
// window. The close is immediate (no close handshake): the peer has stopped
// answering, and waiting for it would be the hang this exists to prevent.
func (h *Hub) watch(ctx context.Context, s *Session) {
	t := time.NewTicker(h.watchInterval())
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if silent := h.now().Sub(s.lastSignOfLife()); silent > h.offlineAfter {
			h.log.Warn("agent session closed: no heartbeat within the offline window",
				"agent_id", s.AgentID, "environment_id", s.EnvID, "silent_for", silent.Round(time.Second).String())
			s.endWith(DisconnectTimeout)
			s.closeNow()
			return
		}
	}
}

// acceptAgentStreams takes the streams the agent opens. The first whose kind byte
// is wire.StreamKindControl is the control stream; any other is closed.
func (h *Hub) acceptAgentStreams(ctx context.Context, s *Session) {
	var inFlight atomic.Int32
	for {
		st, err := s.ym.AcceptStreamWithContext(ctx)
		if err != nil {
			return
		}
		if inFlight.Add(1) > maxAgentStreams {
			inFlight.Add(-1)
			_ = st.Close()
			continue
		}
		go func() {
			defer inFlight.Add(-1)
			h.agentStream(s, st)
		}()
	}
}

// agentStream reads the kind byte of one agent-opened stream and serves it.
func (h *Hub) agentStream(s *Session, st *yamux.Stream) {
	defer func() { _ = st.Close() }()
	_ = st.SetReadDeadline(time.Now().Add(kindTimeout))
	var kind [1]byte
	if _, err := io.ReadFull(st, kind[:]); err != nil {
		return
	}
	_ = st.SetReadDeadline(time.Time{})
	if kind[0] != wire.StreamKindControl || !s.control.CompareAndSwap(false, true) {
		h.log.Warn("closed an agent-opened stream that is not the control stream", "agent_id", s.AgentID, "kind", kind[0])
		return
	}
	h.readControl(s, st)
}

// readControl reads the agent's newline-delimited JSON messages. The input is
// authenticated but not trusted: a line is at most wire.MaxControlLine bytes, a
// line that is not JSON or has a type this server does not know is ignored, and
// nothing here touches the database except the rate-limited last-seen.
func (h *Hub) readControl(s *Session, st io.Reader) {
	sc := bufio.NewScanner(st)
	sc.Buffer(make([]byte, 0, 4096), wire.MaxControlLine)
	for sc.Scan() {
		var hb wire.Heartbeat
		if err := json.Unmarshal(sc.Bytes(), &hb); err != nil || hb.T != wire.TypeHeartbeat {
			continue
		}
		h.beat(s, hb)
	}
	if errors.Is(sc.Err(), bufio.ErrTooLong) {
		h.log.Warn("agent session closed: a control line was longer than the limit", "agent_id", s.AgentID, "limit", wire.MaxControlLine)
		s.close(websocket.StatusMessageTooBig, "control line too long")
	}
}

// beat records a heartbeat in memory and tells the last-seen recorder.
func (h *Hub) beat(s *Session, hb wire.Heartbeat) {
	s.hbMu.Lock()
	s.status = SessionStatus{LastHeartbeat: h.now(), EngineReachable: hb.EngineReachable, ClockMs: hb.ClockMs}
	s.hbMu.Unlock()
	h.touch(s)
}

// touch tells the last-seen recorder that the agent was heard from.
func (h *Hub) touch(s *Session) {
	if h.lastSeen == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), touchTimeout)
	defer cancel()
	h.lastSeen.TouchLastSeen(ctx, s.AgentID)
}

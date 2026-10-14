// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package tunnel

import (
	"time"
)

// AgentLive is what the hub knows about one agent beyond its database row: whether
// it is connected now, when it was last heard from, and how its last session ended.
// It is kept in memory only and is what an environment's health is rolled up from.
type AgentLive struct {
	AgentID, EnvID string
	// Connected is true while the agent has a live session.
	Connected bool
	// ConnectedAt is when the current or last session began.
	ConnectedAt time.Time
	// LastSeen is the last sign of life: the last heartbeat, or the connect itself
	// when the session sent none.
	LastSeen time.Time
	// Ended is when the last session ended; zero while the first is still open.
	Ended time.Time
	// LastCode is why the last session ended, one of the Disconnect codes.
	LastCode string
	// EngineReachable is what the last heartbeat said about Docker. Until the first
	// heartbeat of a session it is true: nothing has said otherwise.
	EngineReachable bool
	// Version is the agent version it reported when it last connected.
	Version string
	// ClockOffset is how far the agent's clock is ahead of the server's, measured at
	// the last handshake, and ClockWarning says it is beyond ClockWarnOffset.
	ClockOffset  time.Duration
	ClockWarning bool
	// ReadOnly is true while the connected agent reported that its host owner set
	// BAYPLAN_AGENT_READ_ONLY. It is a fact of the live session, so it is false for
	// an agent that is not connected: nothing is known then, and nothing can be
	// written anyway.
	ReadOnly bool
	// PrivilegedServices is true while the connected agent reported that its host
	// owner set BAYPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES, so it forwards service specs
	// that give tasks host access. Like ReadOnly it is a fact of the live session.
	PrivilegedServices bool
}

// agentMemory is what the hub keeps of one agent between and during sessions. It
// is written when a session starts and when it ends; while a session lives its
// numbers are read from the session itself, so a heartbeat costs no extra write
// here. sess is the live session, nil once it has ended.
type agentMemory struct {
	envID string
	sess  *Session
	// order is the accept order of the newest connection that registered, which an
	// older connection finishing its handshake later must not replace.
	order uint64

	version         string
	connectedAt     time.Time
	lastSeen        time.Time
	engineReachable bool
	clockOffset     time.Duration
	clockWarning    bool
	lastCode        string
	ended           time.Time
}

// noteConnected starts or restarts the memory of s's agent. What the previous
// session left (how it ended and when) is kept: it is the last thing known about
// the agent's departure. The caller holds h.mu.
func (h *Hub) noteConnected(s *Session) {
	m := h.memory[s.AgentID]
	if m == nil {
		m = &agentMemory{}
		h.memory[s.AgentID] = m
	}
	m.envID, m.sess, m.connectedAt, m.version = s.EnvID, s, s.ConnectedAt, s.AgentVersion
	m.order = s.order
}

// noteEnded records that s has ended and why. When s is still the agent's current
// session its last numbers are kept; when a newer session has replaced it, only the
// end is recorded, since the new session owns the rest.
func (h *Hub) noteEnded(s *Session) {
	st := s.Status()
	seen := s.lastSignOfLife()
	code, at := s.Reason(), h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.memory[s.AgentID]
	if m == nil {
		return // forgotten with its environment
	}
	m.lastCode, m.ended = code, at
	if m.sess == s {
		m.sess = nil
		m.lastSeen, m.engineReachable = seen, st.LastHeartbeat.IsZero() || st.EngineReachable
		m.clockOffset, m.clockWarning = st.ClockOffset, st.ClockWarning
	}
}

// Forget drops what the hub remembers about the agents of envID. The registry
// calls it when an environment is removed.
func (h *Hub) Forget(envID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, m := range h.memory {
		if m.envID == envID {
			delete(h.memory, id)
		}
	}
}

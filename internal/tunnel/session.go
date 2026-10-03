// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package tunnel

import (
	"context"
	"time"
)

// LastSeenRecorder is told about every sign of life from an agent, so that the
// database can keep a "last seen" without a write per heartbeat.
type LastSeenRecorder interface {
	TouchLastSeen(ctx context.Context, agentID string)
}

// SessionStatus is what the agent last reported, held in memory.
type SessionStatus struct {
	// LastHeartbeat is when the server last received a heartbeat, on its own clock.
	LastHeartbeat time.Time
	// EngineReachable is what the last heartbeat said about the Docker engine.
	EngineReachable bool
	// ClockMs is the agent's clock in the last heartbeat, in Unix milliseconds.
	ClockMs int64
}

// Status is the last heartbeat. It reads memory only.
func (s *Session) Status() SessionStatus { return SessionStatus{} }

// WithTiming sets how often agents are told to heartbeat and how long a session
// may be silent before it is closed.
func (h *Hub) WithTiming(heartbeat, offlineAfter time.Duration) *Hub { return h }

// WithWatchInterval sets how often silent sessions are looked for.
func (h *Hub) WithWatchInterval(d time.Duration) *Hub { return h }

// WithLastSeen sets where heartbeats are recorded.
func (h *Hub) WithLastSeen(r LastSeenRecorder) *Hub { return h }

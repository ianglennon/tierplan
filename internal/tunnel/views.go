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
	// ClockOffset is how far the agent's clock is ahead of the server's, measured at
	// the last handshake, and ClockWarning says it is beyond ClockWarnOffset.
	ClockOffset  time.Duration
	ClockWarning bool
}

// Views returns what the hub knows about the agents of envID that have connected
// since the server started, ordered by agent ID.
func (h *Hub) Views(envID string) []AgentLive {
	_ = envID
	return nil
}

// Forget drops what the hub remembers about the agents of envID. The registry
// calls it when an environment is removed.
func (h *Hub) Forget(envID string) {
	_ = envID
}

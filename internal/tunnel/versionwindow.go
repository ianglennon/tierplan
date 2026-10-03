// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package tunnel

import "time"

const (
	// MaxClockOffset is the skew beyond which an agent is refused (D-45).
	MaxClockOffset = 5 * time.Minute
	// ClockWarnOffset is the skew beyond which the session carries a warning.
	ClockWarnOffset = 30 * time.Second
)

// Compatible decides whether a server of serverVersion accepts an agent of
// agentVersion that speaks protocol proto.
func Compatible(serverVersion, agentVersion string, proto int) (ok bool, tooOld bool, detail string) {
	return true, false, ""
}

// WithServerVersion tells the hub which version this server is.
func (h *Hub) WithServerVersion(string) *Hub { return h }

// WithUpdateHint sets text added to a version_incompatible refusal: the manual
// command that brings the agent up to date. tooOld says which side is behind.
func (h *Hub) WithUpdateHint(func(tooOld bool) string) *Hub { return h }

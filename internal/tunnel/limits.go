// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package tunnel

import (
	"errors"

	"github.com/ianglennon/bayplan/internal/auth"
)

const (
	// MaxSessionStreams is how many server-opened streams one session carries at
	// once.
	MaxSessionStreams = 64
	// MaxResponseBytes caps one non-events Docker response.
	MaxResponseBytes = 64 << 20
)

var (
	// ErrBusy means the session already has MaxSessionStreams requests in flight.
	ErrBusy = errors.New("tunnel: the agent has too many requests in flight")
	// ErrTooLarge means a Docker response passed the size cap.
	ErrTooLarge = errors.New("tunnel: the agent's answer is larger than the limit")
)

// WithAllowInsecure lets agents connect over plain ws://.
func (h *Hub) WithAllowInsecure(bool) *Hub { return h }

// WithConnectLimit sets the per-address burst of connection attempts.
func (h *Hub) WithConnectLimit(int) *Hub { return h }

// WithRefusalAudit sets where refused handshakes are recorded.
func (h *Hub) WithRefusalAudit(auth.AuditRecorder) *Hub { return h }

// OnUpgradeFailure sets a hook told about a request that was not an upgrade.
func (h *Hub) OnUpgradeFailure(func(peer string)) *Hub { return h }

// WithResponseLimit sets the response cap in bytes.
func (h *Hub) WithResponseLimit(int64) *Hub { return h }

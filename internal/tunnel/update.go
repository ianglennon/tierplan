// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package tunnel

import (
	"fmt"

	"github.com/ianglennon/coaming/proto/wire"
)

// maxUpdateError caps the error code an agent reports in an update_result. The
// agent is authenticated but not trusted, and the text ends up in the audit log
// and the node list; every code the agents define is shorter.
const maxUpdateError = 64

// UpdateObserver is told what the control streams of agents carry that the
// update orchestration needs (D-24). Both calls come from the goroutine that reads
// one agent's control stream, so they must return at once and must not call back
// into anything that waits for that stream.
type UpdateObserver interface {
	// ControlOpened runs when an agent's control stream is open: the first moment
	// the server can write to it. An agent that has just reconnected after an
	// update is judged by its version here.
	ControlOpened(s *Session)
	// UpdateResult runs for each update_result an agent writes. The id is cut to
	// wire.MaxMessageIDLen and the error to 64 bytes.
	UpdateResult(s *Session, res wire.UpdateResult)
}

// WithUpdateObserver sets who hears of opened control streams and update
// results. Without one both are ignored. Call it before the hub serves.
func (h *Hub) WithUpdateObserver(o UpdateObserver) *Hub {
	h.updates = o
	return h
}

// SendUpdate tells one agent to replace itself with msg.Image, a full reference
// of the agent's own repository (D-25). The type is always set here, so a caller
// cannot send another kind of message through it. It returns at once: the agent's
// answer arrives later through the UpdateObserver. ErrAgentGone means the agent has
// no live session, or has not opened its control stream yet, or the write failed.
func (h *Hub) SendUpdate(agentID string, msg wire.Update) error {
	s := h.Session(agentID)
	if s == nil {
		return fmt.Errorf("%w: agent %s has no live session", ErrAgentGone, agentID)
	}
	msg.T = wire.TypeUpdate
	if err := s.sendControl(msg); err != nil {
		return fmt.Errorf("%w: %w", ErrAgentGone, err)
	}
	return nil
}

// SendUninstall tells one agent to remove itself from its host (D-30). The type is
// always uninstall, as SendUpdate's is always update. It returns at once: the agent's
// answer arrives later through the UpdateObserver, as an update_result. ErrAgentGone
// means the agent has no live session, or no control stream yet, or the write failed.
func (h *Hub) SendUninstall(agentID string, msg wire.Uninstall) error {
	s := h.Session(agentID)
	if s == nil {
		return fmt.Errorf("%w: agent %s has no live session", ErrAgentGone, agentID)
	}
	msg.T = wire.TypeUninstall
	if err := s.sendControl(msg); err != nil {
		return fmt.Errorf("%w: %w", ErrAgentGone, err)
	}
	return nil
}

// updateResult hands an agent's answer to the observer, after cutting what the
// agent controls to a safe size.
func (h *Hub) updateResult(s *Session, res wire.UpdateResult) {
	if h.updates == nil {
		return
	}
	res.ID = wire.CapID(res.ID)
	res.Error = clip(res.Error, maxUpdateError)
	h.updates.UpdateResult(s, res)
}

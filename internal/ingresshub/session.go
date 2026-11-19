// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ianglennon/tierplan/internal/ingressrender"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// bundleWriteTimeout bounds the write of one bundle message, the one large message on the channel.
const bundleWriteTimeout = 30 * time.Second

// Session is one live ingress node connection. The identifying fields are set before the session is
// registered and never change.
type Session struct {
	NodeID, EnvID, Name string
	// IP is the address the node connected from and Version the supervisor version it announced.
	IP, Version string
	// DialHost is the host the node dialed, lower-case and without the port or IPv6 brackets. The handshake
	// proved it by refusing any audience other than the server's, and a node cannot set it.
	DialHost    string
	ConnectedAt time.Time

	// order is the place of this connection among those the hub accepted: a later accept has a higher number.
	order uint64

	ws   *websocket.Conn
	done chan struct{}
	once sync.Once

	reasonMu sync.Mutex
	reason   string

	// heardAt is when the node last sent a message, under heardMu.
	heardMu sync.Mutex
	heardAt time.Time
}

// kickGrace is how long a kicked session waits for the node to answer the close frame before the connection
// is dropped.
const kickGrace = time.Second

// heard records that the node sent a message at t. Every message is a sign of life.
func (s *Session) heard(t time.Time) {
	s.heardMu.Lock()
	if t.After(s.heardAt) {
		s.heardAt = t
	}
	s.heardMu.Unlock()
}

// lastSignOfLife is when the node last sent a message, or when it connected if it has sent none yet, so a new
// session has the whole window to say its first.
func (s *Session) lastSignOfLife() time.Time {
	s.heardMu.Lock()
	defer s.heardMu.Unlock()
	if s.heardAt.After(s.ConnectedAt) {
		return s.heardAt
	}
	return s.ConnectedAt
}

// Done is closed when the session has ended.
func (s *Session) Done() <-chan struct{} { return s.done }

// endWith records why the session ended. The first reason wins, so a session that was replaced and then saw
// its socket close is still reported as replaced.
func (s *Session) endWith(code string) {
	s.reasonMu.Lock()
	if s.reason == "" {
		s.reason = code
	}
	s.reasonMu.Unlock()
}

// Reason is why the session ended: one of the ingresswire close reasons, or "" while nothing more specific
// than the connection closing was recorded.
func (s *Session) Reason() string {
	s.reasonMu.Lock()
	defer s.reasonMu.Unlock()
	return s.reason
}

// close ends the session with a close frame that carries reason, so the node learns why and reconnects with
// backoff.
func (s *Session) close(code websocket.StatusCode, reason string) {
	s.once.Do(func() { _ = s.ws.Close(code, reason) })
}

// kick ends the session with policy-violation status and reason as the close reason, then drops it if the
// node has not answered within kickGrace. A session that is already closing is left alone.
func (s *Session) kick(reason string) { s.kickWith(websocket.StatusPolicyViolation, reason) }

// kickWith is kick with the status of the close frame given.
func (s *Session) kickWith(code websocket.StatusCode, reason string) {
	s.once.Do(func() {
		done := make(chan struct{})
		go func() {
			_ = s.ws.Close(code, reason)
			close(done)
		}()
		t := time.NewTimer(kickGrace)
		defer t.Stop()
		select {
		case <-done:
		case <-t.C:
			_ = s.ws.CloseNow()
		}
	})
}

// run is the session's read loop: it reads node messages until the connection ends or the node breaks the
// protocol, and returns then. The first message must be a hello (anything else, or a binary frame, is a
// protocol error); on it the node is sent its environment's bundle. The remaining messages (applied,
// rejected, health) and the push of a changed bundle are plan 22-16's, so any other message closes the session
// protocol_error for now. Every message read counts as a sign of life for the watchdog (Hub.watch).
func (h *Hub) run(ctx context.Context, s *Session) {
	defer close(s.done)
	hello := false
	for {
		typ, data, err := s.ws.Read(ctx)
		if err != nil {
			s.endWith(closeReason(err))
			return
		}
		s.heard(h.now())
		if typ != websocket.MessageText {
			h.protocolError(s)
			return
		}
		msg, err := ingresswire.ParseNodeMessage(data)
		if err != nil {
			h.protocolError(s)
			return
		}
		m, ok := msg.(ingresswire.Hello)
		if !ok || hello {
			h.protocolError(s)
			return
		}
		hello = true
		if err := h.onHello(ctx, s, m); err != nil {
			h.log.Warn("could not answer an ingress node's hello", "node_id", s.NodeID, "environment_id", s.EnvID, "error", err)
			s.endWith(ingresswire.CloseProtocolError)
			s.close(websocket.StatusInternalError, "internal_error")
			return
		}
	}
}

// closeReason is the reason a read error ends a session with: the close reason the peer or the hub gave, else
// "" (the connection dropped).
func closeReason(err error) string {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		return ce.Reason
	}
	return ""
}

// protocolError closes the session with policy-violation status and the reason protocol_error.
func (h *Hub) protocolError(s *Session) {
	s.endWith(ingresswire.CloseProtocolError)
	s.close(websocket.StatusPolicyViolation, ingresswire.CloseProtocolError)
}

// onHello records what the node said and sends it the bundle of its environment when it is not running it
// already.
func (h *Hub) onHello(ctx context.Context, s *Session, m ingresswire.Hello) error {
	if err := h.svc.RecordHello(ctx, s.NodeID, m.Version, m.Schemas); err != nil {
		return err
	}
	if _, err := ingressrender.PickSchema(m.Schemas); err != nil {
		// A node with no schema in common keeps the bundle it has: it is frozen, not broken, and the node
		// list says so (ListNodes), rather than pushing it a bundle it cannot read.
		return nil
	}
	if h.src == nil {
		return nil
	}
	b, ok := h.src.Current(s.EnvID)
	if !ok || b.ID == m.AppliedID {
		return nil
	}
	return h.sendBundle(ctx, s, b)
}

// sendBundle records the hostnames of the bundle's routes as sent to the node, then writes the bundle as one
// binary message. The record comes first, so a revoke lists every hostname the node could have held whether or
// not the frame arrived (D-24). The envelope is never logged except through its LogValue.
func (h *Hub) sendBundle(ctx context.Context, s *Session, b ingressbundle.Bundle) error {
	names := make([]string, 0, len(b.Manifest.Routes))
	for _, r := range b.Manifest.Routes {
		names = append(names, r.Hostname)
	}
	if err := h.svc.RecordSent(ctx, s.NodeID, names); err != nil {
		return err
	}
	instanceID, err := h.readInstanceID(ctx)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(ingresswire.BundleEnvelope{T: ingresswire.TypeBundle, InstanceID: instanceID, Bundle: b})
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, bundleWriteTimeout)
	defer cancel()
	return s.ws.Write(wctx, websocket.MessageBinary, payload)
}

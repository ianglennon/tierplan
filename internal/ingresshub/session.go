// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// bundleWriteTimeout bounds the write of one bundle message, the one large message on the channel.
const bundleWriteTimeout = time.Minute

// maxSentIDs is how many of the bundle IDs sent on one session are remembered, so an applied or rejected
// message can be matched to a bundle this session was sent. The newest is the current bundle and the one
// before it is the bundle a slow node may still be answering, so four is more than either needs.
const maxSentIDs = 4

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

	// poke asks the session's writer to look at the environment's current bundle again. It holds at most one
	// pending request, so a burst of changes is one look.
	poke chan struct{}

	// push is what the writer needs to decide, and what the read loop checks a node's report against, under
	// pushMu.
	pushMu sync.Mutex
	push   pushState
}

// pushState is the part of a session's state that the read loop and the writer share. It lives and dies with
// the session: a node that reconnects starts again, so a bundle it rejected is offered again.
type pushState struct {
	// hello is true once the node has said hello. Nothing is sent before.
	hello bool
	// schemas are the bundle schemas the node said it can apply, at most ingresswire.MaxSchemas.
	schemas []int
	// applied is the bundle ID the node says it runs: its hello's, then the last one it applied.
	applied string
	// lastSent is the ID of the last bundle written, and rejected the last one the node refused. rejected is
	// cleared when a different bundle is sent.
	lastSent, rejected string
	// sent holds the last maxSentIDs bundle IDs written, oldest first.
	sent []string
}

// wake asks the writer to look at the current bundle. It never blocks.
func (s *Session) wake() {
	select {
	case s.poke <- struct{}{}:
	default: // a request is already pending
	}
}

// pushSnapshot is a copy of the push state and whether the node has said hello.
func (s *Session) pushSnapshot() pushState {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	return s.push
}

// setHello remembers what the node's hello said.
func (s *Session) setHello(schemas []int, appliedID string) {
	s.pushMu.Lock()
	s.push.hello, s.push.schemas, s.push.applied = true, slices.Clone(schemas), appliedID
	s.pushMu.Unlock()
}

// wasSent reports whether this session sent the bundle id, among the last few it sent.
func (s *Session) wasSent(id string) bool {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	return slices.Contains(s.push.sent, id)
}

// markSent records that the bundle id is about to be written. It is recorded before the frame leaves, because
// a fast node can answer before the write call returns, and its answer is only accepted for a bundle sent.
func (s *Session) markSent(id string) {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	s.push.lastSent = id
	if id != s.push.rejected {
		s.push.rejected = ""
	}
	s.push.sent = append(s.push.sent, id)
	if len(s.push.sent) > maxSentIDs {
		s.push.sent = s.push.sent[len(s.push.sent)-maxSentIDs:]
	}
}

func (s *Session) setApplied(id string) {
	s.pushMu.Lock()
	s.push.applied = id
	s.pushMu.Unlock()
}

func (s *Session) setRejected(id string) {
	s.pushMu.Lock()
	s.push.rejected = id
	s.pushMu.Unlock()
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
// protocol, and returns then. After the welcome a node may say only hello (once, and first), applied,
// rejected and health, as text frames (D-13). Anything else, a binary frame, or a report about a bundle this
// session did not send ends the session with policy-violation status and the reason protocol_error, and writes
// nothing to the node's row. Every accepted message counts as a sign of life (the watchdog, Hub.watch, and
// last_seen_at) and wakes the writer, which decides from the session's state whether a bundle is due.
//
// The session's writer runs beside this loop and ends before the session is reported done.
func (h *Hub) run(ctx context.Context, s *Session) {
	defer close(s.done)
	wctx, stop := context.WithCancel(ctx)
	var writer sync.WaitGroup
	defer writer.Wait()
	defer stop()
	writer.Go(func() { h.writeLoop(wctx, s) })

	hello := false
	for {
		typ, data, err := s.ws.Read(ctx)
		if err != nil {
			s.endWith(closeReason(err))
			return
		}
		s.heard(h.now())
		if typ != websocket.MessageText {
			h.protocolError(s, "binary frame")
			return
		}
		msg, err := ingresswire.ParseNodeMessage(data)
		if err != nil {
			// The type is not known to be one of the four, so it is not named: it is the node's text.
			h.protocolError(s, "unknown or malformed message")
			return
		}
		if err := h.handle(ctx, s, msg, &hello); err != nil {
			var pe protocolViolation
			if errors.As(err, &pe) {
				h.protocolError(s, pe.message)
				return
			}
			h.log.Warn("could not record a report of an ingress node", "node_id", s.NodeID, "environment_id", s.EnvID, "error", err)
			s.endWith(ingresswire.CloseProtocolError)
			s.close(websocket.StatusInternalError, "internal_error")
			return
		}
		h.svc.TouchLastSeen(ctx, s.NodeID)
		s.wake()
	}
}

// protocolViolation is a node message the state of the session does not allow. message names its type.
type protocolViolation struct{ message string }

func (e protocolViolation) Error() string { return "ingresshub: protocol violation: " + e.message }

// handle acts on one parsed node message. hello says whether the node has said hello already, and is set by
// the first one. A message the session's state does not allow is a protocolViolation; any other error is a
// failure to record a report.
func (h *Hub) handle(ctx context.Context, s *Session, msg any, hello *bool) error {
	switch m := msg.(type) {
	case ingresswire.Hello:
		if *hello {
			return protocolViolation{"second hello"}
		}
		*hello = true
		return h.onHello(ctx, s, m)
	case ingresswire.Applied:
		if !*hello {
			return protocolViolation{"applied before hello"}
		}
		// Only a bundle this session sent can be reported: otherwise a node writes arbitrary IDs into its own
		// row.
		if !s.wasSent(m.ID) {
			return protocolViolation{"applied for a bundle that was not sent"}
		}
		if err := h.svc.RecordApplied(ctx, s.NodeID, m.ID); err != nil {
			return err
		}
		s.setApplied(m.ID)
	case ingresswire.Rejected:
		if !*hello {
			return protocolViolation{"rejected before hello"}
		}
		if !s.wasSent(m.ID) {
			return protocolViolation{"rejected for a bundle that was not sent"}
		}
		reason := cleanText(m.Reason, ingresswire.MaxReasonLen)
		if err := h.svc.RecordRejected(ctx, s.NodeID, m.ID, reason); err != nil {
			return err
		}
		s.setRejected(m.ID)
		// The reason is the node's text: it is logged cut and cleaned, and the bundle's files never are.
		h.log.Warn("an ingress node rejected a bundle",
			"node_id", s.NodeID, "environment_id", s.EnvID, "bundle", idPrefix(m.ID), "reason", reason)
	case ingresswire.Health:
		if !*hello {
			return protocolViolation{"health before hello"}
		}
		return h.svc.RecordHealth(ctx, s.NodeID, m.TraefikOK)
	}
	return nil
}

// idPrefix is the first 12 characters of a bundle ID, as Bundle.String shows it.
func idPrefix(id string) string {
	id = cleanText(id, maxBundleIDLen)
	if len(id) > 12 {
		return id[:12]
	}
	return id
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

// protocolError closes the session with policy-violation status and the reason protocol_error. what names the
// kind of message that broke the rules and is logged with the node; the payload never is.
func (h *Hub) protocolError(s *Session, what string) {
	h.log.Warn("an ingress node broke the protocol and its session is closed",
		"node_id", s.NodeID, "environment_id", s.EnvID, "message", what)
	s.endWith(ingresswire.CloseProtocolError)
	s.close(websocket.StatusPolicyViolation, ingresswire.CloseProtocolError)
}

// onHello records what the node said and remembers it for the writer, which sends the node the bundle of its
// environment when it is not running it already. A node with no schema in common keeps the bundle it has: it is
// frozen, not broken, and the node list says so (ListNodes), because the writer sends it nothing.
func (h *Hub) onHello(ctx context.Context, s *Session, m ingresswire.Hello) error {
	if err := h.svc.RecordHello(ctx, s.NodeID, m.Version, m.Schemas); err != nil {
		return err
	}
	schemas := m.Schemas
	if len(schemas) > ingresswire.MaxSchemas {
		schemas = schemas[:ingresswire.MaxSchemas]
	}
	s.setHello(schemas, m.AppliedID)
	return nil
}

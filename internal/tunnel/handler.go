// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
	"golang.org/x/time/rate"

	"github.com/ianglennon/tierplan/proto/authn"
	"github.com/ianglennon/tierplan/proto/wire"
)

// nonceLen is the length of both handshake nonces.
const nonceLen = 32

// serve is GET /agent/connect. Everything before the welcome message is
// unauthenticated input, so it runs on the raw WebSocket with a 16 KiB read
// limit and a 10 s deadline; only after the welcome is the connection wrapped
// as a net.Conn, which removes the limit (Pitfall 4).
func (h *Hub) serve(w http.ResponseWriter, r *http.Request) {
	if h.isClosed() {
		http.Error(w, "the server is stopping", http.StatusServiceUnavailable)
		return
	}
	ip := clientAddr(r)
	aud, ref := h.preflight(r, ip)
	if ref != nil {
		if ref.audit {
			h.recordRefusal(r.Context(), ip, ref.code, "", "", ref.code)
		}
		http.Error(w, ref.msg, ref.status)
		return
	}
	release, limited := h.admit(ip)
	if release != nil {
		defer release()
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{wire.Subprotocol}})
	if err != nil {
		return // Accept has answered the client
	}
	defer func() { _ = c.CloseNow() }()
	order := h.accepted.Add(1)
	if limited != nil {
		// The refusal is a message, not an HTTP status, so that an agent reads it
		// the same way as every other refusal and waits as long as it says.
		h.recordRefusal(r.Context(), ip, limited.Code, "", "", limited.Code)
		rctx, rcancel := context.WithTimeout(r.Context(), wire.HandshakeTimeout)
		h.refuseAfter(rctx, c, limited.Code, limited.Detail, time.Duration(limited.RetryAfterMs)*time.Millisecond)
		rcancel()
		return
	}
	if c.Subprotocol() != wire.Subprotocol {
		_ = c.Close(websocket.StatusPolicyViolation, "the subprotocol "+wire.Subprotocol+" is required")
		return
	}
	c.SetReadLimit(wire.MaxHandshakeBytes)

	// The session outlives the handshake deadline, and it must not end when the
	// request context does, so it has a context of its own.
	sctx, scancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer scancel()

	hctx, hcancel := context.WithTimeout(sctx, wire.HandshakeTimeout)
	acc, ok := h.handshake(hctx, c, r, aud)
	hcancel()
	release() // the slot is for the handshake, not for the session that follows
	if !ok {
		return
	}

	conn := websocket.NetConn(sctx, c, websocket.MessageBinary)
	ym, err := yamux.Client(conn, h.muxConfig())
	if err != nil {
		h.log.Error("start the agent multiplexer", "agent_id", acc.AgentID, "error", err)
		return
	}
	s := &Session{
		AgentID: acc.AgentID, EnvID: acc.EnvID, AgentVersion: acc.AgentVersion, Facts: acc.Facts,
		IP: acc.IP, ConnectedAt: h.now(), DialHost: dialHost(aud), order: order, ym: ym, ws: c, done: make(chan struct{}),
		streams: make(chan struct{}, MaxSessionStreams), maxResponse: h.maxResponse,
		interactive: make(chan struct{}, MaxInteractiveStreams),
		denyLimit:   rate.NewLimiter(rate.Every(denyRefill), denyBurst),
		status:      SessionStatus{ClockOffset: acc.ClockOffset, ClockWarning: abs(acc.ClockOffset) > ClockWarnOffset},
		welcomeURLs: acc.URLs,
	}
	if s.status.ClockWarning {
		h.log.Warn("an agent's clock is off by more than the warning threshold",
			"agent_id", s.AgentID, "environment_id", s.EnvID, "offset", acc.ClockOffset.Round(time.Second).String())
	}
	go func() {
		<-ym.CloseChan()
		close(s.done)
	}()

	switch err := h.register(s); {
	case errors.Is(err, errNoLongerActive):
		// A revoke landed between the hello and the registration: the agent was
		// welcomed, and is now told why it is being dropped.
		h.log.Warn("an agent was revoked while it connected", "agent_id", s.AgentID, "environment_id", s.EnvID)
		s.endWith(DisconnectRevoked)
		s.kick(DisconnectRevoked)
		return
	case errors.Is(err, errSuperseded):
		// Another handshake of this agent, begun after this one, registered first.
		// The agent is on that connection; this one is turned away unreported.
		h.log.Info("an older connection of an agent finished its handshake after a newer one and was dropped", "agent_id", s.AgentID, "environment_id", s.EnvID)
		s.endWith(DisconnectReplaced)
		s.close(websocket.StatusGoingAway, "a newer connection of this agent is in use")
		return
	case err != nil:
		s.close(websocket.StatusGoingAway, "the server is stopping")
		return
	}
	h.log.Info("agent connected", "agent_id", s.AgentID, "environment_id", s.EnvID, "version", s.AgentVersion, "ip", s.IP)
	h.runSession(sctx, s)
	h.touch(s)

	<-s.done
	s.closeNow()
	h.finish(s)
	h.log.Info("agent disconnected", "agent_id", s.AgentID, "environment_id", s.EnvID, "reason", s.Reason())
}

// audience is the host the agent must have dialed, from the agent base the hub
// was given. Plain transport is never acceptable, so there is no flag for it:
// secureTransport decides that from the connection alone.
func (h *Hub) audience() (aud string, err error) {
	if h.publicURL == nil {
		return "", errors.New("the hub was given no public URL")
	}
	raw := h.publicURL()
	if raw == "" {
		return "", errors.New("the public URL is not set")
	}
	return authn.Audience(raw)
}

// dialHost is not implemented yet.
func dialHost(aud string) string { return aud }

// secureTransport reports whether the connection reached TierPlan over TLS: only
// the connection itself counts. X-Forwarded-Proto from a trusted proxy does not
// admit a plain agent connection, because the hop from the proxy to TierPlan would
// still be in clear; a proxy must talk https to TierPlan (D-04, D-09).
func secureTransport(r *http.Request) bool {
	return r.TLS != nil
}

func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, v := range r.Header.Values("Connection") {
		for tok := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

func writeJSON(ctx context.Context, c *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}

// clip cuts an agent-supplied string to n bytes on a character boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

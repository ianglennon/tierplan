// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
	"golang.org/x/time/rate"

	"github.com/ianglennon/coaming/internal/httpx"
	"github.com/ianglennon/coaming/proto/authn"
	"github.com/ianglennon/coaming/proto/wire"
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
		IP: acc.IP, ConnectedAt: h.now(), order: order, ym: ym, ws: c, done: make(chan struct{}),
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
// was given, and whether plain transport is acceptable without the development
// switch: only when that base is plain http for this machine (authn.LoopbackHost).
// An http base for any other host does not make plain transport acceptable, only
// the switch does (G-02-R4-3).
func (h *Hub) audience() (aud string, plainOK bool, err error) {
	if h.publicURL == nil {
		return "", false, errors.New("the hub was given no public URL")
	}
	raw := h.publicURL()
	if raw == "" {
		return "", false, errors.New("the public URL is not set")
	}
	aud, err = authn.Audience(raw)
	if err != nil {
		return "", false, err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false, err
	}
	return aud, strings.EqualFold(u.Scheme, "http") && !PlainAddressRefused(raw, false), nil
}

// PlainAddressRefused reports whether raw is an address Coaming refuses to have
// agents dial: it is plain (scheme http or ws, any case) for a host that
// authn.LoopbackHost does not accept, and allowInsecure (COAMING_AGENT_ALLOW_INSECURE)
// is false. raw is an address agents are told to dial, the agent base or the
// endpoint built from it. An https or wss address, an empty value and one that does
// not parse are not refused here; the caller deals with a missing address. This is
// the one server-side rule for plain agent addresses (G-02-R4-3): the hub refuses
// plain connections for such an address, and Coaming does not hand it out.
func PlainAddressRefused(raw string, allowInsecure bool) bool {
	if allowInsecure {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "ws") {
		return false
	}
	return !authn.LoopbackHost(u.Hostname())
}

// secureTransport reports whether the connection reached Coaming over TLS, either
// directly or through a proxy Coaming trusts to say so: the effective scheme
// believes X-Forwarded-Proto from a trusted peer and from nobody else (D-51).
func secureTransport(r *http.Request) bool {
	return r.TLS != nil || httpx.EffectiveScheme(r.Context()) == "https"
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

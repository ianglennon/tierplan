// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

const (
	// recordTimeout bounds the audit write of one refusal.
	recordTimeout = 5 * time.Second

	// Reasons a request is recorded as refused before the WebSocket is accepted. The first and the last are
	// not wire codes: the caller is not a node, so there is nobody to tell.
	refusedOrigin     = "origin"
	refusedNotUpgrade = "not_upgrade"
)

// serve is GET /ingress/connect. Everything before the welcome is unauthenticated input, so it runs on the raw
// WebSocket with a 16 KiB read limit and a 10 s deadline. After the welcome the limit is that of a node
// message, which is small, because the server reads only node messages; the connection is never wrapped as a
// net.Conn (D-21).
func (h *Hub) serve(w http.ResponseWriter, r *http.Request) {
	if h.isClosed() {
		http.Error(w, "the server is stopping", http.StatusServiceUnavailable)
		return
	}
	ip := clientAddr(r)
	aud, ref := h.preflight(r, ip)
	if ref != nil {
		if ref.audit {
			h.recordRefusal(r.Context(), ip, ref.code, "", ref.code)
		}
		http.Error(w, ref.msg, ref.status)
		return
	}
	release, limited := h.gate.admit(ip)
	if release != nil {
		defer release()
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{ingresswire.Subprotocol}})
	if err != nil {
		return // Accept has answered the client
	}
	defer func() { _ = c.CloseNow() }()
	order := h.accepted.Add(1)
	if limited != nil {
		// The refusal is a message, not an HTTP status, so that a node reads it the same way as every other
		// refusal and waits as long as it says.
		h.recordRefusal(r.Context(), ip, limited.Code, "", limited.Code)
		rctx, rcancel := context.WithTimeout(r.Context(), ingresswire.HandshakeTimeout)
		h.refuse(rctx, c, limited.Code, limited.Detail, time.Duration(limited.RetryAfterMs)*time.Millisecond)
		rcancel()
		return
	}
	if c.Subprotocol() != ingresswire.Subprotocol {
		_ = c.Close(websocket.StatusPolicyViolation, "the subprotocol "+ingresswire.Subprotocol+" is required")
		return
	}
	c.SetReadLimit(ingresswire.MaxHandshakeBytes)

	// The session outlives the handshake deadline, and it must not end when the request context does, so it
	// has a context of its own.
	sctx, scancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer scancel()

	hctx, hcancel := context.WithTimeout(sctx, ingresswire.HandshakeTimeout)
	acc, ok := h.handshake(hctx, c, r, aud)
	hcancel()
	release() // the slot is for the handshake, not for the session that follows
	if !ok {
		return
	}
	c.SetReadLimit(ingresswire.MaxNodeMessageBytes)

	s := &Session{
		NodeID: acc.NodeID, EnvID: acc.EnvID, Name: acc.Name, IP: acc.IP, Version: acc.Version,
		DialHost: dialHost(aud), ConnectedAt: h.now(), order: order, ws: c, done: make(chan struct{}),
	}
	if h.beforeRegister != nil {
		h.beforeRegister(s.NodeID)
	}
	switch err := h.register(s); {
	case errors.Is(err, errSuperseded):
		// This connection was accepted before the node's newest one, which has registered: the node has moved
		// on, so this one only says why it is being dropped.
		s.endWith(ingresswire.CloseReplaced)
		s.close(websocket.StatusGoingAway, ingresswire.CloseReplaced)
		return
	case errors.Is(err, errNoLongerActive):
		// A revoke landed between the connect check and the registration: the node was welcomed, and is now
		// told why it is being dropped.
		h.log.Warn("an ingress node was revoked while it connected", "node_id", s.NodeID, "environment_id", s.EnvID)
		s.endWith(ingresswire.CloseRevoked)
		s.close(websocket.StatusPolicyViolation, ingresswire.CloseRevoked)
		return
	case err != nil:
		s.endWith(ingresswire.CloseShutdown)
		s.close(websocket.StatusGoingAway, ingresswire.CloseShutdown)
		return
	}
	h.log.Info("ingress node connected", "node_id", s.NodeID, "environment_id", s.EnvID, "version", s.Version, "ip", s.IP)
	go h.watch(sctx, s)
	h.run(sctx, s)
	h.finish(s)
	h.log.Info("ingress node disconnected", "node_id", s.NodeID, "environment_id", s.EnvID, "reason", s.Reason())
}

// preflightRefusal is a request turned away before the WebSocket is accepted.
type preflightRefusal struct {
	status int
	// code is what the audit entry records, and for a wire code what the body says.
	code string
	msg  string
	// audit is false for a refusal that is the server's own state, not a caller's fault: no public URL.
	audit bool
}

// logBeforeAdmission writes a log line about a request that has not been admitted yet, at most once per client
// address per minute, and drops it otherwise, so any caller cannot fill the log for the price of a GET.
func (h *Hub) logBeforeAdmission(level slog.Level, ip, msg string, args ...any) {
	if !h.warnLimit.Allow(ip) {
		return
	}
	h.log.Log(context.Background(), level, msg, append(args, "ip", ip)...)
}

// preflight runs the checks that need nothing but the request, in this order (D-36, which differs from the
// agent hub's on purpose): an Origin header is 403; a connection that is not TLS is 403 insecure_transport;
// only then is a request that is no upgrade 426; and a hub with no public URL is 503. It returns the audience
// the handshake must be addressed to.
func (h *Hub) preflight(r *http.Request, ip string) (aud string, ref *preflightRefusal) {
	// Browsers always send Origin and nodes never do: a request with one is a web page trying to use this
	// endpoint.
	if r.Header.Get("Origin") != "" {
		return "", &preflightRefusal{status: http.StatusForbidden, code: refusedOrigin, msg: "forbidden", audit: true}
	}
	// The TLS check precedes the upgrade check (D-36), so a plain HTTP request is always 403
	// insecure_transport, with or without upgrade headers, and never reaches the 426 path. Only the
	// connection itself counts: X-Forwarded-Proto from a trusted proxy does not admit a plain connection,
	// because the hop from the proxy to TierPlan would still be in clear.
	if r.TLS == nil {
		h.logBeforeAdmission(slog.LevelWarn, ip, "ingress connection refused: it did not arrive over TLS")
		return "", &preflightRefusal{
			status: http.StatusForbidden, code: ingresswire.RefuseInsecureTransport, audit: true,
			msg: ingresswire.RefuseInsecureTransport + ": ingress nodes must connect over wss://",
		}
	}
	if !isWebSocketUpgrade(r) {
		// A proxy that strips the Upgrade headers lands here.
		h.logBeforeAdmission(slog.LevelWarn, ip, "ingress endpoint reached without a WebSocket upgrade; a reverse proxy may be dropping the Upgrade headers")
		return "", &preflightRefusal{status: http.StatusUpgradeRequired, code: refusedNotUpgrade, msg: "a WebSocket upgrade is required", audit: true}
	}
	aud, err := h.audience()
	if err != nil {
		h.logBeforeAdmission(slog.LevelError, ip, "ingress connection refused: TierPlan has no usable public URL", "error", err)
		return "", &preflightRefusal{status: http.StatusServiceUnavailable, msg: "this server has no public URL, so it cannot accept ingress nodes"}
	}
	return aud, nil
}

// recordRefusal writes one ingress.node_enrol_refused entry for an anonymous caller. The refusals recorder is a
// throttle, so a caller who keeps being refused costs the writer a bounded number of rows and the rest are
// counted. The details hold the code and the mode (enrol or connect, D-35), never anything that could be a
// secret, and the entry never has an environment: an unauthenticated token names one of the caller's choosing.
//
// A refused connect is deliberately written under the same action as a refused enrolment (D-17 names it for
// enrolment; both are the same unauthenticated handshake behind the same throttle), and the mode detail keeps
// them apart on the audit page.
func (h *Hub) recordRefusal(ctx context.Context, ip, code, mode, reason string) {
	if h.refusals == nil {
		return
	}
	d := map[string]string{"code": code}
	if mode != "" {
		d["mode"] = mode
	}
	if reason == "" {
		reason = code
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	err := h.refusals.Record(ctx, audit.Entry{
		Actor: audit.Actor{Type: audit.ActorAnonymous}, Action: audit.ActionIngressNodeEnrolRefused,
		TargetType: targetIngressNode, Result: audit.ResultDenied, Reason: reason, IP: ip, Details: d,
	})
	if err != nil {
		h.log.Warn("could not record a refused ingress connection", "code", code, "error", err)
	}
}

// clientAddr is the address of the caller: the one the client-address middleware recorded, else the TCP peer.
func clientAddr(r *http.Request) string {
	if ip := httpx.ClientIP(r.Context()); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// dialHost is the host of a handshake audience (host[:port], as authn.Audience writes it): lower-case, without
// the port and without the brackets of an IPv6 literal.
func dialHost(aud string) string {
	host := aud
	if h, _, err := net.SplitHostPort(aud); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return strings.ToLower(host)
}

// isWebSocketUpgrade reports whether the request asks for a WebSocket upgrade.
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

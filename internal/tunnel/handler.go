// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/proto/authn"
	"github.com/ianglennon/bayplan/proto/wire"
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
	// Browsers always send Origin and agents never do: a request with one is a
	// web page trying to use this endpoint.
	if r.Header.Get("Origin") != "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !isWebSocketUpgrade(r) {
		// A proxy that strips the Upgrade headers lands here.
		h.log.Warn("agent endpoint reached without a WebSocket upgrade; a reverse proxy may be dropping the Upgrade headers",
			"ip", httpx.ClientIP(r.Context()))
		http.Error(w, "a WebSocket upgrade is required", http.StatusUpgradeRequired)
		return
	}
	aud, plainOK, err := h.audience()
	if err != nil {
		h.log.Error("agent connection refused: Bayplan has no usable public URL", "error", err)
		http.Error(w, "this server has no public URL, so it cannot accept agents", http.StatusServiceUnavailable)
		return
	}
	if !secureTransport(r) {
		if !plainOK {
			h.log.Warn("agent connection refused: it did not arrive over TLS", "ip", httpx.ClientIP(r.Context()))
			http.Error(w, wire.RefuseInsecureTransport+": agents must connect over wss://", http.StatusForbidden)
			return
		}
		h.log.Warn("an agent connected over plain HTTP; the public URL is http, so the join token and Docker answers are not encrypted",
			"ip", httpx.ClientIP(r.Context()))
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{wire.Subprotocol}})
	if err != nil {
		return // Accept has answered the client
	}
	defer func() { _ = c.CloseNow() }()
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
	res, req, ok := h.handshake(hctx, c, r, aud)
	hcancel()
	if !ok {
		return
	}

	conn := websocket.NetConn(sctx, c, websocket.MessageBinary)
	ym, err := yamux.Client(conn, muxConfig())
	if err != nil {
		h.log.Error("start the agent multiplexer", "agent_id", res.AgentID, "error", err)
		return
	}
	s := &Session{
		AgentID: res.AgentID, EnvID: res.EnvID, AgentVersion: req.AgentVersion, Facts: req.Facts,
		IP: req.IP, ConnectedAt: h.now(), ym: ym, ws: c, done: make(chan struct{}),
	}
	go func() {
		<-ym.CloseChan()
		close(s.done)
	}()

	if !h.register(s) {
		s.close(websocket.StatusGoingAway, "the server is stopping")
		return
	}
	h.log.Info("agent connected", "agent_id", s.AgentID, "environment_id", s.EnvID, "version", s.AgentVersion, "ip", s.IP)
	if h.o != nil {
		h.o.Connected(s)
	}

	<-s.done
	s.closeNow()
	h.unregister(s)
	h.log.Info("agent disconnected", "agent_id", s.AgentID, "environment_id", s.EnvID)
	if h.o != nil {
		h.o.Disconnected(s, wire.ReasonAgentOffline)
	}
}

// handshake runs the challenge, auth and welcome exchange. It sends a refuse
// message and closes the connection itself when the agent is turned away, and
// then reports false.
func (h *Hub) handshake(ctx context.Context, c *websocket.Conn, r *http.Request, aud string) (EnrolResult, EnrolRequest, bool) {
	ip := httpx.ClientIP(r.Context())

	serverNonce := make([]byte, nonceLen)
	if _, err := rand.Read(serverNonce); err != nil {
		h.log.Error("generate a handshake nonce", "error", err)
		h.refuse(ctx, c, wire.RefuseInternal, "the server could not start the handshake")
		return EnrolResult{}, EnrolRequest{}, false
	}
	if err := writeJSON(ctx, c, wire.Challenge{
		T: wire.TypeChallenge, Proto: wire.ProtoVersion, Nonce: serverNonce, TS: h.now().UnixMilli(),
	}); err != nil {
		return EnrolResult{}, EnrolRequest{}, false
	}

	typ, data, err := c.Read(ctx)
	if err != nil {
		h.log.Info("agent handshake did not complete", "ip", ip, "error", err)
		return EnrolResult{}, EnrolRequest{}, false
	}
	if typ != websocket.MessageText {
		h.refuse(ctx, c, wire.ReasonAuthRejected, "a handshake message must be a text frame")
		return EnrolResult{}, EnrolRequest{}, false
	}
	var a wire.Auth
	if err := json.Unmarshal(data, &a); err != nil || a.T != wire.TypeAuth {
		h.refuse(ctx, c, wire.ReasonAuthRejected, "the handshake message is malformed")
		return EnrolResult{}, EnrolRequest{}, false
	}

	switch {
	case a.Proto != wire.ProtoVersion:
		h.refuse(ctx, c, wire.ReasonVersionIncompatible,
			fmt.Sprintf("this server speaks protocol %d, the agent sent %d", wire.ProtoVersion, a.Proto))
		return EnrolResult{}, EnrolRequest{}, false
	case a.Mode != wire.ModeEnrol:
		// The hello path (reconnect with a certificate) arrives with the state
		// volume; until then an agent enrols with the join token every start.
		h.refuse(ctx, c, wire.ReasonAuthRejected, "only enrolment with a join token is supported")
		return EnrolResult{}, EnrolRequest{}, false
	case len(a.Nonce) != nonceLen || len(a.Pub) != ed25519.PublicKeySize || len(a.Sig) != ed25519.SignatureSize:
		h.refuse(ctx, c, wire.ReasonAuthRejected, "the handshake message is malformed")
		return EnrolResult{}, EnrolRequest{}, false
	case a.Aud != aud:
		// A relay to a different host signs a different audience and fails here.
		h.log.Warn("agent handshake addressed to a different host", "ip", ip, "audience", a.Aud, "expected", aud)
		h.refuse(ctx, c, wire.ReasonAuthRejected, "the handshake was addressed to a different host than this server's public URL")
		return EnrolResult{}, EnrolRequest{}, false
	}

	// Channel binding. When Bayplan terminates TLS itself both ends compute the
	// same exporter value; behind a TLS-terminating proxy they cannot, so the
	// comparison is made only for a peer that is not a trusted proxy (RESEARCH
	// Pattern 2). A mismatch for such a peer means a proxy that is not listed in
	// BAYPLAN_TRUSTED_PROXIES, or a relay.
	ekm := a.EKM
	if r.TLS != nil && !httpx.PeerTrusted(r.Context()) {
		ekm = authn.EKM(r.TLS)
		if !authn.EKMEqual(a.EKM, ekm) {
			h.log.Warn("agent handshake is not bound to this TLS connection; a TLS-terminating proxy may be missing from BAYPLAN_TRUSTED_PROXIES", "ip", ip)
			h.refuse(ctx, c, wire.ReasonAuthRejected,
				"the handshake is not bound to this TLS connection; if a reverse proxy terminates TLS, add it to BAYPLAN_TRUSTED_PROXIES")
			return EnrolResult{}, EnrolRequest{}, false
		}
	}

	req := EnrolRequest{
		Token: a.Token, Pub: a.Pub, Sig: a.Sig,
		Transcript:   authn.Transcript(a.Mode, a.Proto, serverNonce, a.Nonce, aud, a.Pub, ekm),
		AgentVersion: clip(a.AgentVersion, 64), Facts: a.Facts, IP: ip,
	}
	res, err := h.v.Enrol(ctx, req)
	if err != nil {
		var re *RefuseError
		if errors.As(err, &re) {
			h.log.Info("agent enrolment refused", "ip", ip, "code", re.Code, "reason", re.AuditReason)
			h.refuse(ctx, c, re.Code, re.Detail)
		} else {
			h.log.Error("agent enrolment failed", "ip", ip, "error", err)
			h.refuse(ctx, c, wire.RefuseInternal, "the server failed to enrol this agent; it will retry")
		}
		return EnrolResult{}, EnrolRequest{}, false
	}

	if err := writeJSON(ctx, c, wire.Welcome{
		T: wire.TypeWelcome, AgentID: res.AgentID, EnvID: res.EnvID, Cert: res.Cert,
		HeartbeatMs: wire.DefaultHeartbeatMs, ServerTime: h.now().UnixMilli(),
	}); err != nil {
		h.log.Warn("agent went away before the welcome", "agent_id", res.AgentID, "error", err)
		return EnrolResult{}, EnrolRequest{}, false
	}
	return res, req, true
}

// refuse tells the agent why it is turned away, then closes the connection with
// policy-violation status and the code as the reason.
func (h *Hub) refuse(ctx context.Context, c *websocket.Conn, code, detail string) {
	_ = writeJSON(ctx, c, wire.Refuse{T: wire.TypeRefuse, Code: code, Detail: detail})
	_ = c.Close(websocket.StatusPolicyViolation, code)
}

// audience is the host the agent must have dialed, from the public URL, and
// whether plain transport is acceptable because that URL is itself plain http.
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
	return aud, strings.EqualFold(u.Scheme, "http"), nil
}

// secureTransport reports whether the connection reached Bayplan over TLS, either
// directly or through a proxy Bayplan trusts to say so.
func secureTransport(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return httpx.PeerTrusted(r.Context()) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
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

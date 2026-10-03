// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/proto/authn"
	"github.com/ianglennon/bayplan/proto/wire"
)

// maxCertBytes bounds a certificate in a hello. A real one is under 1 KiB; the
// handshake read limit is the outer bound, this keeps parsing cheap.
const maxCertBytes = 8 << 10

// peerInfo is what the handler knows about the connection that the signed
// handshake must agree with.
type peerInfo struct {
	ip string
	// tls is the connection state when Bayplan terminates TLS itself.
	tls *tls.ConnectionState
	// trusted is true when the peer is a proxy in BAYPLAN_TRUSTED_PROXIES.
	trusted bool
	// aud is the host[:port] the agent must have addressed.
	aud string
	// sentAt is when the challenge was sent and recvAt when the answer arrived,
	// on the server's clock; the agent's clock is measured against their midpoint.
	sentAt, recvAt time.Time
}

// accepted is a handshake the verifier let through.
type accepted struct {
	AgentID, EnvID string
	// Cert is the certificate issued to an agent that enrolled, empty for hello.
	Cert         []byte
	AgentVersion string
	Facts        wire.Facts
	IP           string
	// ClockOffset is how far the agent's clock is ahead of the server's.
	ClockOffset time.Duration
}

// handshake runs the challenge, auth and welcome exchange. It sends a refuse
// message and closes the connection itself when the agent is turned away, and
// then reports false.
func (h *Hub) handshake(ctx context.Context, c *websocket.Conn, r *http.Request, aud string) (accepted, bool) {
	ip := clientAddr(r)

	serverNonce := make([]byte, nonceLen)
	if _, err := rand.Read(serverNonce); err != nil {
		h.log.Error("generate a handshake nonce", "error", err)
		h.refuse(ctx, c, wire.RefuseInternal, "the server could not start the handshake")
		return accepted{}, false
	}
	sentAt := h.now()
	if err := writeJSON(ctx, c, wire.Challenge{
		T: wire.TypeChallenge, Proto: wire.ProtoVersion, Nonce: serverNonce, TS: sentAt.UnixMilli(),
		ServerVersion: h.serverVersion,
	}); err != nil {
		return accepted{}, false
	}

	typ, data, err := c.Read(ctx)
	recvAt := h.now()
	if err != nil {
		h.log.Info("agent handshake did not complete", "ip", ip, "error", err)
		return accepted{}, false
	}
	if typ != websocket.MessageText {
		h.turnAway(ctx, c, ip, "", "", wire.ReasonAuthRejected, "a handshake message must be a text frame", "not_text")
		return accepted{}, false
	}
	var a wire.Auth
	if err := json.Unmarshal(data, &a); err != nil || a.T != wire.TypeAuth {
		h.turnAway(ctx, c, ip, "", "", wire.ReasonAuthRejected, "the handshake message is malformed", "malformed")
		return accepted{}, false
	}

	acc, err := h.verifyAuth(ctx, a, serverNonce, peerInfo{
		ip: ip, tls: r.TLS, trusted: httpx.PeerTrusted(r.Context()), aud: aud, sentAt: sentAt, recvAt: recvAt,
	})
	if err != nil {
		var re *RefuseError
		if errors.As(err, &re) {
			h.log.Info("agent handshake refused", "ip", ip, "mode", a.Mode, "code", re.Code, "reason", re.AuditReason)
			h.turnAway(ctx, c, ip, a.Mode, a.Facts.Hostname, re.Code, re.Detail, re.AuditReason)
		} else {
			h.log.Error("agent handshake failed", "ip", ip, "mode", a.Mode, "error", err)
			h.refuse(ctx, c, wire.RefuseInternal, "the server failed to verify this agent; it will retry")
		}
		return accepted{}, false
	}

	if err := writeJSON(ctx, c, wire.Welcome{
		T: wire.TypeWelcome, AgentID: acc.AgentID, EnvID: acc.EnvID, Cert: acc.Cert,
		HeartbeatMs: int(h.heartbeat / time.Millisecond), ServerTime: h.now().UnixMilli(),
	}); err != nil {
		h.log.Warn("agent went away before the welcome", "agent_id", acc.AgentID, "error", err)
		return accepted{}, false
	}
	return acc, true
}

// verifyAuth checks an auth message and dispatches by mode. The cheap checks
// come first and need no database: protocol version, shape, audience and the
// channel binding. Only then is the verifier asked, with a transcript the
// handler rebuilt itself from the challenge it sent, never one the agent
// supplied. A refusal is a *RefuseError; any other error is a server fault.
func (h *Hub) verifyAuth(ctx context.Context, a wire.Auth, serverNonce []byte, p peerInfo) (accepted, error) {
	malformed := &RefuseError{Code: wire.ReasonAuthRejected, Detail: "the handshake message is malformed", AuditReason: "malformed"}

	// The protocol and the version window come first, for both modes: they need no
	// database and no key, and an agent that cannot work with this server should
	// hear that before anything else about it (D-25).
	agentVersion := clip(a.AgentVersion, 64)
	if ok, tooOld, detail := Compatible(h.serverVersion, agentVersion, a.Proto); !ok {
		reason := "too_new"
		switch {
		case a.Proto != wire.ProtoVersion:
			reason = "protocol"
		case tooOld:
			reason = "too_old"
		}
		if h.updateHint != nil {
			if hint := h.updateHint(tooOld); hint != "" {
				detail += ". " + hint
			}
		}
		return accepted{}, &RefuseError{Code: wire.ReasonVersionIncompatible, Detail: detail, AuditReason: reason}
	}

	switch {
	case a.Mode != wire.ModeEnrol && a.Mode != wire.ModeHello:
		return accepted{}, malformed
	case len(a.Nonce) != nonceLen || len(a.Sig) != ed25519.SignatureSize:
		return accepted{}, malformed
	case a.Mode == wire.ModeEnrol && len(a.Pub) != ed25519.PublicKeySize:
		return accepted{}, malformed
	case a.Mode == wire.ModeHello && (len(a.Cert) == 0 || len(a.Cert) > maxCertBytes):
		return accepted{}, malformed
	case a.Aud != p.aud:
		// A relay to a different host signs a different audience and fails here.
		h.log.Warn("agent handshake addressed to a different host", "ip", p.ip, "audience", a.Aud, "expected", p.aud)
		return accepted{}, &RefuseError{
			Code:        wire.ReasonAuthRejected,
			Detail:      "the handshake was addressed to a different host than this server's public URL",
			AuditReason: "audience",
		}
	}

	// Channel binding (D-05). When Bayplan terminates TLS itself both ends compute
	// the same exporter value, authn.EKM over tls.ConnectionState.ExportKeyingMaterial; behind a TLS-terminating proxy they cannot, so the
	// comparison is made only for a peer that is not a trusted proxy (RESEARCH
	// Pattern 2). A mismatch for such a peer means a proxy that is not listed in
	// BAYPLAN_TRUSTED_PROXIES, or a relay.
	ekm := a.EKM
	if p.tls != nil && !p.trusted {
		ekm = authn.EKM(p.tls)
		if !authn.EKMEqual(a.EKM, ekm) {
			h.log.Warn("agent handshake is not bound to this TLS connection; a TLS-terminating proxy may be missing from BAYPLAN_TRUSTED_PROXIES", "ip", p.ip)
			return accepted{}, &RefuseError{
				Code:        wire.ReasonAuthRejected,
				Detail:      "the handshake is not bound to this TLS connection; if a reverse proxy terminates TLS, add it to BAYPLAN_TRUSTED_PROXIES",
				AuditReason: "channel_binding",
			}
		}
	}

	// The agent's clock against the middle of the time the exchange took, so the
	// round trip does not count as skew (D-45). The Verifier refuses an offset past
	// MaxClockOffset at the right point of its own checks; the hub keeps it for
	// the session, which warns past ClockWarnOffset.
	offset := time.UnixMilli(a.TS).Sub(p.sentAt.Add(p.recvAt.Sub(p.sentAt) / 2))

	if a.Mode == wire.ModeHello {
		res, err := h.v.Hello(ctx, HelloRequest{
			AgentID: a.AgentID, Cert: a.Cert, Sig: a.Sig,
			Transcript:   authn.Transcript(a.Mode, a.Proto, serverNonce, a.Nonce, p.aud, a.Cert, ekm),
			AgentVersion: agentVersion, Facts: a.Facts, IP: p.ip, ClockOffset: offset,
		})
		if err != nil {
			return accepted{}, err
		}
		return accepted{
			AgentID: res.AgentID, EnvID: res.EnvID, AgentVersion: agentVersion, Facts: a.Facts, IP: p.ip, ClockOffset: offset,
		}, nil
	}

	req := EnrolRequest{
		Token: a.Token, Pub: a.Pub, Sig: a.Sig,
		Transcript:   authn.Transcript(a.Mode, a.Proto, serverNonce, a.Nonce, p.aud, a.Pub, ekm),
		AgentVersion: agentVersion, Facts: a.Facts, IP: p.ip, ClockOffset: offset,
	}
	res, err := h.v.Enrol(ctx, req)
	if err != nil {
		return accepted{}, err
	}
	return accepted{
		AgentID: res.AgentID, EnvID: res.EnvID, Cert: res.Cert, AgentVersion: req.AgentVersion, Facts: req.Facts, IP: p.ip,
		ClockOffset: offset,
	}, nil
}

// turnAway records the refusal, through the throttle, and then tells the agent.
func (h *Hub) turnAway(ctx context.Context, c *websocket.Conn, ip, mode, node, code, detail, reason string) {
	h.recordRefusal(ctx, ip, code, mode, node, reason)
	h.refuse(ctx, c, code, detail)
}

// refuse tells the agent why it is turned away, then closes the connection with
// policy-violation status and the code as the reason.
func (h *Hub) refuse(ctx context.Context, c *websocket.Conn, code, detail string) {
	h.refuseAfter(ctx, c, code, detail, 0)
}

// refuseAfter is refuse for a refusal that passes, with the time after which the
// agent may try again.
func (h *Hub) refuseAfter(ctx context.Context, c *websocket.Conn, code, detail string, retryAfter time.Duration) {
	_ = writeJSON(ctx, c, wire.Refuse{
		T: wire.TypeRefuse, Code: code, Detail: detail, RetryAfterMs: int(retryAfter / time.Millisecond),
	})
	_ = c.Close(websocket.StatusPolicyViolation, code)
}

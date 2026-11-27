// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub

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

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/proto/authn"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// nonceLen is the length of both handshake nonces.
const nonceLen = 32

// detailSelfNetworkPeer is what a node that reached TierPlan through its own ingress route is told.
const detailSelfNetworkPeer = "The ingress proxy connects to TierPlan directly, never through its own route."

// peerInfo is what the handler knows about the connection that the signed handshake must agree with.
type peerInfo struct {
	ip string
	// tls is the connection state when TierPlan terminates TLS itself.
	tls *tls.ConnectionState
	// trusted is true when the peer is a proxy in TIERPLAN_TRUSTED_PROXIES.
	trusted bool
	// aud is the host[:port] the node must have addressed.
	aud string
}

// accepted is a handshake the service let through.
type accepted struct {
	NodeID, EnvID, Name string
	Version, IP         string
}

// handshake runs the challenge, auth and welcome exchange. It sends a refuse message and closes the connection
// itself when the node is turned away, and then reports false.
func (h *Hub) handshake(ctx context.Context, c *websocket.Conn, r *http.Request, aud string) (accepted, bool) {
	ip := clientAddr(r)

	serverNonce := make([]byte, nonceLen)
	if _, err := rand.Read(serverNonce); err != nil {
		h.log.Error("generate an ingress handshake nonce", "error", err)
		h.refuse(ctx, c, ingresswire.RefuseInternal, "the server could not start the handshake", 0)
		return accepted{}, false
	}
	if err := writeJSON(ctx, c, ingresswire.Challenge{
		T: ingresswire.TypeChallenge, Proto: ingresswire.ProtoVersion, Nonce: serverNonce,
		TS: h.now().UnixMilli(), ServerVersion: h.serverVersion,
	}); err != nil {
		return accepted{}, false
	}

	// The read of the auth has a shorter limit than the handshake: a client that upgrades and stays silent
	// would otherwise pin one of the handshake slots for the whole handshake timeout for the price of one
	// connection attempt. Only this read is bounded; verification, which can wait on the database, and the
	// welcome stay under the caller's context.
	rctx, rcancel := context.WithTimeout(ctx, h.firstFrame)
	typ, data, err := c.Read(rctx)
	rcancel()
	if err != nil {
		h.log.Info("ingress handshake did not complete", "ip", ip, "error", err)
		return accepted{}, false
	}
	if typ != websocket.MessageText {
		h.turnAway(ctx, c, ip, "", ingresswire.RefuseAuthRejected, "a handshake message must be a text frame", "not_text", 0)
		return accepted{}, false
	}
	var a ingresswire.Auth
	if err := json.Unmarshal(data, &a); err != nil || a.T != ingresswire.TypeAuth {
		h.turnAway(ctx, c, ip, "", ingresswire.RefuseAuthRejected, "the handshake message is malformed", "malformed", 0)
		return accepted{}, false
	}

	// A peer on TierPlan's own self network is a connect relayed through TierPlan's own ingress route. It is
	// refused before anything is verified: the live trusted set would otherwise let it skip the channel
	// binding, and the proxy must reach TierPlan directly (SELF-05, D-10 point 3, A-05). A peer trusted only
	// through TIERPLAN_TRUSTED_PROXIES is not on the self network and goes on as before.
	if httpx.PeerOnSelfNetwork(r.Context()) {
		h.log.Info("ingress handshake refused", "ip", ip, "mode", knownMode(a.Mode), "code", ingresswire.RefuseSelfNetworkPeer, "reason", ingresswire.RefuseSelfNetworkPeer)
		h.turnAway(ctx, c, ip, knownMode(a.Mode), ingresswire.RefuseSelfNetworkPeer, detailSelfNetworkPeer, ingresswire.RefuseSelfNetworkPeer, 0)
		return accepted{}, false
	}

	// The instance ID is read before the auth is verified: an enrolment commits the node's row, and after that
	// commit only the welcome may follow, so nothing that can fail stands between the two (CR-A-01). A node that
	// still misses its welcome (the connection drops) enrols again with the same key and is given the same row.
	instanceID, err := h.readInstanceID(ctx)
	if err != nil {
		h.log.Error("read the instance ID for a welcome", "ip", ip, "error", err)
		h.refuse(ctx, c, ingresswire.RefuseInternal, "the server could not read its instance ID; it will retry", 0)
		return accepted{}, false
	}

	acc, err := h.verifyAuth(ctx, a, serverNonce, peerInfo{
		ip: ip, tls: r.TLS, trusted: httpx.PeerTrusted(r.Context()), aud: aud,
	})
	if err != nil {
		var re *RefuseError
		if errors.As(err, &re) {
			h.log.Info("ingress handshake refused", "ip", ip, "mode", knownMode(a.Mode), "code", re.Code, "reason", re.AuditReason)
			h.turnAway(ctx, c, ip, knownMode(a.Mode), re.Code, re.Detail, re.AuditReason, re.RetryAfter)
		} else {
			h.log.Error("ingress handshake failed", "ip", ip, "mode", knownMode(a.Mode), "error", err)
			h.refuse(ctx, c, ingresswire.RefuseInternal, "the server failed to verify this node; it will retry", 0)
		}
		return accepted{}, false
	}

	if err := writeJSON(ctx, c, ingresswire.Welcome{
		T: ingresswire.TypeWelcome, NodeID: acc.NodeID, EnvID: acc.EnvID, InstanceID: instanceID,
		HealthMs: int(ingresswire.HealthInterval / time.Millisecond), Pins: h.pinList(), ServerTime: h.now().UnixMilli(),
	}); err != nil {
		h.log.Warn("ingress node went away before the welcome", "node_id", acc.NodeID, "error", err)
		return accepted{}, false
	}
	return acc, true
}

// readInstanceID is the installation's ID through the hub's function, read on every call.
func (h *Hub) readInstanceID(ctx context.Context) (string, error) {
	if h.instanceID == nil {
		return "", errors.New("the hub was given no instance ID source")
	}
	return h.instanceID(ctx)
}

// knownMode is a mode of an auth message when it is one of the two, else "": an unknown mode is attacker
// text and never goes to a log line or an audit row.
func knownMode(m string) string {
	if m == ingresswire.ModeEnrol || m == ingresswire.ModeConnect {
		return m
	}
	return ""
}

// verifyAuth checks an auth message and dispatches by mode. The cheap checks come first and need no database:
// protocol version, mode, shape, audience and the channel binding. Only then is the service asked, with a
// transcript the hub rebuilt itself from the challenge it sent, never one the node supplied. A refusal is a
// *RefuseError; any other error is a server fault.
func (h *Hub) verifyAuth(ctx context.Context, a ingresswire.Auth, serverNonce []byte, p peerInfo) (accepted, error) {
	reject := func(detail, reason string) error {
		return &RefuseError{Code: ingresswire.RefuseAuthRejected, Detail: detail, AuditReason: reason}
	}
	switch {
	case a.Proto != ingresswire.ProtoVersion:
		return accepted{}, reject("the handshake protocol version is not supported", "proto")
	case a.Mode != ingresswire.ModeEnrol && a.Mode != ingresswire.ModeConnect:
		return accepted{}, reject("the handshake mode is not enrol or connect", "mode")
	case len(a.Nonce) != nonceLen || len(a.Sig) != ed25519.SignatureSize || len(a.Pub) != ed25519.PublicKeySize:
		return accepted{}, reject("the handshake message is malformed", "malformed")
	case a.Aud != p.aud:
		// A relay to a different host signs a different audience and fails here.
		h.log.Warn("ingress handshake addressed to a different host", "ip", p.ip, "audience", cleanText(a.Aud, 256), "expected", p.aud)
		return accepted{}, reject("the handshake was addressed to a different host than this server's public URL", "audience")
	}

	// Channel binding (D-06, D-07, D-32). The exporter value both rebuilt transcripts below use is fixed here,
	// once, as internal/tunnel/handshake.go does for agents. It starts as the value the node reported. When
	// TierPlan terminates TLS itself (r.TLS set and the peer not a trusted proxy) both ends compute the same
	// exporter over IngressEKMLabel, so the server's own value replaces it after the two are compared; a
	// mismatch means a proxy that is not listed in TIERPLAN_TRUSTED_PROXIES, or a relay. For a trusted peer
	// the reported value stays unchecked (D-32): a TLS terminator holds a different TLS session with the
	// node, whose exporter the server cannot compute, so the binding there rests on the operator's proxy
	// list, exactly as for agents.
	ekm := a.EKM
	if p.tls != nil && !p.trusted {
		ekm = authn.EKMFor(p.tls, authn.IngressEKMLabel)
		if !authn.EKMEqual(a.EKM, ekm) {
			h.log.Warn("ingress handshake is not bound to this TLS connection; a TLS-terminating proxy may be missing from TIERPLAN_TRUSTED_PROXIES", "ip", p.ip)
			return accepted{}, reject("the handshake is not bound to this TLS connection; if a reverse proxy terminates TLS, add it to TIERPLAN_TRUSTED_PROXIES", "channel_binding")
		}
	}

	version := cleanText(a.NodeVersion, ingresswire.MaxVersionLen)
	transcript := authn.IngressTranscript(a.Mode, a.Proto, serverNonce, a.Nonce, p.aud, a.Pub, ekm)

	if a.Mode == ingresswire.ModeConnect {
		res, err := h.svc.Connect(ctx, ConnectRequest{
			NodeID: a.NodeID, Pub: a.Pub, Sig: a.Sig, Transcript: transcript, Version: version, IP: p.ip,
		})
		if err != nil {
			return accepted{}, err
		}
		return accepted{NodeID: res.NodeID, EnvID: res.EnvID, Name: res.Name, Version: version, IP: p.ip}, nil
	}

	// Enrolment is the one handshake that costs a hash compare and a database read, so it is throttled per
	// address before either.
	if !h.gate.allowEnrol(p.ip) {
		return accepted{}, &RefuseError{
			Code: ingresswire.RefuseRateLimited, Detail: "too many enrolment attempts; try again later",
			AuditReason: ingresswire.RefuseRateLimited, RetryAfter: enrolRetryAfter,
		}
	}
	res, err := h.svc.Enrol(ctx, EnrolRequest{
		Token: a.Token, Pub: a.Pub, Sig: a.Sig, Transcript: transcript,
		Name: a.Name, SwarmNodeID: a.SwarmNodeID, Version: version, IP: p.ip,
	})
	if err != nil {
		return accepted{}, err
	}
	return accepted{NodeID: res.NodeID, EnvID: res.EnvID, Name: res.Name, Version: version, IP: p.ip}, nil
}

// turnAway records the refusal, through the throttle, and then tells the node.
func (h *Hub) turnAway(ctx context.Context, c *websocket.Conn, ip, mode, code, detail, reason string, retryAfter time.Duration) {
	h.recordRefusal(ctx, ip, code, mode, reason)
	h.refuse(ctx, c, code, detail, retryAfter)
}

// refuse tells the node why it is turned away, with the time after which it may try again for a refusal that
// passes, then closes the connection with policy-violation status and the code as the reason.
func (h *Hub) refuse(ctx context.Context, c *websocket.Conn, code, detail string, retryAfter time.Duration) {
	_ = writeJSON(ctx, c, ingresswire.Refuse{
		T: ingresswire.TypeRefuse, Code: code, Detail: detail, RetryAfterMs: int(retryAfter / time.Millisecond),
	})
	_ = c.Close(websocket.StatusPolicyViolation, code)
}

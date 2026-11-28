// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package ingresswire is the contract between the TierPlan server and its
// ingress nodes: the WebSocket subprotocol, the handshake messages, the four
// messages a node may send, the bundle envelope the server sends, the size
// limits and the refusal and close codes. The server and the ingress supervisor
// are built from different modules, so everything both sides must agree on
// lives here and is pinned by the golden files in testdata (frozen_test.go).
// Standard library plus proto/ingressbundle (the bundle itself) only.
//
// Frozen for all of 0.x: change no field name, type or meaning, and add only
// optional fields. A change a supervisor in the field cannot follow ships as a
// new subprotocol name (tierplan.ingress.v2). A test that needs a golden file to
// change has found a break; it is not a test to refresh.
//
// This package never imports proto/wire. The agent channel and the ingress
// channel share an idea, not code, so a change to one can never break the other.
// The handshake messages have the same shape on purpose (a challenge, a signed
// answer, a welcome), but they are separate types with separate subprotocol
// names, and each channel refuses the other's signature.
//
// The ingress rule that differs from proto/wire is the closed message set. In
// proto/wire a receiver ignores a message whose type it does not know. Here the
// server does not: a node that sends a message whose "t" is not hello, applied,
// rejected or health has its session closed (reason protocol_error). A field the
// receiver does not know is still ignored, so an optional field can be added
// without a protocol change. A later node message type is announced first: the
// server names it in the welcome's features list, and a node sends it only when
// it is named there. A node never learns its configuration by asking; the
// server pushes every bundle, and the node can only say hello, applied, rejected
// and health.
//
// The instance ID in Welcome and in BundleEnvelope guards against a misdirected
// or a stale TierPlan (a node enrolled with another installation, or one
// restored from an old backup). It is not server authentication: the server is
// authenticated by TLS and the pinned SPKI hashes of the welcome, not by a value
// the server itself reports.
package ingresswire

import "time"

const (
	// Subprotocol names the handshake. It is also the version transition
	// mechanism: an incompatible handshake becomes tierplan.ingress.v2.
	Subprotocol = "tierplan.ingress.v1"
	// ProtoVersion is the protocol version carried in every handshake message
	// and signed into the transcript.
	ProtoVersion = 1
	// MaxHandshakeBytes is the read limit of the raw WebSocket until the
	// handshake has finished.
	MaxHandshakeBytes = 16 << 10
	// HandshakeTimeout bounds the whole handshake, from accept to welcome.
	HandshakeTimeout = 10 * time.Second
	// MaxNodeMessageBytes is the server's read limit for every message after the
	// welcome. A node says hello, applied, rejected and health, and none of them
	// comes near it.
	MaxNodeMessageBytes = 16 << 10
	// MaxBundleMessageBytes is the node's read limit for a bundle message, the
	// only large message on the channel.
	MaxBundleMessageBytes = 64 << 20
	// HealthInterval is how often a node sends a health message. The welcome
	// carries the value in milliseconds.
	HealthInterval = 30 * time.Second
)

// Limits the server applies when it acts on a node message. They are not
// enforced while parsing; they are named here so both modules know them.
const (
	// MaxNameLen is the longest node name (enrol mode).
	MaxNameLen = 128
	// MaxVersionLen is the longest node version string.
	MaxVersionLen = 64
	// MaxSchemas is how many bundle schemas a hello may list.
	MaxSchemas = 16
	// MaxReasonLen is the longest reason in a rejected message.
	MaxReasonLen = 512
	// MaxIdentityLen is the longest swarm node ID either side accepts.
	MaxIdentityLen = 128
)

// Message types, the value of the "t" field.
const (
	// TypeChallenge, TypeWelcome, TypeRefuse and TypeBundle are the only types
	// the server sends. A node must never send one of them.
	TypeChallenge = "challenge"
	TypeWelcome   = "welcome"
	TypeRefuse    = "refuse"
	TypeBundle    = "bundle"
	// TypeAuth is the node's answer to the challenge, the only handshake message
	// a node sends.
	TypeAuth = "auth"
	// TypeHello, TypeApplied, TypeRejected and TypeHealth are the closed set of
	// messages a node sends after the welcome.
	TypeHello    = "hello"
	TypeApplied  = "applied"
	TypeRejected = "rejected"
	TypeHealth   = "health"
)

// Modes of an auth message. hello is the first message after the welcome, never
// an auth mode.
const (
	// ModeEnrol is the first connection of a node: it presents the ingress join
	// token and a public key.
	ModeEnrol = "enrol"
	// ModeConnect is every later connection: it names its node ID and signs with
	// the key it enrolled.
	ModeConnect = "connect"
)

// Token prefixes.
const (
	// TokenPrefix starts an ingress join token, tpit_<envId>_<secret>.
	TokenPrefix = "tpit_"
	// AgentTokenPrefix starts an agent join token. It is named here only so a
	// node and the server can recognise an agent token given to the ingress
	// channel and say so; the agent's own constant lives with the agent.
	AgentTokenPrefix = "tpjt_"
)

// Refusal codes, the code of a Refuse message. The server closes the connection
// with status 1008 and the code as the reason afterwards.
const (
	// RefuseAuthRejected is the one answer to every bad credential, so a probe
	// cannot tell a wrong token from a disabled or unknown one.
	RefuseAuthRejected = "auth_rejected"
	// RefuseWrongTokenKind means the token is an agent join token (AgentTokenPrefix).
	RefuseWrongTokenKind = "wrong_token_kind"
	// RefuseRevoked is sent on connect to a node that proved its key and that an
	// administrator has revoked: the node itself was revoked, not a token. The
	// node must stop connecting. It is the same text as CloseRevoked, the close
	// reason of a live session whose node is revoked.
	RefuseRevoked = "revoked"
	// RefuseNodeRevoked is sent on enrolment from a host an administrator
	// revoked with the re-enrolment block: the join token is still valid, but
	// this node may not enrol again until an administrator allows it.
	RefuseNodeRevoked = "node_revoked"
	// RefuseNodeNotInSwarm means the swarm node the node reports is not in the
	// node list the server last read. It passes once the node is listed.
	RefuseNodeNotInSwarm = "node_not_in_swarm"
	// RefuseNodeUnverified means the server cannot yet confirm the node. It
	// passes after a retry.
	RefuseNodeUnverified = "node_unverified"
	// RefuseNodeAlreadyEnrolled means this swarm node already has a node record.
	RefuseNodeAlreadyEnrolled = "node_already_enrolled"
	// RefuseRateLimited means too many attempts; RetryAfterMs says when to retry.
	RefuseRateLimited = "rate_limited"
	// RefuseInsecureTransport means the session is not TLS-verified.
	RefuseInsecureTransport = "insecure_transport"
	// RefuseSelfNetworkPeer means the connection reached TierPlan through its own ingress route: the TCP
	// peer is on TierPlan's self network. The proxy connects to TierPlan directly, never through a route it
	// serves itself (SELF-05, D-10). A node that does not know the code logs it as it is.
	RefuseSelfNetworkPeer = "self_network_peer"
	// RefuseInternal means the server failed while handling an otherwise valid
	// handshake. The node retries with backoff.
	RefuseInternal = "internal_error"
)

// Close reasons, the websocket close reason text once the session has started.
const (
	CloseProtocolError = "protocol_error"
	CloseRevoked       = "revoked"
	CloseReplaced      = "replaced"
	CloseRemoved       = "removed"
	CloseTimeout       = "timeout"
	CloseShutdown      = "shutdown"
)

// ValidIdentity reports whether s may be a swarm node ID: 1 to MaxIdentityLen
// characters of letters, digits, colon, underscore, dot and hyphen. Real IDs are
// base 36 or colon-separated groups. The server refuses an ID that fails this so
// none reaches SQL or the audit log.
func ValidIdentity(s string) bool {
	if s == "" || len(s) > MaxIdentityLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == ':', c == '_', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}

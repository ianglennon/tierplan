// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package wire is the contract between the Bayplan server and its agents: the
// WebSocket subprotocol, the handshake messages, the reason codes and the kind
// byte that opens every multiplexed stream. The server and the agent are built
// from different modules and released together, so everything both sides must
// agree on lives here and nowhere else. Standard library only.
//
// Every deployed agent speaks this handshake (D-05), so a change that an old
// agent cannot follow ships as a new subprotocol name (bayplan.agent.v2) and
// the server accepts both for a window. That is what Subprotocol is for.
package wire

import "time"

const (
	// Subprotocol names the handshake. It is also the version transition
	// mechanism: an incompatible handshake becomes bayplan.agent.v2.
	Subprotocol = "bayplan.agent.v1"
	// ProtoVersion is the protocol version carried in every handshake message
	// and signed into the transcript.
	ProtoVersion = 1
	// MaxHandshakeBytes is the read limit of the raw WebSocket until the
	// handshake has finished. Wrapping the connection as a net.Conn removes the
	// limit, so it is applied before that point only.
	MaxHandshakeBytes = 16 << 10
	// HandshakeTimeout bounds the whole handshake, from accept to welcome.
	HandshakeTimeout = 10 * time.Second
	// DefaultHeartbeatMs is how often an agent sends a heartbeat, in
	// milliseconds (D-41). The welcome message tells the agent the value.
	DefaultHeartbeatMs = 10_000
)

// StreamKindDockerHTTP is the first byte of a server-opened stream that carries
// plain HTTP/1.1 to the agent's allowlisting proxy in front of the Docker
// socket. A stream that starts with any other byte is closed by the agent.
const StreamKindDockerHTTP byte = 0x01

// StreamKindControl is the first byte of the one stream the agent opens after
// the welcome. It carries newline-delimited JSON, an agent message per line
// (heartbeats), and the server may write lines back. Each side ignores a line
// whose type it does not know, so later plans add messages without a protocol
// change.
const StreamKindControl byte = 0x02

// MaxControlLine is the longest line either side accepts on the control stream.
// The agent is authenticated by then, but not trusted to be well behaved.
const MaxControlLine = 64 << 10

// Message types, the value of the "t" field.
const (
	TypeChallenge = "challenge"
	TypeAuth      = "auth"
	TypeWelcome   = "welcome"
	TypeRefuse    = "refuse"
)

// Control-stream message types, the value of the "t" field of a line.
const (
	// TypeHeartbeat is sent by the agent every heartbeatMs.
	TypeHeartbeat = "heartbeat"
)

// Modes of an auth message.
const (
	// ModeEnrol is the first connection of a host: it presents the join token
	// and a public key, and receives a certificate.
	ModeEnrol = "enrol"
	// ModeHello is every later connection: it presents the certificate and
	// signs with the key it was issued for.
	ModeHello = "hello"
)

// Challenge is the first message, server to agent. The nonce is fresh for
// every connection.
type Challenge struct {
	T             string   `json:"t"`
	Proto         int      `json:"proto"`
	Nonce         []byte   `json:"nonce"`
	TS            int64    `json:"ts"`
	ServerVersion string   `json:"serverVersion,omitempty"`
	Features      []string `json:"features,omitempty"`
}

// Auth is the agent's answer to the challenge. Sig is an Ed25519 signature over
// the transcript (see package authn): a proof of possession of the key in Pub
// when enrolling, and a proof of identity under the certificate's key in hello
// mode.
type Auth struct {
	T            string `json:"t"`
	Mode         string `json:"mode"`
	Proto        int    `json:"proto"`
	AgentVersion string `json:"agentVersion,omitempty"`
	// AgentID is the agent's ID, set in hello mode only.
	AgentID string `json:"agentId,omitempty"`
	// Aud is the host[:port] the agent dialed. The server checks it against its
	// own public URL, so a relay to a different host fails.
	Aud   string `json:"aud"`
	Nonce []byte `json:"nonce"`
	TS    int64  `json:"ts"`
	// Cert is the agent's DER certificate (hello mode).
	Cert []byte `json:"cert,omitempty"`
	// Pub is the agent's Ed25519 public key (enrol mode).
	Pub []byte `json:"pub,omitempty"`
	// Token is the join token (enrol mode). It travels here and never in a URL
	// so a proxy does not log it.
	Token string `json:"token,omitempty"`
	// EKM is the TLS exported keying material, empty when the agent has none.
	EKM   []byte `json:"ekm,omitempty"`
	Sig   []byte `json:"sig"`
	Facts Facts  `json:"facts"`
}

// Facts is what an agent reports about itself and its engine.
type Facts struct {
	EngineID        string      `json:"engineId,omitempty"`
	Hostname        string      `json:"hostname,omitempty"`
	EngineReachable bool        `json:"engineReachable"`
	APIVersion      string      `json:"apiVersion,omitempty"`
	EngineVersion   string      `json:"engineVersion,omitempty"`
	Swarm           *SwarmFacts `json:"swarm,omitempty"`
	Policy          PolicyFacts `json:"policy"`
}

// SwarmFacts describes the swarm a host belongs to. Only a manager knows the
// cluster ID.
type SwarmFacts struct {
	State      string   `json:"state"`
	NodeID     string   `json:"nodeId,omitempty"`
	ClusterID  string   `json:"clusterId,omitempty"`
	Control    bool     `json:"control"`
	ManagerIDs []string `json:"managerIds,omitempty"`
}

// PolicyFacts are the limits the host owner set on the agent (D-28).
type PolicyFacts struct {
	ReadOnly   bool `json:"readOnly"`
	SelfUpdate bool `json:"selfUpdate"`
}

// Welcome is the server's answer to a successful handshake. Cert is the DER
// certificate issued during enrolment and empty in hello mode.
type Welcome struct {
	T           string   `json:"t"`
	AgentID     string   `json:"agentId"`
	EnvID       string   `json:"envId"`
	Cert        []byte   `json:"cert,omitempty"`
	HeartbeatMs int      `json:"heartbeatMs"`
	URLs        []string `json:"urls,omitempty"`
	Pins        []string `json:"pins,omitempty"`
	ServerTime  int64    `json:"serverTime"`
}

// Refuse is the server's answer to a handshake it will not accept. The server
// closes the connection with status 1008 and Code as the reason afterwards.
type Refuse struct {
	T            string `json:"t"`
	Code         string `json:"code"`
	Detail       string `json:"detail,omitempty"`
	RetryAfterMs int    `json:"retryAfterMs,omitempty"`
}

// Heartbeat is the agent's proof of life on the control stream. The server keeps
// it in memory only (RESEARCH Pitfall 8).
type Heartbeat struct {
	T string `json:"t"`
	// EngineReachable is whether GET /_ping on the Docker socket answered.
	EngineReachable bool `json:"engineReachable"`
	// ClockMs is the agent's clock in Unix milliseconds, for the clock-skew hint.
	ClockMs int64 `json:"clockMs"`
}

// Reasons are the connection failure codes of D-40. They are what the UI and
// the OpenAPI contract show, each with plain-language text and a fix hint.
const (
	ReasonAgentOffline        = "agent_offline"
	ReasonTLSUntrusted        = "tls_untrusted"
	ReasonAuthRejected        = "auth_rejected"
	ReasonCertExpired         = "cert_expired"
	ReasonRevoked             = "revoked"
	ReasonBlocked             = "blocked"
	ReasonVersionIncompatible = "version_incompatible"
	ReasonClockSkew           = "clock_skew"
	ReasonTimeout             = "timeout"
	ReasonNotAManager         = "not_a_manager"
	ReasonNoQuorum            = "no_quorum"
	ReasonClusterChanged      = "cluster_changed"
	ReasonEngineUnreachable   = "engine_unreachable"
	ReasonUnsupportedEngine   = "unsupported_engine"
	ReasonAgentPolicyDenied   = "agent_policy_denied"
)

// Refusals only an enrolment can meet. token_disabled is never sent to the
// agent: it is reported as auth_rejected and kept specific in the audit detail,
// so a probe cannot tell a disabled token from a wrong one.
const (
	RefuseDuplicateEnvironment = "duplicate_environment"
	RefuseTypeMismatch         = "type_mismatch"
	RefuseTokenDisabled        = "token_disabled"
	RefuseInsecureTransport    = "insecure_transport"
	RefuseRateLimited          = "rate_limited"
	// RefuseInternal means the server failed while handling an otherwise valid
	// handshake. The agent should retry with backoff.
	RefuseInternal = "internal_error"
)

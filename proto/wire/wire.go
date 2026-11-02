// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package wire is the contract between the TierPlan server and its agents: the
// WebSocket subprotocol, the handshake messages, the reason codes and the kind
// byte that opens every multiplexed stream. The server and the agent are built
// from different modules and released together, so everything both sides must
// agree on lives here and nowhere else. Standard library only.
//
// Every deployed agent speaks this handshake (D-05), so a change that an old
// agent cannot follow ships as a new subprotocol name (tierplan.agent.v2) and
// the server accepts both for a window. That is what Subprotocol is for.
package wire

import "time"

const (
	// Subprotocol names the handshake. It is also the version transition
	// mechanism: an incompatible handshake becomes tierplan.agent.v2.
	Subprotocol = "tierplan.agent.v1"
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

// The control stream is the one stream the agent opens after the welcome; this is
// its first byte. It carries newline-delimited JSON, an agent message per line
// (heartbeats), and the server may write lines back. Each side ignores a line
// whose type it does not know, so later plans add messages without a protocol
// change.
const StreamKindControl byte = 0x02

// StreamKindHostNet is the first byte of a server-opened stream that asks the
// agent for its host's IPv4 routes. The server opens it only when the agent
// reported Facts.HostNet == HostNetOK; an agent older than Phase 10 closes a
// stream of a kind it does not know, so the server never asks one. The agent
// writes one JSON HostNetDoc read fresh from its route file and closes the
// stream. It reads one file and runs nothing: it never reaches the Docker socket.
const StreamKindHostNet byte = 0x03

// What Facts.HostNet says about the host route file.
const (
	// HostNetOK means the agent can read the host's route file.
	HostNetOK = "ok"
	// HostNetUnmounted means the route file is not mounted, or not readable.
	HostNetUnmounted = "unmounted"
)

// HostNetDoc.Error codes.
const (
	// HostNetUnreadable means the route file could not be read.
	HostNetUnreadable = "unreadable"
	// HostNetTooLarge means a line or the file was larger than the agent reads.
	HostNetTooLarge = "too_large"
)

// MaxHostNetBytes is the most a server reads from a host-net stream: the 512
// routes an agent keeps come to well under half of it.
const MaxHostNetBytes = 64 << 10

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
	// TypeDenied is sent by the agent for every request its policy refused.
	TypeDenied = "denied"
	// TypeRenew is sent by the agent when its certificate is due for renewal
	// (D-04). The server answers TypeRenewed or TypeRenewRefused.
	TypeRenew = "renew"
	// TypeRenewed is the server's answer to TypeRenew: the new certificate.
	TypeRenewed = "renewed"
	// TypeRenewRefused is the server's answer when it will not renew.
	TypeRenewRefused = "renew_refused"
	// TypeURLs is sent by the server to a connected agent when the address agents
	// dial, or the server's pins, changed (D-19, D-06).
	TypeURLs = "urls"
)

// Why the server did not renew a certificate, the code of a RenewRefused message.
// A revoked agent is told ReasonRevoked, and the server closes its session.
const (
	// RenewNotDue means the certificate still has more than half of its life left.
	// The agent asks at two thirds of the life, so a request earlier than half is
	// a fault or a flood, and every renewal is a write on the single database
	// writer.
	RenewNotDue = "not_due"
	// RenewInternal means the server failed while renewing. The agent retries.
	RenewInternal = "internal_error"
)

// Limits on a URLUpdate, which the agent applies to what a server sends.
const (
	// MaxPushedURLs is how many addresses one URLUpdate may list.
	MaxPushedURLs = 8
	// MaxURLLen is the longest address an agent keeps.
	MaxURLLen = 2048
)

// Why the agent refused a request, the reason of a Denied message.
const (
	// DeniedAllowlist means the request is not on the agent's allowlist.
	DeniedAllowlist = "allowlist"
	// DeniedReadOnly means the host set TIERPLAN_AGENT_READ_ONLY and the request
	// would change something.
	DeniedReadOnly = "read_only_flag"
	// DeniedHostPolicy means the host did not set
	// TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES and the service spec, a volume it
	// mounts, or a volume create gives tasks host access.
	DeniedHostPolicy = "host_policy"
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
	// HostNet says whether the agent can read its host's routes: HostNetOK or
	// HostNetUnmounted. Absent means an agent older than Phase 10, which has no
	// host-net stream; the server asks only when it is HostNetOK.
	HostNet string `json:"hostNet,omitempty"`
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
	// PrivilegedServices is true when the host set
	// TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES, so a manager's agent forwards
	// service specs that give tasks host access, and every agent-managed host's
	// agent forwards the volume creates it would refuse (volume creates). Absent
	// reads as false.
	PrivilegedServices bool `json:"privilegedServices,omitempty"`
}

// HostRoute is one IPv4 route of the agent's host, as the agent read it: a
// destination network (masked, never the default route, never a single host)
// and the interface it leaves by. The server treats every field as untrusted.
type HostRoute struct {
	Iface   string `json:"iface"`
	Dst     string `json:"dst"`
	Gateway string `json:"gw,omitempty"`
}

// HostNetDoc is the one document an agent writes on a StreamKindHostNet stream.
type HostNetDoc struct {
	V         int         `json:"v"`
	Routes    []HostRoute `json:"routes"`
	Truncated bool        `json:"truncated,omitempty"`
	// Error is HostNetUnreadable or HostNetTooLarge, with no routes.
	Error string `json:"error,omitempty"`
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

// Denied is the agent telling the server that its policy refused a request, with
// what was asked and why. The server records it in the audit log (D-29): it means
// a bug in TierPlan or a server that is asking for more than it should. Method and
// Path come from the server's own request, so they are capped on both ends.
type Denied struct {
	T      string `json:"t"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Renew is the agent asking for a new certificate for the key it already holds.
// It carries nothing: the server knows the agent from the session.
type Renew struct {
	T string `json:"t"`
}

// Renewed is the server's answer to a Renew: a DER certificate for the same key,
// with a new serial and a fresh lifetime.
type Renewed struct {
	T    string `json:"t"`
	Cert []byte `json:"cert"`
}

// RenewRefused is the server saying it did not renew. Code is one of the Renew
// codes above or ReasonRevoked.
type RenewRefused struct {
	T    string `json:"t"`
	Code string `json:"code"`
}

// URLUpdate is the server telling a connected agent where to dial from now on and
// which server pins are current. URLs lists the endpoints, the one to dial first
// at the front. The agent keeps the address it had before in its dial list for a
// while (D-19), but the server accepts only its current host and port, so that
// entry is an undo window, not a fallback the server honours. Pins are what a welcome carries: the server's own certificate pins.
type URLUpdate struct {
	T    string   `json:"t"`
	URLs []string `json:"urls"`
	Pins []string `json:"pins,omitempty"`
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
	// ReasonUnknownAgent is a hello refused because the certificate was issued by
	// the environment's own CA for an agent the environment does not know, as
	// happens to an agent enrolled after the backup this server was restored from
	// (D-31). It is only ever said to a holder of a certificate the environment
	// really issued; every other failed hello stays ReasonAuthRejected.
	ReasonUnknownAgent = "unknown_agent"
)

// Refusals only an enrolment can meet. token_disabled and token_superseded are
// never sent to the agent: each is reported as auth_rejected and kept specific
// in the audit detail, so a probe cannot tell a disabled or replaced token from
// a wrong one.
const (
	RefuseDuplicateEnvironment = "duplicate_environment"
	RefuseTypeMismatch         = "type_mismatch"
	// RefuseHostMismatch means a standalone environment is bound to one host and a
	// different host enrolled into it.
	RefuseHostMismatch      = "host_mismatch"
	RefuseTokenDisabled     = "token_disabled"
	RefuseTokenSuperseded   = "token_superseded"
	RefuseInsecureTransport = "insecure_transport"
	RefuseRateLimited       = "rate_limited"
	// RefuseInternal means the server failed while handling an otherwise valid
	// handshake. The agent should retry with backoff.
	RefuseInternal = "internal_error"
)

// MaxIdentityLen is the longest engine, node or cluster ID either side accepts.
const MaxIdentityLen = 128

// ValidIdentity reports whether s may be an engine, node or cluster ID: 1 to 128
// characters of letters, digits, colon, underscore, dot and hyphen. Real IDs are
// hex, base 36 or colon-separated groups. The agent drops an ID that fails this
// and the server refuses one, so none reaches SQL or the audit log.
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

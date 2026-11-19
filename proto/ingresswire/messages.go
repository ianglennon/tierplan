// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresswire

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// Challenge is the first message, server to node. The nonce is fresh for every
// connection.
type Challenge struct {
	T             string   `json:"t"`
	Proto         int      `json:"proto"`
	Nonce         []byte   `json:"nonce"`
	TS            int64    `json:"ts"`
	ServerVersion string   `json:"serverVersion,omitempty"`
	Features      []string `json:"features,omitempty"`
}

// Auth is the node's answer to the challenge. Sig is an Ed25519 signature over
// the transcript: a proof of possession of the key in Pub. Each field says which
// mode it belongs to.
type Auth struct {
	T           string `json:"t"`
	Mode        string `json:"mode"`
	Proto       int    `json:"proto"`
	NodeVersion string `json:"nodeVersion,omitempty"`
	// NodeID is the node's ID, set in connect mode only.
	NodeID string `json:"nodeId,omitempty"`
	// Name is the name the node asks for (enrol mode).
	Name string `json:"name,omitempty"`
	// SwarmNodeID is the swarm node ID the node runs on (enrol mode).
	SwarmNodeID string `json:"swarmNodeId,omitempty"`
	// Aud is the host[:port] the node dialed. The server checks it against its
	// own public URL, so a relay to a different host fails.
	Aud   string `json:"aud"`
	Nonce []byte `json:"nonce"`
	TS    int64  `json:"ts"`
	// Pub is the node's Ed25519 public key (both modes): the key to enrol, or the
	// key the signature is checked against in connect mode.
	Pub []byte `json:"pub"`
	// Token is the ingress join token (enrol mode). It travels here and never in
	// a URL, so a proxy does not log it.
	Token string `json:"token,omitempty"`
	// EKM is the TLS exported keying material, empty when the node has none.
	EKM []byte `json:"ekm,omitempty"`
	Sig []byte `json:"sig"`
}

// Welcome is the server's answer to a successful handshake. InstanceID is the
// installation's ID: it guards against a misdirected or stale TierPlan and is
// not server authentication (TLS and the pins are).
type Welcome struct {
	T          string   `json:"t"`
	NodeID     string   `json:"nodeId"`
	EnvID      string   `json:"envId"`
	InstanceID string   `json:"instanceId"`
	HealthMs   int      `json:"healthMs"`
	Pins       []string `json:"pins,omitempty"`
	ServerTime int64    `json:"serverTime"`
	// Features lists the optional behaviour the server offers, including any node
	// message type later than the four of this version. A node sends a message
	// type only when it is named here.
	Features []string `json:"features,omitempty"`
}

// Refuse is the server's answer to a handshake it will not accept. The server
// closes the connection with status 1008 and Code as the reason afterwards.
type Refuse struct {
	T            string `json:"t"`
	Code         string `json:"code"`
	Detail       string `json:"detail,omitempty"`
	RetryAfterMs int    `json:"retryAfterMs,omitempty"`
}

// Hello is the first message a node sends after the welcome. It says what the
// node is and which bundle it already runs; the server answers with a bundle.
type Hello struct {
	T string `json:"t"`
	// Version is the node's version string; the server keeps at most
	// MaxVersionLen bytes of it.
	Version string `json:"version"`
	// Schemas lists the bundle schemas the node can apply; the server reads at
	// most MaxSchemas of them.
	Schemas []int `json:"schemas"`
	// AppliedID is the ID of the bundle the node currently runs, if any.
	AppliedID string `json:"appliedId,omitempty"`
}

// Applied says the node verified a bundle and made it live. ID is the bundle ID.
type Applied struct {
	T  string `json:"t"`
	ID string `json:"id"`
}

// Rejected says the node did not apply a bundle, and why. Reason is a short
// machine-readable code; the server keeps at most MaxReasonLen bytes of it.
type Rejected struct {
	T      string `json:"t"`
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// Health is the node's periodic proof of life, every HealthInterval.
type Health struct {
	T         string `json:"t"`
	TraefikOK bool   `json:"traefikOk"`
	// AppliedID is the ID of the bundle the node runs now, if any.
	AppliedID string `json:"appliedId,omitempty"`
}

var (
	// ErrUnknownMessage is the error for a node message whose type is not one of
	// hello, applied, rejected or health. The ingress channel accepts only those
	// four; the server closes the session on it.
	ErrUnknownMessage = errors.New("ingresswire: the ingress channel accepts only hello, applied, rejected and health")
	// ErrMalformed is the error for a node message that is not a JSON object, or
	// has no type, or whose known fields have the wrong JSON type.
	ErrMalformed = errors.New("ingresswire: malformed node message")
)

// ParseNodeMessage parses one message a node sent after the welcome and returns a
// Hello, Applied, Rejected or Health by value. Any other type, including every
// type only the server sends, is ErrUnknownMessage; bad JSON or a missing type is
// ErrMalformed. A field it does not know is ignored. It enforces no field limit:
// the server applies MaxVersionLen, MaxSchemas and MaxReasonLen when it acts on
// the message.
func ParseNodeMessage(data []byte) (any, error) {
	var head struct {
		T string `json:"t"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if head.T == "" {
		return nil, fmt.Errorf("%w: no type", ErrMalformed)
	}
	var v any
	switch head.T {
	case TypeHello:
		v = new(Hello)
	case TypeApplied:
		v = new(Applied)
	case TypeRejected:
		v = new(Rejected)
	case TypeHealth:
		v = new(Health)
	default:
		return nil, ErrUnknownMessage
	}
	if err := json.Unmarshal(data, v); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	switch m := v.(type) {
	case *Hello:
		return *m, nil
	case *Applied:
		return *m, nil
	case *Rejected:
		return *m, nil
	default:
		return *v.(*Health), nil
	}
}

// BundleEnvelope is the one message that carries a bundle, server to node. The
// instance ID sits outside the bundle so the bundle ID stays a function of its
// content alone. It holds every private key of one environment's node, so its
// String, GoString and LogValue never show file data.
//
// It is sent as one binary WebSocket message of at most MaxBundleMessageBytes. A
// node must call Bundle.Verify before it applies anything.
type BundleEnvelope struct {
	T          string               `json:"t"`
	InstanceID string               `json:"instanceId"`
	Bundle     ingressbundle.Bundle `json:"bundle"`
}

// String prints the type, the instance ID and the bundle's own redacted form.
func (e BundleEnvelope) String() string {
	return fmt.Sprintf("%s instance %s: %s", e.T, e.InstanceID, e.Bundle.String())
}

// GoString is String, so %#v shows no file data either.
func (e BundleEnvelope) GoString() string { return e.String() }

// LogValue is a group of the type, the instance ID and the bundle's redacted log
// value, so an envelope passed to slog never writes file data.
func (e BundleEnvelope) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("t", e.T),
		slog.String("instanceId", e.InstanceID),
		slog.Any("bundle", e.Bundle),
	)
}

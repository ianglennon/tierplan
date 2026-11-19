// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresswire

// This file is the node side of the ingress channel: the functions that make the
// node's key, build and sign its auth message, check the welcome and every bundle
// against the instance ID the node pinned, and enforce the bundle size limit.
// The Phase 23 supervisor and the test supervisor both call these, so the two can
// never disagree on a byte of what a node sends or checks. Standard library plus
// proto/authn and proto/ingressbundle only: no WebSocket dependency enters proto.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ianglennon/tierplan/proto/authn"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

var (
	// ErrInstanceMismatch means a welcome or a bundle names an instance ID other
	// than the one the node pinned, or the node has pinned none. The node does not
	// apply the bundle. It guards against a misdirected or a stale TierPlan; it is
	// not server authentication (TLS and the pinned SPKI hashes are).
	ErrInstanceMismatch = errors.New("ingresswire: the instance ID is not the one this node pinned")
	// ErrBundleTooLarge means a bundle message is longer than
	// MaxBundleMessageBytes. It is refused before it is parsed.
	ErrBundleTooLarge = errors.New("ingresswire: the bundle message exceeds MaxBundleMessageBytes")
)

// NewNodeKey makes a node's identity key: an Ed25519 private key from crypto/rand.
// It is the only place a node key is made. A node calls it once, at first start,
// and keeps the key on its own volume (persisting it is the supervisor's job,
// not this package's), because a new key is a new identity that needs a fresh
// enrolment. The private key never leaves the node; only its public half goes to
// the server, in Auth.Pub.
func NewNodeKey() (ed25519.PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ingresswire: make the node key: %w", err)
	}
	return priv, nil
}

// NewNonce returns 32 random bytes from crypto/rand, the node's nonce of one
// handshake.
func NewNonce() ([]byte, error) {
	n := make([]byte, 32)
	if _, err := rand.Read(n); err != nil {
		return nil, fmt.Errorf("ingresswire: make a nonce: %w", err)
	}
	return n, nil
}

// EnrolOptions are the inputs of EnrolAuth.
type EnrolOptions struct {
	// Token is the ingress join token (TokenPrefix).
	Token string
	// Name is the name the node asks for.
	Name string
	// SwarmNodeID is the swarm node ID the node runs on.
	SwarmNodeID string
	// Version is the node's version string.
	Version string
	// Aud is the host[:port] the node dialed.
	Aud string
	// Key is the node's key, from NewNodeKey.
	Key ed25519.PrivateKey
	// EKM is the exporter of the node's own TLS session,
	// authn.EKMFor(state, authn.IngressEKMLabel). Behind a TLS terminator it is
	// the node-to-terminator session's exporter, so a trusted-proxy server folds
	// this reported value into its rebuild in place of its own; a direct peer's
	// server substitutes its own, equal value. The signature covers these bytes
	// and Auth.EKM carries exactly them.
	EKM []byte
	// Now is the time stamped into the message.
	Now time.Time
}

// ConnectOptions are the inputs of ConnectAuth.
type ConnectOptions struct {
	// NodeID is the node ID the server gave at enrolment.
	NodeID string
	// Version is the node's version string.
	Version string
	// Aud is the host[:port] the node dialed.
	Aud string
	// Key is the key the node enrolled, from NewNodeKey.
	Key ed25519.PrivateKey
	// EKM is the exporter of the node's own TLS session, as in EnrolOptions.
	EKM []byte
	// Now is the time stamped into the message.
	Now time.Time
}

// pubOf returns the public half of key, or an error for a key of the wrong size.
func pubOf(key ed25519.PrivateKey) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("ingresswire: the node key is not an Ed25519 private key")
	}
	return []byte(key.Public().(ed25519.PublicKey)), nil
}

// EnrolAuth builds the node's auth message for its first connection, in mode
// enrol, signed with o.Key over authn.IngressTranscript of the challenge's nonce,
// the node's own nonce, the audience, the node's public key and the EKM. It
// refuses a token that is not an ingress join token (naming an agent join token
// as such), an empty audience and a key of the wrong size, and returns no Auth
// then.
func EnrolAuth(ch Challenge, o EnrolOptions) (Auth, error) {
	if !strings.HasPrefix(o.Token, TokenPrefix) {
		if strings.HasPrefix(o.Token, AgentTokenPrefix) {
			return Auth{}, errors.New("ingresswire: this is an agent join token, not an ingress join token")
		}
		return Auth{}, errors.New("ingresswire: not an ingress join token")
	}
	if o.Aud == "" {
		return Auth{}, errors.New("ingresswire: no audience")
	}
	pub, err := pubOf(o.Key)
	if err != nil {
		return Auth{}, err
	}
	nonce, err := NewNonce()
	if err != nil {
		return Auth{}, err
	}
	a := Auth{
		T:           TypeAuth,
		Mode:        ModeEnrol,
		Proto:       ProtoVersion,
		NodeVersion: o.Version,
		Name:        o.Name,
		SwarmNodeID: o.SwarmNodeID,
		Aud:         o.Aud,
		Nonce:       nonce,
		TS:          o.Now.Unix(),
		Pub:         pub,
		Token:       o.Token,
		EKM:         o.EKM,
	}
	a.Sig = authn.Sign(o.Key, authn.IngressTranscript(ModeEnrol, ProtoVersion, ch.Nonce, nonce, o.Aud, pub, o.EKM))
	return a, nil
}

// DecodeBundle parses one bundle message and returns the bundle. It refuses a
// message longer than MaxBundleMessageBytes (ErrBundleTooLarge) before parsing,
// anything that is not a bundle envelope (ErrUnknownMessage), an envelope whose
// instance ID is not the pinned one (ErrInstanceMismatch; an empty pin matches
// nothing, so a node never applies without a pin), and a bundle whose Verify
// fails (the *ingressbundle.VerifyError, wrapped).
//
// A node applies whatever the authenticated, pinned server says is current, even
// a bundle of a lower version than the one it runs, so a restore works.
// Ordering by version is not the node's job.
func DecodeBundle(data []byte, pinned string) (ingressbundle.Bundle, error) {
	if len(data) > MaxBundleMessageBytes {
		return ingressbundle.Bundle{}, ErrBundleTooLarge
	}
	var env BundleEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return ingressbundle.Bundle{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if env.T != TypeBundle {
		return ingressbundle.Bundle{}, ErrUnknownMessage
	}
	if pinned == "" || env.InstanceID != pinned {
		return ingressbundle.Bundle{}, ErrInstanceMismatch
	}
	if err := env.Bundle.Verify(); err != nil {
		return ingressbundle.Bundle{}, fmt.Errorf("ingresswire: %w", err)
	}
	return env.Bundle, nil
}

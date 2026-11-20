// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package channel is the ingress node's half of the channel to TierPlan: it dials
// the server over wss, proves the node's identity with the join token (the first
// connection) or the node key (every later one), checks that the server is the
// instance this node pinned, and says hello. Everything protocol-shaped (the
// auth message, its signature, the welcome check, the read limit) comes from
// proto/ingresswire, which the server's own test node uses too, so the two sides
// cannot disagree on a byte; nothing here builds a transcript or a message of
// its own.
//
// Dial makes one attempt. The bundle loop and the reconnect policy, which read
// RefusedError's hints and decide which refusals end the retrying, are the
// supervisor's (D-19, D-35).
package channel

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/ianglennon/tierplan/proto/authn"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// maxRefuseDetail bounds the server's free text in a RefusedError, so a log line
// built from it stays short.
const maxRefuseDetail = 256

// Credentials is who the node is. A node with no NodeID has not enrolled yet and
// dials in enrol mode, the only mode that reads the join token (D-22).
type Credentials struct {
	// Key is the node's Ed25519 identity key.
	Key ed25519.PrivateKey
	// NodeID is the ID TierPlan gave at enrolment, empty before it.
	NodeID string
	// InstanceID is the TierPlan instance the node pinned, empty before the
	// enrolment.
	InstanceID string
	// SwarmNodeID is the swarm node ID the node runs on, empty on a standalone
	// node. It is sent at enrolment only.
	SwarmNodeID string
	// Name is the name the node asks for at enrolment.
	Name string
	// Token returns the join token. It is called in enrol mode only.
	Token func() (string, error)
}

// Options is how Dial connects.
type Options struct {
	// URL is the server's ingress endpoint, wss:// only.
	URL string
	// InstallPins are the configured certificate pins (TIERPLAN_INGRESS_PIN).
	InstallPins []string
	// Version is the node's version string.
	Version string
	// AppliedID returns the ID of the bundle the node runs now, or "". Nil means
	// none.
	AppliedID func() string
	// OnEnrolled is called with the welcome of an enrolment, before hello, so the
	// identity is saved before the node says anything else (D-38). An error ends
	// the attempt without a hello. It is not called in connect mode.
	OnEnrolled func(ingresswire.Welcome) error
	// Now is the clock stamped into the auth message. Nil means time.Now.
	Now func() time.Time
	// HTTPClient replaces the client Dial builds. Nil means the supervisor's own
	// TLS rules for the URL's host.
	HTTPClient *http.Client
	// Log receives the connection's debug lines. Nil discards them.
	Log *slog.Logger
}

// RefusedError is the server's refusal of a handshake. The reconnect loop applies
// RetryAfter as a floor on its delay and ends the retrying on a terminal Code.
type RefusedError struct {
	// Code is the refusal code (ingresswire.Refuse*).
	Code string
	// Detail is the server's short explanation, cut to a few hundred bytes.
	Detail string
	// RetryAfter is the server's retryAfterMs hint, zero when it gave none.
	RetryAfter time.Duration
}

func (e *RefusedError) Error() string {
	if e.Detail == "" {
		return "ingress channel: refused by the server: " + e.Code
	}
	return "ingress channel: refused by the server: " + e.Code + ": " + e.Detail
}

// Conn is a connection past the handshake: welcomed, the pin checked, hello sent.
type Conn struct {
	ws         *websocket.Conn
	welcome    ingresswire.Welcome
	instanceID string
}

// Welcome is the server's welcome.
func (c *Conn) Welcome() ingresswire.Welcome { return c.welcome }

// InstanceID is the instance ID the node is pinned to: the one it already held,
// or at an enrolment the one the welcome gave, for the caller to persist.
func (c *Conn) InstanceID() string { return c.instanceID }

// WS is the underlying WebSocket, for the bundle loop's reads and writes. Its
// read limit is already MaxBundleMessageBytes.
func (c *Conn) WS() *websocket.Conn { return c.ws }

// Close ends the connection with a normal closure and reason, a short code.
func (c *Conn) Close(reason string) error {
	return c.ws.Close(websocket.StatusNormalClosure, cutUTF8(reason, 120))
}

// Dial makes one connection attempt: wss, challenge, auth, welcome, the instance
// pin, and hello. It returns the connection, a *RefusedError for the server's
// refusal, a *UntrustedError for a certificate the supervisor does not trust,
// ingresswire.ErrInstanceMismatch (wrapped) for a TierPlan that is not the pinned
// one, or another error. ctx bounds the handshake only; the caller bounds each
// later read itself.
//
// No error and no log line holds the join token or key bytes: every error is
// built from a stage and a code, never from the auth message (D-23).
func Dial(ctx context.Context, o Options, cr Credentials) (*Conn, error) {
	u, err := url.Parse(o.URL)
	if err != nil || !strings.EqualFold(u.Scheme, "wss") {
		return nil, errors.New("ingress channel: the server address must be a wss:// URL")
	}
	aud, err := authn.Audience(o.URL)
	if err != nil {
		return nil, errors.New("ingress channel: the server address has no host")
	}
	enrol := cr.NodeID == ""
	if enrol && cr.Token == nil {
		return nil, errors.New("ingress channel: no join token source for an enrolment")
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	logger := o.Log
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	client := o.HTTPClient
	if client == nil {
		client = newHTTPClient(nil, o.InstallPins, nil, u.Hostname())
	}

	ws, resp, err := websocket.Dial(ctx, o.URL, &websocket.DialOptions{ //nolint:bodyclose // a WebSocket upgrade response has no body to close
		HTTPClient:   client,
		Subprotocols: []string{ingresswire.Subprotocol},
	})
	if err != nil {
		if untrusted := asUntrusted(err); untrusted != nil {
			return nil, untrusted
		}
		return nil, fmt.Errorf("ingress channel: dial: %w", err)
	}
	fail := func(err error) (*Conn, error) {
		_ = ws.CloseNow()
		return nil, err
	}

	_, data, err := ws.Read(ctx)
	if err != nil {
		return fail(fmt.Errorf("ingress channel: read the challenge: %w", err))
	}
	msg, err := ingresswire.ParseServerMessage(data)
	if err != nil {
		return fail(fmt.Errorf("ingress channel: the first message: %w", err))
	}
	ch, ok := msg.(ingresswire.Challenge)
	if !ok {
		return fail(errors.New("ingress channel: the server's first message is not a challenge"))
	}

	// The exporter of this node's own TLS session, bound into the signature.
	ekm := authn.EKMFor(resp.TLS, authn.IngressEKMLabel)

	var auth ingresswire.Auth
	if enrol {
		token, err := cr.Token()
		if err != nil {
			return fail(fmt.Errorf("ingress channel: the join token is not available: %w", err))
		}
		auth, err = ingresswire.EnrolAuth(ch, ingresswire.EnrolOptions{
			Token: token, Name: cr.Name, SwarmNodeID: cr.SwarmNodeID, Version: o.Version,
			Aud: aud, Key: cr.Key, EKM: ekm, Now: now(),
		})
		if err != nil {
			return fail(fmt.Errorf("ingress channel: build the enrol message: %w", err))
		}
	} else {
		auth, err = ingresswire.ConnectAuth(ch, ingresswire.ConnectOptions{
			NodeID: cr.NodeID, Version: o.Version, Aud: aud, Key: cr.Key, EKM: ekm, Now: now(),
		})
		if err != nil {
			return fail(fmt.Errorf("ingress channel: build the connect message: %w", err))
		}
	}
	payload, err := json.Marshal(auth)
	if err != nil {
		return fail(errors.New("ingress channel: encode the auth message"))
	}
	if err := ws.Write(ctx, websocket.MessageText, payload); err != nil {
		return fail(fmt.Errorf("ingress channel: send the auth message: %w", err))
	}

	_, data, err = ws.Read(ctx)
	if err != nil {
		return fail(fmt.Errorf("ingress channel: read the answer: %w", err))
	}
	msg, err = ingresswire.ParseServerMessage(data)
	if err != nil {
		return fail(fmt.Errorf("ingress channel: the answer: %w", err))
	}
	var welcome ingresswire.Welcome
	switch m := msg.(type) {
	case ingresswire.Refuse:
		return fail(&RefusedError{
			Code:       m.Code,
			Detail:     cutUTF8(m.Detail, maxRefuseDetail),
			RetryAfter: time.Duration(max(m.RetryAfterMs, 0)) * time.Millisecond,
		})
	case ingresswire.Welcome:
		welcome = m
	default:
		return fail(errors.New("ingress channel: the answer is neither a welcome nor a refusal"))
	}

	pinned, err := ingresswire.CheckWelcome(welcome, cr.InstanceID)
	if err != nil {
		return fail(fmt.Errorf("ingress channel: the welcome: %w", err))
	}
	if enrol {
		if welcome.NodeID == "" {
			return fail(errors.New("ingress channel: the welcome has no node ID"))
		}
		// The identity is saved before hello: a node that lost its enrolment on a
		// crash between the two would have used its single-use token for nothing
		// (D-38).
		if o.OnEnrolled != nil {
			if err := o.OnEnrolled(welcome); err != nil {
				return fail(fmt.Errorf("ingress channel: save the enrolment: %w", err))
			}
		}
	}

	ingresswire.PrepareNodeConn(ws)
	var applied string
	if o.AppliedID != nil {
		applied = o.AppliedID()
	}
	hello, err := json.Marshal(ingresswire.Hello{
		T: ingresswire.TypeHello, Version: o.Version,
		Schemas: ingressbundle.SupportedSchemas(), AppliedID: applied,
	})
	if err != nil {
		return fail(errors.New("ingress channel: encode hello"))
	}
	if err := ws.Write(ctx, websocket.MessageText, hello); err != nil {
		return fail(fmt.Errorf("ingress channel: send hello: %w", err))
	}
	logger.Debug("ingress channel connected", "node_id", welcome.NodeID, "enrol", enrol)
	return &Conn{ws: ws, welcome: welcome, instanceID: pinned}, nil
}

// NodeName is the name the node asks for at enrolment: the host's name, else the
// node ID, cut to ingresswire.MaxNameLen bytes (D-38).
func NodeName(hostname func() (string, error), nodeID string) string {
	name := nodeID
	if hostname != nil {
		if h, err := hostname(); err == nil && h != "" {
			name = h
		}
	}
	return cutUTF8(name, ingresswire.MaxNameLen)
}

// cutUTF8 cuts s to at most n bytes without splitting a character.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresstest

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/proto/authn"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/ingresswire"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// testVersion is the supervisor version the test supervisor announces.
const testVersion = "0.1.0"

// newID is a fresh UUIDv7 for a row a test inserts by hand.
func newID(t testing.TB) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("ingresstest: make an ID: %v", err)
	}
	return id.String()
}

// Identity is what an ingress node holds: its key and, once it has been welcomed, the IDs the server gave and
// the instance ID it pinned. The zero IDs mean the node has not enrolled yet.
type Identity struct {
	Key                       ed25519.PrivateKey
	NodeID, EnvID, InstanceID string
}

// NewIdentity makes an identity with a new key. The key comes from ingresswire.NewNodeKey, the constructor
// Phase 23's supervisor uses, and from no other source (D-31). It panics on that constructor's error, which
// crypto/rand does not return on supported platforms, since it takes no testing.TB.
func NewIdentity() *Identity {
	key, err := ingresswire.NewNodeKey()
	if err != nil {
		panic(err)
	}
	return &Identity{Key: key}
}

// Public is the public key of the identity's private key.
func (i *Identity) Public() ed25519.PublicKey { return ed25519.PublicKey(i.Key[ed25519.SeedSize:]) }

// Dial describes one handshake the test supervisor makes. The zero value enrols with Token when the identity
// has no node ID yet and connects when it has. Every other field is a way to make the handshake wrong.
type Dial struct {
	// Mode is ingresswire.ModeEnrol or ModeConnect. Empty follows the identity.
	Mode string
	// Token is the ingress join token to enrol with. Name and SwarmNodeID are the claims an enrolment makes.
	Token, Name, SwarmNodeID string
	// WireToken, when set, is sent and signed as the enrol token instead of Token. ingresswire.EnrolAuth
	// refuses a token that is not an ingress join token before anything is sent, so this is how a test puts
	// one on the wire (an agent join token, say) to see what the server answers.
	WireToken string
	// Aud is the audience sent and signed. Empty means the audience DialNode was given.
	Aud string
	// SignedAud, when set, is signed instead of Aud, so the signature is for another audience than the one sent.
	SignedAud string
	// Subprotocol is offered instead of ingresswire.Subprotocol. NoSubprotocol offers none.
	Subprotocol   string
	NoSubprotocol bool
	// AgentTranscript signs authn.Transcript instead of authn.IngressTranscript, which the server must refuse:
	// a signature made for the agent channel verifies nowhere here.
	AgentTranscript bool
	// EKM replaces the exporter value sent and signed. NoEKM sends and signs none.
	EKM   []byte
	NoEKM bool
	// Proto replaces the protocol version sent.
	Proto int
	// RawFirstFrame is sent as the first frame instead of the auth, and no auth is built for it. BinaryFirstFrame
	// sends the first frame, the auth or the raw one, as a binary frame.
	RawFirstFrame    []byte
	BinaryFirstFrame bool
	// Origin is sent as the Origin header, as a browser would.
	Origin string
}

// Conn is a connection after the handshake.
type Conn struct {
	WS *websocket.Conn
	// Welcome or Refuse is what the server answered; both are nil when the server closed or the upgrade failed
	// instead, and Err says how.
	Welcome *ingresswire.Welcome
	Refuse  *ingresswire.Refuse
	// Err is the error that ended the exchange: a dial that was refused, a read that found the connection
	// closed, or a welcome whose instance ID is not the one pinned.
	Err error
	// Status is the HTTP status of a refused upgrade, else zero.
	Status int

	t testing.TB
	// pinned is the instance ID checks of this connection's bundles are made against.
	pinned string
}

// DialNode runs the node side of the handshake against url (a ws:// or wss:// URL) with client, addressed to
// aud, as id. It returns after the welcome, the refusal or the failure. The auth and every check of the
// welcome and of bundles are made with the helpers of proto/ingresswire. After a welcome it updates id with
// the IDs and the instance ID the server gave.
func DialNode(t testing.TB, url string, client *http.Client, aud string, id *Identity, d Dial) *Conn {
	t.Helper()
	ctx := testsupport.Context(t)
	c := &Conn{t: t}

	opts := &websocket.DialOptions{HTTPClient: client}
	switch {
	case d.NoSubprotocol:
	case d.Subprotocol != "":
		opts.Subprotocols = []string{d.Subprotocol}
	default:
		opts.Subprotocols = []string{ingresswire.Subprotocol}
	}
	if d.Origin != "" {
		opts.HTTPHeader = http.Header{"Origin": []string{d.Origin}}
	}
	ws, resp, err := websocket.Dial(ctx, url, opts) //nolint:bodyclose // a WebSocket upgrade response has no body to close
	if err != nil {
		c.Err = err
		if resp != nil {
			c.Status = resp.StatusCode
		}
		return c
	}
	c.WS = ws
	t.Cleanup(func() { _ = ws.CloseNow() })

	_, data, err := ws.Read(ctx)
	if err != nil {
		c.Err = err
		return c
	}
	msg, err := ingresswire.ParseServerMessage(data)
	if err != nil {
		t.Fatalf("ingresstest: the first message is not a handshake message: %s: %v", data, err)
	}
	ch, ok := msg.(ingresswire.Challenge)
	if !ok {
		t.Fatalf("ingresstest: the first message is %T, not a challenge", msg)
	}

	sentAud := aud
	if d.Aud != "" {
		sentAud = d.Aud
	}
	signedAud := sentAud
	if d.SignedAud != "" {
		signedAud = d.SignedAud
	}
	// The exporter of this node's own TLS session, as a node behind no proxy computes it. Dial.EKM or NoEKM
	// replaces it, and the replacement is both sent and signed.
	ekm := authn.EKMFor(resp.TLS, authn.IngressEKMLabel)
	switch {
	case d.NoEKM:
		ekm = nil
	case d.EKM != nil:
		ekm = d.EKM
	}

	mode := d.Mode
	if mode == "" {
		mode = ingresswire.ModeEnrol
		if id.NodeID != "" {
			mode = ingresswire.ModeConnect
		}
	}
	typ, payload := websocket.MessageText, d.RawFirstFrame
	if d.BinaryFirstFrame {
		typ = websocket.MessageBinary
	}
	if payload == nil {
		var a ingresswire.Auth
		if mode == ingresswire.ModeConnect {
			a, err = ingresswire.ConnectAuth(ch, ingresswire.ConnectOptions{
				NodeID: id.NodeID, Version: testVersion, Aud: signedAud, Key: id.Key, EKM: ekm, Now: time.Now(),
			})
		} else {
			token := d.Token
			if d.WireToken != "" {
				token = ingresswire.TokenPrefix + "placeholder"
			}
			a, err = ingresswire.EnrolAuth(ch, ingresswire.EnrolOptions{
				Token: token, Name: d.Name, SwarmNodeID: d.SwarmNodeID, Version: testVersion,
				Aud: signedAud, Key: id.Key, EKM: ekm, Now: time.Now(),
			})
		}
		if err != nil {
			t.Fatalf("ingresstest: build the auth: %v", err)
		}
		a.Aud = sentAud
		if d.WireToken != "" {
			// Signed again, over the ingress transcript, so the token is the only fault in the auth.
			a.Token = d.WireToken
			a.Sig = authn.Sign(id.Key, authn.IngressTranscript(mode, ingresswire.ProtoVersion, ch.Nonce, a.Nonce, signedAud, a.Pub, ekm))
		}
		if d.Mode != "" {
			a.Mode = d.Mode
		}
		if d.Proto != 0 {
			a.Proto = d.Proto
		}
		if d.AgentTranscript {
			a.Sig = authn.Sign(id.Key, authn.Transcript(mode, ingresswire.ProtoVersion, ch.Nonce, a.Nonce, signedAud, a.Pub, ekm))
		}
		payload, err = json.Marshal(a)
		if err != nil {
			t.Fatalf("ingresstest: encode the auth: %v", err)
		}
	}
	if err := ws.Write(ctx, typ, payload); err != nil {
		// A node that answers after the server has given up on it finds the connection gone; that is an
		// outcome (Err set, no welcome), not a fault of the test.
		c.Err = err
		return c
	}

	_, data, err = ws.Read(ctx)
	if err != nil {
		c.Err = err
		return c
	}
	msg, err = ingresswire.ParseServerMessage(data)
	if err != nil {
		t.Fatalf("ingresstest: the answer is not a handshake message: %s: %v", data, err)
	}
	switch m := msg.(type) {
	case ingresswire.Welcome:
		c.Welcome = &m
		pinned, err := ingresswire.CheckWelcome(m, id.InstanceID)
		if err != nil {
			c.Err = err
			return c
		}
		id.NodeID, id.EnvID, id.InstanceID = m.NodeID, m.EnvID, pinned
		c.pinned = pinned
		ingresswire.PrepareNodeConn(ws)
	case ingresswire.Refuse:
		c.Refuse = &m
	default:
		t.Fatalf("ingresstest: unexpected answer %T", msg)
	}
	return c
}

// Enrol enrols id with d (its Token and claims) and fails the test unless the server welcomes it.
func (s *Server) Enrol(t testing.TB, id *Identity, d Dial) *Conn {
	t.Helper()
	d.Mode = ingresswire.ModeEnrol
	c := DialNode(t, s.URL(), s.Client(), s.Audience(), id, d)
	if c.Welcome == nil || c.Err != nil {
		t.Fatalf("ingresstest: enrol was not welcomed: refuse %+v, err %v", c.Refuse, c.Err)
	}
	return c
}

// Connect reconnects id with its stored key and fails the test unless the server welcomes it.
func (s *Server) Connect(t testing.TB, id *Identity, d Dial) *Conn {
	t.Helper()
	d.Mode = ingresswire.ModeConnect
	c := DialNode(t, s.URL(), s.Client(), s.Audience(), id, d)
	if c.Welcome == nil || c.Err != nil {
		t.Fatalf("ingresstest: connect was not welcomed: refuse %+v, err %v", c.Refuse, c.Err)
	}
	return c
}

// Send writes v as one JSON text message, as a node writes hello, applied, rejected and health.
func (c *Conn) Send(t testing.TB, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("ingresstest: encode a message: %v", err)
	}
	c.SendRaw(t, websocket.MessageText, b)
}

// SendRaw writes one message of the given type, for a message that is wrong on purpose.
func (c *Conn) SendRaw(t testing.TB, typ websocket.MessageType, b []byte) {
	t.Helper()
	if err := c.WS.Write(testsupport.Context(t), typ, b); err != nil {
		t.Fatalf("ingresstest: write a message: %v", err)
	}
}

// SendText writes b as one text message, whatever it holds: a message that is wrong on purpose.
func (c *Conn) SendText(t testing.TB, b []byte) {
	t.Helper()
	c.SendRaw(t, websocket.MessageText, b)
}

// SendBinary writes b as one binary message, which the node may never send.
func (c *Conn) SendBinary(t testing.TB, b []byte) {
	t.Helper()
	c.SendRaw(t, websocket.MessageBinary, b)
}

// Applied tells the server the node applied the bundle id.
func (c *Conn) Applied(t testing.TB, id string) {
	t.Helper()
	c.Send(t, ingresswire.Applied{T: ingresswire.TypeApplied, ID: id})
}

// Rejected tells the server the node did not apply the bundle id, and why.
func (c *Conn) Rejected(t testing.TB, id, reason string) {
	t.Helper()
	c.Send(t, ingresswire.Rejected{T: ingresswire.TypeRejected, ID: id, Reason: reason})
}

// Health sends the node's periodic proof of life, with whether its Traefik is answering.
func (c *Conn) Health(t testing.TB, ok bool) {
	t.Helper()
	c.Send(t, ingresswire.Health{T: ingresswire.TypeHealth, TraefikOK: ok})
}

// Hello sends the first message after the welcome: the node's version, the schemas it can apply and the ID of
// the bundle it runs.
func (c *Conn) Hello(t testing.TB, schemas []int, appliedID string) {
	t.Helper()
	c.Send(t, ingresswire.Hello{T: ingresswire.TypeHello, Version: testVersion, Schemas: schemas, AppliedID: appliedID})
}

// NextBundle reads the next message, which must be one binary bundle envelope, and decodes it with
// ingresswire.DecodeBundle against the instance ID this connection pinned.
func (c *Conn) NextBundle(t testing.TB) ingressbundle.Bundle {
	t.Helper()
	b, err := c.TryNextBundle(t)
	if err != nil {
		t.Fatalf("ingresstest: decode the bundle: %v", err)
	}
	return b
}

// TryNextBundle is NextBundle for a test that expects the decode to fail: it returns the error of
// ingresswire.DecodeBundle (an ErrInstanceMismatch, say) instead of failing the test. A read that fails, or a
// message that is not a binary frame, still fails the test.
func (c *Conn) TryNextBundle(t testing.TB) (ingressbundle.Bundle, error) {
	t.Helper()
	typ, data, err := c.WS.Read(testsupport.Context(t))
	if err != nil {
		t.Fatalf("ingresstest: read a bundle: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Fatalf("ingresstest: the next message is a text frame, not a bundle: %s", data)
	}
	return ingresswire.DecodeBundle(data, c.pinned)
}

// NextRaw reads the next message as it came off the socket, whatever it is, with no decoding.
func (c *Conn) NextRaw(t testing.TB) (websocket.MessageType, []byte) {
	t.Helper()
	typ, data, err := c.WS.Read(testsupport.Context(t))
	if err != nil {
		t.Fatalf("ingresstest: read a message: %v", err)
	}
	return typ, data
}

// AwaitClose reads until the connection ends and returns the status and reason the server closed it with. A
// message that arrives first is skipped.
func (c *Conn) AwaitClose(t testing.TB) (websocket.StatusCode, string) {
	t.Helper()
	ctx := testsupport.Context(t)
	for {
		if _, _, err := c.WS.Read(ctx); err != nil {
			var ce websocket.CloseError
			if errors.As(err, &ce) {
				return ce.Code, ce.Reason
			}
			t.Fatalf("ingresstest: the connection ended without a close frame: %v", err)
		}
	}
}

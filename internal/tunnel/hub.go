// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package tunnel is the server end of the agent connection: the /agent/connect
// WebSocket, the handshake inside it, the registry of live sessions, and the
// engine.Transport that carries Docker HTTP over one multiplexed stream per
// call (D-05, D-18, D-42).
//
// The agent dials out and the server never dials in. After the handshake the
// WebSocket carries one yamux session in which the server is the client: it
// opens a stream for every Docker call, and the agent answers each through its
// allowlisting proxy.
package tunnel

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/ianglennon/bayplan/internal/engine"
	"github.com/ianglennon/bayplan/proto/wire"
)

// ErrAgentGone means the agent's session is closed or closing. A Docker call
// that meets it ends at once instead of hanging (D-42); a safe read may be
// retried on another manager, a mutation never is.
var ErrAgentGone = errors.New("tunnel: the agent connection is gone")

// RefuseError is what a Verifier returns to turn a handshake away. Code and
// Detail go to the agent in a refuse message; AuditReason stays on the server,
// so a disabled token and a wrong token look alike from outside.
type RefuseError struct {
	Code        string
	Detail      string
	AuditReason string
}

func (e *RefuseError) Error() string {
	if e.AuditReason != "" {
		return "tunnel: handshake refused: " + e.Code + " (" + e.AuditReason + ")"
	}
	return "tunnel: handshake refused: " + e.Code
}

// EnrolRequest is a first connection that wants a certificate.
type EnrolRequest struct {
	// Token is the join token the agent presented.
	Token string
	// Pub is the Ed25519 public key the agent generated.
	Pub []byte
	// Sig is its signature over Transcript, the proof of possession.
	Sig []byte
	// Transcript is the digest the handler rebuilt from the challenge, the
	// agent's nonce, the audience and the key, never taken from the agent.
	Transcript   []byte
	AgentVersion string
	Facts        wire.Facts
	// IP is the client address for the audit entry.
	IP string
}

// EnrolResult is what a successful enrolment hands back.
type EnrolResult struct {
	AgentID, EnvID string
	// Cert is the DER certificate issued to the agent.
	Cert     []byte
	NotAfter time.Time
}

// Verifier decides whether a handshake is accepted.
type Verifier interface {
	// Enrol checks the token and the proof of possession, issues a certificate
	// and records the agent. It returns a *RefuseError to turn the agent away.
	Enrol(ctx context.Context, req EnrolRequest) (EnrolResult, error)
}

// Observer is told when a session starts and ends.
type Observer interface {
	// Connected runs once the session is registered and ready for streams.
	Connected(*Session)
	// Disconnected runs once the session has ended, with the D-40 reason code.
	Disconnected(s *Session, code string)
}

// Session is one live agent connection.
type Session struct {
	AgentID, EnvID string
	AgentVersion   string
	Facts          wire.Facts
	IP             string
	ConnectedAt    time.Time

	ym   *yamux.Session
	ws   *websocket.Conn
	done chan struct{}
	once sync.Once
}

// Transport is how the session carries Docker calls: one stream per dial.
func (s *Session) Transport() engine.Transport { return StreamTransport{s: s} }

// Done is closed when the session has ended.
func (s *Session) Done() <-chan struct{} { return s.done }

// close ends the session. The WebSocket is closed first, with a close frame, so
// the agent learns why and reconnects with backoff; the multiplexer follows.
func (s *Session) close(code websocket.StatusCode, reason string) {
	s.once.Do(func() {
		_ = s.ws.Close(code, reason)
		_ = s.ym.Close()
	})
}

// closeNow drops the connection without waiting for the peer, for a session
// whose peer has already stopped answering.
func (s *Session) closeNow() {
	s.once.Do(func() {
		_ = s.ws.CloseNow()
		_ = s.ym.Close()
	})
}

// Hub holds the live sessions by agent ID and serves /agent/connect.
type Hub struct {
	v   Verifier
	o   Observer
	log *slog.Logger
	now func() time.Time

	publicURL func() string

	mu       sync.Mutex
	sessions map[string]*Session
	closed   bool
}

// NewHub returns a Hub. v is required. o may be nil. now defaults to time.Now.
func NewHub(v Verifier, o Observer, log *slog.Logger, now func() time.Time) *Hub {
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &Hub{v: v, o: o, log: log, now: now, sessions: map[string]*Session{}}
}

// WithPublicURL tells the hub where Bayplan is reached from outside. The
// audience of every handshake is checked against its host, and plain transport
// is accepted only when the public URL itself is plain http. It is read on every
// connection, so a change applies at once. Without it every handshake is refused.
func (h *Hub) WithPublicURL(f func() string) *Hub {
	h.publicURL = f
	return h
}

// Handler serves GET /agent/connect.
func (h *Hub) Handler() http.Handler { return http.HandlerFunc(h.serve) }

// Session returns the live session of agentID, or nil.
func (h *Hub) Session(agentID string) *Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[agentID]
}

// Sessions returns the live sessions, ordered by agent ID.
func (h *Hub) Sessions() []*Session {
	h.mu.Lock()
	out := make([]*Session, 0, len(h.sessions))
	for _, s := range h.sessions {
		out = append(out, s)
	}
	h.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

// Close ends every session with a close frame, so agents reconnect with
// jittered backoff, and refuses new connections. http.Server.Shutdown ignores
// hijacked connections, so the server registers this with RegisterOnShutdown
// (Pitfall 5). It is safe to call more than once.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	live := make([]*Session, 0, len(h.sessions))
	for _, s := range h.sessions {
		live = append(live, s)
	}
	h.mu.Unlock()

	var wg sync.WaitGroup
	for _, s := range live {
		wg.Go(func() { s.close(websocket.StatusGoingAway, "the server is stopping") })
	}
	wg.Wait()
}

func (h *Hub) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// register adds s, replacing any session of the same agent: a half-open stale
// connection must not block the reconnect. It reports false when the hub is
// closed.
func (h *Hub) register(s *Session) bool {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return false
	}
	old := h.sessions[s.AgentID]
	h.sessions[s.AgentID] = s
	h.mu.Unlock()
	if old != nil {
		old.closeNow()
	}
	return true
}

// unregister removes s if it is still the current session of its agent.
func (h *Hub) unregister(s *Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessions[s.AgentID] == s {
		delete(h.sessions, s.AgentID)
	}
}

// muxConfig is the yamux configuration of RESEARCH Pattern 1. Keepalives every
// 10 s with a 10 s write timeout detect a silent black hole in at most about
// 20 s, inside the D-41 window of 30 s. StreamCloseTimeout is cut from the
// default five minutes, which keeps half-closed streams alive far too long.
func muxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.KeepAliveInterval = 10 * time.Second
	c.ConnectionWriteTimeout = 10 * time.Second
	c.StreamCloseTimeout = 30 * time.Second
	c.StreamOpenTimeout = 10 * time.Second
	c.AcceptBacklog = 64
	c.LogOutput = discard{}
	return c
}

// discard drops yamux's own logging; the hub logs through slog.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

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
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
	"golang.org/x/time/rate"

	"github.com/ianglennon/bayplan/internal/auth"
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
	// ClockOffset is how far the agent's clock is ahead of the server's, measured
	// in the handshake. The Verifier refuses it with CheckClock, after it has
	// checked the token and before it issues a certificate.
	ClockOffset time.Duration
}

// EnrolResult is what a successful enrolment hands back.
type EnrolResult struct {
	AgentID, EnvID string
	// Cert is the DER certificate issued to the agent.
	Cert     []byte
	NotAfter time.Time
}

// Refusal is what a Verifier returns to turn a handshake away; the handler
// answers refuse with its Code and Detail. It is the same type as RefuseError,
// under the name the hello path uses.
type Refusal = RefuseError

// HelloRequest is a reconnect of an enrolled agent: the certificate it was issued
// and a signature made with the key behind it.
type HelloRequest struct {
	// AgentID is the ID the agent claims. The certificate's CN is what counts.
	AgentID string
	// Cert is the DER certificate the agent presented.
	Cert []byte
	// Sig is the agent's signature over Transcript.
	Sig []byte
	// Transcript is the digest the handler rebuilt from the challenge, the
	// agent's nonce, the audience and the certificate, never taken from the agent.
	Transcript   []byte
	AgentVersion string
	Facts        wire.Facts
	// IP is the client address for the audit trail.
	IP string
	// ClockOffset is how far the agent's clock is ahead of the server's. The
	// Verifier refuses it with CheckClock once the signature has checked out.
	ClockOffset time.Duration
}

// HelloResult is what a successful hello hands back.
type HelloResult struct {
	AgentID, EnvID string
	// NotAfter is when the certificate presented expires.
	NotAfter time.Time
}

// Verifier decides whether a handshake is accepted.
type Verifier interface {
	// Enrol checks the token and the proof of possession, issues a certificate
	// and records the agent. It returns a *Refusal to turn the agent away.
	Enrol(ctx context.Context, req EnrolRequest) (EnrolResult, error)
	// Hello checks that the certificate chains to the environment it names, that
	// the agent it names is active and holds the same key, and that the signature
	// is made by that key. It returns a *Refusal to turn the agent away.
	Hello(ctx context.Context, req HelloRequest) (HelloResult, error)
	// StillActive reports whether the agent may still hold a session: it exists and
	// has not been revoked. The hub asks it again at the moment it registers a
	// session, under its lifecycle lock, so a revoke committed after Hello looked
	// and before the registration is still honoured. It answers false when it
	// cannot tell, so a failure closes a session the agent can simply reopen.
	StillActive(agentID string) bool
	// Renew issues a new certificate for the same key to the agent whose session
	// asked for one (D-04). ip is the session's client address, for the audit entry.
	// It returns a *Refusal when it will not: revoked, or not yet due.
	Renew(ctx context.Context, agentID, ip string) (RenewResult, error)
}

// RenewResult is what a successful renewal hands back.
type RenewResult struct {
	// Cert is the DER certificate issued to the agent.
	Cert []byte
	// NotAfter is when it expires.
	NotAfter time.Time
}

// Why a session ended, the code Observer.Disconnected receives. The first reason
// set for a session wins.
const (
	// DisconnectClosed means the connection ended: the agent went away or the
	// network dropped.
	DisconnectClosed = "closed"
	// DisconnectTimeout means no heartbeat arrived within the offline window.
	DisconnectTimeout = "timeout"
	// DisconnectReplaced means the same agent connected again and the new session
	// took its place.
	DisconnectReplaced = "replaced"
	// DisconnectShutdown means the server is stopping.
	DisconnectShutdown = "shutdown"
	// DisconnectRevoked means an administrator revoked the agent.
	DisconnectRevoked = "revoked"
	// DisconnectRemoved means the agent's environment was removed.
	DisconnectRemoved = "removed"
)

// Observer is told when a session starts and ends.
type Observer interface {
	// Connected runs once the session is registered and ready for streams.
	Connected(*Session)
	// Disconnected runs once the session has ended, with the D-40 reason code.
	Disconnected(s *Session, code string)
}

// Notifier is told what happens to the agents of every environment, so the health
// stream can publish it (D-36). Unlike an Observer it is only told, never relied
// on: it must return at once, and it may not call back into the hub.
type Notifier interface {
	// AgentConnected runs once a session is registered and ready.
	AgentConnected(*Session)
	// AgentDisconnected runs once a session has ended, with the D-40 reason code.
	AgentDisconnected(s *Session, code string)
	// AgentRefused runs when a handshake was turned away. envID is the environment
	// the attempt named in its join token or certificate, which the attempt chose
	// itself: it is a place to route the event to, never evidence of anything.
	AgentRefused(envID, code, mode, node, ip string)
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

	// streams holds one token per server-opened stream in flight, and maxResponse
	// is the cap on one Docker answer that is not the events feed.
	streams     chan struct{}
	maxResponse int64

	// denyLimit spends one token per audited policy denial, and suppressed counts
	// the denials since the last audited one that were not written (D-29).
	denyLimit  *rate.Limiter
	suppressed atomic.Int64

	// control is set once the agent's control stream has been accepted. ctl is that
	// stream, which the server writes its own messages to (a renewed certificate, new
	// addresses); ctlMu serializes those writes, because lines must not interleave.
	control atomic.Bool
	ctlMu   sync.Mutex
	ctl     *yamux.Stream
	// welcomeURLs is the address list the welcome carried. When the control stream
	// opens and the current list differs, the agent is told at once: the address
	// changed in the moments between its welcome and its control stream.
	welcomeURLs []string
	// renewing is set while a renewal for this session is being made, so a flood of
	// renew lines is one renewal and not a queue of them.
	renewing atomic.Bool
	hbMu     sync.Mutex
	status   SessionStatus

	// order is the place of this connection among those the hub accepted: a later
	// accept has a higher number. Of two handshakes of one agent that overlap, the
	// one with the higher number is the one the agent is on.
	order uint64

	reasonMu sync.Mutex
	reason   string
	// reported makes the Observer's Disconnected run once for this session, from
	// whichever path ends it first.
	reported sync.Once
}

// endWith records why the session ended. The first reason wins, so a session
// that was replaced and then saw its socket close is still reported as replaced.
func (s *Session) endWith(code string) {
	s.reasonMu.Lock()
	if s.reason == "" {
		s.reason = code
	}
	s.reasonMu.Unlock()
}

// Reason is why the session ended, one of the Disconnect codes, or DisconnectClosed
// when nothing more specific was recorded.
func (s *Session) Reason() string {
	s.reasonMu.Lock()
	defer s.reasonMu.Unlock()
	if s.reason == "" {
		return DisconnectClosed
	}
	return s.reason
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

// kickGrace is how long a kicked session waits for the agent to answer the close
// frame before the connection is dropped.
const kickGrace = time.Second

// kick ends the session with policy-violation status and reason as the close
// reason, then drops it if the agent has not answered within kickGrace. A session
// that is already closing is left alone.
func (s *Session) kick(reason string) {
	s.once.Do(func() {
		done := make(chan struct{})
		go func() {
			_ = s.ws.Close(websocket.StatusPolicyViolation, reason)
			close(done)
		}()
		t := time.NewTimer(kickGrace)
		defer t.Stop()
		select {
		case <-done:
		case <-t.C:
			_ = s.ws.CloseNow()
		}
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
	v        Verifier
	o        Observer
	notifier Notifier
	log      *slog.Logger
	now      func() time.Time

	publicURL func() string
	// endpoint is the address the welcome tells agents to dial (D-19).
	endpoint func() string
	// pins is what the welcome tells agents are the server's pins (D-06).
	pins func() []string

	// heartbeat is the interval agents are told to use, offlineAfter how long a
	// session may be silent, watchEvery how often silence is looked for (zero
	// means a sixth of offlineAfter), and lastSeen where heartbeats are recorded.
	// All are set before the hub serves and read-only after.
	heartbeat    time.Duration
	offlineAfter time.Duration
	watchEvery   time.Duration
	lastSeen     LastSeenRecorder

	// allowInsecure lets plain ws:// in. connect is the per-address budget of
	// connection attempts and handshakes the semaphore of handshakes in flight.
	// refusals records refused attempts, onUpgradeFailure hears of requests that
	// were not an upgrade and maxResponse caps one Docker answer. All are set
	// before the hub serves and read-only after.
	allowInsecure    bool
	connect          *auth.IPLimiter
	handshakes       chan struct{}
	refusals         auth.AuditRecorder
	audit            auth.AuditRecorder
	onUpgradeFailure func(peer string)
	maxResponse      int64

	// serverVersion is what Compatible measures agents against, and updateHint
	// the manual update command added to a refusal for a version outside the
	// window.
	serverVersion string
	updateHint    func(tooOld bool, envType string) string

	// life serializes session lifecycle transitions together with the Observer
	// calls they cause, so for one agent the Observer hears Connected, then
	// Disconnected, then Connected again, in that order, never a Disconnected
	// before the Connected of the same session. An Observer must not call back
	// into anything that takes it. mu guards the map and is never held across an
	// Observer call.
	life sync.Mutex

	mu       sync.Mutex
	sessions map[string]*Session
	// memory is what is kept of every agent that has connected, for the health
	// rollup (Views).
	memory map[string]*agentMemory
	closed bool
	// accepted numbers the connections in the order they were accepted.
	accepted atomic.Uint64
}

// NewHub returns a Hub. v is required. o may be nil. now defaults to time.Now.
func NewHub(v Verifier, o Observer, log *slog.Logger, now func() time.Time) *Hub {
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &Hub{
		v: v, o: o, log: log, now: now, sessions: map[string]*Session{}, memory: map[string]*agentMemory{},
		heartbeat: wire.DefaultHeartbeatMs * time.Millisecond, offlineAfter: DefaultOfflineAfter,
		connect:     auth.NewIPLimiter(connectRefill, DefaultConnectBurst, connectLimiterTTL, now),
		handshakes:  make(chan struct{}, maxHandshakes),
		maxResponse: MaxResponseBytes,
	}
}

// WithNotifier sets who is told of connections, disconnections and refused
// handshakes. Call it before the hub serves.
func (h *Hub) WithNotifier(n Notifier) *Hub {
	h.notifier = n
	return h
}

// WithPublicURL tells the hub the URL agents are told to dial: Bayplan's public
// URL, or the agent URL when an admin set one (D-18). The audience of every
// handshake is checked against its host, and plain transport is accepted only
// when that URL itself is plain http. It is read on every
// connection, so a change applies at once. Without it every handshake is refused.
func (h *Hub) WithPublicURL(f func() string) *Hub {
	h.publicURL = f
	return h
}

// WithPins sets the server's certificate pins, "sha256/" and the base64 SPKI
// hash, that every welcome carries so an agent can keep them for the next
// connection. It is read on every handshake. Without it the welcome has none.
// Call it before the hub serves.
func (h *Hub) WithPins(f func() []string) *Hub {
	h.pins = f
	return h
}

// Pins is what a welcome sent now would carry.
func (h *Hub) Pins() []string {
	if h.pins == nil {
		return nil
	}
	return h.pins()
}

// WithEndpoint tells the hub the WebSocket address agents are told to dial
// (wss://host/base/agent/connect), which every welcome carries and every
// PushURLs announces. It is read on every handshake, so a change applies at once.
// Without it welcomes carry no address. Call it before the hub serves.
func (h *Hub) WithEndpoint(f func() string) *Hub {
	h.endpoint = f
	return h
}

// URLs is the address list a welcome sent now would carry: the endpoint, or none
// when it is not set.
func (h *Hub) URLs() []string {
	if h.endpoint == nil {
		return nil
	}
	if u := h.endpoint(); u != "" {
		return []string{u}
	}
	return nil
}

// PushURLs tells every connected agent where to dial from now on and which server
// pins are current (D-19, D-06): urls with the address to dial first at the front.
// It is best effort and returns at once with how many sessions it is writing to; an
// agent whose control stream cannot be written to is logged and left, and learns
// the address from its next welcome. A session that has not opened its control
// stream yet is told when it does (see agentStream).
func (h *Hub) PushURLs(urls, pins []string) int {
	if len(urls) == 0 {
		return 0
	}
	h.mu.Lock()
	live := make([]*Session, 0, len(h.sessions))
	for _, s := range h.sessions {
		if s.control.Load() {
			live = append(live, s)
		}
	}
	h.mu.Unlock()
	msg := wire.URLUpdate{T: wire.TypeURLs, URLs: urls, Pins: pins}
	for _, s := range live {
		go func() {
			// A control stream that is not set up yet is told when it is (agentStream).
			if err := s.sendControl(msg); err != nil && !errors.Is(err, errNoControlStream) {
				h.log.Warn("could not tell an agent the new server address; it learns it from its next welcome", "agent_id", s.AgentID, "environment_id", s.EnvID, "error", err)
			}
		}()
	}
	return len(live)
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

// Views returns what the hub knows about the agents of envID that have connected
// since the server started, ordered by agent ID.
func (h *Hub) Views(envID string) []AgentLive {
	type live struct {
		id string
		m  agentMemory
	}
	h.mu.Lock()
	var found []live
	for id, m := range h.memory {
		if m.envID == envID {
			found = append(found, live{id: id, m: *m})
		}
	}
	h.mu.Unlock()
	sort.Slice(found, func(i, j int) bool { return found[i].id < found[j].id })

	out := make([]AgentLive, 0, len(found))
	for _, f := range found {
		v := AgentLive{
			AgentID: f.id, EnvID: envID, Version: f.m.version, ConnectedAt: f.m.connectedAt, LastSeen: f.m.lastSeen,
			Ended: f.m.ended, LastCode: f.m.lastCode, EngineReachable: f.m.engineReachable,
			ClockOffset: f.m.clockOffset, ClockWarning: f.m.clockWarning,
		}
		if s := f.m.sess; s != nil {
			// Read outside h.mu: a session's status has its own lock.
			st := s.Status()
			v.Connected, v.LastSeen = true, s.lastSignOfLife()
			v.EngineReachable = st.LastHeartbeat.IsZero() || st.EngineReachable
			v.ClockOffset, v.ClockWarning = st.ClockOffset, st.ClockWarning
		}
		out = append(out, v)
	}
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
		s.endWith(DisconnectShutdown)
		wg.Go(func() { s.close(websocket.StatusGoingAway, "the server is stopping") })
	}
	wg.Wait()
}

func (h *Hub) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// errHubClosed and errNoLongerActive are why register turned a session away.
var (
	errHubClosed      = errors.New("tunnel: the hub is closed")
	errNoLongerActive = errors.New("tunnel: the agent was revoked while it connected")
	errSuperseded     = errors.New("tunnel: a newer connection of the agent has registered")
)

// register adds s, replacing any session of the same agent: a half-open stale
// connection must not block the reconnect. The old session is closed and
// reported Disconnected with the reason replaced before the new one is reported
// Connected, all inside one critical section, so two simultaneous hellos for one
// agent can never both remain. It reports errHubClosed when the hub is closed.
//
// A session whose connection was accepted before that of the agent's newest
// session is turned away with errSuperseded instead: two handshakes of one agent
// can overlap, the agent has moved on to the newer connection and dropped the
// older, and an older handshake that finishes last must not replace the live
// session with one nobody answers on.
//
// The agent's status is read again here, after the lifecycle lock is taken. A
// revoke commits first and then calls Kick, and Kick takes the same lock, so
// either the status read below sees the revoke, or the session is already in the
// map when Kick looks and is closed by it. There is no order in which a session
// of a revoked agent stays registered (T-02-55).
func (h *Hub) register(s *Session) error {
	h.life.Lock()
	defer h.life.Unlock()

	if !h.v.StillActive(s.AgentID) {
		return errNoLongerActive
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return errHubClosed
	}
	if m := h.memory[s.AgentID]; m != nil && s.order < m.order {
		h.mu.Unlock()
		return errSuperseded
	}
	old := h.sessions[s.AgentID]
	h.sessions[s.AgentID] = s
	h.noteConnected(s)
	h.mu.Unlock()

	if old != nil {
		old.endWith(DisconnectReplaced)
		old.closeNow()
		h.report(old)
	}
	if h.o != nil {
		h.o.Connected(s)
	}
	if h.notifier != nil {
		h.notifier.AgentConnected(s)
	}
	return nil
}

// Kick ends the live session of agentID, if it has one, with a close frame that
// carries code as its reason, and records code as why it ended. It returns at
// once; the session is closed within a second even if the agent never answers
// the close frame. The agents service calls it after a revoke has committed.
//
// Kick takes the lifecycle lock briefly, which makes it wait for a registration
// in progress: a session that registered before the revoke committed is in the
// map by the time Kick looks (see register).
func (h *Hub) Kick(agentID, code string) {
	h.life.Lock()
	h.mu.Lock()
	s := h.sessions[agentID]
	h.mu.Unlock()
	h.life.Unlock()
	if s != nil {
		s.endWith(code)
		go s.kick(code)
	}
}

// KickEnv is Kick for every live session of the agents of envID.
func (h *Hub) KickEnv(envID, code string) {
	h.life.Lock()
	h.mu.Lock()
	var live []*Session
	for _, s := range h.sessions {
		if s.EnvID == envID {
			live = append(live, s)
		}
	}
	h.mu.Unlock()
	h.life.Unlock()
	for _, s := range live {
		s.endWith(code)
		go s.kick(code)
	}
}

// finish removes s if it is still the current session of its agent and reports
// it Disconnected, unless a replacement already did.
func (h *Hub) finish(s *Session) {
	h.life.Lock()
	defer h.life.Unlock()
	h.mu.Lock()
	if h.sessions[s.AgentID] == s {
		delete(h.sessions, s.AgentID)
	}
	h.mu.Unlock()
	h.report(s)
}

// report tells the Observer, once, that s has ended.
func (h *Hub) report(s *Session) {
	s.reported.Do(func() {
		h.noteEnded(s)
		if h.o != nil {
			h.o.Disconnected(s, s.Reason())
		}
		if h.notifier != nil {
			h.notifier.AgentDisconnected(s, s.Reason())
		}
	})
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

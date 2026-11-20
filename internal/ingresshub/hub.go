// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/ianglennon/tierplan/internal/auth"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/proto/authn"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// DefaultOfflineAfter is how long a session may be silent before the hub closes it: three health intervals,
// so two lost messages in a row do not read as a dead node (D-12).
const DefaultOfflineAfter = 3 * ingresswire.HealthInterval

// BundleSource is where the hub gets the bundle of an environment: its current one, and the notifier that is
// told when an environment's bundle ID changes. ingressrecon.Reconciler satisfies it. The files of a bundle
// are shared with the source and are never modified here.
type BundleSource interface {
	Current(envID string) (ingressbundle.Bundle, bool)
	Changes() *ingress.Notifier
}

// Hub holds the live ingress node sessions by node ID and serves /ingress/connect.
type Hub struct {
	svc *Service
	src BundleSource
	log *slog.Logger
	now func() time.Time

	// publicURL is TierPlan's public URL, whose host is the audience of every handshake. pins is what a
	// welcome tells nodes are the server's pins. instanceID reads the installation's ID for the welcome.
	// All are set before the hub serves and read-only after.
	publicURL  func() string
	pins       func() []string
	instanceID func(context.Context) (string, error)

	serverVersion string
	// refusals records refused handshakes. It must be an auth.AuditThrottle in production: the actor is
	// anonymous, so without a per-address budget one caller could fill the audit log and starve the single
	// database writer.
	refusals auth.AuditRecorder
	// gate admits handshakes; it is the hub's own, shared with nothing (D-10).
	gate *gate
	// warnLimit lets one log line per address per minute through, for what is logged before a request is
	// admitted.
	warnLimit *auth.IPLimiter
	// firstFrame is how long a node has to answer the challenge.
	firstFrame time.Duration
	// offlineAfter is how long a session may be silent, and watchTicks, when set, replaces the watchdog's
	// ticker so a test decides when silence is looked for.
	offlineAfter time.Duration
	watchTicks   <-chan time.Time
	// onUpgradeFailure, when set, hears the client address of every TLS request that is not a WebSocket
	// upgrade (OnUpgradeFailure).
	onUpgradeFailure func(peer string)
	// beforeRegister, when set, is called with the node ID after a handshake has been verified and welcomed
	// and before the session is registered (WithBeforeRegister).
	beforeRegister func(nodeID string)

	// life serializes session lifecycle transitions: a registration and a kick take it, so a revoke that
	// commits and then kicks either finds the session registered or is seen by the registration (register).
	life sync.Mutex

	mu       sync.Mutex
	sessions map[string]*Session
	// newest is the highest accept order that has registered for each node. A session whose connection was
	// accepted earlier is superseded and is turned away. It holds one number per node that ever connected, so
	// it is bounded by the number of nodes.
	newest map[string]uint64
	closed bool
	// regChanged is closed and cleared whenever a session is registered or removed, to wake AwaitSession
	// waiters; a waiter makes it. Guarded by mu.
	regChanged chan struct{}
	// accepted numbers the connections in the order they were accepted.
	accepted atomic.Uint64

	// sub is the subscription to the source's Changes, taken in NewHub so a change that lands before Run starts
	// is not lost. stop is closed by Close and ends Run.
	sub      *ingress.Subscription
	stop     chan struct{}
	stopOnce sync.Once
	// envelopes holds the marshalled bundle message of each environment, shared by its sessions.
	envelopes envelopeCache
	// onPush, when set, is told the outcome of every look at the current bundle (WithPushObserver).
	onPush func(PushEvent)
}

// NewHub returns a Hub. svc is required, src may be nil (a node then gets no bundle), and a nil log is
// slog.Default. It subscribes to the source's Changes at once, before any connection is served; Run consumes
// the subscription. A nil now is time.Now, as NewService treats it (D-36); it is made before the gate is built from
// it, so h.now is never nil and the gate's limiters and every read of the clock here share the one clock.
func NewHub(svc *Service, src BundleSource, log *slog.Logger, now func() time.Time) *Hub {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	h := &Hub{
		svc: svc, src: src, log: log, now: now,
		gate:         newGate(now),
		warnLimit:    auth.NewIPLimiter(time.Minute, 1, limiterTTL, now),
		firstFrame:   firstFrameTimeout,
		offlineAfter: DefaultOfflineAfter,
		sessions:     map[string]*Session{},
		newest:       map[string]uint64{},
		stop:         make(chan struct{}),
		envelopes:    newEnvelopeCache(),
	}
	if src != nil {
		if n := src.Changes(); n != nil {
			h.sub = n.Subscribe()
		}
	}
	return h
}

// WithPublicURL tells the hub TierPlan's public URL. The audience of every handshake is checked against its
// host, and it is read on every connection, so a change applies at once. Without it every handshake is
// refused with 503. Call it before the hub serves.
func (h *Hub) WithPublicURL(f func() string) *Hub {
	h.publicURL = f
	return h
}

// WithPins sets the server's certificate pins ("sha256/" and the base64 SPKI hash) that every welcome carries.
// Without it the welcome has none. Call it before the hub serves.
func (h *Hub) WithPins(f func() []string) *Hub {
	h.pins = f
	return h
}

// WithInstanceID sets where the welcome reads the installation's instance ID from. It is read on every
// handshake and never cached, so a restore that changes it is seen at once. Without it every handshake that
// would be welcomed is refused internal_error. Call it before the hub serves.
func (h *Hub) WithInstanceID(f func(context.Context) (string, error)) *Hub {
	h.instanceID = f
	return h
}

// WithServerVersion sets the version the challenge reports. Call it before the hub serves.
func (h *Hub) WithServerVersion(v string) *Hub {
	h.serverVersion = v
	return h
}

// WithRefusalAudit sets where refused handshakes are recorded as ingress.node_enrol_refused. Nil records
// nothing. Call it before the hub serves.
func (h *Hub) WithRefusalAudit(r auth.AuditRecorder) *Hub {
	h.refusals = r
	return h
}

// WithFirstFrameTimeout sets how long a node has, from the challenge, to send its auth. A value below one
// keeps firstFrameTimeout. Tests change it; nothing else should. Call it before the hub serves.
func (h *Hub) WithFirstFrameTimeout(d time.Duration) *Hub {
	if d > 0 {
		h.firstFrame = d
	}
	return h
}

// WithOfflineAfter sets how long a session may be silent before the watchdog closes it, and for how long a
// session counts as live. A value below one keeps DefaultOfflineAfter. Call it before the hub serves.
func (h *Hub) WithOfflineAfter(d time.Duration) *Hub {
	if d > 0 {
		h.offlineAfter = d
	}
	return h
}

// WithWatchTicks replaces the watchdog's time.Ticker with ticks: every value received is one look for silent
// sessions. A test sends on an unbuffered channel, so a send that completes means the watchdog took the tick,
// and the next send completing means it has finished evaluating the first. The channel is shared by every
// session of the hub, so a test that uses it holds one session. Nil, the default, keeps the ticker. Call it
// before the hub serves.
func (h *Hub) WithWatchTicks(ticks <-chan time.Time) *Hub {
	h.watchTicks = ticks
	return h
}

// OnUpgradeFailure sets a hook that runs, with the client address, for every request to /ingress/connect that
// arrives over TLS and is not a WebSocket upgrade. A proxy that strips the Upgrade headers produces exactly this,
// so the proxy observer reads it as a misconfiguration signal, as it does for the agent endpoint. The hook must
// not block. Call it before the hub serves.
func (h *Hub) OnUpgradeFailure(f func(peer string)) *Hub {
	h.onUpgradeFailure = f
	return h
}

// WithBeforeRegister sets a function the hub calls with the node ID after a handshake was verified and the
// node welcomed, and before its session is registered. It exists for tests, which block in it to revoke a node
// or finish another connection in that window; nothing else should set it. Call it before the hub serves.
func (h *Hub) WithBeforeRegister(f func(nodeID string)) *Hub {
	h.beforeRegister = f
	return h
}

// WithPushObserver sets a function told the outcome of every look at an environment's current bundle on behalf
// of a session. It exists for tests, which wait on these events instead of on the clock; nothing else should
// set it. It is called from the session's writer, so it must return promptly. Call it before the hub serves.
func (h *Hub) WithPushObserver(f func(PushEvent)) *Hub {
	h.onPush = f
	return h
}

// Handler serves GET /ingress/connect.
func (h *Hub) Handler() http.Handler { return http.HandlerFunc(h.serve) }

// pinList is what a welcome sent now would carry.
func (h *Hub) pinList() []string {
	if h.pins == nil {
		return nil
	}
	return h.pins()
}

// audience is the host the node must have dialed, from the public URL the hub was given. Plain transport is
// never acceptable, so there is no flag for it: the preflight decides that from the connection alone.
func (h *Hub) audience() (string, error) {
	if h.publicURL == nil {
		return "", errors.New("the hub was given no public URL")
	}
	raw := h.publicURL()
	if raw == "" {
		return "", errors.New("the public URL is not set")
	}
	return authn.Audience(raw)
}

// Session returns the live session of nodeID, or nil.
func (h *Hub) Session(nodeID string) *Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[nodeID]
}

// notifyRegistryLocked wakes every AwaitSession waiter. h.mu must be held.
func (h *Hub) notifyRegistryLocked() {
	if h.regChanged != nil {
		close(h.regChanged)
		h.regChanged = nil
	}
}

// AwaitSession returns the first registered session of nodeID for which cond is true. cond receives nil while
// the node has no live session, so a test can wait for a registration (cond: s != nil) and for a removal
// (cond: s == nil). It evaluates cond now and again after every registration and removal of any session, and
// never polls. When ctx ends first it returns the session it last saw together with the context's error; the
// context only guards against a hang.
func (h *Hub) AwaitSession(ctx context.Context, nodeID string, cond func(*Session) bool) (*Session, error) {
	for {
		// Take the channel in the same critical section as the read, so a change between the read and the
		// wait is not missed.
		h.mu.Lock()
		if h.regChanged == nil {
			h.regChanged = make(chan struct{})
		}
		ch, s := h.regChanged, h.sessions[nodeID]
		h.mu.Unlock()
		if cond(s) {
			return s, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return s, ctx.Err()
		}
	}
}

// Close ends every session with a close frame, so nodes reconnect with backoff, and refuses new connections.
// http.Server.Shutdown ignores hijacked connections, so the server registers this with RegisterOnShutdown. It
// is safe to call more than once.
func (h *Hub) Close() {
	h.stopOnce.Do(func() { close(h.stop) })
	if h.sub != nil {
		h.sub.Close()
	}
	h.mu.Lock()
	h.closed = true
	live := make([]*Session, 0, len(h.sessions))
	for _, s := range h.sessions {
		live = append(live, s)
	}
	h.mu.Unlock()

	var wg sync.WaitGroup
	for _, s := range live {
		s.endWith(ingresswire.CloseShutdown)
		wg.Go(func() { s.close(websocket.StatusGoingAway, ingresswire.CloseShutdown) })
	}
	wg.Wait()
}

func (h *Hub) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

var (
	errHubClosed      = errors.New("ingresshub: the hub is closed")
	errNoLongerActive = errors.New("ingresshub: the node was revoked while it connected")
)

// errSuperseded is why register turned away a session of a node whose newer connection has registered
// already; errHubClosed and errNoLongerActive, above, are the other reasons.
var errSuperseded = errors.New("ingresshub: a newer connection of the node has registered")

// errStatusUnknown is why register turned away a session whose node's status could not be read: a fault of the
// server's, which the node is told to retry, never a revoke.
var errStatusUnknown = errors.New("ingresshub: the node's status could not be read")

// register adds s, replacing any session of the same node: a half-open stale connection must not block the
// reconnect. The old session is closed with the reason replaced, and it leaves the map in the same critical
// section the new one enters, so two sessions of one node are never registered. It reports errHubClosed when
// the hub is closed.
//
// A session whose connection was accepted before that of the node's newest session is turned away with
// errSuperseded instead: two handshakes of one node can overlap, the node has moved on to the newer connection
// and dropped the older, and an older handshake that finishes last must not replace the live session with one
// nobody answers on (D-09).
//
// The node's status is read again here, after the lifecycle lock is taken. A revoke commits first and then
// calls Kick, and Kick takes the same lock, so either the status read below sees the revoke, or the session is
// already in the map when Kick looks and is closed by it. There is no order in which a session of a revoked
// node stays registered (D-09, T-22-36).
//
// The read cannot move before the lock: a read that saw the node active, then a revoke that committed and
// kicked while the lock was free and found no session, would let this registration proceed for a revoked node.
// So it stays under the lock, bounded by the short stillActiveTimeout, and a read that fails is errStatusUnknown,
// a retryable close, never errNoLongerActive (WR-A-01).
func (h *Hub) register(s *Session) error {
	h.life.Lock()
	defer h.life.Unlock()

	active, err := h.svc.StillActive(s.NodeID)
	if err != nil {
		return fmt.Errorf("%w: %w", errStatusUnknown, err)
	}
	if !active {
		return errNoLongerActive
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return errHubClosed
	}
	if n, ok := h.newest[s.NodeID]; ok && s.order < n {
		h.mu.Unlock()
		return errSuperseded
	}
	h.newest[s.NodeID] = s.order
	old := h.sessions[s.NodeID]
	h.sessions[s.NodeID] = s
	h.notifyRegistryLocked()
	h.mu.Unlock()

	if old != nil {
		old.endWith(ingresswire.CloseReplaced)
		// The close frame waits for the peer, so it is not sent under the lifecycle lock; the grace makes it
		// bounded.
		go old.kickWith(websocket.StatusGoingAway, ingresswire.CloseReplaced)
	}
	return nil
}

// Kick ends the live session of nodeID, if it has one, with policy-violation status and code as the close
// reason, and records code as why it ended. It returns at once; the session is closed within a second even if
// the node never answers the close frame. The service calls it after a revoke has committed.
//
// Kick takes the lifecycle lock briefly, which makes it wait for a registration in progress: a session that
// registered before the revoke committed is in the map by the time Kick looks (see register).
func (h *Hub) Kick(nodeID, code string) {
	h.life.Lock()
	h.mu.Lock()
	s := h.sessions[nodeID]
	h.mu.Unlock()
	h.life.Unlock()
	if s != nil {
		s.endWith(code)
		go s.kick(code)
	}
}

// KickEnv is Kick for every live session of the environment envID, and for no other.
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

// Sessions returns the registered sessions, in no order.
func (h *Hub) Sessions() []*Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*Session, 0, len(h.sessions))
	for _, s := range h.sessions {
		out = append(out, s)
	}
	return out
}

// Live reports whether nodeID has a registered session that has shown a sign of life within the offline
// window.
func (h *Hub) Live(nodeID string) bool {
	s := h.Session(nodeID)
	return s != nil && h.alive(s)
}

// alive is whether s has shown a sign of life within the offline window.
func (h *Hub) alive(s *Session) bool {
	return h.now().Sub(s.lastSignOfLife()) <= h.offlineAfter
}

// DialHosts returns the host each live session dialed, once each and sorted: the names the ingress channel is
// reachable under, which the certificate preflight of an ACME order checks (D-29).
func (h *Hub) DialHosts() []string {
	var hosts []string
	for _, s := range h.Sessions() {
		if h.alive(s) {
			hosts = append(hosts, s.DialHost)
		}
	}
	slices.Sort(hosts)
	return slices.Compact(hosts)
}

// watch closes a session whose last sign of life is older than the offline window, with the reason timeout.
// The close frame is bounded by kickGrace, because the peer has most likely stopped answering. It ends with
// the session or ctx.
func (h *Hub) watch(ctx context.Context, s *Session) {
	ticks := h.watchTicks
	if ticks == nil {
		t := time.NewTicker(max(h.offlineAfter/6, 10*time.Millisecond))
		defer t.Stop()
		ticks = t.C
	}
	for {
		select {
		case <-s.done:
			return
		case <-ctx.Done():
			return
		case <-ticks:
		}
		if silent := h.now().Sub(s.lastSignOfLife()); silent > h.offlineAfter {
			h.log.Warn("ingress node session closed: nothing heard within the offline window",
				"node_id", s.NodeID, "environment_id", s.EnvID, "silent_for", silent.Round(time.Second).String())
			s.endWith(ingresswire.CloseTimeout)
			s.kick(ingresswire.CloseTimeout)
			return
		}
	}
}

// finish removes s from the registry once its session has ended, unless a newer session of the node has
// replaced it already, and wakes AwaitSession waiters.
func (h *Hub) finish(s *Session) {
	h.life.Lock()
	defer h.life.Unlock()
	h.mu.Lock()
	if h.sessions[s.NodeID] == s {
		delete(h.sessions, s.NodeID)
	}
	h.notifyRegistryLocked()
	last := true
	for _, other := range h.sessions {
		if other.EnvID == s.EnvID {
			last = false
			break
		}
	}
	h.mu.Unlock()
	if last {
		// No node of the environment is connected: its bundle bytes (every private key) need not stay in memory.
		h.envelopes.drop(s.EnvID)
	}
}

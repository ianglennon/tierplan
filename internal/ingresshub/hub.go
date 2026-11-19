// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
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

	// life serializes session lifecycle transitions: a registration and a kick take it, so a revoke that
	// commits and then kicks either finds the session registered or is seen by the registration (register).
	life sync.Mutex

	mu       sync.Mutex
	sessions map[string]*Session
	closed   bool
	// regChanged is closed and cleared whenever a session is registered or removed, to wake AwaitSession
	// waiters; a waiter makes it. Guarded by mu.
	regChanged chan struct{}
	// accepted numbers the connections in the order they were accepted.
	accepted atomic.Uint64
}

// NewHub returns a Hub. svc is required, src may be nil (a node then gets no bundle), and a nil log is
// slog.Default. A nil now is time.Now, as NewService treats it (D-36); it is made before the gate is built from
// it, so h.now is never nil and the gate's limiters and every read of the clock here share the one clock.
func NewHub(svc *Service, src BundleSource, log *slog.Logger, now func() time.Time) *Hub {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		svc: svc, src: src, log: log, now: now,
		gate:       newGate(now),
		warnLimit:  auth.NewIPLimiter(time.Minute, 1, limiterTTL, now),
		firstFrame: firstFrameTimeout,
		sessions:   map[string]*Session{},
	}
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

// errHubClosed and errNoLongerActive are why register turned a session away.
var (
	errHubClosed      = errors.New("ingresshub: the hub is closed")
	errNoLongerActive = errors.New("ingresshub: the node was revoked while it connected")
)

// register adds s, replacing any session of the same node: a half-open stale connection must not block the
// reconnect. The node's status is read again here, after the lifecycle lock is taken, so a revoke that commits
// first and then kicks (Kick takes the same lock) is either seen by this read or finds the session already in
// the map; there is no order in which a session of a revoked node stays registered (D-09). It reports
// errHubClosed when the hub is closed.
//
// The superseded-order rule (an older handshake that finishes last must not replace a newer session), Kick,
// KickEnv, Live, DialHosts and the silence watchdog are plan 22-15's.
func (h *Hub) register(s *Session) error {
	h.life.Lock()
	defer h.life.Unlock()

	if !h.svc.StillActive(s.NodeID) {
		return errNoLongerActive
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return errHubClosed
	}
	old := h.sessions[s.NodeID]
	h.sessions[s.NodeID] = s
	h.notifyRegistryLocked()
	h.mu.Unlock()

	if old != nil {
		old.endWith(ingresswire.CloseReplaced)
		// The close frame waits for the peer, so it is not sent under the lifecycle lock.
		go old.close(websocket.StatusGoingAway, ingresswire.CloseReplaced)
	}
	return nil
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
	h.mu.Unlock()
}

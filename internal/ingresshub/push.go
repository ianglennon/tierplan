// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"

	"github.com/ianglennon/tierplan/internal/ingressrender"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// The outcomes of a look at an environment's current bundle on behalf of one session.
const (
	// PushSent: the bundle was written to the node.
	PushSent = "sent"
	// PushSame: the node runs the current bundle already, or has just been sent it.
	PushSame = "same"
	// PushRejectedBefore: the node refused the current bundle on this session, so it is not sent again until the
	// environment's bundle ID changes or the node reconnects.
	PushRejectedBefore = "rejected_before"
	// PushIncompatible: the node's schemas share none with this server's (or with the bundle's), so it keeps
	// the bundle it has.
	PushIncompatible = "incompatible"
	// PushNone: the environment has no current bundle.
	PushNone = "none"
	// PushTooLarge: the marshalled bundle is over ingresswire.MaxBundleMessageBytes, which a node refuses.
	PushTooLarge = "too_large"
	// PushWriteFailed: the bundle could not be written or recorded, and the session ends.
	PushWriteFailed = "write_failed"
	// PushDisabled: the environment's ingress is switched off, or the switch could not be read, so the node is sent
	// nothing and nothing is recorded (D-12).
	PushDisabled = "disabled"
)

// PushEvent is the outcome of one look at the environment's current bundle on behalf of one session. BundleID is
// the current bundle's ID, empty when there is none.
type PushEvent struct {
	NodeID, EnvID, BundleID, Outcome string
}

func (h *Hub) emit(ev PushEvent) {
	if h.onPush != nil {
		h.onPush(ev)
	}
}

// Run delivers changed bundles: it consumes the subscription NewHub took and, when an environment's bundle ID
// changes, or the notifier given to WithEnvChanges names an environment, wakes the writer of every session of that
// environment. It returns when ctx ends or the hub is closed. A hub built without a source or an environment
// notifier has nothing to deliver, and Run only waits.
func (h *Hub) Run(ctx context.Context) {
	var srcC, envC <-chan struct{}
	if h.sub != nil {
		srcC = h.sub.C()
	}
	if h.envSub != nil {
		envC = h.envSub.C()
	}
	// With nothing subscribed both channels are nil and the loop only waits for the end.
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.stop:
			return
		case <-srcC:
			for _, envID := range h.sub.Pending() {
				h.wakeEnv(envID)
			}
		case <-envC:
			for _, envID := range h.envSub.Pending() {
				h.wakeEnv(envID)
			}
		}
	}
}

// wakeEnv asks the writer of every live session of envID to look at the current bundle.
func (h *Hub) wakeEnv(envID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sessions {
		if s.EnvID == envID {
			s.wake()
		}
	}
}

// writeLoop is the session's one writer: on each wake it decides whether the node is owed a bundle and writes
// it. It is the only goroutine that writes to the node after the welcome, and it ends with ctx or when a write
// fails.
func (h *Hub) writeLoop(ctx context.Context, s *Session) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.poke:
		}
		if ctx.Err() != nil {
			return
		}
		if !h.deliver(ctx, s) {
			return
		}
	}
}

// deliver looks at the current bundle of the session's own environment, taken from the authenticated row and
// never from anything the node sent, and writes it when the node is owed it. It reports false when the session
// must end.
func (h *Hub) deliver(ctx context.Context, s *Session) bool {
	if h.src == nil {
		return true
	}
	st := s.pushSnapshot()
	if !st.hello {
		return true // the hello wakes the writer again
	}
	if h.deliveryGate != nil && !h.deliveryGate(ctx, s.EnvID) {
		h.emit(PushEvent{NodeID: s.NodeID, EnvID: s.EnvID, Outcome: PushDisabled})
		return true
	}
	b, have := h.src.Current(s.EnvID)
	ev := PushEvent{NodeID: s.NodeID, EnvID: s.EnvID, BundleID: b.ID}
	switch {
	case !have:
		ev.BundleID, ev.Outcome = "", PushNone
	case !h.renderable(st.schemas, b):
		ev.Outcome = PushIncompatible
	case b.ID == st.applied:
		ev.Outcome = PushSame
	case b.ID == st.rejected:
		ev.Outcome = PushRejectedBefore
	case b.ID == st.lastSent:
		ev.Outcome = PushSame
	default:
		ev.Outcome = h.send(ctx, s, b)
	}
	if ev.Outcome != "" {
		h.emit(ev)
	}
	return ev.Outcome != PushWriteFailed
}

// renderable reports whether the node can apply b: it offered a schema this server renders, and that is the
// schema of b. The reconciler renders one schema (ingressrender.Latest), so with one schema the picked one and
// b's own are the same whenever the node is compatible. A second schema would need a render per schema from the
// reconciler, which does not exist; until then a bundle of another schema than the picked one is not sent.
func (h *Hub) renderable(offered []int, b ingressbundle.Bundle) bool {
	picked, err := ingressrender.PickSchema(offered)
	return err == nil && picked == b.Schema
}

// send writes b to the node and returns the outcome, or "" when the node is no longer active and nothing was
// done. The hostnames of its routes are recorded as sent before the
// frame is written, so a revoke lists every hostname the node could have held whether or not the frame arrived
// (D-24); the ID is remembered before the write too, because a fast node may answer before the write returns.
func (h *Hub) send(ctx context.Context, s *Session, b ingressbundle.Bundle) string {
	active, err := h.svc.StillActive(s.NodeID)
	if err != nil {
		// Not a revoke: the status is unknown, so nothing is sent and the session ends, which the node retries.
		h.log.Warn("could not confirm an ingress node is still active before a bundle", "node_id", s.NodeID, "environment_id", s.EnvID, "error", err)
		h.endForError(s)
		return PushWriteFailed
	}
	if !active {
		// A revoke has committed and its kick is on the way: a revoked node is sent nothing, and the kick gives
		// the node its reason. No outcome is reported, and the next wake looks again.
		return ""
	}
	instanceID, err := h.readInstanceID(ctx)
	if err != nil {
		h.log.Warn("could not read the instance ID for a bundle", "node_id", s.NodeID, "environment_id", s.EnvID, "error", err)
		h.endForError(s)
		return PushWriteFailed
	}
	env, err := h.envelopes.get(s.EnvID, instanceID, b)
	if err != nil {
		h.log.Warn("could not encode a bundle", "node_id", s.NodeID, "environment_id", s.EnvID, "error", err)
		h.endForError(s)
		return PushWriteFailed
	}
	if len(env.payload) > ingresswire.MaxBundleMessageBytes {
		h.log.Warn("an ingress bundle is too large to send and was not",
			"node_id", s.NodeID, "environment_id", s.EnvID, "bundle", idPrefix(b.ID), "bytes", len(env.payload), "limit", ingresswire.MaxBundleMessageBytes)
		return PushTooLarge
	}
	names := make([]string, 0, len(b.Manifest.Routes))
	for _, r := range b.Manifest.Routes {
		names = append(names, r.Hostname)
	}
	if err := h.svc.RecordSent(ctx, s.NodeID, names); errors.Is(err, ErrNodeNotActive) {
		// A revoke committed after the check above: as there, nothing is sent and the kick ends the session.
		return ""
	} else if err != nil {
		h.log.Warn("could not record the hostnames of a bundle sent", "node_id", s.NodeID, "environment_id", s.EnvID, "error", err)
		h.endForError(s)
		return PushWriteFailed
	}
	s.markSent(b.ID)
	if h.beforeWrite != nil {
		h.beforeWrite(s.NodeID)
	}
	wctx, cancel := context.WithTimeout(ctx, bundleWriteTimeout)
	defer cancel()
	if err := s.ws.Write(wctx, websocket.MessageBinary, env.payload); err != nil {
		if ctx.Err() == nil {
			h.log.Warn("could not write a bundle to an ingress node", "node_id", s.NodeID, "environment_id", s.EnvID, "error", err)
			h.endForError(s)
		}
		return PushWriteFailed
	}
	// The envelope is logged through its LogValue only: it holds every private key of the environment.
	h.log.Info("bundle sent to an ingress node", "node_id", s.NodeID, "environment_id", s.EnvID, "envelope", env.envelope)
	return PushSent
}

// endForError closes the session after a failure of the hub's own, with a close frame bounded by kickGrace.
func (h *Hub) endForError(s *Session) {
	s.kickWith(websocket.StatusInternalError, "internal_error")
}

// envelopeKey names one marshalled message: the bundle ID alone is not enough, because the message carries the
// instance ID too.
type envelopeKey struct{ envID, bundleID, instanceID string }

// cachedEnvelope is a marshalled bundle message and the envelope it came from, which is what a log line shows.
type cachedEnvelope struct {
	key      envelopeKey
	envelope ingresswire.BundleEnvelope
	payload  []byte
}

// envelopeCache holds the newest marshalled message of each environment, so a bundle that goes to every node of
// an environment is encoded once. An older bundle of the environment is dropped when a newer one replaces it.
type envelopeCache struct {
	mu     sync.Mutex
	by     map[string]cachedEnvelope
	builds atomic.Int64
}

func newEnvelopeCache() envelopeCache { return envelopeCache{by: map[string]cachedEnvelope{}} }

// get returns the message of b for envID and instanceID, marshalling it when the cache has not got it. The
// files are shared with the source and never modified.
func (c *envelopeCache) get(envID, instanceID string, b ingressbundle.Bundle) (cachedEnvelope, error) {
	key := envelopeKey{envID: envID, bundleID: b.ID, instanceID: instanceID}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.by[envID]; ok && e.key == key {
		return e, nil
	}
	env := ingresswire.BundleEnvelope{T: ingresswire.TypeBundle, InstanceID: instanceID, Bundle: b}
	payload, err := json.Marshal(env)
	if err != nil {
		return cachedEnvelope{}, err
	}
	c.builds.Add(1)
	e := cachedEnvelope{key: key, envelope: env, payload: payload}
	c.by[envID] = e
	return e, nil
}

// drop forgets the environment's message.
func (c *envelopeCache) drop(envID string) {
	c.mu.Lock()
	delete(c.by, envID)
	c.mu.Unlock()
}

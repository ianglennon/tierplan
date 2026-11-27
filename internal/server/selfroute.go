// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
)

// selfReadyTimeout is how long the holder waits for the first observation of its claim's environment before it says it is
// ready anyway (A-08): the proxy observer holds its bell until then, and must not hold it forever when no pass reports.
const selfReadyTimeout = 2 * time.Minute

// selfReloadTimeout bounds the claim read Observe makes. Observe is called from the ensure loop with no context of its own.
const selfReloadTimeout = 5 * time.Second

// selfSnapshot is what the holder knows, swapped whole so a request reads one consistent state. It is never changed after
// it is published.
type selfSnapshot struct {
	// ClaimRead is whether the stored claim was read at least once.
	ClaimRead bool
	// Claimed is whether the system claim exists; EnvID and Host are its environment and hostname.
	Claimed bool
	EnvID   string
	Host    string
	// Origins are the extra origins a mutation may come from: the route host's, while the claim exists (D-07).
	Origins []string
	// Obs is the latest observation of TierPlan's own environment, HasObs whether there is one.
	Obs    ingressdeploy.SelfObservation
	HasObs bool
	// Prefixes are the live trusted-proxy prefixes (D-06).
	Prefixes []netip.Prefix
	// Attached, Observed, Backend and Kind are the observation's, as later readers want them.
	Attached bool
	Observed bool
	Backend  string
	Kind     string
	// WaitSince is when the holder began waiting for the current claim's first observation; the ready fallback counts from it.
	WaitSince time.Time
}

// selfRoute holds the live trusted-proxy set and the extra origin of TierPlan's own route (D-06, D-07). It is filled from
// the stored system claim, a database read, and from the observations of TierPlan's own self network the ingress ensure
// loop publishes. Nothing a request carries reaches it, and an observation of an environment other than TierPlan's own
// changes nothing (SELF-03).
type selfRoute struct {
	systemClaim func(ctx context.Context) (ingressstore.SystemClaim, bool, error)
	now         func() time.Time
	log         *slog.Logger

	// mu serialises writers; readers load snap.
	mu   sync.Mutex
	snap atomic.Pointer[selfSnapshot]
}

// newSelfRoute builds the holder. now nil is time.Now and log nil is slog.Default(). It reads nothing until Reload.
func newSelfRoute(systemClaim func(ctx context.Context) (ingressstore.SystemClaim, bool, error), now func() time.Time, log *slog.Logger) *selfRoute {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	h := &selfRoute{systemClaim: systemClaim, now: now, log: log}
	h.snap.Store(&selfSnapshot{WaitSince: now()})
	return h
}

// Reload reads the stored claim. A read that fails leaves what the holder knew in place.
func (h *selfRoute) Reload(ctx context.Context) error {
	c, ok, err := h.systemClaim(ctx)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cur := *h.snap.Load()
	next := cur
	next.ClaimRead = true
	next.Claimed = ok
	next.EnvID, next.Host = "", ""
	if ok {
		next.EnvID, next.Host = c.EnvID, strings.ToLower(ingress.NormalizeHost(c.Hostname))
	}
	// A claim that moves to another environment waits afresh for that environment's first observation.
	if next.Claimed && (!cur.Claimed || cur.EnvID != next.EnvID) {
		next.WaitSince = h.now()
	}
	h.publish(cur, next)
	return nil
}

// Observe takes an observation the ingress Service published. It reads the claim first, since a switch on or off wakes the
// ensure loop, then keeps the observation only when it is of TierPlan's own environment: the claim's while one exists,
// else the one TierPlan runs in (the carve-out case), as the observation's Kind says.
func (h *selfRoute) Observe(o ingressdeploy.SelfObservation) {
	ctx, cancel := context.WithTimeout(context.Background(), selfReloadTimeout)
	defer cancel()
	if err := h.Reload(ctx); err != nil {
		h.log.Warn("the system claim could not be read; TierPlan's own route keeps what it knew", "error", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cur := *h.snap.Load()
	switch {
	case cur.Claimed && o.EnvID != cur.EnvID:
		return
	case !cur.Claimed && o.Kind == "" && !(cur.HasObs && cur.Obs.EnvID == o.EnvID):
		// No claim, and TierPlan does not run here: the observation is of another environment. One of the environment
		// already held, saying TierPlan is no longer there, replaces it.
		return
	}
	next := cur
	next.Obs, next.HasObs = o, true
	h.publish(cur, next)
}

// publish derives the sets from next and stores it, logging a change of the trusted set (D-18: logged, not audited; the
// audited switch actions precede it). The caller holds mu.
func (h *selfRoute) publish(cur, next selfSnapshot) {
	next.Origins = nil
	if next.Claimed && next.Host != "" {
		next.Origins = []string{"https://" + next.Host}
	}
	next.Attached, next.Observed, next.Backend, next.Kind = false, false, "", ""
	next.Prefixes = nil
	own := next.HasObs && (!next.Claimed || next.Obs.EnvID == next.EnvID)
	if own {
		o := next.Obs
		next.Attached, next.Observed, next.Backend, next.Kind = o.Attached, o.Observed, o.BackendHost, o.Kind
		if o.Exists && o.Attached && !o.Foreign {
			next.Prefixes = ingress.SelfTrustedPrefixes(o.Facts())
		}
	}
	h.snap.Store(&next)
	if !slices.Equal(cur.Prefixes, next.Prefixes) {
		h.log.Info("the trusted proxies on TierPlan's own network changed",
			"env_id", next.Obs.EnvID, "prefixes_before", len(cur.Prefixes), "prefixes_after", len(next.Prefixes))
	}
}

// TrustedPrefixes is the live trusted-proxy set (httpx.Options.LiveTrustedProxies): TierPlan's own self network's subnet
// without its gateway, while TierPlan is attached to it; else none.
func (h *selfRoute) TrustedPrefixes() []netip.Prefix {
	return slices.Clone(h.snap.Load().Prefixes)
}

// ExtraOrigins is the route host's origin while the system claim exists, else none (httpx.Options.ExtraOrigins and the
// shell WebSocket's origin patterns). It comes from the stored claim only.
func (h *selfRoute) ExtraOrigins() []string {
	return slices.Clone(h.snap.Load().Origins)
}

// Ready is whether the trusted set can be believed complete (A-08): the claim was read and, when it exists, the first
// observation of its environment arrived. selfReadyTimeout after the wait began it is true regardless.
func (h *selfRoute) Ready() bool {
	s := h.snap.Load()
	if s.ClaimRead && (!s.Claimed || (s.HasObs && s.Obs.EnvID == s.EnvID && s.Obs.Observed)) {
		return true
	}
	return !h.now().Before(s.WaitSince.Add(selfReadyTimeout))
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrecon

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingresscert"
	"github.com/ianglennon/tierplan/internal/ingressrender"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// Debounce is the one reconciler-wide window (D-32, which amends D-19's per-environment wording). The
// first wake-up after a quiet period arms it; when it fires, every environment that changed since is
// rendered. A stream of writes cannot postpone the render, because the window is fixed, not restarted.
const Debounce = 250 * time.Millisecond

// Model is the ingress model the reconciler reads. *ingressstore.Store satisfies it.
type Model interface {
	// Snapshot reads everything the renderer needs of one environment.
	Snapshot(ctx context.Context, envID string) (ingress.Snapshot, error)
	// Versions reads the version pair of every environment that has an ingress row.
	Versions(ctx context.Context) ([]ingress.EnvVersion, error)
}

// Certs is the certificate source. *ingresscert.Store satisfies it.
type Certs interface {
	// Serving returns the certificate version and, for each hostname, the servable pair or the reason there
	// is none.
	Serving(ctx context.Context, envID string, hostnames []string, now time.Time) (int64, []ingresscert.ServingCert, error)
}

// Deps are what a Reconciler needs. A nil Clock is the wall clock, a nil Log is the default logger and a nil
// Render is ingressrender.Render.
type Deps struct {
	Model    Model
	Certs    Certs
	Notifier *ingress.Notifier
	Clock    stackops.Clock
	Log      *slog.Logger
	// Render turns a snapshot and its certificates into a bundle.
	Render func(ingressrender.Input) (ingressrender.Result, error)
	// OnRender, when set, is told the environment, the bundle ID and whether the ID is new after every render
	// attempt that produced a bundle. It is a test seam; production leaves it nil.
	OnRender func(envID, id string, changed bool)
}

// envState is what the reconciler holds of one environment.
type envState struct {
	// run serializes the reconciles of the environment.
	run sync.Mutex

	// mu guards the fields below, which Current and Skipped read.
	mu      sync.Mutex
	bundle  ingressbundle.Bundle
	have    bool
	skipped []ingressrender.Skipped
	// config and certs are the version pair the current bundle's last render read.
	config, certs int64
}

// Reconciler keeps the current bundle of every environment, in memory only (D-20).
type Reconciler struct {
	clock   stackops.Clock
	log     *slog.Logger
	d       Deps
	render  func(ingressrender.Input) (ingressrender.Result, error)
	changes *ingress.Notifier

	mu   sync.Mutex
	envs map[string]*envState
}

// New returns a Reconciler.
func New(d Deps) *Reconciler {
	clock := d.Clock
	if clock == nil {
		clock = stackops.RealClock()
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	render := d.Render
	if render == nil {
		render = ingressrender.Render
	}
	return &Reconciler{
		clock: clock, log: log.With("component", "ingressrecon"), d: d, render: render,
		changes: ingress.NewNotifier(), envs: map[string]*envState{},
	}
}

// Changes is told an environment's id only when that environment's bundle ID changes.
func (r *Reconciler) Changes() *ingress.Notifier { return r.changes }

// state returns the environment's state, creating it on first use.
func (r *Reconciler) state(envID string) *envState {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.envs[envID]
	if !ok {
		s = &envState{}
		r.envs[envID] = s
	}
	return s
}

// Current is the environment's current bundle and whether it has one. The files are shared with the
// reconciler and must not be modified.
func (r *Reconciler) Current(envID string) (ingressbundle.Bundle, bool) {
	s := r.state(envID)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bundle, s.have
}

// Skipped is the routes the environment's current bundle leaves out, with their reasons. The slice is a copy.
func (r *Reconciler) Skipped(envID string) []ingressrender.Skipped {
	s := r.state(envID)
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.skipped)
}

// Reconcile renders one environment and publishes the bundle when its ID is new. An error leaves the
// stored bundle untouched.
func (r *Reconciler) Reconcile(ctx context.Context, envID string) error {
	s := r.state(envID)
	s.run.Lock()
	defer s.run.Unlock()

	snap, err := r.d.Model.Snapshot(ctx, envID)
	if err != nil {
		return err
	}
	hosts := make([]string, 0, len(snap.Claims))
	for _, c := range snap.Claims {
		hosts = append(hosts, c.Hostname)
	}
	certsVersion, serving, err := r.d.Certs.Serving(ctx, envID, hosts, r.clock.Now())
	if err != nil {
		return err
	}
	certs := make([]ingressrender.HostCert, 0, len(serving))
	for _, c := range serving {
		certs = append(certs, ingressrender.HostCert{
			Hostname: c.Hostname, ChainPEM: c.ChainPEM, KeyPEM: c.KeyPEM, Reason: ingressrender.SkipReason(c.Reason),
		})
	}
	res, err := r.render(ingressrender.Input{Snapshot: snap, Certs: certs, CertsVersion: certsVersion, OwnVersion: 0})
	if err != nil {
		return err
	}

	s.mu.Lock()
	changed := !s.have || s.bundle.ID != res.Bundle.ID
	if changed {
		s.bundle, s.have = res.Bundle, true
	} else {
		// The same content: only the metadata beside the ID moves (Pitfall 11, D-32).
		s.bundle.Version = res.Bundle.Version
	}
	s.skipped = res.Skipped
	s.config, s.certs = snap.Settings.ConfigVersion, certsVersion
	s.mu.Unlock()

	if changed {
		r.changes.Notify(envID)
	}
	if r.d.OnRender != nil {
		r.d.OnRender(envID, res.Bundle.ID, changed)
	}
	return nil
}

// Run renders every environment, then keeps them current until ctx ends: a wake-up from the notifier arms
// the debounce window if it is not armed, and when the window fires every environment that changed is
// reconciled. It subscribes before it renders, so a change during the first pass is not lost.
func (r *Reconciler) Run(ctx context.Context) {
	// A reconciler built without a notifier is woken by nothing; a nil channel never fires.
	var wake <-chan struct{}
	var pending func() []string
	if r.d.Notifier != nil {
		sub := r.d.Notifier.Subscribe()
		defer sub.Close()
		wake, pending = sub.C(), sub.Pending
	}

	r.renderAll(ctx)

	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			if debounce == nil {
				debounce = r.clock.After(Debounce)
			}
		case <-debounce:
			debounce = nil
			if pending == nil {
				continue
			}
			for _, envID := range pending() {
				if ctx.Err() != nil {
					return
				}
				if err := r.Reconcile(ctx, envID); err != nil {
					r.log.Error("the ingress reconciler could not render an environment", "env_id", envID, "error", err)
				}
			}
		}
	}
}

// renderAll renders every environment Versions lists. A read error is logged once and the loop goes on: the
// notifier and the later poll bring the environments in.
func (r *Reconciler) renderAll(ctx context.Context) {
	versions, err := r.d.Model.Versions(ctx)
	if err != nil {
		r.log.Error("the ingress reconciler could not list the environments", "error", err)
		return
	}
	for _, v := range versions {
		if ctx.Err() != nil {
			return
		}
		if err := r.Reconcile(ctx, v.EnvID); err != nil {
			r.log.Error("the ingress reconciler could not render an environment", "env_id", v.EnvID, "error", err)
		}
	}
}

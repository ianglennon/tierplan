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

// PollInterval is how often Run reads every environment's two versions (D-26). It is one indexed read of the
// whole table, cheap enough every two seconds, and it is the only thing that sees a stack deleted by cascade
// until Phase 24 notifies on the stack-delete path: the cascade raises config_version and calls no Notify.
const PollInterval = 2 * time.Second

// SafetyTick is how often Run renders every environment regardless of versions (D-19). It is the last net,
// for a change no version recorded (a certificate source that changed without a bump, say).
const SafetyTick = time.Minute

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
	// Self is TierPlan's own route and trust for the environment that carries TierPlan (D-14): the self
	// input, the own version it was built at (it moves whenever TierPlan's own certificate does) and ok.
	// It is called for every environment rendered and, through the version poll, every PollInterval, so it
	// must be cheap. Nil, or ok false, renders no self input and own version 0.
	Self func(ctx context.Context, envID string) (ingressrender.SelfInput, int64, bool)
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
	// config and certs are the version pair the current bundle's last render read, and own the own version
	// it was rendered at (0 when the environment does not carry TierPlan).
	config, certs, own int64
	// selfRefused is the last render's refusal of the self input as text, "" for none, so a refusal that
	// repeats on every poll is logged once.
	selfRefused string
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

	// fmu guards the failure book: the environments whose latest render failed, and the failures already logged for
	// each, so one that repeats is said once.
	fmu      sync.Mutex
	failures map[string]Failure
	logged   map[string]map[string]struct{}
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
		failures: map[string]Failure{}, logged: map[string]map[string]struct{}{},
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

// RenderedOwn is the own version of the last bundle rendered for the environment: 0 before any render, for an
// environment that does not carry TierPlan and for one the reconciler does not know. The staged install (28-32)
// reads it to tell when the proxies have the trust bundle of TierPlan's new certificate.
func (r *Reconciler) RenderedOwn(envID string) int64 {
	r.mu.Lock()
	s, ok := r.envs[envID]
	r.mu.Unlock()
	if !ok {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.own
}

// Reconcile renders one environment and publishes the bundle when its ID is new. An error leaves the stored
// bundle, its version pair and its skipped list untouched (D-20, PROH-21-06): the environment's nodes keep the last
// good configuration, and the error is recorded as the environment's Failure and logged once. A render that
// succeeds clears the Failure. A render cut short by ctx is neither: it says nothing about the data.
func (r *Reconciler) Reconcile(ctx context.Context, envID string) error {
	s := r.state(envID)
	s.run.Lock()
	defer s.run.Unlock()

	if err := r.reconcile(ctx, envID, s); err != nil {
		if ctx.Err() == nil {
			r.recordFailure(envID, err, r.clock.Now())
		}
		return err
	}
	r.clearFailure(envID)
	return nil
}

// reconcile is one render of the environment, with the environment's run lock held. It writes the stored bundle
// only after the render succeeded.
func (r *Reconciler) reconcile(ctx context.Context, envID string, s *envState) error {
	snap, err := r.d.Model.Snapshot(ctx, envID)
	if err != nil {
		return &readError{err}
	}
	if !snap.Settings.Exists && len(snap.Claims) == 0 {
		// No ingress row and no claim: the environment was removed (its row and claims cascaded away) or
		// never had ingress. It holds no bundle (Pitfall 12).
		r.drop(envID, s)
		return nil
	}
	hosts := make([]string, 0, len(snap.Claims))
	for _, c := range snap.Claims {
		hosts = append(hosts, c.Hostname)
	}
	certsVersion, serving, err := r.d.Certs.Serving(ctx, envID, hosts, r.clock.Now())
	if err != nil {
		return &readError{err}
	}
	certs := make([]ingressrender.HostCert, 0, len(serving))
	for _, c := range serving {
		certs = append(certs, ingressrender.HostCert{
			Hostname: c.Hostname, ChainPEM: c.ChainPEM, KeyPEM: c.KeyPEM, Reason: ingressrender.SkipReason(c.Reason),
		})
	}
	in := ingressrender.Input{Snapshot: snap, Certs: certs, CertsVersion: certsVersion}
	if self, own, ok := r.self(ctx, envID); ok {
		in.Self, in.OwnVersion = &self, own
	}
	res, err := r.render(in)
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
	s.config, s.certs, s.own = snap.Settings.ConfigVersion, certsVersion, in.OwnVersion
	selfRefused := ""
	if res.SelfRefused != nil {
		selfRefused = res.SelfRefused.Error()
	}
	logSelf := selfRefused != "" && selfRefused != s.selfRefused
	s.selfRefused = selfRefused
	s.mu.Unlock()
	if logSelf {
		// The stack routes are rendered all the same (WR-09); only TierPlan's own route and carve-out wait.
		r.log.Warn("TierPlan's own route and HTTP-01 carve-out are left out of the environment's bundle: TierPlan's own input cannot be rendered",
			"environment", envID, "code", res.SelfRefused.Code, "field", res.SelfRefused.Field)
	}

	if changed {
		r.changes.Notify(envID)
	}
	if r.d.OnRender != nil {
		r.d.OnRender(envID, res.Bundle.ID, changed)
	}
	return nil
}

// self is Deps.Self for the environment, or nothing when it is unset.
func (r *Reconciler) self(ctx context.Context, envID string) (ingressrender.SelfInput, int64, bool) {
	if r.d.Self == nil {
		return ingressrender.SelfInput{}, 0, false
	}
	return r.d.Self(ctx, envID)
}

// drop forgets the environment's bundle and tells Changes, but only if there was one.
func (r *Reconciler) drop(envID string, s *envState) {
	s.mu.Lock()
	existed := s.have
	s.bundle, s.have, s.skipped, s.config, s.certs, s.own = ingressbundle.Bundle{}, false, nil, 0, 0, 0
	s.mu.Unlock()
	if existed {
		r.changes.Notify(envID)
	}
}

// Run renders every environment, then keeps them current until ctx ends: a wake-up from the notifier arms
// the debounce window if it is not armed, and when the window fires every environment that changed is
// reconciled. Two more timers run beside it, both armed on the injected clock and re-armed after each fire: every
// PollInterval the environments whose version pair moved since their last render are reconciled (D-26), and every
// SafetyTick every environment is (D-19). It subscribes before it renders, so a change during the first pass is
// not lost.
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

	poll := r.clock.After(PollInterval)
	tick := r.clock.After(SafetyTick)
	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			if debounce == nil {
				debounce = r.clock.After(Debounce)
			}
		case <-poll:
			r.pollVersions(ctx)
			poll = r.clock.After(PollInterval)
		case <-tick:
			r.renderAll(ctx)
			tick = r.clock.After(SafetyTick)
		case <-debounce:
			debounce = nil
			if pending == nil {
				continue
			}
			r.reconcileEach(ctx, pending())
		}
	}
}

// renderAll renders every environment Versions lists and every environment the reconciler holds a bundle or a
// failure for (one that was removed is in the second set only, and its render drops the bundle and the failure). A
// read error is logged and the tracked environments are still rendered; the notifier and the next poll bring the
// others in.
func (r *Reconciler) renderAll(ctx context.Context) {
	versions, err := r.d.Model.Versions(ctx)
	if err != nil {
		r.log.Error("the ingress reconciler could not list the environments", "error", err)
	}
	r.reconcileEach(ctx, r.union(versions))
}

// pollVersions reconciles the environments whose version pair, or own version (read through Deps.Self, A-02), differs
// from what their bundle was last rendered at, those with no bundle yet, and those holding a bundle or a failure that Versions no longer lists. A read error
// is logged and the caller re-arms the timer, so a failing read is never a tight loop (T-21-38).
func (r *Reconciler) pollVersions(ctx context.Context) {
	versions, err := r.d.Model.Versions(ctx)
	if err != nil {
		r.log.Error("the ingress reconciler could not read the ingress versions", "error", err)
		return
	}
	listed := make(map[string]struct{}, len(versions))
	var due []string
	for _, v := range versions {
		listed[v.EnvID] = struct{}{}
		_, own, ok := r.self(ctx, v.EnvID)
		if !ok {
			own = 0
		}
		if r.moved(v, own) {
			due = append(due, v.EnvID)
		}
	}
	for _, envID := range r.tracked() {
		if _, ok := listed[envID]; !ok {
			due = append(due, envID)
		}
	}
	r.reconcileEach(ctx, due)
}

// moved reports whether the environment has no bundle or its bundle was rendered at another version pair or another
// own version. A change of TierPlan's own certificate moves own alone, with the config and certs versions unchanged.
func (r *Reconciler) moved(v ingress.EnvVersion, own int64) bool {
	r.mu.Lock()
	s, ok := r.envs[v.EnvID]
	r.mu.Unlock()
	if !ok {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.have || s.config != v.ConfigVersion || s.certs != v.CertsVersion || s.own != own
}

// tracked lists the environments the reconciler holds a bundle or a failure for. An environment whose renders have
// only ever failed holds no bundle, so without its failure it would never be visited again once Versions stops
// listing it, and the failure would stand in Failures until a restart (phase 21 review, WR-01).
func (r *Reconciler) tracked() []string {
	r.mu.Lock()
	states := make(map[string]*envState, len(r.envs))
	for id, s := range r.envs {
		states[id] = s
	}
	r.mu.Unlock()
	var out []string
	for id, s := range states {
		s.mu.Lock()
		if s.have {
			out = append(out, id)
		}
		s.mu.Unlock()
	}
	r.fmu.Lock()
	for id := range r.failures {
		out = append(out, id)
	}
	r.fmu.Unlock()
	slices.Sort(out)
	return slices.Compact(out)
}

// union is the environments in versions and those holding a bundle or a failure, each once.
func (r *Reconciler) union(versions []ingress.EnvVersion) []string {
	out := r.tracked()
	for _, v := range versions {
		out = append(out, v.EnvID)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// reconcileEach reconciles each environment and goes on to the next after a failure. Reconcile records and logs a
// failure itself, once per distinct cause, so a failure that the poll meets every two seconds is not logged here.
func (r *Reconciler) reconcileEach(ctx context.Context, envIDs []string) {
	for _, envID := range envIDs {
		if ctx.Err() != nil {
			return
		}
		_ = r.Reconcile(ctx, envID)
	}
}

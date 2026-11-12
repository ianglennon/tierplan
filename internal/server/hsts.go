// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// hstsKind is what the policy does with Strict-Transport-Security.
type hstsKind uint8

const (
	// hstsNone sends nothing at all.
	hstsNone hstsKind = iota
	// hstsSend sends a positive max-age, on a request for the verified public host only.
	hstsSend
	// hstsWithdraw sends max-age=0 on every TLS response, which clears a policy a browser holds.
	hstsWithdraw
)

// hstsDecision is what every TLS response carries until the next recompute. It is immutable once
// stored: the request path reads it through an atomic pointer and takes no lock (T-16-30).
type hstsDecision struct {
	kind   hstsKind
	maxAge int64
	// host is the verified public host a positive max-age is sent for.
	host string
	// reason says why the policy is not sending, in the words the status reports (D-10).
	reason string
	// step is the ramp step in force, 0 when the policy is not sending.
	step int
	// The rest is what the status reports about the decision (D-11, D-12, D-14, D-22, D-23). enabled is
	// the effective toggle the decision was made with and source the kind of certificate it judged.
	enabled bool
	source  tlsKind
	// limit is the last ramp step this certificate may reach, and since the instant (unix nanoseconds)
	// the step in force began. Both are set only while sending.
	limit int
	since int64
	// notAfter is the served leaf's end, which the withdrawal is measured from.
	notAfter time.Time
}

// hstsPolicy decides Strict-Transport-Security (TLS-06). It is off by default, sends only over a
// certificate that verifies for the public host, and never sends includeSubDomains or preload (D-13).
// The decision is made off the request path, on install, on a tick and on a setting change, and
// published as an atomic snapshot that Header reads.
type hstsPolicy struct {
	// now is the ACME clock's Now, shared with the certificates and the trust verdict.
	now   func() time.Time
	trust *trustChecker
	rt    *httpsRuntime
	// env is TIERPLAN_HSTS: nil when unset, otherwise the toggle.
	env *bool
	db  hstsStore
	// storedOn is the stored toggle as the last read or write saw it. It is atomic so Enabled, which
	// the Let's Encrypt manager asks from another goroutine, takes no lock.
	storedOn atomic.Bool
	// clock is the clock the periodic loop waits on. It is separate from now's clock because an
	// extra timer on the ACME clock breaks the rigs' armed-timer counting.
	clock stackops.Clock
	log   *slog.Logger

	snap atomic.Pointer[hstsDecision]
	wake chan struct{}

	// mu serialises Recompute, which runs off the request path, and guards what follows.
	mu sync.Mutex
	// loaded is whether the ramp has been read from the database. The ramp in memory is the truth
	// from then on; saved is the part of it the database is known to hold, so a failed write is
	// retried by the next recompute.
	loaded bool
	ramp   hstsRamp
	saved  hstsRamp
	// writeWarned is whether a failed write has been logged since the last write that worked.
	writeWarned bool
}

// hstsStore is the database the policy keeps its toggle and its ramp in.
type hstsStore interface {
	GetHTTPSState(ctx context.Context) (sqlc.HttpsState, error)
	SaveHSTSRamp(ctx context.Context, arg sqlc.SaveHSTSRampParams) error
	SaveHSTSHeldUntil(ctx context.Context, arg sqlc.SaveHSTSHeldUntilParams) error
	SetHSTSEnabled(ctx context.Context, arg sqlc.SetHSTSEnabledParams) error
}

// hstsDB reads from the read pool and writes through the write pool.
type hstsDB struct{ r, w *sqlc.Queries }

func (d hstsDB) GetHTTPSState(ctx context.Context) (sqlc.HttpsState, error) {
	return d.r.GetHTTPSState(ctx)
}

func (d hstsDB) SaveHSTSRamp(ctx context.Context, arg sqlc.SaveHSTSRampParams) error {
	return d.w.SaveHSTSRamp(ctx, arg)
}

func (d hstsDB) SaveHSTSHeldUntil(ctx context.Context, arg sqlc.SaveHSTSHeldUntilParams) error {
	return d.w.SaveHSTSHeldUntil(ctx, arg)
}

func (d hstsDB) SetHSTSEnabled(ctx context.Context, arg sqlc.SetHSTSEnabledParams) error {
	return d.w.SetHSTSEnabled(ctx, arg)
}

// newHSTSPolicy returns a policy that decides nothing until the first Recompute.
func newHSTSPolicy(now func() time.Time, trust *trustChecker, rt *httpsRuntime, env *bool, clock stackops.Clock, db hstsStore, log *slog.Logger) *hstsPolicy {
	if clock == nil {
		clock = stackops.RealClock()
	}
	if log == nil {
		log = slog.Default()
	}
	return &hstsPolicy{now: now, trust: trust, rt: rt, env: env, db: db, clock: clock, log: log, wake: make(chan struct{}, 1)}
}

// Header is the router's hook: the value of Strict-Transport-Security for this request, or "". It
// reads the snapshot and the installed kind only: no database, no clock and no lock.
func (p *hstsPolicy) Header(r *http.Request) string {
	if r.TLS == nil {
		return ""
	}
	d := p.snap.Load()
	if d == nil {
		return ""
	}
	// The decision judged one kind of certificate and the recompute that follows a swap is
	// asynchronous. A decision made for another kind than the one installed now is stale, and nothing
	// HSTS-related is ever sent over a self-signed certificate (D-14). The comparison is one atomic load.
	if p.rt.Kind() != d.source {
		return ""
	}
	switch d.kind {
	case hstsWithdraw:
		return "max-age=0"
	case hstsSend:
		// The Host header is the client's. A positive policy is for the verified public host and no
		// other name (T-16-29).
		if d.host != "" && requestHost(r.Host) == d.host {
			return "max-age=" + strconv.FormatInt(d.maxAge, 10)
		}
	}
	return ""
}

// requestHost is the host of a Host header, lower-cased and without its port.
func requestHost(hostport string) string {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	return strings.ToLower(strings.Trim(h, "[]"))
}

// Wake asks the loop to recompute at once. It never blocks.
func (p *hstsPolicy) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Run recomputes on every wake and once a minute of its own clock, until ctx ends.
func (p *hstsPolicy) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-p.clock.After(time.Minute):
		}
		p.Recompute(ctx)
	}
}

// enabled is the effective toggle: the variable when set, else the stored column.
func (p *hstsPolicy) enabled() bool {
	if p.env != nil {
		return *p.env
	}
	return p.storedOn.Load()
}

// Enabled is the effective HSTS setting, for the callers that must not act while it is on. It takes no
// lock and does no I/O.
func (p *hstsPolicy) Enabled() bool { return p.enabled() }

// hstsSteps are the max-age values of the ramp in seconds (D-11), after hstspreload.org's guidance.
// hstsDwell[i] is how long step i+1 must have been sent before step i+2 is.
var (
	hstsSteps = [...]int64{300, 604800, 2592000, 31536000}
	hstsDwell = [...]time.Duration{24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour}
)

// hstsRamp is the persisted progress of the ramp. Instants are unix nanoseconds.
type hstsRamp struct {
	host  string
	step  int
	since int64
	// held is the instant until which a browser may still hold a policy TierPlan sent. It is never
	// lowered, and a reset of the ramp leaves it alone (D-26).
	held int64
}

// hstsHeldSlack is added when the held-until bound is moved, so it is rewritten at most once an hour.
const hstsHeldSlack = time.Hour

// hstsWindow is the longest withdrawal window (D-12).
const hstsWindow = 7 * 24 * time.Hour

// withdrawWindow is how long before a certificate's NotAfter HSTS is withdrawn: a week, or a third
// of the lifetime for a certificate shorter than three weeks, so a short-lived certificate is not
// permanently inside it (D-12, D-22). The renewal scheduler renews at two thirds of the lifetime, or
// half for a short one, so a certificate in the window has missed its renewal. The division is in
// integer nanoseconds and truncates.
func withdrawWindow(lifetime time.Duration) time.Duration {
	return min(hstsWindow, lifetime/3)
}

// Recompute reads the served certificate and the trust verdict, moves the ramp and publishes the
// decision. A database that cannot be read or written does not change the answer: the ramp in memory
// carries on and the failure is logged once.
func (p *hstsPolicy) Recompute(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.load(ctx)
	now := p.now()
	kind, chain := p.rt.servedChain()
	in := hstsInput{kind: kind, verdict: p.trust.verdictFor(kind, chain), enabled: p.enabled(), now: now}
	if len(chain) > 0 {
		in.notBefore, in.notAfter = chain[0].NotBefore, chain[0].NotAfter
	}
	d, ramp := hstsDecide(in, p.ramp)
	p.ramp = ramp
	p.snap.Store(&d)
	p.persist(ctx, now)
}

// load reads the stored toggle every time and the ramp the first time it can.
func (p *hstsPolicy) load(ctx context.Context) {
	row, err := p.db.GetHTTPSState(ctx)
	if err != nil {
		p.warn("read the HSTS state", err)
		return
	}
	p.storedOn.Store(row.HstsEnabled != 0)
	if p.loaded {
		return
	}
	p.loaded = true
	p.ramp = hstsRamp{host: row.HstsHost, step: int(row.HstsStep), since: row.HstsStepSince, held: row.HstsHeldUntil}
	p.saved = p.ramp
}

// persist writes the ramp, and the held-until bound, when they differ from what the database holds.
func (p *hstsPolicy) persist(ctx context.Context, now time.Time) {
	failed := false
	if p.ramp.host != p.saved.host || p.ramp.step != p.saved.step || p.ramp.since != p.saved.since {
		err := p.db.SaveHSTSRamp(ctx, sqlc.SaveHSTSRampParams{
			HstsHost: p.ramp.host, HstsStep: int64(p.ramp.step), HstsStepSince: p.ramp.since, UpdatedAt: now.UnixNano(),
		})
		if err != nil {
			p.warn("save the HSTS ramp", err)
			failed = true
		} else {
			p.saved.host, p.saved.step, p.saved.since = p.ramp.host, p.ramp.step, p.ramp.since
		}
	}
	if p.ramp.held != p.saved.held {
		err := p.db.SaveHSTSHeldUntil(ctx, sqlc.SaveHSTSHeldUntilParams{HstsHeldUntil: p.ramp.held, UpdatedAt: now.UnixNano()})
		if err != nil {
			p.warn("save the HSTS held-until bound", err)
			failed = true
		} else {
			p.saved.held = p.ramp.held
		}
	}
	if !failed {
		p.writeWarned = false
	}
}

// warn logs a database failure once, until a write works again.
func (p *hstsPolicy) warn(what string, err error) {
	if p.writeWarned {
		return
	}
	p.writeWarned = true
	p.log.Warn("could not "+what+"; the HSTS decision goes on from memory", "error", err)
}

// hstsInput is everything one decision depends on.
type hstsInput struct {
	kind    tlsKind
	verdict trustVerdict
	enabled bool
	now     time.Time
	// notBefore and notAfter are the served leaf's validity.
	notBefore, notAfter time.Time
}

// hstsDecide is the decision table (D-10, D-12, D-14, D-22, D-23, D-25) and the ramp. Nothing is sent
// over the self-signed certificate or an unverified Let's Encrypt certificate. A certificate-files
// certificate that does not verify gets max-age=0, so a browser that trusts its CA drops the one-year
// header earlier versions sent. A verified certificate gets max-age=0 while the toggle is off and
// inside its withdrawal window, and otherwise a positive max-age for the verified host. Every decision
// that is not a send starts the ramp over, and so does a new verified host; a send moves at most one
// step, a step begins when it is entered, and a certificate that lives under ten days stops at the
// one-week step.
func hstsDecide(in hstsInput, ramp hstsRamp) (hstsDecision, hstsRamp) {
	v := in.verdict
	lifetime := in.notAfter.Sub(in.notBefore)
	d := hstsDecision{enabled: in.enabled, source: in.kind, notAfter: in.notAfter}
	switch {
	case in.kind == 0 || !v.Checked:
	case in.kind == tlsKindSelfSigned:
		d.reason = trustSelfSigned
	case !v.Verified:
		d.reason = v.Reason
		if in.kind == tlsKindSupplied {
			d.kind = hstsWithdraw
		}
	case !in.enabled:
		d.kind, d.reason = hstsWithdraw, "toggle_off"
	case !in.now.Before(in.notAfter.Add(-withdrawWindow(lifetime))):
		d.kind, d.reason = hstsWithdraw, "withdrawn"
	default:
		d.kind, d.host = hstsSend, v.Host
	}
	if d.kind != hstsSend {
		return d, hstsRamp{held: ramp.held}
	}

	limit := len(hstsSteps)
	if lifetime < acme.ShortLifetime {
		limit = 2
	}
	now := in.now.UnixNano()
	switch {
	case ramp.step < 1 || ramp.host != v.Host:
		ramp = hstsRamp{host: v.Host, step: 1, since: now, held: ramp.held}
	case ramp.step > limit:
		ramp.step = limit
	case ramp.step < limit && in.now.Sub(time.Unix(0, ramp.since)) >= hstsDwell[ramp.step-1]:
		ramp.step++
		ramp.since = now
	}
	if limit < len(hstsSteps) && ramp.step == limit {
		d.reason = "capped"
	}
	d.step, d.maxAge, d.limit, d.since = ramp.step, hstsSteps[ramp.step-1], limit, ramp.since
	if need := now + d.maxAge*int64(time.Second); ramp.held < need {
		ramp.held = need + int64(hstsHeldSlack)
	}
	return d, ramp
}

// ErrHSTSLocked is what SetEnabled returns while TIERPLAN_HSTS fixes the toggle and the request asks
// for the other value (D-10, D-17).
type ErrHSTSLocked = api.ErrHSTSLocked

// SetEnabled stores the admin's choice and recomputes before it returns, so the next TLS response
// carries the new decision. TIERPLAN_HSTS decides when it is set: its own value is accepted and writes
// nothing, any other is refused with ErrHSTSLocked before anything is written (D-10, D-17).
func (p *hstsPolicy) SetEnabled(ctx context.Context, on bool) error {
	if p.env != nil {
		if *p.env == on {
			return nil
		}
		return &ErrHSTSLocked{Var: config.EnvHSTS}
	}
	var v int64
	if on {
		v = 1
	}
	if err := p.db.SetHSTSEnabled(ctx, sqlc.SetHSTSEnabledParams{HstsEnabled: v, UpdatedAt: p.now().UnixNano()}); err != nil {
		return err
	}
	p.storedOn.Store(on)
	p.log.Info("HSTS setting changed", "enabled", on)
	p.Recompute(ctx)
	return nil
}

// View describes the header the last decision sends, with the facts the HSTS card shows. It reads the
// decision and the ramp as they were left by the last recompute, which every change of the setting
// forces.
func (p *hstsPolicy) View(_ context.Context) api.HSTSView {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	v := api.HSTSView{Enabled: p.enabled(), StoredEnabled: p.storedOn.Load()}
	if p.env != nil {
		v.Lock = &api.HTTPSLockBody{Var: config.EnvHSTS, Value: onOff(*p.env), Stored: onOff(p.storedOn.Load())}
	}
	d := p.snap.Load()
	if d == nil {
		d = &hstsDecision{enabled: v.Enabled}
	}
	switch {
	case d.kind == hstsSend:
		v.State, v.MaxAge, v.Step = api.HSTSStateSending, d.maxAge, d.step
		v.Capped = d.reason == "capped"
		if d.step < d.limit {
			v.NextStepAt = time.Unix(0, d.since).Add(hstsDwell[d.step-1])
		}
	case !d.enabled:
		v.State = api.HSTSStateOffSilent
		if d.kind == hstsWithdraw {
			v.State, v.Clearing = api.HSTSStateOffClearing, true
		}
	case d.kind == hstsWithdraw && d.reason == "withdrawn":
		v.State, v.Clearing = api.HSTSStateWithdrawn, true
		v.Withdrawal = &api.HSTSWithdrawal{NotAfter: d.notAfter, Source: withdrawalSource(d.source)}
	default:
		v.State, v.Clearing, v.NotSentReason = api.HSTSStateNotSent, d.kind == hstsWithdraw, d.reason
	}
	// What a browser may still hold is bounded by the persisted held-until, whichever way the decision
	// went since: while a policy is being sent the bound moves with it and is not an answer.
	if d.kind != hstsSend && p.ramp.held > now.UnixNano() {
		v.ForgetBy = time.Unix(0, p.ramp.held)
	}
	return v
}

// onOff is a toggle in the words of the lock the page shows.
func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// withdrawalSource names the certificate a withdrawal is measured from.
func withdrawalSource(k tlsKind) string {
	if k == tlsKindACME {
		return api.SourceLetsEncrypt
	}
	return api.SourceCertFiles
}

// hstsAPI is the policy as the HTTPS routes use it.
type hstsAPI struct{ p *hstsPolicy }

func (a hstsAPI) HSTSView(ctx context.Context) (api.HSTSView, error) { return a.p.View(ctx), nil }

func (a hstsAPI) SetHSTSEnabled(ctx context.Context, on bool) error { return a.p.SetEnabled(ctx, on) }

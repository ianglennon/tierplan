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

	"github.com/ianglennon/tierplan/internal/stackops"
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
	// clock is the clock the periodic loop waits on. It is separate from now's clock because an
	// extra timer on the ACME clock breaks the rigs' armed-timer counting.
	clock stackops.Clock
	log   *slog.Logger

	snap atomic.Pointer[hstsDecision]
	wake chan struct{}
	// mu serialises Recompute, which runs off the request path.
	mu sync.Mutex
}

// newHSTSPolicy returns a policy that decides nothing until the first Recompute.
func newHSTSPolicy(now func() time.Time, trust *trustChecker, rt *httpsRuntime, env *bool, clock stackops.Clock, log *slog.Logger) *hstsPolicy {
	if clock == nil {
		clock = stackops.RealClock()
	}
	if log == nil {
		log = slog.Default()
	}
	return &hstsPolicy{now: now, trust: trust, rt: rt, env: env, clock: clock, log: log, wake: make(chan struct{}, 1)}
}

// Header is the router's hook: the value of Strict-Transport-Security for this request, or "". It
// reads the snapshot only: no database, no clock and no lock.
func (p *hstsPolicy) Header(r *http.Request) string {
	if r.TLS == nil {
		return ""
	}
	d := p.snap.Load()
	if d == nil {
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

// enabled is the effective toggle: the variable when set.
func (p *hstsPolicy) enabled() bool {
	if p.env != nil {
		return *p.env
	}
	return false
}

// Recompute reads the served certificate and the trust verdict and publishes the decision.
func (p *hstsPolicy) Recompute(_ context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	kind, chain := p.rt.servedChain()
	d := hstsDecide(kind, p.trust.verdictFor(kind, chain), p.enabled())
	p.snap.Store(&d)
}

// hstsFirstStep is the first max-age, in seconds.
const hstsFirstStep int64 = 300

// hstsDecide is the decision table (D-10, D-14, D-23, D-25). Nothing is sent over the self-signed
// certificate or an unverified Let's Encrypt certificate. A certificate-files certificate that does
// not verify gets max-age=0, so a browser that trusts its CA drops the one-year header earlier
// versions sent. A verified certificate gets max-age=0 while the toggle is off, and a positive
// max-age for the verified host while it is on.
func hstsDecide(kind tlsKind, v trustVerdict, enabled bool) hstsDecision {
	switch {
	case kind == 0 || !v.Checked:
		return hstsDecision{}
	case kind == tlsKindSelfSigned:
		return hstsDecision{reason: trustSelfSigned}
	case !v.Verified:
		if kind == tlsKindSupplied {
			return hstsDecision{kind: hstsWithdraw, reason: v.Reason}
		}
		return hstsDecision{reason: v.Reason}
	case !enabled:
		return hstsDecision{kind: hstsWithdraw, reason: "toggle_off"}
	}
	return hstsDecision{kind: hstsSend, maxAge: hstsFirstStep, host: v.Host}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
	"golang.org/x/time/rate"
)

const (
	// hostConcurrency is how many requests one registry host has in flight from the checker.
	hostConcurrency = 4
	// hubRate and otherRate are the requests a second the checker makes to Docker Hub and to any other
	// registry. Hub is slower because anonymous callers share one small allowance (T-12-11).
	hubRate   = 5
	otherRate = 20
	// breakerBase is how long a host's breaker opens the first time a registry says "too many requests",
	// doubling on each consecutive trip up to breakerMax.
	breakerBase = 15 * time.Minute
	breakerMax  = 2 * time.Hour
)

// errBreakerOpen is the error of a request not made because the host's breaker is open. It is stored as
// a rate-limited check: nothing is known about the tag, and the registry has asked to be left alone.
var errBreakerOpen = errors.New("updates: the registry asked to be left alone")

// hostPacer keeps the checker polite to each registry host: a cap on requests in flight, a rate, and a
// breaker that opens when the registry answers "too many requests" and stays open for a while, so the
// rest of the host's keys wait for the next check instead of making the limit worse. The state is per
// host: one registry's limit never slows another.
type hostPacer struct {
	// now is the clock the breaker reads; wait is how a request waits for its turn at the rate. Both can
	// be replaced so a test needs no sleep.
	now  func() time.Time
	wait func(ctx context.Context, l *rate.Limiter) error

	mu    sync.Mutex
	hosts map[string]*pacedHost
}

type pacedHost struct {
	sem *semaphore.Weighted
	lim *rate.Limiter

	mu sync.Mutex
	// until is when the breaker closes, zero when it is closed; trips counts the consecutive trips
	// since the last success, which sets how long the next one lasts.
	until time.Time
	trips int
}

func newHostPacer(now func() time.Time) *hostPacer {
	return &hostPacer{
		now:   now,
		wait:  func(ctx context.Context, l *rate.Limiter) error { return l.Wait(ctx) },
		hosts: map[string]*pacedHost{},
	}
}

// isHub reports whether host is Docker Hub, by either name its images are asked for.
func isHub(host string) bool {
	h := strings.ToLower(host)
	return h == "docker.io" || h == "registry-1.docker.io" || h == "index.docker.io"
}

func (p *hostPacer) host(name string) *pacedHost {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.hosts[name]
	if !ok {
		r := otherRate
		if isHub(name) {
			r = hubRate
		}
		h = &pacedHost{sem: semaphore.NewWeighted(hostConcurrency), lim: rate.NewLimiter(rate.Limit(r), r)}
		p.hosts[name] = h
	}
	return h
}

// open reports whether the breaker is open at now.
func (h *pacedHost) open(now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return now.Before(h.until)
}

// report feeds the outcome of a request to the breaker. A success closes it and forgets the trips. An
// answer of "too many requests" opens it, once per opening: requests already in flight when it opened
// answer the same limit and must not lengthen it. Any other failure says nothing about the limit.
func (h *pacedHost) report(now time.Time, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case err == nil:
		h.until, h.trips = time.Time{}, 0
	case reasonFor(err) == ReasonRateLimited && !errors.Is(err, errBreakerOpen):
		if now.Before(h.until) {
			return
		}
		d := breakerBase
		for i := 0; i < h.trips && d < breakerMax; i++ {
			d *= 2
		}
		h.until = now.Add(min(d, breakerMax))
		h.trips++
	}
}

// Do runs f, one request to host, when the host has a place free, the rate allows it and the breaker is
// closed; with the breaker open it returns errBreakerOpen without calling f. The outcome of f feeds the
// breaker before the place is given up, so the requests queued behind it meet the breaker it opened.
func (p *hostPacer) Do(ctx context.Context, host string, f func(context.Context) error) error {
	h := p.host(host)
	if err := h.sem.Acquire(ctx, 1); err != nil {
		return err
	}
	defer h.sem.Release(1)
	if h.open(p.now()) {
		return errBreakerOpen
	}
	if err := p.wait(ctx, h.lim); err != nil {
		return err
	}
	if h.open(p.now()) {
		return errBreakerOpen
	}
	err := f(ctx)
	if ctx.Err() == nil {
		h.report(p.now(), err)
	}
	return err
}

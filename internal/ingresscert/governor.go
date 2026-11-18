// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"fmt"
	"slices"
	"time"
)

// The limits TierPlan keeps itself inside (D-14, D-15), each a named constant. The CA's own figure is the Limit,
// TierPlan stops at the Cap, below it: the difference absorbs the certificates other tools issue under the same
// registered domain, which TierPlan cannot see.
const (
	// NewCertsCap is the most new production certificates TierPlan starts per registered domain in Window.
	NewCertsCap = 40
	// NewCertsLimit is Let's Encrypt's limit on new certificates per registered domain per week.
	NewCertsLimit = 50
	// StagingNewCertsCap and StagingNewCertsLimit are the same for the staging directory (80 percent of its limit).
	StagingNewCertsCap   = 24000
	StagingNewCertsLimit = 30000
	// DuplicateCap is the most certificates TierPlan issues for one hostname per issuer in Window (D-15).
	DuplicateCap = 4
	// DuplicateLimit is the CA's limit on certificates for one exact set of names per week.
	DuplicateLimit = 5
	// Window is the rolling period the CA counts its weekly limits over.
	Window = 7 * 24 * time.Hour
	// OrderStartsCap is the most orders the account of one production directory starts in OrderStartsWindow.
	OrderStartsCap = 240
	// OrderStartsLimit is the CA's limit on new orders per account in OrderStartsWindow.
	OrderStartsLimit = 300
	// StagingOrderStartsCap and StagingOrderStartsLimit are the same for the staging directory.
	StagingOrderStartsCap   = 1200
	StagingOrderStartsLimit = 1500
	// OrderStartsWindow is the period the order starts are counted over.
	OrderStartsWindow = 3 * time.Hour
	// IssuanceRetention is how long a row of the issuance ledger is kept: a day more than Window, so a row that
	// has just left the window is still there when the clock of another process is a little behind.
	IssuanceRetention = 8 * 24 * time.Hour
)

// The queue codes the governor writes to a row (D-17), beside QueueNoAccount, which the run writes (D-36).
const (
	// QueueRateLimitNew: the registered domain has used its weekly budget of new certificates.
	QueueRateLimitNew = "rate_limit_new"
	// QueueRateLimitDuplicate: the hostname has been issued DuplicateCap times in Window.
	QueueRateLimitDuplicate = "rate_limit_duplicate"
	// QueueRateLimitOrders: the account has started OrderStartsCap orders in OrderStartsWindow.
	QueueRateLimitOrders = "rate_limit_orders"
	// QueueCAHold: the CA answered a new-name order of the registered domain with rateLimited and a Retry-After.
	QueueCAHold = "ca_rate_limited"
	// QueueConcurrency: every one of the MaxConcurrent order slots is taken.
	QueueConcurrency = "concurrency"
)

// caps are the caps and the CA's limits of an issuer.
type caps struct {
	newCap, newLimit       int
	startsCap, startsLimit int
}

// capsOf are the caps of an issuer; one that is not staging takes the production, the stricter, figures.
func capsOf(issuer string) caps {
	if issuer == IssuerStaging {
		return caps{StagingNewCertsCap, StagingNewCertsLimit, StagingOrderStartsCap, StagingOrderStartsLimit}
	}
	return caps{NewCertsCap, NewCertsLimit, OrderStartsCap, OrderStartsLimit}
}

// inWindow returns the instants of issued that fall in (now-window, now], oldest first. An instant exactly window
// old is outside, and one after now (a clock that moved back) is not counted.
func inWindow(now time.Time, issued []time.Time, window time.Duration) []time.Time {
	from := now.Add(-window)
	out := make([]time.Time, 0, len(issued))
	for _, t := range issued {
		if t.After(from) && !t.After(now) {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b time.Time) int { return a.Compare(b) })
	return out
}

// Allow decides whether one more new-name order may start now under a cap. issued are the instants of the
// issuances the budget is counted from and reserved the orders already in flight for it; the window is
// (now-Window, now]. used is what is counted, and when the cap is reached nextSlot is the oldest issuance in the
// window plus Window, computed from the stored instant with no rounding (D-17); it is the zero time when nothing
// in the window will expire (the budget is held by orders in flight only). The whole rule is this function, so
// replacing it with a token-bucket replay later is a change here only (D-38).
func Allow(now time.Time, issued []time.Time, reserved, cap int) (ok bool, used int, nextSlot time.Time) {
	in := inWindow(now, issued, Window)
	used = len(in) + max(reserved, 0)
	if used < cap {
		return true, used, time.Time{}
	}
	if len(in) > 0 {
		nextSlot = in[0].Add(Window)
	}
	return false, used, nextSlot
}

// DecideInput is everything Decide reads, gathered by the caller: Decide itself does no I/O and reads no clock.
type DecideInput struct {
	Now time.Time
	// Renewal is true for a row that holds a chain: it is exempt from the per-domain and the order-start limits.
	Renewal bool
	// Issuer is the row's issuer, and RegisteredDomain the registered domain of its hostname.
	Issuer, RegisteredDomain string
	// DomainIssued are the instants of the new certificates under the registered domain, own certificate
	// included for production; HostIssued those of every certificate of the hostname; OrderStarts those of the
	// orders the issuer's account started. All are for this issuer.
	DomainIssued, HostIssued, OrderStarts []time.Time
	// Reserved is the number of new-name orders in flight under the registered domain for this issuer.
	Reserved int
	// CAHoldUntil is the instant a CA's Retry-After on a new-name order of the registered domain ends; zero for none.
	CAHoldUntil time.Time
}

// Decision is Decide's answer. A refusal carries the code, the figures and the next slot that are written on
// the row; Domain is the registered domain.
type Decision struct {
	OK       bool
	Code     string
	Used     int
	Cap      int
	Limit    int
	Domain   string
	NextSlot time.Time
}

// Decide says whether an order for the row may start now (D-14, D-15). A renewal is exempt from the per-domain
// and the order-start limits, and from a CA hold, but not from the duplicate guard. The first refusal in this
// order is the answer: the duplicate guard, a CA hold, the order-start rate, the weekly budget of new
// certificates.
func Decide(in DecideInput) Decision {
	c := capsOf(in.Issuer)
	refuse := func(code string, used, cap, limit int, next time.Time) Decision {
		return Decision{Code: code, Used: used, Cap: cap, Limit: limit, Domain: in.RegisteredDomain, NextSlot: next}
	}

	host := inWindow(in.Now, in.HostIssued, Window)
	if len(host) >= DuplicateCap {
		return refuse(QueueRateLimitDuplicate, len(host), DuplicateCap, DuplicateLimit, host[0].Add(Window))
	}
	if in.Renewal {
		return Decision{OK: true, Domain: in.RegisteredDomain}
	}
	if in.Now.Before(in.CAHoldUntil) {
		return refuse(QueueCAHold, 0, 0, 0, in.CAHoldUntil)
	}
	if starts := inWindow(in.Now, in.OrderStarts, OrderStartsWindow); len(starts) >= c.startsCap {
		return refuse(QueueRateLimitOrders, len(starts), c.startsCap, c.startsLimit, starts[0].Add(OrderStartsWindow))
	}
	ok, used, next := Allow(in.Now, in.DomainIssued, in.Reserved, c.newCap)
	if !ok {
		return refuse(QueueRateLimitNew, used, c.newCap, c.newLimit, next)
	}
	return Decision{OK: true, Domain: in.RegisteredDomain, Used: used, Cap: c.newCap, Limit: c.newLimit}
}

// QueueReason is the sentence that says why the queued row c waits: one fixed sentence per queue code, built from
// the figures stored on the row. It never carries text of the certificate authority. A row that waits for nothing
// has no reason (""), and a code this version does not know a generic one.
func QueueReason(c Cert) string {
	next := ""
	if !c.QueueNextSlotAt.IsZero() {
		next = "; next slot at " + c.QueueNextSlotAt.UTC().Format("2006-01-02 15:04:05") + " UTC"
	}
	cp := capsOf(c.Issuer)
	switch c.QueueCode {
	case "":
		return ""
	case QueueNoAccount:
		return "no ACME account: accept the terms"
	case QueueRateLimitNew:
		return fmt.Sprintf("rate limit: %d of %d new certificates for %s in 7 days%s", c.QueueUsed, cp.newLimit, c.QueueDomain, next)
	case QueueRateLimitDuplicate:
		return fmt.Sprintf("rate limit: %d of %d certificates for %s in 7 days%s", c.QueueUsed, DuplicateLimit, c.Hostname, next)
	case QueueRateLimitOrders:
		return fmt.Sprintf("rate limit: %d of %d orders for the account in 3 hours%s", c.QueueUsed, cp.startsLimit, next)
	case QueueCAHold:
		return fmt.Sprintf("the certificate authority asked TierPlan to wait before ordering new certificates for %s%s", c.QueueDomain, next)
	case QueueConcurrency:
		return fmt.Sprintf("waiting for a free slot: %d certificates are being ordered", MaxConcurrent)
	default:
		return "waiting to be ordered"
	}
}

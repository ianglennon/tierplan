// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"strings"
	"testing"
	"time"
)

// agesAgo returns n instants, each a second apart, the oldest (the first) exactly oldest before now and the others
// spread after it, all strictly inside the window when oldest is.
func agesAgo(now time.Time, n int, oldest time.Duration) []time.Time {
	out := make([]time.Time, 0, n)
	for i := range n {
		out = append(out, now.Add(-oldest+time.Duration(i)*time.Second))
	}
	return out
}

// D-14, D-15, D-17, CRT-06: the governor is a pure function over what it is given. Each row names the rule it
// pins: the 40 and 39 boundary, the instant exactly a window old, the next slot with no rounding, reserved orders,
// staging, renewals, the duplicate guard, the order-start rate and a CA's hold.
func TestGovernorDecide(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	oldest := now.Add(-6 * day)
	manyStaging := agesAgo(now, StagingNewCertsCap, 6*day)

	cases := []struct {
		name string
		in   DecideInput
		want Decision
	}{
		{
			"39 in the window lets the 40th start",
			DecideInput{Issuer: IssuerProduction, DomainIssued: agesAgo(now, 39, 6*day)},
			Decision{OK: true, Used: 39, Cap: NewCertsCap, Limit: NewCertsLimit},
		},
		{
			"40 in the window stops the 41st, next slot the oldest plus a window",
			DecideInput{Issuer: IssuerProduction, DomainIssued: agesAgo(now, 40, 6*day)},
			Decision{Code: QueueRateLimitNew, Used: 40, Cap: NewCertsCap, Limit: NewCertsLimit, NextSlot: oldest.Add(Window)},
		},
		{
			"an issuance exactly a window old is outside the window",
			DecideInput{Issuer: IssuerProduction, DomainIssued: append(agesAgo(now, 39, 6*day), now.Add(-Window))},
			Decision{OK: true, Used: 39, Cap: NewCertsCap, Limit: NewCertsLimit},
		},
		{
			"an issuance a nanosecond inside the window counts and the next slot is a nanosecond away, unrounded",
			DecideInput{Issuer: IssuerProduction, DomainIssued: append(agesAgo(now, 39, 6*day), now.Add(-Window+1))},
			Decision{Code: QueueRateLimitNew, Used: 40, Cap: NewCertsCap, Limit: NewCertsLimit, NextSlot: now.Add(1)},
		},
		{
			"an issuance after now (a clock that moved back) is not counted",
			DecideInput{Issuer: IssuerProduction, DomainIssued: append(agesAgo(now, 39, 6*day), now.Add(time.Hour))},
			Decision{OK: true, Used: 39, Cap: NewCertsCap, Limit: NewCertsLimit},
		},
		{
			"39 issued and one order in flight stops the next name",
			DecideInput{Issuer: IssuerProduction, DomainIssued: agesAgo(now, 39, 6*day), Reserved: 1},
			Decision{Code: QueueRateLimitNew, Used: 40, Cap: NewCertsCap, Limit: NewCertsLimit, NextSlot: oldest.Add(Window)},
		},
		{
			"orders in flight alone can fill the budget, with no slot to name",
			DecideInput{Issuer: IssuerProduction, Reserved: NewCertsCap},
			Decision{Code: QueueRateLimitNew, Used: NewCertsCap, Cap: NewCertsCap, Limit: NewCertsLimit},
		},
		{
			"staging with 40 issued allows",
			DecideInput{Issuer: IssuerStaging, DomainIssued: agesAgo(now, 40, 6*day)},
			Decision{OK: true, Used: 40, Cap: StagingNewCertsCap, Limit: StagingNewCertsLimit},
		},
		{
			"staging stops at its own cap",
			DecideInput{Issuer: IssuerStaging, DomainIssued: manyStaging},
			Decision{Code: QueueRateLimitNew, Used: StagingNewCertsCap, Cap: StagingNewCertsCap, Limit: StagingNewCertsLimit, NextSlot: oldest.Add(Window)},
		},
		{
			"a renewal runs with 40 issued",
			DecideInput{Issuer: IssuerProduction, Renewal: true, DomainIssued: agesAgo(now, 40, 6*day), Reserved: 5},
			Decision{OK: true},
		},
		{
			"a renewal is still stopped by the duplicate guard",
			DecideInput{Issuer: IssuerProduction, Renewal: true, HostIssued: agesAgo(now, DuplicateCap, 2*day)},
			Decision{Code: QueueRateLimitDuplicate, Used: DuplicateCap, Cap: DuplicateCap, Limit: DuplicateLimit, NextSlot: now.Add(-2 * day).Add(Window)},
		},
		{
			"3 issuances of the hostname still allow it",
			DecideInput{Issuer: IssuerProduction, HostIssued: agesAgo(now, DuplicateCap-1, 2*day)},
			Decision{OK: true, Cap: NewCertsCap, Limit: NewCertsLimit},
		},
		{
			"the duplicate guard is stated before the weekly budget",
			DecideInput{Issuer: IssuerProduction, HostIssued: agesAgo(now, DuplicateCap, 3*day), DomainIssued: agesAgo(now, 40, 6*day)},
			Decision{Code: QueueRateLimitDuplicate, Used: DuplicateCap, Cap: DuplicateCap, Limit: DuplicateLimit, NextSlot: now.Add(-3 * day).Add(Window)},
		},
		{
			"240 order starts in 3 hours stop a new name",
			DecideInput{Issuer: IssuerProduction, OrderStarts: agesAgo(now, OrderStartsCap, 170*time.Minute)},
			Decision{Code: QueueRateLimitOrders, Used: OrderStartsCap, Cap: OrderStartsCap, Limit: OrderStartsLimit, NextSlot: now.Add(-170 * time.Minute).Add(OrderStartsWindow)},
		},
		{
			"239 order starts allow it, and a start exactly 3 hours old is not counted",
			DecideInput{Issuer: IssuerProduction, OrderStarts: append(agesAgo(now, OrderStartsCap-1, 170*time.Minute), now.Add(-OrderStartsWindow))},
			Decision{OK: true, Cap: NewCertsCap, Limit: NewCertsLimit},
		},
		{
			"staging allows 240 order starts",
			DecideInput{Issuer: IssuerStaging, OrderStarts: agesAgo(now, OrderStartsCap, 170*time.Minute)},
			Decision{OK: true, Cap: StagingNewCertsCap, Limit: StagingNewCertsLimit},
		},
		{
			"a renewal ignores the order-start rate",
			DecideInput{Issuer: IssuerProduction, Renewal: true, OrderStarts: agesAgo(now, 300, 170*time.Minute)},
			Decision{OK: true},
		},
		{
			"a CA hold stops a new name until the hold instant",
			DecideInput{Issuer: IssuerProduction, CAHoldUntil: now.Add(2 * time.Hour)},
			Decision{Code: QueueCAHold, NextSlot: now.Add(2 * time.Hour)},
		},
		{
			"a hold that ends now no longer holds",
			DecideInput{Issuer: IssuerProduction, CAHoldUntil: now},
			Decision{OK: true, Cap: NewCertsCap, Limit: NewCertsLimit},
		},
		{
			"a renewal ignores a CA hold",
			DecideInput{Issuer: IssuerProduction, Renewal: true, CAHoldUntil: now.Add(time.Hour)},
			Decision{OK: true},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := c.in
			in.Now = now
			in.RegisteredDomain = "example.com"
			want := c.want
			want.Domain = "example.com"
			if got := Decide(in); !decisionEqual(got, want) {
				t.Errorf("Decide = %+v, want %+v", got, want)
			}
		})
	}
	if len(cases) < 10 {
		t.Fatalf("%d rows, want at least 10", len(cases))
	}

	// D-17 as locked: the next slot is the oldest issuance in the window plus a window, even when more than the cap
	// is in it; the row is looked at again then and waits again if the budget is still used.
	ok, used, next := Allow(now, agesAgo(now, 45, 6*day), 0, NewCertsCap)
	if ok || used != 45 || !next.Equal(oldest.Add(Window)) {
		t.Errorf("Allow with 45 issued = %v %d %v, want a refusal at 45 and the oldest plus a window %v", ok, used, next, oldest.Add(Window))
	}
	if ok, used, next := Allow(now, nil, 0, NewCertsCap); !ok || used != 0 || !next.IsZero() {
		t.Errorf("Allow with nothing issued = %v %d %v, want allowed, 0, no slot", ok, used, next)
	}
}

func decisionEqual(a, b Decision) bool {
	return a.OK == b.OK && a.Code == b.Code && a.Used == b.Used && a.Cap == b.Cap && a.Limit == b.Limit &&
		a.Domain == b.Domain && a.NextSlot.Equal(b.NextSlot)
}

// D-17, D-36: every queue code has one fixed sentence that is built from the stored figures only. The terms code is
// exactly the sentence the contract names, an unknown code gets the generic one, and nothing the certificate
// authority said can appear in any of them.
func TestQueueReasonForEveryCode(t *testing.T) {
	slot := time.Date(2026, 11, 24, 10, 30, 15, 0, time.UTC)
	const caText = "SECRET-CA-TEXT"
	base := Cert{
		Hostname: "app.example.com", Issuer: IssuerProduction, QueueDomain: "example.com", QueueNextSlotAt: slot,
		LastErrorDetail: caText,
	}
	const when = "; next slot at 2026-11-24 10:30:15 UTC"
	cases := []struct {
		name string
		mod  func(*Cert)
		want string
	}{
		{"no_account", func(c *Cert) { c.QueueCode = QueueNoAccount; c.QueueNextSlotAt = time.Time{} }, "no ACME account: accept the terms"},
		{"no_account ignores a stale slot", func(c *Cert) { c.QueueCode = QueueNoAccount }, "no ACME account: accept the terms"},
		{"rate_limit_new", func(c *Cert) { c.QueueCode = QueueRateLimitNew; c.QueueUsed = 40; c.QueueCap = 40 },
			"rate limit: 40 of 50 new certificates for example.com in 7 days" + when},
		{"rate_limit_new on staging", func(c *Cert) { c.QueueCode = QueueRateLimitNew; c.Issuer = IssuerStaging; c.QueueUsed = 24000 },
			"rate limit: 24000 of 30000 new certificates for example.com in 7 days" + when},
		{"rate_limit_new with no slot", func(c *Cert) { c.QueueCode = QueueRateLimitNew; c.QueueUsed = 40; c.QueueNextSlotAt = time.Time{} },
			"rate limit: 40 of 50 new certificates for example.com in 7 days"},
		{"rate_limit_duplicate", func(c *Cert) { c.QueueCode = QueueRateLimitDuplicate; c.QueueUsed = 4 },
			"rate limit: 4 of 5 certificates for app.example.com in 7 days" + when},
		{"rate_limit_orders", func(c *Cert) { c.QueueCode = QueueRateLimitOrders; c.QueueUsed = 240 },
			"rate limit: 240 of 300 orders for the account in 3 hours" + when},
		{"ca_rate_limited", func(c *Cert) { c.QueueCode = QueueCAHold },
			"the certificate authority asked TierPlan to wait before ordering new certificates for example.com" + when},
		{"concurrency", func(c *Cert) { c.QueueCode = QueueConcurrency; c.QueueNextSlotAt = time.Time{} },
			"waiting for a free slot: 2 certificates are being ordered"},
		{"unknown", func(c *Cert) { c.QueueCode = "from_the_future" }, "waiting to be ordered"},
		{"no code", func(c *Cert) {}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := base
			c.mod(&row)
			got := QueueReason(row)
			if got != c.want {
				t.Errorf("QueueReason = %q, want %q", got, c.want)
			}
			if strings.Contains(got, caText) {
				t.Errorf("QueueReason %q carries the CA's text", got)
			}
		})
	}
}

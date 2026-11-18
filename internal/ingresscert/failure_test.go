// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/acme/dns"
)

// tokenInvalid makes the DNS provider refuse the domain's token on every record it is asked to create.
func (g *mgrRig) tokenInvalid() {
	g.t.Helper()
	g.prov.FailCreate(&dns.Error{Provider: "cloudflare", Op: "create record", Class: dns.ClassTokenInvalid, Status: 403})
}

// failedRound runs a round and waits for the one run it starts to end as a failure.
func (g *mgrRig) failedRound(m *Manager) {
	g.t.Helper()
	m.round(context.Background())
	if end := g.awaitEnd(); end.outcome != OutcomeFailed {
		g.t.Fatalf("the run ended %q, want failed", end.outcome)
	}
}

// ranNothing asserts that a round started no run: the hub holds no more jobs than before, no run ended and the
// CA and the DNS provider saw nothing more.
func (g *mgrRig) ranNothing(m *Manager, jobsBefore, requestsBefore, callsBefore int) {
	g.t.Helper()
	m.round(context.Background())
	if n := len(g.ingressJobs()); n != jobsBefore {
		g.t.Errorf("%d jobs, want still %d: the certificate is backing off", n, jobsBefore)
	}
	if n := len(g.ended); n != 0 {
		g.t.Errorf("%d runs ended during a round that should start none", n)
	}
	if n := g.requests(); n != requestsBefore {
		g.t.Errorf("the CA saw %d requests, want still %d", n, requestsBefore)
	}
	if n := len(g.prov.Calls()); n != callsBefore {
		g.t.Errorf("the DNS provider saw %d calls, want still %d", n, callsBefore)
	}
}

// CRT-05, D-11, the tracer: a DNS-01 order the provider refuses is recorded on its certificate with the class, a
// fixed hint, the failure count, the instants and the next attempt on the published schedule (1 m, 10 m, 100 m,
// then 24 h). No run starts for it before that time, and one starts when it comes.
func TestTracerAFailedOrderIsRecordedAndBacksOff(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	g.claim("app.example.com")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	g.tokenInvalid()
	m := g.newManager()

	var failedAt []time.Time
	for i, wait := range []time.Duration{time.Minute, 10 * time.Minute, 100 * time.Minute, 24 * time.Hour} {
		if i > 0 {
			// Before the next attempt nothing runs; at it, a run does.
			row := g.live("app.example.com")
			g.clock.Advance(row.NextAttemptAt.Sub(g.clock.Now()) / 2)
			g.ranNothing(m, len(g.ingressJobs()), g.requests(), len(g.prov.Calls()))
			g.clock.Advance(row.NextAttemptAt.Sub(g.clock.Now()))
		}
		g.failedRound(m)
		failedAt = append(failedAt, g.clock.Now())

		row := g.live("app.example.com")
		if row.Phase != PhaseQueued || row.LastErrorClass != acme.ClassDNSTokenInvalid || row.ConsecutiveFailures != i+1 {
			t.Fatalf("failure %d: row = phase %q class %q failures %d, want queued dns_token_invalid %d", i+1, row.Phase, row.LastErrorClass, row.ConsecutiveFailures, i+1)
		}
		if want := g.clock.Now().Add(wait); !row.NextAttemptAt.Equal(want) {
			t.Errorf("failure %d: next_attempt_at = %v, want now + %v = %v", i+1, row.NextAttemptAt, wait, want)
		}
		if !row.LastAttemptAt.Equal(g.clock.Now()) || !row.LastErrorAt.Equal(g.clock.Now()) {
			t.Errorf("failure %d: last attempt %v, last error %v, want now %v", i+1, row.LastAttemptAt, row.LastErrorAt, g.clock.Now())
		}
		newest := failedAt[max(0, len(failedAt)-acme.FailureGuardRuns):]
		if len(row.FailureTimes) != len(newest) {
			t.Fatalf("failure %d: failure_times = %v, want the newest %d: %v", i+1, row.FailureTimes, len(newest), newest)
		}
		for j := range newest {
			if !row.FailureTimes[j].Equal(newest[j]) {
				t.Errorf("failure %d: failure_times[%d] = %v, want %v", i+1, j, row.FailureTimes[j], newest[j])
			}
		}
		if len(row.ChainPEM) != 0 || row.KeySealed != "" {
			t.Errorf("failure %d: the row holds a chain or key though nothing was issued", i+1)
		}
		if got := FailureHint(row.LastErrorClass); got == "" || got != FailureHint(acme.ClassDNSTokenInvalid) {
			t.Errorf("failure %d: hint %q", i+1, got)
		}
	}
	if g.fake.Count(acmetest.EndpointNewOrder) == 0 {
		t.Error("the CA saw no order: the failures were not the provider's")
	}
}

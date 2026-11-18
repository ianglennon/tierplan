// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-acme/lego/v5/acme/api"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/audit"
)

// newRenewRig is the manager rig with a fake CA that offers renewal information or, with noARI, does not.
func newRenewRig(t *testing.T, noARI bool) *mgrRig {
	t.Helper()
	g := newMgrRig(t, nil, "")
	g.fake = acmetest.NewFake(t, acmetest.Options{
		Clock: g.clock, ChallengeType: acme.ChallengeDNS01, NoRenewalInfo: noARI,
		LookupTXT: func(fqdn string) []string {
			var out []string
			for _, rec := range g.prov.Records() {
				if strings.EqualFold(rec.Name, fqdn) {
					out = append(out, rec.Value())
				}
			}
			return out
		},
	})
	return g
}

// zeroRand is the Rand of a manager whose random pick is always the start of the window.
func zeroRand(d *Deps) { d.Rand = func(int64) int64 { return 0 } }

// issueOne claims host, acknowledges the terms and runs one round to the end of the first order. It returns the
// manager and the row; the certificate has no renewal clock until the next round.
func (g *mgrRig) issueOne(host string, mod ...func(*Deps)) (*Manager, Cert) {
	g.t.Helper()
	ctx := context.Background()
	g.claim(host)
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		g.t.Fatalf("AcceptTerms: %v", err)
	}
	m := g.newManager(append([]func(*Deps){zeroRand}, mod...)...)
	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeIssued {
		g.t.Fatalf("the first run ended %q, want issued", end.outcome)
	}
	return m, g.live(host)
}

// CRT-04, D-09, the tracer: with no renewal information from the CA an issued certificate is due at half of its
// lifetime. When the clock reaches it a round places a renewal in the same row, the new chain is stored, the
// renewal is audited and the issuance is counted as a renewal.
func TestTracerAnIssuedCertificateRenewsAtHalfLife(t *testing.T) {
	ctx := context.Background()
	g := newRenewRig(t, true)
	m, _ := g.issueOne("app.example.com")
	m.round(ctx) // gives the certificate its renewal clock
	first := g.live("app.example.com")

	wantAt, _ := HalfLifeRenewAt(first.NotBefore, first.NotAfter)
	if !first.RenewAt.Equal(wantAt) || first.RenewBasis != string(acme.BasisHalf) {
		t.Fatalf("renew_at = %v basis %q, want %v half", first.RenewAt, first.RenewBasis, wantAt)
	}
	if want := first.NotBefore.Add(first.NotAfter.Sub(first.NotBefore) / 2); !first.RenewAt.Equal(want) {
		t.Fatalf("renew_at = %v, want half way: %v", first.RenewAt, want)
	}
	if first.Phase != PhaseIdle {
		t.Fatalf("phase = %q before the renewal is due, want idle", first.Phase)
	}
	// A round before the time starts nothing.
	m.round(ctx)
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 1 {
		t.Fatalf("%d orders before the renewal is due, want 1", n)
	}

	g.clock.Advance(first.RenewAt.Sub(g.clock.Now()))
	m.round(ctx)
	end := g.awaitEnd()

	row := g.live("app.example.com")
	if end.certID != first.ID || end.outcome != OutcomeRenewed || row.ID != first.ID {
		t.Fatalf("the run ended %+v, want %s renewed in the same row (row %s)", end, first.ID, row.ID)
	}
	if row.Serial == first.Serial || len(row.ChainPEM) == 0 || row.Phase != PhaseIdle {
		t.Errorf("row = serial %q (was %q) phase %q, want a new serial and idle", row.Serial, first.Serial, row.Phase)
	}
	if n := g.count(`SELECT COUNT(*) FROM ingress_cert_issuances WHERE kind = 'renewal'`); n != 1 {
		t.Errorf("%d renewal issuances, want 1", n)
	}
	if n := g.count(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, audit.ActionIngressCertRenewed); n != 1 {
		t.Errorf("%d ingress.cert_renewed rows, want 1", n)
	}
	if orders := g.fake.Orders(); len(orders) != 2 || orders[1].Replaces != "" {
		t.Errorf("the CA saw %+v, want two orders and no replaces (it offers no renewal information)", orders)
	}
	if !row.RenewAt.IsZero() {
		t.Errorf("renew_at = %v right after a renewal, want it worked out afresh at the next round", row.RenewAt)
	}
	m.round(ctx)
	if again := g.live("app.example.com"); !again.RenewAt.After(g.clock.Now()) {
		t.Errorf("renew_at = %v after the next round, want a time ahead of %v", again.RenewAt, g.clock.Now())
	}
}

// D-09: a certificate lasts 90 days, 64 days or no time at all, and is due at half of it, on the basis half.
func TestHalfLifeRenewAt(t *testing.T) {
	nb := time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	for _, tc := range []struct {
		name     string
		lifetime time.Duration
		want     time.Time
	}{
		{"90 days", 90 * day, nb.Add(45 * day)},
		{"64 days", 64 * day, nb.Add(32 * day)},
		{"6 days", 6 * day, nb.Add(3 * day)},
		{"zero lifetime", 0, nb},
		{"inverted lifetime", -day, nb},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at, basis := HalfLifeRenewAt(nb, nb.Add(tc.lifetime))
			if !at.Equal(tc.want) || basis != acme.BasisHalf {
				t.Errorf("HalfLifeRenewAt = %v %q, want %v %q", at, basis, tc.want, acme.BasisHalf)
			}
		})
	}
}

// D-09: when the CA offers a renewal window, renew_at is picked inside it and kept across polls, the window is
// asked again at the CA's Retry-After clamped between one and twelve hours (six when it gives none), and the
// renewal runs when the picked time comes.
func TestRenewalFollowsTheARIWindow(t *testing.T) {
	ctx := context.Background()
	g := newRenewRig(t, false)
	var picks atomic.Int64
	// Every draw is a later hour, so a pick that was moved by a later poll would show.
	m, first := g.issueOne("app.example.com", func(d *Deps) {
		d.Rand = func(int64) int64 { return picks.Add(1) * int64(time.Hour) }
	})
	day := 24 * time.Hour
	start, end := first.NotBefore.Add(60*day), first.NotBefore.Add(62*day)
	g.fake.SetRenewalWindow(leaf(t, first.ChainPEM), start, end, 3*time.Hour)

	m.round(ctx)
	row := g.live("app.example.com")
	pick := start.Add(time.Hour)
	if !row.RenewAt.Equal(pick) || row.RenewBasis != string(acme.BasisARI) {
		t.Fatalf("renew_at = %v basis %q, want %v ari", row.RenewAt, row.RenewBasis, pick)
	}
	if !row.AriWindowStart.Equal(start) || !row.AriWindowEnd.Equal(end) || !row.AriCheckedAt.Equal(g.clock.Now()) {
		t.Errorf("window %v..%v checked %v, want %v..%v checked now", row.AriWindowStart, row.AriWindowEnd, row.AriCheckedAt, start, end)
	}
	if want := g.clock.Now().Add(3 * time.Hour); !row.AriNextCheckAt.Equal(want) {
		t.Errorf("next check = %v, want %v (the CA's Retry-After)", row.AriNextCheckAt, want)
	}

	// Not yet time to ask again: no call.
	asked := g.fake.Count(acmetest.EndpointRenewalInfo)
	m.round(ctx)
	if n := g.fake.Count(acmetest.EndpointRenewalInfo); n != asked {
		t.Errorf("%d renewal information calls before the next check, want %d", n, asked)
	}

	// A second poll with the same window keeps the picked time, and takes the new Retry-After.
	g.clock.Advance(3 * time.Hour)
	g.fake.SetRenewalWindow(leaf(t, first.ChainPEM), start, end, 30*time.Hour)
	m.round(ctx)
	row = g.live("app.example.com")
	if !row.RenewAt.Equal(pick) {
		t.Errorf("renew_at = %v after a second poll, want it kept at %v", row.RenewAt, pick)
	}
	if want := g.clock.Now().Add(12 * time.Hour); !row.AriNextCheckAt.Equal(want) {
		t.Errorf("next check = %v for a Retry-After of 30h, want it clamped to 12h: %v", row.AriNextCheckAt, want)
	}

	g.clock.Advance(12 * time.Hour)
	g.fake.SetRenewalWindow(leaf(t, first.ChainPEM), start, end, 0)
	m.round(ctx)
	if want, got := g.clock.Now().Add(6*time.Hour), g.live("app.example.com").AriNextCheckAt; !got.Equal(want) {
		t.Errorf("next check = %v for no Retry-After, want 6h: %v", got, want)
	}

	// The picked time comes: the renewal runs.
	g.clock.Advance(pick.Sub(g.clock.Now()))
	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeRenewed {
		t.Fatalf("the run ended %q, want renewed", end.outcome)
	}
	if renewed := g.live("app.example.com"); renewed.Serial == first.Serial {
		t.Errorf("the serial did not change")
	}
}

// D-09: a failure to get the renewal window is not a renewal failure. The picked time is kept and the CA is
// asked again in an hour; with no time picked yet the half life stands in for it.
func TestARIPollFailureKeepsRenewAt(t *testing.T) {
	ctx := context.Background()
	g := newRenewRig(t, false)
	m, first := g.issueOne("app.example.com")
	problem := acmetest.Problem{Status: 500, Type: "serverInternal", Detail: "try later"}

	// No time picked yet and the CA fails: the half life, and a look again in an hour.
	g.fake.FailNext(acmetest.EndpointRenewalInfo, problem)
	m.round(ctx)
	row := g.live("app.example.com")
	half, _ := HalfLifeRenewAt(first.NotBefore, first.NotAfter)
	if !row.RenewAt.Equal(half) || row.RenewBasis != string(acme.BasisHalf) {
		t.Fatalf("renew_at = %v basis %q after a failed first poll, want the half life %v", row.RenewAt, row.RenewBasis, half)
	}
	if want := g.clock.Now().Add(time.Hour); !row.AriNextCheckAt.Equal(want) {
		t.Errorf("next check = %v, want an hour on: %v", row.AriNextCheckAt, want)
	}

	// An hour later the CA answers, and the window sets the time.
	g.clock.Advance(time.Hour)
	m.round(ctx)
	row = g.live("app.example.com")
	picked := row.RenewAt
	if row.RenewBasis != string(acme.BasisARI) {
		t.Fatalf("basis = %q after the CA answered, want ari", row.RenewBasis)
	}

	// Once a time is picked, a failure leaves it where it is.
	g.clock.Advance(row.AriNextCheckAt.Sub(g.clock.Now()))
	g.fake.FailNext(acmetest.EndpointRenewalInfo, problem)
	m.round(ctx)
	row = g.live("app.example.com")
	if !row.RenewAt.Equal(picked) || row.RenewBasis != string(acme.BasisARI) {
		t.Errorf("renew_at = %v basis %q after a failed poll, want %v ari kept", row.RenewAt, row.RenewBasis, picked)
	}
	if want := g.clock.Now().Add(time.Hour); !row.AriNextCheckAt.Equal(want) {
		t.Errorf("next check = %v, want an hour on: %v", row.AriNextCheckAt, want)
	}
	if row.ConsecutiveFailures != 0 || row.LastErrorClass != "" || row.Phase != PhaseIdle {
		t.Errorf("row = %d failures, class %q, phase %q: a failed poll records no failure", row.ConsecutiveFailures, row.LastErrorClass, row.Phase)
	}
	if n := g.count(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, audit.ActionIngressCertFailed); n != 0 {
		t.Errorf("%d ingress.cert_failed rows, want none", n)
	}
}

// Pitfall 4: a renewal names the certificate it replaces only while the certificate was issued to the account
// registered now. After the CA forgot the account and a new one was registered, it names nothing.
func TestRenewalNamesTheReplacedCertificateOnlyForTheSameAccount(t *testing.T) {
	ctx := context.Background()
	g := newRenewRig(t, false)
	m, _ := g.issueOne("app.example.com")
	m.round(ctx)
	first := g.live("app.example.com")

	g.clock.Advance(first.RenewAt.Sub(g.clock.Now()))
	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeRenewed {
		t.Fatalf("the run ended %q, want renewed", end.outcome)
	}
	wantID, err := api.MakeARICertID(leaf(t, first.ChainPEM))
	if err != nil {
		t.Fatal(err)
	}
	if orders := g.fake.Orders(); len(orders) != 2 || orders[1].Replaces != wantID {
		t.Fatalf("the CA saw %+v, want a second order that replaces %q", orders, wantID)
	}

	// The CA forgets the account, and the manager registers a new one.
	m.round(ctx)
	second := g.live("app.example.com")
	g.fake.ForgetAccounts()
	if _, err := g.st.Write.Exec(`DELETE FROM ingress_acme_account`); err != nil {
		t.Fatal(err)
	}
	g.clock.Advance(second.RenewAt.Sub(g.clock.Now()))
	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeRenewed {
		t.Fatalf("the run ended %q after the account changed, want renewed", end.outcome)
	}
	orders := g.fake.Orders()
	if len(orders) != 3 || orders[2].Replaces != "" {
		t.Errorf("the CA saw %+v, want a third order that replaces nothing: the new account does not own the certificate", orders)
	}
	// The orderer would retry a refused replacement without it, so what shows the manager named nothing is that
	// the CA was never asked and refused.
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 3 {
		t.Errorf("the CA received %d newOrder calls, want 3: a refused replacement counts too", n)
	}
	if n := g.count(`SELECT COUNT(*) FROM ingress_acme_account`); n != 1 {
		t.Errorf("%d ingress accounts, want the one registered again", n)
	}
}

// D-14: a renewal that is due starts before names that have no certificate yet, even when those rows are older.
func TestRenewalsAreScheduledBeforeNewNames(t *testing.T) {
	ctx := context.Background()
	g := newRenewRig(t, true)
	m, _ := g.issueOne("a.example.com")
	m.round(ctx)
	first := g.live("a.example.com")

	// Two new names, both made before the renewal is due and so older than the renewal row (set to the far future), and one slot of the two taken by something else.
	g.claim("a.example.com", "b.example.com", "c.example.com")
	if _, err := g.st.Write.Exec(`UPDATE ingress_certs SET created_at = ? WHERE id = ?`, toNanos(g.clock.Now().Add(1000*24*time.Hour)), first.ID); err != nil {
		t.Fatal(err)
	}
	g.clock.Advance(first.RenewAt.Sub(g.clock.Now()))
	if !m.take("occupied") {
		t.Fatal("no slot to occupy")
	}
	m.round(ctx)

	if end := g.awaitEnd(); end.certID != first.ID || end.outcome != OutcomeRenewed {
		t.Fatalf("the run that took the free slot ended %+v, want the renewal of %s", end, first.ID)
	}
	for _, host := range []string{"b.example.com", "c.example.com"} {
		if row := g.live(host); row.Phase != PhaseQueued || len(row.ChainPEM) != 0 {
			t.Errorf("%s = phase %q, want it still queued behind the renewal", host, row.Phase)
		}
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 2 {
		t.Errorf("the CA saw %d orders, want the first issue and the renewal only", n)
	}

	// With the slot free again the new names are ordered too.
	m.drop("occupied")
	m.round(ctx)
	g.awaitEnd()
	g.awaitEnd()
}

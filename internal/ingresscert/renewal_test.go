// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"strings"
	"testing"

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

// issueOne claims host, acknowledges the terms and runs one round to the end of the first order, then one more
// round to give the certificate its renewal clock. It returns the manager and the row.
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
	m.round(ctx)
	return m, g.live(host)
}

// CRT-04, D-09, the tracer: with no renewal information from the CA an issued certificate is due at half of its
// lifetime. When the clock reaches it a round places a renewal in the same row, the new chain is stored, the
// renewal is audited and the issuance is counted as a renewal.
func TestTracerAnIssuedCertificateRenewsAtHalfLife(t *testing.T) {
	ctx := context.Background()
	g := newRenewRig(t, true)
	m, first := g.issueOne("app.example.com")

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

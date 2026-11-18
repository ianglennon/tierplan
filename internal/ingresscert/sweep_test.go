// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"errors"
	"testing"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
)

// leave puts a challenge TXT record at the fake provider with the token of domain dom, as an order does, and leaves
// it there: the record and its ledger row (credential_ref the domain's id) are what a failed cleanup leaves behind.
func (g *mgrRig) leave(m *Manager, dom ingress.Domain, host, challenge string) {
	g.t.Helper()
	s := &acme.DNSSolver{
		Engine: m.engine, Provider: dom.DNSProvider, CredentialRef: dom.ID,
		Tokens: func(context.Context) ([]byte, error) { return []byte(rigToken), nil },
		Owner:  func(context.Context) (string, error) { return "order-" + challenge, nil },
	}
	if err := s.Present(context.Background(), host, challenge, challenge+".thumb"); err != nil {
		g.t.Fatalf("Present(%s): %v", host, err)
	}
}

// sweepManager is a manager whose providers also answer to the id the fake provider writes to the ledger rows
// ("fake"), the id a sweep reads back from a row.
func (g *mgrRig) sweepManager() *Manager {
	g.t.Helper()
	return g.newManager(func(d *Deps) { d.DNS.Providers[rigProvider] = g.prov.Factory(rigToken) })
}

// ledgerRows is how many ledger rows of hostname certificates there are.
func (g *mgrRig) ledgerRows() int {
	g.t.Helper()
	rows, err := g.ledger.IngressDNSRecords(context.Background(), g.clock.Now(), false)
	if err != nil {
		g.t.Fatalf("IngressDNSRecords: %v", err)
	}
	return len(rows)
}

// D-27, the tracer: a domain whose token still has a challenge record in the ledger cannot be deleted; the ingress
// sweep deletes the record with that token, and the delete then succeeds.
func TestTracerADomainDeleteWaitsForItsRecordsToBeSwept(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	m := g.sweepManager()
	g.leave(m, g.domain, "app.example.com", "chal-1")
	if n := len(g.prov.Records()); n != 1 || g.ledgerRows() != 1 {
		t.Fatalf("the provider holds %d records and the ledger %d rows, want 1 and 1", n, g.ledgerRows())
	}

	err := g.dom.DeleteDomain(ctx, "env-1", g.domain.ID, nil)
	var pending *ingressstore.CleanupPendingError
	if !errors.As(err, &pending) || pending.Records != 1 {
		t.Fatalf("DeleteDomain = %v, want *CleanupPendingError with 1 record", err)
	}
	if _, err := g.dom.DomainByID(ctx, g.domain.ID); err != nil {
		t.Fatalf("the refused delete changed the domain: %v", err)
	}

	m.SweepNow()
	select {
	case <-m.wake:
	default:
		t.Error("SweepNow did not wake the manager")
	}
	m.round(ctx)
	if recs := g.prov.Records(); len(recs) != 0 {
		t.Errorf("the provider still holds %+v after the sweep", recs)
	}
	if n := g.ledgerRows(); n != 0 {
		t.Errorf("%d ledger rows left after the sweep, want none", n)
	}
	if err := g.dom.DeleteDomain(ctx, "env-1", g.domain.ID, nil); err != nil {
		t.Fatalf("DeleteDomain after the sweep = %v, want success", err)
	}
}

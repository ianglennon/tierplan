// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
)

// leave puts a challenge TXT record at the fake provider with the token of domain dom, as an order does, and leaves
// it there: the record and its ledger row (credential_ref the domain's id) are what a failed cleanup leaves behind.
func (g *mgrRig) leave(m *Manager, dom ingress.Domain, host, challenge, token string) {
	g.t.Helper()
	s := &acme.DNSSolver{
		Engine: m.engine, Provider: dom.DNSProvider, CredentialRef: dom.ID,
		Tokens: func(context.Context) ([]byte, error) { return []byte(token), nil },
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
	g.leave(m, g.domain, "app.example.com", "chal-1", rigToken)
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

// deleteLog records, for every record a provider deletes, the token the provider was built with.
type deleteLog struct {
	mu     sync.Mutex
	tokens map[string]string // record id -> token that deleted it
}

func (l *deleteLog) tokenOf(recordID string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tokens[recordID]
}

// tokenRecorder is a provider that records the token each delete was made with.
type tokenRecorder struct {
	dns.Provider
	token string
	log   *deleteLog
}

func (p tokenRecorder) DeleteTXT(ctx context.Context, ref dns.RecordRef) error {
	p.log.mu.Lock()
	p.log.tokens[ref.RecordID] = p.token
	p.log.mu.Unlock()
	return p.Provider.DeleteTXT(ctx, ref)
}

// acceptAnyToken makes the manager's providers (under the fake's own id too) accept every token and record which
// token deleted which record.
func (g *mgrRig) acceptAnyToken() (func(*Deps), *deleteLog) {
	l := &deleteLog{tokens: map[string]string{}}
	f := func(token string) dns.Provider {
		return tokenRecorder{Provider: g.prov.Factory(token)(token), token: token, log: l}
	}
	return func(d *Deps) {
		d.DNS.Providers = map[string]dns.Factory{ingress.ProviderCloudflare: f, rigProvider: f}
	}, l
}

// D-27: two domains' rows in one sweep are each deleted with the token of the domain that made them.
func TestSweepUsesEachDomainsOwnToken(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	second := g.secondEnv(ingress.IssuerProduction, "tok-two")
	mod, deletes := g.acceptAnyToken()
	m := g.newManager(mod)
	g.leave(m, g.domain, "one.example.com", "chal-1", rigToken)
	g.leave(m, second, "two.example.com", "chal-2", "tok-two")
	rows, err := g.ledger.IngressDNSRecords(ctx, g.clock.Now(), false)
	if err != nil || len(rows) != 2 {
		t.Fatalf("IngressDNSRecords = %d rows, %v, want 2", len(rows), err)
	}

	m.SweepNow()
	m.round(ctx)

	if n := g.ledgerRows(); n != 0 {
		t.Errorf("%d ledger rows left, want none", n)
	}
	if recs := g.prov.Records(); len(recs) != 0 {
		t.Errorf("the provider still holds %+v", recs)
	}
	want := map[string]string{g.domain.ID: rigToken, second.ID: "tok-two"}
	for _, r := range rows {
		if got := deletes.tokenOf(r.RecordID); got != want[r.CredentialRef] {
			t.Errorf("record %s of domain %s was deleted with token %q, want %q", r.RecordID, r.CredentialRef, got, want[r.CredentialRef])
		}
	}
}

// D-32: while issuing is paused after a restore, a sweep, full or due-only, makes no provider call.
func TestSweepMakesNoCallWhilePaused(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	m := g.sweepManager()
	g.leave(m, g.domain, "app.example.com", "chal-1", rigToken)
	if _, err := g.st.Write.Exec(`UPDATE ingress_cert_state SET paused_reason = ? WHERE id = 1`, PausedRestore); err != nil {
		t.Fatalf("pause: %v", err)
	}
	before := len(g.prov.Calls())

	m.SweepNow()
	m.round(ctx)
	m.sweep(ctx, false)
	m.sweep(ctx, true)

	if got := len(g.prov.Calls()); got != before {
		t.Errorf("the provider saw %d calls while paused, want none", got-before)
	}
	if n := g.ledgerRows(); n != 1 {
		t.Errorf("%d ledger rows, want the 1 left alone", n)
	}
}

// D-41: a row whose domain is gone (its environment was deleted) cannot be deleted at the provider; it is stamped
// dns_token_invalid on the backoff schedule, and a due-only sweep leaves it alone until its time.
func TestSweepStampsARecordWhoseDomainIsGone(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	m := g.sweepManager()
	now := g.clock.Now()
	if err := g.ledger.InsertDNSRecord(ctx, acme.DNSRecord{
		ID: "row-gone", Provider: rigProvider, ZoneID: "zone-1", RecordID: "rec-gone", Name: "_acme-challenge.app.example.com",
		Value: "v", OwnerKind: acme.OwnerOrder, OwnerID: "order-gone", CreatedAt: now, CredentialRef: "domain-gone",
	}); err != nil {
		t.Fatalf("InsertDNSRecord: %v", err)
	}
	row := func() acme.DNSRecord {
		rows, err := g.ledger.IngressDNSRecords(ctx, now.Add(1000*time.Hour), false)
		if err != nil || len(rows) != 1 {
			t.Fatalf("IngressDNSRecords = %d rows, %v, want the 1", len(rows), err)
		}
		return rows[0]
	}

	m.SweepNow()
	m.round(ctx)
	r := row()
	if r.Attempts != 1 || r.LastErrorClass != dns.ClassTokenInvalid {
		t.Fatalf("row = attempts %d class %q, want 1 and %s", r.Attempts, r.LastErrorClass, dns.ClassTokenInvalid)
	}
	if want := now.Add(acme.BackoffDelay(1, 0, 0)); !r.NextTryAt.Equal(want) {
		t.Errorf("next try = %v, want %v on the backoff schedule", r.NextTryAt, want)
	}

	m.sweep(ctx, true)
	if r := row(); r.Attempts != 1 {
		t.Errorf("attempts = %d after a due-only sweep before the time, want it untouched at 1", r.Attempts)
	}

	g.clock.Advance(acme.BackoffDelay(1, 0, 0) + time.Second)
	m.sweep(ctx, true)
	if r := row(); r.Attempts != 2 {
		t.Errorf("attempts = %d after the time came, want 2", r.Attempts)
	}
}

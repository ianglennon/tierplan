// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"log/slog"
	"testing"

	"github.com/ianglennon/tierplan/internal/acme/dns"
)

// domainToken is the one token the fake provider of the shared-solver tests accepts.
const domainToken = "tok-a" //nolint:gosec // G101: a fake token of a test provider

// sharedEngine is a DNSEngine over e's fakes whose provider accepts only domainToken, writing to e's
// store: the engine a hostname certificate's order would run on.
func (e *dnsEnv) sharedEngine() *DNSEngine {
	return NewDNSEngine(DNSEngineDeps{
		DNS: DNSDeps{
			Providers:       map[string]dns.Factory{dnsProviderID: e.prov.Factory(domainToken)},
			NameserverAddrs: func(context.Context, string) ([]string, error) { return []string{e.nsAddr}, nil },
			ResolvConf:      func() ([]string, error) { return []string{e.dns.Addr()}, nil },
		},
		Ledger: e.store, Clock: e.clock, Log: slog.New(slog.NewTextHandler(e.logs, nil)),
		Jitter: func() float64 { return 0 },
	})
}

// sharedSolver is a DNSSolver for the order orderID under credential credentialRef, whose token source
// gives token.
func (e *dnsEnv) sharedSolver(credentialRef, orderID, token string) *DNSSolver {
	return &DNSSolver{
		Engine: e.sharedEngine(), Provider: dnsProviderID, CredentialRef: credentialRef,
		Tokens: func(context.Context) ([]byte, error) { return []byte(token), nil },
		Owner:  func(context.Context) (string, error) { return orderID, nil },
	}
}

// The shared solver records the credential on its ledger row, and the own manager's reads (the sweep's
// two lists, the owner lookup its CleanUp uses) never see it (PROH-20-01, D-41). The credential-scoped
// reads do, and CleanUp deletes the record and the row.
func TestDNSSolverRecordsItsCredentialAndTheOwnSweepNeverSeesIt(t *testing.T) {
	ctx := context.Background()
	e := newDNSEnv(t, ChallengeDNS01)
	s := e.sharedSolver("dom-1", "order-ingress", domainToken)

	if err := s.Present(ctx, testHost, "chal-token", "chal-token.thumb"); err != nil {
		t.Fatalf("Present: %v", err)
	}
	if n := len(e.prov.Records()); n != 1 {
		t.Fatalf("the provider holds %d records, want the one TXT record", n)
	}

	rows, err := e.store.DNSRecordsForCredential(ctx, "dom-1", OwnerOrder, "order-ingress", "chal-token")
	if err != nil || len(rows) != 1 {
		t.Fatalf("DNSRecordsForCredential = %+v, %v, want the one row", rows, err)
	}
	if rows[0].CredentialRef != "dom-1" || rows[0].OwnerKind != OwnerOrder || rows[0].OwnerID != "order-ingress" {
		t.Errorf("row = %+v, want credential dom-1 owned by order order-ingress", rows[0])
	}
	if all, err := e.store.IngressDNSRecords(ctx, e.clock.Now(), false); err != nil || len(all) != 1 || all[0].ID != rows[0].ID {
		t.Errorf("IngressDNSRecords = %+v, %v, want the row", all, err)
	}
	if n, err := e.store.CountDNSRecordsByCredential(ctx, "dom-1"); err != nil || n != 1 {
		t.Errorf("CountDNSRecordsByCredential = %d, %v, want 1", n, err)
	}
	if n, err := e.store.CountDNSRecordsByCredential(ctx, "dom-2"); err != nil || n != 0 {
		t.Errorf("CountDNSRecordsByCredential(dom-2) = %d, %v, want 0", n, err)
	}

	// No own read sees the row.
	if own, err := e.store.DNSRecords(ctx); err != nil || len(own) != 0 {
		t.Errorf("DNSRecords = %+v, %v, want none", own, err)
	}
	if due, err := e.store.DueDNSRecords(ctx, e.clock.Now()); err != nil || len(due) != 0 {
		t.Errorf("DueDNSRecords = %+v, %v, want none", due, err)
	}
	if own, err := e.store.DNSRecordsFor(ctx, OwnerOrder, "order-ingress", "chal-token"); err != nil || len(own) != 0 {
		t.Errorf("DNSRecordsFor = %+v, %v, want none", own, err)
	}
	if _, found, err := e.store.PendingDNSRecord(ctx, rows[0].Provider, rows[0].ZoneID, rows[0].Name, rows[0].Value); err != nil || found {
		t.Errorf("PendingDNSRecord found = %v, %v, want not found: it is an ingress row", found, err)
	}

	if err := s.CleanUp(ctx, testHost, "chal-token", "chal-token.thumb"); err != nil {
		t.Fatalf("CleanUp: %v", err)
	}
	if recs := e.prov.Records(); len(recs) != 0 {
		t.Errorf("the provider still holds %+v after CleanUp", recs)
	}
	if n, err := e.store.CountDNSRecordsByCredential(ctx, "dom-1"); err != nil || n != 0 {
		t.Errorf("after CleanUp CountDNSRecordsByCredential = %d, %v, want 0", n, err)
	}
}

// A token the provider does not accept fails Present with the token class and leaves no row and no record.
func TestDNSSolverWithAWrongTokenFailsAsTokenInvalid(t *testing.T) {
	ctx := context.Background()
	e := newDNSEnv(t, ChallengeDNS01)
	s := e.sharedSolver("dom-1", "order-ingress", "wrong")

	err := s.Present(ctx, testHost, "chal-token", "chal-token.thumb")
	if !isDNSClass(err, dns.ClassTokenInvalid) {
		t.Fatalf("Present error = %v, want class %s", err, dns.ClassTokenInvalid)
	}
	if n, cerr := e.store.CountDNSRecordsByCredential(ctx, "dom-1"); cerr != nil || n != 0 {
		t.Errorf("CountDNSRecordsByCredential = %d, %v, want no row", n, cerr)
	}
	if recs := e.prov.Records(); len(recs) != 0 {
		t.Errorf("the provider holds %+v, want none", recs)
	}

	// An empty token is the same class and never reaches the provider.
	empty := e.sharedSolver("dom-1", "order-ingress", "")
	if err := empty.Present(ctx, testHost, "chal-token", "chal-token.thumb"); !isDNSClass(err, dns.ClassTokenInvalid) {
		t.Errorf("Present with an empty token = %v, want class %s", err, dns.ClassTokenInvalid)
	}
}

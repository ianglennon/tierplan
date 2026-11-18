// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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

// ingressRow makes a record at the provider and a ledger row for it under credentialRef, the way a
// hostname certificate's order that was cut short leaves one.
func (e *dnsEnv) ingressRow(credentialRef, fqdn, value string, created time.Time) DNSRecord {
	e.t.Helper()
	ctx := context.Background()
	p := e.prov.Factory(dnsToken)(dnsToken)
	z, err := p.Zone(ctx, fqdn)
	if err != nil {
		e.t.Fatalf("Zone(%s): %v", fqdn, err)
	}
	ref, err := p.CreateTXT(ctx, z, fqdn, value, "tierplan:old-install:old-order")
	if err != nil {
		e.t.Fatalf("CreateTXT: %v", err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		e.t.Fatal(err)
	}
	row := DNSRecord{
		ID: id.String(), Provider: dnsProviderID, ZoneID: ref.ZoneID, ZoneName: z.Name, AccountID: z.AccountID, RecordID: ref.RecordID,
		Name: fqdn, Value: value, Comment: "tierplan:old-install:old-order", OwnerKind: OwnerOrder, OwnerID: "old-order",
		Token: "old", CreatedAt: created, CredentialRef: credentialRef,
	}
	if err := e.store.InsertDNSRecord(ctx, row); err != nil {
		e.t.Fatalf("InsertDNSRecord: %v", err)
	}
	return row
}

// deleteTracker is a provider that notes which credential's provider deleted which record.
type deleteTracker struct {
	dns.Provider
	credentialRef string
	deleted       *[]string
}

func (d deleteTracker) DeleteTXT(ctx context.Context, ref dns.RecordRef) error {
	*d.deleted = append(*d.deleted, d.credentialRef+":"+ref.RecordID)
	return d.Provider.DeleteTXT(ctx, ref)
}

// The sweep builds one provider per (provider, credential_ref) group and deletes each row with its own
// credential's provider; a credential that cannot be opened stamps only its own rows; a cancelled context
// stops it before the next row.
func TestDNSEngineSweepGroupsByCredential(t *testing.T) {
	ctx := context.Background()
	tokens := map[string]string{"dom-1": "tok-a", "dom-2": "tok-b"}

	// resolver builds a provider per credential from a fake provider that accepts that credential's
	// token, counting the builds and noting every delete by the credential that made it.
	type rig struct {
		e       *dnsEnv
		eng     *DNSEngine
		built   map[string]int
		deleted []string
	}
	newRig := func(t *testing.T) *rig {
		e := newDNSEnv(t, ChallengeDNS01)
		return &rig{e: e, eng: e.sharedEngine(), built: map[string]int{}}
	}
	resolve := func(r *rig, unopenable ...string) CredentialResolver {
		return func(_ context.Context, provider, ref string) (dns.Provider, string) {
			r.built[ref]++
			for _, u := range unopenable {
				if u == ref {
					return nil, dns.ClassTokenInvalid
				}
			}
			factory := r.e.prov.Factory(tokens[ref])
			return deleteTracker{Provider: factory(tokens[ref]), credentialRef: ref, deleted: &r.deleted}, ""
		}
	}

	t.Run("each row is deleted with its own credential's provider", func(t *testing.T) {
		r := newRig(t)
		a1 := r.e.ingressRow("dom-1", "_acme-challenge.a.example.com", "va1", testNow)
		b1 := r.e.ingressRow("dom-2", "_acme-challenge.b.example.com", "vb1", testNow.Add(time.Second))
		a2 := r.e.ingressRow("dom-1", "_acme-challenge.c.example.com", "va2", testNow.Add(2*time.Second))
		rows, err := r.e.store.IngressDNSRecords(ctx, r.e.clock.Now(), false)
		if err != nil || len(rows) != 3 {
			t.Fatalf("IngressDNSRecords = %+v, %v, want 3 rows", rows, err)
		}

		r.eng.Sweep(ctx, rows, resolve(r))

		want := []string{"dom-1:" + a1.RecordID, "dom-2:" + b1.RecordID, "dom-1:" + a2.RecordID}
		if !slices.Equal(r.deleted, want) {
			t.Errorf("deletes = %v, want %v", r.deleted, want)
		}
		if r.built["dom-1"] != 1 || r.built["dom-2"] != 1 {
			t.Errorf("providers built = %v, want one per credential", r.built)
		}
		if left, _ := r.e.store.IngressDNSRecords(ctx, r.e.clock.Now(), false); len(left) != 0 {
			t.Errorf("ledger = %+v, want every row gone", left)
		}
		if recs := r.e.prov.Records(); len(recs) != 0 {
			t.Errorf("the provider still holds %+v", recs)
		}
	})

	t.Run("a credential that cannot be opened stamps its own rows and spares the others", func(t *testing.T) {
		r := newRig(t)
		r.e.ingressRow("dom-1", "_acme-challenge.a.example.com", "va1", testNow)
		bad := r.e.ingressRow("dom-2", "_acme-challenge.b.example.com", "vb1", testNow.Add(time.Second))
		rows, _ := r.e.store.IngressDNSRecords(ctx, r.e.clock.Now(), false)

		r.eng.Sweep(ctx, rows, resolve(r, "dom-2"))

		left, err := r.e.store.IngressDNSRecords(ctx, r.e.clock.Now(), false)
		if err != nil || len(left) != 1 || left[0].ID != bad.ID {
			t.Fatalf("ledger = %+v, %v, want only dom-2's row", left, err)
		}
		wantNext := r.e.clock.Now().Add(BackoffDelay(1, 0, 0))
		if left[0].Attempts != 1 || left[0].LastErrorClass != dns.ClassTokenInvalid || !left[0].NextTryAt.Equal(wantNext) {
			t.Errorf("row = attempts %d class %q next %v, want 1, %s, %v", left[0].Attempts, left[0].LastErrorClass, left[0].NextTryAt, dns.ClassTokenInvalid, wantNext)
		}
		if len(r.deleted) != 1 || !strings.HasPrefix(r.deleted[0], "dom-1:") {
			t.Errorf("deletes = %v, want only dom-1's record", r.deleted)
		}
	})

	t.Run("a cancelled context stops before the next row", func(t *testing.T) {
		r := newRig(t)
		r.e.ingressRow("dom-1", "_acme-challenge.a.example.com", "va1", testNow)
		second := r.e.ingressRow("dom-1", "_acme-challenge.b.example.com", "va2", testNow.Add(time.Second))
		rows, _ := r.e.store.IngressDNSRecords(ctx, r.e.clock.Now(), false)

		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		inner := resolve(r)
		r.eng.Sweep(cctx, rows, func(c context.Context, provider, ref string) (dns.Provider, string) {
			cancel() // cut short while the first row is being handled
			return inner(c, provider, ref)
		})

		left, err := r.e.store.IngressDNSRecords(ctx, r.e.clock.Now(), false)
		if err != nil || len(left) != 1 || left[0].ID != second.ID {
			t.Errorf("ledger = %+v, %v, want only the second row left", left, err)
		}
	})
}

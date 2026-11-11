// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/acme/dns/cloudflare"
	"github.com/ianglennon/tierplan/internal/acme/dns/dnstest"
)

// refusedZone is what a provider answers for a zone ID it does not accept: a token without the
// permission, a zone that is gone and a zone in another account all look alike.
func refusedZone() error {
	return &dns.Error{Provider: "fake", Op: "delete record", Class: dns.ClassTokenScope, Status: 403, Code: 10000, Err: dns.ErrZoneRefused}
}

// A zone that was deleted takes its records with it (WR-04). The delete is refused for the zone ID, the
// sweep asks for the zone by its stored name, finds that the token still sees zones and none of them is
// it, and drops the row: nothing is deleted by name, and nothing else is touched.
func TestDNSSweepDropsTheRowOfAZoneThatIsGone(t *testing.T) {
	ctx := context.Background()
	e := newDNSEnv(t, ChallengeDNS01)
	e.prov.AddZone("other.org", dnsNS) // the same account, so the token still sees a zone there
	row := e.leftOver("_acme-challenge.old.example.com", "old-value", testNow)
	keep := e.leftOver("_acme-challenge.keep.other.org", "keep-value", testNow.Add(time.Second))
	e.prov.RemoveZone("example.com")

	e.mgr.sweepDNS(ctx, false)

	rows := e.ledger()
	if len(rows) != 0 {
		t.Fatalf("ledger = %+v, want the gone zone's row dropped and the other record deleted", rows)
	}
	if got := e.deletes(); len(got) != 2 || got[0] != row.RecordID || got[1] != keep.RecordID {
		t.Errorf("deletes = %v, want one delete by ID for each of %s and %s", got, row.RecordID, keep.RecordID)
	}
	var lookups []string
	for _, c := range e.prov.Calls() {
		if c.Op == "account_zone" && c.Name == "example.com" {
			lookups = append(lookups, c.Name)
		}
	}
	if len(lookups) != 1 {
		t.Errorf("zone lookups of the stored name = %v, want one", lookups)
	}
	if left := leftOversOf(t, e.mgr); len(left) != 0 {
		t.Errorf("left-over = %+v, want none", left)
	}
	if strings.Contains(e.logs.String(), "old-value") {
		t.Errorf("the log holds a record value: %s", e.logs.String())
	}
}

// A zone deleted and made again has a new ID under the same name: the old one is gone.
func TestDNSSweepDropsTheRowOfAZoneMadeAgainUnderANewID(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	row := e.leftOver("_acme-challenge.old.example.com", "old-value", testNow)
	e.prov.RemoveZone("example.com")
	if id := e.prov.AddZone("example.com", dnsNS); id == row.ZoneID {
		t.Fatalf("the zone came back with the same ID %s", id)
	}

	e.mgr.sweepDNS(context.Background(), false)

	if rows := e.ledger(); len(rows) != 0 {
		t.Errorf("ledger = %+v, want the row dropped", rows)
	}
}

// Where the question cannot be answered, the row is kept and retried as before (D-18): the lookup
// fails, the token sees no zone in the record's own account (it is another account's token, which sees
// other zones and says the same thing), the zone is still there under its ID (the token may not edit
// its records), or the row has no account ID, with or without a zone name.
func TestDNSSweepKeepsTheRowWhenTheZoneIsNotShownToBeGone(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		setup                     func(e *dnsEnv)
		withZoneName, withAccount bool
	}{
		{"the zone lookup fails", func(e *dnsEnv) {
			e.prov.RemoveZone("example.com")
			e.prov.AddZone("other.org", dnsNS)
			e.prov.FailZone(&dns.Error{Provider: "fake", Op: "find zone", Class: dns.ClassUnavailable})
		}, true, true},
		{"the token sees no zone at all", func(e *dnsEnv) { e.prov.RemoveZone("example.com") }, true, true},
		{"a token for another account that sees other zones", func(e *dnsEnv) {
			e.prov.HideZone("example.com")
			e.prov.AddZoneInAccount("another-account", "foreign.net", dnsNS)
		}, true, true},
		{"a token for another account, the zone gone from view", func(e *dnsEnv) {
			e.prov.RemoveZone("example.com")
			e.prov.AddZoneInAccount("another-account", "foreign.net", dnsNS)
		}, true, true},
		{"the same name in another account", func(e *dnsEnv) {
			e.prov.RemoveZone("example.com")
			e.prov.AddZoneInAccount("another-account", "example.com", dnsNS)
		}, true, true},
		{"the zone is still there", func(e *dnsEnv) { e.prov.FailDelete(-1, refusedZone()) }, true, true},
		{"the row has a zone name but no account ID", func(e *dnsEnv) {
			e.prov.RemoveZone("example.com")
			e.prov.AddZone("other.org", dnsNS)
		}, true, false},
		{"the row has neither", func(e *dnsEnv) {
			e.prov.RemoveZone("example.com")
			e.prov.AddZone("other.org", dnsNS)
		}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDNSEnv(t, ChallengeDNS01)
			row := e.leftOverNamed("_acme-challenge.old.example.com", "old-value", testNow, tc.withZoneName, tc.withAccount)
			tc.setup(e)

			e.mgr.sweepDNS(context.Background(), false)

			rows := e.ledger()
			if len(rows) != 1 || rows[0].ID != row.ID {
				t.Fatalf("ledger = %+v, want the row kept", rows)
			}
			if r := rows[0]; r.Attempts != 1 || r.LastErrorClass != dns.ClassTokenScope || !r.NextTryAt.Equal(testNow.Add(time.Minute)) {
				t.Errorf("row = %+v, want one failed attempt of class dns_token_scope and the next in a minute", r)
			}
		})
	}
}

// A token the provider does not accept at all is not a refused zone: the row is retried.
func TestDNSSweepKeepsTheRowOfARejectedToken(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	row := e.leftOver("_acme-challenge.old.example.com", "old-value", testNow)
	e.mgr.d.DNS.Providers[dnsProviderID] = e.prov.Factory("some-other-token")
	zonesBefore := e.prov.CallCount("zone")

	e.mgr.sweepDNS(context.Background(), false)

	rows := e.ledger()
	if len(rows) != 1 || rows[0].ID != row.ID || rows[0].LastErrorClass != dns.ClassTokenInvalid {
		t.Errorf("ledger = %+v, want the row kept with class dns_token_invalid", rows)
	}
	if n := e.prov.CallCount("zone") - zonesBefore; n != 0 {
		t.Errorf("%d zone lookups for a token the provider rejects, want none", n)
	}
}

// The ledger keeps the zone's name and account from the moment the record is made, for an order and for a test.
func TestDNSLedgerRowsCarryTheZoneName(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	e.prov.FailDelete(-1, &dns.Error{Provider: "fake", Op: "delete record", Class: dns.ClassUnavailable})

	e.inJob(func(ctx context.Context, steps *jobSteps) {
		_ = e.solver(steps).Present(ctx, testHost, "tok", "tok.thumb")
	})
	e.mgr.testName(context.Background(), e.prov.Factory(dnsToken)(dnsToken), testHost, nil, "install-unit")

	rows := e.ledger()
	if len(rows) != 2 {
		t.Fatalf("ledger = %+v, want the order's row and the test's row", rows)
	}
	for _, r := range rows {
		if r.ZoneName != "example.com" || r.AccountID != dnstest.FakeDefaultAccount {
			t.Errorf("%s row has zone name %q and account %q, want example.com in %s", r.OwnerKind, r.ZoneName, r.AccountID, dnstest.FakeDefaultAccount)
		}
	}
}

// The whole path against the Cloudflare client and its fake API: a zone deleted in Cloudflare answers
// the delete with 403 and code 10000, the zone is looked up by name and is not found while the token
// still sees another zone in the record's account, and the ledger row goes. A token for another
// account, or one that sees no zone, keeps the row.
func TestDNSSweepOfAZoneDeletedAtCloudflare(t *testing.T) {
	setup := func(t *testing.T) (*dnsEnv, *dnstest.FakeCloudflare, DNSRecord) {
		t.Helper()
		ctx := context.Background()
		e := newDNSEnv(t, ChallengeDNS01)
		fake := dnstest.NewFakeCloudflare(t)
		fake.AddZone("example.com", dnsNS)
		fake.AddZone("other.org", dnsNS)
		factory := cloudflare.New(cloudflare.Options{BaseURL: fake.URL(), HTTPClient: fake.Client(), UserAgent: "TierPlan/test"})
		p := factory(fake.Token)
		fqdn := "_acme-challenge.old.example.com"
		z, err := p.Zone(ctx, fqdn)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := p.CreateTXT(ctx, z, fqdn, "old-value", "tierplan:old-install:old-order")
		if err != nil {
			t.Fatal(err)
		}
		id, err := uuid.NewV7()
		if err != nil {
			t.Fatal(err)
		}
		row := DNSRecord{
			ID: id.String(), Provider: "cloudflare", ZoneID: ref.ZoneID, ZoneName: z.Name, AccountID: z.AccountID, RecordID: ref.RecordID, Name: fqdn,
			Value: "old-value", OwnerKind: OwnerOrder, OwnerID: "old-order", Token: "old", CreatedAt: testNow,
		}
		if err := e.store.InsertDNSRecord(ctx, row); err != nil {
			t.Fatal(err)
		}
		tok := fake.Token
		if err := e.store.SaveValidation(ctx, ValidationWrite{ChallengeType: ChallengeDNS01, Provider: "cloudflare", Token: &tok, Now: e.clock.Now()}); err != nil {
			t.Fatal(err)
		}
		e.mgr.d.DNS.Providers["cloudflare"] = factory
		e.mgr.log = slog.New(slog.NewTextHandler(e.logs, nil))
		return e, fake, row
	}

	t.Run("the token still sees other zones", func(t *testing.T) {
		e, fake, row := setup(t)
		fake.RemoveZone("example.com")

		e.mgr.sweepDNS(context.Background(), false)

		if rows := e.ledger(); len(rows) != 0 {
			t.Errorf("ledger = %+v, want the row of %s dropped with its zone", rows, row.Name)
		}
		for _, r := range fake.Requests() {
			if r.Method == "DELETE" && !strings.HasSuffix(r.Path, "/dns_records/"+row.RecordID) {
				t.Errorf("a DELETE went to %s, want only the stored record ID", r.Path)
			}
		}
	})

	t.Run("a token for another account that sees other zones", func(t *testing.T) {
		e, fake, row := setup(t)
		fake.RemoveZone("example.com")
		fake.AddZoneInAccount("account-two", "foreign.net", dnsNS)
		fake.LimitZones("foreign.net") // the record's own account shows this token nothing

		e.mgr.sweepDNS(context.Background(), false)

		rows := e.ledger()
		if len(rows) != 1 || rows[0].ID != row.ID || rows[0].LastErrorClass != dns.ClassTokenScope || rows[0].Attempts != 1 {
			t.Errorf("ledger = %+v, want the row kept for a retry: the record may still exist in its own account", rows)
		}
	})

	t.Run("the token sees no zone", func(t *testing.T) {
		e, fake, row := setup(t)
		fake.RemoveZone("example.com")
		fake.LimitZones() // none visible

		e.mgr.sweepDNS(context.Background(), false)

		rows := e.ledger()
		if len(rows) != 1 || rows[0].ID != row.ID || rows[0].LastErrorClass != dns.ClassTokenScope || rows[0].Attempts != 1 {
			t.Errorf("ledger = %+v, want the row kept for a retry with class dns_token_scope", rows)
		}
	})

	t.Run("the zone is there and the token may not edit its records", func(t *testing.T) {
		e, fake, row := setup(t)
		fake.Script(dnstest.OpDelete, dnstest.FakeResponse{Status: 403, Code: 10000, Message: "Authentication error"})

		e.mgr.sweepDNS(context.Background(), false)

		rows := e.ledger()
		if len(rows) != 1 || rows[0].ID != row.ID || rows[0].LastErrorClass != dns.ClassTokenScope {
			t.Errorf("ledger = %+v, want the row kept", rows)
		}
		if n := len(fake.Records()); n != 1 {
			t.Errorf("the fake holds %d records, want the one that was not deleted", n)
		}
	})
}

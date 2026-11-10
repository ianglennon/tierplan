// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/jobs"
)

// inJob runs fn as the body of a job on the hub, so it has the steps a solver reports to, and waits
// for the job to end.
func (e *dnsEnv) inJob(fn func(ctx context.Context, steps *jobSteps)) jobs.Job {
	e.t.Helper()
	job, err := e.hub.Start(jobs.Spec{Kind: "test", Owner: "user:1", Items: itemsFor(ChallengeDNS01)},
		func(ctx context.Context, r *jobs.Reporter) {
			fn(ctx, &jobSteps{r: r, in: ClassifyInput{ChallengeType: ChallengeDNS01}, items: itemsFor(ChallengeDNS01)})
		})
	if err != nil {
		e.t.Fatalf("start a job: %v", err)
	}
	return e.wait(RunStart{JobID: job.ID})
}

// solver is a solver for the order "order-unit", reporting to steps.
func (e *dnsEnv) solver(steps *jobSteps) *dnsSolver {
	return &dnsSolver{m: e.mgr, steps: steps, provider: dnsProviderID, orderID: "order-unit"}
}

func TestDNSPresentRollbackOnLedgerFailure(t *testing.T) {
	ctx := context.Background()
	e := newDNSEnv(t, ChallengeDNS01)
	// A row with the ID the solver is about to use makes the ledger insert fail after the create.
	if err := e.store.InsertDNSRecord(ctx, DNSRecord{
		ID: "taken", Provider: "fake", ZoneID: "z", RecordID: "r", Name: "n", Value: "v", OwnerKind: OwnerOrder,
		OwnerID: "other-order", Token: "other", CreatedAt: testNow,
	}); err != nil {
		t.Fatal(err)
	}

	var presentErr error
	e.inJob(func(ctx context.Context, steps *jobSteps) {
		s := e.solver(steps)
		s.newID = func() (string, error) { return "taken", nil }
		presentErr = s.Present(ctx, testHost, "tok", "tok.thumb")
		if steps.leftOver != 0 {
			t.Errorf("left over = %d, want 0: the created record was deleted", steps.leftOver)
		}
	})

	if presentErr == nil {
		t.Fatal("Present succeeded though the ledger insert failed")
	}
	if class := Classify(presentErr, ClassifyInput{ChallengeType: ChallengeDNS01}).Class; class != ClassInternal {
		t.Errorf("a ledger failure classified as %q, want internal", class)
	}
	if recs := e.prov.Records(); len(recs) != 0 {
		t.Errorf("the provider holds %+v, want the record created before the ledger failed deleted again", recs)
	}
	if rows := e.ledger(); len(rows) != 1 || rows[0].ID != "taken" {
		t.Errorf("ledger = %+v, want only the row that was already there", rows)
	}
}

func TestDNSRollbackDeleteFailureKeepsTheRow(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	// A zone with no nameserver: the record is created and written to the ledger, then the check has
	// nobody to ask, so Present fails after both.
	e.prov.AddZone("bare.example")
	e.prov.FailDelete(1, &dns.Error{Provider: "fake", Op: "delete record", Class: dns.ClassRateLimited})

	var presentErr error
	var leftOver int
	e.inJob(func(ctx context.Context, steps *jobSteps) {
		presentErr = e.solver(steps).Present(ctx, "host.bare.example", "tok", "tok.thumb")
		leftOver = steps.leftOver
	})

	var de *dns.Error
	if !errors.As(presentErr, &de) || de.Class != dns.ClassNotVisible {
		t.Fatalf("Present = %v, want a dns_not_visible error", presentErr)
	}
	if leftOver != 1 {
		t.Errorf("left over = %d, want 1", leftOver)
	}
	rows := e.ledger()
	if len(rows) != 1 {
		t.Fatalf("ledger = %+v, want the row of the record that could not be deleted", rows)
	}
	if r := rows[0]; r.Attempts != 1 || !r.LastTryAt.Equal(testNow) || !r.NextTryAt.Equal(testNow.Add(time.Minute)) || r.LastErrorClass != dns.ClassRateLimited {
		t.Errorf("row = %+v, want 1 attempt now, the next in a minute, class dns_rate_limited", r)
	}
	if n := len(e.prov.Records()); n != 1 {
		t.Errorf("the provider holds %d records, want the 1 that could not be deleted", n)
	}
}

func TestDNSPresentRefusedAsIdenticalTouchesNothing(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	chal := dns.ChallengeFQDN(testHost)
	foreign := e.prov.AddForeignTXT(chal, dns.TXTValue("tok.thumb")) // another tool made exactly this record

	var presentErr error
	var leftOver int
	e.inJob(func(ctx context.Context, steps *jobSteps) {
		presentErr = e.solver(steps).Present(ctx, testHost, "tok", "tok.thumb")
		leftOver = steps.leftOver
	})

	if presentErr == nil {
		t.Fatal("Present succeeded though the provider refused an identical record")
	}
	recs := e.prov.Records()
	if len(recs) != 1 || recs[0].ID != foreign {
		t.Errorf("the provider holds %+v, want only the foreign record", recs)
	}
	if n := e.prov.CallCount("delete"); n != 0 {
		t.Errorf("TierPlan made %d delete calls for a record it holds no ID for, want 0", n)
	}
	if rows := e.ledger(); len(rows) != 0 || leftOver != 0 {
		t.Errorf("ledger = %+v, left over = %d, want nothing recorded", rows, leftOver)
	}
}

func TestDNSCleanUpDeletesOnlyTheRecordsOfItsToken(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	chal := dns.ChallengeFQDN(testHost)

	e.inJob(func(ctx context.Context, steps *jobSteps) {
		s := e.solver(steps)
		for _, tok := range []string{"a", "b"} {
			if err := s.Present(ctx, testHost, tok, tok+".thumb"); err != nil {
				t.Errorf("Present(%s): %v", tok, err)
			}
		}
		if err := s.CleanUp(ctx, testHost, "a", "a.thumb"); err != nil {
			t.Errorf("CleanUp: %v", err)
		}
	})

	recs := e.prov.Records()
	if len(recs) != 1 || recs[0].Name != chal || recs[0].Value() != dns.TXTValue("b.thumb") {
		t.Errorf("the provider holds %+v, want only the record of token b", recs)
	}
	rows := e.ledger()
	if len(rows) != 1 || rows[0].Token != "b" {
		t.Errorf("ledger = %+v, want only the row of token b", rows)
	}
}

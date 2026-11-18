// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"crypto/x509"
	"errors"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
)

// issueSeamLeaf registers an account with fake in accounts and issues a certificate for
// app.example.com from it, through the in-memory stores of the seam test, and returns the leaf.
func issueSeamLeaf(t *testing.T, fake *acmetest.Fake, clock *acmetest.Clock, accounts *memAccountStore) *x509.Certificate {
	t.Helper()
	ctx := context.Background()
	dir := Directory{URL: fake.DirectoryURL(), Roots: fake.RootPool(), Overridden: true}
	client := NewClient(dir, accounts, clock, "tierplan-test/0")
	if _, err := client.Register(ctx, "", true); err != nil {
		t.Fatalf("Register: %v", err)
	}
	issued, err := NewOrderer(client, &memOrderStore{}, noopDNSSolver{}, clock).Run(ctx,
		OrderRequest{Kind: KindIssue, Names: []string{"app.example.com"}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return issued.Leaf
}

func TestClientRenewalWindow(t *testing.T) {
	ctx := context.Background()
	start := testNow.Add(30 * 24 * time.Hour)
	end := start.Add(10 * 24 * time.Hour)

	t.Run("the CA's suggested window and Retry-After", func(t *testing.T) {
		clock := acmetest.NewClock(testNow)
		fake := acmetest.NewFake(t, acmetest.Options{Clock: clock, ChallengeType: ChallengeDNS01})
		accounts := newMemAccountStore()
		leaf := issueSeamLeaf(t, fake, clock, accounts)
		fake.SetRenewalWindow(leaf, start, end, 3*time.Hour)

		client := NewClient(Directory{URL: fake.DirectoryURL(), Roots: fake.RootPool(), Overridden: true}, accounts, clock, "tierplan-test/0")
		got, ok, err := client.RenewalWindow(ctx, leaf)
		if err != nil || !ok {
			t.Fatalf("RenewalWindow = ok %v, err %v; want ok and no error", ok, err)
		}
		if !got.Start.Equal(start) || !got.End.Equal(end) || got.RetryAfter != 3*time.Hour {
			t.Errorf("RenewalWindow = {%v %v %v}, want {%v %v 3h}", got.Start, got.End, got.RetryAfter, start, end)
		}
	})

	t.Run("a directory without renewal information is no answer, and no account is looked up", func(t *testing.T) {
		clock := acmetest.NewClock(testNow)
		fake := acmetest.NewFake(t, acmetest.Options{Clock: clock, ChallengeType: ChallengeDNS01, NoRenewalInfo: true})
		leaf := issueSeamLeaf(t, fake, clock, newMemAccountStore())

		// A client with no account: it would get ErrNoAccount if it looked one up.
		client := NewClient(Directory{URL: fake.DirectoryURL(), Roots: fake.RootPool(), Overridden: true}, newMemAccountStore(), clock, "tierplan-test/0")
		got, ok, err := client.RenewalWindow(ctx, leaf)
		if err != nil || ok || got != (RenewalWindow{}) {
			t.Errorf("RenewalWindow = %+v, ok %v, err %v; want zero, false, nil", got, ok, err)
		}
		if n := fake.Count(acmetest.EndpointRenewalInfo); n != 0 {
			t.Errorf("renewalInfo was asked %d times, want 0", n)
		}
	})

	t.Run("a certificate the CA does not know is no answer", func(t *testing.T) {
		clock := acmetest.NewClock(testNow)
		other := acmetest.NewFake(t, acmetest.Options{Clock: clock, ChallengeType: ChallengeDNS01})
		stranger := issueSeamLeaf(t, other, clock, newMemAccountStore())

		fake := acmetest.NewFake(t, acmetest.Options{Clock: clock, ChallengeType: ChallengeDNS01})
		accounts := newMemAccountStore()
		issueSeamLeaf(t, fake, clock, accounts)
		client := NewClient(Directory{URL: fake.DirectoryURL(), Roots: fake.RootPool(), Overridden: true}, accounts, clock, "tierplan-test/0")

		got, ok, err := client.RenewalWindow(ctx, stranger)
		if err != nil || ok || got != (RenewalWindow{}) {
			t.Errorf("RenewalWindow = %+v, ok %v, err %v; want zero, false, nil", got, ok, err)
		}
		if n := fake.Count(acmetest.EndpointRenewalInfo); n != 1 {
			t.Errorf("renewalInfo was asked %d times, want 1 (a 404, not a skipped question)", n)
		}
	})

	t.Run("a directory that cannot be read is an error", func(t *testing.T) {
		clock := acmetest.NewClock(testNow)
		fake := acmetest.NewFake(t, acmetest.Options{Clock: clock, ChallengeType: ChallengeDNS01})
		accounts := newMemAccountStore()
		leaf := issueSeamLeaf(t, fake, clock, accounts)

		client := NewClient(Directory{URL: fake.DirectoryURL(), Roots: fake.RootPool(), Overridden: true}, accounts, clock, "tierplan-test/0")
		fake.FailNext(acmetest.EndpointDirectory, acmetest.Problem{Status: 503, Type: "serverInternal", Detail: "down"})
		_, ok, err := client.RenewalWindow(ctx, leaf)
		if err == nil || ok {
			t.Errorf("RenewalWindow = ok %v, err %v; want an error and not ok", ok, err)
		}
	})

	t.Run("no account for the directory is ErrNoAccount", func(t *testing.T) {
		clock := acmetest.NewClock(testNow)
		fake := acmetest.NewFake(t, acmetest.Options{Clock: clock, ChallengeType: ChallengeDNS01})
		leaf := issueSeamLeaf(t, fake, clock, newMemAccountStore())

		client := NewClient(Directory{URL: fake.DirectoryURL(), Roots: fake.RootPool(), Overridden: true}, newMemAccountStore(), clock, "tierplan-test/0")
		_, ok, err := client.RenewalWindow(ctx, leaf)
		if !errors.Is(err, ErrNoAccount) || ok {
			t.Errorf("RenewalWindow = ok %v, err %v; want ErrNoAccount", ok, err)
		}
	})
}

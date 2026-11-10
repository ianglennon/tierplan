// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/config"
)

var testNow = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

// newFakeClient is a Client over a fake ACME directory that it trusts through a CA file, the way the
// test-only overrides are used, with a store over a temporary database and a clock the test moves.
func newFakeClient(t *testing.T, o acmetest.Options) (*Client, *acmetest.Fake, *Store, *acmetest.Clock) {
	t.Helper()
	fake := acmetest.NewFake(t, o)
	dir, err := NewDirectory(config.Config{ACMEDirectoryURL: fake.DirectoryURL(), ACMECAFile: fake.CAFile(t)})
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}
	s, _, _ := newTestStore(t)
	clock := acmetest.NewClock(testNow)
	return NewClient(dir, s, clock, "tierplan-test/0"), fake, s, clock
}

func TestRegisterAgainstTheFakeDirectory(t *testing.T) {
	ctx := context.Background()
	c, fake, s, _ := newFakeClient(t, acmetest.Options{TermsOfService: "https://acme.example/tos"})

	acct, err := c.Register(ctx, "admin@example.com", true)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if acct.DirectoryURL != fake.DirectoryURL() || acct.Contact != "mailto:admin@example.com" ||
		acct.TermsOfService != "https://acme.example/tos" || acct.URL == "" || !acct.CreatedAt.Equal(testNow) {
		t.Errorf("Register returned %+v", acct)
	}

	got, key, found, err := s.Account(ctx, fake.DirectoryURL())
	if err != nil || !found {
		t.Fatalf("the account row = found %v, %v; want it stored", found, err)
	}
	if key == nil || got.URL != acct.URL || got.Contact != "mailto:admin@example.com" {
		t.Errorf("stored account = %+v, key %v", got, key)
	}

	if n := fake.Count(acmetest.EndpointNewAccount); n != 1 {
		t.Errorf("newAccount requests = %d, want 1", n)
	}

	seen := fake.AccountsSeen()
	if len(seen) != 1 || !seen[0].TermsOfServiceAgreed || len(seen[0].Contact) != 1 || seen[0].Contact[0] != "mailto:admin@example.com" {
		t.Errorf("the CA saw %+v, want one account with the terms agreed and the mailto contact", seen)
	}

	// The stored account signs a Core: the CA's account URL is the key id.
	core, _, ckey, err := c.core(ctx)
	if err != nil {
		t.Fatalf("core: %v", err)
	}
	if core.GetKID() != acct.URL || !ckey.Equal(key) {
		t.Errorf("core kid = %q, want %q", core.GetKID(), acct.URL)
	}

	// Account reports what is registered, without the key.
	if a, ok, err := c.Account(ctx); err != nil || !ok || a.URL != acct.URL {
		t.Errorf("Account = %+v, %v, %v", a, ok, err)
	}
}

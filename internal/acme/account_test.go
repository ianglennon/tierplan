// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/proto/testsupport"
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

func TestRegisterRefusesWithoutTermsOrWithABadEmail(t *testing.T) {
	ctx := context.Background()
	c, fake, s, _ := newFakeClient(t, acmetest.Options{TermsOfService: "https://acme.example/tos"})

	if _, err := c.Register(ctx, "admin@example.com", false); !errors.Is(err, ErrTOSNotAccepted) {
		t.Errorf("without the terms: error = %v, want ErrTOSNotAccepted", err)
	}
	for _, email := range []string{"not an email", "Admin <a@example.com>", "a@example.com, b@example.com", "@example.com"} {
		if _, err := c.Register(ctx, email, true); !errors.Is(err, ErrContactInvalid) {
			t.Errorf("email %q: error = %v, want ErrContactInvalid", email, err)
		}
	}

	if n := fake.Count(acmetest.EndpointNewAccount); n != 0 {
		t.Errorf("newAccount requests = %d, want 0: a refusal must not reach the CA", n)
	}
	if n := fake.Count(acmetest.EndpointDirectory); n != 0 {
		t.Errorf("directory requests = %d, want 0: a refusal must not reach the CA", n)
	}
	if _, _, found, err := s.Account(ctx, fake.DirectoryURL()); found || err != nil {
		t.Errorf("a refused registration stored an account: found %v, %v", found, err)
	}
}

func TestRegisterWithoutEmailSendsNoContact(t *testing.T) {
	ctx := context.Background()
	c, fake, s, _ := newFakeClient(t, acmetest.Options{})

	acct, err := c.Register(ctx, "", true)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if acct.Contact != "" {
		t.Errorf("Register returned contact %q, want none", acct.Contact)
	}
	seen := fake.AccountsSeen()
	if len(seen) != 1 || len(seen[0].Contact) != 0 || !seen[0].TermsOfServiceAgreed {
		t.Errorf("the CA saw %+v, want one account with no contact and the terms agreed", seen)
	}
	got, _, found, err := s.Account(ctx, fake.DirectoryURL())
	if err != nil || !found || got.Contact != "" {
		t.Errorf("stored account = %+v, found %v, %v; want an empty contact", got, found, err)
	}
}

func TestRegisterIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c, fake, s, _ := newFakeClient(t, acmetest.Options{})

	first, err := c.Register(ctx, "admin@example.com", true)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, keyBefore, _, err := s.Account(ctx, fake.DirectoryURL())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.Register(ctx, "other@example.com", true); !errors.Is(err, ErrAccountExists) {
		t.Fatalf("second Register: error = %v, want ErrAccountExists", err)
	}
	if n := fake.Count(acmetest.EndpointNewAccount); n != 1 {
		t.Errorf("newAccount requests = %d, want 1: a second Register must send nothing", n)
	}
	got, keyAfter, _, err := s.Account(ctx, fake.DirectoryURL())
	if err != nil {
		t.Fatal(err)
	}
	if !keyAfter.Equal(keyBefore) || got.URL != first.URL || got.Contact != first.Contact {
		t.Errorf("the stored account changed: %+v, key equal %v", got, keyAfter.Equal(keyBefore))
	}
}

func TestRegisterConcurrentlyCreatesOneAccount(t *testing.T) {
	ctx := context.Background()
	c, fake, _, _ := newFakeClient(t, acmetest.Options{})

	release := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-release
			_, err := c.Register(ctx, "admin@example.com", true)
			results <- err
		}()
	}
	close(release)

	var created, existed int
	for range 2 {
		switch err := testsupport.Await(t, "a Register call to finish", results); {
		case err == nil:
			created++
		case errors.Is(err, ErrAccountExists):
			existed++
		default:
			t.Errorf("Register: %v", err)
		}
	}
	if created != 1 || existed != 1 {
		t.Errorf("created %d, already existed %d; want exactly one of each", created, existed)
	}
	if n := fake.Count(acmetest.EndpointNewAccount); n != 1 {
		t.Errorf("newAccount requests = %d, want 1", n)
	}
}

func TestMetaReadsTermsProfilesAndCaches(t *testing.T) {
	ctx := context.Background()
	c, fake, _, clock := newFakeClient(t, acmetest.Options{TermsOfService: "https://acme.example/tos"})

	m, err := c.Meta(ctx)
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if m.TermsOfService != "https://acme.example/tos" || !m.RenewalInfo {
		t.Errorf("Meta = %+v", m)
	}
	if _, ok := m.Profiles["classic"]; !ok || len(m.Profiles) != 2 {
		t.Errorf("profiles = %v, want classic and shortlived", m.Profiles)
	}
	if _, ok := m.Profiles["shortlived"]; !ok {
		t.Errorf("profiles = %v, want shortlived too", m.Profiles)
	}
	// The caller owns what it was given: changing it must not change the cache.
	m.Profiles["classic"] = "changed"

	clock.Advance(24*time.Hour - time.Second)
	m2, err := c.Meta(ctx)
	if err != nil || m2.Profiles["classic"] == "changed" {
		t.Errorf("a second Meta = %+v, %v; want the cached document, unchanged", m2, err)
	}
	if n := fake.Count(acmetest.EndpointDirectory); n != 1 {
		t.Errorf("directory requests inside 24 h = %d, want 1", n)
	}

	clock.Advance(2 * time.Second)
	if _, err := c.Meta(ctx); err != nil {
		t.Fatal(err)
	}
	if n := fake.Count(acmetest.EndpointDirectory); n != 2 {
		t.Errorf("directory requests after 24 h = %d, want 2", n)
	}
}

func TestMetaWithoutRenewalInfoAndFailuresCachedForAMinute(t *testing.T) {
	ctx := context.Background()
	c, fake, _, clock := newFakeClient(t, acmetest.Options{NoRenewalInfo: true})

	fake.FailNext(acmetest.EndpointDirectory, acmetest.Problem{Status: 503, Type: "serverInternal", Detail: "down"})
	if _, err := c.Meta(ctx); err == nil {
		t.Fatal("Meta against a failing directory returned no error")
	}
	if _, err := c.Meta(ctx); err == nil {
		t.Error("a second Meta inside a minute returned no error")
	}
	if n := fake.Count(acmetest.EndpointDirectory); n != 1 {
		t.Errorf("directory requests inside the failure window = %d, want 1", n)
	}

	clock.Advance(time.Minute + time.Second)
	m, err := c.Meta(ctx)
	if err != nil {
		t.Fatalf("Meta after the failure window: %v", err)
	}
	if m.RenewalInfo {
		t.Error("RenewalInfo = true for a directory that does not offer it")
	}
}

// A-WR-02: a caller whose context has ended gets its error, and the next caller reads the directory:
// one caller going away is not a directory failure to remember for a minute.
func TestMetaDoesNotCacheACallersCancellation(t *testing.T) {
	c, fake, _, _ := newFakeClient(t, acmetest.Options{})

	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Meta(gone); !errors.Is(err, context.Canceled) {
		t.Fatalf("Meta with an ended context = %v, want context.Canceled", err)
	}
	m, err := c.Meta(context.Background())
	if err != nil {
		t.Fatalf("Meta after a caller went away = %v, want the directory read", err)
	}
	if !m.RenewalInfo || len(m.Profiles) == 0 {
		t.Errorf("Meta = %+v, want the directory's renewal information and profiles", m)
	}
	if n := fake.Count(acmetest.EndpointDirectory); n != 1 {
		t.Errorf("directory requests = %d, want 1", n)
	}
}

func TestClientTrustsOnlyTheConfiguredCA(t *testing.T) {
	ctx := context.Background()
	fake := acmetest.NewFake(t, acmetest.Options{})
	s, _, _ := newTestStore(t)

	// No roots of its own: the system pool does not vouch for the fake, and verification is not
	// skipped.
	untrusting := NewClient(Directory{URL: fake.DirectoryURL(), Overridden: true}, s, acmetest.NewClock(testNow), "tierplan-test/0")
	_, err := untrusting.Register(ctx, "admin@example.com", true)
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Fatalf("Register against a CA nothing trusts: error = %v, want an unknown-authority error", err)
	}
	if got := Classify(err, ClassifyInput{}); got.Class != ClassCAUntrusted {
		t.Errorf("class = %q, want %q", got.Class, ClassCAUntrusted)
	}
	if _, _, found, err := s.Account(ctx, fake.DirectoryURL()); found || err != nil {
		t.Errorf("a failed registration stored an account: found %v, %v", found, err)
	}
	if n := fake.Count(acmetest.EndpointNewAccount); n != 0 {
		t.Errorf("newAccount requests = %d, want 0", n)
	}

	// The same directory with its own root as the only root works.
	trusting := NewClient(Directory{URL: fake.DirectoryURL(), Overridden: true, Roots: fake.RootPool()}, s, acmetest.NewClock(testNow), "tierplan-test/0")
	if _, err := trusting.Register(ctx, "admin@example.com", true); err != nil {
		t.Fatalf("Register with the CA file's roots: %v", err)
	}
}

func TestForgetAllowsRegisteringAgain(t *testing.T) {
	ctx := context.Background()
	c, fake, _, _ := newFakeClient(t, acmetest.Options{})

	first, err := c.Register(ctx, "admin@example.com", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Forget(ctx); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, ok, err := c.Account(ctx); ok || err != nil {
		t.Errorf("Account after Forget = found %v, %v; want none", ok, err)
	}
	if _, _, _, err := c.core(ctx); !errors.Is(err, ErrNoAccount) {
		t.Errorf("core after Forget: error = %v, want ErrNoAccount", err)
	}

	// The CA still knows the key's account, so it answers with the same one; what matters here is
	// that TierPlan is willing to ask again.
	fake.ForgetAccounts()
	second, err := c.Register(ctx, "admin@example.com", true)
	if err != nil {
		t.Fatalf("Register after Forget: %v", err)
	}
	if second.URL == first.URL {
		t.Errorf("the second account reuses the first's URL %q: the key should be new", second.URL)
	}
	if n := fake.Count(acmetest.EndpointNewAccount); n != 2 {
		t.Errorf("newAccount requests = %d, want 2", n)
	}
}

func TestNewDirectoryResolvesTheOverrides(t *testing.T) {
	d, err := NewDirectory(config.Config{})
	if err != nil || d.URL != LetsEncryptDirectory || d.Overridden || d.Roots != nil {
		t.Errorf("unset = %+v, %v; want Let's Encrypt production on the system pool", d, err)
	}

	d, err = NewDirectory(config.Config{ACMEDirectoryURL: "https://ca.example/dir"})
	if err != nil || d.URL != "https://ca.example/dir" || !d.Overridden || d.Roots != nil {
		t.Errorf("directory only = %+v, %v", d, err)
	}

	fake := acmetest.NewFake(t, acmetest.Options{})
	d, err = NewDirectory(config.Config{ACMEDirectoryURL: fake.DirectoryURL(), ACMECAFile: fake.CAFile(t)})
	if err != nil || !d.Overridden || d.Roots == nil {
		t.Errorf("directory and CA file = %+v, %v; want a private pool", d, err)
	}

	empty := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(empty, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]config.Config{
		"CA file without a directory": {ACMECAFile: empty},
		"CA file with no certificate": {ACMEDirectoryURL: "https://ca.example/dir", ACMECAFile: empty},
		"CA file that is missing":     {ACMEDirectoryURL: "https://ca.example/dir", ACMECAFile: filepath.Join(t.TempDir(), "nope.pem")},
	} {
		if _, err := NewDirectory(cfg); err == nil || !strings.Contains(err.Error(), "TIERPLAN_ACME_CA_FILE") {
			t.Errorf("%s: error = %v, want one naming TIERPLAN_ACME_CA_FILE", name, err)
		}
	}
}

func TestEveryCoreGetsItsOwnHTTPClient(t *testing.T) {
	a, b := newHTTPClient(nil), newHTTPClient(nil)
	if a == b || a.Transport == b.Transport {
		t.Error("newHTTPClient shared a client or a transport: lego wraps the transport it is given")
	}
	if a.Timeout != 30*time.Second {
		t.Errorf("timeout = %v, want 30s", a.Timeout)
	}
}

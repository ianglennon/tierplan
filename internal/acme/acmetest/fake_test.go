// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acmetest

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-acme/lego/v5/acme"
	"github.com/go-acme/lego/v5/acme/api"
	"github.com/go-acme/lego/v5/certcrypto"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The fake directory (plan 14-03, D-17): lego's acme/api.Core is driven against it over verified TLS,
// the challenge is validated by a real GET, and certificate times follow the injected clock.

// The clock satisfies the interface the rest of the code takes.
var _ stackops.Clock = (*Clock)(nil)

// epoch is the instant the tests' clock starts at, a whole second (a certificate holds no more).
var epoch = time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)

// challengeServer serves the http-01 key authorizations a test presents, as the sniff listener of
// the product does, and counts the GETs.
type challengeServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	auth map[string]string // token -> key authorization
	hits map[string]int
}

func newChallengeServer(t testing.TB) *challengeServer {
	t.Helper()
	cs := &challengeServer{auth: map[string]string{}, hits: map[string]int{}}
	cs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.URL.Path, "/.well-known/acme-challenge/")
		cs.mu.Lock()
		body, found := cs.auth[token]
		if ok {
			cs.hits[token]++
		}
		cs.mu.Unlock()
		if !ok || !found {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(cs.srv.Close)
	return cs
}

func (cs *challengeServer) present(token, keyAuth string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.auth[token] = keyAuth
}

func (cs *challengeServer) total() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	n := 0
	for _, h := range cs.hits {
		n += h
	}
	return n
}

// fix is a fake directory, its clock, a challenge server every name resolves to, and a lego Core
// with a registered account.
type fix struct {
	fake *Fake
	clk  *Clock
	chal *challengeServer
	core *api.Core
	key  crypto.Signer
	acct acme.ExtendedAccount
	ctx  context.Context
}

func newFix(t *testing.T, o Options) *fix {
	t.Helper()
	f := &fix{clk: NewClock(epoch), chal: newChallengeServer(t), ctx: testsupport.Context(t)}
	o.Clock = f.clk
	if o.Validate == nil {
		o.Validate = func(string) string { return f.chal.srv.Listener.Addr().String() }
	}
	f.fake = NewFake(t, o)
	f.key = newKey(t, certcrypto.EC256)
	f.core = f.newCore(t, f.key, "")
	acct, err := f.core.Accounts.New(f.ctx, acme.Account{TermsOfServiceAgreed: true})
	if err != nil {
		t.Fatalf("register an account: %v", err)
	}
	f.acct = acct
	return f
}

// newCore is a lego Core that trusts the fake's root and nothing else.
func (f *fix) newCore(t *testing.T, key crypto.Signer, kid string) *api.Core {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.fake.RootPool(), MinVersion: tls.VersionTLS12}}}
	t.Cleanup(client.CloseIdleConnections)
	core, err := api.New(client, "tierplan-test", f.fake.DirectoryURL(), kid, key)
	if err != nil {
		t.Fatalf("api.New against the fake: %v", err)
	}
	return core
}

func newKey(t testing.TB, kt certcrypto.KeyType) crypto.Signer {
	t.Helper()
	k, err := certcrypto.GeneratePrivateKey(kt)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// solve presents the key authorization of every authorization of the order and answers each
// challenge, as the product's http-01 provider will.
func (f *fix) solve(t *testing.T, o acme.ExtendedOrder) {
	t.Helper()
	for _, u := range o.Authorizations {
		az, err := f.core.Authorizations.Get(f.ctx, u)
		if err != nil {
			t.Fatalf("get the authorization: %v", err)
		}
		var ch *acme.Challenge
		for i := range az.Challenges {
			if az.Challenges[i].Type == "http-01" {
				ch = &az.Challenges[i]
			}
		}
		if ch == nil {
			t.Fatalf("no http-01 challenge in %+v", az)
		}
		keyAuth, err := f.core.GetKeyAuthorization(ch.Token)
		if err != nil {
			t.Fatal(err)
		}
		f.chal.present(ch.Token, keyAuth)
		if _, err := f.core.Challenges.New(f.ctx, ch.URL); err != nil {
			t.Fatalf("answer the challenge: %v", err)
		}
	}
}

// finalize sends a CSR for names signed by a fresh certificate key.
func (f *fix) finalize(t *testing.T, o acme.ExtendedOrder, names ...string) acme.ExtendedOrder {
	t.Helper()
	csr, err := certcrypto.CreateCSR(newKey(t, certcrypto.EC256), certcrypto.CSROptions{Domain: names[0], SAN: names})
	if err != nil {
		t.Fatal(err)
	}
	done, err := f.core.Orders.UpdateForCSR(f.ctx, o.Finalize, csr)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	return done
}

func leafOf(t *testing.T, chain []byte) *x509.Certificate {
	t.Helper()
	certs, err := certcrypto.ParsePEMBundle(chain)
	if err != nil {
		t.Fatal(err)
	}
	return certs[0]
}

func TestFakeIssuesThroughLegoCore(t *testing.T) {
	t.Parallel()
	f := newFix(t, Options{})
	if f.acct.Location == "" {
		t.Fatal("registration returned no account URL")
	}

	order, err := f.core.Orders.New(f.ctx, []string{"tierplan.example.com"}, &api.OrderOptions{Profile: "classic"})
	if err != nil {
		t.Fatalf("new order: %v", err)
	}
	f.solve(t, order)

	ready, err := f.core.Orders.Get(f.ctx, order.Location)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Status != acme.StatusReady {
		t.Fatalf("order is %q after its challenge was validated, want ready", ready.Status)
	}
	done := f.finalize(t, order, "tierplan.example.com")
	if done.Status != acme.StatusValid || done.Certificate == "" {
		t.Fatalf("finalized order = %+v, want valid with a certificate URL", done.Order)
	}
	raw, err := f.core.Certificates.Get(f.ctx, done.Certificate, true)
	if err != nil {
		t.Fatalf("download the certificate: %v", err)
	}

	leaf := leafOf(t, raw.Cert)
	if !slices.Equal(leaf.DNSNames, []string{"tierplan.example.com"}) {
		t.Fatalf("leaf names = %v", leaf.DNSNames)
	}
	if !leaf.NotBefore.Equal(f.clk.Now()) {
		t.Fatalf("NotBefore = %v, want the injected clock's %v", leaf.NotBefore, f.clk.Now())
	}
	if want := f.clk.Now().Add(90 * 24 * time.Hour); !leaf.NotAfter.Equal(want) {
		t.Fatalf("NotAfter = %v, want %v (classic, 90 days)", leaf.NotAfter, want)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: f.fake.IssuerPool(), DNSName: "tierplan.example.com", CurrentTime: f.clk.Now()}); err != nil {
		t.Fatalf("leaf does not verify against the issuer pool: %v", err)
	}
	if f.chal.total() == 0 {
		t.Fatal("the challenge server was never asked: the fake did not perform a real GET")
	}
	// The fake's HTTPS certificate is trusted through the pool only: a client without it fails.
	bare := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}
	t.Cleanup(bare.CloseIdleConnections)
	if _, err := api.New(bare, "tierplan-test", f.fake.DirectoryURL(), "", f.key); err == nil {
		t.Fatal("a client that does not trust the fake's root reached the directory")
	}
}

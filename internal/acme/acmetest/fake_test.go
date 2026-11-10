// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acmetest

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
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

// statusLog records the status of every response the client under test received, so a test can tell
// that the fake answered a request with badNonce even though lego retried it away.
type statusLog struct {
	rt    http.RoundTripper
	mu    sync.Mutex
	codes []int
}

func (s *statusLog) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := s.rt.RoundTrip(r)
	if err == nil {
		s.mu.Lock()
		s.codes = append(s.codes, resp.StatusCode)
		s.mu.Unlock()
	}
	return resp, err
}

func (s *statusLog) count(code int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.codes {
		if c == code {
			n++
		}
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
	log  *statusLog
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
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.fake.RootPool(), MinVersion: tls.VersionTLS12}}
	f.log = &statusLog{rt: tr}
	client := &http.Client{Transport: f.log}
	t.Cleanup(tr.CloseIdleConnections)
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
func (f *fix) solve(t *testing.T, o acme.ExtendedOrder) []acme.Challenge {
	t.Helper()
	var answered []acme.Challenge
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
		got, err := f.core.Challenges.New(f.ctx, ch.URL)
		if err != nil {
			t.Fatalf("answer the challenge: %v", err)
		}
		answered = append(answered, got.Challenge)
		answered[len(answered)-1].Token = ch.Token
	}
	return answered
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

// issue runs one order for names to a downloaded certificate.
func (f *fix) issue(t *testing.T, opts *api.OrderOptions, names ...string) (acme.ExtendedOrder, *x509.Certificate) {
	t.Helper()
	order, err := f.core.Orders.New(f.ctx, names, opts)
	if err != nil {
		t.Fatalf("new order: %v", err)
	}
	f.solve(t, order)
	done := f.finalize(t, order, names...)
	raw, err := f.core.Certificates.Get(f.ctx, done.Certificate, true)
	if err != nil {
		t.Fatalf("download the certificate: %v", err)
	}
	return order, leafOf(t, raw.Cert)
}

func TestFakeRateLimitCarriesRetryAfter(t *testing.T) {
	t.Parallel()
	f := newFix(t, Options{})
	f.fake.FailNext(EndpointNewOrder, Problem{Status: http.StatusTooManyRequests, Type: "rateLimited", Detail: "too many new orders", RetryAfterSeconds: 3600})

	_, err := f.core.Orders.New(f.ctx, []string{"tierplan.example.com"}, nil)
	var rl *acme.RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("new order error = %v, want *acme.RateLimitedError", err)
	}
	if rl.RetryAfter != time.Hour {
		t.Fatalf("RetryAfter = %v, want 1h", rl.RetryAfter)
	}
	if _, err := f.core.Orders.New(f.ctx, []string{"tierplan.example.com"}, nil); err != nil {
		t.Fatalf("the failure was scripted once; the next order failed too: %v", err)
	}
	if got := f.fake.Count(EndpointNewOrder); got != 2 {
		t.Fatalf("newOrder count = %d, want 2 (the refused call counts)", got)
	}
}

func TestFakeServiceUnavailable(t *testing.T) {
	t.Parallel()
	f := newFix(t, Options{})
	f.fake.FailNext(EndpointNewOrder, Problem{Status: http.StatusServiceUnavailable, Type: "serverInternal", Detail: "down for maintenance", RetryAfterSeconds: 120})
	_, err := f.core.Orders.New(f.ctx, []string{"tierplan.example.com"}, nil)
	var pd *acme.ProblemDetails
	if !errors.As(err, &pd) || pd.HTTPStatus != http.StatusServiceUnavailable || pd.Type != acme.ServerInternalErrorType {
		t.Fatalf("new order error = %v, want a 503 serverInternal problem", err)
	}

	// lego keeps Retry-After only for a rate limit, so the header is read off the wire.
	f.fake.FailNext(EndpointDirectory, Problem{Status: http.StatusServiceUnavailable, RetryAfterSeconds: 90})
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.fake.RootPool(), MinVersion: tls.VersionTLS12}}}
	t.Cleanup(client.CloseIdleConnections)
	req, err := http.NewRequestWithContext(f.ctx, http.MethodGet, f.fake.DirectoryURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "90" {
		t.Fatalf("directory answered %d with Retry-After %q, want 503 and the delta-seconds 90", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestFakeBadNonceOnce(t *testing.T) {
	t.Parallel()
	f := newFix(t, Options{})
	f.fake.BadNonceOnce()
	if _, err := f.core.Orders.New(f.ctx, []string{"tierplan.example.com"}, nil); err != nil {
		t.Fatalf("lego's own retry should have survived one badNonce: %v", err)
	}
	if got := f.log.count(http.StatusBadRequest); got != 1 {
		t.Fatalf("the client saw %d 400 answers, want exactly the one badNonce", got)
	}
	// The scripted failure is spent: another call goes straight through.
	before := f.log.count(http.StatusBadRequest)
	if _, err := f.core.Orders.New(f.ctx, []string{"other.example.com"}, nil); err != nil {
		t.Fatal(err)
	}
	if f.log.count(http.StatusBadRequest) != before {
		t.Fatal("a second badNonce was answered")
	}
}

func TestFakeFailedValidation(t *testing.T) {
	t.Parallel()
	f := newFix(t, Options{})
	f.fake.FailValidation("bad.example.com", Problem{Type: "connection", Detail: "timeout during connect"})

	order, err := f.core.Orders.New(f.ctx, []string{"bad.example.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	answered := f.solve(t, order)
	if len(answered) != 1 || answered[0].Status != acme.StatusInvalid || answered[0].Error == nil || answered[0].Error.Type != acme.ConnectionErrorType {
		t.Fatalf("challenge after the scripted failure = %+v, want invalid with a connection problem", answered)
	}
	az, err := f.core.Authorizations.Get(f.ctx, order.Authorizations[0])
	if err != nil {
		t.Fatal(err)
	}
	if az.Status != acme.StatusInvalid || az.Challenges[0].Error == nil || az.Challenges[0].Error.Detail != "timeout during connect" {
		t.Fatalf("authorization = %+v, want invalid carrying the problem", az)
	}
	got, err := f.core.Orders.Get(f.ctx, order.Location)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != acme.StatusInvalid {
		t.Fatalf("order is %q, want invalid", got.Status)
	}
	if hits := f.fake.ValidationHits(answered[0].Token); hits != 0 {
		t.Fatalf("a scripted failure still performed %d GETs", hits)
	}
}

func TestFakeHoldsValidationAndFinalize(t *testing.T) {
	t.Parallel()
	f := newFix(t, Options{})
	releaseValidation := f.fake.HoldValidation()

	order, err := f.core.Orders.New(f.ctx, []string{"tierplan.example.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	answered := f.solve(t, order)
	token := answered[0].Token
	if answered[0].Status != acme.StatusProcessing {
		t.Fatalf("held challenge is %q, want processing", answered[0].Status)
	}
	if got, _ := f.core.Orders.Get(f.ctx, order.Location); got.Status != acme.StatusPending {
		t.Fatalf("order is %q while its validation is held, want pending", got.Status)
	}
	if hits := f.fake.ValidationHits(token); hits != 0 {
		t.Fatalf("%d GETs while validation was held", hits)
	}

	releaseValidation()
	if got, _ := f.core.Orders.Get(f.ctx, order.Location); got.Status != acme.StatusReady {
		t.Fatalf("order is %q after the release, want ready", got.Status)
	}
	if hits := f.fake.ValidationHits(token); hits != 1 {
		t.Fatalf("%d GETs after the release, want 1", hits)
	}

	releaseFinalize := f.fake.HoldFinalize()
	done := f.finalize(t, order, "tierplan.example.com")
	if done.Status != acme.StatusProcessing || done.Certificate != "" {
		t.Fatalf("held finalize = %+v, want processing without a certificate", done.Order)
	}
	if got, _ := f.core.Orders.Get(f.ctx, order.Location); got.Status != acme.StatusProcessing {
		t.Fatalf("order is %q while finalize is held, want processing", got.Status)
	}
	releaseFinalize()
	got, err := f.core.Orders.Get(f.ctx, order.Location)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != acme.StatusValid || got.Certificate == "" {
		t.Fatalf("order after the release = %+v, want valid with a certificate", got.Order)
	}
	if _, err := f.core.Certificates.Get(f.ctx, got.Certificate, true); err != nil {
		t.Fatalf("download after the release: %v", err)
	}
}

func TestFakeRenewalInfo(t *testing.T) {
	t.Parallel()
	f := newFix(t, Options{})
	_, leaf := f.issue(t, &api.OrderOptions{Profile: "classic"}, "tierplan.example.com")
	id, err := api.MakeARICertID(leaf)
	if err != nil {
		t.Fatal(err)
	}

	info, err := f.core.Certificates.GetRenewalInfo(f.ctx, id)
	if err != nil {
		t.Fatalf("renewal info: %v", err)
	}
	mid := leaf.NotBefore.Add(60 * 24 * time.Hour)
	if !info.SuggestedWindow.Start.Equal(mid.Add(-24*time.Hour)) || !info.SuggestedWindow.End.Equal(mid.Add(24*time.Hour)) {
		t.Fatalf("default window = %v to %v, want two thirds of the lifetime, a day either side", info.SuggestedWindow.Start, info.SuggestedWindow.End)
	}
	if info.RetryAfter != 6*time.Hour {
		t.Fatalf("RetryAfter = %v, want 6h", info.RetryAfter)
	}

	start, end := f.clk.Now().Add(time.Hour), f.clk.Now().Add(3*time.Hour)
	f.fake.SetRenewalWindow(leaf, start, end, 2*time.Hour)
	info, err = f.core.Certificates.GetRenewalInfo(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !info.SuggestedWindow.Start.Equal(start) || !info.SuggestedWindow.End.Equal(end) || info.RetryAfter != 2*time.Hour {
		t.Fatalf("scripted window = %+v retry %v, want %v to %v retry 2h", info.SuggestedWindow, info.RetryAfter, start, end)
	}
	if got := f.fake.Count(EndpointRenewalInfo); got != 2 {
		t.Fatalf("renewalInfo count = %d, want 2", got)
	}

	// A CA without ARI: the directory has no renewalInfo, and lego says so.
	bare := newFix(t, Options{NoRenewalInfo: true})
	_, other := bare.issue(t, nil, "tierplan.example.com")
	otherID, err := api.MakeARICertID(other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bare.core.Certificates.GetRenewalInfo(bare.ctx, otherID); !errors.Is(err, api.ErrNoARI) {
		t.Fatalf("renewal info without an endpoint: %v, want ErrNoARI", err)
	}
	if got := bare.fake.Count(EndpointRenewalInfo); got != 0 {
		t.Fatalf("lego asked a directory without renewalInfo %d times", got)
	}
}

func TestFakeAlreadyReplaced(t *testing.T) {
	t.Parallel()
	f := newFix(t, Options{})
	_, leaf := f.issue(t, nil, "tierplan.example.com")
	id, err := api.MakeARICertID(leaf)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.core.Orders.New(f.ctx, []string{"tierplan.example.com"}, &api.OrderOptions{ReplacesCertID: id}); err != nil {
		t.Fatalf("the first replacement: %v", err)
	}
	// The second replacement of the same certificate is refused with alreadyReplaced; lego retries
	// the order without replaces, which is accepted.
	if _, err := f.core.Orders.New(f.ctx, []string{"tierplan.example.com"}, &api.OrderOptions{ReplacesCertID: id}); err != nil {
		t.Fatalf("lego should retry an alreadyReplaced order without replaces: %v", err)
	}
	seen := f.fake.Orders()
	if len(seen) != 3 {
		t.Fatalf("Orders() = %+v, want the first, the replacement and the retry (the refusal is not an order)", seen)
	}
	if seen[1].Replaces != id || seen[2].Replaces != "" {
		t.Fatalf("replaces = %q then %q, want the id then none", seen[1].Replaces, seen[2].Replaces)
	}
	if got := f.fake.Count(EndpointNewOrder); got != 4 {
		t.Fatalf("newOrder count = %d, want 4 (issue, replacement, refusal, retry)", got)
	}

	// A certificate the account was never issued cannot be replaced.
	if _, err := f.core.Orders.New(f.ctx, []string{"tierplan.example.com"}, &api.OrderOptions{ReplacesCertID: "AAAA.BBBB"}); err == nil {
		t.Fatal("an order replacing an unknown certificate was accepted")
	}
}

func TestFakeForgetsAccounts(t *testing.T) {
	t.Parallel()
	f := newFix(t, Options{})
	f.fake.ForgetAccounts()
	_, err := f.core.Orders.New(f.ctx, []string{"tierplan.example.com"}, nil)
	var pd *acme.ProblemDetails
	if !errors.As(err, &pd) || pd.Type != acme.AccountDoesNotExistErrorType {
		t.Fatalf("new order after the account was forgotten: %v, want accountDoesNotExist", err)
	}
	// Looking the key up without creating anything also finds nothing.
	_, err = f.core.Accounts.New(f.ctx, acme.Account{OnlyReturnExisting: true})
	if !errors.As(err, &pd) || pd.Type != acme.AccountDoesNotExistErrorType {
		t.Fatalf("onlyReturnExisting after the account was forgotten: %v, want accountDoesNotExist", err)
	}
	again, err := f.core.Accounts.New(f.ctx, acme.Account{TermsOfServiceAgreed: true})
	if err != nil {
		t.Fatal(err)
	}
	if again.Location == f.acct.Location {
		t.Fatalf("the re-registered account kept the old URL %s", again.Location)
	}
	if _, err := f.core.Orders.New(f.ctx, []string{"tierplan.example.com"}, nil); err != nil {
		t.Fatalf("new order after registering again: %v", err)
	}
}

func TestFakeCounts(t *testing.T) {
	t.Parallel()
	f := newFix(t, Options{})
	for _, e := range []string{EndpointNewOrder, EndpointChallenge, EndpointFinalize, EndpointCert, EndpointRenewalInfo} {
		if got := f.fake.Count(e); got != 0 {
			t.Fatalf("Count(%s) = %d before any order", e, got)
		}
	}
	if f.fake.Count(EndpointDirectory) != 1 || f.fake.Count(EndpointNewAccount) != 1 {
		t.Fatalf("directory %d and newAccount %d after registering, want 1 each", f.fake.Count(EndpointDirectory), f.fake.Count(EndpointNewAccount))
	}

	order, leaf := f.issue(t, &api.OrderOptions{Profile: "shortlived"}, "tierplan.example.com", "www.example.com")
	for endpoint, want := range map[string]int{
		EndpointNewOrder: 1, EndpointChallenge: 2, EndpointFinalize: 1, EndpointCert: 1, EndpointAuthz: 2,
	} {
		if got := f.fake.Count(endpoint); got != want {
			t.Errorf("Count(%s) = %d, want %d", endpoint, got, want)
		}
	}
	if got := f.fake.Count(EndpointOrder); got != 0 {
		t.Errorf("Count(order) = %d, want 0: nothing fetched the order", got)
	}
	seen := f.fake.Orders()
	if len(seen) != 1 || !slices.Equal(seen[0].Names, []string{"tierplan.example.com", "www.example.com"}) || seen[0].Profile != "shortlived" || seen[0].Replaces != "" {
		t.Fatalf("Orders() = %+v", seen)
	}
	if want := f.clk.Now().Add(160 * time.Hour); !leaf.NotAfter.Equal(want) {
		t.Fatalf("NotAfter = %v, want %v (shortlived, 160 hours)", leaf.NotAfter, want)
	}
	az, err := f.core.Authorizations.Get(f.ctx, order.Authorizations[0])
	if err != nil {
		t.Fatal(err)
	}
	if hits := f.fake.ValidationHits(az.Challenges[0].Token); hits != 1 {
		t.Fatalf("ValidationHits = %d, want 1", hits)
	}
	if hits := f.fake.ValidationHits("no-such-token"); hits != 0 {
		t.Fatalf("ValidationHits of an unknown token = %d", hits)
	}

	if _, err := f.core.Orders.New(f.ctx, []string{"x.example.com"}, &api.OrderOptions{Profile: "nope"}); err == nil {
		t.Fatal("an unknown profile was accepted")
	}
}

func TestClockNeverSleeps(t *testing.T) {
	t.Parallel()
	c := NewClock(epoch)
	ch := c.After(time.Hour)
	c.Advance(30 * time.Minute)
	select {
	case <-ch:
		t.Fatal("a timer fired before it was due")
	default:
	}
	if got := c.Now(); !got.Equal(epoch.Add(30 * time.Minute)) {
		t.Fatalf("Now = %v", got)
	}
	c.Advance(30 * time.Minute)
	select {
	case at := <-ch:
		if !at.Equal(epoch.Add(time.Hour)) {
			t.Fatalf("timer fired with %v", at)
		}
	default:
		t.Fatal("a due timer did not fire on Advance")
	}

	// Tick waits for something to arm a timer, so a goroutine that waits on the clock is never
	// raced past: the test learns it is waiting from the announcement, not from a delay.
	c = NewClock(epoch)
	woke := make(chan time.Time, 1)
	go func() { woke <- <-c.After(10 * time.Minute) }()
	c.Tick(t, 10*time.Minute)
	if got := testsupport.Await(t, "the waiter to wake", woke); !got.Equal(epoch.Add(10 * time.Minute)) {
		t.Fatalf("waiter woke at %v", got)
	}
}

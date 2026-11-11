// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/acme/dns/dnstest"
	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The DNS-01 routes of /api/https (phase 15, CHAL-01, CHAL-02, CHAL-04, CHAL-05, D-02 to D-09).

// dnsAPIToken is the one token the fake provider accepts; the tests look for it in every answer.
const dnsAPIToken = "cf-token-7f3a91c2d04b8e65" //nolint:gosec // G101: a recognisable fake token, so a test can prove it never leaks

// dnsAPIWrongToken is a well-formed token the fake provider refuses.
const dnsAPIWrongToken = "a-token-the-provider-refuses" //nolint:gosec // G101: a recognisable fake token, not a credential

// dnsAPINS is the host name the fake zone's name server goes by; the checker maps it to the responder.
const dnsAPINS = "ns1.tierplan-test.net"

// dnsFix is the API over a real manager whose DNS provider is a fake and whose DNS answers come from a
// local responder.
type dnsFix struct {
	*acmeFix
	prov *dnstest.FakeProvider
	srv  *acmetest.DNSServer
}

// newDNSFix builds the API over a fake CA that offers challenge ("" is http-01) and a fake provider that
// holds example.com and accepts dnsAPIToken as the cloudflare provider.
func newDNSFix(t *testing.T, challenge string, o acmeOpts) *dnsFix {
	t.Helper()
	return newDNSFixWith(t, challenge, o, nil)
}

// newDNSFixWith is newDNSFix with the provider factory passed through wrap when it is not nil.
func newDNSFixWith(t *testing.T, challenge string, o acmeOpts, wrap func(dns.Factory) dns.Factory) *dnsFix {
	t.Helper()
	prov := dnstest.NewFakeProvider()
	srv := acmetest.StartDNSServer(t)
	prov.Sink = srv
	prov.AddZone("example.com", dnsAPINS)
	if o.dns == nil {
		o.dns = &acme.DNSDeps{}
	}
	factory := prov.Factory(dnsAPIToken)
	if wrap != nil {
		factory = wrap(factory)
	}
	o.dns.Providers = map[string]dns.Factory{"cloudflare": factory}
	o.dns.NameserverAddrs = func(context.Context, string) ([]string, error) { return []string{srv.Addr()}, nil }
	o.dns.ResolvConf = func() ([]string, error) { return []string{srv.Addr()}, nil }
	if challenge == acme.ChallengeDNS01 {
		o.fake.ChallengeType = acme.ChallengeDNS01
		o.fake.LookupTXT = func(fqdn string) []string {
			var out []string
			for _, r := range prov.Records() {
				if strings.EqualFold(r.Name, fqdn) {
					out = append(out, r.Value())
				}
			}
			return out
		}
	}
	return &dnsFix{acmeFix: newACMEFix(t, o), prov: prov, srv: srv}
}

// validationAnswer is the answer of PUT /api/https/acme/validation.
type validationAnswer struct {
	Saved         bool                        `json:"saved"`
	Test          *api.HTTPSACMETokenTestBody `json:"test"`
	ChallengeType string                      `json:"challengeType"`
	TokenSaved    bool                        `json:"tokenSaved"`
	Resolvers     []string                    `json:"resolvers"`
}

// putValidation sends the validation save as the admin.
func (f *dnsFix) putValidation(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, http.MethodPut, "/api/https/acme/validation", body)
}

// decodeValidation requires a 200 and decodes the answer.
func decodeValidation(t *testing.T, rec *httptest.ResponseRecorder) validationAnswer {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT validation: %d %s, want 200", rec.Code, rec.Body)
	}
	var got validationAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode the answer: %v: %s", err, rec.Body)
	}
	return got
}

func TestHTTPSValidationSavesAndTheStatusShowsTokenSaved(t *testing.T) {
	t.Parallel()
	f := newDNSFix(t, "", acmeOpts{})

	before := f.status(t).ACME
	if before.DNS.Provider != "cloudflare" || before.DNS.TokenSaved || before.ChallengeType != "http-01" {
		t.Fatalf("before the save: dns %+v challenge %q, want cloudflare, no token, http-01", before.DNS, before.ChallengeType)
	}
	if before.DNS.Resolvers == nil || before.DNS.LeftOver == nil {
		t.Errorf("dns lists = %+v, want empty arrays and not null", before.DNS)
	}

	rec := f.putValidation(t, `{"challengeType":"dns-01","token":"`+dnsAPIToken+`"}`)
	got := decodeValidation(t, rec)

	if !got.Saved || got.Test == nil || !got.Test.Passed || got.ChallengeType != "dns-01" || !got.TokenSaved {
		t.Fatalf("answer = %+v, want saved after a passed test, dns-01 and a token saved", got)
	}
	if len(got.Test.Names) != 1 || got.Test.Names[0].Name != "tierplan.example.com" || got.Test.Names[0].Zone != "example.com" {
		t.Fatalf("test names = %+v, want the public host in example.com", got.Test.Names)
	}
	for i, id := range []string{"zone", "created", "read", "deleted"} {
		if s := got.Test.Names[0].Steps[i]; s.ID != id || s.State != "done" {
			t.Errorf("step %d = %+v, want %s done", i, s, id)
		}
	}
	if got.Test.Names[0].Info.State != "seen" {
		t.Errorf("info = %+v, want the record seen", got.Test.Names[0].Info)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}

	after := f.get(t, "/api/https/status")
	if strings.Contains(rec.Body.String(), dnsAPIToken) || strings.Contains(after.Body.String(), dnsAPIToken) {
		t.Error("a response holds the token")
	}
	var st api.HTTPSStatusBody
	if err := json.Unmarshal(after.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.ACME.DNS.TokenSaved || st.ACME.ChallengeType != "dns-01" {
		t.Errorf("status dns %+v challenge %q, want a token saved and dns-01", st.ACME.DNS, st.ACME.ChallengeType)
	}
	if recs := f.prov.Records(); len(recs) != 0 {
		t.Errorf("the provider still holds %+v after the test", recs)
	}
	if n := f.fake.Count(acmetest.EndpointNewOrder); n != 0 {
		t.Errorf("the CA saw %d orders from a save, want none", n)
	}
}

// sendDNS sends a DNS route request as the admin.
func (f *dnsFix) sendDNS(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, method, path, body)
}

// problemValue is a problem body with the value of its errors, which the shared problemBody leaves out.
type problemValue struct {
	Type   string `json:"type"`
	Errors []struct {
		Location string `json:"location"`
		Message  string `json:"message"`
		Value    any    `json:"value"`
	} `json:"errors"`
}

func decodeProblemValue(t *testing.T, rec *httptest.ResponseRecorder) problemValue {
	t.Helper()
	var p problemValue
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("not a problem body: %v: %s", err, rec.Body)
	}
	return p
}

// requireNoStore fails the test unless the answer is not to be cached.
func requireNoStore(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q on %d, want no-store", got, rec.Code)
	}
}

func TestHTTPSDNSTestToken(t *testing.T) {
	t.Parallel()
	f := newDNSFix(t, "", acmeOpts{})

	rec := f.sendDNS(t, http.MethodPost, "/api/https/acme/dns/test", `{"token":"`+dnsAPIToken+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST dns/test: %d %s, want 200", rec.Code, rec.Body)
	}
	requireNoStore(t, rec)
	var good api.HTTPSACMETokenTestBody
	if err := json.Unmarshal(rec.Body.Bytes(), &good); err != nil {
		t.Fatal(err)
	}
	if !good.Passed || len(good.Names) != 1 || good.Names[0].Error != nil || len(good.Names[0].Steps) != 4 {
		t.Fatalf("a good token: %+v, want one name that passed four rows with no error", good)
	}

	rec = f.sendDNS(t, http.MethodPost, "/api/https/acme/dns/test", `{"token":"`+dnsAPIWrongToken+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST dns/test with a refused token: %d %s, want 200", rec.Code, rec.Body)
	}
	var bad api.HTTPSACMETokenTestBody
	if err := json.Unmarshal(rec.Body.Bytes(), &bad); err != nil {
		t.Fatal(err)
	}
	if bad.Passed || len(bad.Names) != 1 {
		t.Fatalf("a refused token: %+v, want one name and passed false", bad)
	}
	n := bad.Names[0]
	if n.Error == nil || n.Error.Code != "dns_token_invalid" || n.Error.Host != "tierplan.example.com" {
		t.Errorf("error = %+v, want dns_token_invalid on the public host", n.Error)
	}
	if n.Steps[0].State != "failed" || n.Steps[0].Code != "dns_token_invalid" || n.Steps[1].State != "skipped" {
		t.Errorf("steps = %+v, want the zone row failed and the rest skipped", n.Steps)
	}
	if n.Info.State != "not_checked" {
		t.Errorf("info = %+v, want not_checked", n.Info)
	}

	// Nothing was stored and no order was placed.
	if st := f.status(t).ACME; st.DNS.TokenSaved {
		t.Error("a test stored the token")
	}
	if n := f.fake.Count(acmetest.EndpointNewOrder); n != 0 {
		t.Errorf("the CA saw %d orders from a test, want none", n)
	}
	if recs := f.prov.Records(); len(recs) != 0 {
		t.Errorf("the provider still holds %+v", recs)
	}
}

func TestHTTPSDNSTestTokenUsesTheSavedTokenWhenNoneIsTyped(t *testing.T) {
	t.Parallel()
	f := newDNSFix(t, "", acmeOpts{})
	acmeProblemOf(t, f.sendDNS(t, http.MethodPost, "/api/https/acme/dns/test", `{}`), 422, "acme_dns_token_required")

	decodeValidation(t, f.putValidation(t, `{"challengeType":"http-01","token":"`+dnsAPIToken+`"}`))
	rec := f.sendDNS(t, http.MethodPost, "/api/https/acme/dns/test", `{}`)
	var got api.HTTPSACMETokenTestBody
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || !got.Passed {
		t.Fatalf("testing the saved token: %d %s, want 200 and passed", rec.Code, rec.Body)
	}
}

func TestHTTPSValidationTestFailureSavesNothing(t *testing.T) {
	t.Parallel()
	f := newDNSFix(t, "", acmeOpts{})
	decodeValidation(t, f.putValidation(t, `{"challengeType":"dns-01","token":"`+dnsAPIToken+`","resolvers":["`+f.srv.Addr()+`"]}`))
	before := f.status(t).ACME

	rec := f.putValidation(t, `{"challengeType":"http-01","token":"`+dnsAPIWrongToken+`","resolvers":[]}`)
	got := decodeValidation(t, rec)

	if got.Saved || got.Test == nil || got.Test.Passed || got.Test.Names[0].Error == nil || got.Test.Names[0].Error.Code != "dns_token_invalid" {
		t.Fatalf("answer = %+v, want saved false and the refused token's test", got)
	}
	if got.ChallengeType != "dns-01" || !got.TokenSaved || len(got.Resolvers) != 1 {
		t.Errorf("answer settings = %+v, want the previous ones", got)
	}
	after := f.status(t).ACME
	if after.ChallengeType != before.ChallengeType || after.DNS.TokenSaved != before.DNS.TokenSaved ||
		strings.Join(after.DNS.Resolvers, ",") != strings.Join(before.DNS.Resolvers, ",") {
		t.Errorf("status after = %q %+v, before = %q %+v, want it unchanged", after.ChallengeType, after.DNS, before.ChallengeType, before.DNS)
	}
	// The saved token still works.
	rec = f.sendDNS(t, http.MethodPost, "/api/https/acme/dns/test", `{}`)
	var kept api.HTTPSACMETokenTestBody
	if json.Unmarshal(rec.Body.Bytes(), &kept) != nil || !kept.Passed {
		t.Errorf("the saved token no longer passes: %d %s", rec.Code, rec.Body)
	}
}

func TestHTTPSValidationProblems(t *testing.T) {
	t.Parallel()
	f := newDNSFix(t, "", acmeOpts{})

	rec := f.putValidation(t, `{"challengeType":"dns-01"}`)
	p := decodeProblemValue(t, rec)
	acmeProblemOf(t, rec, 422, "acme_dns_token_required")
	if len(p.Errors) == 0 || p.Errors[0].Location != "body.token" {
		t.Errorf("errors = %+v, want the first at body.token", p.Errors)
	}

	rec = f.putValidation(t, `{"challengeType":"dns-01","token":"`+dnsAPIToken+`","resolvers":["bad/entry"]}`)
	acmeProblemOf(t, rec, 422, "acme_dns_resolver_invalid")
	p = decodeProblemValue(t, rec)
	if len(p.Errors) == 0 || p.Errors[0].Location != "body.resolvers" || p.Errors[0].Value != "bad/entry" {
		t.Errorf("errors = %+v, want the first at body.resolvers with the entry bad/entry as the value", p.Errors)
	}
	if strings.Contains(rec.Body.String(), dnsAPIToken) {
		t.Error("the problem holds the token")
	}

	rec = f.sendDNS(t, http.MethodPost, "/api/https/acme/dns/test", `{"token":"`+dnsAPIToken+`","resolvers":["a b"]}`)
	acmeProblemOf(t, rec, 422, "acme_dns_resolver_invalid")

	rec = f.putValidation(t, `{"challengeType":"dns-01","token":"`+dnsAPIToken+`","resolvers":["1.1.1.1","1.0.0.1","8.8.8.8","8.8.4.4","9.9.9.9","149.112.112.112"]}`)
	acmeProblemOf(t, rec, 422, "acme_dns_resolver_invalid")

	if st := f.status(t).ACME; st.DNS.TokenSaved || st.ChallengeType != "http-01" {
		t.Errorf("a refused save changed the state: %+v", st)
	}

	// A token over the size limit fails its test as an invalid token with no request made, and is never
	// echoed: a schema violation would answer with the offending value.
	big := strings.Repeat("t", 1100)
	calls := len(f.prov.Calls())
	rec = f.putValidation(t, `{"challengeType":"dns-01","token":"`+big+`"}`)
	if strings.Contains(rec.Body.String(), big[:200]) {
		t.Error("PUT validation echoed the oversized token")
	}
	if got := decodeValidation(t, rec); got.Saved || got.Test == nil || got.Test.Passed || got.Test.Names[0].Error == nil || got.Test.Names[0].Error.Code != "dns_token_invalid" {
		t.Errorf("an oversized token: %+v, want saved false and dns_token_invalid", got)
	}
	rec = f.sendDNS(t, http.MethodPost, "/api/https/acme/dns/test", `{"token":"`+big+`"}`)
	if strings.Contains(rec.Body.String(), big[:200]) {
		t.Error("POST dns/test echoed the oversized token")
	}
	var tested api.HTTPSACMETokenTestBody
	if err := json.Unmarshal(rec.Body.Bytes(), &tested); err != nil || tested.Passed || tested.Names[0].Error == nil || tested.Names[0].Error.Code != "dns_token_invalid" {
		t.Errorf("an oversized token test: %d %s, want passed false and dns_token_invalid", rec.Code, rec.Body)
	}
	if got := len(f.prov.Calls()); got != calls {
		t.Errorf("the provider saw %d calls for an oversized token, want none", got-calls)
	}
}

func TestHTTPSDNSRemoveToken(t *testing.T) {
	t.Parallel()
	f := newDNSFix(t, "", acmeOpts{})
	decodeValidation(t, f.putValidation(t, `{"challengeType":"dns-01","token":"`+dnsAPIToken+`"}`))

	rec := f.sendDNS(t, http.MethodDelete, "/api/https/acme/dns/token", "")
	acmeProblemOf(t, rec, 409, "acme_dns_token_in_use")
	if !f.status(t).ACME.DNS.TokenSaved {
		t.Fatal("the token was removed while DNS-01 is the saved method")
	}

	decodeValidation(t, f.putValidation(t, `{"challengeType":"http-01"}`))
	rec = f.sendDNS(t, http.MethodDelete, "/api/https/acme/dns/token", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE token with http-01 saved: %d %s, want 204", rec.Code, rec.Body)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("a 204 carries a body: %s", rec.Body)
	}
	if st := f.status(t).ACME; st.DNS.TokenSaved || st.ChallengeType != "http-01" {
		t.Errorf("after the removal: %+v, want no token and http-01", st)
	}
	// Choosing DNS-01 again needs a token again.
	acmeProblemOf(t, f.putValidation(t, `{"challengeType":"dns-01"}`), 422, "acme_dns_token_required")
}

// The three routes are refused, with the reason, while Let's Encrypt cannot be changed.
func TestHTTPSDNSRoutesRefusedWhenTheStateCannotChange(t *testing.T) {
	t.Parallel()
	routes := []struct{ method, path, body string }{
		{http.MethodPut, "/api/https/acme/validation", `{"challengeType":"http-01"}`},
		{http.MethodPost, "/api/https/acme/dns/test", `{"token":"` + dnsAPIToken + `"}`},
		{http.MethodDelete, "/api/https/acme/dns/token", ""},
	}
	check := func(t *testing.T, f *dnsFix, ptype string) {
		t.Helper()
		for _, r := range routes {
			acmeProblemOf(t, f.sendDNS(t, r.method, r.path, r.body), 409, ptype)
		}
	}

	t.Run("a run is in progress", func(t *testing.T) {
		t.Parallel()
		f := newDNSFix(t, "", acmeOpts{})
		f.register(t, "")
		f.fake.HoldFinalize()
		if _, err := f.mgr.Issue(context.Background(), "user:test"); err != nil {
			t.Fatalf("Issue: %v", err)
		}
		check(t, f, "acme_run_active")
	})
	t.Run("paused after a restore", func(t *testing.T) {
		t.Parallel()
		f := newDNSFix(t, "", acmeOpts{})
		f.pause(t)
		check(t, f, "acme_paused")
	})
	t.Run("certificate files", func(t *testing.T) {
		t.Parallel()
		f := newDNSFix(t, "", acmeOpts{certFiles: true})
		check(t, f, "acme_cert_files")
	})
	t.Run("a token test holds the slot", func(t *testing.T) {
		t.Parallel()
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		f := newDNSFixWith(t, "", acmeOpts{}, func(inner dns.Factory) dns.Factory {
			return func(token string) dns.Provider {
				return &gateProvider{Provider: inner(token), enter: func() { once.Do(func() { close(entered) }) }, release: release}
			}
		})
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			done <- f.sendDNS(t, http.MethodPost, "/api/https/acme/dns/test", `{"token":"`+dnsAPIToken+`"}`)
		}()
		testsupport.Await(t, "the token test to reach the provider", entered)
		check(t, f, "acme_run_active")
		close(release)
		select {
		case rec := <-done:
			if rec.Code != http.StatusOK {
				t.Errorf("the held test: %d %s, want 200", rec.Code, rec.Body)
			}
		case <-t.Context().Done():
		}
	})
}

// gateProvider stops at the first zone lookup until release is closed.
type gateProvider struct {
	dns.Provider
	enter   func()
	release chan struct{}
}

func (g *gateProvider) Zone(ctx context.Context, fqdn string) (dns.Zone, error) {
	g.enter()
	select {
	case <-g.release:
	case <-ctx.Done():
	}
	return g.Provider.Zone(ctx, fqdn)
}

// The token the admin types appears in no answer, problem, header or contract (T-15-26).
func TestHTTPSResponsesNeverCarryTheDNSToken(t *testing.T) {
	t.Parallel()
	f := newDNSFix(t, "", acmeOpts{})
	secrets := []string{dnsAPIToken, dnsAPIWrongToken}
	spec, err := os.ReadFile("../../openapi.json")
	if err != nil {
		t.Fatal(err)
	}

	answers := map[string]*httptest.ResponseRecorder{
		"save, good":      f.putValidation(t, `{"challengeType":"dns-01","token":"`+dnsAPIToken+`"}`),
		"save, refused":   f.putValidation(t, `{"challengeType":"http-01","token":"`+dnsAPIWrongToken+`"}`),
		"save, bad entry": f.putValidation(t, `{"challengeType":"dns-01","token":"`+dnsAPIToken+`","resolvers":["bad/entry"]}`),
		"test, good":      f.sendDNS(t, http.MethodPost, "/api/https/acme/dns/test", `{"token":"`+dnsAPIToken+`"}`),
		"test, refused":   f.sendDNS(t, http.MethodPost, "/api/https/acme/dns/test", `{"token":"`+dnsAPIWrongToken+`"}`),
		"test, saved":     f.sendDNS(t, http.MethodPost, "/api/https/acme/dns/test", `{}`),
		"remove, in use":  f.sendDNS(t, http.MethodDelete, "/api/https/acme/dns/token", ""),
		"status":          f.get(t, "/api/https/status"),
	}
	for name, rec := range answers {
		if rec.Body.Len() == 0 && rec.Code != http.StatusNoContent {
			t.Errorf("%s: empty body on %d", name, rec.Code)
		}
		if rec.Code < 300 && name != "status" {
			requireNoStore(t, rec)
		}
		all := rec.Body.String()
		for k, v := range rec.Header() {
			all += k + ": " + strings.Join(v, ",") + "\n"
		}
		for _, secret := range secrets {
			if strings.Contains(all, secret) {
				t.Errorf("%s holds a token: %s", name, all)
			}
		}
	}
	for _, secret := range secrets {
		if strings.Contains(string(spec), secret) {
			t.Errorf("openapi.json holds the token %q", secret)
		}
	}
}

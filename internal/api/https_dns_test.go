// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/acme/dns/dnstest"
	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/config"
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

// dnsStepIDs are the steps of a DNS-01 run, in order.
var dnsStepIDs = []string{"account", "order", "challenge", "propagation", "validate", "finalize", "stored", "cleanup"}

// seedLeftOver stores a TXT record TierPlan could not delete yet.
func (f *dnsFix) seedLeftOver(t *testing.T, id, name string, created time.Time, class string, lastTry, nextTry time.Time) {
	t.Helper()
	err := f.store.InsertDNSRecord(context.Background(), acme.DNSRecord{
		ID: id, Provider: "fake", ZoneID: "zone-1", RecordID: "rec-" + id, Name: name, Value: "v-" + id,
		OwnerKind: acme.OwnerOrder, OwnerID: "order-1", CreatedAt: created, Attempts: 1,
		LastErrorClass: class, LastTryAt: lastTry, NextTryAt: nextTry,
	})
	if err != nil {
		t.Fatalf("InsertDNSRecord: %v", err)
	}
}

func TestHTTPSStatusShowsTheDNSView(t *testing.T) {
	t.Parallel()
	f := newDNSFix(t, "", acmeOpts{})
	f.register(t, "")
	decodeValidation(t, f.putValidation(t, `{"challengeType":"dns-01","token":"`+dnsAPIToken+`"}`))
	// Saved with no token typed, so no test asks these resolvers over the network.
	decodeValidation(t, f.putValidation(t, `{"challengeType":"dns-01","resolvers":["1.1.1.1","dns.example.net:5353"]}`))
	base := acmeNow
	// Inserted newest first: the status lists the oldest first.
	f.seedLeftOver(t, "b", "_acme-challenge.b.example.com", base.Add(-time.Hour), "dns_unavailable", base.Add(-time.Minute), base.Add(time.Hour))
	f.seedLeftOver(t, "a", "_acme-challenge.a.example.com", base.Add(-2*time.Hour), "dns_rate_limited", base.Add(-2*time.Minute), base.Add(30*time.Minute))

	got := f.status(t).ACME
	if !got.ValidationUntested {
		t.Error("validationUntested is false after the method changed on an install with an account")
	}
	if got.SuggestDNS01 != nil {
		t.Errorf("suggestDns01 = %+v, want none for dns-01", got.SuggestDNS01)
	}
	if !slices.Equal(got.DNS.Resolvers, []string{"1.1.1.1:53", "dns.example.net:5353"}) || !got.DNS.TokenSaved || got.DNS.Provider != "cloudflare" {
		t.Errorf("dns = %+v, want the normalised resolvers and a saved token", got.DNS)
	}
	if len(got.DNS.LeftOver) != 2 {
		t.Fatalf("leftOver = %+v, want two items", got.DNS.LeftOver)
	}
	a, b := got.DNS.LeftOver[0], got.DNS.LeftOver[1]
	if a.Name != "_acme-challenge.a.example.com" || b.Name != "_acme-challenge.b.example.com" {
		t.Errorf("leftOver names = %q, %q, want the oldest first", a.Name, b.Name)
	}
	if a.RecordID != "rec-a" || a.Code != "dns_rate_limited" || a.LastTryAt == nil || !a.LastTryAt.Equal(base.Add(-2*time.Minute)) ||
		a.NextTryAt == nil || !a.NextTryAt.Equal(base.Add(30*time.Minute)) || a.RetryPaused || a.PausedReason != "" {
		t.Errorf("leftOver[0] = %+v, want the class, both tries and no pause", a)
	}

	// After a restore no try is promised, and the reason is named.
	f.pause(t)
	paused := f.status(t).ACME.DNS.LeftOver
	for _, it := range paused {
		if !it.RetryPaused || it.PausedReason != "restore" || it.NextTryAt != nil {
			t.Errorf("a paused item = %+v, want retryPaused, the restore reason and no next try", it)
		}
	}
}

func TestHTTPSStatusCarriesTheDNSFactsOfTheLastError(t *testing.T) {
	t.Parallel()
	f := newDNSFix(t, "", acmeOpts{})
	st, err := f.store.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	st.LastErrorAt, st.LastErrorClass = acmeNow, "dns_not_visible"
	st.LastErrorDetail = `{"v":1,"host":"tierplan.example.com","zone":"example.com","target":"acme.dns.example.net","resolvers":["1.1.1.1:53","8.8.8.8:53"],"detail":"not seen"}`
	if err := f.store.SaveState(context.Background(), st); err != nil {
		t.Fatal(err)
	}

	e := f.status(t).ACME.LastError
	if e == nil || e.Code != "dns_not_visible" || e.Host != "tierplan.example.com" || e.Zone != "example.com" ||
		e.Target != "acme.dns.example.net" || !slices.Equal(e.Resolvers, []string{"1.1.1.1:53", "8.8.8.8:53"}) {
		t.Errorf("lastError = %+v, want dns_not_visible with the host, zone, target and resolvers", e)
	}
}

func TestHTTPSStatusSuggestsDNS01ForAPrivateHost(t *testing.T) {
	t.Parallel()
	lookup := func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.168.1.20")}, nil
	}
	f := newDNSFix(t, "", acmeOpts{realDirectory: true, dns: &acme.DNSDeps{LookupIP: lookup}})

	got := f.status(t).ACME
	if got.SuggestDNS01 == nil || got.SuggestDNS01.Host != "tierplan.example.com" || got.SuggestDNS01.Address != "192.168.1.20" {
		t.Errorf("suggestDns01 = %+v, want the host and its private address", got.SuggestDNS01)
	}

	// Left out unless the manager suggests it: a public address, or DNS-01 already chosen.
	plain := newACMEFix(t, acmeOpts{})
	if s := plain.status(t).ACME.SuggestDNS01; s != nil {
		t.Errorf("suggestDns01 = %+v on a fixture whose host does not resolve privately", s)
	}
	if body := plain.get(t, "/api/https/status").Body.String(); strings.Contains(body, "suggestDns01") {
		t.Errorf("the status names suggestDns01 when there is none: %s", body)
	}
}

// runStream starts the issue as the admin, opens the stream and returns the connection and the job.
func (f *dnsFix) streamRun(t *testing.T, jobID string) *sseConn {
	t.Helper()
	c := openSSE(t, f.authFixture, "/api/https/acme/runs/"+jobID+"/stream", f.cookie, "")
	if c.status0() != http.StatusOK {
		t.Fatalf("stream status = %d", c.status0())
	}
	return c
}

// followRun reads a stream to its done frame and returns the snapshot, the last state of every step
// from the item frames and the done frame.
func followRun(t *testing.T, c *sseConn) (snap, done api.HTTPSACMERunFrameBody) {
	t.Helper()
	first := nextFrame(t, c)
	if first.event != "snapshot" {
		t.Fatalf("first frame = %+v, want a snapshot", first)
	}
	snap = unmarshalFrame[api.HTTPSACMERunFrameBody](t, first.data)
	for {
		fr := nextFrame(t, c)
		switch fr.event {
		case "item":
			if it := unmarshalFrame[api.HTTPSACMEStepBody](t, fr.data); !slices.Contains(dnsStepIDs, it.ID) {
				t.Errorf("item frame for an unknown step: %+v", it)
			}
		case "done":
			c.awaitEnd(t, "the done frame")
			return snap, done0(t, fr.data)
		default:
			t.Fatalf("unexpected frame %+v", fr)
		}
	}
}

func done0(t *testing.T, data string) api.HTTPSACMERunFrameBody {
	t.Helper()
	return unmarshalFrame[api.HTTPSACMERunFrameBody](t, data)
}

func stepsByID(items []api.HTTPSACMEStepBody) map[string]api.HTTPSACMEStepBody {
	m := map[string]api.HTTPSACMEStepBody{}
	for _, it := range items {
		m[it.ID] = it
	}
	return m
}

func TestACMERunStreamCarriesTheDNSSteps(t *testing.T) {
	t.Parallel()
	f := newDNSFix(t, acme.ChallengeDNS01, acmeOpts{})
	f.register(t, "")
	decodeValidation(t, f.putValidation(t, `{"challengeType":"dns-01","token":"`+dnsAPIToken+`"}`))
	release := f.fake.HoldFinalize()

	jobID := startRun(t, f.issue(t))
	if run := f.status(t).ACME.ActiveRun; run == nil || run.Challenge != "dns-01" {
		t.Errorf("activeRun = %+v, want a run that says dns-01", run)
	}
	c := f.streamRun(t, jobID)
	release()
	snap, done := followRun(t, c)

	if snap.Challenge != "dns-01" || done.Challenge != "dns-01" {
		t.Errorf("challenge = %q in the snapshot and %q in the done frame, want dns-01", snap.Challenge, done.Challenge)
	}
	if got := stepIDsOf(done.Items); !slices.Equal(got, dnsStepIDs) {
		t.Fatalf("done steps = %v, want %v", got, dnsStepIDs)
	}
	if done.State != "done" {
		t.Errorf("done state = %q, want done", done.State)
	}
	steps := stepsByID(done.Items)
	if p := steps["propagation"]; p.Record != "_acme-challenge.tierplan.example.com" || p.Host != "tierplan.example.com" {
		t.Errorf("propagation = %+v, want the record and host it waited for", p)
	}
	if c := steps["challenge"]; c.Record != "_acme-challenge.tierplan.example.com" || c.Host != "tierplan.example.com" || c.Target != "" {
		t.Errorf("challenge = %+v, want the record and host and no delegation", c)
	}
	if cl := steps["cleanup"]; cl.State != "done" || cl.Code != "" || cl.LeftOver != 0 {
		t.Errorf("cleanup = %+v, want done with nothing left over", cl)
	}
	if run := f.status(t).ACME.LastRun; run == nil || run.Challenge != "dns-01" || run.Outcome != "succeeded" {
		t.Errorf("lastRun = %+v, want a dns-01 run that succeeded", run)
	}
	if st := f.status(t).ACME; st.ValidationUntested {
		t.Error("validationUntested is still true after a run that succeeded")
	}
}

func TestACMERunStreamReportsALeftOverRecord(t *testing.T) {
	t.Parallel()
	f := newDNSFix(t, acme.ChallengeDNS01, acmeOpts{})
	f.register(t, "")
	decodeValidation(t, f.putValidation(t, `{"challengeType":"dns-01","token":"`+dnsAPIToken+`"}`))
	f.prov.FailDelete(-1, &dns.Error{Provider: "fake", Op: "delete record", Class: dns.ClassUnavailable})

	jobID := startRun(t, f.issue(t))
	f.awaitRun(t, jobID)
	_, done := followRun(t, f.streamRun(t, jobID))

	cl := stepsByID(done.Items)["cleanup"]
	if cl.State != "done" || cl.Code != "left_over" || cl.LeftOver != 1 {
		t.Errorf("cleanup = %+v, want done with code left_over and one record left over", cl)
	}
	if done.State != "done" {
		t.Errorf("done state = %q, want done: a record that stays is not a failed run", done.State)
	}
	left := f.status(t).ACME.DNS.LeftOver
	if len(left) != 1 || left[0].Name != "_acme-challenge.tierplan.example.com" || left[0].Code == "" {
		t.Errorf("leftOver = %+v, want the record with the class of its last delete error", left)
	}
}

func TestACMERunStreamDelegatedStepCarriesTheTarget(t *testing.T) {
	t.Parallel()
	f := newDNSFix(t, acme.ChallengeDNS01, acmeOpts{})
	f.prov.AddZone("dns.example.net", dnsAPINS)
	f.srv.SetCNAME("_acme-challenge.tierplan.example.com", "tierplan.dns.example.net")
	f.register(t, "")
	decodeValidation(t, f.putValidation(t, `{"challengeType":"dns-01","token":"`+dnsAPIToken+`"}`))

	jobID := startRun(t, f.issue(t))
	f.awaitRun(t, jobID)
	_, done := followRun(t, f.streamRun(t, jobID))

	ch := stepsByID(done.Items)["challenge"]
	if ch.Target != "tierplan.dns.example.net" {
		t.Errorf("challenge = %+v, want the delegation target", ch)
	}
}

// seedValidation stores the validation settings the way an earlier save did, without going through
// the API, so a test can start from a stored value a variable then disagrees with.
func (f *dnsFix) seedValidation(t *testing.T, challenge string, resolvers []string) {
	t.Helper()
	if err := f.store.SaveValidation(context.Background(), acme.ValidationWrite{
		ChallengeType: challenge, Provider: "cloudflare", Resolvers: resolvers, Now: f.clock.Now(),
	}); err != nil {
		t.Fatalf("seed the validation settings: %v", err)
	}
}

// seedOpenOrder stores an open order, so a test can see whether a save closed it.
func (f *dnsFix) seedOpenOrder(t *testing.T) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := f.clock.Now()
	if err := f.store.InsertOrder(context.Background(), acme.Order{
		ID: "order-1", DirectoryURL: f.fake.DirectoryURL(), URL: f.fake.DirectoryURL() + "/order/1", Kind: acme.KindIssue,
		Names: []string{"tierplan.example.com"}, Stage: acme.StageCreated, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}, key); err != nil {
		t.Fatalf("seed an open order: %v", err)
	}
}

// storedState reads the stored Let's Encrypt state, which no overlay of the environment touches.
func (f *dnsFix) storedState(t *testing.T) acme.State {
	t.Helper()
	st, err := f.store.State(context.Background())
	if err != nil {
		t.Fatalf("read the stored state: %v", err)
	}
	return st
}

// A validation method fixed by TIERPLAN_ACME_CHALLENGE is reported with its variable, another method is
// refused, and the variable's own method changes nothing: not the stored method, not an open order (D-17).
func TestPutACMEValidationLockedChallenge(t *testing.T) {
	t.Parallel()
	f := newDNSFix(t, "", acmeOpts{env: config.ACMEEnv{Challenge: acme.ChallengeDNS01}})
	f.seedValidation(t, acme.ChallengeHTTP01, nil)
	f.seedOpenOrder(t)

	st := f.status(t).ACME
	lock := st.Locks.ChallengeType
	if lock == nil || lock.Var != "TIERPLAN_ACME_CHALLENGE" || lock.Value != "dns-01" || lock.Stored != "http-01" {
		t.Fatalf("locks.challengeType = %+v, want TIERPLAN_ACME_CHALLENGE, value dns-01, stored http-01", lock)
	}
	if st.ChallengeType != "dns-01" {
		t.Errorf("challengeType = %q, want the variable's dns-01", st.ChallengeType)
	}

	rec := f.putValidation(t, `{"challengeType":"http-01"}`)
	if rec.Code != http.StatusConflict || decodeProblemValue(t, rec).Type != "acme_challenge_locked" {
		t.Fatalf("PUT http-01: %d %s, want 409 acme_challenge_locked", rec.Code, rec.Body)
	}
	if got := f.storedState(t).ChallengeType; got != acme.ChallengeHTTP01 {
		t.Errorf("stored challenge after the refusal = %q, want http-01", got)
	}

	got := decodeValidation(t, f.putValidation(t, `{"challengeType":"dns-01"}`))
	if !got.Saved || got.ChallengeType != "dns-01" {
		t.Errorf("echo answer = %+v, want saved, reporting the variable's dns-01", got)
	}
	if got := f.storedState(t).ChallengeType; got != acme.ChallengeHTTP01 {
		t.Errorf("stored challenge after the echo = %q, want http-01: the variable's value is never stored", got)
	}
	if _, _, open, err := f.store.OpenOrder(context.Background()); err != nil || !open {
		t.Errorf("open order after the echo = %v (%v), want it left open", open, err)
	}
	if lock := f.status(t).ACME.Locks.ChallengeType; lock == nil || lock.Stored != "http-01" {
		t.Errorf("locks.challengeType after the echo = %+v, want stored http-01", lock)
	}
}

// The provider and its token lock together, the token is proved but never stored or shown, and a typed
// token or a removal is refused while a variable supplies it (SET-02, D-03, D-05, D-06, D-17).
func TestPutACMEValidationLockedDNSToken(t *testing.T) {
	t.Parallel()
	f := newDNSFix(t, "", acmeOpts{env: config.ACMEEnv{
		DNSProvider: "cloudflare", CloudflareToken: dnsAPIToken, TokenVar: config.EnvCloudflareTokenFile,
	}})
	ctx := context.Background()

	st := f.status(t).ACME
	if l := st.Locks.DNSProvider; l == nil || l.Var != "TIERPLAN_DNS_PROVIDER" || l.Value != "cloudflare" {
		t.Errorf("locks.dnsProvider = %+v, want TIERPLAN_DNS_PROVIDER fixing cloudflare", l)
	}
	if l := st.Locks.DNSToken; l == nil || l.Var != "TIERPLAN_CLOUDFLARE_API_TOKEN_FILE" || l.Value != "" || l.Stored != "" {
		t.Errorf("locks.dnsToken = %+v, want the variable's name and nothing else", l)
	}
	if st.Locks.ChallengeType != nil || st.Locks.Resolvers != nil {
		t.Errorf("locks = challenge %+v resolvers %+v, want them free", st.Locks.ChallengeType, st.Locks.Resolvers)
	}
	if st.DNS.TokenSource != "env" || st.DNS.TokenSaved {
		t.Errorf("dns = %+v, want the token from env and none saved", st.DNS)
	}

	// A typed token is refused before it is tested, and nothing is stored.
	rec := f.putValidation(t, `{"challengeType":"dns-01","token":"`+dnsAPIWrongToken+`"}`)
	if rec.Code != http.StatusConflict || decodeProblemValue(t, rec).Type != "acme_dns_token_locked" {
		t.Fatalf("PUT with a typed token: %d %s, want 409 acme_dns_token_locked", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), dnsAPIWrongToken) || strings.Contains(rec.Body.String(), dnsAPIToken) {
		t.Errorf("the refusal holds a token: %s", rec.Body)
	}
	if got := f.storedState(t).ChallengeType; got == acme.ChallengeDNS01 {
		t.Errorf("stored challenge = %q after a refused save, want it unchanged", got)
	}

	// With the environment's token alone DNS-01 can be saved, and nothing is sealed.
	got := decodeValidation(t, f.putValidation(t, `{"challengeType":"dns-01"}`))
	if !got.Saved || got.Test != nil || got.ChallengeType != "dns-01" || got.TokenSaved {
		t.Errorf("save = %+v, want dns-01 saved with no test and no token saved", got)
	}
	if saved, err := f.store.DNSTokenSaved(ctx, "cloudflare"); err != nil || saved {
		t.Errorf("DNSTokenSaved = %v (%v), want the environment's token never stored", saved, err)
	}

	// Test token proves the environment's token and stores nothing.
	rec = f.sendDNS(t, http.MethodPost, "/api/https/acme/dns/test", `{}`)
	var tested api.HTTPSACMETokenTestBody
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &tested) != nil || !tested.Passed {
		t.Fatalf("test with the environment's token: %d %s, want 200 and passed", rec.Code, rec.Body)
	}
	if saved, err := f.store.DNSTokenSaved(ctx, "cloudflare"); err != nil || saved {
		t.Errorf("DNSTokenSaved after a test = %v (%v), want none", saved, err)
	}
	rec = f.sendDNS(t, http.MethodPost, "/api/https/acme/dns/test", `{"token":"`+dnsAPIWrongToken+`"}`)
	if rec.Code != http.StatusConflict || decodeProblemValue(t, rec).Type != "acme_dns_token_locked" {
		t.Errorf("test with a typed token: %d %s, want 409 acme_dns_token_locked", rec.Code, rec.Body)
	}

	// The token cannot be removed while a variable supplies it.
	rec = f.sendDNS(t, http.MethodDelete, "/api/https/acme/dns/token", "")
	if rec.Code != http.StatusConflict || decodeProblemValue(t, rec).Type != "acme_dns_token_locked" {
		t.Errorf("DELETE token: %d %s, want 409 acme_dns_token_locked", rec.Code, rec.Body)
	}
	if recs := f.prov.Records(); len(recs) != 0 {
		t.Errorf("the provider still holds %+v", recs)
	}
}

// A well-formed token from the environment that the provider refuses shows as a failed test with the
// classified error; the page and the status keep working (D-06).
func TestPutACMEValidationLockedDNSTokenRefusedByTheProvider(t *testing.T) {
	t.Parallel()
	f := newDNSFix(t, "", acmeOpts{env: config.ACMEEnv{
		DNSProvider: "cloudflare", CloudflareToken: dnsAPIWrongToken, TokenVar: config.EnvCloudflareToken,
	}})

	rec := f.sendDNS(t, http.MethodPost, "/api/https/acme/dns/test", `{}`)

	var got api.HTTPSACMETokenTestBody
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("test: %d %s, want 200", rec.Code, rec.Body)
	}
	if got.Passed || len(got.Names) != 1 || got.Names[0].Error == nil || got.Names[0].Error.Code != "dns_token_invalid" {
		t.Errorf("test = %+v, want a failed name classified dns_token_invalid", got)
	}
	if strings.Contains(rec.Body.String(), dnsAPIWrongToken) {
		t.Errorf("the answer holds the token: %s", rec.Body)
	}
	if st := f.status(t).ACME; st.DNS.TokenSource != "env" {
		t.Errorf("tokenSource = %q, want env", st.DNS.TokenSource)
	}
}

// Resolvers fixed by TIERPLAN_DNS_RESOLVERS: the same list in the same order is an echo that stores
// nothing, and any other list is refused with nothing stored (D-17).
func TestPutACMEValidationLockedResolvers(t *testing.T) {
	t.Parallel()
	f := newDNSFix(t, "", acmeOpts{env: config.ACMEEnv{Resolvers: []string{"1.1.1.1:53", "9.9.9.9:53"}, ResolversSet: true}})
	f.seedValidation(t, acme.ChallengeHTTP01, []string{"8.8.8.8:53"})

	lock := f.status(t).ACME.Locks.Resolvers
	if lock == nil || lock.Var != "TIERPLAN_DNS_RESOLVERS" || lock.Value != "1.1.1.1:53,9.9.9.9:53" || lock.Stored != "8.8.8.8:53" {
		t.Fatalf("locks.resolvers = %+v, want the variable, its list and the stored list", lock)
	}

	for _, body := range []string{
		`{"challengeType":"http-01","resolvers":["9.9.9.9","1.1.1.1"]}`,
		`{"challengeType":"http-01","resolvers":["8.8.8.8"]}`,
		`{"challengeType":"http-01","resolvers":[]}`,
	} {
		rec := f.putValidation(t, body)
		if rec.Code != http.StatusConflict || decodeProblemValue(t, rec).Type != "acme_resolvers_locked" {
			t.Errorf("PUT %s: %d %s, want 409 acme_resolvers_locked", body, rec.Code, rec.Body)
		}
		if got := f.storedState(t).DNSResolvers; !slices.Equal(got, []string{"8.8.8.8:53"}) {
			t.Fatalf("stored resolvers after %s = %v, want them unchanged", body, got)
		}
	}

	got := decodeValidation(t, f.putValidation(t, `{"challengeType":"http-01","resolvers":["1.1.1.1","9.9.9.9"]}`))
	if !got.Saved || !slices.Equal(got.Resolvers, []string{"1.1.1.1:53", "9.9.9.9:53"}) {
		t.Errorf("echo answer = %+v, want saved, reporting the variable's list", got)
	}
	if got := f.storedState(t).DNSResolvers; !slices.Equal(got, []string{"8.8.8.8:53"}) {
		t.Errorf("stored resolvers after the echo = %v, want 8.8.8.8:53: the variable is never stored", got)
	}
}

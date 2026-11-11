// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/acme/dns/dnstest"
	"github.com/ianglennon/tierplan/internal/api"
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
	prov := dnstest.NewFakeProvider()
	srv := acmetest.StartDNSServer(t)
	prov.Sink = srv
	prov.AddZone("example.com", dnsAPINS)
	if o.dns == nil {
		o.dns = &acme.DNSDeps{}
	}
	o.dns.Providers = map[string]dns.Factory{"cloudflare": prov.Factory(dnsAPIToken)}
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

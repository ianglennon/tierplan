// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package cloudflare_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/acme/dns/cloudflare"
	"github.com/ianglennon/tierplan/internal/acme/dns/dnstest"
)

// sinkLog records what a fake Cloudflare tells its TXTSink, in order.
type sinkLog struct {
	mu     sync.Mutex
	events []string
}

func (s *sinkLog) AddTXT(fqdn, value string)    { s.add("add " + fqdn + " " + value) }
func (s *sinkLog) RemoveTXT(fqdn, value string) { s.add("remove " + fqdn + " " + value) }
func (s *sinkLog) add(e string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}
func (s *sinkLog) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

// newProvider is a Cloudflare provider on the fake with the fake's own token.
func newProvider(fake *dnstest.FakeCloudflare) dns.Provider {
	return cloudflare.New(cloudflare.Options{BaseURL: fake.URL(), HTTPClient: fake.Client(), UserAgent: "TierPlan/test"})(fake.Token)
}

// TestCloudflareZoneCreateReadDelete is the tracer: a token finds the zone for a challenge name,
// creates a commented TXT record, reads it back by its ID and deletes it by its ID, over TLS
// against the fake API.
func TestCloudflareZoneCreateReadDelete(t *testing.T) {
	ctx := context.Background()
	fake := dnstest.NewFakeCloudflare(t)
	zoneID := fake.AddZone("example.com", "ns1.example.net", "ns2.example.net")
	sink := &sinkLog{}
	fake.Sink = sink
	p := newProvider(fake)

	if p.Name() != "cloudflare" {
		t.Fatalf("Name = %q", p.Name())
	}
	fqdn := dns.ChallengeFQDN("tp.example.com")
	zone, err := p.Zone(ctx, fqdn)
	if err != nil {
		t.Fatalf("Zone: %v", err)
	}
	if zone.ID != zoneID || zone.Name != "example.com" {
		t.Fatalf("Zone = %+v, want id %s name example.com", zone, zoneID)
	}
	if len(zone.NameServers) != 2 || zone.NameServers[0] != "ns1.example.net" {
		t.Fatalf("Zone name servers = %v", zone.NameServers)
	}

	value := dns.TXTValue("token.thumbprint")
	comment := dns.RecordComment("11111111-1111-7111-8111-111111111111", "test-0a1b2c3d")
	ref, err := p.CreateTXT(ctx, zone, fqdn, value, comment)
	if err != nil {
		t.Fatalf("CreateTXT: %v", err)
	}
	if ref.ZoneID != zoneID || len(ref.RecordID) != 32 {
		t.Fatalf("RecordRef = %+v, want zone %s and a 32 character record id", ref, zoneID)
	}

	rec, err := p.GetTXT(ctx, ref)
	if err != nil {
		t.Fatalf("GetTXT: %v", err)
	}
	if rec.Content != value || rec.Comment != comment || rec.Name != fqdn {
		t.Fatalf("GetTXT = %+v, want content %q comment %q name %q", rec, value, comment, fqdn)
	}
	held := fake.Records()
	if len(held) != 1 || held[0].Content != `"`+value+`"` || held[0].ID != ref.RecordID {
		t.Fatalf("the fake holds %+v, want the one quoted record", held)
	}

	if err := p.DeleteTXT(ctx, ref); err != nil {
		t.Fatalf("DeleteTXT: %v", err)
	}
	if left := fake.Records(); len(left) != 0 {
		t.Fatalf("the fake still holds %+v after the delete", left)
	}
	want := []string{"add " + fqdn + " " + value, "remove " + fqdn + " " + value}
	if got := sink.all(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("sink saw %v, want %v", got, want)
	}

	for _, r := range fake.Requests() {
		if !r.Bearer || r.LegacyAuth {
			t.Fatalf("request %+v: only Authorization: Bearer may be sent", r)
		}
	}
}

const challengeName = "_acme-challenge.tp.example.com"

// withRecord returns a provider on a fake that holds example.com, the zone found for the challenge
// name and one record created in it, ready for a scripted get or delete.
func withRecord(t *testing.T) (*dnstest.FakeCloudflare, dns.Provider, dns.Zone, dns.RecordRef) {
	t.Helper()
	fake := dnstest.NewFakeCloudflare(t)
	fake.AddZone("example.com", "ns1.example.net")
	p := newProvider(fake)
	z, err := p.Zone(context.Background(), challengeName)
	if err != nil {
		t.Fatalf("Zone: %v", err)
	}
	ref, err := p.CreateTXT(context.Background(), z, challengeName, dns.TXTValue("k"), dns.RecordComment("i", "test-00000000"))
	if err != nil {
		t.Fatalf("CreateTXT: %v", err)
	}
	return fake, p, z, ref
}

// asDNS is err as a *dns.Error, failing the test when it is not one.
func asDNS(t *testing.T, err error) *dns.Error {
	t.Helper()
	var de *dns.Error
	if !errors.As(err, &de) {
		t.Fatalf("error %v is not a *dns.Error", err)
	}
	return de
}

// TestCloudflareClassifies checks every Cloudflare answer against the class the page explains
// (CHAL-02, D-19). The statuses and codes marked probed were read from the live API with dummy
// tokens (RESEARCH Pattern 6); the others are the documented or assumed ones, with a fallback.
func TestCloudflareClassifies(t *testing.T) {
	ctx := context.Background()
	zoneCall := func(p dns.Provider, _ dns.Zone, _ dns.RecordRef) error {
		_, err := p.Zone(ctx, challengeName)
		return err
	}
	createCall := func(p dns.Provider, z dns.Zone, _ dns.RecordRef) error {
		_, err := p.CreateTXT(ctx, z, challengeName, dns.TXTValue("other"), "c")
		return err
	}
	getCall := func(p dns.Provider, _ dns.Zone, ref dns.RecordRef) error {
		_, err := p.GetTXT(ctx, ref)
		return err
	}
	deleteCall := func(p dns.Provider, _ dns.Zone, ref dns.RecordRef) error { return p.DeleteTXT(ctx, ref) }

	tests := []struct {
		name       string
		op         string
		resp       dnstest.FakeResponse
		call       func(dns.Provider, dns.Zone, dns.RecordRef) error
		class      string
		status     int
		code       int
		retryAfter time.Duration
	}{
		{"a malformed token (probed)", dnstest.OpZones, dnstest.FakeResponse{Status: 400, Code: 6003, Chain: 6111, Message: "Invalid request headers"}, zoneCall, dns.ClassTokenInvalid, 400, 6003, 0},
		{"an unknown 401 (probed)", dnstest.OpZones, dnstest.FakeResponse{Status: 401, Code: 1000, Message: "Invalid API Token"}, zoneCall, dns.ClassTokenInvalid, 401, 1000, 0},
		{"an unknown token 403 (probed)", dnstest.OpZones, dnstest.FakeResponse{Status: 403, Code: 9109, Message: "Invalid access token"}, zoneCall, dns.ClassTokenInvalid, 403, 9109, 0},
		{"no permission to list zones", dnstest.OpZones, dnstest.FakeResponse{Status: 403, Code: 0, Message: "requires permission \"com.cloudflare.api.account.zone.list\" to list zones"}, zoneCall, dns.ClassTokenScope, 403, 0, 0},
		{"no permission to edit records on create", dnstest.OpCreate, dnstest.FakeResponse{Status: 403, Code: 10000, Message: "Authentication error"}, createCall, dns.ClassTokenScope, 403, 10000, 0},
		{"a 403 with a code this build does not know", dnstest.OpZones, dnstest.FakeResponse{Status: 403, Code: 12345, Message: "something new"}, zoneCall, dns.ClassTokenScope, 403, 12345, 0},
		{"rate limited with retry-after", dnstest.OpZones, dnstest.FakeResponse{Status: 429, Code: 971, RetryAfter: "30"}, zoneCall, dns.ClassRateLimited, 429, 971, 30 * time.Second},
		{"a retry-after over the cap", dnstest.OpZones, dnstest.FakeResponse{Status: 429, RetryAfter: "9999"}, zoneCall, dns.ClassRateLimited, 429, 0, 5 * time.Minute},
		{"no retry-after", dnstest.OpZones, dnstest.FakeResponse{Status: 429}, zoneCall, dns.ClassRateLimited, 429, 0, 60 * time.Second},
		{"a retry-after that is a date", dnstest.OpZones, dnstest.FakeResponse{Status: 429, RetryAfter: "Wed, 21 Oct 2026 07:28:00 GMT"}, zoneCall, dns.ClassRateLimited, 429, 0, 60 * time.Second},
		{"a server error", dnstest.OpZones, dnstest.FakeResponse{Status: 500, Code: 1001, Message: "boom"}, zoneCall, dns.ClassUnavailable, 500, 1001, 0},
		{"a body that is not JSON", dnstest.OpZones, dnstest.FakeResponse{Status: 200, Body: "<html>gateway</html>"}, zoneCall, dns.ClassUnavailable, 200, 0, 0},
		{"a CNAME at the name on create", dnstest.OpCreate, dnstest.FakeResponse{Status: 400, Code: 81053, Message: "CNAME record already exists"}, createCall, dns.ClassDelegationTarget, 400, 81053, 0},
		{"another CNAME code on create", dnstest.OpCreate, dnstest.FakeResponse{Status: 400, Code: 81054, Message: "CNAME record already exists"}, createCall, dns.ClassDelegationTarget, 400, 81054, 0},
		{"any other 4xx has no class", dnstest.OpCreate, dnstest.FakeResponse{Status: 422, Code: 9999, Message: "odd"}, createCall, "", 422, 9999, 0},
		{"a record Cloudflare no longer has on read", dnstest.OpGet, dnstest.FakeResponse{Status: 404, Code: 81044, Message: "Record does not exist."}, getCall, dns.ClassUnavailable, 404, 81044, 0},
		{"a delete answered 5xx", dnstest.OpDelete, dnstest.FakeResponse{Status: 503}, deleteCall, dns.ClassUnavailable, 503, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake, p, z, ref := withRecord(t)
			fake.Script(tc.op, tc.resp)
			err := tc.call(p, z, ref)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			de := asDNS(t, err)
			if de.Class != tc.class || de.Status != tc.status || de.Code != tc.code || de.RetryAfter != tc.retryAfter {
				t.Fatalf("got class %q status %d code %d retry-after %v, want %q %d %d %v",
					de.Class, de.Status, de.Code, de.RetryAfter, tc.class, tc.status, tc.code, tc.retryAfter)
			}
		})
	}
}

// TestCloudflareClassifies404 checks the three meanings of a 404 (D-19): a zone ID Cloudflare no
// longer routes, a record it no longer has, and a delete of a record that is already gone.
func TestCloudflareClassifies404(t *testing.T) {
	ctx := context.Background()

	t.Run("create under a zone that is gone", func(t *testing.T) {
		fake, p, z, _ := withRecord(t)
		fake.RemoveZone("example.com")
		_, err := p.CreateTXT(ctx, z, challengeName, dns.TXTValue("x"), "c")
		de := asDNS(t, err)
		if de.Class != dns.ClassZoneNotFound || de.Status != 404 || de.Code != 7003 || de.Host != challengeName || de.Zone != "example.com" {
			t.Fatalf("got %+v, want dns_zone_not_found 404/7003 with host %q and zone example.com", de, challengeName)
		}
	})
	t.Run("read under a zone that is gone", func(t *testing.T) {
		fake, p, _, ref := withRecord(t)
		fake.RemoveZone("example.com")
		_, err := p.GetTXT(ctx, ref)
		de := asDNS(t, err)
		if de.Class != dns.ClassZoneNotFound || de.Status != 404 || de.Host != challengeName || de.Zone != "example.com" {
			t.Fatalf("got %+v, want dns_zone_not_found 404 with host %q and zone example.com", de, challengeName)
		}
	})
	t.Run("read of a record Cloudflare no longer has", func(t *testing.T) {
		_, p, _, ref := withRecord(t)
		if err := p.DeleteTXT(ctx, ref); err != nil {
			t.Fatalf("DeleteTXT: %v", err)
		}
		_, err := p.GetTXT(ctx, ref)
		de := asDNS(t, err)
		if de.Class != dns.ClassUnavailable || de.Status != 404 || de.Code != 81044 {
			t.Fatalf("got %+v, want dns_unavailable 404/81044", de)
		}
	})
	t.Run("delete of a record that is already gone is success", func(t *testing.T) {
		_, p, _, ref := withRecord(t)
		if err := p.DeleteTXT(ctx, ref); err != nil {
			t.Fatalf("first delete: %v", err)
		}
		if err := p.DeleteTXT(ctx, ref); err != nil {
			t.Fatalf("second delete of the same ID = %v, want nil", err)
		}
	})
	t.Run("delete under a zone that is gone is success", func(t *testing.T) {
		fake, p, _, ref := withRecord(t)
		fake.RemoveZone("example.com")
		if err := p.DeleteTXT(ctx, ref); err != nil {
			t.Fatalf("DeleteTXT = %v, want nil", err)
		}
	})
}

// TestCloudflareClassifiesUnreachable covers the answers that are no answer at all.
func TestCloudflareClassifiesUnreachable(t *testing.T) {
	ctx := context.Background()
	t.Run("a closed server", func(t *testing.T) {
		fake := dnstest.NewFakeCloudflare(t)
		fake.AddZone("example.com")
		p := newProvider(fake)
		fake.Close()
		_, err := p.Zone(ctx, challengeName)
		if de := asDNS(t, err); de.Class != dns.ClassUnavailable || de.Status != 0 {
			t.Fatalf("got %+v, want dns_unavailable with no status", de)
		}
	})
	t.Run("a response body over 1 MiB", func(t *testing.T) {
		fake := dnstest.NewFakeCloudflare(t)
		fake.AddZone("example.com")
		fake.HugeBody(dnstest.OpZones)
		_, err := newProvider(fake).Zone(ctx, challengeName)
		if de := asDNS(t, err); de.Class != dns.ClassUnavailable {
			t.Fatalf("got %+v, want dns_unavailable", de)
		}
	})
	t.Run("an identical record is the sentinel", func(t *testing.T) {
		_, p, z, _ := withRecord(t)
		_, err := p.CreateTXT(ctx, z, challengeName, dns.TXTValue("k"), "c")
		if !errors.Is(err, dns.ErrIdentical) {
			t.Fatalf("a duplicate create returned %v, want dns.ErrIdentical", err)
		}
	})
}

// TestCloudflareZoneWalk checks which names are asked about, in which order, and what no match
// means.
func TestCloudflareZoneWalk(t *testing.T) {
	ctx := context.Background()
	queries := func(f *dnstest.FakeCloudflare) []string {
		var out []string
		for _, r := range f.Requests() {
			if r.Path == "/client/v4/zones" {
				out = append(out, r.Query)
			}
		}
		return out
	}

	t.Run("longest match wins", func(t *testing.T) {
		fake := dnstest.NewFakeCloudflare(t)
		fake.AddZone("example.com", "ns1.parent.net")
		want := fake.AddZone("tp.example.com", "ns1.child.net")
		z, err := newProvider(fake).Zone(ctx, challengeName)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		if z.ID != want || z.Name != "tp.example.com" || z.NameServers[0] != "ns1.child.net" {
			t.Fatalf("Zone = %+v, want the tp.example.com zone", z)
		}
	})
	t.Run("the walk stops at the registrable domain", func(t *testing.T) {
		fake := dnstest.NewFakeCloudflare(t)
		fake.AddZone("elsewhere.org")
		_, err := newProvider(fake).Zone(ctx, challengeName)
		de := asDNS(t, err)
		if de.Class != dns.ClassZoneNotFound || de.Host != challengeName {
			t.Fatalf("got %+v, want dns_zone_not_found for the challenge name", de)
		}
		want := []string{"name=_acme-challenge.tp.example.com", "name=tp.example.com", "name=example.com", "per_page=5"}
		if got := queries(fake); !slices.Equal(got, want) {
			t.Fatalf("zone queries = %v, want %v", got, want)
		}
	})
	t.Run("a token that sees no zone at all", func(t *testing.T) {
		fake := dnstest.NewFakeCloudflare(t)
		fake.AddZone("example.com")
		fake.LimitZones()
		_, err := newProvider(fake).Zone(ctx, challengeName)
		if de := asDNS(t, err); de.Class != dns.ClassTokenScope {
			t.Fatalf("got %+v, want dns_token_scope", de)
		}
	})
	t.Run("the unfiltered list failing is classified too", func(t *testing.T) {
		fake := dnstest.NewFakeCloudflare(t)
		fake.Script(dnstest.OpZonesAll, dnstest.FakeResponse{Status: 403, Code: 0, Message: "requires permission"})
		_, err := newProvider(fake).Zone(ctx, challengeName)
		if de := asDNS(t, err); de.Class != dns.ClassTokenScope || de.Status != 403 {
			t.Fatalf("got %+v, want dns_token_scope 403", de)
		}
	})
	t.Run("a zone-scoped token asked about another zone", func(t *testing.T) {
		fake := dnstest.NewFakeCloudflare(t)
		fake.AddZone("example.com")
		fake.AddZone("other.org")
		fake.LimitZones("other.org")
		_, err := newProvider(fake).Zone(ctx, challengeName)
		if de := asDNS(t, err); de.Class != dns.ClassZoneNotFound {
			t.Fatalf("got %+v, want dns_zone_not_found", de)
		}
	})
	t.Run("an unknown single-label suffix is walked to the name under it and no further", func(t *testing.T) {
		fake := dnstest.NewFakeCloudflare(t)
		fake.AddZone("elsewhere.org")
		_, err := newProvider(fake).Zone(ctx, "_acme-challenge.tierplan.test")
		if de := asDNS(t, err); de.Class != dns.ClassZoneNotFound {
			t.Fatalf("got %+v, want dns_zone_not_found", de)
		}
		want := []string{"name=_acme-challenge.tierplan.test", "name=tierplan.test", "per_page=5"}
		if got := queries(fake); !slices.Equal(got, want) {
			t.Fatalf("zone queries = %v, want %v (never the suffix test)", got, want)
		}
	})
	t.Run("a bare public suffix is refused without a request", func(t *testing.T) {
		fake := dnstest.NewFakeCloudflare(t)
		_, err := newProvider(fake).Zone(ctx, "co.uk")
		if de := asDNS(t, err); de.Class != dns.ClassZoneNotFound {
			t.Fatalf("got %+v, want dns_zone_not_found", de)
		}
		if n := len(fake.Requests()); n != 0 {
			t.Fatalf("a bare public suffix made %d requests, want none", n)
		}
	})
}

// TestCloudflareBearerOnly checks D-06: only Authorization: Bearer is sent, so a Global API Key
// fails as an invalid token.
func TestCloudflareBearerOnly(t *testing.T) {
	fake := dnstest.NewFakeCloudflare(t)
	fake.AddZone("example.com")
	p := cloudflare.New(cloudflare.Options{BaseURL: fake.URL(), HTTPClient: fake.Client()})("0123456789abcdef0123456789abcdef01234")
	_, err := p.Zone(context.Background(), challengeName)
	if de := asDNS(t, err); de.Class != dns.ClassTokenInvalid || de.Status != 403 || de.Code != 9109 {
		t.Fatalf("got %+v, want dns_token_invalid 403/9109", de)
	}
	for _, r := range fake.Requests() {
		if !r.Bearer || r.LegacyAuth {
			t.Fatalf("request %+v: only Authorization: Bearer may be sent", r)
		}
	}
}

// TestCloudflareNeverFollowsRedirects checks T-15-02: a 302 to another host is not followed, so
// the bearer token never reaches it.
func TestCloudflareNeverFollowsRedirects(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"success":true,"errors":[],"result":[]}`))
	}))
	t.Cleanup(other.Close)

	fake := dnstest.NewFakeCloudflare(t)
	fake.AddZone("example.com")
	fake.RedirectTo(other.URL + "/zones")
	_, err := newProvider(fake).Zone(context.Background(), challengeName)
	if de := asDNS(t, err); de.Class != dns.ClassUnavailable || de.Status != 302 {
		t.Fatalf("got %+v, want dns_unavailable with status 302", de)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the redirect target saw %d requests, want none", n)
	}
}

// TestCloudflareTokenNeverInErrors checks T-15-01: for every failure a call can end in, the token
// is absent from the error however it is printed.
func TestCloudflareTokenNeverInErrors(t *testing.T) {
	const secret = "cf-SECRET-token-7c1e9a52f0b8d3"
	ctx := context.Background()
	scripts := []struct {
		op   string
		resp dnstest.FakeResponse
	}{
		{dnstest.OpZones, dnstest.FakeResponse{Status: 400, Code: 6003, Chain: 6111, Message: "Invalid request headers " + secret}},
		{dnstest.OpZones, dnstest.FakeResponse{Status: 403, Code: 9109, Message: "Invalid access token " + secret}},
		{dnstest.OpZones, dnstest.FakeResponse{Status: 403, Code: 0, Message: "requires permission " + secret}},
		{dnstest.OpZones, dnstest.FakeResponse{Status: 429, RetryAfter: "5"}},
		{dnstest.OpZones, dnstest.FakeResponse{Status: 500, Body: secret}},
		{dnstest.OpCreate, dnstest.FakeResponse{Status: 403, Code: 10000, Message: secret}},
		{dnstest.OpCreate, dnstest.FakeResponse{Status: 400, Code: 81058, Message: secret}},
		{dnstest.OpGet, dnstest.FakeResponse{Status: 404, Code: 81044, Message: secret}},
		{dnstest.OpDelete, dnstest.FakeResponse{Status: 502, Body: secret}},
	}
	check := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: want an error", name)
			return
		}
		for _, text := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
			if strings.Contains(text, secret) {
				t.Errorf("%s: the token or a remote message is in the error text %q", name, text)
			}
		}
		for e := err; e != nil; e = errors.Unwrap(e) {
			if strings.Contains(e.Error(), secret) {
				t.Errorf("%s: the token is in a wrapped error %q", name, e.Error())
			}
		}
	}
	for _, sc := range scripts {
		fake := dnstest.NewFakeCloudflare(t)
		fake.Token = secret
		fake.AddZone("example.com")
		p := newProvider(fake)
		z, err := p.Zone(ctx, challengeName)
		if err != nil {
			t.Fatalf("Zone: %v", err)
		}
		ref, err := p.CreateTXT(ctx, z, challengeName, dns.TXTValue("k"), "c")
		if err != nil {
			t.Fatalf("CreateTXT: %v", err)
		}
		fake.Script(sc.op, sc.resp)
		switch sc.op {
		case dnstest.OpZones:
			_, err = p.Zone(ctx, challengeName)
		case dnstest.OpCreate:
			_, err = p.CreateTXT(ctx, z, challengeName, dns.TXTValue("k"), "c")
		case dnstest.OpGet:
			_, err = p.GetTXT(ctx, ref)
		case dnstest.OpDelete:
			err = p.DeleteTXT(ctx, ref)
		}
		check(fmt.Sprintf("%s %d", sc.op, sc.resp.Status), err)
	}

	t.Run("a closed server", func(t *testing.T) {
		fake := dnstest.NewFakeCloudflare(t)
		fake.Token = secret
		p := newProvider(fake)
		fake.Close()
		_, err := p.Zone(ctx, challengeName)
		check("closed server", err)
	})
	t.Run("a wrong token", func(t *testing.T) {
		fake := dnstest.NewFakeCloudflare(t)
		fake.AddZone("example.com")
		p := cloudflare.New(cloudflare.Options{BaseURL: fake.URL(), HTTPClient: fake.Client()})(secret)
		_, err := p.Zone(ctx, challengeName)
		check("wrong token", err)
	})
}

// TestCloudflareDeletesOnlyByID checks the adjacency rule: records at one name are told apart by
// their IDs, and deleting one leaves the others and any record TierPlan did not create.
func TestCloudflareDeletesOnlyByID(t *testing.T) {
	ctx := context.Background()
	fake := dnstest.NewFakeCloudflare(t)
	fake.AddZone("example.com")
	foreignID := fake.AddForeignTXT(challengeName, `"someone-elses-value"`)
	p := newProvider(fake)
	z, err := p.Zone(ctx, challengeName)
	if err != nil {
		t.Fatalf("Zone: %v", err)
	}
	a, err := p.CreateTXT(ctx, z, challengeName, dns.TXTValue("one"), "tierplan:i:a")
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	b, err := p.CreateTXT(ctx, z, challengeName, dns.TXTValue("two"), "tierplan:i:b")
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if a.RecordID == b.RecordID {
		t.Fatalf("two records at one name share the ID %s", a.RecordID)
	}

	if err := p.DeleteTXT(ctx, a); err != nil {
		t.Fatalf("DeleteTXT: %v", err)
	}
	var left []string
	for _, r := range fake.Records() {
		left = append(left, r.ID)
	}
	slices.Sort(left)
	want := []string{b.RecordID, foreignID}
	slices.Sort(want)
	if !slices.Equal(left, want) {
		t.Fatalf("records left = %v, want the second record %s and the foreign record %s", left, b.RecordID, foreignID)
	}
	if got, err := p.GetTXT(ctx, b); err != nil || got.Content != dns.TXTValue("two") {
		t.Fatalf("the second record read back as %+v, %v", got, err)
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package cloudflare_test

import (
	"context"
	"sync"
	"testing"

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

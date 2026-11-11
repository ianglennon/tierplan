// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package acmetest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/acme/dns/cloudflare"
	"github.com/ianglennon/tierplan/internal/acme/dns/dnstest"
	"github.com/ianglennon/tierplan/internal/server/servertest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Phase 15 (CHAL-03, CHAL-04, CHAL-05, D-19): DNS-01 end to end. A real TierPlan server orders from
// Pebble; Pebble resolves the challenge through pebble-challtestsrv; TierPlan writes the TXT record
// through its own Cloudflare client to a fake Cloudflare API, and the fake tells the challenge test
// server what it holds. No live Cloudflare account is involved.

// sinkEvent is one record the fake Cloudflare created or deleted, as it told the challenge test server.
type sinkEvent struct {
	op, fqdn, value string
}

// recordingSink forwards what the fake Cloudflare holds to the challenge test server and remembers the
// calls, so a test can say where the record was written.
type recordingSink struct {
	next dnstest.TXTSink

	mu     sync.Mutex
	events []sinkEvent
}

func (r *recordingSink) AddTXT(fqdn, value string) {
	r.mu.Lock()
	r.events = append(r.events, sinkEvent{"add", dnsName(fqdn), value})
	r.mu.Unlock()
	r.next.AddTXT(fqdn, value)
}

func (r *recordingSink) RemoveTXT(fqdn, value string) {
	r.mu.Lock()
	r.events = append(r.events, sinkEvent{"remove", dnsName(fqdn), value})
	r.mu.Unlock()
	r.next.RemoveTXT(fqdn, value)
}

// added lists the names a record was ever added at, and held the net count of the records still held at the end.
func (r *recordingSink) added() (names []string, held map[string]int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	held = map[string]int{}
	for _, e := range r.events {
		key := e.fqdn + "=" + e.value
		if e.op == "add" {
			names = append(names, e.fqdn)
			held[key]++
			continue
		}
		held[key]--
	}
	return names, held
}

// dnsName is a DNS name in lowercase without the trailing dot.
func dnsName(s string) string { return strings.TrimSuffix(strings.ToLower(s), ".") }

// nameserverMap points the fake zones' nameserver names at one responder, and fails for any other name,
// so a lookup of a name the test did not expect shows.
func nameserverMap(addr string, hosts ...string) func(context.Context, string) ([]string, error) {
	known := map[string]bool{}
	for _, h := range hosts {
		known[dnsName(h)] = true
	}
	return func(_ context.Context, host string) ([]string, error) {
		if !known[dnsName(host)] {
			return nil, fmt.Errorf("the test does not map the name server %q", host)
		}
		return []string{addr}, nil
	}
}

// dnsPebbleServer is the pieces of one DNS-01 run against Pebble.
type dnsPebbleServer struct {
	*pebbleServer
	host  string
	fake  *dnstest.FakeCloudflare
	chall *acmetest.ChallTestSrv
	sink  *recordingSink
}

// startDNSPebbleServer runs the challenge test server, a Pebble that resolves through it, and a real
// server whose public name is host and whose Cloudflare client talks to fake.
func startDNSPebbleServer(t *testing.T, host string, fake *dnstest.FakeCloudflare, chall *acmetest.ChallTestSrv, sink *recordingSink, deps acme.DNSDeps) *dnsPebbleServer {
	t.Helper()
	pebble := acmetest.StartPebble(t, acmetest.PebbleOptions{DNSServer: chall.InNetworkDNS(), Network: chall.Network()})
	deps.Providers = map[string]dns.Factory{
		"cloudflare": cloudflare.New(cloudflare.Options{BaseURL: fake.URL(), HTTPClient: fake.Client()}),
	}
	s := servertest.Start(t, servertest.Options{
		Host: "127.0.0.1", PublicHost: host,
		Env: map[string]string{
			"TIERPLAN_ACME_DIRECTORY_URL": pebble.DirectoryURL(),
			"TIERPLAN_ACME_CA_FILE":       pebble.CAFile(),
		},
		ACMEDNS: &deps,
	})
	roots := pebble.IssuerRoots(t)
	s.TrustCertificates(roots...)
	return &dnsPebbleServer{
		pebbleServer: &pebbleServer{s: s, mgr: s.Core().ACME(), pebble: pebble, roots: roots},
		host:         host, fake: fake, chall: chall, sink: sink,
	}
}

// newFakeCloudflare starts the fake with one zone, wired to the challenge test server through a
// recording sink.
func newFakeCloudflare(t *testing.T, chall *acmetest.ChallTestSrv, zone string, nameservers ...string) (*dnstest.FakeCloudflare, string, *recordingSink) {
	t.Helper()
	fake := dnstest.NewFakeCloudflare(t)
	sink := &recordingSink{next: chall}
	fake.Sink = sink
	return fake, fake.AddZone(zone, nameservers...), sink
}

// saveValidation switches the server to DNS-01 with the fake's token, as the Settings page does, and
// requires the save to pass its token test.
func (p *dnsPebbleServer) saveValidation(t *testing.T, resolvers ...string) {
	t.Helper()
	body := map[string]any{"challengeType": acme.ChallengeDNS01, "token": p.fake.Token}
	if len(resolvers) > 0 {
		body["resolvers"] = resolvers
	}
	code, raw := p.s.Do(http.MethodPut, "/api/https/acme/validation", body)
	if code != http.StatusOK {
		t.Fatalf("PUT /api/https/acme/validation: %d %s", code, raw)
	}
	var got struct {
		Saved bool            `json:"saved"`
		Test  json.RawMessage `json:"test"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode the validation answer: %v: %s", err, raw)
	}
	if !got.Saved {
		t.Fatalf("the validation was not saved; the token test said: %s", got.Test)
	}
}

// issueViaAPI starts a run with POST /api/https/acme/issue, for an account that is registered, and waits
// for it to succeed.
func (p *dnsPebbleServer) issueViaAPI(t *testing.T) {
	t.Helper()
	code, raw := p.s.Do(http.MethodPost, "/api/https/acme/issue", nil)
	if code != http.StatusAccepted {
		t.Fatalf("POST /api/https/acme/issue: %d %s; want 202", code, raw)
	}
	var start struct {
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(raw, &start); err != nil || start.JobID == "" {
		t.Fatalf("the issue answer %s holds no job id: %v", raw, err)
	}
	p.awaitRun(t, start.JobID)
}

// dnsStatus is the part of GET /api/https/status that says what DNS-01 left behind.
type dnsStatus struct {
	ACME struct {
		ChallengeType string `json:"challengeType"`
		DNS           struct {
			TokenSaved bool              `json:"tokenSaved"`
			LeftOver   []json.RawMessage `json:"leftOver"`
		} `json:"dns"`
		LastRun *struct {
			Challenge string `json:"challenge"`
			Outcome   string `json:"outcome"`
		} `json:"lastRun"`
	} `json:"acme"`
}

func (p *dnsPebbleServer) dnsStatus(t *testing.T) dnsStatus {
	t.Helper()
	code, raw := p.s.Do(http.MethodGet, "/api/https/status", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/https/status: %d %s", code, raw)
	}
	var st dnsStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("decode the status: %v: %s", err, raw)
	}
	return st
}

// requireNothingLeft asserts what CHAL-04 promises after a run: the fake Cloudflare holds no record, the
// challenge test server serves no TXT at the challenge names, every record the fake ever told it about
// was withdrawn again, and the status lists no left-over item.
func (p *dnsPebbleServer) requireNothingLeft(t *testing.T, challengeNames ...string) {
	t.Helper()
	if recs := p.fake.Records(); len(recs) != 0 {
		t.Errorf("the fake Cloudflare still holds %d record(s) after the run: %+v", len(recs), recs)
	}
	for _, name := range challengeNames {
		if got := txtAt(t, p.chall.DNSAddr(), name); len(got) != 0 {
			t.Errorf("the challenge test server still serves TXT %v at %s", got, name)
		}
	}
	_, held := p.sink.added()
	for key, n := range held {
		if n != 0 {
			t.Errorf("record %s was added %+d time(s) more than it was removed", key, n)
		}
	}
	if st := p.dnsStatus(t); len(st.ACME.DNS.LeftOver) != 0 {
		t.Errorf("the status lists %d left-over record(s): %s", len(st.ACME.DNS.LeftOver), st.ACME.DNS.LeftOver)
	}
}

// txtAt asks the DNS server at addr for the TXT records at name and returns their values.
func txtAt(t *testing.T, addr, name string) []string {
	t.Helper()
	q := new(mdns.Msg)
	q.SetQuestion(mdns.Fqdn(name), mdns.TypeTXT)
	ctx, cancel := context.WithTimeout(testsupport.Context(t), 30*time.Second)
	defer cancel()
	resp, _, err := (&mdns.Client{Net: "udp", Timeout: 10 * time.Second}).ExchangeContext(ctx, q, addr)
	if err != nil {
		t.Fatalf("ask %s for TXT %s: %v", addr, name, err)
	}
	var out []string
	for _, rr := range resp.Answer {
		if txt, ok := rr.(*mdns.TXT); ok {
			out = append(out, strings.Join(txt.Txt, ""))
		}
	}
	return out
}

// requestedCreateIn reports whether the fake Cloudflare received a request that created a record in zoneID.
func requestedCreateIn(fake *dnstest.FakeCloudflare, zoneID string) bool {
	for _, r := range fake.Requests() {
		if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/zones/"+zoneID+"/dns_records") {
			return true
		}
	}
	return false
}

// TestPebbleDNS01IssuesAndLeavesNoRecord is the conformance test of DNS-01 (ROADMAP SC 5): the server
// saves a Cloudflare token, orders through the API, writes the TXT record to the fake Cloudflare, waits
// for the zone's own name servers (the challenge test server, here) to show it, and Pebble validates it
// through that server. The certificate is the one served afterwards, and no record is left anywhere.
func TestPebbleDNS01IssuesAndLeavesNoRecord(t *testing.T) {
	chall := acmetest.StartChallTestSrv(t)
	fake, zoneID, sink := newFakeCloudflare(t, chall, "tierplan.test", "ns1.cf.test", "ns2.cf.test")
	// Stands in for the container's resolver (a LAN resolver with its own view of the name): D-11 says
	// the default path never asks it.
	system := acmetest.StartDNSServer(t)

	p := startDNSPebbleServer(t, publicName, fake, chall, sink, acme.DNSDeps{
		NameserverAddrs: nameserverMap(chall.DNSAddr(), "ns1.cf.test", "ns2.cf.test"),
		ResolvConf:      func() ([]string, error) { return []string{system.Addr()}, nil },
	})
	p.register(t)
	p.saveValidation(t)
	p.issueViaAPI(t)

	// The certificate Pebble issued is the one served.
	leaf := p.presented(t)
	if err := leaf.VerifyHostname(publicName); err != nil {
		t.Errorf("the presented certificate is not for %s: %v", publicName, err)
	}
	st := p.status(t)
	if st.Certificate.Source != "lets_encrypt" || !st.Certificate.TestCA {
		t.Errorf("certificate source %q testCA %v; want lets_encrypt and true", st.Certificate.Source, st.Certificate.TestCA)
	}
	if run := p.dnsStatus(t).ACME.LastRun; run == nil || run.Challenge != acme.ChallengeDNS01 {
		t.Errorf("the last run = %+v; want a dns-01 run", run)
	}

	// The record went to the zone through TierPlan's own client, at the challenge name.
	challengeName := "_acme-challenge." + publicName
	if !requestedCreateIn(fake, zoneID) {
		t.Errorf("the fake Cloudflare saw no record created in zone %s: %+v", zoneID, fake.Requests())
	}
	names, _ := sink.added()
	if !containsName(names, challengeName) {
		t.Errorf("records were added at %v; want one at %s", names, challengeName)
	}
	if q := system.Queries(); len(q) != 0 {
		t.Errorf("the system resolver was asked %+v; the default path asks the zone's own name servers only", q)
	}
	p.requireNothingLeft(t, challengeName)
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

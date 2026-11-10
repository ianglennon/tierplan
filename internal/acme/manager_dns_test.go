// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	mdns "github.com/miekg/dns"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/acme/dns/dnstest"
	"github.com/ianglennon/tierplan/internal/jobs"
)

// syncBuffer is the log a test reads: the manager writes it from its own goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// dnsToken is the one token the fake provider accepts.
const dnsToken = "cf-token-7f3a91c2d04b8e65"

// dnsNS is the host name the fake zone's nameserver goes by; the checker maps it to the responder.
const dnsNS = "ns1.tierplan-test.net"

// dnsEnv is a Manager over a fake CA that validates by looking the TXT record up in a fake DNS
// provider, a DNS responder that serves what the provider holds, and a job hub on the test's clock.
type dnsEnv struct {
	*mgrEnv
	prov *dnstest.FakeProvider
	dns  *acmetest.DNSServer
	logs *syncBuffer
}

// txtAt is the TXT values the fake provider holds at fqdn: what the fake CA finds when it looks.
func txtAt(prov *dnstest.FakeProvider, fqdn string) []string {
	var out []string
	for _, r := range prov.Records() {
		if strings.EqualFold(r.Name, fqdn) {
			out = append(out, r.Value())
		}
	}
	return out
}

// newDNSEnv is a DNS-01 environment: the CA offers challenge, the provider holds the zone example.com,
// and the saved validation is DNS-01 with the provider's token. challenge "http-01" gives the same
// environment over an HTTP-01 CA with the DNS settings saved but unused.
func newDNSEnv(t *testing.T, challenge string) *dnsEnv {
	t.Helper()
	prov := dnstest.NewFakeProvider()
	srv := acmetest.StartDNSServer(t)
	prov.Sink = srv
	prov.AddZone("example.com", dnsNS)

	opts := acmetest.Options{ChallengeType: challenge}
	if challenge == ChallengeDNS01 {
		opts.LookupTXT = func(fqdn string) []string { return txtAt(prov, fqdn) }
	}
	oe := newOrderEnvWith(t, opts)
	hub := jobs.New(jobs.Options{Now: oe.clock.Now})
	t.Cleanup(hub.Close)
	logs := &syncBuffer{}
	e := &dnsEnv{
		mgrEnv: &mgrEnv{orderEnv: oe, hub: hub, inst: &recInstaller{}, urls: &urlSet{public: "https://" + testHost}},
		prov:   prov, dns: srv, logs: logs,
	}
	e.mgr = e.build(false)
	e.save(challenge, nil)
	return e
}

// build is a Manager over the stored state, with the DNS side pointed at the fakes.
func (e *dnsEnv) build(certFiles bool) *Manager {
	return NewManager(Deps{
		Store: e.store, Client: e.client, Challenges: e.tokens, Jobs: e.hub, Installer: e.inst, Clock: e.clock,
		PublicURL: e.urls.getPublic, AgentURL: e.urls.getAgent, BasePath: e.urls.getBase,
		CertFiles: certFiles, Log: slog.New(slog.NewTextHandler(e.logs, nil)),
		Jitter: func() float64 { return 0 },
		Rand:   func(int64) int64 { return 0 },
		DNS: DNSDeps{
			Providers:       map[string]dns.Factory{"cloudflare": e.prov.Factory(dnsToken)},
			NameserverAddrs: func(context.Context, string) ([]string, error) { return []string{e.dns.Addr()}, nil },
			ResolvConf:      func() ([]string, error) { return []string{e.dns.Addr()}, nil },
		},
	})
}

// save stores the validation settings the way a settings save does: the challenge, the provider, the
// token and the resolvers.
func (e *dnsEnv) save(challenge string, resolvers []string) {
	e.t.Helper()
	tok := dnsToken
	if err := e.store.SaveValidation(context.Background(), ValidationWrite{
		ChallengeType: challenge, Provider: "cloudflare", Resolvers: resolvers, Token: &tok, Now: e.clock.Now(),
	}); err != nil {
		e.t.Fatalf("SaveValidation: %v", err)
	}
}

func itemIDs(job jobs.Job) []string {
	out := make([]string, len(job.Items))
	for i, it := range job.Items {
		out[i] = it.ID
	}
	return out
}

func specIDs(specs []jobs.ItemSpec) []string {
	out := make([]string, len(specs))
	for i, it := range specs {
		out[i] = it.ID
	}
	return out
}

func (e *dnsEnv) ledger() []DNSRecord {
	e.t.Helper()
	rows, err := e.store.DNSRecords(context.Background())
	if err != nil {
		e.t.Fatalf("DNSRecords: %v", err)
	}
	return rows
}

func TestManagerDNS01IssuesAndRemovesItsRecord(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)

	job := e.issue()

	if job.Meta["challenge"] != ChallengeDNS01 {
		t.Errorf("job meta challenge = %q, want dns-01", job.Meta["challenge"])
	}
	if got, want := itemIDs(job), specIDs(stepItemsDNS01); !slices.Equal(got, want) {
		t.Errorf("job items = %v, want %v", got, want)
	}
	items := itemsOf(job)
	for _, id := range specIDs(stepItemsDNS01) {
		want := jobs.ItemDone
		if id == StepAccount {
			want = jobs.ItemSkipped
		}
		if items[id].State != want || items[id].Code != "" {
			t.Errorf("step %s is %q (%q), want %q", id, items[id].State, items[id].Code, want)
		}
	}

	if e.inst.served() != 1 {
		t.Fatalf("ServeACME was called %d times, want 1", e.inst.served())
	}
	if leaf := leafOf(t, e.inst.chains[0]); !slices.Equal(leaf.DNSNames, []string{testHost}) {
		t.Errorf("the served leaf covers %v, want [%s]", leaf.DNSNames, testHost)
	}
	if got := e.fake.TXTLookups("_acme-challenge." + testHost); got < 1 {
		t.Errorf("the CA looked up the TXT record %d times, want at least once", got)
	}
	if recs := e.prov.Records(); len(recs) != 0 {
		t.Errorf("the provider still holds %+v after the run", recs)
	}
	if rows := e.ledger(); len(rows) != 0 {
		t.Errorf("the ledger still holds %+v after the run", rows)
	}
	if runs := e.runs(); len(runs) != 1 || runs[0].Outcome != OutcomeSucceeded || runs[0].ChallengeType != ChallengeDNS01 {
		t.Errorf("runs = %+v, want one succeeded dns-01 run", runs)
	}

	queries := e.dns.Queries()
	if len(queries) == 0 {
		t.Fatal("the responder was asked nothing")
	}
	sawTXT := false
	for _, q := range queries {
		if q.Recursive {
			t.Errorf("the responder saw a recursive query %+v, want only questions to the zone's own nameserver", q)
		}
		sawTXT = sawTXT || q.Type == mdns.TypeTXT
	}
	if !sawTXT {
		t.Error("the responder was never asked for the TXT record")
	}
}

func TestManagerDNSPicksTheSolverForEverySavedChallenge(t *testing.T) {
	cases := []struct {
		challenge string
		steps     []jobs.ItemSpec
		class     string
	}{
		{ChallengeHTTP01, stepItems, ClassValidationUnreachable},
		{ChallengeDNS01, stepItemsDNS01, ClassValidationTXT},
	}
	for _, tc := range cases {
		t.Run(tc.challenge, func(t *testing.T) {
			e := newDNSEnv(t, tc.challenge)
			// The CA refuses the validation, so the class shows which challenge the run classified for.
			e.fake.FailValidation(testHost, acmetest.Problem{Type: "connection", Detail: "no answer"})

			job := e.issue()

			if job.Meta["challenge"] != tc.challenge {
				t.Errorf("job meta challenge = %q, want %q", job.Meta["challenge"], tc.challenge)
			}
			if got, want := itemIDs(job), specIDs(tc.steps); !slices.Equal(got, want) {
				t.Errorf("job items = %v, want %v", got, want)
			}
			runs := e.runs()
			if len(runs) != 1 || runs[0].ChallengeType != tc.challenge || runs[0].ErrorClass != tc.class {
				t.Errorf("runs = %+v, want one %s run that failed as %s", runs, tc.challenge, tc.class)
			}
			sv, err := e.mgr.newSolver(context.Background(), &jobSteps{}, tc.challenge)
			if err != nil || sv.Type() != tc.challenge {
				t.Errorf("newSolver(%s) = %v, %v; want a %s solver", tc.challenge, sv, err, tc.challenge)
			}
		})
	}
}

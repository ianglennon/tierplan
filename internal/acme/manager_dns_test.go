// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	mdns "github.com/miekg/dns"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/acme/dns/dnstest"
	"github.com/ianglennon/tierplan/internal/acme/dnscheck"
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

// dnsProviderID is the provider id of the fake provider, which is what its ledger rows carry.
const dnsProviderID = "fake"

// dnsToken is the one token the fake provider accepts.
const dnsToken = "cf-token-7f3a91c2d04b8e65" //nolint:gosec // G101: a recognisable fake token, so a test can prove it never leaks

// dnsNS is the host name the fake zone's nameserver goes by; the checker maps it to the responder.
const dnsNS = "ns1.tierplan-test.net"

// dnsEnv is a Manager over a fake CA that validates by looking the TXT record up in a fake DNS
// provider, a DNS responder that serves what the provider holds, and a job hub on the test's clock.
type dnsEnv struct {
	*mgrEnv
	prov *dnstest.FakeProvider
	dns  *acmetest.DNSServer
	logs *syncBuffer
	// nsAddr is where the zone's nameserver answers; the responder unless a test points it elsewhere.
	nsAddr string

	// built counts the providers the manager built, and so the tokens it opened for one.
	built atomic.Int32

	// lookupIP is the manager's host lookup (DNSDeps.LookupIP), applied by build; nil is the default.
	lookupIP func(ctx context.Context, host string) ([]netip.Addr, error)

	mu     sync.Mutex
	cnames map[string]string
	// seen is what the provider held at each TXT lookup the CA made.
	seen [][]dnstest.FakeRecord
}

// countingFactory is the provider factory, counting every provider built from it.
func (e *dnsEnv) countingFactory() dns.Factory {
	inner := e.prov.Factory(dnsToken)
	return func(token string) dns.Provider {
		e.built.Add(1)
		return inner(token)
	}
}

// cname delegates name to target, for the CA as well as for the responder.
func (e *dnsEnv) cname(name, target string) {
	e.mu.Lock()
	e.cnames[strings.ToLower(name)] = strings.ToLower(target)
	e.mu.Unlock()
	e.dns.SetCNAME(name, target)
}

// lookup is what the fake CA finds at fqdn: it follows a CNAME as a CA does.
func (e *dnsEnv) lookup(fqdn string) []string {
	e.mu.Lock()
	if target, ok := e.cnames[strings.ToLower(fqdn)]; ok {
		fqdn = target
	}
	e.mu.Unlock()
	recs := e.prov.Records()
	e.mu.Lock()
	e.seen = append(e.seen, recs)
	e.mu.Unlock()
	return txtAt(e.prov, fqdn)
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

	e := &dnsEnv{prov: prov, dns: srv, logs: &syncBuffer{}, nsAddr: srv.Addr(), cnames: map[string]string{}}
	opts := acmetest.Options{ChallengeType: challenge}
	if challenge == ChallengeDNS01 {
		opts.LookupTXT = e.lookup
	}
	oe := newOrderEnvWith(t, opts)
	hub := jobs.New(jobs.Options{Now: oe.clock.Now})
	t.Cleanup(hub.Close)
	e.mgrEnv = &mgrEnv{orderEnv: oe, hub: hub, inst: &recInstaller{}, urls: &urlSet{public: "https://" + testHost}}
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
			Providers:       map[string]dns.Factory{dnsProviderID: e.countingFactory()},
			LookupIP:        e.lookupIP,
			NameserverAddrs: func(context.Context, string) ([]string, error) { return []string{e.nsAddr}, nil },
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
		ChallengeType: challenge, Provider: dnsProviderID, Resolvers: resolvers, Token: &tok, Now: e.clock.Now(),
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

// multiSink tells several responders about every record the fake provider holds.
type multiSink []dnstest.TXTSink

func (m multiSink) AddTXT(fqdn, value string) {
	for _, s := range m {
		s.AddTXT(fqdn, value)
	}
}

func (m multiSink) RemoveTXT(fqdn, value string) {
	for _, s := range m {
		s.RemoveTXT(fqdn, value)
	}
}

// start begins an issue and does not wait for it.
func (e *dnsEnv) start() RunStart {
	e.t.Helper()
	rs, err := e.mgr.Issue(context.Background(), "user:1")
	if err != nil {
		e.t.Fatalf("Issue: %v", err)
	}
	return rs
}

// failedDoc is the error document of the last failure.
func (e *dnsEnv) failedDoc() errorDoc { return decodeError(e.state().LastErrorDetail) }

func (e *dnsEnv) setAgent(u string) {
	e.urls.mu.Lock()
	defer e.urls.mu.Unlock()
	e.urls.agent = u
}

// wantItem fails the test unless the step ended in state with code.
func wantItem(t *testing.T, items map[string]jobs.Item, id, state, code string) {
	t.Helper()
	if it := items[id]; it.State != state || it.Code != code {
		t.Errorf("step %s is %q with code %q, want %q with code %q", id, it.State, it.Code, state, code)
	}
}

func TestDNSPresentRollbackOnTimeout(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	chal := dns.ChallengeFQDN(testHost)
	e.dns.Hold(chal) // the record is created and never shows

	rs := e.start()
	e.clock.Tick(t, dnscheck.PropagationTimeout)
	job := e.wait(rs)

	items := itemsOf(job)
	wantItem(t, items, StepChallenge, jobs.ItemDone, "")
	wantItem(t, items, StepPropagation, jobs.ItemFailed, dns.ClassNotVisible)
	wantItem(t, items, StepValidate, jobs.ItemSkipped, "")
	wantItem(t, items, StepFinalize, jobs.ItemSkipped, "")
	wantItem(t, items, StepStored, jobs.ItemSkipped, "")
	wantItem(t, items, StepCleanup, jobs.ItemDone, "")
	if recs := e.prov.Records(); len(recs) != 0 {
		t.Errorf("the provider still holds %+v after a failed Present", recs)
	}
	if rows := e.ledger(); len(rows) != 0 {
		t.Errorf("the ledger still holds %+v after a failed Present", rows)
	}
	runs := e.runs()
	if len(runs) != 1 || runs[0].Outcome != OutcomeFailed || runs[0].ErrorClass != dns.ClassNotVisible {
		t.Errorf("runs = %+v, want one run that failed as dns_not_visible", runs)
	}
	doc := e.failedDoc()
	if doc.Host != testHost || doc.Zone != "example.com" || !slices.Equal(doc.Resolvers, []string{dnsNS}) {
		t.Errorf("error doc = %+v, want host %s, zone example.com and the nameserver %s", doc, testHost, dnsNS)
	}
	if e.inst.served() != 0 {
		t.Error("a certificate was served for a run that failed")
	}
}

func TestDNSPresentRollbackOnCancel(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	e.dns.Hold(dns.ChallengeFQDN(testHost))

	rs := e.start()
	e.clock.WaitArmed(t) // the run is waiting for the record to show
	if err := e.hub.Cancel(rs.JobID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	e.wait(rs)

	if recs := e.prov.Records(); len(recs) != 0 {
		t.Errorf("the provider still holds %+v after the run was cancelled", recs)
	}
	if rows := e.ledger(); len(rows) != 0 {
		t.Errorf("the ledger still holds %+v after the run was cancelled", rows)
	}
	if runs := e.runs(); len(runs) != 1 || runs[0].Outcome != OutcomeCancelled {
		t.Errorf("runs = %+v, want one cancelled run", runs)
	}
}

func TestDNSCleanupLeftOverKeepsTheRunGreen(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	e.prov.FailDelete(-1, &dns.Error{Provider: "fake", Op: "delete record", Class: dns.ClassUnavailable})

	job := e.issue()

	for _, it := range job.Items {
		if it.State == jobs.ItemFailed {
			t.Errorf("step %s failed (%s), want no failed step on a run that issued", it.ID, it.Code)
		}
	}
	cleanup := itemsOf(job)[StepCleanup]
	if cleanup.State != jobs.ItemDone || cleanup.Code != "left_over" || cleanup.Result["left_over"] != "1" {
		t.Errorf("Clean-up = %q %q %v, want done with code left_over and left_over 1", cleanup.State, cleanup.Code, cleanup.Result)
	}
	if want := "Couldn't remove 1 TXT record(s). TierPlan keeps trying."; cleanup.Message != want {
		t.Errorf("Clean-up message = %q, want %q", cleanup.Message, want)
	}
	if runs := e.runs(); len(runs) != 1 || runs[0].Outcome != OutcomeSucceeded {
		t.Errorf("runs = %+v, want one succeeded run", runs)
	}
	if st := e.state(); st.Mode != ModeActive {
		t.Errorf("mode = %q, want active", st.Mode)
	}
	rows := e.ledger()
	if len(rows) != 1 {
		t.Fatalf("ledger = %+v, want the one record left behind", rows)
	}
	if r := rows[0]; r.Attempts != 1 || !r.LastTryAt.Equal(testNow) || !r.NextTryAt.Equal(testNow.Add(time.Minute)) || r.LastErrorClass != dns.ClassUnavailable {
		t.Errorf("left-over row = %+v, want 1 attempt at %v, next a minute later, class dns_unavailable", r, testNow)
	}
	if n := len(e.prov.Records()); n != 1 {
		t.Errorf("the provider holds %d records, want the 1 left over", n)
	}
}

func TestDNSCleanupLeavesForeignRecords(t *testing.T) {
	chal := dns.ChallengeFQDN(testHost)
	t.Run("after an order that issued", func(t *testing.T) {
		e := newDNSEnv(t, ChallengeDNS01)
		foreign := e.prov.AddForeignTXT(chal, "someone-elses-value")

		e.issue()

		recs := e.prov.Records()
		if len(recs) != 1 || recs[0].ID != foreign {
			t.Errorf("the provider holds %+v, want only the foreign record %s", recs, foreign)
		}
		for _, c := range e.prov.Calls() {
			if c.Op == "delete" && c.RecordID == foreign {
				t.Errorf("TierPlan deleted the foreign record: %+v", c)
			}
		}
	})
	t.Run("after an order that failed", func(t *testing.T) {
		e := newDNSEnv(t, ChallengeDNS01)
		foreign := e.prov.AddForeignTXT(chal, "someone-elses-value")
		e.fake.FailValidation(testHost, acmetest.Problem{Type: "connection", Detail: "no answer"})

		e.issue()

		recs := e.prov.Records()
		if len(recs) != 1 || recs[0].ID != foreign {
			t.Errorf("the provider holds %+v, want only the foreign record %s", recs, foreign)
		}
		if n := e.prov.CallCount("delete"); n != 1 {
			t.Errorf("TierPlan made %d delete calls, want exactly one, for its own record", n)
		}
	})
}

func TestDNSZoneNotFoundCreatesNothing(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	e.urls.setPublic("https://tierplan.example.org") // the provider holds example.com only

	job := e.issue()

	items := itemsOf(job)
	wantItem(t, items, StepChallenge, jobs.ItemFailed, dns.ClassZoneNotFound)
	wantItem(t, items, StepPropagation, jobs.ItemSkipped, "")
	wantItem(t, items, StepCleanup, jobs.ItemSkipped, "")
	if got := items[StepCleanup].Message; got != "Nothing to remove." {
		t.Errorf("Clean-up message = %q, want %q", got, "Nothing to remove.")
	}
	if n, d := e.prov.CallCount("create"), e.prov.CallCount("delete"); n != 0 || d != 0 {
		t.Errorf("the provider saw %d creates and %d deletes, want none", n, d)
	}
}

func TestDNSNoTokenSavedFailsBeforeAnyProviderCall(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	if err := e.store.DeleteDNSToken(context.Background(), dnsProviderID); err != nil {
		t.Fatal(err)
	}

	job := e.issue()

	items := itemsOf(job)
	wantItem(t, items, StepChallenge, jobs.ItemFailed, dns.ClassTokenInvalid)
	wantItem(t, items, StepCleanup, jobs.ItemSkipped, "")
	if calls := e.prov.Calls(); len(calls) != 0 {
		t.Errorf("the provider was called %+v with no token saved", calls)
	}
}

func TestDNSDelegatedChallenge(t *testing.T) {
	const target = "_acme-challenge.tierplan.acme-target.net"
	chal := dns.ChallengeFQDN(testHost)

	t.Run("to a record in another provider zone", func(t *testing.T) {
		e := newDNSEnv(t, ChallengeDNS01)
		e.prov.AddZone("acme-target.net", dnsNS)
		e.cname(chal, target)

		job := e.issue()

		challenge := itemsOf(job)[StepChallenge]
		if challenge.State != jobs.ItemDone || challenge.Result["target"] != target || challenge.Result["record"] != target || challenge.Result["host"] != testHost {
			t.Errorf("Challenge = %q %v, want done with host %s, record and target %s", challenge.State, challenge.Result, testHost, target)
		}
		var created []string
		for _, c := range e.prov.Calls() {
			if c.Op == "create" {
				created = append(created, c.Name)
			}
		}
		if !slices.Equal(created, []string{target}) {
			t.Errorf("records were created at %v, want only %s", created, target)
		}
		if recs := e.prov.Records(); len(recs) != 0 {
			t.Errorf("the provider still holds %+v", recs)
		}
	})

	t.Run("by a name whose own zone is not on the provider", func(t *testing.T) {
		e := newDNSEnv(t, ChallengeDNS01)
		e.urls.setPublic("https://tierplan.example.org")
		e.prov.AddZone("acme-target.net", dnsNS)
		e.cname(dns.ChallengeFQDN("tierplan.example.org"), "_acme-challenge.x.acme-target.net")

		job := e.issue()

		if state := itemsOf(job)[StepChallenge].State; state != jobs.ItemDone {
			t.Fatalf("Challenge is %q, want done: a delegation into a provider zone validates (D-15)", state)
		}
		if runs := e.runs(); len(runs) != 1 || runs[0].Outcome != OutcomeSucceeded {
			t.Errorf("runs = %+v, want one succeeded run", runs)
		}
	})

	t.Run("to a zone the token cannot see", func(t *testing.T) {
		e := newDNSEnv(t, ChallengeDNS01)
		e.prov.AddZone("acme-target.net", dnsNS)
		e.prov.HideZone("acme-target.net")
		e.cname(chal, target)

		job := e.issue()

		wantItem(t, itemsOf(job), StepChallenge, jobs.ItemFailed, dns.ClassDelegationTarget)
		if doc := e.failedDoc(); doc.Target != target || doc.Host != testHost {
			t.Errorf("error doc = %+v, want host %s and target %s", doc, testHost, target)
		}
		if n := e.prov.CallCount("create"); n != 0 {
			t.Errorf("%d records were created for a delegation the token cannot edit, want none", n)
		}
	})
}

func TestDNSCNAMEProbeAsksTheZoneFirst(t *testing.T) {
	chal := dns.ChallengeFQDN(testHost)
	const target = "_acme-challenge.deleg.example.com"

	t.Run("the zone's own nameserver answers", func(t *testing.T) {
		e := newDNSEnv(t, ChallengeDNS01)
		resolver := acmetest.StartDNSServer(t)
		e.prov.Sink = multiSink{e.dns, resolver}
		e.save(ChallengeDNS01, []string{resolver.Addr()})

		e.issue()

		sawCNAME := false
		for _, q := range e.dns.Queries() {
			if q.Type == mdns.TypeCNAME && !q.Recursive {
				sawCNAME = true
			}
			if q.Type == mdns.TypeTXT {
				t.Errorf("the zone's nameserver was asked for the TXT record %+v, but the listed resolvers replace it for the check", q)
			}
		}
		if !sawCNAME {
			t.Error("the zone's own nameserver was never asked for the CNAME")
		}
		qs := resolver.Queries()
		if len(qs) == 0 {
			t.Fatal("the listed resolver was never asked for the TXT record")
		}
		for _, q := range qs {
			if q.Type == mdns.TypeCNAME {
				t.Errorf("the listed resolver was asked for the CNAME %+v, want that only when no zone nameserver answers", q)
			}
			if !q.Recursive {
				t.Errorf("the listed resolver was asked without recursion: %+v", q)
			}
		}
	})

	t.Run("no zone nameserver answers", func(t *testing.T) {
		e := newDNSEnv(t, ChallengeDNS01)
		resolver := acmetest.StartDNSServer(t)
		e.prov.Sink = multiSink{e.dns, resolver}
		e.save(ChallengeDNS01, []string{resolver.Addr()})
		e.nsAddr = "127.0.0.1:1" // nothing listens there
		e.cname(chal, target)
		resolver.SetCNAME(chal, target)

		job := e.issue()

		if got := itemsOf(job)[StepChallenge].Result["target"]; got != target {
			t.Errorf("Challenge target = %q, want %q found through the listed resolver", got, target)
		}
		sawCNAME := false
		for _, q := range resolver.Queries() {
			sawCNAME = sawCNAME || q.Type == mdns.TypeCNAME
		}
		if !sawCNAME {
			t.Error("the listed resolver was never asked for the CNAME after the zone's nameservers did not answer")
		}
		if runs := e.runs(); len(runs) != 1 || runs[0].Outcome != OutcomeSucceeded {
			t.Errorf("runs = %+v, want one succeeded run", runs)
		}
	})
}

func TestDNSTwoNamesToOneTargetGetTwoRecords(t *testing.T) {
	const target = "_acme-challenge.shared.example.com"
	e := newDNSEnv(t, ChallengeDNS01)
	e.setAgent("https://agents.example.com")
	e.cname(dns.ChallengeFQDN(testHost), target)
	e.cname(dns.ChallengeFQDN("agents.example.com"), target)

	job := e.issue()

	if runs := e.runs(); len(runs) != 1 || runs[0].Outcome != OutcomeSucceeded {
		t.Fatalf("runs = %+v, want one succeeded run (steps %v)", runs, itemsOf(job))
	}
	created := map[string]string{} // record ID -> name
	for _, c := range e.prov.Calls() {
		if c.Op == "create" {
			created[c.RecordID] = c.Name
		}
	}
	if len(created) != 2 {
		t.Fatalf("created %v, want two records with distinct IDs", created)
	}
	deleted := map[string]bool{}
	for _, c := range e.prov.Calls() {
		if c.Op == "delete" {
			deleted[c.RecordID] = true
		}
	}
	for id, name := range created {
		if name != target || !deleted[id] {
			t.Errorf("record %s at %s deleted = %v, want a record at %s deleted by its own ID", id, name, deleted[id], target)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	both := false
	for _, snap := range e.seen {
		values := map[string]bool{}
		for _, r := range snap {
			if r.Name == target {
				values[r.Value()] = true
			}
		}
		both = both || len(values) == 2
	}
	if !both {
		t.Error("the CA never saw two records with distinct values at the shared target")
	}
}

func TestDNSJobItemsCarryNoSecrets(t *testing.T) {
	scenarios := map[string]func(e *dnsEnv){
		"issued": func(e *dnsEnv) {},
		"validation failed": func(e *dnsEnv) {
			e.fake.FailValidation(testHost, acmetest.Problem{Type: "connection", Detail: "no answer"})
		},
		"left over": func(e *dnsEnv) {
			e.prov.FailDelete(-1, &dns.Error{Provider: "fake", Op: "delete record", Class: dns.ClassUnavailable})
		},
		"zone missing": func(e *dnsEnv) { e.urls.setPublic("https://tierplan.example.org") },
	}
	for name, setup := range scenarios {
		t.Run(name, func(t *testing.T) {
			e := newDNSEnv(t, ChallengeDNS01)
			setup(e)

			job := e.issue()

			doc, err := json.Marshal(job)
			if err != nil {
				t.Fatal(err)
			}
			st := e.state()
			for where, text := range map[string]string{
				"the job": string(doc), "last_error_detail": st.LastErrorDetail, "the log": e.logs.String(),
			} {
				if strings.Contains(text, dnsToken) {
					t.Errorf("%s holds the DNS token", where)
				}
			}
		})
	}
}

// leftOver makes a record the way an earlier run did and then could not delete: it is at the provider,
// and the ledger holds its ID, owned by an order that is gone.
func (e *dnsEnv) leftOver(fqdn, value string, created time.Time) DNSRecord {
	e.t.Helper()
	ctx := context.Background()
	p := e.prov.Factory(dnsToken)(dnsToken)
	z, err := p.Zone(ctx, fqdn)
	if err != nil {
		e.t.Fatalf("Zone(%s): %v", fqdn, err)
	}
	ref, err := p.CreateTXT(ctx, z, fqdn, value, "tierplan:old-install:old-order")
	if err != nil {
		e.t.Fatalf("CreateTXT: %v", err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		e.t.Fatal(err)
	}
	row := DNSRecord{
		ID: id.String(), Provider: dnsProviderID, ZoneID: ref.ZoneID, RecordID: ref.RecordID, Name: fqdn, Value: value,
		Comment: "tierplan:old-install:old-order", OwnerKind: OwnerOrder, OwnerID: "old-order", Token: "old", CreatedAt: created,
	}
	if err := e.store.InsertDNSRecord(ctx, row); err != nil {
		e.t.Fatalf("InsertDNSRecord: %v", err)
	}
	return row
}

// deletes are the record IDs the provider was asked to delete, in order.
func (e *dnsEnv) deletes() []string {
	var out []string
	for _, c := range e.prov.Calls() {
		if c.Op == "delete" {
			out = append(out, c.RecordID)
		}
	}
	return out
}

func TestDNSSweepAtStart(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	row := e.leftOver("_acme-challenge.old.example.com", "old-value", testNow)

	if err := e.mgr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if recs := e.prov.Records(); len(recs) != 0 {
		t.Errorf("the provider still holds %+v after Start", recs)
	}
	if rows := e.ledger(); len(rows) != 0 {
		t.Errorf("the ledger still holds %+v after Start (row %s)", rows, row.ID)
	}
}

func TestDNSSweepNeverWhilePaused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(e *dnsEnv) *Manager
	}{
		{"paused after a restore", func(e *dnsEnv) *Manager {
			st := e.state()
			st.PausedReason = PausedRestore
			if err := e.store.SaveState(context.Background(), st); err != nil {
				t.Fatal(err)
			}
			return e.mgr
		}},
		{"certificate files set", func(e *dnsEnv) *Manager { return e.build(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDNSEnv(t, ChallengeDNS01)
			e.leftOver("_acme-challenge.old.example.com", "old-value", testNow)
			mgr := tc.setup(e)
			before := len(e.prov.Calls())

			if err := mgr.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			mgr.sweepDNS(context.Background(), true)

			if n := len(e.prov.Calls()) - before; n != 0 {
				t.Errorf("the provider was called %d times while ACME was paused, want 0", n)
			}
			if rows := e.ledger(); len(rows) != 1 {
				t.Errorf("ledger = %+v, want the row kept", rows)
			}
		})
	}
}

func TestDNSSweepBeforeEveryOrder(t *testing.T) {
	for _, challenge := range []string{ChallengeHTTP01, ChallengeDNS01} {
		t.Run(challenge+" deletes a pending row first", func(t *testing.T) {
			e := newDNSEnv(t, challenge)
			row := e.leftOver("_acme-challenge.old.example.com", "old-value", testNow)
			// Not due yet: a run deletes every pending row, not only the due ones.
			if err := e.store.MarkDNSRecordFailed(context.Background(), row.ID, 1, testNow, testNow.Add(time.Hour), dns.ClassUnavailable); err != nil {
				t.Fatal(err)
			}

			e.issue()

			if got := e.deletes(); len(got) == 0 || got[0] != row.RecordID {
				t.Errorf("deletes = %v, want the pending record %s deleted first", got, row.RecordID)
			}
			if rows := e.ledger(); len(rows) != 0 {
				t.Errorf("ledger = %+v, want it empty", rows)
			}
			if runs := e.runs(); len(runs) != 1 || runs[0].Outcome != OutcomeSucceeded {
				t.Errorf("runs = %+v, want one succeeded run", runs)
			}
		})
	}

	t.Run("a failing delete is stamped and the order still runs", func(t *testing.T) {
		e := newDNSEnv(t, ChallengeDNS01)
		row := e.leftOver("_acme-challenge.old.example.com", "old-value", testNow)
		e.prov.FailDelete(1, &dns.Error{Provider: "fake", Op: "delete record", Class: dns.ClassUnavailable})

		e.issue()

		if runs := e.runs(); len(runs) != 1 || runs[0].Outcome != OutcomeSucceeded {
			t.Fatalf("runs = %+v, want one succeeded run", runs)
		}
		rows := e.ledger()
		if len(rows) != 1 || rows[0].ID != row.ID {
			t.Fatalf("ledger = %+v, want only the row that could not be deleted", rows)
		}
		if r := rows[0]; r.Attempts != 1 || !r.NextTryAt.Equal(testNow.Add(time.Minute)) || r.LastErrorClass != dns.ClassUnavailable {
			t.Errorf("row = %+v, want 1 attempt and the next try a minute on, class dns_unavailable", r)
		}
	})
}

func TestDNSSweepBacksOff(t *testing.T) {
	ctx := context.Background()
	e := newDNSEnv(t, ChallengeDNS01)
	e.leftOver("_acme-challenge.old.example.com", "old-value", testNow)
	e.prov.FailDelete(-1, &dns.Error{Provider: "fake", Op: "delete record", Class: dns.ClassUnavailable})

	waits := []time.Duration{time.Minute, 10 * time.Minute, 100 * time.Minute, 24 * time.Hour, 24 * time.Hour}
	for i, wait := range waits {
		before := e.prov.CallCount("delete")
		e.mgr.sweepDNS(ctx, true)
		if n := e.prov.CallCount("delete") - before; n != 1 {
			t.Fatalf("attempt %d made %d delete calls, want 1", i+1, n)
		}
		rows := e.ledger()
		if len(rows) != 1 || rows[0].Attempts != i+1 || !rows[0].NextTryAt.Equal(e.clock.Now().Add(wait)) {
			t.Fatalf("after attempt %d the row is %+v, want %d attempts and the next try %v on", i+1, rows, i+1, wait)
		}
		// Before the next try is due the sweep leaves the row alone.
		e.clock.Advance(wait - time.Second)
		before = e.prov.CallCount("delete")
		e.mgr.sweepDNS(ctx, true)
		if n := e.prov.CallCount("delete") - before; n != 0 {
			t.Fatalf("a sweep a second early made %d delete calls, want 0", n)
		}
		e.clock.Advance(time.Second)
	}
}

func TestDNSSweepWithoutATokenStampsTheRows(t *testing.T) {
	ctx := context.Background()
	e := newDNSEnv(t, ChallengeDNS01)
	e.leftOver("_acme-challenge.old.example.com", "old-value", testNow)
	if err := e.store.DeleteDNSToken(ctx, dnsProviderID); err != nil {
		t.Fatal(err)
	}
	built, calls := e.built.Load(), len(e.prov.Calls())

	e.mgr.sweepDNS(ctx, false)

	if e.built.Load() != built || len(e.prov.Calls()) != calls {
		t.Error("a provider was built or called with no token saved")
	}
	rows := e.ledger()
	if len(rows) != 1 || rows[0].LastErrorClass != dns.ClassTokenInvalid || rows[0].Attempts != 1 || !rows[0].NextTryAt.Equal(testNow.Add(time.Minute)) {
		t.Errorf("ledger = %+v, want the row stamped dns_token_invalid with 1 attempt and a minute's backoff", rows)
	}
}

func TestDNSSweepOfAnEmptyLedgerBuildsNothing(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)

	e.mgr.sweepDNS(context.Background(), false)
	e.mgr.sweepDNS(context.Background(), true)

	if e.built.Load() != 0 || len(e.prov.Calls()) != 0 {
		t.Errorf("an empty ledger built %d providers and made %d calls, want none", e.built.Load(), len(e.prov.Calls()))
	}
}

func TestDNSSweepDeletesOldestFirst(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	// Inserted out of order; two share an instant, so their IDs decide.
	newer := e.leftOver("_acme-challenge.c.example.com", "c", testNow.Add(2*time.Hour))
	oldest := e.leftOver("_acme-challenge.a.example.com", "a", testNow.Add(-time.Hour))
	tieA := e.leftOver("_acme-challenge.b.example.com", "b", testNow)
	tieB := e.leftOver("_acme-challenge.d.example.com", "d", testNow)
	want := []string{oldest.RecordID}
	if tieA.ID < tieB.ID {
		want = append(want, tieA.RecordID, tieB.RecordID)
	} else {
		want = append(want, tieB.RecordID, tieA.RecordID)
	}
	want = append(want, newer.RecordID)

	e.mgr.sweepDNS(context.Background(), false)

	if got := e.deletes(); !slices.Equal(got, want) {
		t.Errorf("deleted %v, want %v (created_at, then id)", got, want)
	}
}

func TestDNSResumeReusesThePendingRecord(t *testing.T) {
	ctx := context.Background()
	e := newDNSEnv(t, ChallengeDNS01)
	var firstErr, secondErr error
	var created int

	e.inJob(func(ctx context.Context, steps *jobSteps) {
		first := e.solver(steps)
		first.orderID = "order-old"
		firstErr = first.Present(ctx, testHost, "old", "tok.thumb")
		// The order is resumed: a new solver presents the same value for the same name.
		second := e.solver(steps)
		second.orderID = "order-new"
		secondErr = second.Present(ctx, testHost, "new", "tok.thumb")
		created = steps.created
	})

	if firstErr != nil || secondErr != nil {
		t.Fatalf("Present = %v, then %v; want both to succeed", firstErr, secondErr)
	}
	if n := e.prov.CallCount("create"); n != 1 {
		t.Errorf("the provider saw %d creates, want 1: a resume reuses the record", n)
	}
	if created != 2 {
		t.Errorf("the run counted %d records to clean up, want 2 (each Present owes a clean-up)", created)
	}
	rows, err := e.store.DNSRecordsFor(ctx, OwnerOrder, "order-new", "new")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows for the resumed order = %+v, %v; want the one restamped row", rows, err)
	}
	if all := e.ledger(); len(all) != 1 {
		t.Errorf("ledger = %+v, want one row", all)
	}
}

func TestManagerDNSTwoNamesShareOneStep(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	e.setAgent("https://agents.example.com")

	job := e.issue()

	if runs := e.runs(); len(runs) != 1 || runs[0].Outcome != OutcomeSucceeded {
		t.Fatalf("runs = %+v, want one succeeded run", runs)
	}
	count := map[string]int{}
	for _, it := range job.Items {
		count[it.ID]++
	}
	if count[StepChallenge] != 1 || count[StepPropagation] != 1 {
		t.Errorf("item counts = %v, want one Challenge and one Propagation step", count)
	}
	var made []string
	for _, c := range e.prov.Calls() {
		if c.Op == "create" {
			made = append(made, c.Name)
		}
	}
	if len(made) != 2 || made[0] == made[1] {
		t.Fatalf("records were created at %v, want two different names", made)
	}
	items := itemsOf(job)
	for _, id := range []string{StepChallenge, StepPropagation} {
		if got := items[id].Result["record"]; got != made[1] {
			t.Errorf("%s names the record %q, want the second record %q", id, got, made[1])
		}
	}
	if recs := e.prov.Records(); len(recs) != 0 {
		t.Errorf("the provider still holds %+v, want both records deleted", recs)
	}
}

func TestManagerDNSStepOrderMatchesTheList(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)
	rs := e.start()
	e.wait(rs)

	// Event 1 is the Account step ending; every later step announces itself running, in turn.
	_, events, cancel, err := e.hub.Subscribe(context.Background(), rs.JobID, 1)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()
	var order []string
	seen := map[string]bool{}
	for ev := range events {
		if ev.Type == jobs.EventItem && ev.Item.State == jobs.ItemRunning && !seen[ev.Item.ID] {
			seen[ev.Item.ID] = true
			order = append(order, ev.Item.ID)
		}
	}
	want := specIDs(stepItemsDNS01)[1:] // the Account step is skipped, never running
	if !slices.Equal(order, want) {
		t.Errorf("steps became running in the order %v, want the list's %v", order, want)
	}
}

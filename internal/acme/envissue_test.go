// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/acme/dns/cloudflare"
	"github.com/ianglennon/tierplan/internal/acme/dns/dnstest"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The start-time issue of TIERPLAN_ACME=on (phase 16, SET-02, D-05 to D-09).

// cfProvider is the provider id the Cloudflare client answers to.
const cfProvider = "cloudflare"

// cfEnv is a scheduler over a fake CA that validates DNS-01 by looking the TXT record up in the fake
// Cloudflare API, with a DNS responder that serves what that API holds. The manager is built the way a
// process started with the environment is: Deps.Env says what the variables say.
type cfEnv struct {
	*mgrEnv
	cf    *dnstest.FakeCloudflare
	dns   *acmetest.DNSServer
	logs  *syncBuffer
	ended chan string
}

func newCFEnv(t *testing.T) *cfEnv {
	t.Helper()
	cf := dnstest.NewFakeCloudflare(t)
	srv := acmetest.StartDNSServer(t)
	cf.Sink = srv
	cf.AddZone("example.com", dnsNS)

	oe := newOrderEnvWith(t, acmetest.Options{
		ChallengeType: ChallengeDNS01,
		LookupTXT: func(fqdn string) []string {
			var out []string
			for _, r := range cf.Records() {
				if strings.EqualFold(r.Name, fqdn) {
					out = append(out, r.Value())
				}
			}
			return out
		},
	})
	hub := jobs.New(jobs.Options{Now: oe.clock.Now})
	t.Cleanup(hub.Close)
	return &cfEnv{
		mgrEnv: &mgrEnv{orderEnv: oe, hub: hub, inst: &recInstaller{}, urls: &urlSet{public: "https://" + testHost}},
		cf:     cf, dns: srv, logs: &syncBuffer{}, ended: make(chan string, 64),
	}
}

// manager is a Manager over the stored state with env as the variables, as a restarted process builds one.
func (e *cfEnv) manager(env config.ACMEEnv) *Manager {
	return NewManager(Deps{
		Store: e.store, Client: e.client, Challenges: e.tokens, Jobs: e.hub, Installer: e.inst, Clock: e.clock,
		PublicURL: e.urls.getPublic, AgentURL: e.urls.getAgent, BasePath: e.urls.getBase,
		Log: slog.New(slog.NewTextHandler(e.logs, nil)), Env: env,
		Jitter:   func() float64 { return 0 },
		Rand:     func(n int64) int64 { return n / 2 },
		RunEnded: func(_, outcome string) { e.ended <- outcome },
		DNS: DNSDeps{
			Providers: map[string]dns.Factory{cfProvider: cloudflare.New(cloudflare.Options{
				BaseURL: e.cf.URL(), HTTPClient: e.cf.Client(), UserAgent: "TierPlan/test",
			})},
			NameserverAddrs: func(context.Context, string) ([]string, error) { return []string{e.dns.Addr()}, nil },
			ResolvConf:      func() ([]string, error) { return []string{e.dns.Addr()}, nil },
		},
	})
}

// dnsEnvVars is TIERPLAN_ACME=on with DNS-01, Cloudflare and the fake's token given by token.
func (e *cfEnv) dnsEnvVars(token string) config.ACMEEnv {
	on := true
	return config.ACMEEnv{
		Enabled: &on, AcceptTOS: true, Challenge: ChallengeDNS01, DNSProvider: cfProvider,
		CloudflareToken: token, TokenVar: config.EnvCloudflareTokenFile,
	}
}

// awaitEnd waits for the run a round started to end, and says how it ended.
func (e *cfEnv) awaitEnd() string {
	e.t.Helper()
	return testsupport.Await(e.t, "the start-time run to end", e.ended)
}

// sealedToken is the stored envelope of the provider's token, read from the table.
func (e *cfEnv) sealedToken() string {
	e.t.Helper()
	row, err := e.store.st.RQ.GetACMEDNSCredential(context.Background(), cfProvider)
	if err != nil {
		e.t.Fatalf("read the stored token: %v", err)
	}
	return row.TokenSealed
}

// With TIERPLAN_ACME=on, DNS-01 and the Cloudflare token in the environment, the start-time issue
// validates through DNS-01 with that token, and the token is not stored anywhere: it is read at start,
// held in memory, and removing the variable leaves the database as it was (D-05, T-16-24).
func TestEnvIssueAtStartThroughDNS01WithTheEnvToken(t *testing.T) {
	ctx := context.Background()
	e := newCFEnv(t)
	mgr := e.manager(e.dnsEnvVars(e.cf.Token))

	mgr.round(ctx)
	if got := e.awaitEnd(); got != OutcomeSucceeded {
		t.Fatalf("the start-time run ended %q, want succeeded\nlog:\n%s", got, e.logs.String())
	}

	runs := e.runs()
	if len(runs) != 1 || runs[0].Outcome != OutcomeSucceeded || runs[0].ChallengeType != ChallengeDNS01 || runs[0].Trigger != TriggerSchedule {
		t.Fatalf("runs = %+v, want one succeeded scheduled dns-01 run", runs)
	}
	if e.inst.served() != 1 {
		t.Errorf("ServeACME was called %d times, want 1", e.inst.served())
	}
	if got := e.fake.TXTLookups("_acme-challenge." + testHost); got < 1 {
		t.Errorf("the CA looked up the TXT record %d times, want at least once", got)
	}

	// Cloudflare saw the environment's bearer token, and the record it created was deleted by its ID.
	var created, deleted int
	for _, r := range e.cf.Requests() {
		if !r.Bearer || r.LegacyAuth {
			t.Errorf("request %s %s carried bearer=%v legacy=%v, want only a bearer token", r.Method, r.Path, r.Bearer, r.LegacyAuth)
		}
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/dns_records"):
			created++
		case r.Method == http.MethodDelete && strings.Contains(r.Path, "/dns_records/"):
			deleted++
		}
	}
	if created != 1 || deleted != 1 {
		t.Errorf("Cloudflare saw %d creates and %d deletes, want one of each", created, deleted)
	}
	if recs := e.cf.Records(); len(recs) != 0 {
		t.Errorf("Cloudflare still holds %+v after the run", recs)
	}
	if rows, err := e.store.DNSRecords(ctx); err != nil || len(rows) != 0 {
		t.Errorf("the record ledger holds %+v (%v) after the run, want it empty", rows, err)
	}

	// Nothing about the token reached the database or the log.
	if saved, err := e.store.DNSTokenSaved(ctx, cfProvider); err != nil || saved {
		t.Errorf("DNSTokenSaved = %v (%v), want no token stored for the environment's", saved, err)
	}
	if _, found, err := e.store.DNSToken(ctx, cfProvider); err != nil || found {
		t.Errorf("DNSToken found = %v (%v), want not found", found, err)
	}
	// The stored settings are the defaults: the variables decided the run without being written down.
	if st := e.state(); st.ChallengeType != ChallengeHTTP01 || len(st.DNSResolvers) != 0 {
		t.Errorf("stored challenge %q and resolvers %v, want the defaults: the variables were not written", st.ChallengeType, st.DNSResolvers)
	}
	if strings.Contains(e.logs.String(), e.cf.Token) {
		t.Error("the log holds the Cloudflare token")
	}
}

// A token the environment gives is used and a stored one is left alone: the stored envelope is
// byte-identical after the run, and the run used the variable's token, not the stored one (D-05).
func TestEnvIssueAtStartLeavesAStoredTokenUnchanged(t *testing.T) {
	ctx := context.Background()
	e := newCFEnv(t)
	const stored = "cf-stored-token-11f0b2c4" //nolint:gosec // G101: a recognisable fake token, so a test can prove it is not used
	if err := e.store.SaveDNSToken(ctx, cfProvider, stored, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	before := e.sealedToken()
	mgr := e.manager(e.dnsEnvVars(e.cf.Token))

	mgr.round(ctx)
	if got := e.awaitEnd(); got != OutcomeSucceeded {
		t.Fatalf("the start-time run ended %q, want succeeded with the environment's token\nlog:\n%s", got, e.logs.String())
	}

	if after := e.sealedToken(); before != after {
		t.Error("the stored token's envelope changed, want it untouched")
	}
	if tok, found, err := e.store.DNSToken(ctx, cfProvider); err != nil || !found || tok != stored {
		t.Errorf("the stored token reads %q (found %v, %v), want the one that was stored", tok, found, err)
	}
	if strings.Contains(e.logs.String(), e.cf.Token) || strings.Contains(e.logs.String(), stored) {
		t.Error("the log holds a Cloudflare token")
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// envTOS is the terms of service the fake CA advertises, so an account needs the agreement.
const envTOS = "https://ca.example.test/terms"

// envACMEOn is the environment of an install that switches Let's Encrypt on and accepts the terms.
func envACMEOn() map[string]string {
	return map[string]string{
		"TIERPLAN_ACME":            "on",
		"TIERPLAN_ACME_ACCEPT_TOS": "true",
		"TIERPLAN_ACME_EMAIL":      "ops@example.com",
	}
}

// awaitListening waits until the server accepts connections.
func awaitListening(t *testing.T, srv *Server) {
	t.Helper()
	testsupport.Await(t, "the server to listen", srv.Listening())
}

// acmeJobs are the certificate runs the server has started.
func acmeJobs(srv *Server) []jobs.Job {
	return srv.jobs.List(func(j jobs.Job) bool { return j.Kind == acme.JobKind })
}

// TIERPLAN_ACME=on issues at start without a click (D-07): once the listener is bound the scheduler
// registers the account, with the variable's address as contact, and places one order as a scheduled
// run owned by the scheduler. Nothing is ordered before the listener is up, so the run does not fail
// its validation, and later ticks order nothing more.
func TestACMEEnvIssuesAtStart(t *testing.T) {
	r := newACMERig(t, acmetest.Options{TermsOfService: envTOS}, envACMEOn())
	srv := r.startNew()
	awaitListening(t, srv)
	// The listener is bound now, so this tick's round is allowed to order.
	r.step(acme.Tick)
	r.awaitRuns()

	jobsSeen := acmeJobs(srv)
	if len(jobsSeen) != 1 {
		t.Fatalf("%d certificate runs, want exactly one", len(jobsSeen))
	}
	if got := jobsSeen[0].Owner; got != acme.OwnerScheduler {
		t.Errorf("the run is owned by %q, want the scheduler %q", got, acme.OwnerScheduler)
	}
	r.requireNoFailedStep(jobsSeen[0].ID)

	st, err := srv.acme.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.LastRun == nil || st.LastRun.Trigger != acme.TriggerSchedule || st.LastRun.Outcome != acme.OutcomeSucceeded {
		t.Errorf("last run = %+v, want a succeeded scheduled run", st.LastRun)
	}
	if st.Failures != 0 || st.LastError != nil {
		t.Errorf("failures = %d, last error = %+v, want none: the run must not order before the listener is up", st.Failures, st.LastError)
	}
	if st.Mode != acme.ModeActive {
		t.Errorf("mode = %q, want active", st.Mode)
	}

	accounts := r.fake.AccountsSeen()
	if len(accounts) != 1 || !slices.Equal(accounts[0].Contact, []string{"mailto:ops@example.com"}) || !accounts[0].TermsOfServiceAgreed {
		t.Errorf("accounts seen = %+v, want one with the contact from TIERPLAN_ACME_EMAIL and the terms agreed", accounts)
	}
	if got := r.fake.Count(acmetest.EndpointNewOrder); got != 1 {
		t.Errorf("%d orders placed, want exactly one", got)
	}
	if got := r.fake.Orders(); len(got) != 1 || !slices.Equal(got[0].Names, []string{acmeHost}) {
		t.Errorf("orders = %+v, want one for %s", got, acmeHost)
	}

	// The certificate is served, and further ticks place nothing.
	if srv.https.Kind() != tlsKindACME {
		t.Errorf("served kind = %d, want tlsKindACME", srv.https.Kind())
	}
	if leaf, err := r.handshake(r.fake.IssuerPool()); err != nil || !r.issuedBy(leaf) {
		t.Errorf("the handshake does not present a certificate the fake CA issued: %v", err)
	}
	for range 3 {
		r.step(acme.Tick)
	}
	r.awaitRuns()
	if got := r.fake.Count(acmetest.EndpointNewOrder); got != 1 {
		t.Errorf("%d orders after more ticks, want still one", got)
	}
}

// Without the variable the scheduler never starts an issue: issuing stays an explicit action for an
// install that is set up in the UI (D-07).
func TestACMEEnvUnsetNeverIssuesAtStart(t *testing.T) {
	r := newACMERig(t, acmetest.Options{TermsOfService: envTOS}, nil)
	srv := r.startNew()
	awaitListening(t, srv)
	for range 4 {
		r.step(acme.Tick)
	}
	if got := r.fake.Count(acmetest.EndpointNewOrder); got != 0 {
		t.Errorf("%d orders placed with TIERPLAN_ACME unset, want none", got)
	}
	if got := r.fake.Count(acmetest.EndpointNewAccount); got != 0 {
		t.Errorf("%d accounts registered with TIERPLAN_ACME unset, want none", got)
	}
	if n := len(acmeJobs(srv)); n != 0 {
		t.Errorf("%d certificate runs with TIERPLAN_ACME unset, want none", n)
	}
	if srv.https.Kind() == tlsKindACME {
		t.Error("a Let's Encrypt certificate is served with TIERPLAN_ACME unset")
	}
}

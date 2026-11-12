// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/backup"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/vault"
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

// putProfile sends PUT /api/https/acme/profile as the signed-in admin and returns the status code and the
// problem type the answer carries.
func (r *acmeRig) putProfile(c *http.Client, cookie *http.Cookie, csrf, profile string) (int, string) {
	r.t.Helper()
	req, err := http.NewRequestWithContext(testsupport.Context(r.t), http.MethodPut, r.base+"/api/https/acme/profile",
		strings.NewReader(`{"profile":"`+profile+`"}`))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(cookie)
	resp, err := c.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var problem struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(body, &problem)
	return resp.StatusCode, problem.Type
}

// storedProfile is the profile in the database, read from the store and never from the status.
func (r *acmeRig) storedProfile() string {
	r.t.Helper()
	st, err := acme.NewStore(r.srv.Store(), r.srv.Vault()).State(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return st.Profile
}

// TIERPLAN_ACME_PROFILE locks the profile end to end (D-24, D-17): the status names the variable, a
// different value answers 409 and stores nothing, the variable's own value is accepted and stores
// nothing, and an order asks the CA for the variable's profile.
func TestACMEEnvLocksTheProfile(t *testing.T) {
	r := newACMERig(t, acmetest.Options{TermsOfService: envTOS}, map[string]string{"TIERPLAN_ACME_PROFILE": "shortlived"})
	srv := r.startNew()
	awaitListening(t, srv)
	client := r.client(r.mirrorPool())
	cookie := r.signIn(client)
	csrf := r.csrf(client, cookie)

	got := r.trustStatus(client, cookie).ACME
	if got == nil || got.Locks.Profile == nil {
		t.Fatalf("status = %+v, want locks.profile", got)
	}
	if l := got.Locks.Profile; l.Var != "TIERPLAN_ACME_PROFILE" || l.Value != "shortlived" || l.Stored != "" {
		t.Errorf("locks.profile = %+v, want var TIERPLAN_ACME_PROFILE, value shortlived and nothing stored", l)
	}
	if got.Profile != "shortlived" {
		t.Errorf("profile = %q, want the variable's profile in force", got.Profile)
	}
	if l := got.Locks; l.ACME != nil || l.Email != nil || l.AcceptTOS != nil {
		t.Errorf("locks = %+v, want only the profile locked: no other variable is set", l)
	}

	if code, typ := r.putProfile(client, cookie, csrf, "classic"); code != http.StatusConflict || typ != "acme_profile_locked" {
		t.Errorf("PUT profile classic = %d %q, want 409 acme_profile_locked", code, typ)
	}
	if p := r.storedProfile(); p != "" {
		t.Errorf("stored profile = %q after a refused write, want nothing stored", p)
	}
	if code, typ := r.putProfile(client, cookie, csrf, "shortlived"); code != http.StatusOK {
		t.Errorf("PUT profile shortlived = %d %q, want 200: echoing the fixed value is not a change", code, typ)
	}
	if p := r.storedProfile(); p != "" {
		t.Errorf("stored profile = %q after the echo, want nothing stored", p)
	}

	r.issue()
	if got := r.fake.Orders(); len(got) != 1 || got[0].Profile != "shortlived" {
		t.Errorf("orders = %+v, want one that asks for the profile shortlived", got)
	}
}

// refusedOrder is a CA answer that fails an order without telling the client to wait.
var refusedOrder = acmetest.Problem{Status: 400, Type: "malformed", Detail: "scripted refusal"}

// restartAndRound restarts the server on its address, as a supervisor does after a crash, and lets one
// scheduler round pass once the listener is up. It returns the new server with its runs finished.
func (r *acmeRig) restartAndRound(addr string) *Server {
	r.t.Helper()
	srv := r.restart(addr)
	awaitListening(r.t, srv)
	r.step(acme.Tick)
	r.awaitRuns()
	return srv
}

// restartPastBackoff is restartAndRound for a restart whose round comes after the persisted wait: the
// ledger's backoff carries up to ten percent of jitter, so the round is placed at the end of the wait
// the running server reports, and at least one tick on.
func (r *acmeRig) restartPastBackoff(addr string) *Server {
	r.t.Helper()
	st, err := r.srv.acme.Status(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	wait := max(acme.Tick, st.NextAttemptAt.Sub(r.clock.Now()))
	srv := r.restart(addr)
	awaitListening(r.t, srv)
	r.step(wait)
	r.awaitRuns()
	return srv
}

// A restart loop never orders early (D-07, 14 D-22, T-16-25). The ledger that holds the wait is in the
// database, so a process that is started again every minute orders no more often than one that runs:
// with a CA that refuses, five restarts inside the second backoff step place no order, and the order
// that follows comes when the persisted step ends.
func TestACMEEnvRestartLoopStaysInsideThePersistedBackoff(t *testing.T) {
	r := newACMERig(t, acmetest.Options{TermsOfService: envTOS}, envACMEOn())
	r.fake.FailNext(acmetest.EndpointNewOrder, refusedOrder)
	r.fake.FailNext(acmetest.EndpointNewOrder, refusedOrder)
	srv := r.startNew()
	addr := r.address()
	awaitListening(t, srv)
	r.step(acme.Tick)
	r.awaitRuns()
	if got := r.fake.Count(acmetest.EndpointNewOrder); got != 1 {
		t.Fatalf("%d orders after the first start, want 1", got)
	}

	// The first step is a minute: a restart whose round comes after it places the second order, which
	// fails, and the persisted wait is now ten minutes.
	r.restartPastBackoff(addr)
	if got := r.fake.Count(acmetest.EndpointNewOrder); got != 2 {
		t.Fatalf("%d orders after the first restart, want 2", got)
	}
	failedAt := r.clock.Now()
	if st, err := r.srv.acme.Status(context.Background()); err != nil || st.NextAttemptAt.Sub(failedAt) < 9*time.Minute {
		t.Fatalf("next attempt %v (%v), want the step of about ten minutes after %v", st.NextAttemptAt, err, failedAt)
	}

	for i := range 5 {
		r.restartAndRound(addr)
		if got := r.fake.Count(acmetest.EndpointNewOrder); got != 2 {
			t.Fatalf("%d orders after restart %d inside the ten-minute step, want still 2", got, i+1)
		}
	}
	if st, err := r.srv.acme.Status(context.Background()); err != nil || st.Failures != 2 {
		t.Fatalf("failures = %d (%v), want the two charged runs and nothing for the restarts", st.Failures, err)
	}

	// Once the persisted step has ended, the order is placed.
	r.restartPastBackoff(addr)
	if got := r.fake.Count(acmetest.EndpointNewOrder); got != 3 {
		t.Errorf("%d orders once the persisted step ended, want 3", got)
	}
	if st, err := r.srv.acme.Status(context.Background()); err != nil || st.Mode != acme.ModeActive || st.Failures != 0 {
		t.Errorf("mode = %q, failures = %d (%v), want the third order to have issued", st.Mode, st.Failures, err)
	}
}

// With TIERPLAN_ACME=on and the wanted names changed by a new TIERPLAN_PUBLIC_URL, a restart orders for
// the new names; with the variable unset the certificate stays until the admin acts (D-08, 14 D-05).
func TestACMEEnvNamesChangeReissuesAtStart(t *testing.T) {
	r := newACMERig(t, acmetest.Options{TermsOfService: envTOS}, envACMEOn())
	srv := r.startNew()
	addr := r.address()
	awaitListening(t, srv)
	r.step(acme.Tick)
	r.awaitRuns()
	if got := r.fake.Orders(); len(got) != 1 || !slices.Equal(got[0].Names, []string{acmeHost}) {
		t.Fatalf("orders = %+v, want one for %s", got, acmeHost)
	}

	const newHost = "moved.tierplan.test"
	r.setEnv(map[string]string{"TIERPLAN_PUBLIC_URL": "https://" + newHost, "TIERPLAN_ACME": ""})
	r.restartAndRound(addr)
	if got := r.fake.Count(acmetest.EndpointNewOrder); got != 1 {
		t.Errorf("%d orders with the variable unset and the names changed, want the admin to be waited for", got)
	}

	r.setEnv(map[string]string{"TIERPLAN_ACME": "on"})
	srv = r.restartAndRound(addr)
	got := r.fake.Orders()
	if len(got) != 2 || !slices.Equal(got[1].Names, []string{newHost}) {
		t.Fatalf("orders = %+v, want a second one for %s", got, newHost)
	}
	if leaf := leafOf(t, srv.https.Config()); !slices.Equal(leaf.DNSNames, []string{newHost}) {
		t.Errorf("the certificate served covers %v, want [%s]", leaf.DNSNames, newHost)
	}
	r.restartAndRound(addr)
	if n := r.fake.Count(acmetest.EndpointNewOrder); n != 2 {
		t.Errorf("%d orders once the names match, want still 2", n)
	}
}

// After a restore TIERPLAN_ACME=on orders nothing until an admin clicks Resume, and orders on the next
// round after it (D-09).
func TestACMEEnvAfterARestoreOrdersOnlyAfterResume(t *testing.T) {
	ctx := context.Background()
	r := newACMERig(t, acmetest.Options{TermsOfService: envTOS}, nil)
	dirA := r.cfg.DataDir
	a := r.startNew()
	r.issue()
	var file bytes.Buffer
	if _, err := backup.Export(ctx, a.Store(), a.Vault(), dirA, &file, []byte(restoreBackupPass),
		backup.ExportOptions{AppVersion: "v0.1.1", InstanceName: "A"}); err != nil {
		t.Fatal(err)
	}
	r.stop()

	dirB := t.TempDir() + "/data"
	if err := os.MkdirAll(dirB, 0o700); err != nil {
		t.Fatal(err)
	}
	target, err := vault.LoadOrCreate("", dirB, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	schema, err := store.TargetSchema()
	if err != nil {
		t.Fatal(err)
	}
	staged, err := backup.Stage(ctx, bytes.NewReader(file.Bytes()), []byte(restoreBackupPass), dirB, schema)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	defer staged.Discard()
	if err := staged.Prepare(ctx, target, backup.PrepareOptions{Source: "cli", AppVersion: "v0.1.1"}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := staged.Swap(ctx, dirB, time.Now().UTC()); err != nil {
		t.Fatalf("Swap: %v", err)
	}

	// The restored install is switched on by the environment and wants names its certificate lacks, so
	// it would order at once were it not paused.
	const newHost = "restored.tierplan.test"
	vars := envACMEOn()
	vars["TIERPLAN_DATA_DIR"], vars["TIERPLAN_PUBLIC_URL"] = dirB, "https://"+newHost
	r.setEnv(vars)
	ordersBefore := r.fake.Count(acmetest.EndpointNewOrder)
	b := r.startNew()
	awaitListening(t, b)
	if st, err := b.acme.Status(ctx); err != nil || st.PausedReason != acme.PausedRestore {
		t.Fatalf("PausedReason = %q (%v), want %q", st.PausedReason, err, acme.PausedRestore)
	}
	for range 3 {
		r.step(acme.Tick)
	}
	r.awaitRuns()
	if got := r.fake.Count(acmetest.EndpointNewOrder); got != ordersBefore {
		t.Fatalf("%d orders while paused after the restore, want none beyond the %d before", got-ordersBefore, ordersBefore)
	}

	if err := b.acme.Resume(ctx, "test"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	r.step(acme.Tick)
	r.awaitRuns()
	got := r.fake.Orders()
	if len(got) != ordersBefore+1 || !slices.Equal(got[len(got)-1].Names, []string{newHost}) {
		t.Errorf("orders = %+v, want one more, for %s, on the round after Resume", got, newHost)
	}
}

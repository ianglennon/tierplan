// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"crypto/x509"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// auditSink collects the audit entries a Manager writes, in the order it wrote them.
type auditSink struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (k *auditSink) record(_ context.Context, e audit.Entry) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.entries = append(k.entries, e)
	return nil
}

func (k *auditSink) all() []audit.Entry {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]audit.Entry(nil), k.entries...)
}

// byAction is the entries of the given actions, in order.
func (k *auditSink) byAction(actions ...string) []audit.Entry {
	var out []audit.Entry
	for _, e := range k.all() {
		for _, a := range actions {
			if e.Action == a {
				out = append(out, e)
			}
		}
	}
	return out
}

// outcomes are the three actions that end a run.
func (k *auditSink) outcomes() []audit.Entry {
	return k.byAction(audit.ActionHTTPSIssued, audit.ActionHTTPSRenewed, audit.ActionHTTPSRunFailed)
}

func (k *auditSink) indexOf(action string) int {
	for i, e := range k.all() {
		if e.Action == action {
			return i
		}
	}
	return -1
}

// auditEnv is a scheduler environment whose Manager writes its audit rows to a sink and tells the test, on
// RunEnded, how many rows the sink held at that moment.
type auditEnv struct {
	*schedEnv
	sink *auditSink
	// atEnd is the sink's length when each run was told to have ended, by run ID.
	mu    sync.Mutex
	atEnd map[string]int
}

func newAuditEnv(t *testing.T, opts acmetest.Options) *auditEnv {
	t.Helper()
	s := newSchedEnv(t, opts)
	a := &auditEnv{schedEnv: s, sink: &auditSink{}, atEnd: map[string]int{}}
	s.mgr = a.build(s.inst)
	return a
}

// build is a Manager over the environment's stored state that writes its audit rows to the sink.
func (a *auditEnv) build(inst Installer) *Manager { return a.buildWithEnv(inst, config.ACMEEnv{}) }

// buildWithEnv is build with the environment's TIERPLAN_ACME* values, the way a process start builds one.
func (a *auditEnv) buildWithEnv(inst Installer, env config.ACMEEnv) *Manager {
	return NewManager(Deps{
		Store: a.store, Client: a.client, Challenges: a.tokens, Jobs: a.hub, Installer: inst, Clock: a.clock,
		PublicURL: a.urls.getPublic, AgentURL: a.urls.getAgent, BasePath: a.urls.getBase,
		Env: env, Log: slog.New(slog.DiscardHandler),
		Jitter: func() float64 { return 0 },
		Rand:   func(n int64) int64 { return n / 2 },
		Audit:  a.sink.record,
		RunEnded: func(id, outcome string) {
			a.mu.Lock()
			a.atEnd[id] = len(a.sink.all())
			a.mu.Unlock()
			a.ended <- outcome
		},
	})
}

func (a *auditEnv) rowsAtEnd(runID string) (int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n, ok := a.atEnd[runID]
	return n, ok
}

// onlyOutcome is the one outcome row the sink holds, failing the test when it holds another number.
func (a *auditEnv) onlyOutcome() audit.Entry {
	a.t.Helper()
	rows := a.sink.outcomes()
	if len(rows) != 1 {
		a.t.Fatalf("the sink holds %d outcome rows, want 1: %+v", len(rows), rows)
	}
	return rows[0]
}

// gatedInstaller signals when ServeACME is entered and holds it until the test lets it go.
type gatedInstaller struct {
	entered chan struct{}
	proceed chan struct{}
}

func (g *gatedInstaller) ServeACME(_, _ []byte) error {
	close(g.entered)
	<-g.proceed
	return nil
}

func (g *gatedInstaller) ServeSelfSigned() error { return nil }

func TestRunAudit(t *testing.T) {
	ctx := context.Background()

	t.Run("an admin issue writes one https.issued row as system:acme", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		rs, err := a.mgr.Issue(ctx, "user:1")
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		a.wait(rs)
		if got := a.awaitRun(); got != OutcomeSucceeded {
			t.Fatalf("the run ended %q, want succeeded", got)
		}
		leaf := leafOf(t, a.inst.chains[0])
		e := a.onlyOutcome()
		if e.Action != audit.ActionHTTPSIssued || e.Actor != audit.SystemACME || e.Result != audit.ResultSuccess {
			t.Fatalf("row = %+v, want https.issued by system:acme, success", e)
		}
		if e.TargetType != "https" || e.TargetID != "https" || e.EnvID != "" {
			t.Errorf("target = %q/%q env %q, want https/https and no environment", e.TargetType, e.TargetID, e.EnvID)
		}
		want := map[string]string{
			"trigger": TriggerAdmin, "started_by": "user:1", "job": rs.JobID, "names": testHost,
			"issuer": issuerName(leaf), "serial": leaf.SerialNumber.Text(16),
			"not_after": leaf.NotAfter.UTC().Format(time.RFC3339), "challenge": ChallengeHTTP01, "served": "true",
			"profile": a.mgr.profile(a.state()),
		}
		if want["profile"] == "" {
			delete(want, "profile")
		}
		for k, v := range want {
			if e.Details[k] != v {
				t.Errorf("details[%q] = %q, want %q", k, e.Details[k], v)
			}
		}
		if _, ok := e.Details["cancelled"]; ok {
			t.Errorf("a run nobody cancelled has cancelled = %q", e.Details["cancelled"])
		}
		if e.Details["issuer"] == "" || e.Details["serial"] == "" {
			t.Errorf("issuer %q and serial %q should be filled", e.Details["issuer"], e.Details["serial"])
		}
	})

	t.Run("an account the CA no longer knows is one https.account_forgotten row beside the failure row, both before RunEnded", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		a.fake.ForgetAccounts()
		rs, err := a.mgr.Issue(ctx, "user:1")
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		a.wait(rs)
		if got := a.awaitRun(); got != OutcomeFailed {
			t.Fatalf("the run ended %q, want failed", got)
		}
		e := a.onlyOutcome()
		if e.Action != audit.ActionHTTPSRunFailed || e.Details["class"] != ClassAccountGone {
			t.Fatalf("row = %+v, want https.run_failed with class %s", e, ClassAccountGone)
		}
		gone := a.sink.byAction(audit.ActionHTTPSAccountForgotten)
		if len(gone) != 1 {
			t.Fatalf("%d https.account_forgotten rows, want 1: %+v", len(gone), a.sink.all())
		}
		dir, err := url.Parse(a.client.Directory().URL)
		if err != nil {
			t.Fatal(err)
		}
		f := gone[0]
		if f.Actor != audit.SystemACME || f.Result != audit.ResultSuccess || f.TargetType != "https" || f.TargetID != "https" {
			t.Errorf("row = %+v, want a success by system:acme about https", f)
		}
		if len(f.Details) != 2 || f.Details["directory"] != dir.Host || f.Details["job"] != rs.JobID {
			t.Errorf("details = %+v, want only the directory host %q and the job %q", f.Details, dir.Host, rs.JobID)
		}
		if n, ok := a.rowsAtEnd(rs.JobID); !ok || n != 2 {
			t.Errorf("the sink held %d rows when RunEnded fired (told: %v), want the failure and the forgotten account", n, ok)
		}
	})

	t.Run("the outcome row is in the sink before RunEnded is told", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		rs, err := a.mgr.Issue(ctx, "user:1")
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		a.wait(rs)
		n, ok := a.rowsAtEnd(rs.JobID)
		if !ok || n != 1 {
			t.Errorf("the sink held %d rows when RunEnded fired (told: %v), want the run's row", n, ok)
		}
	})

	t.Run("renew now writes https.renewed", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		a.issueFirst()
		rs, err := a.mgr.RenewNow(ctx, "user:2")
		if err != nil {
			t.Fatalf("RenewNow: %v", err)
		}
		a.wait(rs)
		a.awaitRun()
		rows := a.sink.byAction(audit.ActionHTTPSRenewed)
		if len(rows) != 1 {
			t.Fatalf("%d https.renewed rows, want 1: %+v", len(rows), a.sink.all())
		}
		d := rows[0].Details
		if d["served"] != "true" || d["started_by"] != "user:2" || d["job"] != rs.JobID || d["trigger"] != TriggerAdmin {
			t.Errorf("details = %+v", d)
		}
		if rows[0].Actor != audit.SystemACME {
			t.Errorf("actor = %+v, want system:acme", rows[0].Actor)
		}
	})

	t.Run("a certificate stored but not served says served false and stays a success", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		a.inst.serveErr = errTestInstall
		a.issue()
		a.awaitRun()
		e := a.onlyOutcome()
		if e.Action != audit.ActionHTTPSIssued || e.Result != audit.ResultSuccess || e.Details["served"] != "false" {
			t.Errorf("row = %+v, want https.issued, success, served false", e)
		}
		for k, v := range e.Details {
			if strings.Contains(v, errTestInstall.Error()) {
				t.Errorf("details[%q] carries the install error text", k)
			}
		}
	})

	t.Run("a failed run writes one https.run_failed row with the class and no CA text", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		const caText = "scripted validation detail the bell must not see"
		a.fake.FailValidation(testHost, acmetest.Problem{Status: 403, Type: "unauthorized", Detail: caText})
		rs, err := a.mgr.Issue(ctx, "user:1")
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		a.wait(rs)
		if got := a.awaitRun(); got != OutcomeFailed {
			t.Fatalf("the run ended %q, want failed", got)
		}
		e := a.onlyOutcome()
		if e.Action != audit.ActionHTTPSRunFailed || e.Result != audit.ResultFailure || e.Actor != audit.SystemACME {
			t.Fatalf("row = %+v, want https.run_failed by system:acme, failure", e)
		}
		if e.Reason == "" || e.Reason != e.Details["class"] || e.Reason != a.state().LastErrorClass {
			t.Errorf("reason %q, class %q, stored class %q: want all equal and not empty", e.Reason, e.Details["class"], a.state().LastErrorClass)
		}
		d := e.Details
		if d["kind"] != KindIssue || d["host"] != testHost || d["trigger"] != TriggerAdmin || d["started_by"] != "user:1" ||
			d["job"] != rs.JobID || d["challenge"] != ChallengeHTTP01 {
			t.Errorf("details = %+v", d)
		}
		for k, v := range d {
			if strings.Contains(v, caText) {
				t.Errorf("details[%q] carries the CA's text", k)
			}
		}
		if strings.Contains(e.Reason, caText) {
			t.Error("the reason carries the CA's text")
		}
		if n, ok := a.rowsAtEnd(rs.JobID); !ok || n != 1 {
			t.Errorf("the sink held %d rows when RunEnded fired, want the failed run's row", n)
		}
	})

	t.Run("each scheduled backoff attempt is one row", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		a.issueFirst()
		a.fake.FailValidation(testHost, acmetest.Problem{Status: 403, Type: "unauthorized", Detail: "scripted"})
		a.schedule(a.mgr)

		a.stepTo(a.state().RenewAt)
		if got := a.awaitRun(); got != OutcomeFailed {
			t.Fatalf("the first scheduled run ended %q, want failed", got)
		}
		a.step(Tick)
		if got := a.awaitRun(); got != OutcomeFailed {
			t.Fatalf("the second scheduled run ended %q, want failed", got)
		}
		// Ten quiet minutes follow the second failure: no run, so no row.
		a.idle(9*time.Minute, 3)

		failed := a.sink.byAction(audit.ActionHTTPSRunFailed)
		if len(failed) != 2 {
			t.Fatalf("%d https.run_failed rows, want one per attempt (2): %+v", len(failed), a.sink.all())
		}
		for _, e := range failed {
			if e.Details["trigger"] != TriggerSchedule || e.Details["kind"] != KindRenew || e.Actor != audit.SystemACME {
				t.Errorf("row = %+v", e)
			}
			if _, ok := e.Details["started_by"]; ok {
				t.Errorf("a scheduled run has started_by = %q", e.Details["started_by"])
			}
		}
		if failed[0].Details["job"] == failed[1].Details["job"] {
			t.Error("the two rows name the same job")
		}
	})

	t.Run("a run cancelled before its certificate writes no outcome row and the hook's row stays", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		hooked := a.hookCtx(ctx)
		rs, release := a.holdRun(func() (RunStart, error) { return a.mgr.Issue(hooked, "user:1") })
		if err := a.hub.Cancel(rs.JobID); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		a.wait(rs)
		release()
		if got := a.awaitRun(); got != OutcomeCancelled {
			t.Fatalf("the run ended %q, want cancelled", got)
		}
		if rows := a.sink.outcomes(); len(rows) != 0 {
			t.Errorf("a cancelled run wrote outcome rows: %+v", rows)
		}
		if markers := a.sink.byAction(audit.ActionHTTPSIssueRequested); len(markers) != 1 || markers[0].Details["job"] != rs.JobID {
			t.Errorf("the caller's row = %+v, want it kept, naming job %s", markers, rs.JobID)
		}
	})

	t.Run("a run cut short by shutdown writes no outcome row", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		rs, release := a.holdRun(func() (RunStart, error) { return a.mgr.Issue(ctx, "user:1") })
		a.hub.Close()
		release()
		if got := a.awaitRun(); got != OutcomeFailed {
			t.Fatalf("the run ended %q, want failed (interrupted)", got)
		}
		if rows := a.sink.outcomes(); len(rows) != 0 {
			t.Errorf("a run cut short by shutdown wrote outcome rows: %+v (job %s)", rows, rs.JobID)
		}
	})

	t.Run("a cancel after the order is final keeps the issued row, marked cancelled", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		gate := &gatedInstaller{entered: make(chan struct{}), proceed: make(chan struct{})}
		a.mgr = a.build(gate)
		rs, err := a.mgr.Issue(ctx, "user:1")
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		testsupport.Await(t, "the install to begin", gate.entered)
		if err := a.hub.Cancel(rs.JobID); err != nil {
			t.Fatalf("Cancel while the certificate is being installed: %v", err)
		}
		close(gate.proceed)
		a.wait(rs)
		if got := a.awaitRun(); got != OutcomeSucceeded {
			t.Fatalf("the run ended %q, want succeeded", got)
		}
		e := a.onlyOutcome()
		if e.Action != audit.ActionHTTPSIssued || e.Result != audit.ResultSuccess || e.Actor != audit.SystemACME {
			t.Fatalf("row = %+v, want the normal https.issued row", e)
		}
		if e.Details["job"] != rs.JobID || e.Details["served"] != "true" || e.Details["cancelled"] != "true" {
			t.Errorf("details = %+v, want job %s, served true, cancelled true", e.Details, rs.JobID)
		}
	})

	t.Run("the caller's hook row precedes the outcome row of a run that fails at its first CA call", func(t *testing.T) {
		for _, kind := range []string{KindIssue, KindRenew} {
			t.Run(kind, func(t *testing.T) {
				a := newAuditEnv(t, acmetest.Options{})
				if kind == KindRenew {
					a.issueFirst()
				}
				a.fake.FailNext(acmetest.EndpointNewOrder, acmetest.Problem{Status: 400, Type: "malformed", Detail: "scripted"})
				var calls []RunStart
				runsBefore, ordersBefore := len(a.runs()), a.fake.Count(acmetest.EndpointNewOrder)
				hooked := WithRunStarted(ctx, func(rs RunStart) {
					calls = append(calls, rs)
					// The run has done nothing yet: no run row, and the CA has not been asked.
					if n := len(a.runs()); n != runsBefore {
						t.Errorf("the run had stored %d run rows when the hook ran, want %d", n, runsBefore)
					}
					if n := a.fake.Count(acmetest.EndpointNewOrder); n != ordersBefore {
						t.Errorf("the CA had seen %d orders when the hook ran, want %d", n, ordersBefore)
					}
					_ = a.sink.record(ctx, audit.Entry{Action: audit.ActionHTTPSIssueRequested, Details: map[string]string{"job": rs.JobID}})
				})
				var rs RunStart
				var err error
				if kind == KindIssue {
					rs, err = a.mgr.Issue(hooked, "user:1")
				} else {
					rs, err = a.mgr.RenewNow(hooked, "user:1")
				}
				if err != nil {
					t.Fatalf("start: %v", err)
				}
				a.wait(rs)
				a.awaitRun()
				if len(calls) != 1 || calls[0].JobID != rs.JobID {
					t.Fatalf("the hook ran %d times with %+v, want once with job %s", len(calls), calls, rs.JobID)
				}
				marker, failed := a.sink.indexOf(audit.ActionHTTPSIssueRequested), a.sink.indexOf(audit.ActionHTTPSRunFailed)
				if marker < 0 || failed < 0 || marker > failed {
					t.Errorf("marker at %d, run_failed at %d: the caller's row must come first; rows %+v", marker, failed, a.sink.all())
				}
			})
		}
	})

	t.Run("the hook does not run for a refusal or a reused certificate", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		var calls int
		hooked := WithRunStarted(ctx, func(RunStart) { calls++ })

		// Nothing is issued yet: a renewal is refused.
		if _, err := a.mgr.RenewNow(hooked, "user:1"); err == nil {
			t.Fatal("RenewNow with no certificate was accepted")
		}
		// A run is active: a second Issue is refused.
		rs, release := a.holdRun(func() (RunStart, error) { return a.mgr.Issue(ctx, "user:1") })
		if _, err := a.mgr.Issue(hooked, "user:2"); err == nil {
			t.Fatal("a second Issue during a run was accepted")
		}
		release()
		a.clock.Advance(PollInterval)
		a.wait(rs)
		a.awaitRun()
		// The certificate covers the names: Issue reuses it and starts no run.
		reused, err := a.mgr.Issue(hooked, "user:1")
		if err != nil || !reused.Reused {
			t.Fatalf("Issue = %+v, %v, want the stored certificate reused", reused, err)
		}
		if calls != 0 {
			t.Errorf("the hook ran %d times for runs that were never started", calls)
		}
	})

	t.Run("a run started without a hook starts and ends as before", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		a.issueFirst()
		if rows := a.sink.byAction(audit.ActionHTTPSIssued); len(rows) != 1 {
			t.Errorf("%d https.issued rows, want 1", len(rows))
		}
	})
}

// hookCtx is a context whose WithRunStarted hook writes a stand-in for the admin's request row.
func (a *auditEnv) hookCtx(ctx context.Context) context.Context {
	return WithRunStarted(ctx, func(rs RunStart) {
		_ = a.sink.record(ctx, audit.Entry{Action: audit.ActionHTTPSIssueRequested, Details: map[string]string{"job": rs.JobID}})
	})
}

// onEnv is TIERPLAN_ACME=on with the terms accepted and an address, which is all a start-time registration needs.
func onEnv() config.ACMEEnv {
	env := envSwitch(true)
	env.AcceptTOS = true
	env.Email = "ops@example.com"
	return env
}

// The run TIERPLAN_ACME=on starts and the account it registers are the system's own doing, and the log says so (SET-04,
// D-10, D-14, D-28). The stored run stays a scheduled one: env is a word of the audit rows only.
func TestEnvRunAudit(t *testing.T) {
	ctx := context.Background()

	t.Run("the account a start-time issue registers is one row and the run's row says env", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		a.noAccount()
		a.buildWithEnv(a.inst, onEnv()).round(ctx)
		if got := a.awaitRun(); got != OutcomeSucceeded {
			t.Fatalf("the start-time run ended %q, want succeeded", got)
		}

		regs := a.sink.byAction(audit.ActionHTTPSAccountRegistered)
		if len(regs) != 1 {
			t.Fatalf("%d https.account_registered rows, want 1: %+v", len(regs), a.sink.all())
		}
		reg := regs[0]
		dir, err := url.Parse(a.client.Directory().URL)
		if err != nil {
			t.Fatal(err)
		}
		if reg.Actor != audit.SystemACME || reg.Result != audit.ResultSuccess || reg.TargetType != "https" || reg.TargetID != "https" {
			t.Errorf("row = %+v, want a success by system:acme about https", reg)
		}
		want := map[string]string{"contact": "ops@example.com", "terms_agreed": "true", "directory": dir.Host, "trigger": "env"}
		if len(reg.Details) != len(want) {
			t.Errorf("details = %+v, want only %v", reg.Details, want)
		}
		for k, v := range want {
			if reg.Details[k] != v {
				t.Errorf("details[%q] = %q, want %q", k, reg.Details[k], v)
			}
		}

		e := a.onlyOutcome()
		if e.Action != audit.ActionHTTPSIssued || e.Actor != audit.SystemACME || e.Details["trigger"] != "env" || e.Details["started_by"] != "" {
			t.Errorf("row = %+v, want https.issued by system:acme with trigger env and nobody who started it", e)
		}
		if a.sink.indexOf(audit.ActionHTTPSAccountRegistered) > a.sink.indexOf(audit.ActionHTTPSIssued) {
			t.Error("the account row comes after the run's row, want the registration first")
		}
		if runs := a.runs(); len(runs) != 1 || runs[0].Trigger != TriggerSchedule {
			t.Errorf("runs = %+v, want one run stored with the schedule trigger", runs)
		}
		if n := len(a.sink.all()); n != 2 {
			t.Errorf("the sink holds %d rows, want the account and the certificate: %+v", n, a.sink.all())
		}
	})

	t.Run("a start with an account registered writes no account row and the run still says env", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		a.buildWithEnv(a.inst, onEnv()).round(ctx)
		if got := a.awaitRun(); got != OutcomeSucceeded {
			t.Fatalf("the start-time run ended %q, want succeeded", got)
		}
		if rows := a.sink.byAction(audit.ActionHTTPSAccountRegistered); len(rows) != 0 {
			t.Errorf("%d account rows for an account that was already registered: %+v", len(rows), rows)
		}
		if e := a.onlyOutcome(); e.Details["trigger"] != "env" {
			t.Errorf("trigger = %q, want env", e.Details["trigger"])
		}
	})

	t.Run("a start-time run that fails says env in its failure row too", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		a.failOrders(1, refusal)
		a.buildWithEnv(a.inst, onEnv()).round(ctx)
		if got := a.awaitRun(); got != OutcomeFailed {
			t.Fatalf("the start-time run ended %q, want failed", got)
		}
		e := a.onlyOutcome()
		if e.Action != audit.ActionHTTPSRunFailed || e.Actor != audit.SystemACME || e.Details["trigger"] != "env" {
			t.Errorf("row = %+v, want https.run_failed by system:acme with trigger env", e)
		}
	})

	t.Run("an admin issue on a process that has the variable set still says admin", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		m := a.buildWithEnv(a.inst, onEnv())
		rs, err := m.Issue(ctx, "user:1")
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		a.wait(rs)
		a.awaitRun()
		e := a.onlyOutcome()
		if e.Details["trigger"] != TriggerAdmin || e.Details["started_by"] != "user:1" {
			t.Errorf("details = %+v, want trigger admin started by user:1", e.Details)
		}
	})

	t.Run("a failed registration writes no account row", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		a.noAccount()
		env := onEnv()
		env.AcceptTOS = false // the CA's terms are not accepted, so no account can be registered
		a.buildWithEnv(a.inst, env).round(ctx)
		if got := a.awaitRun(); got != OutcomeFailed {
			t.Fatalf("the start-time run ended %q, want failed at the account step", got)
		}
		if rows := a.sink.byAction(audit.ActionHTTPSAccountRegistered); len(rows) != 0 {
			t.Errorf("%d account rows for a registration that was refused: %+v", len(rows), rows)
		}
	})
}

// fallbackRows are the two kinds of row the fallback to self-signed writes, in order.
func (k *auditSink) fallbackRows() []audit.Entry {
	return k.byAction(audit.ActionHTTPSFallbackStarted, audit.ActionHTTPSFallbackEnded)
}

// requireFallbackRows fails the test unless the sink holds exactly the wanted fallback actions, in order.
func requireFallbackRows(t *testing.T, what string, a *auditEnv, want ...string) {
	t.Helper()
	var got []string
	for _, e := range a.sink.fallbackRows() {
		got = append(got, e.Action)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s: fallback rows = %v, want %v", what, got, want)
	}
}

// lapse issues the first certificate and moves the clock to the end of it, so the certificate is expired.
func (a *auditEnv) lapse() *x509.Certificate {
	a.t.Helper()
	a.issueFirst()
	first := leafOf(a.t, a.inst.chains[0])
	a.advanceTo(first.NotAfter)
	return first
}

// requireStarted checks the one https.fallback_started row about leaf: written by the system, with the instant the
// certificate expired and its names.
func requireStarted(t *testing.T, e audit.Entry, leaf *x509.Certificate) {
	t.Helper()
	if e.Action != audit.ActionHTTPSFallbackStarted || e.Actor != audit.SystemACME || e.Result != audit.ResultSuccess ||
		e.TargetType != "https" || e.TargetID != "https" {
		t.Errorf("row = %+v, want https.fallback_started, a success by system:acme about https", e)
	}
	want := map[string]string{"expired_at": leaf.NotAfter.UTC().Format(time.RFC3339), "names": strings.Join(leaf.DNSNames, ",")}
	if len(e.Details) != len(want) {
		t.Errorf("details = %+v, want only %v", e.Details, want)
	}
	for k, v := range want {
		if e.Details[k] != v {
			t.Errorf("details[%q] = %q, want %q", k, e.Details[k], v)
		}
	}
}

// The fallback to self-signed at expiry, and the return from it, are the system's doing and are written once for each
// lapse, however many scheduler rounds and restarts it spans (SET-04, D-09, D-10, T-17-32, T-17-35).
func TestFallbackAudit(t *testing.T) {
	ctx := context.Background()

	t.Run("the swap at expiry writes one row and the renewal after it writes the end", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		a.issueFirst()
		first := leafOf(t, a.inst.chains[0])
		a.failOrders(2, refusal)
		a.schedule(a.mgr)

		a.stepTo(a.state().RenewAt)
		if got := a.awaitRun(); got != OutcomeFailed {
			t.Fatalf("the first renewal ended %q, want failed", got)
		}
		requireFallbackRows(t, "before expiry", a)

		a.stepTo(first.NotAfter)
		if got := a.awaitRun(); got != OutcomeFailed {
			t.Fatalf("the renewal at expiry ended %q, want failed", got)
		}
		requireFallbackRows(t, "at expiry", a, audit.ActionHTTPSFallbackStarted)
		requireStarted(t, a.sink.fallbackRows()[0], first)
		if got, err := a.store.FallbackNoted(ctx); err != nil || got != first.SerialNumber.Text(16) {
			t.Errorf("FallbackNoted = %q (%v), want the expired certificate's serial %q", got, err, first.SerialNumber.Text(16))
		}

		// More rounds of the lapse write nothing; the renewal that works writes the end.
		a.step(Tick)
		a.step(Tick)
		requireFallbackRows(t, "two rounds into the lapse", a, audit.ActionHTTPSFallbackStarted)
		a.step(25 * time.Hour)
		if got := a.awaitRun(); got != OutcomeSucceeded {
			t.Fatalf("the renewal after expiry ended %q, want succeeded", got)
		}
		requireFallbackRows(t, "after the renewal", a, audit.ActionHTTPSFallbackStarted, audit.ActionHTTPSFallbackEnded)
		renewed := leafOf(t, a.inst.chains[len(a.inst.chains)-1])
		end := a.sink.fallbackRows()[1]
		if end.Actor != audit.SystemACME || end.Result != audit.ResultSuccess || end.TargetType != "https" {
			t.Errorf("row = %+v, want https.fallback_ended, a success by system:acme about https", end)
		}
		if end.Details["names"] != strings.Join(renewed.DNSNames, ",") || end.Details["not_after"] != renewed.NotAfter.UTC().Format(time.RFC3339) || len(end.Details) != 2 {
			t.Errorf("details = %+v, want the new certificate's names and not_after", end.Details)
		}
		// The end row comes before the renewal's own row and before the run is told to have ended.
		if a.sink.indexOf(audit.ActionHTTPSFallbackEnded) > a.sink.indexOf(audit.ActionHTTPSRenewed) {
			t.Error("the end of the fallback comes after the renewal's row, want it first")
		}
		a.step(Tick)
		requireFallbackRows(t, "a round after the return", a, audit.ActionHTTPSFallbackStarted, audit.ActionHTTPSFallbackEnded)
	})

	t.Run("a process started over an expired certificate writes the lapse once", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		first := a.lapse()

		inst := &recInstaller{}
		m := a.build(inst)
		if err := m.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		requireFallbackRows(t, "at start", a)
		m.syncServed(ctx)
		requireFallbackRows(t, "first round", a, audit.ActionHTTPSFallbackStarted)
		requireStarted(t, a.sink.fallbackRows()[0], first)
		if got, err := a.store.FallbackNoted(ctx); err != nil || got != first.SerialNumber.Text(16) {
			t.Errorf("FallbackNoted = %q (%v), want %q", got, err, first.SerialNumber.Text(16))
		}
		m.syncServed(ctx)
		m.syncServed(ctx)
		requireFallbackRows(t, "later rounds", a, audit.ActionHTTPSFallbackStarted)
		if inst.selfSignedCount() != 0 || inst.served() != 0 {
			t.Errorf("the start-up round swapped the certificate (self-signed %d, Let's Encrypt %d); self-signed was served already",
				inst.selfSignedCount(), inst.served())
		}
		if on, at := a.fallbackOf(m); !on || !at.Equal(first.NotAfter) {
			t.Errorf("Status.Fallback = %v at %v, want true at %v", on, at, first.NotAfter)
		}
	})

	t.Run("a restart while still lapsed writes no second row", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		a.lapse()
		for i := range 3 {
			m := a.build(&recInstaller{})
			if err := m.Start(ctx); err != nil {
				t.Fatalf("Start %d: %v", i, err)
			}
			m.syncServed(ctx)
			m.syncServed(ctx)
		}
		requireFallbackRows(t, "three process starts over one lapse", a, audit.ActionHTTPSFallbackStarted)
	})

	t.Run("a swap in process and then a restart writes the lapse once", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		first := a.lapse()
		a.mgr.syncServed(ctx)
		a.mgr.syncServed(ctx)
		requireFallbackRows(t, "the swap and a second round", a, audit.ActionHTTPSFallbackStarted)
		requireStarted(t, a.sink.fallbackRows()[0], first)
		if n := a.inst.selfSignedCount(); n != 1 {
			t.Errorf("self-signed was served %d times, want once", n)
		}

		m := a.build(&recInstaller{})
		if err := m.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		m.syncServed(ctx)
		requireFallbackRows(t, "the restart", a, audit.ActionHTTPSFallbackStarted)
	})

	t.Run("the return after a start-up lapse writes its one end row, and a later lapse has its own row", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		first := a.lapse()
		a.mgr.syncServed(ctx) // the lapse is in the log, written by the process that is replaced below

		inst := &recInstaller{}
		m := a.build(inst)
		if err := m.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		m.round(ctx) // notes the lapse without a second row, then renews: the clock is past renew_at
		if got := a.awaitRun(); got != OutcomeSucceeded {
			t.Fatalf("the renewal ended %q, want succeeded", got)
		}
		requireFallbackRows(t, "after the return", a, audit.ActionHTTPSFallbackStarted, audit.ActionHTTPSFallbackEnded)
		renewed := leafOf(t, inst.chains[0])
		if renewed.SerialNumber.Cmp(first.SerialNumber) == 0 {
			t.Fatal("the renewal served the expired certificate again")
		}
		if end := a.sink.fallbackRows()[1]; end.Details["not_after"] != renewed.NotAfter.UTC().Format(time.RFC3339) {
			t.Errorf("details = %+v, want the renewed certificate's not_after", end.Details)
		}
		m.syncServed(ctx)
		requireFallbackRows(t, "a round after the return", a, audit.ActionHTTPSFallbackStarted, audit.ActionHTTPSFallbackEnded)

		// The renewed certificate lapses in its turn: a different serial is a lapse of its own.
		a.advanceTo(renewed.NotAfter)
		m.syncServed(ctx)
		requireFallbackRows(t, "the second lapse", a, audit.ActionHTTPSFallbackStarted, audit.ActionHTTPSFallbackEnded, audit.ActionHTTPSFallbackStarted)
		requireStarted(t, a.sink.fallbackRows()[2], renewed)
		if got, err := a.store.FallbackNoted(ctx); err != nil || got != renewed.SerialNumber.Text(16) {
			t.Errorf("FallbackNoted = %q (%v), want the second certificate's serial %q", got, err, renewed.SerialNumber.Text(16))
		}
	})

	t.Run("a valid certificate the Installer refused is no fallback", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		a.issueFirst()
		m := a.build(&recInstaller{serveErr: errTestInstall})
		if err := m.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		m.syncServed(ctx)
		requireFallbackRows(t, "a refused install of a valid certificate", a)
		if got, err := a.store.FallbackNoted(ctx); err != nil || got != "" {
			t.Errorf("FallbackNoted = %q (%v), want nothing noted", got, err)
		}
	})

	t.Run("a fallback is written as the system whoever started the process", func(t *testing.T) {
		a := newAuditEnv(t, acmetest.Options{})
		a.lapse()
		m := a.buildWithEnv(&recInstaller{}, onEnv())
		if err := m.Start(ctx); err != nil {
			t.Fatalf("Start: %v", err)
		}
		m.syncServed(ctx)
		rows := a.sink.fallbackRows()
		if len(rows) != 1 || rows[0].Actor != audit.SystemACME {
			t.Fatalf("rows = %+v, want one by system:acme", rows)
		}
	})
}

func TestFallbackNoted(t *testing.T) {
	ctx := context.Background()
	s := newSchedEnv(t, acmetest.Options{})

	if got, err := s.store.FallbackNoted(ctx); err != nil || got != "" {
		t.Fatalf("FallbackNoted on a new store = %q (%v), want empty", got, err)
	}
	if err := s.store.NoteFallback(ctx, "0a1b", s.clock.Now()); err != nil {
		t.Fatalf("NoteFallback: %v", err)
	}
	if got, err := s.store.FallbackNoted(ctx); err != nil || got != "0a1b" {
		t.Errorf("FallbackNoted = %q (%v), want 0a1b", got, err)
	}
	if err := s.store.NoteFallback(ctx, "ff02", s.clock.Now().Add(time.Hour)); err != nil {
		t.Fatalf("NoteFallback again: %v", err)
	}
	if got, err := s.store.FallbackNoted(ctx); err != nil || got != "ff02" {
		t.Errorf("FallbackNoted after a second note = %q (%v), want ff02", got, err)
	}
}

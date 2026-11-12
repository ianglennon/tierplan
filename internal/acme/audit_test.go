// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/audit"
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
func (a *auditEnv) build(inst Installer) *Manager {
	return NewManager(Deps{
		Store: a.store, Client: a.client, Challenges: a.tokens, Jobs: a.hub, Installer: inst, Clock: a.clock,
		PublicURL: a.urls.getPublic, AgentURL: a.urls.getAgent, BasePath: a.urls.getBase,
		Log:    slog.New(slog.DiscardHandler),
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

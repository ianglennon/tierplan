// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-acme/lego/v5/acme/api"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

var errTestInstall = errors.New("the Installer refused the certificate")

// schedEnv is a manager whose scheduler a test drives with the fake clock: every round is one
// Advance, and the manager's RunEnded seam tells the test when a run it started has finished.
type schedEnv struct {
	*mgrEnv
	ended chan string
	// picks counts the draws the scheduler made inside an ARI window.
	picks atomic.Int32
}

func newSchedEnv(t *testing.T, opts acmetest.Options) *schedEnv {
	t.Helper()
	oe := newOrderEnvWith(t, opts)
	hub := jobs.New(jobs.Options{Now: oe.clock.Now})
	t.Cleanup(hub.Close)
	s := &schedEnv{
		mgrEnv: &mgrEnv{orderEnv: oe, hub: hub, inst: &recInstaller{}, urls: &urlSet{public: "https://" + testHost}},
		ended:  make(chan string, 64),
	}
	s.mgr = s.build(hub, s.inst, false)
	return s
}

// build is a Manager over the environment's stored state, the way a restarted process builds one. Its
// draw inside an ARI window is the middle of the window.
func (s *schedEnv) build(hub *jobs.Hub, inst Installer, certFiles bool) *Manager {
	return NewManager(Deps{
		Store: s.store, Client: s.client, Challenges: s.tokens, Jobs: hub, Installer: inst, Clock: s.clock,
		PublicURL: s.urls.getPublic, AgentURL: s.urls.getAgent, BasePath: s.urls.getBase,
		CertFiles: certFiles, Log: slog.New(slog.DiscardHandler),
		Jitter:   func() float64 { return 0 },
		Rand:     func(n int64) int64 { s.picks.Add(1); return n / 2 },
		RunEnded: func(_, outcome string) { s.ended <- outcome },
	})
}

// schedule runs the scheduler of m until the test ends, and returns once its first round is done.
func (s *schedEnv) schedule(m *Manager) (stop func()) {
	s.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	s.clock.WaitArmed(s.t)
	stop = func() {
		cancel()
		testsupport.Await(s.t, "the scheduler to stop", done)
	}
	s.t.Cleanup(stop)
	return stop
}

// step moves the clock on by d, which ends the scheduler's wait, and returns once the round that
// follows is done.
func (s *schedEnv) step(d time.Duration) {
	s.t.Helper()
	s.clock.Advance(d)
	s.clock.WaitArmed(s.t)
}

// idle moves the clock on by d, a Tick at a time, and checks after every round that none started a run
// beyond the wantRuns the history holds.
func (s *schedEnv) idle(d time.Duration, wantRuns int) {
	s.t.Helper()
	for left := d; left > 0; left -= Tick {
		s.step(min(left, Tick))
		s.quiet(wantRuns)
	}
}

// stepTo is step to the instant at.
func (s *schedEnv) stepTo(at time.Time) {
	s.t.Helper()
	s.step(at.Sub(s.clock.Now()))
}

// issueFirst issues the first certificate as an admin would, and leaves the run's end behind it.
func (s *schedEnv) issueFirst() {
	s.t.Helper()
	s.issue()
	if got := s.awaitRun(); got != OutcomeSucceeded {
		s.t.Fatalf("the first issue ended %q, want succeeded", got)
	}
}

// awaitRun waits for the run a round started to end, and says how it ended.
func (s *schedEnv) awaitRun() string {
	s.t.Helper()
	return testsupport.Await(s.t, "the scheduled run to end", s.ended)
}

func TestSchedulerRenewsAtTheARIInstantWithReplaces(t *testing.T) {
	s := newSchedEnv(t, acmetest.Options{})
	s.issueFirst()
	first := leafOf(t, s.inst.chains[0])
	firstID, err := api.MakeARICertID(first)
	if err != nil {
		t.Fatalf("MakeARICertID: %v", err)
	}
	start := testNow.Add(30 * 24 * time.Hour)
	end := start.Add(10 * 24 * time.Hour)
	s.fake.SetRenewalWindow(first, start, end, 6*time.Hour)

	s.schedule(s.mgr)

	// The first round asked the CA and picked an instant inside the window.
	if n := s.fake.Count(acmetest.EndpointRenewalInfo); n != 1 {
		t.Fatalf("renewalInfo was asked %d times in the first round, want 1", n)
	}
	st := s.state()
	picked := start.Add(end.Sub(start) / 2)
	if !st.RenewAt.Equal(picked) || st.RenewBasis != string(BasisARI) {
		t.Errorf("renew_at = %v (%s), want %v (%s)", st.RenewAt, st.RenewBasis, picked, BasisARI)
	}
	if !st.AriWindowStart.Equal(start) || !st.AriWindowEnd.Equal(end) || !st.AriNextCheckAt.Equal(testNow.Add(6*time.Hour)) {
		t.Errorf("ARI window %v to %v, next check %v; want %v to %v, next check %v",
			st.AriWindowStart, st.AriWindowEnd, st.AriNextCheckAt, start, end, testNow.Add(6*time.Hour))
	}
	if n := s.fake.Count(acmetest.EndpointNewOrder); n != 1 {
		t.Fatalf("%d orders before renew_at, want only the first issue", n)
	}

	// The clock reaches renew_at: the manager renews with nobody pressing anything.
	s.stepTo(st.RenewAt)
	if got := s.awaitRun(); got != OutcomeSucceeded {
		t.Fatalf("the scheduled run ended %q, want succeeded", got)
	}

	orders := s.fake.Orders()
	if len(orders) != 2 {
		t.Fatalf("the CA saw %d orders, want the issue and the renewal", len(orders))
	}
	if !slices.Equal(orders[1].Names, first.DNSNames) || orders[1].Replaces != firstID {
		t.Errorf("the renewal ordered %v replacing %q, want %v replacing %q",
			orders[1].Names, orders[1].Replaces, first.DNSNames, firstID)
	}
	if s.inst.served() != 2 {
		t.Fatalf("ServeACME was called %d times, want the issue and the renewal", s.inst.served())
	}
	if leafOf(t, s.inst.chains[1]).Equal(first) {
		t.Error("the renewal served the old certificate again")
	}
	runs := s.runs()
	if len(runs) != 2 || runs[0].Kind != KindRenew || runs[0].Trigger != TriggerSchedule || runs[0].Actor != "" ||
		runs[0].Outcome != OutcomeSucceeded {
		t.Errorf("runs = %+v, want a succeeded renewal started by the schedule, with no actor", runs)
	}
	if st := s.state(); st.ConsecutiveFailures != 0 || st.LastSuccessKind != KindRenew {
		t.Errorf("state = %+v, want a renewal as the last success and no failures", st)
	}
}

// quiet fails the test if a round started a run: none is active, none ended, and the history holds
// exactly the runs the test has seen end.
func (s *schedEnv) quiet(wantRuns int) {
	s.t.Helper()
	if s.mgr.runActive() {
		s.t.Error("a run is in progress, want the round to start none")
	}
	if n := len(s.ended); n != 0 {
		s.t.Errorf("%d runs ended that the test did not expect", n)
	}
	if got := len(s.runs()); got != wantRuns {
		s.t.Errorf("the run history holds %d runs, want %d", got, wantRuns)
	}
}

// failOrders makes the next n orders the CA is asked for fail with a refusal that does not retry.
func (s *schedEnv) failOrders(n int, p acmetest.Problem) {
	for range n {
		s.fake.FailNext(acmetest.EndpointNewOrder, p)
	}
}

var refusal = acmetest.Problem{Status: 400, Type: "malformed", Detail: "scripted refusal"}

func TestSchedulerFallsBackToTwoThirdsOrHalf(t *testing.T) {
	ctx := context.Background()
	lifetimeOf := func(chain []byte, t *testing.T) (time.Time, time.Duration) {
		leaf := leafOf(t, chain)
		return leaf.NotBefore, leaf.NotAfter.Sub(leaf.NotBefore)
	}
	// forget wipes the instant issuing stored, so what the test reads is the scheduler's own work.
	forget := func(s *schedEnv) {
		st := s.state()
		st.RenewAt, st.RenewBasis, st.AriNextCheckAt = time.Time{}, "", time.Time{}
		if err := s.store.SaveState(ctx, st); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
	}

	t.Run("the CA offers no renewal information", func(t *testing.T) {
		s := newSchedEnv(t, acmetest.Options{NoRenewalInfo: true})
		s.issueFirst()
		forget(s)
		s.schedule(s.mgr)
		from, life := lifetimeOf(s.inst.chains[0], t)
		st := s.state()
		if want := from.Add(life * 2 / 3); !st.RenewAt.Equal(want) || st.RenewBasis != string(BasisTwoThirds) {
			t.Errorf("renew_at = %v (%s), want %v (%s)", st.RenewAt, st.RenewBasis, want, BasisTwoThirds)
		}
		if !st.AriNextCheckAt.Equal(testNow.Add(6 * time.Hour)) {
			t.Errorf("next ARI check = %v, want six hours on", st.AriNextCheckAt)
		}
		if n := s.fake.Count(acmetest.EndpointRenewalInfo); n != 0 {
			t.Errorf("renewalInfo was asked %d times of a CA that does not offer it", n)
		}
	})

	t.Run("the CA answers with a window that cannot be used", func(t *testing.T) {
		s := newSchedEnv(t, acmetest.Options{})
		s.issueFirst()
		forget(s)
		leaf := leafOf(t, s.inst.chains[0])
		s.fake.SetRenewalWindow(leaf, testNow.Add(time.Hour), testNow.Add(time.Hour), time.Hour) // end == start
		s.schedule(s.mgr)
		from, life := lifetimeOf(s.inst.chains[0], t)
		st := s.state()
		if want := from.Add(life * 2 / 3); !st.RenewAt.Equal(want) || st.RenewBasis != string(BasisTwoThirds) {
			t.Errorf("renew_at = %v (%s), want %v (%s)", st.RenewAt, st.RenewBasis, want, BasisTwoThirds)
		}
		if n := s.fake.Count(acmetest.EndpointRenewalInfo); n != 1 {
			t.Errorf("renewalInfo was asked %d times, want 1", n)
		}
	})

	t.Run("a short-lived certificate renews at half its life", func(t *testing.T) {
		s := newSchedEnv(t, acmetest.Options{NoRenewalInfo: true})
		if err := s.mgr.SetProfile(ctx, "shortlived"); err != nil {
			t.Fatalf("SetProfile: %v", err)
		}
		s.issueFirst()
		forget(s)
		s.schedule(s.mgr)
		from, life := lifetimeOf(s.inst.chains[0], t)
		if life >= ShortLifetime {
			t.Fatalf("the certificate lives %v, want a short-lived one", life)
		}
		st := s.state()
		if want := from.Add(life / 2); !st.RenewAt.Equal(want) || st.RenewBasis != string(BasisHalf) {
			t.Errorf("renew_at = %v (%s), want %v (%s)", st.RenewAt, st.RenewBasis, want, BasisHalf)
		}
	})
}

func TestSchedulerKeepsTheARIInstant(t *testing.T) {
	s := newSchedEnv(t, acmetest.Options{})
	s.issueFirst()
	leaf := leafOf(t, s.inst.chains[0])
	day := 24 * time.Hour
	start, end := testNow.Add(30*day), testNow.Add(40*day)
	s.fake.SetRenewalWindow(leaf, start, end, 6*time.Hour)
	s.schedule(s.mgr)

	picked := start.Add(end.Sub(start) / 2)
	if st := s.state(); !st.RenewAt.Equal(picked) || s.picks.Load() != 1 {
		t.Fatalf("renew_at = %v after %d draws, want %v after one", st.RenewAt, s.picks.Load(), picked)
	}

	// The CA moves the window but it still holds the instant: nothing is drawn again.
	s.fake.SetRenewalWindow(leaf, start.Add(-day), end.Add(day), 6*time.Hour)
	s.stepTo(s.state().AriNextCheckAt)
	if st := s.state(); !st.RenewAt.Equal(picked) || s.picks.Load() != 1 {
		t.Errorf("renew_at = %v after %d draws, want %v kept with the one draw", st.RenewAt, s.picks.Load(), picked)
	}
	if n := s.fake.Count(acmetest.EndpointRenewalInfo); n != 2 {
		t.Errorf("renewalInfo was asked %d times, want 2 (one a check)", n)
	}

	// A window that no longer holds it makes the scheduler draw again, inside the new window.
	newStart, newEnd := end.Add(5*day), end.Add(9*day)
	s.fake.SetRenewalWindow(leaf, newStart, newEnd, 6*time.Hour)
	s.stepTo(s.state().AriNextCheckAt)
	if want, st := newStart.Add(newEnd.Sub(newStart)/2), s.state(); !st.RenewAt.Equal(want) || s.picks.Load() != 2 {
		t.Errorf("renew_at = %v after %d draws, want %v after two", st.RenewAt, s.picks.Load(), want)
	}
	s.quiet(1)
}

func TestSchedulerARIFailureIsNotARenewalFailure(t *testing.T) {
	s := newSchedEnv(t, acmetest.Options{})
	s.issueFirst()
	issuedRenewAt := s.state().RenewAt
	s.fake.FailNext(acmetest.EndpointRenewalInfo, acmetest.Problem{Status: 500, Type: "serverInternal", Detail: "scripted"})
	s.schedule(s.mgr)

	st := s.state()
	if !st.RenewAt.Equal(issuedRenewAt) || st.ConsecutiveFailures != 0 || !st.NextAttemptAt.IsZero() || st.LastErrorClass != "" {
		t.Errorf("state = %+v, want renew_at %v kept and nothing counted as a failure", st, issuedRenewAt)
	}
	if want := testNow.Add(time.Hour); !st.AriNextCheckAt.Equal(want) {
		t.Errorf("next ARI check = %v, want %v, an hour on", st.AriNextCheckAt, want)
	}

	s.stepTo(st.AriNextCheckAt)
	if st := s.state(); st.RenewBasis != string(BasisARI) || st.ConsecutiveFailures != 0 {
		t.Errorf("state = %+v, want the ARI instant once the CA answers", st)
	}
	s.quiet(1)
}

func TestSchedulerRetriesOnTheBackoffSchedule(t *testing.T) {
	s := newSchedEnv(t, acmetest.Options{})
	s.issueFirst()
	leaf := leafOf(t, s.inst.chains[0])
	s.fake.FailValidation(testHost, acmetest.Problem{Status: 403, Type: "unauthorized", Detail: "scripted"})
	s.schedule(s.mgr)
	served := s.inst.served()
	chainBefore := string(s.inst.chains[0])

	s.stepTo(s.state().RenewAt)
	if got := s.awaitRun(); got != OutcomeFailed {
		t.Fatalf("the first scheduled run ended %q, want failed", got)
	}
	now := s.clock.Now()
	if st := s.state(); st.ConsecutiveFailures != 1 || !st.NextAttemptAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("after one failure: %d failures, next attempt %v; want 1 and %v", st.ConsecutiveFailures, st.NextAttemptAt, now.Add(time.Minute))
	}
	if s.inst.served() != served || string(s.inst.chains[0]) != chainBefore {
		t.Error("a failed renewal changed the certificate being served")
	}

	// The wait is a minute: the very next round attempts again, and then the wait grows to ten minutes.
	s.step(Tick)
	if got := s.awaitRun(); got != OutcomeFailed {
		t.Fatalf("the second scheduled run ended %q, want failed", got)
	}
	now = s.clock.Now()
	if st := s.state(); st.ConsecutiveFailures != 2 || !st.NextAttemptAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("after two failures: %d, next attempt %v; want 2 and %v", st.ConsecutiveFailures, st.NextAttemptAt, now.Add(10*time.Minute))
	}
	s.idle(9*time.Minute, 3)
	s.step(Tick)
	if got := s.awaitRun(); got != OutcomeFailed {
		t.Fatalf("the third scheduled run ended %q, want failed", got)
	}
	now = s.clock.Now()
	if st := s.state(); st.ConsecutiveFailures != 3 || !st.NextAttemptAt.Equal(now.Add(100*time.Minute)) {
		t.Fatalf("after three failures: %d, next attempt %v; want 3 and %v", st.ConsecutiveFailures, st.NextAttemptAt, now.Add(100*time.Minute))
	}
	if s.inst.served() != served || !leafOf(t, s.inst.chains[0]).Equal(leaf) {
		t.Error("failed renewals changed the certificate being served")
	}
}

func TestSchedulerHonoursRetryAfter(t *testing.T) {
	s := newSchedEnv(t, acmetest.Options{})
	s.issueFirst()
	s.fake.FailNext(acmetest.EndpointNewOrder, acmetest.Problem{
		Status: 429, Type: "rateLimited", Detail: "scripted", RetryAfterSeconds: 7200,
	})
	s.schedule(s.mgr)

	s.stepTo(s.state().RenewAt)
	if got := s.awaitRun(); got != OutcomeFailed {
		t.Fatalf("the rate-limited run ended %q, want failed", got)
	}
	failedAt := s.clock.Now()
	st := s.state()
	if !st.RetryAfterUntil.Equal(failedAt.Add(2*time.Hour)) || !st.NextAttemptAt.Equal(failedAt.Add(time.Minute)) {
		t.Fatalf("retry-after until %v, next attempt %v; want %v and %v",
			st.RetryAfterUntil, st.NextAttemptAt, failedAt.Add(2*time.Hour), failedAt.Add(time.Minute))
	}
	orders := s.fake.Count(acmetest.EndpointNewOrder)

	// Our own backoff is long over, the CA's word is not.
	s.stepTo(failedAt.Add(2*time.Hour - time.Second))
	s.quiet(2)
	if n := s.fake.Count(acmetest.EndpointNewOrder); n != orders {
		t.Errorf("the CA saw %d new orders before its Retry-After passed, want %d", n, orders)
	}
	s.step(Tick)
	if got := s.awaitRun(); got != OutcomeSucceeded {
		t.Fatalf("the run after Retry-After ended %q, want succeeded", got)
	}
}

func TestSchedulerLedgerSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	s := newSchedEnv(t, acmetest.Options{})
	s.issueFirst()
	s.failOrders(1, refusal)
	stop := s.schedule(s.mgr)
	s.stepTo(s.state().RenewAt)
	if got := s.awaitRun(); got != OutcomeFailed {
		t.Fatalf("the run ended %q, want failed", got)
	}
	before := s.state()
	if before.ConsecutiveFailures != 1 || before.NextAttemptAt.IsZero() {
		t.Fatalf("state = %+v, want one failure and a next attempt", before)
	}

	// The process restarts: a new hub and a new manager over the same database.
	stop()
	s.hub.Close()
	hub := jobs.New(jobs.Options{Now: s.clock.Now})
	t.Cleanup(hub.Close)
	restarted := s.build(hub, s.inst, false)
	s.mgr = restarted
	if err := restarted.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.schedule(restarted)

	after := s.state()
	if after.ConsecutiveFailures != before.ConsecutiveFailures || !after.NextAttemptAt.Equal(before.NextAttemptAt) ||
		!after.RenewAt.Equal(before.RenewAt) || after.RenewBasis != before.RenewBasis {
		t.Errorf("the restart changed the ledger: before %+v, after %+v", before, after)
	}
	s.quiet(2)
	s.step(Tick) // the attempt is a minute after the failure
	if got := s.awaitRun(); got != OutcomeSucceeded {
		t.Fatalf("the retry after the restart ended %q, want succeeded", got)
	}
}

func TestSchedulerResumesAnInterruptedOrder(t *testing.T) {
	ctx := context.Background()
	s := newSchedEnv(t, acmetest.Options{})
	s.issueFirst()
	first := leafOf(t, s.inst.chains[0])
	release := s.fake.HoldValidation()
	stop := s.schedule(s.mgr)

	// A scheduled renewal gets as far as an order at the CA, waiting on its validation.
	s.stepTo(s.state().RenewAt)
	s.clock.WaitArmed(t) // the run waiting on the CA
	orders := s.fake.Count(acmetest.EndpointNewOrder)
	if orders != 2 {
		t.Fatalf("the CA saw %d orders, want the issue and the renewal", orders)
	}

	// The process stops with the run in the middle: the hub cancels it, as a shutdown does.
	stop()
	s.hub.Close()
	if got := s.awaitRun(); got != OutcomeFailed {
		t.Fatalf("the cut-short run ended %q, want failed", got)
	}

	hub := jobs.New(jobs.Options{Now: s.clock.Now})
	t.Cleanup(hub.Close)
	restarted := s.build(hub, s.inst, false)
	s.mgr = restarted
	if err := restarted.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.schedule(restarted)
	// The first round picked the order up and the run is waiting on the CA again. Now the CA validates.
	s.clock.WaitArmed(t)
	release()
	s.clock.Advance(PollInterval)

	if got := s.awaitRun(); got != OutcomeSucceeded {
		t.Fatalf("the resumed run ended %q, want succeeded (state %+v)", got, s.state())
	}
	if n := s.fake.Count(acmetest.EndpointNewOrder); n != orders {
		t.Errorf("the CA saw %d orders, want the interrupted one picked up, not %d", n, orders)
	}
	runs := s.runs()
	if len(runs) < 1 || runs[0].Trigger != TriggerResume || runs[0].Kind != KindRenew || runs[0].Outcome != OutcomeSucceeded {
		t.Errorf("runs = %+v, want the newest to be a succeeded resume of the renewal", runs)
	}
	if last := leafOf(t, s.inst.chains[len(s.inst.chains)-1]); last.Equal(first) {
		t.Error("the resumed order did not end in a new certificate being served")
	}
	if _, _, open, err := s.store.OpenOrder(ctx); err != nil || open {
		t.Errorf("OpenOrder = open %v, %v; want the order closed", open, err)
	}
}

func TestSchedulerFallsBackToSelfSignedAtExpiry(t *testing.T) {
	ctx := context.Background()
	s := newSchedEnv(t, acmetest.Options{})
	s.issueFirst()
	first := leafOf(t, s.inst.chains[0])
	s.failOrders(2, refusal)
	s.schedule(s.mgr)

	s.stepTo(s.state().RenewAt)
	if got := s.awaitRun(); got != OutcomeFailed {
		t.Fatalf("the first renewal ended %q, want failed", got)
	}
	if n := s.inst.selfSignedCount(); n != 0 {
		t.Fatalf("self-signed was served %d times before the certificate expired", n)
	}

	// The certificate reaches its end while renewals keep failing: the next round serves self-signed.
	s.stepTo(first.NotAfter)
	if got := s.awaitRun(); got != OutcomeFailed {
		t.Fatalf("the renewal at expiry ended %q, want failed (the second scripted refusal)", got)
	}
	if n := s.inst.selfSignedCount(); n != 1 {
		t.Errorf("self-signed was served %d times at expiry, want exactly once", n)
	}
	status, err := s.mgr.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Fallback || !status.ExpiredAt.Equal(first.NotAfter) {
		t.Errorf("Status.Fallback = %v, ExpiredAt = %v; want true at %v", status.Fallback, status.ExpiredAt, first.NotAfter)
	}
	if s.inst.served() != 1 {
		t.Errorf("ServeACME was called %d times while the certificate was expired, want only the issue", s.inst.served())
	}

	// Renewals carry on, and the next that works serves Let's Encrypt again.
	s.step(25 * time.Hour)
	if got := s.awaitRun(); got != OutcomeSucceeded {
		t.Fatalf("the renewal after expiry ended %q, want succeeded", got)
	}
	if n := s.inst.selfSignedCount(); n != 1 {
		t.Errorf("self-signed was served %d times in all, want still once", n)
	}
	if s.inst.served() != 2 || leafOf(t, s.inst.chains[1]).Equal(first) {
		t.Errorf("ServeACME was called %d times, want the renewed certificate served once more", s.inst.served())
	}
	if status, err := s.mgr.Status(ctx); err != nil || status.Fallback {
		t.Errorf("Status.Fallback = %v (%v), want false once Let's Encrypt is served again", status.Fallback, err)
	}
}

func TestSchedulerServesTheCertificateAgainAfterAFailedInstall(t *testing.T) {
	s := newSchedEnv(t, acmetest.Options{})
	s.inst.serveErr = errTestInstall
	s.issueFirst() // stored, but the Installer refused it
	if s.inst.served() != 0 {
		t.Fatalf("ServeACME succeeded %d times, want the Installer to have refused", s.inst.served())
	}
	s.inst.mu.Lock()
	s.inst.serveErr = nil
	s.inst.mu.Unlock()

	s.schedule(s.mgr)
	if s.inst.served() != 1 {
		t.Errorf("ServeACME succeeded %d times after the first round, want the stored certificate served", s.inst.served())
	}
	s.step(time.Minute)
	if s.inst.served() != 1 {
		t.Errorf("ServeACME succeeded %d times, want it asked for once", s.inst.served())
	}
}

func TestSchedulerIsSilentWhilePausedAfterRestore(t *testing.T) {
	ctx := context.Background()
	s := newSchedEnv(t, acmetest.Options{})
	s.issueFirst()
	first := leafOf(t, s.inst.chains[0])

	// What a restore leaves: paused, and an order the original instance had open.
	st := s.state()
	st.PausedReason = PausedRestore
	if err := s.store.SaveState(ctx, st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	key, err := newKey()
	if err != nil {
		t.Fatalf("newKey: %v", err)
	}
	open := Order{
		ID: "11111111-1111-7111-8111-111111111111", DirectoryURL: s.client.Directory().URL,
		URL: s.client.Directory().URL + "/order/1", Kind: KindRenew, Names: first.DNSNames,
		Stage: StageCreated, CreatedAt: testNow, UpdatedAt: testNow,
	}
	if err := s.store.InsertOrder(ctx, open, key); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	// Whatever the scheduler asked of the CA while the restore was being prepared is behind us.
	before := counts(s.mgrEnv)

	s.schedule(s.mgr)
	s.stepTo(st.RenewAt.Add(24 * time.Hour))
	s.stepTo(first.NotAfter.Add(-time.Hour))
	s.quiet(1)
	s.noCARequests(before)
	if n := s.inst.selfSignedCount(); n != 0 {
		t.Errorf("self-signed was served %d times before expiry", n)
	}

	// The certificate expires: serving self-signed is local, so it still happens, and still no request.
	s.stepTo(first.NotAfter)
	s.quiet(1)
	s.noCARequests(before)
	if n := s.inst.selfSignedCount(); n != 1 {
		t.Errorf("self-signed was served %d times at expiry, want once", n)
	}
	if status, err := s.mgr.Status(ctx); err != nil || !status.Fallback || status.PausedReason != PausedRestore {
		t.Errorf("Status = fallback %v, paused %q (%v); want a fallback and the pause", status.Fallback, status.PausedReason, err)
	}

	// An admin resumes, and the first round after that carries on.
	if err := s.store.CloseOrder(ctx, open.ID, StageFailed, s.clock.Now()); err != nil {
		t.Fatalf("CloseOrder: %v", err)
	}
	if err := s.mgr.Resume(ctx, "user:1"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	s.step(time.Minute)
	if got := s.awaitRun(); got != OutcomeSucceeded {
		t.Fatalf("the first run after Resume ended %q, want succeeded", got)
	}
	if s.inst.served() != 2 {
		t.Errorf("ServeACME was called %d times, want the renewed certificate served after Resume", s.inst.served())
	}
}

func TestSchedulerDoesNothingWithCertificateFiles(t *testing.T) {
	s := newSchedEnv(t, acmetest.Options{})
	s.issueFirst()
	first := leafOf(t, s.inst.chains[0])
	before := counts(s.mgrEnv)
	served := s.inst.served()

	files := s.build(s.hub, s.inst, true)
	s.schedule(files)
	s.stepTo(s.state().RenewAt.Add(time.Hour))
	s.stepTo(first.NotAfter.Add(time.Hour))
	s.quiet(1)
	s.noCARequests(before)
	if n := s.inst.selfSignedCount(); n != 0 || s.inst.served() != served {
		t.Errorf("the scheduler swapped certificates with files configured: self-signed %d, ServeACME %d (was %d)",
			n, s.inst.served(), served)
	}
}

// A-WR-01: with the account forgotten (the CA said it was gone), a due renewal is not started. Such a
// run could only fail before reaching the CA, and every failure would be charged to the ledger and
// count toward the manual-attempt guard that then refuses the admin's Issue after registering again.
func TestSchedulerDoesNotRenewWithoutAnAccount(t *testing.T) {
	ctx := context.Background()
	// Without renewal information, so the renewal names no certificate to replace: the CA refuses to
	// replace a certificate issued to the account that was forgotten.
	s := newSchedEnv(t, acmetest.Options{NoRenewalInfo: true})
	s.issueFirst()
	if err := s.client.Forget(ctx); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	orders := s.fake.Count(acmetest.EndpointNewOrder)
	s.schedule(s.mgr)

	s.stepTo(s.state().RenewAt)
	s.quiet(1)
	s.idle(15*time.Minute, 1)
	if st := s.state(); st.ConsecutiveFailures != 0 || !st.NextAttemptAt.IsZero() || st.LastErrorClass != "" {
		t.Errorf("state = %+v, want nothing charged to the ledger without an account", st)
	}
	if n := s.fake.Count(acmetest.EndpointNewOrder); n != orders {
		t.Errorf("the CA saw %d new orders, want %d", n, orders)
	}
	if err := s.mgr.manualBlock(ctx, s.state()); err != nil {
		t.Errorf("manualBlock = %v, want the admin's next attempt allowed", err)
	}

	// The admin registers again: the next round renews.
	if _, err := s.client.Register(ctx, "", true); err != nil {
		t.Fatalf("Register: %v", err)
	}
	s.step(Tick)
	if got := s.awaitRun(); got != OutcomeSucceeded {
		t.Fatalf("the renewal after registering again ended %q, want succeeded (state %+v)", got, s.state())
	}
}

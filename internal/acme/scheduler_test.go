// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
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

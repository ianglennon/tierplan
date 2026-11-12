// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/jobs"
)

// The TIERPLAN_ACME overrides (phase 16, SET-02, D-02, D-04, D-17, RESEARCH F3).

func envSwitch(on bool) config.ACMEEnv { return config.ACMEEnv{Enabled: &on} }

// buildEnv is build with the environment's TIERPLAN_ACME* values.
func (s *schedEnv) buildEnv(hub *jobs.Hub, inst Installer, env config.ACMEEnv) *Manager {
	return NewManager(Deps{
		Store: s.store, Client: s.client, Challenges: s.tokens, Jobs: hub, Installer: inst, Clock: s.clock,
		PublicURL: s.urls.getPublic, AgentURL: s.urls.getAgent, BasePath: s.urls.getBase,
		Log: slog.New(slog.DiscardHandler), Env: env,
		Jitter:   func() float64 { return 0 },
		Rand:     func(n int64) int64 { s.picks.Add(1); return n / 2 },
		RunEnded: func(_, outcome string) { s.ended <- outcome },
	})
}

// requireLocked fails the test unless err is the lock on Let's Encrypt.
func requireLocked(t *testing.T, what string, err error) {
	t.Helper()
	var le *ErrLocked
	if !errors.As(err, &le) {
		t.Fatalf("%s = %v, want *ErrLocked", what, err)
	}
	if le.Field != LockACME || le.Var != config.EnvACME || le.Problem != "acme_locked" {
		t.Errorf("%s = %+v, want field %q, var %q, problem acme_locked", what, le, LockACME, config.EnvACME)
	}
	if msg := le.Error(); !strings.Contains(msg, config.EnvACME) {
		t.Errorf("the error text %q does not name %s", msg, config.EnvACME)
	}
}

// With the variable off, a stored Let's Encrypt certificate is not served, the status says off and
// names the lock, and the stored mode is untouched; without the variable it is served again (D-02).
func TestEnvOffKeepsTheStoredStateButServesSelfSigned(t *testing.T) {
	ctx := context.Background()
	s := newSchedEnv(t, acmetest.Options{})
	s.issueFirst()

	inst := &recInstaller{}
	off := s.buildEnv(s.hub, inst, envSwitch(false))
	if err := off.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if inst.served() != 0 {
		t.Errorf("ServeACME was called %d times with TIERPLAN_ACME=off, want the stored certificate left alone", inst.served())
	}
	st, err := off.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != ModeOff {
		t.Errorf("Status.Mode = %q, want off", st.Mode)
	}
	if want := (&Lock{Var: config.EnvACME, Value: "off", Stored: "on"}); st.Locks.ACME == nil || *st.Locks.ACME != *want {
		t.Errorf("Status.Locks.ACME = %+v, want %+v", st.Locks.ACME, want)
	}
	if got := s.state().Mode; got != ModeActive {
		t.Errorf("the stored mode is %q, want it still active", got)
	}

	// The variable removed: the same stored certificate is served, and nothing is locked.
	back := &recInstaller{}
	unset := s.buildEnv(s.hub, back, config.ACMEEnv{})
	if err := unset.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if back.served() != 1 {
		t.Errorf("ServeACME was called %d times without the variable, want the stored certificate served", back.served())
	}
	st, err = unset.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != ModeActive || st.Locks.ACME != nil {
		t.Errorf("without the variable: mode %q, lock %+v; want active and no lock", st.Mode, st.Locks.ACME)
	}
}

// With the variable off, Issue and Renew now are refused by the manager itself and nothing is ordered.
func TestEnvOffRefusesIssueAndRenewNow(t *testing.T) {
	ctx := context.Background()
	s := newSchedEnv(t, acmetest.Options{})
	s.issueFirst()
	off := s.buildEnv(s.hub, &recInstaller{}, envSwitch(false))
	before := counts(s.mgrEnv)

	_, err := off.Issue(ctx, "user:1")
	requireLocked(t, "Issue", err)
	_, err = off.RenewNow(ctx, "user:1")
	requireLocked(t, "RenewNow", err)

	s.noCARequests(before)
	if n := len(s.runs()); n != 1 {
		t.Errorf("the run history holds %d runs, want only the first issue", n)
	}
}

// Either value of the variable fixes whether Let's Encrypt is stopped, so Stop is refused, and nothing
// stored changes.
func TestEnvRefusesStopWhateverTheValue(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "on", false: "off"}[on], func(t *testing.T) {
			ctx := context.Background()
			s := newSchedEnv(t, acmetest.Options{})
			s.issueFirst()
			inst := &recInstaller{}
			m := s.buildEnv(s.hub, inst, envSwitch(on))

			requireLocked(t, "Stop", m.Stop(ctx, "user:1"))

			if got := s.state().Mode; got != ModeActive {
				t.Errorf("the stored mode is %q after a refused Stop, want active", got)
			}
			if _, _, found, err := s.store.Certificate(ctx); err != nil || !found {
				t.Errorf("the stored certificate: found %v, err %v; want it kept", found, err)
			}
			if _, ok, err := s.client.Account(ctx); err != nil || !ok {
				t.Errorf("the account: found %v, err %v; want it kept", ok, err)
			}
			if inst.selfSigned != 0 {
				t.Errorf("the self-signed certificate was served %d times by a refused Stop", inst.selfSigned)
			}
		})
	}
}

// Issue stays allowed with the variable on: it is the manual retry (D-04).
func TestEnvOnAllowsIssue(t *testing.T) {
	ctx := context.Background()
	s := newSchedEnv(t, acmetest.Options{})
	on := s.buildEnv(s.hub, s.inst, envSwitch(true))

	rs, err := on.Issue(ctx, "user:1")
	if err != nil {
		t.Fatalf("Issue with TIERPLAN_ACME=on: %v", err)
	}
	job := s.wait(rs)
	if job.Meta["trigger"] != TriggerAdmin {
		t.Errorf("trigger = %q, want admin", job.Meta["trigger"])
	}
	if got := s.awaitRun(); got != OutcomeSucceeded {
		t.Errorf("the run ended %q, want succeeded", got)
	}
	if got := s.state().Mode; got != ModeActive {
		t.Errorf("stored mode = %q, want active", got)
	}
}

// An environment value never reaches the database (RESEARCH F3): with the variable off and the stored
// mode active, saving the profile leaves the mode active.
func TestEnvOffSetProfileNeverSavesTheOverride(t *testing.T) {
	ctx := context.Background()
	s := newSchedEnv(t, acmetest.Options{})
	s.issueFirst()
	off := s.buildEnv(s.hub, &recInstaller{}, envSwitch(false))

	if err := off.SetProfile(ctx, "shortlived"); err != nil {
		t.Fatalf("SetProfile: %v", err)
	}
	st := s.state()
	if st.Profile != "shortlived" {
		t.Errorf("profile = %q, want shortlived saved", st.Profile)
	}
	if st.Mode != ModeActive {
		t.Errorf("acme_state.mode = %q after SetProfile with TIERPLAN_ACME=off, want active: the variable must not be saved", st.Mode)
	}
}

// With the variable off a scheduler round orders nothing, however due the renewal is, and the stored
// state is left as it was.
func TestEnvOffSchedulerRenewsNothing(t *testing.T) {
	s := newSchedEnv(t, acmetest.Options{})
	s.issueFirst()
	renewAt := s.state().RenewAt
	off := s.buildEnv(s.hub, &recInstaller{}, envSwitch(false))
	s.schedule(off)
	before := counts(s.mgrEnv)

	s.stepTo(renewAt.Add(time.Hour))
	s.quiet(1)
	s.noCARequests(before)
	if got := s.state().Mode; got != ModeActive {
		t.Errorf("stored mode = %q, want active", got)
	}
}

// With the variable off an open order is not continued; with the variable removed the same order is
// picked up (D-02).
func TestEnvOffContinuesNoOpenOrder(t *testing.T) {
	ctx := context.Background()
	s := newSchedEnv(t, acmetest.Options{})
	s.issueFirst()
	release := s.fake.HoldValidation()
	stop := s.schedule(s.mgr)

	// A scheduled renewal gets as far as an order at the CA, then the process stops in the middle.
	s.stepTo(s.state().RenewAt)
	s.clock.WaitArmed(t)
	orders := s.fake.Count(acmetest.EndpointNewOrder)
	stop()
	s.hub.Close()
	if got := s.awaitRun(); got != OutcomeFailed {
		t.Fatalf("the cut-short run ended %q, want failed", got)
	}

	hub := jobs.New(jobs.Options{Now: s.clock.Now})
	t.Cleanup(hub.Close)
	off := s.buildEnv(hub, s.inst, envSwitch(false))
	s.mgr = off
	if err := off.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stopOff := s.schedule(off)
	for range 2 {
		s.step(Tick)
		s.quiet(2)
	}
	if n := s.fake.Count(acmetest.EndpointNewOrder); n != orders {
		t.Errorf("the CA saw %d orders with TIERPLAN_ACME=off, want %d", n, orders)
	}
	if _, _, open, err := s.store.OpenOrder(ctx); err != nil || !open {
		t.Fatalf("OpenOrder = open %v, %v; want the order left open", open, err)
	}
	stopOff()

	// The variable is gone: the first round carries the order on.
	back := s.buildEnv(hub, s.inst, config.ACMEEnv{})
	s.mgr = back
	if err := back.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.schedule(back)
	s.clock.WaitArmed(t)
	release()
	s.clock.Advance(PollInterval)
	if got := s.awaitRun(); got != OutcomeSucceeded {
		t.Fatalf("the resumed run ended %q, want succeeded", got)
	}
	if n := s.fake.Count(acmetest.EndpointNewOrder); n != orders {
		t.Errorf("the CA saw %d orders, want the open one picked up, not %d", n, orders)
	}
}

// Without the variable nothing is locked and Stop works as before.
func TestEnvUnsetLocksNothing(t *testing.T) {
	ctx := context.Background()
	s := newSchedEnv(t, acmetest.Options{})
	s.issueFirst()

	st, err := s.mgr.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Locks.ACME != nil {
		t.Errorf("Status.Locks.ACME = %+v with no variable, want none", st.Locks.ACME)
	}
	if err := s.mgr.Stop(ctx, "user:1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := s.state().Mode; got != ModeOff {
		t.Errorf("stored mode = %q after Stop, want off", got)
	}
}

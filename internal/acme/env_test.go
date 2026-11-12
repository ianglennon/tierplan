// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"errors"
	"log/slog"
	"slices"
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

// requireFieldLocked fails the test unless err is the lock on field, naming variable and answering problem.
func requireFieldLocked(t *testing.T, what string, err error, field, variable, problem string) {
	t.Helper()
	var le *ErrLocked
	if !errors.As(err, &le) {
		t.Fatalf("%s = %v, want *ErrLocked", what, err)
	}
	if le.Field != field || le.Var != variable || le.Problem != problem {
		t.Errorf("%s = %+v, want field %q, var %q, problem %s", what, le, field, variable, problem)
	}
}

// accountsAtTheCA is how many accounts the fake CA has created since noAccount.
func (s *schedEnv) accountsAtTheCA() int { return len(s.fake.AccountsSeen()) - s.accountsBefore }

// noAccount forgets the account the environment registered, so a registration starts from none, the way a
// fresh install does. The CA keeps its record of that first account; accountsAtTheCA counts from here.
func (s *schedEnv) noAccount() {
	s.t.Helper()
	if err := s.client.Forget(context.Background()); err != nil {
		s.t.Fatal(err)
	}
	s.accountsBefore = len(s.fake.AccountsSeen())
}

// With TIERPLAN_ACME_EMAIL set, a registration with no address types the variable's, the variable's own
// address is accepted, and any other address is refused before the CA is asked (D-17, UI-SPEC A2).
func TestEnvEmailLockDecidesTheRegisteredContact(t *testing.T) {
	ctx := context.Background()
	env := config.ACMEEnv{Email: "ops@example.com"}

	for _, typed := range []string{"", "ops@example.com"} {
		t.Run("typed="+typed, func(t *testing.T) {
			s := newSchedEnv(t, acmetest.Options{})
			s.noAccount()
			m := s.buildEnv(s.hub, s.inst, env)
			acct, err := m.Register(ctx, typed, true)
			if err != nil {
				t.Fatalf("Register(%q): %v", typed, err)
			}
			if acct.Contact != "mailto:ops@example.com" {
				t.Errorf("contact = %q, want the variable's address", acct.Contact)
			}
			seen := s.fake.AccountsSeen()[s.accountsBefore:]
			if len(seen) != 1 || !slices.Equal(seen[0].Contact, []string{"mailto:ops@example.com"}) {
				t.Errorf("the CA saw %+v, want one account with the variable's address", seen)
			}
		})
	}

	t.Run("a different address is refused", func(t *testing.T) {
		s := newSchedEnv(t, acmetest.Options{})
		s.noAccount()
		m := s.buildEnv(s.hub, s.inst, env)
		before := counts(s.mgrEnv)
		_, err := m.Register(ctx, "other@example.com", true)
		requireFieldLocked(t, "Register", err, LockEmail, config.EnvACMEEmail, "acme_email_locked")
		if strings.Contains(err.Error(), "ops@example.com") || strings.Contains(err.Error(), "other@example.com") {
			t.Errorf("the refusal %q shows an address, want the variable's name only", err)
		}
		s.noCARequests(before)
		if n := s.accountsAtTheCA(); n != 0 {
			t.Errorf("%d accounts at the CA after a refused registration, want none", n)
		}
		if _, ok, err := s.client.Account(ctx); err != nil || ok {
			t.Errorf("Account = found %v, %v; want nothing stored", ok, err)
		}
	})
}

// With TIERPLAN_ACME_ACCEPT_TOS set, a registration that does not agree to the terms is refused, and one
// that does is accepted (D-17, UI-SPEC A3).
func TestEnvAcceptTOSLockRefusesADisagreement(t *testing.T) {
	ctx := context.Background()
	env := config.ACMEEnv{AcceptTOS: true}

	s := newSchedEnv(t, acmetest.Options{TermsOfService: "https://acme.example/tos"})
	s.noAccount()
	m := s.buildEnv(s.hub, s.inst, env)
	before := counts(s.mgrEnv)
	_, err := m.Register(ctx, "a@example.com", false)
	requireFieldLocked(t, "Register", err, LockAcceptTOS, config.EnvACMEAcceptTOS, "acme_tos_locked")
	s.noCARequests(before)
	if n := s.accountsAtTheCA(); n != 0 {
		t.Errorf("%d accounts at the CA after a refused registration, want none", n)
	}

	if _, err := m.Register(ctx, "a@example.com", true); err != nil {
		t.Fatalf("Register with the terms agreed: %v", err)
	}
	if n := s.accountsAtTheCA(); n != 1 {
		t.Errorf("%d accounts at the CA, want one", n)
	}
}

// With neither variable set the registration is exactly what the person typed.
func TestEnvRegisterWithoutTheVariablesIsUnchanged(t *testing.T) {
	ctx := context.Background()
	s := newSchedEnv(t, acmetest.Options{})
	s.noAccount()
	if _, err := s.mgr.Register(ctx, "a@example.com", false); !errors.Is(err, ErrTOSNotAccepted) {
		t.Errorf("Register without the terms = %v, want ErrTOSNotAccepted, not a lock", err)
	}
	acct, err := s.mgr.Register(ctx, "a@example.com", true)
	if err != nil || acct.Contact != "mailto:a@example.com" {
		t.Errorf("Register = %+v, %v; want the typed address", acct, err)
	}
}

// An account that exists keeps its contact: the variable names the address for a registration, it does
// not rewrite an account, and the status shows the account's real contact (UI-SPEC A2).
func TestEnvEmailLockLeavesAnExistingAccountAlone(t *testing.T) {
	ctx := context.Background()
	s := newSchedEnv(t, acmetest.Options{})
	s.noAccount()
	if _, err := s.mgr.Register(ctx, "a@example.com", true); err != nil {
		t.Fatal(err)
	}
	m := s.buildEnv(s.hub, s.inst, config.ACMEEnv{Email: "b@example.com"})
	before := counts(s.mgrEnv)

	if _, err := m.Register(ctx, "", true); !errors.Is(err, ErrAccountExists) {
		t.Errorf("Register over an existing account = %v, want ErrAccountExists", err)
	}
	s.noCARequests(before)
	if n := s.accountsAtTheCA(); n != 1 {
		t.Errorf("%d accounts at the CA, want only the first", n)
	}
	st, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Account == nil || st.Account.Contact != "mailto:a@example.com" {
		t.Errorf("Status.Account = %+v, want the account's own contact a@example.com", st.Account)
	}
	if want := (&Lock{Var: config.EnvACMEEmail, Value: "b@example.com"}); st.Locks.Email == nil || *st.Locks.Email != *want {
		t.Errorf("Status.Locks.Email = %+v, want %+v", st.Locks.Email, want)
	}
}

// The status names the variable that fixes each field, its value and what is stored beneath it; a field
// no variable fixes has no entry, and the terms agreement has no stored value.
func TestEnvLocksReportEachFieldTheEnvironmentFixes(t *testing.T) {
	ctx := context.Background()
	s := newSchedEnv(t, acmetest.Options{})
	s.issueFirst()
	if err := s.mgr.SetProfile(ctx, "shortlived"); err != nil {
		t.Fatal(err)
	}

	all := s.buildEnv(s.hub, s.inst, config.ACMEEnv{
		Enabled: new(false), Email: "ops@example.com", AcceptTOS: true, Profile: "classic",
	})
	st, err := all.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := Locks{
		ACME:      &Lock{Var: config.EnvACME, Value: "off", Stored: "on"},
		Email:     &Lock{Var: config.EnvACMEEmail, Value: "ops@example.com"},
		AcceptTOS: &Lock{Var: config.EnvACMEAcceptTOS, Value: "true"},
		Profile:   &Lock{Var: config.EnvACMEProfile, Value: "classic", Stored: "shortlived"},
	}
	for name, p := range map[string][2]*Lock{
		"acme": {st.Locks.ACME, want.ACME}, "email": {st.Locks.Email, want.Email},
		"acceptTos": {st.Locks.AcceptTOS, want.AcceptTOS}, "profile": {st.Locks.Profile, want.Profile},
	} {
		if p[0] == nil || *p[0] != *p[1] {
			t.Errorf("Locks.%s = %+v, want %+v", name, p[0], p[1])
		}
	}
	if st.Profile != "classic" {
		t.Errorf("Status.Profile = %q, want the variable's profile in force", st.Profile)
	}
	if got := s.state().Profile; got != "shortlived" {
		t.Errorf("stored profile = %q, want shortlived kept beneath the variable", got)
	}

	none, err := s.mgr.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if none.Locks != (Locks{}) {
		t.Errorf("Locks = %+v with nothing set, want every entry nil", none.Locks)
	}
	if none.Profile != "shortlived" {
		t.Errorf("Status.Profile = %q once the variable is gone, want the stored shortlived back", none.Profile)
	}
}

// The profile a renewal asks for is the variable's when it is set, otherwise the stored one (D-24).
func TestEnvProfileDecidesTheRenewalOrder(t *testing.T) {
	ctx := context.Background()
	s := newSchedEnv(t, acmetest.Options{})
	s.issueFirst()
	if err := s.mgr.SetProfile(ctx, "classic"); err != nil {
		t.Fatal(err)
	}
	m := s.buildEnv(s.hub, s.inst, config.ACMEEnv{Profile: "shortlived"})

	rs, err := m.RenewNow(ctx, "user:1")
	if err != nil {
		t.Fatalf("RenewNow: %v", err)
	}
	s.wait(rs)
	if got := s.awaitRun(); got != OutcomeSucceeded {
		t.Fatalf("the renewal ended %q, want succeeded", got)
	}
	orders := s.fake.Orders()
	if len(orders) != 2 || orders[1].Profile != "shortlived" {
		t.Errorf("orders = %+v, want the renewal to ask for shortlived, not the stored classic", orders)
	}
}

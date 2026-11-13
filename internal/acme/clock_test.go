// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/jobs"
)

// rtFunc is an http.RoundTripper made of a function.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// answer is a round tripper that replies with the given headers, after the clock has moved on by took.
func answer(clk *acmetest.Clock, took time.Duration, header http.Header) rtFunc {
	return func(*http.Request) (*http.Response, error) {
		clk.Advance(took)
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: http.NoBody}, nil
	}
}

func dateHeader(t time.Time, kv ...string) http.Header {
	h := http.Header{}
	if !t.IsZero() {
		h.Set("Date", t.UTC().Format(http.TimeFormat))
	}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestSkewRecorder(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	req, err := http.NewRequest(http.MethodGet, "https://ca.example/directory", nil)
	if err != nil {
		t.Fatal(err)
	}

	// measure sends one request through a recorder whose clock stands at base and returns the sample.
	measure := func(t *testing.T, took time.Duration, h http.Header) ClockSample {
		t.Helper()
		clk := acmetest.NewClock(base)
		rec := newSkewRecorder(clk)
		resp, err := rec.wrap(answer(clk, took, h)).RoundTrip(req)
		if err != nil || resp == nil {
			t.Fatalf("RoundTrip = %v, %v", resp, err)
		}
		_ = resp.Body.Close()
		return rec.sample()
	}

	// The CA's Date is cut to the second, so the offset between whole seconds is exactly their difference
	// and the five-minute line falls between 300 s and 301 s on either side.
	for _, tc := range []struct {
		name  string
		date  time.Duration // the CA's Date relative to the host's clock
		want  time.Duration
		isOff bool
	}{
		{"the CA 301 s behind", -301 * time.Second, 301 * time.Second, true},
		{"the CA 300 s behind", -300 * time.Second, 300 * time.Second, false},
		{"the CA 299 s behind", -299 * time.Second, 299 * time.Second, false},
		{"the CA 301 s ahead", 301 * time.Second, -301 * time.Second, true},
		{"the CA 300 s ahead", 300 * time.Second, -300 * time.Second, false},
		{"the CA in step", 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := measure(t, 0, dateHeader(base.Add(tc.date)))
			if got.Offset != tc.want || !got.At.Equal(base) {
				t.Fatalf("sample = %+v, want offset %v at %v", got, tc.want, base)
			}
			if got.Off(base) != tc.isOff {
				t.Fatalf("Off = %v, want %v", got.Off(base), tc.isOff)
			}
		})
	}

	t.Run("the offset is taken from the midpoint of the request", func(t *testing.T) {
		t.Parallel()
		// Asked at base, answered at base+2s: the midpoint is base+1s. A Date of base-399s is 400 s behind it.
		got := measure(t, 2*time.Second, dateHeader(base.Add(-399*time.Second)))
		if got.Offset != 400*time.Second || !got.At.Equal(base.Add(2*time.Second)) {
			t.Fatalf("sample = %+v, want 400 s at base+2s", got)
		}
	})

	t.Run("a midpoint inside a second is rounded half up against the middle of the Date second", func(t *testing.T) {
		t.Parallel()
		// Midpoint base+1.5s, Date base: 1.5 s against the Date's middle (base+0.5s) is 1 s.
		if got := measure(t, 3*time.Second, dateHeader(base)); got.Offset != time.Second {
			t.Fatalf("offset = %v, want 1s", got.Offset)
		}
		// Midpoint base+0.5s, Date base: in step within the second.
		if got := measure(t, time.Second, dateHeader(base)); got.Offset != 0 {
			t.Fatalf("offset = %v, want 0", got.Offset)
		}
	})

	t.Run("a response from a cache, an unreadable Date and a failed request leave the sample alone", func(t *testing.T) {
		t.Parallel()
		clk := acmetest.NewClock(base)
		rec := newSkewRecorder(clk)
		send := func(rt http.RoundTripper) {
			t.Helper()
			resp, err := rec.wrap(rt).RoundTrip(req)
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			_ = err
		}
		send(answer(clk, 0, dateHeader(base.Add(-600*time.Second))))
		first := rec.sample()
		if first.Offset != 600*time.Second {
			t.Fatalf("first sample = %+v", first)
		}
		clk.Advance(time.Minute)
		send(answer(clk, 0, dateHeader(base, "Age", "3")))
		send(answer(clk, 0, dateHeader(time.Time{}, "Date", "yesterday-ish")))
		send(answer(clk, 0, http.Header{}))
		send(rtFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("connection refused") }))
		if got := rec.sample(); got != first {
			t.Fatalf("sample = %+v, want the first, %+v", got, first)
		}
		// Age 0 is the CA's own answer, and a later sample replaces the earlier one.
		send(answer(clk, 0, dateHeader(clk.Now().Add(-10*time.Second), "Age", "0")))
		if got := rec.sample(); got.Offset != 10*time.Second || !got.At.Equal(clk.Now()) {
			t.Fatalf("sample = %+v, want 10 s at %v", got, clk.Now())
		}
	})

	t.Run("a sample is believed for 48 hours", func(t *testing.T) {
		t.Parallel()
		s := ClockSample{Offset: 600 * time.Second, At: base}
		for _, tc := range []struct {
			name string
			at   time.Time
			want bool
		}{
			{"at the time", base, true},
			{"at exactly 48 hours", base.Add(ClockMeasureTTL), true},
			{"a second later", base.Add(ClockMeasureTTL + time.Second), false},
		} {
			if got := s.Off(tc.at); got != tc.want {
				t.Errorf("%s: Off = %v, want %v", tc.name, got, tc.want)
			}
		}
		if (ClockSample{}).Off(base) {
			t.Error("the zero sample is off")
		}
		if !(ClockSample{Offset: -600 * time.Second, At: base}).Off(base) {
			t.Error("a clock 10 minutes behind is not off")
		}
	})
}

// newClockEnv is a Manager over a fake CA that answers with the given behaviour, an account already
// registered and a job hub on the test's clock.
func newClockEnv(t *testing.T, opts acmetest.Options) *mgrEnv {
	t.Helper()
	oe := newOrderEnvWith(t, opts)
	hub := jobs.New(jobs.Options{Now: oe.clock.Now})
	t.Cleanup(hub.Close)
	e := &mgrEnv{orderEnv: oe, hub: hub, inst: &recInstaller{}, urls: &urlSet{public: "https://" + testHost}}
	e.mgr = e.manager(e.inst, false)
	return e
}

// restarted is a Manager over the environment's stored state and a new Client: what a restarted
// process builds, with no clock sample yet.
func (e *mgrEnv) restarted(env config.ACMEEnv, certFiles bool) (*Manager, *recInstaller) {
	e.t.Helper()
	e.client = NewClient(e.client.Directory(), e.store, e.clock, "tierplan-test/0")
	inst := &recInstaller{}
	return NewManager(Deps{
		Store: e.store, Client: e.client, Challenges: e.tokens, Jobs: e.hub, Installer: inst, Clock: e.clock,
		PublicURL: e.urls.getPublic, AgentURL: e.urls.getAgent, BasePath: e.urls.getBase,
		CertFiles: certFiles, Env: env, Log: slog.New(slog.DiscardHandler),
		Jitter: func() float64 { return 0 },
		Rand:   func(int64) int64 { return 0 },
	}), inst
}

func clockOffOf(t *testing.T, m *Manager, sv Served) []Alert {
	t.Helper()
	as, err := m.Alerts(context.Background(), sv)
	if err != nil {
		t.Fatalf("Alerts: %v", err)
	}
	var out []Alert
	for _, a := range as {
		if a.Kind == AlertClockOff {
			out = append(out, a)
		}
	}
	return out
}

func TestClockOffThroughTheFakeCA(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tenMinutes := 10 * time.Minute

	t.Run("active: the alert appears at 10 minutes behind and clears on the first answer that is in step", func(t *testing.T) {
		t.Parallel()
		e := newClockEnv(t, acmetest.Options{DateOffset: -tenMinutes})
		e.issue()
		if got := e.client.ClockSample().Offset; got != tenMinutes {
			t.Fatalf("ClockSample().Offset = %v, want +10m (TierPlan ahead)", got)
		}
		as, err := e.mgr.Alerts(ctx, Served{})
		if err != nil {
			t.Fatalf("Alerts: %v", err)
		}
		if len(as) != 1 || as[0].Kind != AlertClockOff || as[0].Offset != tenMinutes ||
			as[0].MeasuredAt.IsZero() || as[0].MeasuredAt.After(e.clock.Now()) {
			t.Fatalf("alerts = %+v, want one clock_off of +10m measured now", as)
		}

		// The CA's clock is put right. The next answer is a fresh sample, not the cached directory.
		e.fake.SetDateOffset(0)
		e.clock.Advance(metaTTL + time.Second)
		if _, err := e.client.Meta(ctx); err != nil {
			t.Fatalf("Meta: %v", err)
		}
		if got := e.client.ClockSample(); got.Offset != 0 || !got.At.Equal(e.clock.Now()) {
			t.Fatalf("ClockSample = %+v, want 0 at %v", got, e.clock.Now())
		}
		if as := clockOffOf(t, e.mgr, Served{}); len(as) != 0 {
			t.Fatalf("alerts = %+v, want the clock item gone", as)
		}
		if st, err := e.mgr.Status(ctx); err != nil || st.ClockOff != nil {
			t.Fatalf("Status.ClockOff = %+v (%v), want nil", st.ClockOff, err)
		}
	})

	t.Run("a sample older than 48 hours is not shown, in active mode", func(t *testing.T) {
		t.Parallel()
		e := newClockEnv(t, acmetest.Options{DateOffset: -tenMinutes})
		e.issue()
		if len(clockOffOf(t, e.mgr, Served{})) != 1 {
			t.Fatal("no clock item right after the run")
		}
		e.clock.Advance(ClockMeasureTTL)
		if len(clockOffOf(t, e.mgr, Served{})) != 1 {
			t.Fatal("the item went at exactly 48 hours")
		}
		e.clock.Advance(time.Second)
		if st, err := e.mgr.Status(ctx); err != nil || st.ClockOff != nil {
			t.Fatalf("Status.ClockOff = %+v (%v), want nil after 48 hours", st.ClockOff, err)
		}
	})

	t.Run("off with no run yet: no sample, no clock item and no request to the CA", func(t *testing.T) {
		t.Parallel()
		e := newClockEnv(t, acmetest.Options{DateOffset: -tenMinutes})
		m, _ := e.restarted(config.ACMEEnv{}, false)
		before := counts(e)
		st, err := m.Status(ctx)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if st.Mode != ModeOff || st.ClockOff != nil {
			t.Fatalf("Status = mode %q, ClockOff %+v; want off and nil", st.Mode, st.ClockOff)
		}
		if as := clockOffOf(t, m, Served{}); len(as) != 0 {
			t.Fatalf("alerts = %+v, want none", as)
		}
		if s := e.client.ClockSample(); s != (ClockSample{}) {
			t.Fatalf("ClockSample = %+v, want none", s)
		}
		e.noCARequests(before)
	})

	t.Run("off, a failed first Issue: the run measured, so the clock is set beside the error (D-07, D-29)", func(t *testing.T) {
		t.Parallel()
		e := newClockEnv(t, acmetest.Options{DateOffset: -tenMinutes})
		m, _ := e.restarted(config.ACMEEnv{}, false)
		e.mgr = m
		e.fake.FailValidation(testHost, acmetest.Problem{Type: "connection", Detail: "refused"})
		e.issue()
		if got := e.client.ClockSample().Offset; got != tenMinutes {
			t.Fatalf("ClockSample().Offset = %v, want +10m", got)
		}
		st, err := m.Status(ctx)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if st.Mode != ModeOff || st.LastError == nil || st.ClockOff == nil || st.ClockOff.Offset != tenMinutes {
			t.Fatalf("Status = mode %q, LastError %+v, ClockOff %+v; want off with both set", st.Mode, st.LastError, st.ClockOff)
		}
		as, err := m.Alerts(ctx, Served{})
		if err != nil {
			t.Fatalf("Alerts: %v", err)
		}
		if len(as) != 1 || as[0].Kind != AlertClockOff {
			t.Fatalf("alerts = %+v, want only clock_off (an admin's first Issue gives no renewal_failed)", as)
		}

		e.clock.Advance(ClockMeasureTTL + time.Second)
		if st, err := m.Status(ctx); err != nil || st.ClockOff != nil {
			t.Fatalf("Status.ClockOff = %+v (%v), want nil once the sample is stale", st.ClockOff, err)
		}
	})
}

// managerWith is a Manager over the environment's stored state and a new Client, the way a restarted
// process builds one, for the scheduler tests: it has no clock sample yet.
func (s *schedEnv) managerWith(env config.ACMEEnv, certFiles bool) *Manager {
	s.t.Helper()
	s.client = NewClient(s.client.Directory(), s.store, s.clock, "tierplan-test/0")
	return NewManager(Deps{
		Store: s.store, Client: s.client, Challenges: s.tokens, Jobs: s.hub, Installer: s.inst, Clock: s.clock,
		PublicURL: s.urls.getPublic, AgentURL: s.urls.getAgent, BasePath: s.urls.getBase,
		CertFiles: certFiles, Env: env, Log: slog.New(slog.DiscardHandler),
		Jitter:   func() float64 { return 0 },
		Rand:     func(n int64) int64 { return n / 2 },
		RunEnded: func(_, outcome string) { s.ended <- outcome },
	})
}

// notDue leaves a stored certificate whose renewal and renewal-information check are both far off, so
// a scheduler round has no business with the CA.
func (s *schedEnv) notDue() {
	s.t.Helper()
	st := s.state()
	st.RenewAt, st.AriNextCheckAt = testNow.Add(60*24*time.Hour), testNow.Add(24*time.Hour)
	if err := s.store.SaveState(context.Background(), st); err != nil {
		s.t.Fatalf("SaveState: %v", err)
	}
}

func TestClockProbeAtStart(t *testing.T) {
	ctx := context.Background()
	off := false

	t.Run("active: the first round reads the directory once, the second adds nothing", func(t *testing.T) {
		s := newSchedEnv(t, acmetest.Options{DateOffset: -10 * time.Minute})
		s.issueFirst()
		s.notDue()
		m := s.managerWith(config.ACMEEnv{}, false)
		before := s.fake.Count(acmetest.EndpointDirectory)
		if (s.client.ClockSample() != ClockSample{}) {
			t.Fatal("the new client already holds a sample")
		}

		s.schedule(m)
		if n := s.fake.Count(acmetest.EndpointDirectory) - before; n != 1 {
			t.Fatalf("the first round read the directory %d times, want once", n)
		}
		if got := s.client.ClockSample().Offset; got != 10*time.Minute {
			t.Fatalf("ClockSample().Offset = %v, want +10m", got)
		}
		st, err := m.Status(ctx)
		if err != nil || st.Mode != ModeActive || st.ClockOff == nil || st.ClockOff.Offset != 10*time.Minute {
			t.Fatalf("Status = mode %q, ClockOff %+v (%v); want active and +10m", st.Mode, st.ClockOff, err)
		}

		s.step(Tick)
		s.quiet(1)
		if n := s.fake.Count(acmetest.EndpointDirectory) - before; n != 1 {
			t.Fatalf("the directory was read %d times after two rounds, want the probe's one", n)
		}
	})

	// Nothing measures in the background in any other state: a round makes no request and no sample exists.
	for name, setup := range map[string]func(s *schedEnv) *Manager{
		"paused after a restore": func(s *schedEnv) *Manager {
			st := s.state()
			st.PausedReason = PausedRestore
			if err := s.store.SaveState(ctx, st); err != nil {
				t.Fatalf("SaveState: %v", err)
			}
			return s.managerWith(config.ACMEEnv{}, false)
		},
		"certificate files": func(s *schedEnv) *Manager { return s.managerWith(config.ACMEEnv{}, true) },
		"TIERPLAN_ACME=off": func(s *schedEnv) *Manager { return s.managerWith(config.ACMEEnv{Enabled: &off}, false) },
	} {
		t.Run(name+": no request and no sample", func(t *testing.T) {
			s := newSchedEnv(t, acmetest.Options{DateOffset: -10 * time.Minute})
			s.issueFirst()
			s.notDue()
			m := setup(s)
			before := counts(s.mgrEnv)

			s.schedule(m)
			s.step(Tick)
			s.quiet(1)
			s.noCARequests(before)
			if (s.client.ClockSample() != ClockSample{}) {
				t.Fatalf("ClockSample = %+v, want none", s.client.ClockSample())
			}
			if st, err := m.Status(ctx); err != nil || st.ClockOff != nil {
				t.Fatalf("Status.ClockOff = %+v (%v), want nil", st.ClockOff, err)
			}
		})
	}
}

// TestPreflightIgnoresClock is the prohibition of D-07: the measured offset drives the bell and the
// page hint and nothing else. A CA two hours out refuses nothing and moves no time TierPlan stores.
func TestPreflightIgnoresClock(t *testing.T) {
	type outcome struct {
		Mode                       string
		RenewAt                    time.Time
		RenewBasis                 string
		AriNextCheckAt             time.Time
		Failures                   int
		NextAttemptAt              time.Time
		Refusals, Runs, Orders     int
		LastSuccessKind, LastError string
	}
	run := func(t *testing.T, dateOffset time.Duration) (outcome, ClockSample) {
		t.Helper()
		s := newSchedEnv(t, acmetest.Options{DateOffset: dateOffset})
		s.issueFirst() // an Issue the offset must not refuse: issue() fails the test on a refusal
		s.schedule(s.mgr)
		s.idle(2*Tick, 1)
		st := s.state()
		status, err := s.mgr.Status(context.Background())
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		return outcome{
			Mode: st.Mode, RenewAt: st.RenewAt, RenewBasis: st.RenewBasis, AriNextCheckAt: st.AriNextCheckAt,
			Failures: st.ConsecutiveFailures, NextAttemptAt: st.NextAttemptAt, Refusals: len(status.Refusals),
			Runs: len(s.runs()), Orders: len(s.fake.Orders()), LastSuccessKind: st.LastSuccessKind, LastError: st.LastErrorClass,
		}, s.client.ClockSample()
	}

	t.Run("a CA two hours out changes no decision", func(t *testing.T) {
		inStep, sampleA := run(t, 0)
		out, sampleB := run(t, -2*time.Hour)
		if sampleA.Offset != 0 || sampleB.Offset != 2*time.Hour {
			t.Fatalf("samples = %v and %v, want 0 and +2h: the offset was not measured", sampleA.Offset, sampleB.Offset)
		}
		if inStep != out {
			t.Fatalf("with the CA two hours out: %+v\nwith the CA in step:            %+v", out, inStep)
		}
		if out.Runs != 1 || out.Orders != 1 || out.Mode != ModeActive || out.Failures != 0 || out.Refusals != 0 {
			t.Fatalf("outcome = %+v, want one successful run on an active install", out)
		}
	})
}

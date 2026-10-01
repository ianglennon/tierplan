// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// The per-environment snapshot cache (plan 18, Pitfall 22, T-01-79). Docker is
// a FakeDaemon, so a real client fetches real list endpoints; a request count
// on the fake shows how many fetches reached the engine.

// stepClock is a clock the test moves by hand.
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func newStepClock() *stepClock { return &stepClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)} }

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// tickClock moves a minute on every reading, so a cached snapshot is always
// older than any TTL: only coalescing can then keep two callers to one fetch.
func tickClock() func() time.Time {
	c := newStepClock()
	return func() time.Time {
		c.advance(time.Minute)
		return c.now()
	}
}

// snapRig is a started monitor over one fake engine.
type snapRig struct {
	mon  *Monitor
	fake *dockertest.Fake
}

func newSnapRig(t *testing.T, cfg dockertest.FakeConfig, wantState State) *snapRig {
	t.Helper()
	fake := dockertest.NewFake(t, cfg)
	mon := NewMonitor(slog.New(slog.DiscardHandler), nil, blockingSleep)
	t.Cleanup(mon.StopAll)
	if err := mon.Start(context.Background(), "env1", fake); err != nil {
		t.Fatalf("Start: %v", err)
	}
	eventually(t, "the first probe", func() bool { return mon.Status("env1").State == wantState })
	return &snapRig{mon: mon, fake: fake}
}

func (r *snapRig) listRequests() int {
	n := 0
	for _, p := range r.fake.Requests() {
		if strings.HasSuffix(p, "/containers/json") {
			n++
		}
	}
	return n
}

func oneContainer() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Containers: []dockertest.FakeContainer{{ID: "c1", Names: []string{"/web"}, State: "running"}},
		Volumes:    []dockertest.FakeVolume{{Name: "data"}},
	}
}

func TestSnapshotsServeFromTheCacheWithinTheTTL(t *testing.T) {
	r := newSnapRig(t, oneContainer(), StateOnline)
	clk := newStepClock()
	s := NewSnapshots(r.mon, 5*time.Second, clk.now)

	first, st, err := s.Get(context.Background(), "env1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if st.State != StateOnline || first.Counts.Containers != 1 || first.Counts.Volumes != 1 {
		t.Fatalf("status %q, counts %+v", st.State, first.Counts)
	}
	clk.advance(4 * time.Second)
	second, _, err := s.Get(context.Background(), "env1")
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if got := r.listRequests(); got != 1 {
		t.Errorf("%d fetches reached the engine within the TTL, want 1", got)
	}
	if !second.FetchedAt.Equal(first.FetchedAt) {
		t.Errorf("the cached snapshot changed: %v then %v", first.FetchedAt, second.FetchedAt)
	}
}

func TestSnapshotsRefetchAfterTheTTL(t *testing.T) {
	r := newSnapRig(t, oneContainer(), StateOnline)
	clk := newStepClock()
	s := NewSnapshots(r.mon, 5*time.Second, clk.now)
	if _, _, err := s.Get(context.Background(), "env1"); err != nil {
		t.Fatal(err)
	}
	clk.advance(6 * time.Second)
	if _, _, err := s.Get(context.Background(), "env1"); err != nil {
		t.Fatal(err)
	}
	if got := r.listRequests(); got != 2 {
		t.Errorf("%d fetches after the TTL expired, want 2", got)
	}
}

func TestSnapshotsRefetchAfterInvalidate(t *testing.T) {
	r := newSnapRig(t, oneContainer(), StateOnline)
	s := NewSnapshots(r.mon, 5*time.Second, newStepClock().now)
	if _, _, err := s.Get(context.Background(), "env1"); err != nil {
		t.Fatal(err)
	}
	s.Invalidate("env1")
	s.Invalidate("not-an-environment") // harmless
	if _, _, err := s.Get(context.Background(), "env1"); err != nil {
		t.Fatal(err)
	}
	if got := r.listRequests(); got != 2 {
		t.Errorf("%d fetches after Invalidate, want 2", got)
	}
}

func TestConcurrentGetsForOneEnvironmentTriggerASingleFetch(t *testing.T) {
	cfg := oneContainer()
	cfg.ListDelay = 150 * time.Millisecond // long enough that all callers overlap
	r := newSnapRig(t, cfg, StateOnline)
	// The clock jumps a minute per reading, so the cache can never answer: only
	// joining the fetch already in flight keeps this to one request.
	s := NewSnapshots(r.mon, 5*time.Second, tickClock())

	const callers = 20
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Go(func() {
			snap, _, err := s.Get(context.Background(), "env1")
			if err == nil && snap.Counts.Containers != 1 {
				err = errors.New("a joined caller got the wrong snapshot")
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if got := r.listRequests(); got != 1 {
		t.Errorf("%d fetches for %d concurrent callers, want 1 (T-01-79)", got, callers)
	}
}

func TestACallerThatGivesUpDoesNotCancelTheSharedFetch(t *testing.T) {
	cfg := oneContainer()
	cfg.ListDelay = 150 * time.Millisecond
	r := newSnapRig(t, cfg, StateOnline)
	s := NewSnapshots(r.mon, 5*time.Second, newStepClock().now)

	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := s.Get(short, "env1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a caller whose context ended got %v, want its deadline error", err)
	}
	// The fetch carries on; a patient caller gets its result without a second one.
	snap, _, err := s.Get(context.Background(), "env1")
	if err != nil || snap.Counts.Containers != 1 {
		t.Fatalf("patient Get = %+v, %v", snap.Counts, err)
	}
	if got := r.listRequests(); got != 1 {
		t.Errorf("%d fetches, want 1", got)
	}
}

func TestSnapshotsAreKeptPerEnvironment(t *testing.T) {
	r := newSnapRig(t, oneContainer(), StateOnline)
	other := dockertest.NewFake(t, dockertest.FakeConfig{})
	if err := r.mon.Start(context.Background(), "env2", other); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the second probe", func() bool { return r.mon.Status("env2").State == StateOnline })
	s := NewSnapshots(r.mon, 5*time.Second, newStepClock().now)

	a, _, err := s.Get(context.Background(), "env1")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := s.Get(context.Background(), "env2")
	if err != nil {
		t.Fatal(err)
	}
	if a.Counts.Containers != 1 || b.Counts.Containers != 0 {
		t.Errorf("env1 has %d containers and env2 %d, want 1 and 0", a.Counts.Containers, b.Counts.Containers)
	}
}

func TestSnapshotsReturnTheMonitorsOfflineAndUnsupportedErrorsUntouched(t *testing.T) {
	t.Run("offline", func(t *testing.T) {
		r := newSnapRig(t, oneContainer(), StateOnline)
		r.fake.Stop()
		r.mon.Kick("env1")
		eventually(t, "offline", func() bool { return r.mon.Status("env1").State == StateOffline })
		_, st, err := NewSnapshots(r.mon, time.Second, nil).Get(context.Background(), "env1")
		if !errors.Is(err, ErrOffline) || st.State != StateOffline {
			t.Errorf("err %v, state %q, want ErrOffline and offline", err, st.State)
		}
	})
	t.Run("unsupported", func(t *testing.T) {
		r := newSnapRig(t, dockertest.FakeConfig{APIVersion: "1.41", Version: "20.10.24"}, StateUnsupported)
		_, st, err := NewSnapshots(r.mon, time.Second, nil).Get(context.Background(), "env1")
		if !errors.Is(err, ErrUnsupported) || st.State != StateUnsupported || st.Reason != "api_too_old" {
			t.Errorf("err %v, state %q reason %q, want ErrUnsupported and api_too_old", err, st.State, st.Reason)
		}
		if got := r.listRequests(); got != 0 {
			t.Errorf("an unsupported engine was asked for its containers %d times", got)
		}
	})
}

func TestSnapshotsCountNodesAndServicesOnSwarmManagersOnly(t *testing.T) {
	mgr := oneContainer()
	mgr.SwarmState, mgr.SwarmControl, mgr.Nodes, mgr.Services = "active", true, 2, 4
	r := newSnapRig(t, mgr, StateOnline)
	snap, _, err := NewSnapshots(r.mon, time.Second, nil).Get(context.Background(), "env1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Counts.Nodes == nil || *snap.Counts.Nodes != 2 || snap.Counts.Services == nil || *snap.Counts.Services != 4 {
		t.Errorf("manager counts = %v/%v, want 2/4", snap.Counts.Nodes, snap.Counts.Services)
	}

	worker := oneContainer()
	worker.SwarmState, worker.SwarmControl = "active", false
	w := newSnapRig(t, worker, StateOnline)
	snap, _, err = NewSnapshots(w.mon, time.Second, nil).Get(context.Background(), "env1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Counts.Nodes != nil || snap.Counts.Services != nil {
		t.Errorf("worker counts = %v/%v, want none", snap.Counts.Nodes, snap.Counts.Services)
	}
	for _, p := range w.fake.Requests() {
		if strings.HasSuffix(p, "/nodes") || strings.HasSuffix(p, "/services") {
			t.Errorf("a worker was asked for %s", p)
		}
	}
}

func TestAFailedReadAsksTheMonitorToProbeNow(t *testing.T) {
	r := newSnapRig(t, oneContainer(), StateOnline)
	r.fake.Stop() // the monitor sleeps for ever, so it still believes the engine is online
	_, _, err := NewSnapshots(r.mon, time.Second, nil).Get(context.Background(), "env1")
	if err == nil || errors.Is(err, ErrOffline) {
		t.Fatalf("Get = %v, want a read failure that is not yet ErrOffline", err)
	}
	eventually(t, "the kicked probe to find the engine offline", func() bool {
		return r.mon.Status("env1").State == StateOffline
	})
}

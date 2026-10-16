// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// awaitMonitor is a monitor that probes only at start and when kicked, so the
// states a test sees are the ones it caused.
func awaitMonitor(t *testing.T) *Monitor {
	t.Helper()
	m := NewMonitor(slog.New(slog.DiscardHandler), nil, blockingSleep)
	t.Cleanup(m.StopAll)
	return m
}

// awaitCtx is the deadline that only guards a test against a hang.
func awaitCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func isOnline(s Status) bool  { return s.State == StateOnline }
func isOffline(s Status) bool { return s.State == StateOffline }

func TestAwaitStatusWaitsForAnEnvironmentThatIsNotMonitoredYet(t *testing.T) {
	m := awaitMonitor(t)
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	type result struct {
		st  Status
		err error
	}
	got := make(chan result, 1)
	ctx := awaitCtx(t)
	// AwaitProbe would return at once for an unmonitored environment; AwaitStatus waits.
	go func() {
		st, err := m.AwaitStatus(ctx, "late", isOnline)
		got <- result{st, err}
	}()
	if err := m.Start(context.Background(), "late", fake); err != nil {
		t.Fatal(err)
	}
	r := <-got
	if r.err != nil || r.st.State != StateOnline {
		t.Fatalf("AwaitStatus = %+v, %v, want online and no error", r.st, r.err)
	}
}

func TestAwaitStatusReturnsAtOnceWhenTheConditionAlreadyHolds(t *testing.T) {
	m := awaitMonitor(t)
	if err := m.Start(context.Background(), "e", dockertest.NewFake(t, dockertest.FakeConfig{})); err != nil {
		t.Fatal(err)
	}
	if st, err := m.AwaitStatus(awaitCtx(t), "e", func(s Status) bool { return s.State != StateUnknown }); err != nil || st.State != StateOnline {
		t.Fatalf("first AwaitStatus = %+v, %v, want online", st, err)
	}
	// A context that has already ended proves nothing is waited for: the condition is
	// evaluated before the context is looked at.
	done, cancel := context.WithCancel(context.Background())
	cancel()
	st, err := m.AwaitStatus(done, "e", isOnline)
	if err != nil || st.State != StateOnline {
		t.Fatalf("AwaitStatus with a true condition = %+v, %v, want online and no error", st, err)
	}
}

func TestAwaitStatusWithACancelledContextReturnsTheLastStatusAndTheContextError(t *testing.T) {
	m := awaitMonitor(t)
	done, cancel := context.WithCancel(context.Background())
	cancel()
	st, err := m.AwaitStatus(done, "never-started", isOnline)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if st.State != StateUnknown {
		t.Errorf("status = %+v, want the last status, unknown", st)
	}
}

func TestAwaitStatusSeesAnOnlineEnvironmentGoOfflineWhenItsDaemonStopsAndItIsKicked(t *testing.T) {
	m := awaitMonitor(t)
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	if err := m.Start(context.Background(), "e", fake); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AwaitStatus(awaitCtx(t), "e", isOnline); err != nil {
		t.Fatalf("never came online: %v", err)
	}
	fake.Stop()
	m.Kick("e")
	st, err := m.AwaitStatus(awaitCtx(t), "e", isOffline)
	if err != nil || st.State != StateOffline {
		t.Fatalf("AwaitStatus = %+v, %v, want offline after the daemon stopped", st, err)
	}
}

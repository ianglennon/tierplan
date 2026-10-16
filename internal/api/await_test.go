// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/tunnel"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The api suite's waits (phase 6.1, D-07). Every helper here waits on a signal the code under
// test already sends: the monitor's probe results, the tunnel session's heartbeats, the hub's
// registrations, the fake daemon's requests and the health broker's events. None polls, and the
// 30 s of testsupport.Context only guards against a hang. A test that asserts something an
// eventually-consistent read shows first awaits what that read depends on, then reads once.

// awaitMonitor returns the status of the monitor key for which cond is true, and fails the test
// naming what was awaited and the last status seen.
func awaitMonitor(t testing.TB, mon *envs.Monitor, key, what string, cond func(envs.Status) bool) envs.Status {
	t.Helper()
	st, err := mon.AwaitStatus(testsupport.Context(t), key, cond)
	if err != nil {
		t.Fatalf("waiting for %s of %s: %v; the last status was %+v", what, key, err, st)
	}
	return st
}

// awaitOnline waits until the monitor has probed id (an environment, or a target key) and found
// it online.
func awaitOnline(t testing.TB, mon *envs.Monitor, id string) envs.Status {
	t.Helper()
	return awaitMonitor(t, mon, id, "the monitor's first probe to find it online", func(s envs.Status) bool {
		return s.State == envs.StateOnline
	})
}

// awaitOffline waits until the monitor has found id offline.
func awaitOffline(t testing.TB, mon *envs.Monitor, id string) envs.Status {
	t.Helper()
	return awaitMonitor(t, mon, id, "the monitor to find it offline", func(s envs.Status) bool {
		return s.State == envs.StateOffline
	})
}

// awaitProbed waits until the monitor has probed the engine of every key (a target key such as
// envs.AgentTarget(id)) and found it online. An environment is online as soon as its agent's
// session is, while a call to one node goes through the monitor's client, which is ErrOffline
// until the first probe of that engine has finished.
func awaitProbed(t testing.TB, mon *envs.Monitor, keys ...string) {
	t.Helper()
	for _, k := range keys {
		awaitOnline(t, mon, k)
	}
}

// awaitFirstProbe waits until the first probe of every key has finished with any answer: online,
// offline or unsupported. It is what agentFix.connect waits for, because a test may connect an
// agent whose engine is meant to read unsupported or offline.
func awaitFirstProbe(t testing.TB, mon *envs.Monitor, keys ...string) {
	t.Helper()
	for _, k := range keys {
		awaitMonitor(t, mon, k, "the monitor's first probe to finish", func(s envs.Status) bool {
			return s.State != envs.StateUnknown
		})
	}
}

// awaitFake waits until cond, which reads the fake daemon's counters, is true. The fake wakes it
// on every request served or finished and on every change a long-lived handler makes.
func awaitFake(t testing.TB, fake *dockertest.Fake, what string, cond func() bool) {
	t.Helper()
	if err := fake.Await(testsupport.Context(t), cond); err != nil {
		t.Fatalf("waiting for the fake daemon: %s: %v", what, err)
	}
}

// awaitHeartbeat returns the status of the tunnel session s for which cond is true. The session
// wakes it on every heartbeat and every denial, and it fails if the session ends first.
func awaitHeartbeat(t testing.TB, s *tunnel.Session, cond func(tunnel.SessionStatus) bool) tunnel.SessionStatus {
	t.Helper()
	st, err := s.AwaitStatus(testsupport.Context(t), cond)
	if err != nil {
		t.Fatalf("waiting for the session of agent %s: %v; the last status was %+v", s.AgentID, err, st)
	}
	return st
}

// awaitEnvState waits until GET /api/environments shows id in state, and returns that row. It
// reads once at the start, and again each time something that can change an environment's health
// has happened: an event on the health broker (an agent connecting, disconnecting or being
// refused, a failover) or any change of the monitor (a probe finished). A state that follows
// the test clock alone is read as it stands, since the test moved the clock before it asked.
func awaitEnvState(t *testing.T, f *agentFix, id, state string) agentEnvBody {
	t.Helper()
	ctx, stop := context.WithCancel(testsupport.Context(t))
	defer stop()

	wake := make(chan struct{}, 1)
	poke := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	// The condition never holds: AwaitStatus evaluates it once now and after every change of the
	// monitor, so each evaluation is one wake-up and no change falls between two waits.
	go func() {
		_, _ = f.mon.AwaitStatus(ctx, id, func(envs.Status) bool { poke(); return false })
	}()

	_, events, cancel := f.brk.Subscribe(ctx)
	defer func() { cancel() }()

	var e agentEnvBody
	for {
		e = f.environment(t, id)
		if e.Status.State == state {
			return e
		}
		select {
		case _, ok := <-events:
			if !ok {
				// The broker dropped this subscriber for falling behind (or closed); take a new
				// subscription, whose backlog is not needed because the next read sees it all.
				cancel()
				_, events, cancel = f.brk.Subscribe(ctx)
			}
		case <-wake:
		case <-ctx.Done():
			t.Fatalf("waiting for environment %s to be %s: %v; its last row was %+v", id, state, ctx.Err(), e.Status)
		}
	}
}

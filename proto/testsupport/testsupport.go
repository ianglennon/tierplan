// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

// Package testsupport is the one set of waiting helpers every Go module of the
// repository uses in its tests. It lives in the proto module because that is the
// one module both the server and the agent already require, and it imports only
// the standard library.
//
// The rule it encodes: a condition that code in the same process brings about
// is waited on through a signal, never polled. Await and AwaitWithin wait on a
// channel; Context gives the seam methods that take a context (an event hub's
// Await, a monitor's AwaitStatus) a context that ends with the test. WaitFor
// and Poll exist only for conditions outside the test's control: a real Docker
// engine, an OS lock, a real process. Each has one generous deadline, so a run
// on a slow CI machine does not fail by a margin, and a genuine hang is still
// caught by "go test -timeout".
//
// Every timeout failure reads "testsupport: timed out after <d> waiting for
// <what>", followed by "; last seen: <seen>" when a Poll condition reported one,
// so a log names what was awaited without a rerun.
//
// Quarantine is the last resort for a test that cannot yet be made
// deterministic; see its doc.
package testsupport

import (
	"context"
	"os"
	"regexp"
	"testing"
	"time"
)

// Deadline is how long any wait in a test may take before it fails. It is
// generous on purpose: CI runs three to four times slower than a developer
// machine and a short deadline is a flake.
const Deadline = 30 * time.Second

// Context returns a context that ends after Deadline or when the test and its
// subtests finish, whichever is first. Pass it to the seam methods that wait on
// a signal and take a context.
func Context(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), Deadline)
	t.Cleanup(cancel)
	return ctx
}

// Await receives from ch and returns the value, failing the test after
// Deadline. A closed channel yields the zero value, so closing is a signal too.
func Await[T any](t testing.TB, what string, ch <-chan T) T {
	t.Helper()
	return AwaitWithin(t, Deadline, what, ch)
}

// AwaitWithin is Await with its own timeout, for a wait that is expected to be
// short or whose failure the test itself provokes.
func AwaitWithin[T any](t testing.TB, d time.Duration, what string, ch <-chan T) T {
	t.Helper()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case v := <-ch:
		return v
	case <-timer.C:
		t.Fatalf("%s", timeoutText(d, what, ""))
		var zero T
		return zero
	}
}

// WaitFor polls cond until it is true, failing the test after Deadline. It is
// for conditions outside the test's control only: a real Docker engine, an OS
// lock, a real process. A condition that code in the same process brings about
// is waited on through a signal; see Await.
func WaitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	Poll(t, Deadline, what, func() (bool, string) { return cond(), "" })
}

// Poll calls cond until it reports done, failing the test after timeout. When
// cond is not done it may return a description of what it saw, and the last
// non-empty one is part of the failure text. Like WaitFor it is for conditions
// outside the test's control only.
//
// The pace is a time.Ticker of timeout/300, kept between 10 ms and 250 ms.
func Poll(t testing.TB, timeout time.Duration, what string, cond func() (done bool, seen string)) {
	t.Helper()
	var last string
	check := func() bool {
		done, seen := cond()
		if seen != "" {
			last = seen
		}
		return done
	}
	if check() {
		return
	}
	ticker := time.NewTicker(pollInterval(timeout))
	defer ticker.Stop()
	expired := time.NewTimer(timeout)
	defer expired.Stop()
	for {
		select {
		case <-ticker.C:
			if check() {
				return
			}
		case <-expired.C:
			// One last look: the condition may have become true while the
			// ticker was between beats.
			if check() {
				return
			}
			t.Fatalf("%s", timeoutText(timeout, what, last))
			return
		}
	}
}

// pollInterval is timeout/300 within 10 ms to 250 ms.
func pollInterval(timeout time.Duration) time.Duration {
	return min(max(timeout/300, 10*time.Millisecond), 250*time.Millisecond)
}

// RunQuarantinedEnv is the environment variable that makes Quarantine run the
// tests it would otherwise skip. make test-stress (scripts/stress.sh) sets it to 1.
const RunQuarantinedEnv = "COAMING_RUN_QUARANTINED"

var quarantineReason = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \S`)

// Quarantine skips the calling test unless COAMING_RUN_QUARANTINED is 1, in
// which case it logs the reason and lets the test run. It is a last resort for
// a test that cannot yet be made deterministic (D-12, D-13): the reason must
// start with the date the test was quarantined, a space and a tracking
// reference, for example "2026-10-16 #123 fails on arm64 under load", and a
// reason without them fails the test. Every call site is listed in
// docs/testing.md and runs in make test-stress, so a quarantined test is never forgotten.
func Quarantine(t testing.TB, reason string) {
	t.Helper()
	if !quarantineReason.MatchString(reason) {
		t.Fatalf("testsupport: quarantine reason %q must start with YYYY-MM-DD, a space and a tracking reference", reason)
		return
	}
	if os.Getenv(RunQuarantinedEnv) != "1" {
		t.Skipf("quarantined: %s", reason)
		return
	}
	t.Logf("quarantined test running: %s", reason)
}

// timeoutText is the failure message every helper uses.
func timeoutText(d time.Duration, what, seen string) string {
	msg := "testsupport: timed out after " + d.String() + " waiting for " + what
	if seen != "" {
		msg += "; last seen: " + seen
	}
	return msg
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

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

// timeoutText is the failure message every helper uses.
func timeoutText(d time.Duration, what, seen string) string {
	msg := "testsupport: timed out after " + d.String() + " waiting for " + what
	if seen != "" {
		msg += "; last seen: " + seen
	}
	return msg
}

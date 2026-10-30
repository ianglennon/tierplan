// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package dockertest

import (
	"testing"
	"time"
)

// The COAMING_TEST_PROBE_DELAY sweep must keep applying after a test reconfigures the fake,
// or the tests that reconfigure (an upgraded daemon, a downgraded one) quietly leave the sweep.
func TestTheProbeDelaySweepSurvivesReconfigure(t *testing.T) {
	t.Setenv("COAMING_TEST_PROBE_DELAY", "150ms")
	f := NewFake(t, FakeConfig{})
	if got := f.config().ProbeDelay; got != 150*time.Millisecond {
		t.Fatalf("NewFake ProbeDelay = %s, want 150ms from COAMING_TEST_PROBE_DELAY", got)
	}
	f.Reconfigure(FakeConfig{Version: "28.0.0"})
	if got := f.config().ProbeDelay; got != 150*time.Millisecond {
		t.Fatalf("ProbeDelay after Reconfigure = %s, want 150ms from COAMING_TEST_PROBE_DELAY", got)
	}
	f.Reconfigure(FakeConfig{ProbeDelay: 5 * time.Millisecond})
	if got := f.config().ProbeDelay; got != 5*time.Millisecond {
		t.Fatalf("ProbeDelay after a Reconfigure that sets it = %s, want the configured 5ms", got)
	}
}

func TestNoProbeDelaySweepLeavesReconfigureWithoutADelay(t *testing.T) {
	t.Setenv("COAMING_TEST_PROBE_DELAY", "")
	f := NewFake(t, FakeConfig{})
	f.Reconfigure(FakeConfig{})
	if got := f.config().ProbeDelay; got != 0 {
		t.Fatalf("ProbeDelay = %s, want none without the sweep", got)
	}
}

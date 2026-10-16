// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package testsupport

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// helperEnv names the case the child process runs. A test that needs to see a
// helper fail runs the test binary again with it set, because a failing helper
// ends its test and the parent cannot observe that from inside.
const helperEnv = "TESTSUPPORT_HELPER_CASE"

// TestHelperProcess is the child side of runFailing. Without the marker it does
// nothing, so a normal run passes it.
func TestHelperProcess(t *testing.T) {
	switch os.Getenv(helperEnv) {
	case "":
		return
	case "await-within":
		AwaitWithin(t, 50*time.Millisecond, "the answer", make(chan int))
	}
}

// runFailing runs the named helper case in a child process and returns its
// combined output and whether it exited non-zero.
func runFailing(t *testing.T, name string, env ...string) (string, bool) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.v") //nolint:gosec // re-runs this test binary
	cmd.Env = append(os.Environ(), append([]string{helperEnv + "=" + name}, env...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err != nil
}

func TestAwaitReturnsTheValueSentOnTheChannel(t *testing.T) {
	t.Parallel()
	ch := make(chan string, 1)
	ch <- "ready"
	if got := Await(t, "a value", ch); got != "ready" {
		t.Fatalf("Await = %q, want %q", got, "ready")
	}
}

func TestAwaitReturnsTheZeroValueOfAClosedChannel(t *testing.T) {
	t.Parallel()
	ch := make(chan int)
	close(ch)
	if got := Await(t, "a close", ch); got != 0 {
		t.Fatalf("Await on a closed channel = %d, want 0", got)
	}
}

func TestContextEndsWhenTheTestFinishes(t *testing.T) {
	t.Parallel()
	var ctx context.Context
	t.Run("holder", func(t *testing.T) {
		ctx = Context(t)
		if err := ctx.Err(); err != nil {
			t.Fatalf("context is already done inside the test: %v", err)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("context has no deadline")
		}
	})
	if ctx.Err() == nil {
		t.Fatal("context was not cancelled when its test finished")
	}
}

func TestAwaitWithinFailsNamingWhatWasAwaited(t *testing.T) {
	t.Parallel()
	out, failed := runFailing(t, "await-within")
	if !failed {
		t.Fatalf("child exited zero, want non-zero; output:\n%s", out)
	}
	if want := "testsupport: timed out after 50ms waiting for the answer"; !strings.Contains(out, want) {
		t.Fatalf("child output lacks %q:\n%s", want, out)
	}
}

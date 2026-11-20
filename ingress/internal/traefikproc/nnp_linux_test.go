// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package traefikproc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// prGetNoNewPrivs is PR_GET_NO_NEW_PRIVS from <linux/prctl.h>.
const prGetNoNewPrivs = 39

const (
	nnpHelperEnv = "TRAEFIKPROC_NNP_HELPER"
	// exitUnsupported is the helper's exit code for ENOTSUP, which AllThreadsSyscall returns in a cgo binary.
	exitUnsupported = 3
)

// TestNoNewPrivsHelper is the child side of TestSetNoNewPrivs: it sets the flag on itself and reads it back.
// Without the marker it does nothing, so a normal run passes it.
func TestNoNewPrivsHelper(t *testing.T) {
	if os.Getenv(nnpHelperEnv) != "1" {
		return
	}
	if err := SetNoNewPrivs(); err != nil {
		if errors.Is(err, syscall.ENOTSUP) {
			os.Exit(exitUnsupported)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	got, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prGetNoNewPrivs, 0, 0)
	if errno != 0 || got != 1 {
		fmt.Fprintf(os.Stderr, "PR_GET_NO_NEW_PRIVS = %d, errno %v, want 1\n", got, errno)
		os.Exit(1)
	}
	os.Exit(0)
}

// The real prctl, in a child process so the test binary itself is left as it was. A cgo build (which
// `go test -race` is) cannot use AllThreadsSyscall, so the helper reports that and the test skips, exactly as
// the agent's test does; the run without -race proves the real call.
func TestSetNoNewPrivs(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestNoNewPrivsHelper$") //nolint:gosec // re-runs this test binary
	cmd.Env = append(os.Environ(), nnpHelperEnv+"=1")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == exitUnsupported {
		t.Skip("AllThreadsSyscall is unavailable in a cgo build; the image smoke checks the real binary")
	}
	if err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
}

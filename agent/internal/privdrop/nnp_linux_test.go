// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package privdrop

import (
	"errors"
	"syscall"
	"testing"
)

// prGetNoNewPrivs is PR_GET_NO_NEW_PRIVS from <linux/prctl.h>.
const prGetNoNewPrivs = 39

// The real prctl, on the test process itself (harmless: it only forbids this
// test binary from gaining privileges). It proves RESEARCH A9 for a cgo-off
// build; a cgo build, which `go test -race` is, cannot use AllThreadsSyscall,
// and the image smoke script covers the shipped binary instead.
func TestNoNewPrivsSetsTheFlag(t *testing.T) {
	err := noNewPrivs()
	if errors.Is(err, syscall.ENOTSUP) {
		t.Skip("AllThreadsSyscall is unavailable in a cgo build; the image smoke checks the real binary")
	}
	if err != nil {
		t.Fatal(err)
	}
	got, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prGetNoNewPrivs, 0, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	if got != 1 {
		t.Fatalf("PR_GET_NO_NEW_PRIVS = %d, want 1", got)
	}
}

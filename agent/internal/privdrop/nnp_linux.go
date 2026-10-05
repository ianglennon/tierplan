// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package privdrop

import (
	"fmt"
	"syscall"
)

// prSetNoNewPrivs is PR_SET_NO_NEW_PRIVS from <linux/prctl.h>.
const prSetNoNewPrivs = 38

// noNewPrivs stops this process, and everything it starts, from gaining
// privileges through setuid or file-capability executables. The agent sets it
// itself because `docker service create` has no --security-opt flag, so a swarm
// install cannot ask the engine for it (Pitfall 10, D-22).
//
// The flag is per thread and inherited by threads created later, so it must be
// set on every existing thread: AllThreadsSyscall does that. It is available
// only when cgo is off, which is how the release binary is built; with cgo the
// call fails with ENOTSUP and Drop reports it rather than running half
// hardened.
func noNewPrivs() error {
	if _, _, errno := syscall.AllThreadsSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); errno != 0 {
		return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", errno)
	}
	return nil
}

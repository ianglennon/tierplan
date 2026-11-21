// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package traefikproc

import (
	"fmt"
	"syscall"
)

// prSetNoNewPrivs is PR_SET_NO_NEW_PRIVS from <linux/prctl.h>.
const prSetNoNewPrivs = 38

// SetNoNewPrivs stops this process, and everything it starts, from gaining privileges through setuid or
// file-capability executables (D-06). A swarm service cannot pass --security-opt, so the supervisor sets the
// flag itself before it starts Traefik, which inherits it. It costs nothing for a binary that runs as a
// non-root user. The supervisor calls it first, before it starts anything.
//
// The flag is per thread and inherited by threads created later, so it must be set on every existing
// thread: the all-threads form of syscall does that. It is available only when cgo is off, which is how the release binary
// is built; with cgo the call fails with ENOTSUP and the caller must treat that as an error rather than run
// half hardened. This is the agent's privdrop implementation, copied because the modules cannot share
// internal packages.
func SetNoNewPrivs() error {
	if _, _, errno := syscall.AllThreadsSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); errno != 0 {
		return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", errno)
	}
	return nil
}

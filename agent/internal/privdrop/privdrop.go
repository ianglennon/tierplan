// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package privdrop takes the agent from root to UID and GID 1000 (D-21, D-22).
//
// The image has no USER instruction. The binary starts as root so it can read
// a root-only Docker secret (config.Load runs first), then joins the Docker
// socket's group, becomes UID and GID 1000 and sets no-new-privileges. The
// state directory is owned by 1000 in the image already, so, unlike the
// server's internal/privdrop, there is no ownership walk and no CAP_CHOWN.
//
// This is a smaller copy of the server's package, not a shared one: an
// internal package of another module cannot be imported here, and refactoring
// the Phase 1 server code is not worth the risk (RESEARCH Open Question 8).
package privdrop

import (
	"fmt"
	"os"
	"syscall"
)

const (
	// UID and GID are the identity the agent runs as after the drop.
	UID = 1000
	GID = 1000

	// NoGroup is Result.GID when no supplementary group was joined.
	NoGroup = -1
)

// Options carries the inputs and, for tests, the syscalls. A nil function
// defaults to the real one.
type Options struct {
	// SocketPath is the Docker socket whose group is joined. It may not exist.
	SocketPath string
	// DockerGID is TIERPLAN_AGENT_DOCKER_GID and wins over the socket's group.
	DockerGID *int

	Geteuid func() int
	// Stat reports the group that owns the named file.
	Stat      func(string) (gid int, err error)
	Setgroups func([]int) error
	Setgid    func(int) error
	Setuid    func(int) error
	// NoNewPrivs sets PR_SET_NO_NEW_PRIVS for every thread of the process.
	NoNewPrivs func() error
}

// Result reports what Drop did.
type Result struct {
	// Dropped is false when the process was not root and nothing changed.
	Dropped bool
	// GID is the supplementary group joined for the Docker socket, or NoGroup.
	GID int
}

func (o *Options) defaults() {
	if o.Geteuid == nil {
		o.Geteuid = os.Geteuid
	}
	if o.Stat == nil {
		o.Stat = statGID
	}
	if o.Setgroups == nil {
		o.Setgroups = syscall.Setgroups
	}
	if o.Setgid == nil {
		o.Setgid = syscall.Setgid
	}
	if o.Setuid == nil {
		o.Setuid = syscall.Setuid
	}
	if o.NoNewPrivs == nil {
		o.NoNewPrivs = noNewPrivs
	}
}

// Drop does nothing unless the effective user is root. As root it sets the
// supplementary groups to the Docker socket's group (or none), the GID to 1000
// and the UID to 1000, in that order (setuid last, because an unprivileged
// process can no longer change its groups), and then sets no-new-privileges.
// It returns an error, and the caller must exit, if any step fails or the
// process is still root afterwards. Nothing after a failing step runs.
func Drop(o Options) (Result, error) {
	o.defaults()
	if o.Geteuid() != 0 {
		return Result{}, nil
	}

	socketGID := NoGroup
	switch {
	case o.DockerGID != nil:
		socketGID = *o.DockerGID
	case o.SocketPath != "":
		// A missing or unreadable socket is not an error here: the server
		// reports the engine as unreachable itself, and the agent keeps
		// retrying until the socket appears.
		if gid, err := o.Stat(o.SocketPath); err == nil {
			socketGID = gid
		}
	}

	groups := []int{}
	if socketGID != NoGroup {
		groups = []int{socketGID}
	}
	if err := o.Setgroups(groups); err != nil {
		return Result{}, fmt.Errorf("drop privileges: set supplementary groups %v: %w", groups, err)
	}
	if err := o.Setgid(GID); err != nil {
		return Result{}, fmt.Errorf("drop privileges: set gid %d: %w", GID, err)
	}
	if err := o.Setuid(UID); err != nil {
		return Result{}, fmt.Errorf("drop privileges: set uid %d: %w", UID, err)
	}
	if got := o.Geteuid(); got != UID {
		return Result{}, fmt.Errorf("drop privileges: still running as uid %d after setuid(%d)", got, UID)
	}
	if err := o.NoNewPrivs(); err != nil {
		return Result{}, fmt.Errorf("drop privileges: set no-new-privileges: %w", err)
	}
	return Result{Dropped: true, GID: socketGID}, nil
}

// statGID is the real Stat: the group of the file the path names (a symbolic
// link is followed, as the Docker client would).
func statGID(path string) (int, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("stat %s: no group information on this platform", path)
	}
	return int(st.Gid), nil
}

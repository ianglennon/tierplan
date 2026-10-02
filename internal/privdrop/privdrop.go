// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package privdrop takes a Bayplan process that started as root down to the
// unprivileged user the image runs as (D-17).
//
// The image has no USER instruction. The binary starts as root so it can fix
// the ownership of /data (a fresh bind mount is root-owned, Pitfall 9), then
// joins the Docker socket's group and becomes UID and GID 1000. Every
// subcommand does the same, because `docker exec <c> bayplan user unlock`
// also starts as root and would otherwise leave root-owned database files the
// server cannot open (RESEARCH A8).
package privdrop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const (
	// UID and GID are the identity Bayplan runs as after the drop.
	UID = 1000
	GID = 1000

	// NoGroup is Result.GID when no supplementary group was joined.
	NoGroup = -1
)

// subdirs are the data-directory subdirectories whose entries are fixed too.
// A CLI run as root may have written into them.
var subdirs = map[string]bool{"backups": true, "tls": true}

// Options carries the inputs and, for tests, the syscalls. A nil function
// defaults to the real one. On Linux syscall.Setuid and friends apply to every
// thread (Go 1.16 and later, and the build has CGO off).
type Options struct {
	// DataDir is fixed (created if missing, then chowned) before the drop.
	// Empty means the command does not use it.
	DataDir string
	// SocketPath is the Docker socket whose group is joined. It may not exist.
	SocketPath string
	// GIDOverride is BAYPLAN_DOCKER_GID and wins over the socket's group.
	GIDOverride *int

	Geteuid   func() int
	Stat      func(string) (os.FileInfo, error)
	Lchown    func(string, int, int) error
	ReadDir   func(string) ([]os.DirEntry, error)
	MkdirAll  func(string, os.FileMode) error
	Setgroups func([]int) error
	Setgid    func(int) error
	Setuid    func(int) error
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
		o.Stat = os.Stat
	}
	if o.Lchown == nil {
		o.Lchown = os.Lchown
	}
	if o.ReadDir == nil {
		o.ReadDir = os.ReadDir
	}
	if o.MkdirAll == nil {
		o.MkdirAll = os.MkdirAll
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
}

// Drop does nothing unless the effective user is root. As root it fixes the
// data directory's ownership, then sets the supplementary groups to the Docker
// socket's group (or none), the GID to 1000 and the UID to 1000, in that order:
// setuid last, because an unprivileged process can no longer change its groups.
// It returns an error, and the caller must exit, if any step fails or the
// process is still root afterwards.
func Drop(o Options) (Result, error) {
	o.defaults()
	if o.Geteuid() != 0 {
		return Result{}, nil
	}

	socketGID := NoGroup
	switch {
	case o.GIDOverride != nil:
		socketGID = *o.GIDOverride
	case o.SocketPath != "":
		// A missing or unreadable socket is not an error here: the server
		// reports "Docker is not reachable" itself, with the path.
		if fi, err := o.Stat(o.SocketPath); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok {
				socketGID = int(st.Gid)
			}
		}
	}

	if o.DataDir != "" {
		if err := fixOwnership(o); err != nil {
			return Result{}, err
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
	return Result{Dropped: true, GID: socketGID}, nil
}

// fixOwnership creates the data directory when it is missing and gives
// UID:GID 1000 to it, to its direct entries and to the entries of backups/
// and tls/. Entries that are already right are not touched. Symbolic links are
// never followed.
func fixOwnership(o Options) error {
	fi, err := o.Stat(o.DataDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := o.MkdirAll(o.DataDir, 0o750); err != nil {
			return fmt.Errorf("create the data directory %s: %w", o.DataDir, err)
		}
		if fi, err = o.Stat(o.DataDir); err != nil {
			return fmt.Errorf("data directory %s: %w", o.DataDir, err)
		}
	case err != nil:
		return fmt.Errorf("data directory %s: %w", o.DataDir, err)
	}
	if err := chownIfNeeded(o, o.DataDir, fi); err != nil {
		return err
	}

	entries, err := o.ReadDir(o.DataDir)
	if err != nil {
		return fmt.Errorf("read the data directory %s: %w", o.DataDir, err)
	}
	for _, e := range entries {
		path := filepath.Join(o.DataDir, e.Name())
		info, err := e.Info()
		if err != nil {
			return fmt.Errorf("inspect %s: %w", path, err)
		}
		if err := chownIfNeeded(o, path, info); err != nil {
			return err
		}
		if !e.IsDir() || !subdirs[e.Name()] {
			continue
		}
		inner, err := o.ReadDir(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		for _, ie := range inner {
			innerPath := filepath.Join(path, ie.Name())
			innerInfo, err := ie.Info()
			if err != nil {
				return fmt.Errorf("inspect %s: %w", innerPath, err)
			}
			if err := chownIfNeeded(o, innerPath, innerInfo); err != nil {
				return err
			}
		}
	}
	return nil
}

func chownIfNeeded(o Options, path string, fi os.FileInfo) error {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid == UID && st.Gid == GID {
		return nil
	}
	if err := o.Lchown(path, UID, GID); err != nil {
		return fmt.Errorf("give %s to %d:%d: %w", path, UID, GID, err)
	}
	return nil
}

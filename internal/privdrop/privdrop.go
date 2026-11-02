// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package privdrop takes a TierPlan process that started as root down to the
// unprivileged user the image runs as (D-17).
//
// The image has no USER instruction. The binary starts as root so it can fix
// the ownership of /data (a fresh bind mount is root-owned, Pitfall 9), then
// joins the Docker socket's group and becomes UID and GID 1000. Every
// subcommand does the same, because `docker exec <c> tierplan user unlock`
// also starts as root and would otherwise leave root-owned database files the
// server cannot open (RESEARCH A8).
package privdrop

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"syscall"
)

const (
	// UID and GID are the identity TierPlan runs as after the drop.
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
	// GIDOverride is TIERPLAN_DOCKER_GID and wins over the socket's group.
	GIDOverride *int

	Geteuid func() int
	// Stat is used to find the Docker socket's group only.
	Stat     func(string) (os.FileInfo, error)
	MkdirAll func(string, os.FileMode) error
	// Chown gives name, relative to the data directory, to uid:gid without
	// following a final symbolic link. r is the os.Root of the data directory,
	// so the path can never leave it. "." is the data directory itself.
	Chown func(r *os.Root, name string, uid, gid int) error
	// Owner reports who owns the entry that fi describes; name is relative to
	// the data directory. ok is false when the owner cannot be read, which is
	// treated as "needs fixing".
	Owner func(name string, fi fs.FileInfo) (uid, gid uint32, ok bool)
	// Umask sets the file creation mask and returns the old one.
	Umask     func(int) int
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
	if o.MkdirAll == nil {
		o.MkdirAll = os.MkdirAll
	}
	if o.Chown == nil {
		o.Chown = chownInRoot
	}
	if o.Owner == nil {
		o.Owner = ownerOf
	}
	if o.Umask == nil {
		o.Umask = syscall.Umask
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
// data directory's ownership, sets umask 077, then sets the supplementary groups to the Docker
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

	// Every file the process creates after the drop (the database and its
	// SQLite sidecars, backups, the key file) is private to UID 1000, whatever
	// umask the container runtime started it with (WR-07).
	o.Umask(0o077)

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
// and tls/. Entries that are already right are not touched.
//
// The walk runs as root inside a directory the unprivileged server can write
// to, so it goes through an os.Root: no path it handles can leave the data
// directory, an entry's type is read again at the moment the walk decides to
// descend into it, and a symbolic link is chowned itself and never entered
// (WR-08, T-01-108). A data directory that is itself a symbolic link is
// resolved first, so its real target is the one that is fixed.
func fixOwnership(o Options) error {
	dir, err := filepath.EvalSymlinks(o.DataDir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := o.MkdirAll(o.DataDir, 0o750); err != nil {
			return fmt.Errorf("create the data directory %s: %w", o.DataDir, err)
		}
		dir, err = filepath.EvalSymlinks(o.DataDir)
	}
	if err != nil {
		return fmt.Errorf("data directory %s: %w", o.DataDir, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open the data directory %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	if err := chownEntry(o, root, ".", dir); err != nil {
		return err
	}
	return walk(o, root, ".", dir, true)
}

// walk fixes the entries of rel (a name relative to the data directory). When
// descend is true, entries that really are the backups or tls directories are
// walked one level further.
func walk(o Options, root *os.Root, rel, dir string, descend bool) error {
	names, err := listDir(root, rel)
	if err != nil {
		return fmt.Errorf("read %s: %w", filepath.Join(dir, rel), err)
	}
	for _, n := range names {
		name := path.Join(rel, n)
		full := filepath.Join(dir, filepath.FromSlash(name))
		// Re-read the type now, not from the listing: a directory the
		// server swapped for a link since then is a link here.
		fi, err := root.Lstat(name)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", full, err)
		}
		if err := chownIfNeeded(o, root, name, full, fi); err != nil {
			return err
		}
		if descend && fi.IsDir() && rel == "." && subdirs[n] {
			if err := walk(o, root, name, dir, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// listDir returns the names inside rel without following a link out of root.
func listDir(root *os.Root, rel string) ([]string, error) {
	sub, err := root.OpenRoot(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sub.Close() }()
	f, err := sub.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

// chownEntry reads name's owner through the root and fixes it when needed.
func chownEntry(o Options, root *os.Root, name, full string) error {
	fi, err := root.Lstat(name)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", full, err)
	}
	return chownIfNeeded(o, root, name, full, fi)
}

func chownIfNeeded(o Options, root *os.Root, name, full string, fi fs.FileInfo) error {
	if uid, gid, ok := o.Owner(name, fi); ok && uid == UID && gid == GID {
		return nil
	}
	if err := o.Chown(root, name, UID, GID); err != nil {
		return fmt.Errorf("give %s to %d:%d: %w", full, UID, GID, err)
	}
	return nil
}

// ownerOf reads the owner from the file system's own record.
func ownerOf(_ string, fi fs.FileInfo) (uid, gid uint32, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return st.Uid, st.Gid, true
}

// chownInRoot is the real Chown: Lchown through the root, so a final link is
// changed itself. The data directory ("."), which a root cannot name for
// Lchown, is changed through a descriptor opened inside the root.
func chownInRoot(r *os.Root, name string, uid, gid int) error {
	if name != "." {
		return r.Lchown(name, uid, gid)
	}
	f, err := r.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Chown(uid, gid)
}

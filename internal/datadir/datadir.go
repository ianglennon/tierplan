// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

// Package datadir prepares the /data directory and reports whether what is
// written there will survive the container (D-15, RESEARCH Pitfall 10).
package datadir

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/moby/sys/mountinfo"
)

// Info describes a prepared data directory.
type Info struct {
	Path string
	// Persisted is false when the directory is probably lost with the container.
	Persisted bool
	// Reason says why Persisted is false: "not a mount point", or the file
	// system type ("tmpfs", "ramfs"). Empty when Persisted.
	Reason string
}

// Options injects the two environment probes so tests need no real container.
type Options struct {
	// InContainer reports whether the process runs in a container. Default:
	// /.dockerenv exists.
	InContainer func() bool
	// LookupMount returns the mount entry whose mount point is exactly path, or
	// nil when there is none. Default: moby/sys/mountinfo.
	LookupMount func(path string) (*mountinfo.Info, error)
	// Chmod narrows the directory mode. Default: os.Chmod.
	Chmod func(string, os.FileMode) error
}

// Prepare creates path (mode 0750) if it is missing, proves it is writable and
// classifies it. Outside a container the directory is always "persisted".
// Inside one, a path that is not a mount point, or sits on tmpfs or ramfs, is
// reported as not persisted; that is a warning for the caller, not an error.
//
// Anonymous Docker volumes look like named volumes from inside the container,
// so they report persisted. The README tells operators to use a named volume
// or a bind mount.
func Prepare(path string, opts Options) (Info, error) {
	if opts.InContainer == nil {
		opts.InContainer = inDockerContainer
	}
	if opts.LookupMount == nil {
		opts.LookupMount = lookupMount
	}
	if opts.Chmod == nil {
		opts.Chmod = os.Chmod
	}

	if err := os.MkdirAll(path, 0o750); err != nil {
		return Info{}, fmt.Errorf("create data directory %s: %w", path, err)
	}
	if err := CheckWritable(path); err != nil {
		return Info{}, err
	}

	narrow(path, opts.Chmod)

	info := Info{Path: path, Persisted: true}
	if !opts.InContainer() {
		return info, nil
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved = path
	}
	m, err := opts.LookupMount(resolved)
	if err != nil {
		return Info{}, fmt.Errorf("inspect mounts for %s: %w", path, err)
	}
	switch {
	case m == nil:
		info.Persisted, info.Reason = false, "not a mount point"
	case m.FSType == "tmpfs" || m.FSType == "ramfs":
		info.Persisted, info.Reason = false, m.FSType
	}
	return info, nil
}

// narrow removes group-write and every other-permission bit from the directory
// (0755 and 0777 become 0750, 0700 is untouched), so local users on the host
// cannot list or read what Coaming keeps there. When the mode cannot be changed
// (a file system without chmod, a directory owned by someone else) Coaming
// starts anyway and warns, because refusing to start would turn a hardening
// step into an outage.
func narrow(path string, chmod func(string, os.FileMode) error) {
	st, err := os.Stat(path)
	if err != nil {
		return
	}
	perm := st.Mode().Perm()
	if perm&0o027 == 0 {
		return
	}
	if err := chmod(path, perm&^0o027); err != nil {
		slog.Warn("data directory is readable beyond its owner and group; run chmod 750 on it",
			"path", path, "mode", fmt.Sprintf("%04o", perm), "error", err)
	}
}

// CheckWritable proves the directory accepts a new file by creating and
// removing one. It is also the readiness probe for /readyz.
func CheckWritable(path string) error {
	f, err := os.CreateTemp(path, ".coaming-write-check-*")
	if err != nil {
		return fmt.Errorf("data directory %s is not writable: %w", path, err)
	}
	name := f.Name()
	closeErr := f.Close()
	rmErr := os.Remove(name)
	if closeErr != nil {
		return fmt.Errorf("data directory %s is not writable: %w", path, closeErr)
	}
	if rmErr != nil {
		return fmt.Errorf("data directory %s is not writable: %w", path, rmErr)
	}
	return nil
}

func inDockerContainer() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

// lookupMount returns the topmost mount at exactly path. A missing
// /proc/self/mountinfo (non-Linux) means "no entry", not a failure.
func lookupMount(path string) (*mountinfo.Info, error) {
	mounts, err := mountinfo.GetMounts(mountinfo.SingleEntryFilter(path))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(mounts) == 0 {
		return nil, nil
	}
	return mounts[len(mounts)-1], nil
}

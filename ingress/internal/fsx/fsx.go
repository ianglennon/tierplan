// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package fsx is the supervisor's only door to the disk. It is an interface so a test can record every
// operation, fail the n-th one (a process killed at that instant) and inject a cross-device rename, which
// is how the cache's crash safety is proved without killing a process (D-31).
package fsx

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// TempPattern is the pattern of every temporary file: a name that ends in none of .yml, .yaml or .toml, so
// a temporary file left behind is never parsed as Traefik configuration (S1 O13).
const TempPattern = ".part-*"

// File is a file being written.
type File interface {
	Name() string
	Write(p []byte) (int, error)
	Chmod(mode fs.FileMode) error
	Sync() error
	Close() error
}

// FS is the set of disk operations the supervisor uses.
type FS interface {
	MkdirAll(path string, perm fs.FileMode) error
	// CreateTemp makes a new file in dir, named by pattern as os.CreateTemp does, open for writing.
	CreateTemp(dir, pattern string) (File, error)
	// Rename moves oldpath to newpath. Across devices it fails (EXDEV): nothing copies a file instead.
	Rename(oldpath, newpath string) error
	Remove(name string) error
	ReadFile(name string) ([]byte, error)
	ReadDir(name string) ([]fs.DirEntry, error)
	// SyncDir flushes the directory itself, so a rename in it survives a power loss.
	SyncDir(dir string) error
}

type osFS struct{}

// OS is the real filesystem.
func OS() FS { return osFS{} }

func (osFS) MkdirAll(path string, perm fs.FileMode) error { return os.MkdirAll(path, perm) }

func (osFS) CreateTemp(dir, pattern string) (File, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (osFS) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

func (osFS) Remove(name string) error { return os.Remove(name) }

func (osFS) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) } //nolint:gosec // G304: fsx is the supervisor's one door to its own volume; every caller passes a path under the node root

func (osFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }

// SyncDir opens the directory read-only and flushes it.
func (osFS) SyncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // dir is a path the supervisor built under its own root
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

// WriteAtomicNoSync writes data to a temporary file in tmpDir (which must be on the same filesystem as
// dst), flushes it, sets mode exactly (os.CreateTemp makes 0600, and the umask must not matter) and
// renames it to dst. It does not flush dst's directory: a caller that writes several files flushes the
// directory once after the last. A failure removes the temporary file; the error wraps the cause, so an
// EXDEV from the rename can be found with errors.Is.
func WriteAtomicNoSync(fsys FS, tmpDir, dst string, data []byte, mode fs.FileMode) error {
	f, err := fsys.CreateTemp(tmpDir, TempPattern)
	if err != nil {
		return fmt.Errorf("fsx: create a temporary file in %s: %w", tmpDir, err)
	}
	name := f.Name()
	renamed := false
	defer func() {
		// Once the rename has happened the name is gone and the file is dst: nothing to remove.
		if !renamed {
			_ = fsys.Remove(name)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsx: write %s: %w", name, err)
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsx: set the mode of %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsx: flush %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("fsx: close %s: %w", name, err)
	}
	if err := fsys.Rename(name, dst); err != nil {
		return fmt.Errorf("fsx: rename %s to %s: %w", name, dst, err)
	}
	renamed = true
	return nil
}

// WriteAtomic is WriteAtomicNoSync followed by a flush of dst's directory, so the rename itself is
// durable when it returns.
func WriteAtomic(fsys FS, tmpDir, dst string, data []byte, mode fs.FileMode) error {
	if err := WriteAtomicNoSync(fsys, tmpDir, dst, data, mode); err != nil {
		return err
	}
	if err := fsys.SyncDir(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("fsx: flush %s: %w", filepath.Dir(dst), err)
	}
	return nil
}

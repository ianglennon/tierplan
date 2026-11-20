// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package fsxtest is a filesystem for tests of code that must survive a kill at any instant: it records
// every operation that changes the disk, fails the n-th one and all after it (a process that was killed
// does nothing more), and injects a failure into the rename to one chosen path.
package fsxtest

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ianglennon/tierplan/ingress/internal/fsx"
)

// ErrKilled is what an operation returns once FailFrom has stopped the filesystem.
var ErrKilled = errors.New("fsxtest: the process was killed here")

// Recorder wraps a real filesystem. Ops lists the operations that changed the disk and succeeded, in
// order, as "<verb> <path>" or "rename <old> -> <new>", with paths relative to the root the Recorder was
// given and every temporary file named "<dir>/.part-*", so a test compares sequences without random
// names. Reads (ReadFile, ReadDir) are neither recorded nor failed by FailFrom.
type Recorder struct {
	inner fsx.FS
	root  string

	mu       sync.Mutex
	ops      []string
	count    int
	failFrom int
	renames  map[string]error
}

// New returns a Recorder over inner. Paths in Ops are relative to root.
func New(inner fsx.FS, root string) *Recorder {
	return &Recorder{inner: inner, root: root, failFrom: -1, renames: map[string]error{}}
}

// FailFrom makes the n-th mutating operation from now (counting from 0) and every later one return
// ErrKilled without touching the disk. A negative n turns it off.
func (r *Recorder) FailFrom(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count = 0
	r.failFrom = n
}

// RenameErr makes a rename whose destination is dst (slash form, relative to the root) fail with err and
// change nothing. A nil err removes the rule.
func (r *Recorder) RenameErr(dst string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		delete(r.renames, dst)
		return
	}
	r.renames[dst] = err
}

// Ops returns a copy of the operations performed so far.
func (r *Recorder) Ops() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ops...)
}

// rel names a path relative to the root, with a temporary file's random part replaced.
func (r *Recorder) rel(p string) string {
	if rel, err := filepath.Rel(r.root, p); err == nil && !strings.HasPrefix(rel, "..") {
		p = rel
	}
	p = filepath.ToSlash(p)
	dir, base := filepath.Split(p)
	if strings.HasPrefix(base, ".part-") {
		base = ".part-*"
	}
	return dir + base
}

// mutate runs one operation that changes the disk: it is counted, failed when the kill point has been
// reached, and recorded when it succeeds.
func (r *Recorder) mutate(desc string, op func() error) error {
	r.mu.Lock()
	idx := r.count
	r.count++
	killed := r.failFrom >= 0 && idx >= r.failFrom
	r.mu.Unlock()
	if killed {
		return ErrKilled
	}
	if err := op(); err != nil {
		return err
	}
	r.mu.Lock()
	r.ops = append(r.ops, desc)
	r.mu.Unlock()
	return nil
}

func (r *Recorder) MkdirAll(path string, perm fs.FileMode) error {
	return r.mutate("mkdir "+r.rel(path), func() error { return r.inner.MkdirAll(path, perm) })
}

func (r *Recorder) CreateTemp(dir, pattern string) (fsx.File, error) {
	var f fsx.File
	err := r.mutate("create "+r.rel(filepath.Join(dir, ".part-x")), func() error {
		var err error
		f, err = r.inner.CreateTemp(dir, pattern)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &file{r: r, inner: f}, nil
}

func (r *Recorder) Rename(oldpath, newpath string) error {
	r.mu.Lock()
	injected := r.renames[filepath.ToSlash(r.rel(newpath))]
	r.mu.Unlock()
	if injected != nil {
		return injected
	}
	return r.mutate("rename "+r.rel(oldpath)+" -> "+r.rel(newpath), func() error { return r.inner.Rename(oldpath, newpath) })
}

func (r *Recorder) Remove(name string) error {
	return r.mutate("remove "+r.rel(name), func() error { return r.inner.Remove(name) })
}

func (r *Recorder) SyncDir(dir string) error {
	return r.mutate("syncdir "+r.rel(dir), func() error { return r.inner.SyncDir(dir) })
}

func (r *Recorder) ReadFile(name string) ([]byte, error) { return r.inner.ReadFile(name) }

func (r *Recorder) ReadDir(name string) ([]fs.DirEntry, error) { return r.inner.ReadDir(name) }

// file records the operations on one open file.
type file struct {
	r     *Recorder
	inner fsx.File
}

func (f *file) Name() string { return f.inner.Name() }

func (f *file) desc(verb string) string { return verb + " " + f.r.rel(f.inner.Name()) }

func (f *file) Write(p []byte) (int, error) {
	n := 0
	err := f.r.mutate(f.desc("write"), func() error {
		var err error
		n, err = f.inner.Write(p)
		return err
	})
	return n, err
}

func (f *file) Chmod(mode fs.FileMode) error {
	return f.r.mutate(f.desc("chmod"), func() error { return f.inner.Chmod(mode) })
}

func (f *file) Sync() error {
	return f.r.mutate(f.desc("sync"), func() error { return f.inner.Sync() })
}

// Close always closes the real file, so a test does not leak a descriptor, and is recorded and failed like
// any other operation.
func (f *file) Close() error {
	err := f.r.mutate(f.desc("close"), func() error { return nil })
	closeErr := f.inner.Close()
	if err != nil {
		return err
	}
	return closeErr
}

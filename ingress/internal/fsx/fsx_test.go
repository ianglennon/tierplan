// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package fsx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// spy is the real filesystem with the SyncDir calls counted and the rename optionally failed.
type spy struct {
	FS
	synced    []string
	renameErr error
}

func (s *spy) SyncDir(dir string) error {
	s.synced = append(s.synced, dir)
	return s.FS.SyncDir(dir)
}

func (s *spy) Rename(oldpath, newpath string) error {
	if s.renameErr != nil {
		return s.renameErr
	}
	return s.FS.Rename(oldpath, newpath)
}

func layout(t *testing.T) (tmp, dst string) {
	t.Helper()
	root := t.TempDir()
	tmp = filepath.Join(root, "tmp")
	dstDir := filepath.Join(root, "out")
	for _, d := range []string{tmp, dstDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("make %s: %v", d, err)
		}
	}
	return tmp, filepath.Join(dstDir, "file")
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name())
	}
	return names
}

func TestWriteAtomicModes(t *testing.T) {
	for _, mode := range []fs.FileMode{0o600, 0o644} {
		t.Run(mode.String(), func(t *testing.T) {
			tmp, dst := layout(t)
			if err := WriteAtomic(OS(), tmp, dst, []byte("data"), mode); err != nil {
				t.Fatalf("WriteAtomic: %v", err)
			}
			st, err := os.Stat(dst)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if st.Mode().Perm() != mode {
				t.Fatalf("mode = %v, want %v", st.Mode().Perm(), mode)
			}
			got, err := os.ReadFile(dst)
			if err != nil || string(got) != "data" {
				t.Fatalf("content = %q, %v", got, err)
			}
		})
	}
}

func TestWriteAtomicLeavesNoTempFile(t *testing.T) {
	tmp, dst := layout(t)
	if err := WriteAtomic(OS(), tmp, dst, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	if left := entries(t, tmp); len(left) != 0 {
		t.Fatalf("temp dir holds %v after a write", left)
	}
}

func TestWriteAtomicReplacesInPlace(t *testing.T) {
	tmp, dst := layout(t)
	for _, body := range []string{"one", "two"} {
		if err := WriteAtomic(OS(), tmp, dst, []byte(body), 0o644); err != nil {
			t.Fatalf("WriteAtomic %s: %v", body, err)
		}
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "two" {
		t.Fatalf("content = %q, want the second write", got)
	}
}

func TestWriteAtomicSyncsTheDestinationDirectory(t *testing.T) {
	tmp, dst := layout(t)
	s := &spy{FS: OS()}
	if err := WriteAtomic(s, tmp, dst, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	if len(s.synced) != 1 || s.synced[0] != filepath.Dir(dst) {
		t.Fatalf("synced %v, want exactly %s", s.synced, filepath.Dir(dst))
	}

	s = &spy{FS: OS()}
	if err := WriteAtomicNoSync(s, tmp, dst, []byte("y"), 0o644); err != nil {
		t.Fatalf("WriteAtomicNoSync: %v", err)
	}
	if len(s.synced) != 0 {
		t.Fatalf("WriteAtomicNoSync flushed %v", s.synced)
	}
}

func TestWriteAtomicFailedRenameKeepsDestinationAndRemovesTemp(t *testing.T) {
	tmp, dst := layout(t)
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := &spy{FS: OS(), renameErr: syscall.EXDEV}
	err := WriteAtomic(s, tmp, dst, []byte("new"), 0o644)
	if !errors.Is(err, syscall.EXDEV) {
		t.Fatalf("error = %v, want one wrapping EXDEV", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "old" {
		t.Fatalf("destination = %q, want the old content", got)
	}
	if left := entries(t, tmp); len(left) != 0 {
		t.Fatalf("temp dir holds %v after a failed write", left)
	}
}

func TestTempNamesNeverEndInConfigurationSuffixes(t *testing.T) {
	tmp, _ := layout(t)
	f, err := OS().CreateTemp(tmp, TempPattern)
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer func() { _ = f.Close() }()
	base := filepath.Base(f.Name())
	if !strings.HasPrefix(base, ".part-") {
		t.Fatalf("temp name %q does not start with .part-", base)
	}
	for _, suffix := range []string{".yml", ".yaml", ".toml"} {
		if strings.HasSuffix(base, suffix) {
			t.Fatalf("temp name %q ends in %s", base, suffix)
		}
	}
}

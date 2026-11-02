// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package datadir_test

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/sys/mountinfo"

	"github.com/ianglennon/tierplan/internal/datadir"
)

func opts(inContainer bool, m *mountinfo.Info) datadir.Options {
	return datadir.Options{
		InContainer: func() bool { return inContainer },
		LookupMount: func(string) (*mountinfo.Info, error) { return m, nil },
	}
}

func TestPrepareCreatesDirectoryAndOutsideContainerIsPersisted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	info, err := datadir.Prepare(dir, opts(false, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Persisted || info.Reason != "" {
		t.Errorf("outside a container: %+v, want persisted", info)
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o750 {
		t.Errorf("mode = %o, want 750", perm)
	}
	left, _ := filepath.Glob(filepath.Join(dir, ".tierplan-*"))
	if len(left) != 0 {
		t.Errorf("writability probe left files behind: %v", left)
	}
}

func TestPrepareInContainerWithoutMountIsNotPersisted(t *testing.T) {
	info, err := datadir.Prepare(t.TempDir(), opts(true, nil))
	if err != nil {
		t.Fatal(err)
	}
	if info.Persisted || info.Reason != "not a mount point" {
		t.Errorf("got %+v, want not persisted: not a mount point", info)
	}
}

func TestPrepareInContainerOnTmpfsIsNotPersisted(t *testing.T) {
	for _, fstype := range []string{"tmpfs", "ramfs"} {
		info, err := datadir.Prepare(t.TempDir(), opts(true, &mountinfo.Info{FSType: fstype, Mountpoint: "/data"}))
		if err != nil {
			t.Fatal(err)
		}
		if info.Persisted || info.Reason != fstype {
			t.Errorf("%s: got %+v, want not persisted with reason %s", fstype, info, fstype)
		}
	}
}

func TestPrepareInContainerOnRealMountIsPersisted(t *testing.T) {
	info, err := datadir.Prepare(t.TempDir(), opts(true, &mountinfo.Info{FSType: "ext4", Mountpoint: "/data"}))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Persisted || info.Reason != "" {
		t.Errorf("got %+v, want persisted", info)
	}
}

func TestPrepareReadOnlyDirectoryFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // a directory needs the search bit to be removed
	if _, err := datadir.Prepare(dir, opts(false, nil)); err == nil {
		t.Fatal("expected an error for a read-only directory")
	}
	if err := datadir.CheckWritable(dir); err == nil {
		t.Fatal("CheckWritable should fail for a read-only directory")
	}
}

func TestPreparePathThatIsAFileFails(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := datadir.Prepare(f, opts(false, nil)); err == nil {
		t.Fatal("expected an error when the path is a file")
	}
}

func TestDefaultsRunOutsideAContainerWithoutPanicking(t *testing.T) {
	// Zero Options use the real /.dockerenv and mountinfo lookups.
	if _, err := datadir.Prepare(t.TempDir(), datadir.Options{}); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareNarrowsABroadDataDirectory(t *testing.T) {
	for _, tc := range []struct{ from, want os.FileMode }{
		{0o755, 0o750}, {0o777, 0o750}, {0o700, 0o700},
	} {
		dir := t.TempDir()
		if err := os.Chmod(dir, tc.from); err != nil { //nolint:gosec // a broad directory is the case under test
			t.Fatal(err)
		}
		if _, err := datadir.Prepare(dir, opts(false, nil)); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm != tc.want {
			t.Errorf("%o became %o, want %o", tc.from, perm, tc.want)
		}
	}
}

func TestPrepareWarnsAndContinuesWhenItCannotNarrowTheMode(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // a broad directory is the case under test
		t.Fatal(err)
	}
	o := opts(false, nil)
	o.Chmod = func(string, os.FileMode) error { return errors.New("operation not permitted") }
	info, err := datadir.Prepare(dir, o)
	if err != nil {
		t.Fatalf("Prepare must continue, got %v", err)
	}
	if !info.Persisted {
		t.Errorf("info = %+v, want persisted", info)
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, dir) || !strings.Contains(out, "755") {
		t.Errorf("warning must name the path and the mode, got %q", out)
	}
}

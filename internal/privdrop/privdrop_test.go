// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package privdrop

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeInfo is a FileInfo whose Sys() reports an owner, so the ownership logic
// runs without root.
type fakeInfo struct {
	name string
	dir  bool
	uid  uint32
	gid  uint32
}

func (f fakeInfo) Name() string { return f.name }
func (f fakeInfo) Size() int64  { return 0 }
func (f fakeInfo) Mode() fs.FileMode {
	if f.dir {
		return fs.ModeDir | 0o750
	}
	return 0o600
}
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.dir }
func (f fakeInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid, Gid: f.gid} }

// rig records every privileged call, in order, and serves a fake file tree.
type rig struct {
	t     *testing.T
	euid  int
	calls []string
	// tree maps a path to its owner; dirs lists the directories among them.
	tree map[string]fakeInfo
	// children maps a directory to the names inside it.
	children map[string][]string
	chowns   []string

	setuidErr error
}

func newRig(t *testing.T) *rig {
	return &rig{t: t, euid: 0, tree: map[string]fakeInfo{}, children: map[string][]string{}}
}

func (r *rig) add(path string, uid, gid uint32, dir bool) {
	r.tree[path] = fakeInfo{name: filepath.Base(path), dir: dir, uid: uid, gid: gid}
	if parent := filepath.Dir(path); parent != path {
		if _, known := r.tree[parent]; known {
			r.children[parent] = append(r.children[parent], filepath.Base(path))
		}
	}
}

func (r *rig) options(dataDir, socket string) Options {
	return Options{
		DataDir:    dataDir,
		SocketPath: socket,
		Geteuid:    func() int { return r.euid },
		Stat: func(p string) (os.FileInfo, error) {
			if fi, ok := r.tree[p]; ok {
				return fi, nil
			}
			return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
		},
		Lchown: func(p string, uid, gid int) error {
			r.chowns = append(r.chowns, fmt.Sprintf("%s=%d:%d", p, uid, gid))
			return nil
		},
		ReadDir: func(p string) ([]os.DirEntry, error) {
			names, ok := r.children[p]
			if !ok {
				if _, exists := r.tree[p]; exists {
					return nil, nil
				}
				return nil, &fs.PathError{Op: "readdir", Path: p, Err: fs.ErrNotExist}
			}
			var out []os.DirEntry
			for _, n := range names {
				out = append(out, fs.FileInfoToDirEntry(r.tree[filepath.Join(p, n)]))
			}
			return out, nil
		},
		MkdirAll: func(p string, _ os.FileMode) error {
			r.calls = append(r.calls, "mkdir "+p)
			r.add(p, 0, 0, true)
			return nil
		},
		Setgroups: func(g []int) error {
			r.calls = append(r.calls, fmt.Sprintf("setgroups %v", g))
			return nil
		},
		Setgid: func(g int) error {
			r.calls = append(r.calls, fmt.Sprintf("setgid %d", g))
			return nil
		},
		Setuid: func(u int) error {
			r.calls = append(r.calls, fmt.Sprintf("setuid %d", u))
			if r.setuidErr != nil {
				return r.setuidErr
			}
			r.euid = u
			return nil
		},
	}
}

func TestDropJoinsTheSocketGroupThenSetsGIDThenUID(t *testing.T) {
	r := newRig(t)
	r.add("/data", 1000, 1000, true)
	r.add("/run/docker.sock", 0, 998, false)

	res, err := Drop(r.options("/data", "/run/docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"setgroups [998]", "setgid 1000", "setuid 1000"}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("privileged calls = %v, want %v", r.calls, want)
	}
	if !res.Dropped || res.GID != 998 {
		t.Fatalf("result = %+v, want Dropped with GID 998", res)
	}
}

func TestDropUsesTheOverrideGIDInsteadOfTheSocketGroup(t *testing.T) {
	r := newRig(t)
	r.add("/data", 1000, 1000, true)
	r.add("/run/docker.sock", 0, 998, false)
	o := r.options("/data", "/run/docker.sock")
	gid := 4242
	o.GIDOverride = &gid

	res, err := Drop(o)
	if err != nil {
		t.Fatal(err)
	}
	if r.calls[0] != "setgroups [4242]" || res.GID != 4242 {
		t.Fatalf("calls = %v, result = %+v; want group 4242", r.calls, res)
	}
}

func TestDropWithNoSocketJoinsNoSupplementaryGroup(t *testing.T) {
	r := newRig(t)
	r.add("/data", 1000, 1000, true)

	res, err := Drop(r.options("/data", "/run/docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"setgroups []", "setgid 1000", "setuid 1000"}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("privileged calls = %v, want %v", r.calls, want)
	}
	if !res.Dropped || res.GID != NoGroup {
		t.Fatalf("result = %+v, want Dropped with GID NoGroup", res)
	}
}

func TestDropChownsRootOwnedDataEntriesAndLeavesOwnedOnesAlone(t *testing.T) {
	r := newRig(t)
	r.add("/data", 0, 0, true) // a fresh bind mount starts root-owned (Pitfall 9)
	r.add("/data/bayplan.db", 0, 0, false)
	r.add("/data/bayplan.db-wal", 0, 0, false) // left behind by a CLI run as root (A8)
	r.add("/data/bayplan.key", 1000, 1000, false)
	r.add("/data/setup-token", 0, 0, false)
	r.add("/data/backups", 0, 0, true)
	r.add("/data/backups/pre-0003.db", 0, 0, false)
	r.add("/data/tls", 1000, 1000, true)
	r.add("/data/tls/selfsigned.key", 0, 0, false)

	if _, err := Drop(r.options("/data", "")); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/data/backups/pre-0003.db=1000:1000",
		"/data/backups=1000:1000",
		"/data/bayplan.db-wal=1000:1000",
		"/data/bayplan.db=1000:1000",
		"/data/setup-token=1000:1000",
		"/data/tls/selfsigned.key=1000:1000",
		"/data=1000:1000",
	}
	got := append([]string(nil), r.chowns...)
	sortStrings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chowns =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Ownership is fixed while still root, so before the first identity change.
	if r.calls[0] != "setgroups []" {
		t.Fatalf("calls = %v", r.calls)
	}
}

func TestDropCreatesAMissingDataDirectoryBeforeChowningIt(t *testing.T) {
	r := newRig(t)
	if _, err := Drop(r.options("/data", "")); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) == 0 || r.calls[0] != "mkdir /data" {
		t.Fatalf("calls = %v, want the data directory created first", r.calls)
	}
	if !reflect.DeepEqual(r.chowns, []string{"/data=1000:1000"}) {
		t.Fatalf("chowns = %v", r.chowns)
	}
}

func TestDropWithAnEmptyDataDirTouchesNoFiles(t *testing.T) {
	r := newRig(t)
	if _, err := Drop(r.options("", "")); err != nil {
		t.Fatal(err)
	}
	if len(r.chowns) != 0 {
		t.Fatalf("chowns = %v, want none", r.chowns)
	}
	for _, c := range r.calls {
		if strings.HasPrefix(c, "mkdir") {
			t.Fatalf("calls = %v, want no directory created", r.calls)
		}
	}
}

func TestDropAsANonRootUserChangesNothing(t *testing.T) {
	r := newRig(t)
	r.euid = 1000
	r.add("/data", 0, 0, true)

	res, err := Drop(r.options("/data", "/run/docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Dropped {
		t.Fatalf("result = %+v, want Dropped false", res)
	}
	if len(r.calls) != 0 || len(r.chowns) != 0 {
		t.Fatalf("calls = %v, chowns = %v; a non-root process changes nothing", r.calls, r.chowns)
	}
}

func TestDropReturnsAnErrorWhenSetuidFails(t *testing.T) {
	r := newRig(t)
	r.add("/data", 1000, 1000, true)
	r.setuidErr = errors.New("operation not permitted")

	_, err := Drop(r.options("/data", ""))
	if err == nil || !strings.Contains(err.Error(), "operation not permitted") {
		t.Fatalf("err = %v, want the setuid failure", err)
	}
}

func TestDropRefusesWhenTheProcessIsStillRootAfterTheDrop(t *testing.T) {
	r := newRig(t)
	r.add("/data", 1000, 1000, true)
	o := r.options("/data", "")
	// A Setuid that reports success but changes nothing must not pass.
	o.Setuid = func(int) error { return nil }

	if _, err := Drop(o); err == nil {
		t.Fatal("a drop that leaves euid 0 must be an error")
	}
}

func TestDropReturnsAnErrorWhenChownFails(t *testing.T) {
	r := newRig(t)
	r.add("/data", 0, 0, true)
	o := r.options("/data", "")
	o.Lchown = func(string, int, int) error { return errors.New("read-only file system") }

	if _, err := Drop(o); err == nil || !strings.Contains(err.Error(), "read-only file system") {
		t.Fatalf("err = %v, want the chown failure", err)
	}
	for _, c := range r.calls {
		if strings.HasPrefix(c, "setuid") {
			t.Fatalf("calls = %v: the process must not drop while the data directory is unfixed", r.calls)
		}
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

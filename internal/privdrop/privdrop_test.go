// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package privdrop

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// socketInfo is a FileInfo for the pretend Docker socket; Sys reports its group.
type socketInfo struct{ gid uint32 }

func (s socketInfo) Name() string       { return "docker.sock" }
func (s socketInfo) Size() int64        { return 0 }
func (s socketInfo) Mode() fs.FileMode  { return fs.ModeSocket | 0o660 }
func (s socketInfo) ModTime() time.Time { return time.Time{} }
func (s socketInfo) IsDir() bool        { return false }
func (s socketInfo) Sys() any           { return &syscall.Stat_t{Gid: s.gid} }

// rig builds a real directory tree under t.TempDir() and fakes only the
// ownership and the privileged calls, so the os.Root walk runs for real while
// the test needs no root. Every privileged call is recorded, in order.
type rig struct {
	t     *testing.T
	base  string // the real directory that holds the data directory
	euid  int
	calls []string
	// owners maps a data-directory-relative name ("." is the data directory,
	// "backups/x.db" an entry of a subdirectory) to a pretend owner; a name
	// that is not listed is owned by root.
	owners map[string][2]uint32
	// socketGID is the group of the pretend Docker socket; 0 means no socket.
	socketGID uint32
	chowns    []string

	setuidErr error
}

func newRig(t *testing.T) *rig {
	t.Helper()
	return &rig{t: t, base: t.TempDir(), euid: 0, owners: map[string][2]uint32{}}
}

// data is the path of the data directory the rig builds.
func (r *rig) data() string { return filepath.Join(r.base, "data") }

// add creates a file or directory below the data directory and records its
// owner. The name is relative to the data directory.
func (r *rig) add(name string, uid, gid uint32, dir bool) {
	r.t.Helper()
	path := filepath.Join(r.data(), name)
	if dir {
		if err := os.MkdirAll(path, 0o750); err != nil {
			r.t.Fatal(err)
		}
	} else if err := os.WriteFile(path, nil, 0o600); err != nil {
		r.t.Fatal(err)
	}
	r.owners[name] = [2]uint32{uid, gid}
}

func (r *rig) options(dataDir, socket string) Options {
	return Options{
		DataDir:    dataDir,
		SocketPath: socket,
		Geteuid:    func() int { return r.euid },
		Stat: func(p string) (os.FileInfo, error) {
			if p == socket && r.socketGID != 0 {
				return socketInfo{gid: r.socketGID}, nil
			}
			return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
		},
		Owner: func(name string, _ fs.FileInfo) (uint32, uint32, bool) {
			o := r.owners[name]
			return o[0], o[1], true
		},
		Chown: func(_ *os.Root, name string, uid, gid int) error {
			rec := fmt.Sprintf("%s=%d:%d", name, uid, gid)
			r.chowns = append(r.chowns, rec)
			r.calls = append(r.calls, "chown "+rec)
			return nil
		},
		MkdirAll: func(p string, perm os.FileMode) error {
			r.calls = append(r.calls, "mkdir")
			return os.MkdirAll(p, perm)
		},
		Umask: func(m int) int {
			r.calls = append(r.calls, fmt.Sprintf("umask %03o", m))
			return 0o022
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
	r.add(".", 1000, 1000, true)
	r.socketGID = 998

	res, err := Drop(r.options(r.data(), "/run/docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"umask 077", "setgroups [998]", "setgid 1000", "setuid 1000"}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("privileged calls = %v, want %v", r.calls, want)
	}
	if !res.Dropped || res.GID != 998 {
		t.Fatalf("result = %+v, want Dropped with GID 998", res)
	}
}

func TestDropUsesTheOverrideGIDInsteadOfTheSocketGroup(t *testing.T) {
	r := newRig(t)
	r.add(".", 1000, 1000, true)
	r.socketGID = 998
	o := r.options(r.data(), "/run/docker.sock")
	gid := 4242
	o.GIDOverride = &gid

	res, err := Drop(o)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r.calls, "setgroups [4242]") || res.GID != 4242 {
		t.Fatalf("calls = %v, result = %+v; want group 4242", r.calls, res)
	}
}

func TestDropWithNoSocketJoinsNoSupplementaryGroup(t *testing.T) {
	r := newRig(t)
	r.add(".", 1000, 1000, true)

	res, err := Drop(r.options(r.data(), "/run/docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"umask 077", "setgroups []", "setgid 1000", "setuid 1000"}
	if !reflect.DeepEqual(r.calls, want) {
		t.Fatalf("privileged calls = %v, want %v", r.calls, want)
	}
	if !res.Dropped || res.GID != NoGroup {
		t.Fatalf("result = %+v, want Dropped with GID NoGroup", res)
	}
}

func TestDropChownsRootOwnedDataEntriesAndLeavesOwnedOnesAlone(t *testing.T) {
	r := newRig(t)
	r.add(".", 0, 0, true) // a fresh bind mount starts root-owned (Pitfall 9)
	r.add("tierplan.db", 0, 0, false)
	r.add("tierplan.db-wal", 0, 0, false) // left behind by a CLI run as root (A8)
	r.add("tierplan.key", 1000, 1000, false)
	r.add("setup-token", 0, 0, false)
	r.add("backups", 0, 0, true)
	r.add("backups/pre-0003.db", 0, 0, false)
	r.add("tls", 1000, 1000, true)
	r.add("tls/selfsigned.key", 0, 0, false)
	// A directory that is not backups or tls is chowned itself but not entered.
	r.add("other", 0, 0, true)
	r.add("other/inner", 0, 0, false)

	if _, err := Drop(r.options(r.data(), "")); err != nil {
		t.Fatal(err)
	}
	want := []string{
		".=1000:1000",
		"backups/pre-0003.db=1000:1000",
		"backups=1000:1000",
		"other=1000:1000",
		"setup-token=1000:1000",
		"tierplan.db-wal=1000:1000",
		"tierplan.db=1000:1000",
		"tls/selfsigned.key=1000:1000",
	}
	got := slices.Sorted(slices.Values(r.chowns))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chowns =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Ownership is fixed while still root, so before the first identity change.
	if r.calls[len(r.calls)-4] != "umask 077" {
		t.Fatalf("calls = %v", r.calls)
	}
}

func TestDropNeverFollowsASymlinkedSubdirectoryOutOfTheDataDirectory(t *testing.T) {
	r := newRig(t)
	r.add(".", 1000, 1000, true)
	// backups is a link to a directory outside the data directory that holds
	// root-owned files; a walk that followed it would chown them (T-01-108).
	outside := filepath.Join(r.base, "outside")
	if err := os.MkdirAll(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "victim"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(r.data(), "backups")); err != nil {
		t.Fatal(err)
	}
	r.owners["backups"] = [2]uint32{0, 0}
	r.owners["backups/victim"] = [2]uint32{0, 0}

	if _, err := Drop(r.options(r.data(), "")); err != nil {
		t.Fatal(err)
	}
	if want := []string{"backups=1000:1000"}; !reflect.DeepEqual(r.chowns, want) {
		t.Fatalf("chowns = %v, want only the link itself %v", r.chowns, want)
	}
}

func TestDropChownsTheRealDirectoryBehindASymlinkedDataDir(t *testing.T) {
	r := newRig(t)
	r.add(".", 0, 0, true)
	link := filepath.Join(r.base, "link")
	if err := os.Symlink(r.data(), link); err != nil {
		t.Fatal(err)
	}

	if _, err := Drop(r.options(link, "")); err != nil {
		t.Fatal(err)
	}
	if want := []string{".=1000:1000"}; !reflect.DeepEqual(r.chowns, want) {
		t.Fatalf("chowns = %v, want %v", r.chowns, want)
	}
}

func TestDropCreatesAMissingDataDirectoryBeforeChowningIt(t *testing.T) {
	r := newRig(t)
	if _, err := Drop(r.options(r.data(), "")); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) < 2 || r.calls[0] != "mkdir" || r.calls[1] != "chown .=1000:1000" {
		t.Fatalf("calls = %v, want the data directory created, then chowned", r.calls)
	}
	if st, err := os.Stat(r.data()); err != nil || !st.IsDir() {
		t.Fatalf("data directory not created: %v", err)
	}
}

func TestDropSetsUmask077BeforeTheIdentityChange(t *testing.T) {
	r := newRig(t)
	r.add(".", 0, 0, true)
	r.add("tierplan.db", 0, 0, false)

	if _, err := Drop(r.options(r.data(), "")); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"chown .=1000:1000",
		"chown tierplan.db=1000:1000",
		"umask 077",
		"setgroups []",
		"setgid 1000",
		"setuid 1000",
	}
	got := slices.Clone(r.calls)
	// Directory entries are read in name order; "." is always first.
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
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
	if slices.Contains(r.calls, "mkdir") {
		t.Fatalf("calls = %v, want no directory created", r.calls)
	}
}

func TestDropAsANonRootUserChangesNothing(t *testing.T) {
	r := newRig(t)
	r.euid = 1000
	r.add(".", 0, 0, true)

	res, err := Drop(r.options(r.data(), "/run/docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Dropped {
		t.Fatalf("result = %+v, want Dropped false", res)
	}
	if len(r.calls) != 0 || len(r.chowns) != 0 {
		t.Fatalf("calls = %v, chowns = %v; a non-root process changes nothing (umask included)", r.calls, r.chowns)
	}
}

func TestDropReturnsAnErrorWhenSetuidFails(t *testing.T) {
	r := newRig(t)
	r.add(".", 1000, 1000, true)
	r.setuidErr = errors.New("operation not permitted")

	_, err := Drop(r.options(r.data(), ""))
	if err == nil || !strings.Contains(err.Error(), "operation not permitted") {
		t.Fatalf("err = %v, want the setuid failure", err)
	}
}

func TestDropRefusesWhenTheProcessIsStillRootAfterTheDrop(t *testing.T) {
	r := newRig(t)
	r.add(".", 1000, 1000, true)
	o := r.options(r.data(), "")
	// A Setuid that reports success but changes nothing must not pass.
	o.Setuid = func(int) error { return nil }

	if _, err := Drop(o); err == nil {
		t.Fatal("a drop that leaves euid 0 must be an error")
	}
}

func TestDropReturnsAnErrorWhenChownFails(t *testing.T) {
	r := newRig(t)
	r.add(".", 0, 0, true)
	o := r.options(r.data(), "")
	o.Chown = func(*os.Root, string, int, int) error { return errors.New("read-only file system") }

	if _, err := Drop(o); err == nil || !strings.Contains(err.Error(), "read-only file system") {
		t.Fatalf("err = %v, want the chown failure", err)
	}
	for _, c := range r.calls {
		if strings.HasPrefix(c, "setuid") {
			t.Fatalf("calls = %v: the process must not drop while the data directory is unfixed", r.calls)
		}
	}
}

// chownInRoot is the production Chown. As an unprivileged user a chown to the
// caller's own identity succeeds, which is enough to prove the calls are valid
// for both a plain entry and the data directory itself.
func TestChownInRootAcceptsAnEntryAndTheRootDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, name := range []string{"f", "."} {
		if err := chownInRoot(root, name, os.Getuid(), os.Getgid()); err != nil {
			t.Errorf("chownInRoot(%q): %v", name, err)
		}
	}
	if err := chownInRoot(root, "../escape", os.Getuid(), os.Getgid()); err == nil {
		t.Error("a name that leaves the root must be refused")
	}
}

func TestOwnerOfReadsTheRealOwner(t *testing.T) {
	fi, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	uid, gid, ok := ownerOf(".", fi)
	if !ok || int(uid) != os.Getuid() || int(gid) != os.Getgid() {
		t.Errorf("ownerOf = %d, %d, %v; want %d, %d", uid, gid, ok, os.Getuid(), os.Getgid())
	}
}

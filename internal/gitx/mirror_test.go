// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/gitx/gittest"
)

// Mirrors and reading them (plan 09-09, GIT-02, D-06, D-12): a bare repository per Bayplan repository
// ID on the server's disk, filled with one ref at a time and read by commit ID.

// browseFixture is a repository with folders, a symlink and a submodule, served over TLS.
type browseFixture struct {
	repo *gittest.Repo
	srv  *gittest.HTTPServer
	sha  string
	root string
	g    *gitx.GoGit
	m    gitx.MirrorID
}

func newBrowseFixture(t *testing.T, o gittest.HTTPOptions) *browseFixture {
	t.Helper()
	repo := gittest.NewRepo(t)
	repo.Symlink(t, "link", "stacks/a")
	repo.Submodule(t, "lib")
	sha := repo.Commit(t, map[string]string{
		"compose.yaml":         "services: {}\n",
		"stacks/a/compose.yml": "services: {}\n",
		"stacks/b/notes.txt":   "not a stack\n",
	}, "first")
	repo.Branch(t, "other", sha)
	o.TLS = true
	root := t.TempDir()
	return &browseFixture{
		repo: repo, srv: gittest.NewHTTPServer(t, repo, o), sha: sha, root: root,
		g: gitx.NewGoGit(gitx.Options{Root: root}),
		m: gitx.MirrorID(uuid.NewString()),
	}
}

func (f *browseFixture) remote() gitx.Remote {
	return gitx.Remote{URL: f.srv.URL, CAPEM: f.srv.CAPEM}
}

func (f *browseFixture) mirrorDir() string { return filepath.Join(f.root, string(f.m)+".git") }

func TestFetchRefAndReadDir(t *testing.T) {
	t.Parallel()
	f := newBrowseFixture(t, gittest.HTTPOptions{})
	ctx := context.Background()

	c, err := f.g.FetchRef(ctx, f.m, f.remote(), gitx.RefBranch, "main")
	if err != nil {
		t.Fatalf("FetchRef: %v", err)
	}
	if c.SHA != f.sha || !c.When.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("commit = %+v, want %s at 2026-01-01", c, f.sha)
	}

	root, err := f.g.ReadDir(ctx, f.m, c.SHA, "", 0)
	if err != nil {
		t.Fatalf("ReadDir root: %v", err)
	}
	wantRoot := []gitx.Entry{
		{Name: "stacks", Kind: gitx.EntryDir},
		{Name: "compose.yaml", Kind: gitx.EntryFile},
		{Name: "lib", Kind: gitx.EntrySubmodule},
		{Name: "link", Kind: gitx.EntrySymlink},
	}
	if root.Total != 4 || root.Capped || !equalEntries(root.Entries, wantRoot) {
		t.Errorf("root = %+v, want the folder first, then compose.yaml, the submodule lib and the symlink link", root)
	}

	stacks, err := f.g.ReadDir(ctx, f.m, c.SHA, "stacks", 0)
	if err != nil {
		t.Fatalf("ReadDir stacks: %v", err)
	}
	wantStacks := []gitx.Entry{{Name: "a", Kind: gitx.EntryDir, HasCompose: true}, {Name: "b", Kind: gitx.EntryDir}}
	if stacks.Path != "stacks" || !equalEntries(stacks.Entries, wantStacks) {
		t.Errorf("stacks = %+v, want a (holds compose.yml) and b (does not)", stacks)
	}
}

func equalEntries(a, b []gitx.Entry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFetchRefAsksOnlyForTheTrackedRef(t *testing.T) {
	t.Parallel()
	f := newBrowseFixture(t, gittest.HTTPOptions{})
	if _, err := f.g.FetchRef(context.Background(), f.m, f.remote(), gitx.RefBranch, "main"); err != nil {
		t.Fatal(err)
	}
	// The repository has another branch; the mirror holds only the one asked for.
	if _, err := os.Stat(filepath.Join(f.mirrorDir(), "refs", "heads", "main")); err != nil {
		t.Errorf("the tracked branch is not in the mirror: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.mirrorDir(), "refs", "heads", "other")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the other branch is in the mirror (err %v): a fetch asks for the tracked ref only", err)
	}
	// An already-current mirror is a success, not an error.
	if _, err := f.g.FetchRef(context.Background(), f.m, f.remote(), gitx.RefBranch, "main"); err != nil {
		t.Errorf("a second fetch of an unchanged branch failed: %v", err)
	}
}

func TestMirrorIsABareRepositoryNamedByItsIDAndHoldsNoCredential(t *testing.T) {
	t.Parallel()
	const token = "glpat-MIRRORtoken0123456789" //nolint:gosec // G101: a made-up token for the test
	f := newBrowseFixture(t, gittest.HTTPOptions{BasicAuth: &[2]string{"bot", token}})
	r := f.remote()
	r.Auth = gitx.TokenAuth{Username: "bot", Token: token}
	if _, err := f.g.FetchRef(context.Background(), f.m, r, gitx.RefBranch, "main"); err != nil {
		t.Fatalf("FetchRef: %v", err)
	}

	entries, err := os.ReadDir(f.root)
	if err != nil || len(entries) != 1 || entries[0].Name() != string(f.m)+".git" {
		t.Fatalf("the mirrors directory holds %v (%v), want only %s.git", entries, err, f.m)
	}
	// Nothing is checked out: a bare repository has no working tree and none of the repository's files.
	for _, name := range []string{".git", "compose.yaml", "stacks", "link", "lib"} {
		if _, err := os.Lstat(filepath.Join(f.mirrorDir(), name)); err == nil {
			t.Errorf("%s is in the mirror's directory: nothing may be checked out", name)
		}
	}
	// Neither the credential nor the address is on disk, wherever go-git put anything.
	_, port, _ := strings.Cut(strings.TrimPrefix(f.srv.URL, "https://"), ":")
	port, _, _ = strings.Cut(port, "/")
	err = filepath.WalkDir(f.mirrorDir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(p) //nolint:gosec // G304: a file under the test's own temporary directory
		if rerr != nil {
			return rerr
		}
		for _, secret := range []string{token, "127.0.0.1:" + port} {
			if bytes.Contains(b, []byte(secret)) {
				t.Errorf("%s holds %q", p, secret)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAMirrorIDThatIsNotAUUIDIsRefused(t *testing.T) {
	t.Parallel()
	f := newBrowseFixture(t, gittest.HTTPOptions{})
	for _, id := range []gitx.MirrorID{"", "../escape", "main", gitx.MirrorID(strings.ToUpper(uuid.NewString())), "urn:uuid:" + gitx.MirrorID(uuid.NewString())} {
		if _, err := f.g.FetchRef(context.Background(), id, f.remote(), gitx.RefBranch, "main"); !errors.Is(err, gitx.ErrMirrorID) {
			t.Errorf("FetchRef(%q) = %v, want ErrMirrorID", id, err)
		}
		if _, err := f.g.ReadDir(context.Background(), id, f.sha, "", 0); !errors.Is(err, gitx.ErrMirrorID) {
			t.Errorf("ReadDir(%q) = %v, want ErrMirrorID", id, err)
		}
	}
	if entries, _ := os.ReadDir(f.root); len(entries) != 0 {
		t.Errorf("a refused ID created %v", entries)
	}
}

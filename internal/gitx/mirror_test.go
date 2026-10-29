// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// moveBranch points a branch of the fixture's bare repository at a commit. It returns an error
// instead of failing the test, so a goroutine can call it.
func moveBranch(dir, branch, sha string) error {
	out, err := exec.Command("git", "-C", dir, "update-ref", "refs/heads/"+branch, sha).CombinedOutput() //nolint:gosec // G204: a test's own constants
	if err != nil {
		return fmt.Errorf("git update-ref: %w: %s", err, out)
	}
	return nil
}

// TestMirrorConcurrentFetchAndRead has eight goroutines fetch a branch that keeps moving and read a
// folder at the commit each fetch answered. Every read must show exactly the content of that commit,
// never a mix of two, and the race detector must stay quiet.
func TestMirrorConcurrentFetchAndRead(t *testing.T) {
	t.Parallel()
	repo := gittest.NewRepo(t)
	const commits = 6
	shas := make([]string, commits)
	index := map[string]int{}
	for i := range commits {
		shas[i] = repo.Commit(t, map[string]string{
			"n":                           fmt.Sprint(i),
			fmt.Sprintf("d/only-%d", i):   "x\n",
			fmt.Sprintf("d/only-%d", i-1): gittest.Delete,
		}, fmt.Sprintf("c%d", i))
		index[shas[i]] = i
	}
	if err := moveBranch(repo.Dir(), "moving", shas[0]); err != nil {
		t.Fatal(err)
	}
	srv := gittest.NewHTTPServer(t, repo, gittest.HTTPOptions{TLS: true})
	g := gitx.NewGoGit(gitx.Options{Root: t.TempDir()})
	m := gitx.MirrorID(uuid.NewString())
	remote := gitx.Remote{URL: srv.URL, CAPEM: srv.CAPEM}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	moverDone := make(chan error, 1)
	go func() {
		for i := 0; ctx.Err() == nil; i++ {
			if err := moveBranch(repo.Dir(), "moving", shas[i%commits]); err != nil {
				moverDone <- err
				return
			}
		}
		moverDone <- nil
	}()

	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 12 {
				c, err := g.FetchRef(context.Background(), m, remote, gitx.RefBranch, "moving")
				if err != nil {
					t.Errorf("worker %d: FetchRef: %v", w, err)
					return
				}
				i, ok := index[c.SHA]
				if !ok {
					t.Errorf("worker %d: FetchRef answered %s, which is not a commit of the fixture", w, c.SHA)
					return
				}
				d, err := g.ReadDir(context.Background(), m, c.SHA, "d", 0)
				if err != nil || len(d.Entries) != 1 || d.Entries[0].Name != fmt.Sprintf("only-%d", i) {
					t.Errorf("worker %d: ReadDir(d) at commit %d = %+v, %v, want the one file only-%d", w, i, d, err, i)
					return
				}
				root, err := g.ReadDir(context.Background(), m, c.SHA, "", 0)
				if err != nil || root.Total != 2 {
					t.Errorf("worker %d: ReadDir(root) at commit %d = %+v, %v, want d and n", w, i, root, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	cancel()
	if err := <-moverDone; err != nil {
		t.Fatal(err)
	}
}

func TestMirrorCreateOnce(t *testing.T) {
	t.Parallel()
	f := newBrowseFixture(t, gittest.HTTPOptions{})
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, err := f.g.FetchRef(context.Background(), f.m, f.remote(), gitx.RefBranch, "main"); err != nil || c.SHA != f.sha {
				t.Errorf("worker %d: FetchRef = %+v, %v, want %s", w, c, err, f.sha)
			}
		}()
	}
	wg.Wait()

	entries, err := os.ReadDir(f.root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("the mirrors directory holds %v (%v), want one mirror", entries, err)
	}
	cfg, err := os.ReadFile(filepath.Join(f.mirrorDir(), "config"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(cfg), `[remote "origin"]`); n != 1 {
		t.Errorf("the mirror's configuration has %d origin remotes, want 1:\n%s", n, cfg)
	}
}

func TestMirrorRecreated(t *testing.T) {
	t.Parallel()
	f := newBrowseFixture(t, gittest.HTTPOptions{})
	ctx := context.Background()
	if _, err := f.g.FetchRef(ctx, f.m, f.remote(), gitx.RefBranch, "main"); err != nil {
		t.Fatal(err)
	}

	// Removed between calls: a read finds nothing, and the next fetch makes it again.
	if err := os.RemoveAll(f.mirrorDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.g.ReadDir(ctx, f.m, f.sha, "", 0); codeOf(err) != "commit_missing" {
		t.Errorf("ReadDir of a removed mirror = %s, want commit_missing", codeOf(err))
	}
	if c, err := f.g.FetchRef(ctx, f.m, f.remote(), gitx.RefBranch, "main"); err != nil || c.SHA != f.sha {
		t.Fatalf("FetchRef after the removal = %+v, %v, want %s", c, err, f.sha)
	}
	if d, err := f.g.ReadDir(ctx, f.m, f.sha, "stacks", 0); err != nil || len(d.Entries) != 2 {
		t.Errorf("ReadDir after the fetch = %+v, %v, want the two folders of stacks", d, err)
	}

	// A directory left empty by a fetch that died while making it is made good too.
	if err := os.RemoveAll(f.mirrorDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.mirrorDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if c, err := f.g.FetchRef(ctx, f.m, f.remote(), gitx.RefBranch, "main"); err != nil || c.SHA != f.sha {
		t.Errorf("FetchRef into an empty directory = %+v, %v, want %s", c, err, f.sha)
	}
}

// Keeping the mirrors (plan 09-32, D-08): they can be listed, measured, dated by their last use and removed, so a sweep
// can keep a server's disk bounded. A removed mirror is built again by the next fetch.
func TestMirrorKeeping(t *testing.T) {
	t.Parallel()
	f := newBrowseFixture(t, gittest.HTTPOptions{})
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	now := at
	g := gitx.NewGoGit(gitx.Options{Root: f.root, Now: func() time.Time { return now }})

	if ids, err := g.ListMirrors(); err != nil || len(ids) != 0 {
		t.Fatalf("ListMirrors before any fetch = %v, %v, want none", ids, err)
	}
	if _, _, err := g.MirrorStats(f.m); err == nil {
		t.Fatal("MirrorStats of a mirror that is not there succeeded")
	}
	if err := g.RemoveMirror(f.m); err != nil {
		t.Fatalf("RemoveMirror of a mirror that is not there: %v", err)
	}

	c, err := g.FetchRef(ctx, f.m, f.remote(), gitx.RefBranch, "main")
	if err != nil {
		t.Fatalf("FetchRef: %v", err)
	}
	// A directory that is not a mirror's is left alone by the listing.
	if err := os.MkdirAll(filepath.Join(f.root, "not-a-mirror.git"), 0o700); err != nil {
		t.Fatal(err)
	}
	ids, err := g.ListMirrors()
	if err != nil || len(ids) != 1 || ids[0] != f.m {
		t.Fatalf("ListMirrors = %v, %v, want the one mirror", ids, err)
	}
	size, last, err := g.MirrorStats(f.m)
	if err != nil || size <= 0 || !last.Equal(at) {
		t.Fatalf("MirrorStats = %d, %v, %v, want a size and the time of the fetch %v", size, last, err, at)
	}

	// Reading the mirror is a use as well.
	now = at.Add(3 * 24 * time.Hour)
	if _, err := g.ReadDir(ctx, f.m, c.SHA, "", 0); err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if _, last, _ = g.MirrorStats(f.m); !last.Equal(now) {
		t.Errorf("last use after a read = %v, want %v", last, now)
	}

	// A process that has not used the mirror (a restart) reads the time from the mirror's own file.
	now = at.Add(10 * 24 * time.Hour)
	restarted := gitx.NewGoGit(gitx.Options{Root: f.root, Now: func() time.Time { return now }})
	if _, last, _ = restarted.MirrorStats(f.m); !last.Equal(at.Add(3 * 24 * time.Hour)) {
		t.Errorf("last use after a restart = %v, want the time of the read %v", last, at.Add(3*24*time.Hour))
	}

	if err := g.RemoveMirror(f.m); err != nil {
		t.Fatalf("RemoveMirror: %v", err)
	}
	if _, err := os.Stat(f.mirrorDir()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the mirror is still on disk after RemoveMirror: %v", err)
	}
	if _, err := g.ReadDir(ctx, f.m, c.SHA, "", 0); err == nil {
		t.Error("ReadDir of a removed mirror succeeded")
	}
	// The next fetch builds it again.
	if _, err := g.FetchRef(ctx, f.m, f.remote(), gitx.RefBranch, "main"); err != nil {
		t.Fatalf("FetchRef after the removal: %v", err)
	}
	if _, err := g.ReadDir(ctx, f.m, c.SHA, "", 0); err != nil {
		t.Errorf("ReadDir after the mirror was built again: %v", err)
	}

	// A name that is not a repository ID never reaches the disk.
	if err := g.RemoveMirror(gitx.MirrorID("../x")); !errors.Is(err, gitx.ErrMirrorID) {
		t.Errorf("RemoveMirror(../x) = %v, want ErrMirrorID", err)
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package gitx_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/gitx"
	"github.com/ianglennon/tierplan/internal/gitx/gittest"
)

// Reading the files of a folder of a commit (plan 09-10, GIT-02, D-27): only what is asked for is read, the
// rest is counted and named, and a link is reported and never followed.

type folderFixture struct {
	*browseFixture
}

func newFolderFixture(t *testing.T) *folderFixture {
	t.Helper()
	return newFolderFixtureWith(t, func(repo *gittest.Repo) {
		repo.Symlink(t, "stacks/web/out", "../../outside")
		repo.Symlink(t, "stacks/web/same", "web.env")
		repo.Submodule(t, "stacks/web/lib")
	}, map[string]string{
		"stacks/web/compose.yaml":  "services: {}\n",
		"stacks/web/.env":          "TAG=1\n",
		"stacks/web/web.env":       "A=b\n",
		"stacks/web/README.md":     "# web\n",
		"stacks/web/docs/a.md":     "a\n",
		"stacks/other/compose.yml": "services: {}\n",
	})
}

func newFolderFixtureWith(t *testing.T, prepare func(*gittest.Repo), files map[string]string) *folderFixture {
	t.Helper()
	repo := gittest.NewRepo(t)
	prepare(repo)
	sha := repo.Commit(t, files, "first")
	f := &browseFixture{repo: repo, sha: sha, root: t.TempDir()}
	f.srv = gittest.NewHTTPServer(t, repo, gittest.HTTPOptions{TLS: true})
	f.g = gitx.NewGoGit(gitx.Options{Root: f.root})
	f.m = "00000000-0000-4000-8000-000000000001"
	if _, err := f.g.FetchRef(context.Background(), f.m, f.remote(), gitx.RefBranch, "main"); err != nil {
		t.Fatalf("FetchRef: %v", err)
	}
	return &folderFixture{browseFixture: f}
}

func paths(fs []gitx.FolderFile) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Path
	}
	return out
}

func TestReadFolderReadsOnlyWhatIsAskedFor(t *testing.T) {
	t.Parallel()
	f := newFolderFixture(t)
	ctx := context.Background()
	want := map[string]bool{"compose.yaml": true, ".env": true, "web.env": true}

	got, err := f.g.ReadFolder(ctx, f.m, f.sha, "stacks/web", func(rel string) bool { return want[rel] }, gitx.ReadLimits{})
	if err != nil {
		t.Fatalf("ReadFolder: %v", err)
	}
	if p := paths(got.Files); !slices.Equal(p, []string{".env", "compose.yaml", "web.env"}) {
		t.Errorf("files = %v, want the three asked for, sorted", p)
	}
	for _, file := range got.Files {
		if file.Kind != gitx.EntryFile || file.Path == "compose.yaml" && string(file.Content) != "services: {}\n" {
			t.Errorf("file %+v is wrong", file)
		}
	}
	// Everything else is counted and named, the links included, and nothing of it is read.
	if wantOthers := []string{"README.md", "docs/a.md", "lib", "out", "same"}; !slices.Equal(got.Others, wantOthers) || got.OthersTotal != 5 {
		t.Errorf("others = %v (%d), want %v", got.Others, got.OthersTotal, wantOthers)
	}
	if l := got.Links["out"]; l.Kind != gitx.EntrySymlink || l.Target != "../../outside" {
		t.Errorf("link out = %+v, want the symlink to ../../outside", l)
	}
	if l := got.Links["lib"]; l.Kind != gitx.EntrySubmodule || l.Target != "" {
		t.Errorf("link lib = %+v, want a submodule", l)
	}
	if got.Tree != f.repo.Tree(t, f.sha, "stacks/web") {
		t.Errorf("tree = %s, want the folder's tree object", got.Tree)
	}
}

func TestReadFolderNamesAreCapped(t *testing.T) {
	t.Parallel()
	f := newFolderFixture(t)
	got, err := f.g.ReadFolder(context.Background(), f.m, f.sha, "", func(string) bool { return false }, gitx.ReadLimits{MaxOthers: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Others) != 3 || got.OthersTotal != 9 || len(got.Files) != 0 {
		t.Errorf("others = %v (%d), want the first 3 names and a total of 9", got.Others, got.OthersTotal)
	}
}

func TestReadFolderAWantedLinkInsideTheFolderIsReadAsItsFile(t *testing.T) {
	t.Parallel()
	f := newFolderFixture(t)
	got, err := f.g.ReadFolder(context.Background(), f.m, f.sha, "stacks/web", func(rel string) bool { return rel == "same" || rel == "lib" || rel == "out" }, gitx.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 3 || got.Files[0].Path != "lib" || got.Files[0].Kind != gitx.EntrySubmodule ||
		got.Files[1].Path != "out" || got.Files[1].Kind != gitx.EntrySymlink ||
		got.Files[2].Path != "same" || got.Files[2].Kind != gitx.EntryFile || string(got.Files[2].Content) != "A=b\n" {
		t.Errorf("files = %+v, want the submodule, the unfollowed outside link and same read as web.env", got.Files)
	}
	if !got.Links["same"].Followed || got.Links["out"].Followed || !got.Links["out"].Outside {
		t.Errorf("links = %+v, want same followed and out outside and unfollowed", got.Links)
	}
}

// The outside marker lives in the same repository, so a read that followed a link out of the folder would find it.
func TestReadFolderSymlinks(t *testing.T) {
	t.Parallel()
	const marker = "OUTSIDE-MARKER"
	f := newFolderFixtureWith(t, func(r *gittest.Repo) {
		r.Symlink(t, "stacks/web/inside", "real.env")
		r.Symlink(t, "stacks/web/sub/deep", "../real.env")
		r.Symlink(t, "stacks/web/dotted", "./sub/../real.env")
		r.Symlink(t, "stacks/web/outside", "../secret/key.env")
		r.Symlink(t, "stacks/web/sneaky", "sub/../../secret/key.env")
		r.Symlink(t, "stacks/web/absolute", "/etc/passwd")
		r.Symlink(t, "stacks/web/chain", "inside")
		r.Symlink(t, "stacks/web/todir", "sub")
		r.Symlink(t, "stacks/web/nowhere", "missing.env")
		r.Symlink(t, "stacks/web/toself", ".")
	}, map[string]string{
		"stacks/web/compose.yaml":  "services: {}\n",
		"stacks/web/real.env":      "A=b\n",
		"stacks/web/sub/x.env":     "X=1\n",
		"stacks/secret/key.env":    marker + "\n",
		"stacks/other/compose.yml": "services: {}\n",
	})
	got, err := f.g.ReadFolder(context.Background(), f.m, f.sha, "stacks/web", func(string) bool { return true }, gitx.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]gitx.FolderFile{}
	for _, file := range got.Files {
		byPath[file.Path] = file
		if strings.Contains(string(file.Content), marker) {
			t.Errorf("%s holds the content of a file outside the folder", file.Path)
		}
	}
	for _, name := range []string{"inside", "sub/deep", "dotted"} {
		file := byPath[name]
		if file.Kind != gitx.EntryFile || string(file.Content) != "A=b\n" || !got.Links[name].Followed {
			t.Errorf("%s = %+v (followed %v), want real.env's content as a file", name, file, got.Links[name].Followed)
		}
	}
	for name, outside := range map[string]bool{"outside": true, "sneaky": true, "absolute": true, "chain": false, "todir": false, "nowhere": false, "toself": false} {
		l := got.Links[name]
		if l.Followed || l.Outside != outside || byPath[name].Kind != gitx.EntrySymlink {
			t.Errorf("%s = link %+v file %+v, want an unfollowed link with outside=%v", name, l, byPath[name], outside)
		}
	}
}

func TestFolderTreeFollowsTheFolder(t *testing.T) {
	t.Parallel()
	f := newFolderFixture(t)
	ctx := context.Background()
	web, err := f.g.FolderTree(ctx, f.m, f.sha, "stacks/web")
	if err != nil {
		t.Fatal(err)
	}
	root, err := f.g.FolderTree(ctx, f.m, f.sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if web != f.repo.Tree(t, f.sha, "stacks/web") || root != f.repo.Tree(t, f.sha, "") {
		t.Errorf("trees = %s and %s, want git's", web, root)
	}

	// A change outside the folder moves the root and leaves the folder's tree alone; one inside moves both.
	outside := f.repo.Commit(t, map[string]string{"stacks/other/compose.yml": "services: {a: {}}\n"}, "outside")
	inside := f.repo.Commit(t, map[string]string{"stacks/web/README.md": "# changed\n"}, "inside")
	if _, err := f.g.FetchRef(ctx, f.m, f.remote(), gitx.RefBranch, "main"); err != nil {
		t.Fatal(err)
	}
	webOutside, _ := f.g.FolderTree(ctx, f.m, outside, "stacks/web")
	rootOutside, _ := f.g.FolderTree(ctx, f.m, outside, "")
	webInside, _ := f.g.FolderTree(ctx, f.m, inside, "stacks/web")
	if webOutside != web || rootOutside == root {
		t.Errorf("after a change outside: folder %s (was %s), root %s (was %s)", webOutside, web, rootOutside, root)
	}
	if webInside == web {
		t.Errorf("after a change inside the folder tree is still %s", web)
	}
}

func TestReadFolderMissingThings(t *testing.T) {
	t.Parallel()
	f := newFolderFixture(t)
	ctx := context.Background()
	all := func(string) bool { return true }

	for name, call := range map[string]func() error{
		"folder": func() error { _, err := f.g.ReadFolder(ctx, f.m, f.sha, "nope", all, gitx.ReadLimits{}); return err },
		"file as a folder": func() error {
			_, err := f.g.ReadFolder(ctx, f.m, f.sha, "stacks/web/.env", all, gitx.ReadLimits{})
			return err
		},
		"tree of a folder": func() error { _, err := f.g.FolderTree(ctx, f.m, f.sha, "nope"); return err },
	} {
		var ge *gitx.Error
		if err := call(); !errors.As(err, &ge) || ge.Code != gitx.CodeFolderMissing {
			t.Errorf("%s: err = %v, want folder_missing", name, err)
		}
	}
	missing := strings.Repeat("a", 40)
	var ge *gitx.Error
	if _, err := f.g.ReadFolder(ctx, f.m, missing, "", all, gitx.ReadLimits{}); !errors.As(err, &ge) || ge.Code != gitx.CodeCommitMissing {
		t.Errorf("a commit the mirror lacks: err = %v, want commit_missing", err)
	}
	if _, err := f.g.FolderTree(ctx, f.m, missing, ""); !errors.As(err, &ge) || ge.Code != gitx.CodeCommitMissing {
		t.Errorf("FolderTree of a missing commit: err = %v, want commit_missing", err)
	}
}

func TestReadFolderLimits(t *testing.T) {
	t.Parallel()
	f := newFolderFixture(t)
	ctx := context.Background()
	all := func(rel string) bool { return rel == "compose.yaml" || rel == "web.env" || rel == ".env" }

	var le *gitx.LimitError
	// One file over the per-file limit names the file.
	if _, err := f.g.ReadFolder(ctx, f.m, f.sha, "stacks/web", all, gitx.ReadLimits{MaxFileBytes: 10}); !errors.As(err, &le) || le.Path != "compose.yaml" {
		t.Errorf("per-file: err = %v, want a LimitError for compose.yaml", err)
	}
	// The files together over the total limit name none.
	if _, err := f.g.ReadFolder(ctx, f.m, f.sha, "stacks/web", all, gitx.ReadLimits{MaxTotalBytes: 20}); !errors.As(err, &le) || le.Path != "" {
		t.Errorf("total: err = %v, want a LimitError with no path", err)
	}
	// Too many entries to look at.
	if _, err := f.g.ReadFolder(ctx, f.m, f.sha, "", all, gitx.ReadLimits{MaxEntries: 3}); !errors.As(err, &le) {
		t.Errorf("entries: err = %v, want a LimitError", err)
	}
}

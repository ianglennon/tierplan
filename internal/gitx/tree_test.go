// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/gitx/gittest"
)

// Tags, fixed commits, missing refs and folders, and ref names (plan 09-09, GIT-02, D-12).

// codeOf is the gitx code of err, or its text when it is not a *gitx.Error.
func codeOf(err error) string {
	var ge *gitx.Error
	if errors.As(err, &ge) {
		return string(ge.Code)
	}
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// historyFixture has two commits on main (first, second), a lightweight tag v1 at first, an annotated
// tag v2 at second, a branch and a tag both named rel at different commits, and a commit that no
// branch or tag reaches (orphan), held by a pull-request ref as a forge does.
type historyFixture struct {
	*browseFixture
	first, second, orphan string
}

func newHistoryFixture(t *testing.T, o gittest.HTTPOptions) *historyFixture {
	t.Helper()
	repo := gittest.NewRepo(t)
	first := repo.Commit(t, map[string]string{"compose.yaml": "services: {}\n", "a/compose.yaml": "services: {}\n"}, "first")
	second := repo.Commit(t, map[string]string{"compose.yaml": "services: {a: {image: x}}\n"}, "second")
	orphan := repo.Commit(t, map[string]string{"orphan.txt": "nobody points here\n"}, "orphan")
	repo.Branch(t, "main", second)
	repo.Ref(t, "refs/pull/1/head", orphan)
	repo.Tag(t, "v1", first, false)
	repo.Tag(t, "v2", second, true)
	repo.Branch(t, "rel", second)
	repo.Tag(t, "rel", first, false)
	o.TLS = true
	root := t.TempDir()
	return &historyFixture{
		browseFixture: &browseFixture{
			repo: repo, srv: gittest.NewHTTPServer(t, repo, o), sha: second, root: root,
			g: gitx.NewGoGit(gitx.Options{Root: root}), m: gitx.MirrorID(uuid.NewString()),
		},
		first: first, second: second, orphan: orphan,
	}
}

func TestResolveTags(t *testing.T) {
	t.Parallel()
	f := newHistoryFixture(t, gittest.HTTPOptions{})
	ctx := context.Background()
	for _, tc := range []struct {
		kind gitx.RefKind
		ref  string
		want string
	}{
		{gitx.RefTag, "v1", f.first},  // a lightweight tag is its commit
		{gitx.RefTag, "v2", f.second}, // an annotated tag is peeled to the commit, never the tag object
		{gitx.RefTag, "rel", f.first}, // a tag and a branch of one name are two refs
		{gitx.RefBranch, "rel", f.second},
	} {
		c, err := f.g.FetchRef(ctx, f.m, f.remote(), tc.kind, tc.ref)
		if err != nil || c.SHA != tc.want {
			t.Errorf("FetchRef(%s %s) = %+v, %v, want %s", tc.kind, tc.ref, c, err, tc.want)
		}
	}
	// A commit that is a tag's is readable.
	if d, err := f.g.ReadDir(ctx, f.m, f.first, "a", 0); err != nil || len(d.Entries) != 1 {
		t.Errorf("ReadDir at the commit of v1 = %+v, %v, want the one file of a", d, err)
	}
}

func TestResolveCommitPrefix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("a prefix or the full ID of a commit a branch reaches", func(t *testing.T) {
		t.Parallel()
		f := newHistoryFixture(t, gittest.HTTPOptions{})
		for _, ref := range []string{f.first, f.first[:7], strings.ToUpper(f.first[:12])} {
			m := gitx.MirrorID(uuid.NewString()) // each on a fresh mirror: nothing is already there
			c, err := f.g.FetchRef(ctx, m, f.remote(), gitx.RefCommit, ref)
			if err != nil || c.SHA != f.first {
				t.Errorf("FetchRef(commit %s) = %+v, %v, want %s", ref, c, err, f.first)
			}
		}
	})

	t.Run("a prefix that matches nothing is commit_missing", func(t *testing.T) {
		t.Parallel()
		f := newHistoryFixture(t, gittest.HTTPOptions{})
		bad := "0000000"
		if strings.HasPrefix(f.first, bad) || strings.HasPrefix(f.second, bad) || strings.HasPrefix(f.orphan, bad) {
			t.Skip("the fixture's commit starts with seven zeros")
		}
		_, err := f.g.FetchRef(ctx, f.m, f.remote(), gitx.RefCommit, bad)
		if got := codeOf(err); got != "commit_missing" {
			t.Errorf("FetchRef(commit %s) = %s, want commit_missing", bad, got)
		}
	})

	t.Run("a commit nothing points at is found by asking for it when the server allows it", func(t *testing.T) {
		t.Parallel()
		f := newHistoryFixture(t, gittest.HTTPOptions{AllowAnySHA1: true})
		c, err := f.g.FetchRef(ctx, f.m, f.remote(), gitx.RefCommit, f.orphan)
		if err != nil || c.SHA != f.orphan {
			t.Errorf("FetchRef(commit %s) = %+v, %v, want it", f.orphan, c, err)
		}
		// Having it, the mirror answers without the server: a prefix of it resolves too.
		if c, err := f.g.FetchRef(ctx, f.m, f.remote(), gitx.RefCommit, f.orphan[:8]); err != nil || c.SHA != f.orphan {
			t.Errorf("FetchRef(commit %s) = %+v, %v, want %s", f.orphan[:8], c, err, f.orphan)
		}
	})

	t.Run("a commit nothing points at is commit_missing when the server refuses to hand it out", func(t *testing.T) {
		t.Parallel()
		f := newHistoryFixture(t, gittest.HTTPOptions{})
		_, err := f.g.FetchRef(ctx, f.m, f.remote(), gitx.RefCommit, f.orphan)
		if got := codeOf(err); got != "commit_missing" {
			t.Errorf("FetchRef(commit %s) = %s, want commit_missing", f.orphan, got)
		}
	})
}

func TestMissingRefAndFolder(t *testing.T) {
	t.Parallel()
	f := newHistoryFixture(t, gittest.HTTPOptions{})
	ctx := context.Background()

	for _, tc := range []struct {
		kind gitx.RefKind
		ref  string
	}{{gitx.RefBranch, "never-existed"}, {gitx.RefTag, "never-existed"}, {gitx.RefBranch, "v1"}, {gitx.RefTag, "main"}} {
		if _, err := f.g.FetchRef(ctx, f.m, f.remote(), tc.kind, tc.ref); codeOf(err) != "ref_missing" {
			t.Errorf("FetchRef(%s %s) = %s, want ref_missing", tc.kind, tc.ref, codeOf(err))
		}
	}

	// A branch deleted on the server is missing even though the mirror still has what it fetched.
	f.repo.Branch(t, "short-lived", f.first)
	if _, err := f.g.FetchRef(ctx, f.m, f.remote(), gitx.RefBranch, "short-lived"); err != nil {
		t.Fatalf("FetchRef(short-lived): %v", err)
	}
	f.repo.DeleteRef(t, "refs/heads/short-lived")
	if _, err := f.g.FetchRef(ctx, f.m, f.remote(), gitx.RefBranch, "short-lived"); codeOf(err) != "ref_missing" {
		t.Errorf("FetchRef of a deleted branch = %s, want ref_missing", codeOf(err))
	}

	c, err := f.g.FetchRef(ctx, f.m, f.remote(), gitx.RefBranch, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"nope", "a/nope", "compose.yaml", "a/compose.yaml/x", "../a", "a/../a"} {
		if _, err := f.g.ReadDir(ctx, f.m, c.SHA, dir, 0); codeOf(err) != "folder_missing" {
			t.Errorf("ReadDir(%q) = %s, want folder_missing (a file or an absent path)", dir, codeOf(err))
		}
	}
	if _, err := f.g.ReadDir(ctx, f.m, strings.Repeat("0", 40), "", 0); codeOf(err) != "commit_missing" {
		t.Errorf("ReadDir at a commit the mirror lacks = %s, want commit_missing", codeOf(err))
	}
}

func TestRefNameValidation(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{
		"", "a..b", "-x", "a\nb", strings.Repeat("a", 300), "a b", "a~b", "a^b", "a:b", "a?b", "a*b", "a[b", `a\b`,
		"/a", "a/", "a//b", "a.lock", "a/b.lock/c", "a/.b", "a@{b", "@", "a.", "a\x00b", "a\x7fb", "a\tb",
	} {
		for _, kind := range []gitx.RefKind{gitx.RefBranch, gitx.RefTag} {
			if err := gitx.ValidRefName(kind, ref); !errors.Is(err, gitx.ErrRefInvalid) {
				t.Errorf("ValidRefName(%s, %q) = %v, want ErrRefInvalid", kind, ref, err)
			}
		}
	}
	for _, ref := range []string{"main", "release/1.2", "v1.0.0", "dev_2", "feature/ünï", "a.b", "a-b", strings.Repeat("a", 255)} {
		for _, kind := range []gitx.RefKind{gitx.RefBranch, gitx.RefTag} {
			if err := gitx.ValidRefName(kind, ref); err != nil {
				t.Errorf("ValidRefName(%s, %q) = %v, want nil", kind, ref, err)
			}
		}
	}
	for _, ref := range []string{"", "abcdef", "abcdef0123456789abcdef0123456789abcdef012", "ghijklm", "-abcdef1", "abcdef1 ", "main"} {
		if err := gitx.ValidRefName(gitx.RefCommit, ref); !errors.Is(err, gitx.ErrRefInvalid) {
			t.Errorf("ValidRefName(commit, %q) = %v, want ErrRefInvalid", ref, err)
		}
	}
	for _, ref := range []string{"abcdef1", "ABCDEF1", "abcdef0123456789abcdef0123456789abcdef01"} {
		if err := gitx.ValidRefName(gitx.RefCommit, ref); err != nil {
			t.Errorf("ValidRefName(commit, %q) = %v, want nil", ref, err)
		}
	}
	if err := gitx.ValidRefName("nonsense", "main"); !errors.Is(err, gitx.ErrRefInvalid) {
		t.Errorf("ValidRefName with an unknown kind = %v, want ErrRefInvalid", err)
	}

	// A refused name never reaches a server.
	f := newHistoryFixture(t, gittest.HTTPOptions{})
	for _, ref := range []string{"a..b", "-x", "a\nb", strings.Repeat("a", 300)} {
		if _, err := f.g.FetchRef(context.Background(), f.m, f.remote(), gitx.RefBranch, ref); !errors.Is(err, gitx.ErrRefInvalid) {
			t.Errorf("FetchRef(%q) = %v, want ErrRefInvalid", ref, err)
		}
	}
	if n := f.srv.Requests(); n != 0 {
		t.Errorf("the server saw %d requests for refused names, want none", n)
	}
}

func TestReadDirCap(t *testing.T) {
	t.Parallel()
	repo := gittest.NewRepo(t)
	files := map[string]string{}
	for i := range 600 {
		files[fmt.Sprintf("many/f%03d", i)] = "x\n"
	}
	repo.Commit(t, files, "many")
	root := t.TempDir()
	g := gitx.NewGoGit(gitx.Options{Root: root})
	m := gitx.MirrorID(uuid.NewString())
	srv := gittest.NewHTTPServer(t, repo, gittest.HTTPOptions{TLS: true})
	c, err := g.FetchRef(context.Background(), m, gitx.Remote{URL: srv.URL, CAPEM: srv.CAPEM}, gitx.RefBranch, "main")
	if err != nil {
		t.Fatal(err)
	}

	d, err := g.ReadDir(context.Background(), m, c.SHA, "many", 500)
	if err != nil || len(d.Entries) != 500 || d.Total != 600 || !d.Capped || d.Entries[0].Name != "f000" || d.Entries[499].Name != "f499" {
		t.Errorf("ReadDir(many, 500) = %d entries, total %d, capped %v (%v), want the first 500 by name of 600", len(d.Entries), d.Total, d.Capped, err)
	}
	d, err = g.ReadDir(context.Background(), m, c.SHA, "many", 0)
	if err != nil || len(d.Entries) != 500 || d.Total != 600 || !d.Capped {
		t.Errorf("ReadDir(many, 0) = %d entries, total %d, capped %v (%v), want the default limit of 500", len(d.Entries), d.Total, d.Capped, err)
	}
	d, err = g.ReadDir(context.Background(), m, c.SHA, "many", 1000)
	if err != nil || len(d.Entries) != 600 || d.Total != 600 || d.Capped {
		t.Errorf("ReadDir(many, 1000) = %d entries, total %d, capped %v (%v), want all 600", len(d.Entries), d.Total, d.Capped, err)
	}
}

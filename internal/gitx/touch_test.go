// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/gitx/gittest"
)

// Counting the commits that changed one folder (plan 09-14, GIT-03, D-13): a monorepo's other folders do not move a
// stack.

type touchFixture struct {
	*browseFixture
	first, second, third, fourth string
}

// newTouchFixture serves a repository of two stack folders: the first commit makes both, the second changes a, the
// third changes b and the fourth adds a file to a.
func newTouchFixture(t *testing.T) *touchFixture {
	t.Helper()
	repo := gittest.NewRepo(t)
	first := repo.Commit(t, map[string]string{"a/compose.yaml": "services: {}\n", "b/compose.yaml": "services: {}\n"}, "first")
	second := repo.Commit(t, map[string]string{"a/compose.yaml": "services: {a: {image: x}}\n"}, "second\n\nlonger body")
	third := repo.Commit(t, map[string]string{"b/compose.yaml": "services: {b: {image: x}}\n"}, "third")
	fourth := repo.Commit(t, map[string]string{"a/extra.env": "A=1\n"}, strings.Repeat("é", 300))
	root := t.TempDir()
	f := &browseFixture{
		repo: repo, srv: gittest.NewHTTPServer(t, repo, gittest.HTTPOptions{TLS: true}), sha: fourth, root: root,
		g: gitx.NewGoGit(gitx.Options{Root: root}), m: gitx.MirrorID(uuid.NewString()),
	}
	if _, err := f.g.FetchRef(context.Background(), f.m, f.remote(), gitx.RefBranch, "main"); err != nil {
		t.Fatalf("FetchRef: %v", err)
	}
	return &touchFixture{browseFixture: f, first: first, second: second, third: third, fourth: fourth}
}

func shas(cs []gitx.TouchCommit) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.SHA
	}
	return out
}

func TestCommitsTouchingFolder(t *testing.T) {
	t.Parallel()
	f := newTouchFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		dir  string
		want []string
	}{
		{"a", []string{f.second, f.fourth}},
		{"b", []string{f.third}},
		{"", []string{f.second, f.third, f.fourth}},
		{"nowhere", nil},
	} {
		got, err := f.g.CommitsTouching(ctx, f.m, f.fourth, f.first, tc.dir, 0)
		if err != nil {
			t.Fatalf("CommitsTouching(%q): %v", tc.dir, err)
		}
		if got.Count != len(tc.want) || got.Capped || got.NotAncestor {
			t.Errorf("CommitsTouching(%q) = %+v, want %d commits", tc.dir, got, len(tc.want))
		}
		gotSHAs := shas(got.Commits)
		slices.Sort(gotSHAs)
		want := slices.Clone(tc.want)
		slices.Sort(want)
		if !slices.Equal(gotSHAs, want) {
			t.Errorf("CommitsTouching(%q) commits = %v, want %v", tc.dir, gotSHAs, want)
		}
	}
}

func TestCommitsTouchingNothingNew(t *testing.T) {
	t.Parallel()
	f := newTouchFixture(t)
	ctx := context.Background()
	same, err := f.g.CommitsTouching(ctx, f.m, f.fourth, f.fourth, "a", 0)
	if err != nil || same.Count != 0 || same.NotAncestor || same.Capped {
		t.Errorf("head equal to base = %+v, %v, want nothing", same, err)
	}
	// b did not change after the third commit.
	got, err := f.g.CommitsTouching(ctx, f.m, f.fourth, f.third, "b", 0)
	if err != nil || got.Count != 0 || got.NotAncestor {
		t.Errorf("b since its last change = %+v, %v, want 0", got, err)
	}
}

func TestCommitsTouchingOrderAndSubjects(t *testing.T) {
	t.Parallel()
	f := newTouchFixture(t)
	got, err := f.g.CommitsTouching(context.Background(), f.m, f.fourth, f.first, "a", 0)
	if err != nil || len(got.Commits) != 2 {
		t.Fatalf("CommitsTouching = %+v, %v", got, err)
	}
	// Every commit of the fixture has the same time, so the tie is broken by SHA.
	if got.Commits[0].SHA > got.Commits[1].SHA {
		t.Errorf("commits = %v, want ties by SHA", shas(got.Commits))
	}
	for _, c := range got.Commits {
		switch c.SHA {
		case f.second:
			if c.Subject != "second" || c.Author != "Bayplan Test" {
				t.Errorf("second = %+v, want the first line only and the author's name", c)
			}
		case f.fourth:
			if n := len([]rune(c.Subject)); n != 200 {
				t.Errorf("long subject has %d characters, want it cut to 200", n)
			}
		}
	}
}

func TestCommitsTouchingCap(t *testing.T) {
	t.Parallel()
	f := newTouchFixture(t)
	got, err := f.g.CommitsTouching(context.Background(), f.m, f.fourth, f.first, "", 2)
	if err != nil || !got.Capped || got.NotAncestor {
		t.Errorf("a walk of 2 over 3 commits = %+v, %v, want capped", got, err)
	}
}

func TestCommitsTouchingNotAnAncestor(t *testing.T) {
	t.Parallel()
	f := newTouchFixture(t)
	ctx := context.Background()
	// The first commit is no descendant of the fourth.
	got, err := f.g.CommitsTouching(ctx, f.m, f.first, f.fourth, "a", 0)
	if err != nil || !got.NotAncestor || got.Count != 0 || len(got.Commits) != 0 {
		t.Errorf("reversed = %+v, %v, want not an ancestor and no commits", got, err)
	}
	// A base the mirror does not hold (a force-push dropped it) is the same.
	got, err = f.g.CommitsTouching(ctx, f.m, f.fourth, strings.Repeat("ab", 20), "a", 0)
	if err != nil || !got.NotAncestor {
		t.Errorf("unknown base = %+v, %v, want not an ancestor", got, err)
	}
}

func TestCommitsTouchingNeedsTheHead(t *testing.T) {
	t.Parallel()
	f := newTouchFixture(t)
	_, err := f.g.CommitsTouching(context.Background(), f.m, strings.Repeat("cd", 20), f.first, "a", 0)
	var ge *gitx.Error
	if !errors.As(err, &ge) || ge.Code != gitx.CodeCommitMissing {
		t.Errorf("unknown head err = %v, want commit_missing", err)
	}
}

func TestCommitsTouchingNotAncestorAndCap(t *testing.T) {
	t.Parallel()
	f := newTouchFixture(t)
	ctx := context.Background()
	fetch := func() string {
		t.Helper()
		c, err := f.g.FetchRef(ctx, f.m, f.remote(), gitx.RefBranch, "main")
		if err != nil {
			t.Fatalf("FetchRef: %v", err)
		}
		return c.SHA
	}

	// Twelve more commits: eight change a and four change b.
	head := f.repo.CommitMany(t, 12, func(i int) string {
		if i%3 == 2 {
			return "b/n.env"
		}
		return "a/n.env"
	})
	if got := fetch(); got != head {
		t.Fatalf("head = %s, want %s", got, head)
	}
	exact, err := f.g.CommitsTouching(ctx, f.m, head, f.fourth, "a", 12)
	if err != nil || exact.Capped || exact.NotAncestor || exact.Count != 8 {
		t.Errorf("a walk of exactly the 12 commits = %+v, %v, want 8 and neither capped nor not-ancestor", exact, err)
	}
	short, err := f.g.CommitsTouching(ctx, f.m, head, f.fourth, "a", 11)
	if err != nil || !short.Capped || short.NotAncestor {
		t.Errorf("a walk of 11 over 12 commits = %+v, %v, want capped", short, err)
	}

	// A history that replaces the old one: the deployed commit is no ancestor of the new head.
	orphan := f.repo.Orphan(t, map[string]string{"a/compose.yaml": "services: {z: {image: z}}\n"}, "replacement")
	if got := fetch(); got != orphan {
		t.Fatalf("head after the force-push = %s, want %s", got, orphan)
	}
	gone, err := f.g.CommitsTouching(ctx, f.m, orphan, f.fourth, "a", 0)
	if err != nil || !gone.NotAncestor || gone.Count != 0 || gone.Capped || len(gone.Commits) != 0 {
		t.Errorf("after a force-push = %+v, %v, want not an ancestor with nothing counted", gone, err)
	}
}

// A feature branch that forked before the deployed commit and was merged after it: the commits behind the deployed
// commit are not between it and the head, and are not counted (WR-A04).
func TestCommitsTouchingLeavesOutTheHistoryBehindTheBaseAcrossAMerge(t *testing.T) {
	t.Parallel()
	repo := gittest.NewRepo(t)
	root := repo.Commit(t, map[string]string{"a/compose.yaml": "services: {}\n"}, "root")
	repo.Commit(t, map[string]string{"a/old.env": "OLD=1\n"}, "older, on main")
	feature := repo.Fork(t, root, "feature", map[string]string{"a/feature.env": "F=1\n"}, "feature")
	base := repo.Commit(t, map[string]string{"a/compose.yaml": "services: {a: {image: x}}\n"}, "deployed")
	head := repo.Merge(t, "feature", "merge feature")

	dir := t.TempDir()
	f := &browseFixture{
		repo: repo, srv: gittest.NewHTTPServer(t, repo, gittest.HTTPOptions{TLS: true}), sha: head, root: dir,
		g: gitx.NewGoGit(gitx.Options{Root: dir}), m: gitx.MirrorID(uuid.NewString()),
	}
	if _, err := f.g.FetchRef(context.Background(), f.m, f.remote(), gitx.RefBranch, "main"); err != nil {
		t.Fatalf("FetchRef: %v", err)
	}

	got, err := f.g.CommitsTouching(context.Background(), f.m, head, base, "a", 0)
	if err != nil || got.Capped || got.NotAncestor {
		t.Fatalf("CommitsTouching = %+v, %v", got, err)
	}
	want := []string{feature, head}
	gotSHAs := shas(got.Commits)
	slices.Sort(gotSHAs)
	slices.Sort(want)
	if got.Count != 2 || !slices.Equal(gotSHAs, want) {
		t.Errorf("commits = %v (count %d), want only the feature commit and the merge: %v", gotSHAs, got.Count, want)
	}
}
